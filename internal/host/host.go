package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xpath"
	"golang.org/x/net/html"
)

// Declarative host resolution rules (REDESIGN §4.2).
// A rule is JSON data; saving a file hot-reloads it. Multiple rules may
// target the same image host (e.g. keep an old and a new variant); the
// first enabled rule whose domain matches wins.

type Step struct {
	Action   string            `json:"action"` // get|post|set-cookie|extract|regex|set
	Source   string            `json:"url,omitempty"`
	Field    string            `json:"field,omitempty"` // page_url|image_url|filename
	Selector string            `json:"selector,omitempty"`
	Attr     string            `json:"attr,omitempty"` // src|href|title|alt|value|text|content
	Prefix   string            `json:"prefix,omitempty"`
	Default  string            `json:"default,omitempty"`
	Find     string            `json:"find,omitempty"`
	Replace  string            `json:"replace,omitempty"`
	Data     map[string]string `json:"data,omitempty"`      // post form values (literal)
	GrabForm bool              `json:"grab_form,omitempty"` // merge page <input name=value> into form
	Name     string            `json:"name,omitempty"`      // set-cookie name
	// Purpose 标记步骤用途："filename" = 只为抓原文件名。用户关闭
	// FetchOrigName 时这类步骤整体跳过（省去为文件名访问页面的开销，
	// 与原版 vripper 的 TRY_TO_FETCH_ORIGINAL_FILENAME 行为一致）。
	Purpose string `json:"purpose,omitempty"`
	Value   string `json:"value,omitempty"` // set-cookie value
}

type Rule struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Note    string   `json:"note,omitempty"`
	Referer string   `json:"referer,omitempty"` // download-time Referer override
	Steps   []Step   `json:"steps"`

	// runtime metadata (not part of rule file identity)
	Enabled bool   `json:"enabled"`
	Builtin bool   `json:"builtin"`
	File    string `json:"-"`
	// ID identifies one loaded rule file (e.g. "imagetwist.custom.json").
	// Two rules may share a name (a custom rule overriding a builtin one),
	// so the UI must act on ID, not name.
	ID string `json:"id,omitempty"`
	// ModifiedAt 是规则文件的最后修改时间（界面上叫“添加日期”）；Origin 区分
	// 内置 / 自定义 / 由 cmd/importv 从原版导入。两者都只在加载时算出来，
	// 不写回 JSON（marshalRuleFile 只输出落盘字段）。
	ModifiedAt string `json:"added,omitempty"`
	Origin     string `json:"origin,omitempty"`
}

type Result struct {
	ImageURL string
	Filename string
	Trace    []string
}

type Resolver interface {
	// Match reports whether this URL is handled by any enabled rule.
	Match(pageURL string) bool
	// Resolve extracts the direct image URL and original filename.
	Resolve(pageURL string) (*Result, error)
	// ResolveWith runs a specific rule by name (used by the host tester).
	// thumbURL feeds {thumb_url} and may be empty for page-parsing rules;
	// rules that derive the direct link from the thumb path (imx) need it.
	ResolveWith(ruleName, pageURL, thumbURL string) (*Result, error)
}

type Registry struct {
	mu            sync.RWMutex
	rules         []*Rule
	dataDir       string
	hostsDir      string // 规则目录，默认 <data>/hosts，可在界面里改到任意位置
	fetchOrigName bool   // 默认 true；pipeline 会按用户设置注入
}

func NewRegistry(dataDir string) *Registry {
	return &Registry{dataDir: dataDir, hostsDir: filepath.Join(dataDir, "hosts")}
}

// HostsDir returns the directory rules are read from / written to.
func (r *Registry) HostsDir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hostsDir
}

// SetHostsDir points the registry at another directory (and reloads).
// Pass "" to fall back to the default <data>/hosts.
func (r *Registry) SetHostsDir(dir string) error {
	if dir == "" {
		dir = filepath.Join(r.dataDir, "hosts")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	r.mu.Lock()
	r.hostsDir = abs
	r.mu.Unlock()
	return r.Reload()
}

func (r *Registry) fileFor(name string, builtin bool) string {
	suffix := ""
	if !builtin {
		suffix = ".custom"
	}
	return filepath.Join(r.HostsDir(), sanitize(name)+suffix+".json")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', ' ':
			return '-'
		}
		return r
	}, s)
}

// Seed writes the builtin rules to disk on first run (so users can edit them).
// When the user pointed the registry at a custom folder, the builtins are
// seeded there instead — one folder is always the single source of truth.
func (r *Registry) Seed() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := r.hostsDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.rules = nil
	for i := range builtinRules {
		def := builtinRules[i]
		file := filepath.Join(dir, sanitize(def.Name)+".json")
		raw, err := MarshalRule(&def)
		if err != nil {
			return err
		}
		if _, err := os.Stat(file); os.IsNotExist(err) {
			if err := os.WriteFile(file, raw, 0o644); err != nil {
				return err
			}
		}
	}
	return r.loadLocked()
}

