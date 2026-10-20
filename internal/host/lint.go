package host

// 规则体检：不做网络请求，只做静态检查。目的是"改一次覆盖所有 host"——
// 语法、变量、步骤顺序、域名这些错误靠人肉逐条试太慢，也很容易漏。

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/antchfx/xpath"
)

// LintIssue 一条体检结论。Level: error（规则必然跑不通）/ warn（可能有问题）。
type LintIssue struct {
	Rule    string `json:"rule"`
	ID      string `json:"id,omitempty"`
	Level   string `json:"level"`
	Step    int    `json:"step"` // 1-based，0 表示整条规则
	Message string `json:"message"`
}

var knownVars = map[string]bool{
	"page_url": true, "url_name": true, "thumb_url": true,
	"image_url": true, "filename": true,
	// {body}：get/post 拿到的原始响应正文，供 regex 在 JSON/脚本里捞直链
	"body": true,
}

// uiLang 决定体检/校验消息用哪种语言（"zh" / "en"）。由 App.SetUILang 同步。
var uiLang = "zh"

// SetLang 设置后端生成文案的语言。
func SetLang(l string) {
	if l == "en" || l == "zh" {
		uiLang = l
	}
}

// pick 按当前语言选中文或英文。
func pick(zh, en string) string {
	if uiLang == "en" {
		return en
	}
	return zh
}

// LintRules 体检一组规则，按规则顺序返回问题列表。
func LintRules(rules []*Rule) []LintIssue {
	var out []LintIssue
	for _, r := range rules {
		out = append(out, LintRule(r)...)
	}
	return out
}

// LintRule 体检单条规则。
func LintRule(r *Rule) []LintIssue {
	var iss []LintIssue
	add := func(level string, step int, zh, en string, a ...any) {
		iss = append(iss, LintIssue{Rule: r.Name, ID: r.ID, Level: level, Step: step, Message: fmt.Sprintf(pick(zh, en), a...)})
	}

	if strings.TrimSpace(r.Name) == "" {
		add("error", 0, "规则没有名字", "Rule has no name")
	}
	if len(r.Domains) == 0 {
		add("error", 0, "没有匹配域名，永远不会被命中", "No domains — this rule will never match")
	}
	seen := map[string]bool{}
	for _, d := range r.Domains {
		nd := normalizeDomain(d)
		if nd == "" {
			add("warn", 0, "域名 %q 归一化后为空", "Domain %q is empty after normalisation", d)
			continue
		}
		if seen[nd] {
			add("warn", 0, "域名 %q 重复", "Duplicate domain %q", nd)
		}
		seen[nd] = true
		if strings.Contains(d, "*") {
			add("warn", 0, "域名 %q 含通配符，匹配是按 host 后缀做的，通配符不会生效", d)
		}
	}
	if r.Referer != "" {
		if u, err := url.Parse(r.Referer); err != nil || u.Host == "" {
			add("warn", 0, "referer %q 不是合法 URL（可留空，会自动用图片页站点根）", "referer %q is not a valid URL (leave empty to use the image page's site root)", r.Referer)
		}
	}
	if len(r.Steps) == 0 {
		add("error", 0, "规则没有任何步骤", "Rule has no steps")
		return iss
	}

	haveDoc := false
	haveImageURL := false
	haveFilename := false
	for i, st := range r.Steps {
		n := i + 1
		switch st.Action {
		case "get", "post":
			if strings.TrimSpace(st.Source) == "" {
				add("error", n, "%s 步骤缺少 url", "%s step is missing a URL", st.Action)
			} else if !strings.Contains(st.Source, "{page_url}") && !strings.Contains(st.Source, "http") {
				add("warn", n, "%s 步骤的 url 既不是 {page_url} 也不含 http：%q", "%s step URL is neither {page_url} nor contains http: %q", st.Action, st.Source)
			}
			if st.Action == "post" && len(st.Data) == 0 && !st.GrabForm {
				add("warn", n, "post 步骤没有任何表单字段", "POST step has no form fields")
			}
			haveDoc = true
		case "extract":
			if !haveDoc {
				add("error", n, "extract 之前没有 get/post，没有页面可取", "extract without a preceding get/post — no page to read")
			}
			if strings.TrimSpace(st.Selector) == "" {
				add("error", n, "extract 缺少 selector", "extract is missing a selector")
			} else if _, err := xpath.Compile(st.Selector); err != nil {
				add("error", n, "XPath 语法错误：%s", "XPath syntax error: %s", st.Selector)
			}
			if strings.TrimSpace(st.Attr) == "" {
				add("warn", n, "extract 没有 attr（默认取不到内容）", "extract has no attr (nothing will be read by default)")
			}
			if looksThumbSelector(st.Selector) {
				add("warn", n, "选择器 %q 看起来指向缩略图/预览，确认取到的是原图", "Selector %q looks like a thumbnail/preview — make sure it yields the full image", st.Selector)
			}
			if st.Field == "image_url" {
				haveImageURL = true
			}
			if st.Field == "filename" {
				haveFilename = true
			}
		case "search":
			if strings.TrimSpace(st.Selector) == "" {
				add("error", n, "search 缺少正则表达式", "search is missing a regex")
			} else if _, err := regexp.Compile(st.Selector); err != nil {
				add("error", n, "正则语法错误：%v", "Regex syntax error: %v", err)
			}
			if st.Field == "image_url" {
				haveImageURL = true
			}
		case "regex":
			if strings.TrimSpace(st.Selector) == "" {
				add("error", n, "regex 缺少表达式", "regex is missing an expression")
			} else if _, err := regexp.Compile(st.Selector); err != nil {
				add("error", n, "正则语法错误：%v", "Regex syntax error: %v", err)
			}
			if st.Field == "image_url" {
				haveImageURL = true
			}
		case "set":
			if strings.TrimSpace(st.Field) == "" {
				add("error", n, "set 步骤缺少 field", "set step is missing a field")
			}
			if st.Field == "image_url" {
				haveImageURL = true
			}
		case "set-cookie":
			if st.Name == "" {
				add("error", n, "set-cookie 缺少 cookie 名", "set-cookie is missing a cookie name")
			}
		default:
			add("error", n, "未知步骤类型 %q", "Unknown step type %q", st.Action)
		}
		// 变量引用必须在白名单内
		for _, raw := range []string{st.Source, st.Replace, st.Default, st.Value} {
			for _, v := range referencedVars(raw) {
				if !knownVars[v] {
					add("warn", n, "引用了未知变量 {%s}", "References unknown variable {%s}", v)
				}
			}
		}
	}

	if !haveImageURL {
		add("error", 0, "没有任何步骤产出 image_url", "No step produces image_url")
	}
	if !haveFilename && !RuleNeedsThumb(r) {
		add("warn", 0, "没有 filename 步骤，下载会退化为 001.jpg 编号命名", "No filename step — downloads fall back to 001.jpg numbering")
	}
	return iss
}