// loadLocked reads every rule JSON under the configured hosts dir.
func (r *Registry) loadLocked() error {
	dir := r.hostsDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var rules []*Rule
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		rule, err := ParseRule(raw)
		if err != nil {
			continue // skip broken rules rather than failing startup
		}
		rule.File = filepath.Join(dir, e.Name())
		rule.ID = e.Name()
		rule.Builtin = !strings.HasSuffix(e.Name(), ".custom.json")
		if rule.Name == "" {
			rule.Name = strings.TrimSuffix(e.Name(), ".json")
		}
		rule.Origin = ruleOrigin(rule)
		if st, serr := os.Stat(rule.File); serr == nil && !st.ModTime().IsZero() {
			rule.ModifiedAt = st.ModTime().Format("2006-01-02 15:04")
		}
		rules = append(rules, rule)
	}
	r.rules = rules
	return nil
}

// Reload re-reads all rule files (hot reload, REDESIGN §4.2).
func (r *Registry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

// SetFetchOrigName 控制解析时是否执行 purpose=="filename" 的步骤（专为
// 抓取原文件名访问页面）。由 pipeline 按用户设置注入。
func (r *Registry) SetFetchOrigName(on bool) {
	r.mu.Lock()
	r.fetchOrigName = on
	r.mu.Unlock()
}

func (r *Registry) List() []Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Rule, len(r.rules))
	for i, rule := range r.rules {
		out[i] = *rule
	}
	return out
}

func (r *Registry) Get(name string) (*Rule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if rule.Name == name {
			cp := *rule
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("rule %q not found", name)
}

// normalizeDomain turns a rule's "domains" entry into a bare lower-case host:
// tolerates scheme/path/port/leading www., so旧规则写成
// "https://imagetwist.com/" 也照样能用。
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return ""
	}
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if h, _, err := net.SplitHostPort(d); err == nil {
		d = h
	}
	d = strings.TrimSuffix(d, ".")
	return strings.TrimPrefix(d, "www.")
}

// hostMatches reports whether pageURL belongs to domain. 以前用的是对整条
// URL 做 strings.Contains，于是 "imx.to" 会命中 "https://evilimx.to/" 和
// "https://foo.com/?ref=imx.to"；这里改成按 hostname 做后缀匹配。
func hostMatches(pageURL, domain string) bool {
	d := normalizeDomain(domain)
	if d == "" {
		return false
	}
	if u, err := url.Parse(pageURL); err == nil && u.Host != "" {
		h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		return h == d || strings.HasSuffix(h, "."+d)
	}
	// 非标准 URL（测试里的裸字符串等）退回宽松匹配
	return strings.Contains(strings.ToLower(pageURL), d)
}

func (r *Registry) match(rule *Rule, pageURL string) bool {
	if !rule.Enabled {
		return false
	}
	for _, d := range rule.Domains {
		if hostMatches(pageURL, d) {
			return true
		}
	}
	return false
}

func (r *Registry) Match(pageURL string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if r.match(rule, pageURL) {
			return true
		}
	}
	return false
}

// RuleNameOf returns the rule name handling the URL (for display).
func (r *Registry) RuleNameOf(pageURL string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if r.match(rule, pageURL) {
			return rule.Name
		}
	}
	return ""
}

func (r *Registry) Resolve(pageURL string) (*Result, error) {
	return r.ResolveCtx(context.Background(), pageURL)
}

// ResolveCtx resolves with a cancellable context so pipeline stops take
// effect immediately (the underlying HTTP requests honor ctx).
func (r *Registry) ResolveCtx(ctx context.Context, pageURL string) (*Result, error) {
	return r.ResolveImageCtx(ctx, pageURL, "")
}

// DownloadReferer returns the Referer the download request for pageURL
// should carry.
//
// 优先用规则自己声明的 referer；一条都没有时，退到**图片页自己的站点根**
// 而不是硬编码 vipergirls.to —— 防盗链几乎都放行同站 Referer，而用论坛
// 地址去抓 imagetwist 会被判成盗链，直接回一张 "Hotlinking is disabled"
// 的占位图（这也是自定义规则没写 referer 就"全是缩略图"的根因）。
func (r *Registry) DownloadReferer(pageURL string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if r.match(rule, pageURL) && rule.Referer != "" {
			return rule.Referer
		}
	}
	if u, err := url.Parse(pageURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/"
	}
	return "https://vipergirls.to/"
}

func (r *Registry) ResolveWith(ruleName, pageURL, thumbURL string) (*Result, error) {
	return r.ResolveWithCtx(context.Background(), ruleName, pageURL, thumbURL)
}

// ResolveImageCtx resolves with the image's thumbnail URL available to the
// rule as {thumb_url} — some hosts (imx) derive the direct url from the
// thumb path, which carries segments the short page link does not have.
func (r *Registry) ResolveImageCtx(ctx context.Context, pageURL, thumbURL string) (*Result, error) {
	r.mu.RLock()
	var matches []*Rule
	for _, rule := range r.rules {
		if r.match(rule, pageURL) {
			cp := *rule
			matches = append(matches, &cp)
		}
	}
	r.mu.RUnlock()
	if len(matches) == 0 {
		return nil, fmt.Errorf("no enabled host rule matches %s", pageURL)
	}
	var lastErr error
	// 直接用上面拷贝出来的规则对象：同名规则（自定义遮蔽内置）必须各自
	// 跑一遍，改成按名字重新 Get 会永远拿到第一条，内置那条的兜底就失效了。
	for _, rule := range matches {
		vars := map[string]string{
			"page_url": pageURL,
			"url_name": urlNameOf(pageURL),
		}
		if thumbURL != "" {
			vars["thumb_url"] = thumbURL
		}
		e := &executor{rule: rule, pageURL: pageURL, ctx: ctx, vars: vars, fetchOrigName: r.fetchOrigName}
		res, err := e.run()
		if err == nil && res != nil && res.ImageURL != "" {
			return res, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all matching rules produced no image_url for %s", pageURL)
	}
	return nil, lastErr
}

func (r *Registry) ResolveWithCtx(ctx context.Context, ruleName, pageURL, thumbURL string) (*Result, error) {
	rule, err := r.Get(ruleName)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{
		"page_url": pageURL,
		"url_name": urlNameOf(pageURL),
	}
	if thumbURL != "" {
		vars["thumb_url"] = thumbURL
	}
	e := &executor{rule: rule, pageURL: pageURL, ctx: ctx, vars: vars, fetchOrigName: r.fetchOrigName}
	return e.run()
}

// ResolveRule runs an unsaved rule object (rule editor / browser extension
// preview): the same executor a saved rule uses, without touching disk.
func (r *Registry) ResolveRule(ctx context.Context, rule *Rule, pageURL, thumbURL string) (*Result, error) {
	if rule == nil {
		return nil, fmt.Errorf("nil rule")
	}
	vars := map[string]string{
		"page_url": pageURL,
		"url_name": urlNameOf(pageURL),
	}
	if thumbURL != "" {
		vars["thumb_url"] = thumbURL
	}
	e := &executor{rule: rule, pageURL: pageURL, ctx: ctx, vars: vars, fetchOrigName: r.fetchOrigName}
	return e.run()
}

// NameStatus reports whether a rule with this name is loaded, and whether the
// loaded one is a builtin (a custom rule with the same name would shadow it).
func (r *Registry) NameStatus(name string) (exists, builtin bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if rule.Name == name {
			return true, rule.Builtin
		}
	}
	return false, false
}

// ImportedNoteMarker 是 cmd/importv 写进规则备注里的来源标记，界面据此把
// 从原版 hosts.xml 搬来的规则单独归为一类（方便整体审视/回退）。
const ImportedNoteMarker = "由原版 ViperRipper (2018) hosts.xml 转换"

// IsImportedNote 判断备注是不是来自 cmd/importv（兼容早期措辞，
// 单采用这两个关键词，改文案不用跟着改这里）。
func IsImportedNote(note string) bool {
	return strings.Contains(note, "hosts.xml") && strings.Contains(note, "转换")
}

// ruleOrigin 给界面用的分类：imported / builtin / custom。
func ruleOrigin(r *Rule) string {
	switch {
	case IsImportedNote(r.Note):
		return "imported"
	case r.Builtin:
		return "builtin"
	default:
		return "custom"
	}
}

// RuleNeedsThumb reports whether the rule derives its URLs from {thumb_url}
// (imx and friends). Such rules cannot be exercised with a page URL alone.
func RuleNeedsThumb(rule *Rule) bool {
	if rule == nil {
		return false
	}
	for _, st := range rule.Steps {
		if strings.Contains(st.Source, "{thumb_url}") || strings.Contains(st.Replace, "{thumb_url}") {
			return true
		}
	}
	return false
}

// DownloadRefererFor is DownloadReferer for a rule object that may not be
// saved yet.
func DownloadRefererFor(rule *Rule) string {
	if rule != nil && rule.Referer != "" {
		return rule.Referer
	}
	return "https://vipergirls.to/"
}

// ---------- persistence helpers ----------

func MarshalRule(rule *Rule) ([]byte, error) {
	raw, err := marshalRuleFile(rule)
	return raw, err
}