var varRef = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)

func referencedVars(s string) []string {
	var out []string
	for _, m := range varRef.FindAllStringSubmatch(s, -1) {
		// ${1} / $1 是正则替换里的捕获组，不是变量
		if isDigits(m[1]) {
			continue
		}
		out = append(out, m[1])
	}
	return out
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func looksThumbSelector(sel string) bool {
	l := strings.ToLower(sel)
	for _, k := range []string{"thumb", "preview", "small", "icon"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// Lint 体检当前已加载的全部规则（内置 + 自定义）。
func (r *Registry) Lint() []LintIssue {
	r.mu.RLock()
	rules := make([]*Rule, 0, len(r.rules))
	for _, rule := range r.rules {
		cp := *rule
		rules = append(rules, &cp)
	}
	r.mu.RUnlock()
	return LintRules(rules)
}

// ValidateRule 只拦“写错了、一定跑不通”的结构/语法问题：
// XPath 或正则编译失败、动作名拼错、extract/search/regex 缺表达式、get/post 缺 URL。
// 不要求“必须产出 image_url”之类的内容完整性 —— 编辑到一半也想能存。
func ValidateRule(r *Rule) error {
	if r == nil {
		return fmt.Errorf("%s", pick("空规则", "empty rule"))
	}
	for i, st := range r.Steps {
		n := i + 1
		switch st.Action {
		case "get", "post":
			if strings.TrimSpace(st.Source) == "" {
				return fmt.Errorf(pick("第 %d 步 %s 缺少 url", "Step %d (%s) is missing a URL"), n, st.Action)
			}
		case "extract":
			if strings.TrimSpace(st.Selector) == "" {
				return fmt.Errorf(pick("第 %d 步 extract 缺少选择器", "Step %d: extract is missing a selector"), n)
			}
			if _, err := xpath.Compile(repairSelector(st.Selector)); err != nil {
				return fmt.Errorf(pick("第 %d 步的 XPath 写错了：%v（属性要写成 @class，不是 @//class/）", "Step %d: bad XPath: %v (write attributes as @class, not @//class/)"), n, err)
			}
		case "search", "regex":
			if strings.TrimSpace(st.Selector) == "" {
				return fmt.Errorf(pick("第 %d 步 %s 缺少正则", "Step %d: %s is missing a regex"), n, st.Action)
			}
			if _, err := regexp.Compile(st.Selector); err != nil {
				return fmt.Errorf(pick("第 %d 步的正则写错了：%v", "Step %d: bad regex: %v"), n, err)
			}
		case "set-cookie":
			if strings.TrimSpace(st.Name) == "" {
				return fmt.Errorf(pick("第 %d 步 set-cookie 缺少 cookie 名", "Step %d: set-cookie is missing a cookie name"), n)
			}
		case "set":
			if strings.TrimSpace(st.Field) == "" {
				return fmt.Errorf(pick("第 %d 步 set 缺少字段名", "Step %d: set is missing a field name"), n)
			}
		default:
			return fmt.Errorf(pick("第 %d 步的动作 %q 不认识（可用：get/post/extract/search/regex/set/set-cookie）", "Step %d: unknown action %q (available: get/post/extract/search/regex/set/set-cookie)"), n, st.Action)
		}
	}
	return nil
}