func marshalRuleFile(rule *Rule) ([]byte, error) {
	type fileRule struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Note    string   `json:"note,omitempty"`
		Referer string   `json:"referer,omitempty"`
		Enabled *bool    `json:"enabled,omitempty"`
		Steps   []Step   `json:"steps"`
	}
	enabled := rule.Enabled
	fr := fileRule{Name: rule.Name, Domains: rule.Domains, Note: rule.Note, Referer: rule.Referer, Enabled: &enabled, Steps: rule.Steps}
	raw, err := json.MarshalIndent(fr, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func ParseRule(raw []byte) (*Rule, error) {
	var fileRule struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Note    string   `json:"note"`
		Referer string   `json:"referer"`
		Enabled *bool    `json:"enabled"`
		Steps   []Step   `json:"steps"`
	}
	if err := json.Unmarshal(raw, &fileRule); err != nil {
		return nil, err
	}
	rule := &Rule{Name: fileRule.Name, Domains: fileRule.Domains, Note: fileRule.Note, Referer: fileRule.Referer, Steps: fileRule.Steps}
	if rule.Name == "" {
		return nil, fmt.Errorf("rule has no name")
	}
	// absent field defaults to enabled
	rule.Enabled = fileRule.Enabled == nil || *fileRule.Enabled
	return rule, nil
}

// Save writes a rule (custom or builtin override) and hot-reloads.
func (r *Registry) Save(rule *Rule) error {
	if err := r.writeRule(rule); err != nil {
		return err
	}
	return r.Reload()
}

// writeRule 只落盘、不重载：批量操作时用它攒着，最后 reload 一次。
func (r *Registry) writeRule(rule *Rule) error {
	if rule.Name == "" {
		return fmt.Errorf("rule name required")
	}
	if len(rule.Domains) == 0 {
		return fmt.Errorf("at least one domain required")
	}
	file := rule.File
	if file == "" {
		file = r.fileFor(rule.Name, rule.Builtin)
	}
	raw, err := MarshalRule(rule)
	if err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0o644)
}

// SetEnabledByIDs 批量启停：全部写盘后只热加载一次。界面里 Ctrl/Shift 多选
// 之后一次改几十上百条，逐条 Save 会把整个目录来回扫同样多遍。
func (r *Registry) SetEnabledByIDs(ids []string, enabled bool) (ok, failed int) {
	for _, id := range ids {
		rule, err := r.getByID(id)
		if err != nil {
			failed++
			continue
		}
		rule.Enabled = enabled
		if err := r.writeRule(rule); err != nil {
			failed++
			continue
		}
		ok++
	}
	if ok > 0 {
		_ = r.Reload()
	}
	return ok, failed
}

// DeleteByIDs 批量删除：删完统一重载一次。
func (r *Registry) DeleteByIDs(ids []string) (ok, failed int) {
	for _, id := range ids {
		rule, err := r.getByID(id)
		if err != nil {
			failed++
			continue
		}
		if rule.File == "" {
			failed++
			continue
		}
		if err := os.Remove(rule.File); err != nil {
			failed++
			continue
		}
		ok++
	}
	if ok > 0 {
		_ = r.Reload()
	}
	return ok, failed
}

func (r *Registry) SetEnabled(name string, enabled bool) error {
	rule, err := r.Get(name)
	if err != nil {
		return err
	}
	rule.Enabled = enabled
	return r.Save(rule)
}

// SetEnabledByID toggles one specific rule file. Needed because a custom
// rule may shadow a builtin one under the same name: by name we would always
// hit the first match (the custom file), so unticking the builtin row would
// silently disable the custom rule instead.
func (r *Registry) SetEnabledByID(id string, enabled bool) error {
	rule, err := r.getByID(id)
	if err != nil {
		return err
	}
	rule.Enabled = enabled
	return r.Save(rule)
}

// DeleteByID removes one specific rule file (builtin files come back on the
// next Seed, custom ones are gone for good).
func (r *Registry) DeleteByID(id string) error {
	rule, err := r.getByID(id)
	if err != nil {
		return err
	}
	if rule.File == "" {
		return fmt.Errorf("rule has no file")
	}
	if err := os.Remove(rule.File); err != nil {
		return err
	}
	return r.Reload()
}

func (r *Registry) getByID(id string) (*Rule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if rule.ID == id {
			cp := *rule
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("rule %q not found", id)
}

// GetByID returns the loaded rule backed by the given rule file name.
func (r *Registry) GetByID(id string) (*Rule, error) { return r.getByID(id) }

func (r *Registry) Delete(name string) error {
	rule, err := r.Get(name)
	if err != nil {
		return err
	}
	if rule.File == "" {
		return fmt.Errorf("rule has no file")
	}
	if err := os.Remove(rule.File); err != nil {
		return err
	}
	return r.Reload()
}

// SaveCustom writes a rule as a user rule (data/hosts/<name>.custom.json)
// and hot-reloads. Returns the file it wrote.
func (r *Registry) SaveCustom(rule *Rule) (string, error) {
	if rule == nil {
		return "", fmt.Errorf("nil rule")
	}
	// 保存前的语法自检放这里，不放 Save：Save 也被“勾选启停”复用，
	// 不能让一条历史坏规则连禁用都禁用不了。
	if err := ValidateRule(rule); err != nil {
		return "", fmt.Errorf("规则自检未通过：%w", err)
	}
	rule.Builtin = false
	file := r.fileFor(rule.Name, false)
	rule.File = file
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	if err := r.Save(rule); err != nil {
		return "", err
	}
	return file, nil
}

// ---------- execution engine ----------

type executor struct {
	rule    *Rule
	pageURL string
	ctx     context.Context
	vars    map[string]string
	client  *fetcher
	trace   []string
	doc     *html.Node
	// directImage 表示 {page_url} 本身就是一张图片（服务端直接返回 image/*），
	// 此时后续 extract 步骤没有意义，直接采用这个地址。
	directImage bool
	// fetchOrigName=false 时跳过 purpose=="filename" 的步骤（不专为文件名
	// 访问页面），由 Registry 全局注入。
	fetchOrigName bool
	// gateRetried 防止“自动补 POST”无限递归。
	gateRetried bool
}

func (e *executor) log(format string, a ...any) {
	e.trace = append(e.trace, fmt.Sprintf(format, a...))
}

func (e *executor) run() (*Result, error) {
	res, err := e.runSteps()
	if err == nil {
		return res, nil
	}
	// 失败时看看是不是“点击继续”中间页：这类页面第一跳根本没有原图，
	// 任何 img 选择器都必然落空。规则没写 post 步骤时，这里替它补一次。
	if e.gateRetried || e.doc == nil {
		return res, err
	}
	form := detectGateForm(e.doc)
	if len(form) == 0 {
		return res, err
	}
	steps2 := insertGatePost(e.rule.Steps, form)
	if steps2 == nil {
		return res, err
	}
	dup := &Rule{Name: e.rule.Name, Domains: e.rule.Domains, Referer: e.rule.Referer, Steps: steps2}
	vars := map[string]string{}
	for k, v := range e.vars {
		vars[k] = v
	}
	e2 := &executor{rule: dup, pageURL: e.pageURL, ctx: e.ctx, vars: vars, gateRetried: true}
	res2, err2 := e2.runSteps()
	if err2 != nil {
		return res, err
	}
	res2.Trace = append(res2.Trace, "检测到“点击继续”中间页，已自动 POST 表单后重试")
	return res2, nil
}

func (e *executor) runSteps() (*Result, error) {
	e.client = newFetcher(e.ctx)
	defer e.client.Close()
	fetchName := e.fetchOrigName
	for i := range e.rule.Steps {
		st := &e.rule.Steps[i]
		if st.Purpose == "filename" && !fetchName {
			continue // 用户关了“抓取原文件名”，不为文件名访问页面
		}
		if err := e.step(st); err != nil {
			e.log("✘ step %d (%s) failed: %v", i+1, st.Action, err)
			return nil, fmt.Errorf("step %d %s: %w", i+1, st.Action, err)
		}
		if e.directImage {
			break // 地址本身就是图片，后面的解析步骤不再适用
		}
	}
	res := &Result{
		ImageURL: e.vars["image_url"],
		Filename: strings.TrimSpace(e.vars["filename"]),
		Trace:    e.trace,
	}
	if res.ImageURL == "" {
		return res, fmt.Errorf("rule produced no image_url")
	}
	// JSON/JS 里的地址常把斜杠转义成 \/（"https://////cdn..."），规则从源码里
	// 捞出来时要还原，否则拼出来的直链没法下载。
	if strings.Contains(res.ImageURL, "\\/") {
		res.ImageURL = strings.ReplaceAll(res.ImageURL, "\\/", "/")
	}
	res.ImageURL = resolveURL(e.pageURL, res.ImageURL)
	return res, nil
}

func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if strings.HasPrefix(u, "http://") {
		u = "https://" + u[len("http://"):]
	}
	return u
}

// resolveURL absolutizes extracted URLs: protocol-relative, site-relative
// ("/img/1.jpg") and plain http upgrade all resolve against the page URL.
func resolveURL(pageURL, u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return u
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		// keep http on loopback hosts (local test servers have no TLS)
		host := u
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if j := strings.Index(host, "/"); j >= 0 {
			host = host[:j]
		}
		if h, _, err := net.SplitHostPort(host); err == nil && (h == "127.0.0.1" || h == "localhost" || h == "::1") {
			return u
		}
		if host == "localhost" || host == "::1" {
			return u
		}
		return normalizeURL(u)
	}
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	if strings.HasPrefix(u, "/") {
		if base, err := url.Parse(pageURL); err == nil {
			return base.Scheme + "://" + base.Host + u
		}
		return u
	}
	return u
}

func (e *executor) expand(s string) string {
	return expandVars(s, e.vars)
}

func (e *executor) step(st *Step) error {
	switch st.Action {
	case "get":
		url := e.expand(st.Source)
		doc, body, ctype, err := e.client.get(url)
		if err != nil {
			return err
		}
		if strings.HasPrefix(strings.ToLower(ctype), "image/") {
			// 有些“图片页”其实就是原图本身；再拿 HTML 解析只会报选择器没命中。
			// 但要先排掉“页面直接回一张占位图”的情况（imagevenue 就是
			// img.php 直接吐 240x240 的 Thumbnail unavailable）。
			if w, h, ok := imageSize([]byte(body)); ok && looksLikePlaceholderSize(w, h) {
				return fmt.Errorf("目标返回的是一张 %dx%d 的小图（疑似占位图/缩略图），不是原图", w, h)
			}
			e.vars["image_url"] = url
			e.directImage = true
			e.log("GET %s → 响应直接是图片（%s），按直链处理", url, ctype)
			return nil
		}
		e.doc = doc
		e.vars["body"] = clipBody(body)
		e.log("GET %s → ok", url)
	case "post":
		url := e.expand(st.Source)
		form := map[string]string{}
		if st.GrabForm && e.doc != nil {
			for _, in := range htmlquery.Find(e.doc, "//input") {
				name := attrOf(in, "name")
				if name == "" {
					continue
				}
				v := attrOf(in, "value")
				if _, exists := form[name]; !exists {
					form[name] = v
				}
			}
			for _, ta := range htmlquery.Find(e.doc, "//textarea") {
				name := attrOf(ta, "name")
				if name != "" {
					if _, exists := form[name]; !exists {
						form[name] = htmlquery.InnerText(ta)
					}
				}
			}
		}
		for k, v := range st.Data {
			form[k] = e.expand(v)
		}
		doc, body, ctype, err := e.client.post(url, form)
		if err != nil {
			return err
		}
		if strings.HasPrefix(strings.ToLower(ctype), "image/") {
			if w, h, ok := imageSize([]byte(body)); ok && looksLikePlaceholderSize(w, h) {
				return fmt.Errorf("目标返回的是一张 %dx%d 的小图（疑似占位图/缩略图），不是原图", w, h)
			}
			e.vars["image_url"] = url
			e.directImage = true
			e.log("POST %s → 响应直接是图片（%s），按直链处理", url, ctype)
			return nil
		}
		e.doc = doc
		e.vars["body"] = clipBody(body)
		e.log("POST %s (%d fields) → ok", url, len(form))
	case "set-cookie":
		e.client.setCookie(e.pageURL, e.expand(st.Name), e.expand(st.Value))
		e.log("SET-COOKIE %s", st.Name)
	case "set":
		e.vars[e.expand(st.Field)] = e.expand(st.Value)
		e.log("SET %s = %s", st.Field, truncate(e.vars[st.Field], 90))
	case "regex":
		// st.Source holds a variable expression for the source text (e.g.
		// "{page_url}"); when empty the field's current value is used.
		source := e.vars[st.Field]
		if st.Source != "" {
			source = e.expand(st.Source)
		}
		// selector 也做变量展开：规则可写 {url_name} 让"抓本图文件名"的
		// 正则泛化到任意图片，而不是只对录制时那张图有效
		re, err := regexp.Compile(e.expand(st.Selector))
		if err != nil {
			return fmt.Errorf("bad regex %q: %w", st.Selector, err)
		}
		out := re.ReplaceAllString(source, e.expand(st.Replace))
		e.vars[st.Field] = out
		e.log("REGEX %s: %s → %s", st.Field, truncate(source, 60), truncate(out, 60))
	case "search":
		// 在文本（通常是 {body} 页面源码/JSON）里找第一处正则匹配，
		// 取捕获组 1（没有分组就取整个匹配）。和 regex 的“替换”语义不同：
		// regex 会把没匹配到的上下文原样保留，search 只输出匹配到的那段。
		source := e.vars[st.Field]
		if st.Source != "" {
			source = e.expand(st.Source)
		}
		if strings.TrimSpace(st.Selector) == "" {
			return fmt.Errorf("search 步骤缺少正则表达式")
		}
		// 同 regex：selector 支持 {url_name} 等变量展开
		re, err := regexp.Compile(e.expand(st.Selector))
		if err != nil {
			return fmt.Errorf("bad regex %q: %w", st.Selector, err)
		}
		m := re.FindStringSubmatch(source)
		if m == nil {
			if st.Default != "" {
				e.vars[e.expand(st.Field)] = e.expand(st.Default)
				e.log("SEARCH %s: no match, default=%s", st.Field, truncate(e.vars[st.Field], 60))
				return nil
			}
			return fmt.Errorf("正则 %q 在页面里没有匹配到内容", truncate(st.Selector, 80))
		}
		val := m[0]
		if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			val = m[1]
		}
		e.vars[st.Field] = strings.TrimSpace(val)
		e.log("SEARCH %s = %s", st.Field, truncate(e.vars[st.Field], 90))
	case "extract":
		if e.doc == nil {
			return fmt.Errorf("no document to extract from")
		}
		if strings.TrimSpace(st.Selector) == "" {
			return fmt.Errorf("extract 步骤缺少 selector")
		}
		// 兼容历史坏写法：@class 被写成 @//class/（早期工具生成的规则里有），
		// 这种 XPath 永远匹配不到。自动修正一次并记录，避免整条规则报废。
		sel := repairSelector(st.Selector)
		if sel != st.Selector {
			e.log("WARN 选择器已自动修正：%s → %s", st.Selector, sel)
		}
		// 先做语法校验：语法错时 htmlquery 只会返回空集合，看起来像
		// “没匹配到”，实际是选择器写坏了。
		if _, cerr := xpath.Compile(sel); cerr != nil {
			return fmt.Errorf("XPath 语法错误 %q：%v（检查引号与方括号，属性应写成 @class 而不是 @//class/）", sel, cerr)
		}
		nodes := htmlquery.Find(e.doc, sel)
		if len(nodes) == 0 {
			if st.Default != "" {
				e.vars[e.expand(st.Field)] = e.expand(st.Default)
				e.log("EXTRACT %s: no match, default=%s", st.Field, truncate(e.vars[st.Field], 60))
				return nil
			}
			hint := ""
			if htmlquery.FindOne(e.doc, "//input[@name='imgContinue']") != nil ||
				htmlquery.FindOne(e.doc, "//*[@id='continuebutton']") != nil {
				hint = "；该页是“点击继续”中间页（imgContinue），规则需要先发一次 POST 才能拿到图片"
			} else if t := htmlquery.FindOne(e.doc, "//title"); t != nil {
				if title := strings.TrimSpace(htmlquery.InnerText(t)); title != "" {
					hint = "；页面标题：" + truncate(title, 60)
				}
			}
			return fmt.Errorf("[%s] 选择器 %q 在页面上没有命中%s", ruleNameOf(e.rule), sel, hint)
		}
		val := pickNodeValue(nodes, st.Attr)
		if val == "" && st.Default != "" {
			val = e.expand(st.Default)
		}
		if st.Prefix != "" && val != "" && !strings.HasPrefix(val, "http") {
			val = st.Prefix + val
		}
		e.vars[e.expand(st.Field)] = val
		e.log("EXTRACT %s = %s", st.Field, truncate(val, 90))
	default:
		return fmt.Errorf("unknown action %q", st.Action)
	}
	return nil
}

// pickNodeValue 取第一个“像原图”的命中节点。选择器写宽了（例如
// //img[contains(@class,'img')]）时，页面头部常有 logo / 1px 占位 / svg 图标
// 也满足条件，直接取 nodes[0] 会拿到图标。这里跳过明显的图标类 URL；
// 全都像图标时退回第一个有值的节点，行为不会比以前更差。
func pickNodeValue(nodes []*html.Node, attr string) string {
	textAttr := false
	switch strings.ToLower(attr) {
	case "text", "textcontent", "innertext":
		textAttr = true
	}
	var firstContent string
	for _, n := range nodes {
		var v string
		if textAttr {
			v = htmlquery.InnerText(n)
		} else {
			v = attrOf(n, attr)
		}
		v = strings.TrimSpace(v)
		if v == "" || looksLikeIconURL(v) {
			continue
		}
		if firstContent == "" {
			firstContent = v
		}
		if looksLikeThumbURL(v) {
			continue // 缩略图先跳过，继续找更像原图的
		}
		return v
	}
	if firstContent != "" {
		return firstContent // 只有缩略图/非图标候选时，退而求其次
	}
	// 全是图标：保持旧行为（取第一个有值的），不因过滤而失败
	for _, n := range nodes {
		var v string
		if textAttr {
			v = htmlquery.InnerText(n)
		} else {
			v = attrOf(n, attr)
		}
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// looksLikeThumbURL 判断 URL 是否带常见的缩略图特征（/th/、th_、_thumb、/small/…）。
// 这类地址不直接丢弃，只在有更像原图的候选时让位。
func looksLikeThumbURL(u string) bool {
	l := strings.ToLower(u)
	if strings.Contains(l, "/thumb") || strings.Contains(l, "/th/") ||
		strings.Contains(l, "thumb_") || strings.Contains(l, "/thumbs/") ||
		strings.Contains(l, "_thumb.") || strings.Contains(l, "_t.") ||
		strings.Contains(l, "/small/") || strings.Contains(l, "/mini/") {
		return true
	}
	// th_0123.jpg 这种前缀
	base := l
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.HasPrefix(base, "th_")
}

// looksLikeIconURL 判断一个 src/href 是否明显是图标/占位图而非内容图。
func looksLikeIconURL(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	if l == "" {
		return true
	}
	if strings.HasPrefix(l, "data:") {
		return true
	}
	for _, suffix := range []string{".svg", ".ico"} {
		if strings.HasSuffix(l, suffix) || strings.Contains(l, suffix+"?") {
			return true
		}
	}
	for _, k := range []string{"favicon", "/logo", "logo-", "spacer", "pixel.", "1x1", "blank.gif", "loading.gif", "spinner"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// detectGateForm 从“点击继续”中间页里取出要提交的表单字段。
// 认三种：input[name=imgContinue]、#continuebutton，以及所在表单里
// 文案含 continue/继续/proceed 的提交按钮。
func detectGateForm(doc *html.Node) map[string]string {
	if doc == nil {
		return nil
	}
	var trigger *html.Node
	for _, sel := range []string{
		"//input[@name='imgContinue']",
		"//*[@id='continuebutton']",
		"//input[contains(translate(@value,'CONTINUE','continue'),'continue')]",
		"//button[contains(translate(.,'CONTINUE继续','continue继续'),'continue')]",
	} {
		if n := htmlquery.FindOne(doc, sel); n != nil {
			trigger = n
			break
		}
	}
	if trigger == nil {
		return nil
	}
	form := map[string]string{}
	// 找最近的 form 祖先，把其中的 hidden / submit 字段一起带上
	for p := trigger.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "form" {
			for _, in := range htmlquery.Find(p, ".//input[@name]") {
				typ := strings.ToLower(attrOf(in, "type"))
				if typ == "" || typ == "hidden" || typ == "submit" || typ == "button" {
					form[attrOf(in, "name")] = attrOf(in, "value")
				}
			}
			break
		}
	}
	if len(form) == 0 {
		name := attrOf(trigger, "name")
		if name == "" {
			name = "imgContinue"
		}
		v := attrOf(trigger, "value")
		if v == "" {
			v = "Continue to your image..."
		}
		form[name] = v
	}
	return form
}

// insertGatePost 在第一个 extract/search 之前插入 post 步骤；
// 已经有 post 步骤的规则不动（返回 nil）。
func insertGatePost(steps []Step, form map[string]string) []Step {
	for _, st := range steps {
		if st.Action == "post" {
			return nil
		}
	}
	post := Step{Action: "post", Source: "{page_url}", Data: form}
	out := make([]Step, 0, len(steps)+1)
	inserted := false
	for _, st := range steps {
		if !inserted && (st.Action == "extract" || st.Action == "search" || st.Action == "regex") {
			out = append(out, post)
			inserted = true
		}
		out = append(out, st)
	}
	if !inserted {
		return nil
	}
	return out
}

// reBadAttrPath 匹配被写坏的属性路径：@//class/ 、@//src/ 之类。
var reBadAttrPath = regexp.MustCompile(`@//([A-Za-z_][\w:-]*)/`)

// repairSelector 修掉“@//属性/”这种早期工具生成的坏写法。
func repairSelector(sel string) string {
	if !strings.Contains(sel, "@//") {
		return sel
	}
	return reBadAttrPath.ReplaceAllString(sel, "@$1")
}

// ruleNameOf 给报错加上规则名，便于定位是哪条规则。
func ruleNameOf(r *Rule) string {
	if r == nil || r.Name == "" {
		return "规则"
	}
	return r.Name
}

// clipBody 把响应正文存进 {body} 变量，供 regex 步骤在原始 HTML/JSON 上抽取。
// 大页面截断到 4MB，避免一个图集把内存吃满。
func clipBody(body string) string {
	const max = 4 << 20
	if len(body) > max {
		body = body[:max]
	}
	// JSON/JS 里的地址常写成 "https://////cdn..."，把 \/ 还原成 / 之后，
	// 规则里的正则就能统一用 https?://，不用为转义斜杠写两套。
	if strings.Contains(body, "\\/") {
		body = strings.ReplaceAll(body, "\\/", "/")
	}
	return body
}

func attrOf(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

var varRe = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)

// urlNameOf returns the last path segment without extension — the same
// getDefaultImageName() fallback the original Host.kt used.
func urlNameOf(rawURL string) string {
	u := strings.TrimRight(rawURL, "/")
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.IndexByte(u, '.'); i > 0 {
		u = u[:i]
	}
	return u
}

func expandVars(s string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		key := m[1 : len(m)-1]
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
