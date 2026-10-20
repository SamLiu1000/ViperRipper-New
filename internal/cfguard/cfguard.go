// Package cfguard 负责两件事：
//
//  1. 认出 Cloudflare 的“人机验证”拦截页（返回的不是图片/正文，而是挑战页）；
//  2. 保存用户手动过一次验证后拿到的通行证（cf_clearance 等 cookie + 当时的
//     浏览器 UA），并按域名复用给两条纯 HTTP 链路（图床解析、图片下载）。
//
// 为什么必须存 UA：Cloudflare 的 cf_clearance 与发起验证的 User-Agent 绑定，
// 拿验证窗口的 cookie 配另一个 UA 发请求照样被判无效。
package cfguard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultUA 是两条 HTTP 链路默认使用的 UA；拿到验证窗口的真实 UA 后会被替换。
const DefaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// Challenge 表示服务端返回的是人机验证页而不是真实内容。
type Challenge struct {
	Host string
	URL  string
}

func (c *Challenge) Error() string {
	if c == nil {
		return ""
	}
	if c.Host == "" {
		return "需要人机验证"
	}
	return "需要人机验证（" + c.Host + "）"
}

// Session 是某个域名下一次“已验证”的凭证。
type Session struct {
	Cookies   []*http.Cookie `json:"cookies"`
	UserAgent string         `json:"userAgent"`
	At        int64          `json:"at"` // unix 秒
}

// Guard 保存各域名的验证凭证，并在被拦截时通知界面（带防抖）。
type Guard struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	asked     map[string]time.Time // host → 上次通知时间（防抖）
	blocked   map[string]time.Time // host → 最近一次被拦的时间
	defaultUA string

	// OnNeed 在请求被拦截、且该域名近期没通知过时调用。
	OnNeed func(*Challenge)
	// OnPass 在用户完成验证、凭证落到 Guard 后调用。
	OnPass func(host string)
}

// New 创建一个空的 Guard。
func New() *Guard {
	return &Guard{
		sessions:  map[string]*Session{},
		asked:     map[string]time.Time{},
		blocked:   map[string]time.Time{},
		defaultUA: DefaultUA,
	}
}

// Default 是全局单例：host / pipeline 两个包直接用它，省掉层层传参。
var Default = New()

// DomainOf 取出 URL 的主机名（小写、去端口）；解析失败返回空串。
func DomainOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func normalizeDomain(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimPrefix(h, ".")
}

// Set 记录某域名的验证凭证。host 允许传页面主机名，cookie 里的 Domain 优先
// （Cloudflare 的 cf_clearance 通常挂在 .example.com 上，子域名也要能用）。
func (g *Guard) Set(host string, cookies []*http.Cookie, ua string) {
	host = normalizeDomain(host)
	key := host
	for _, c := range cookies {
		d := normalizeDomain(c.Domain)
		if d == "" {
			continue
		}
		if key == "" || key == d || strings.HasSuffix(host, "."+d) {
			key = d
			break
		}
	}
	if key == "" {
		return
	}
	if ua != "" {
		g.mu.Lock()
		g.defaultUA = ua
		g.mu.Unlock()
	}
	g.mu.Lock()
	g.sessions[key] = &Session{Cookies: cookies, UserAgent: ua, At: time.Now().Unix()}
	delete(g.blocked, key) // 拿到凭证，解封
	g.mu.Unlock()

	if g.OnPass != nil {
		g.OnPass(key)
	}
}

// Get 取某域名的凭证（域名按后缀匹配：a.example.com 能用 example.com 的）。
func (g *Guard) Get(host string) *Session {
	host = normalizeDomain(host)
	if host == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sessions[host]; s != nil {
		return s
	}
	for h, s := range g.sessions {
		if h != "" && (strings.HasSuffix(host, "."+h) || host == h) {
			return s
		}
	}
	return nil
}

// CookiesFor 返回发往该 URL 时应带的 cookie。
func (g *Guard) CookiesFor(rawURL string) []*http.Cookie {
	s := g.Get(DomainOf(rawURL))
	if s == nil {
		return nil
	}
	return s.Cookies
}

// UserAgent 返回应使用的 UA：拿到过验证窗口的 UA 就用它，否则用默认值。
func (g *Guard) UserAgent() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.defaultUA == "" {
		return DefaultUA
	}
	return g.defaultUA
}

// Clear 丢掉某域名的凭证（例如凭证过期后再次被拦截）。
func (g *Guard) Clear(host string) {
	key := normalizeDomain(host)
	g.mu.Lock()
	delete(g.sessions, key)
	g.mu.Unlock()
}

// KnownHosts 返回当前持有凭证的域名。
func (g *Guard) KnownHosts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.sessions))
	for h := range g.sessions {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Notify 通知界面“这个域名需要人机验证”，同一域名 20 秒内只通知一次，
// 免得一批图每条都弹。
func (g *Guard) Notify(ch *Challenge) {
	if ch == nil || g.OnNeed == nil {
		return
	}
	host := normalizeDomain(ch.Host)
	g.mu.Lock()
	if t, ok := g.asked[host]; ok && time.Since(t) < 20*time.Second {
		g.mu.Unlock()
		return
	}
	g.asked[host] = time.Now()
	g.mu.Unlock()
	g.OnNeed(ch)
}

// Blocked 表示这个域名刚刚（60 秒内）拦过我们，且还没有新的凭证：
// 这时不要再发请求，直接按“需要验证”处理——反复撞只会让 Cloudflare 关更久。
func (g *Guard) Blocked(rawURL string) bool {
	host := DomainOf(rawURL)
	if host == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for h, t := range g.blocked {
		if h != "" && (host == h || strings.HasSuffix(host, "."+h)) {
			if time.Since(t) < 60*time.Second {
				return true
			}
			delete(g.blocked, h)
			return false
		}
	}
	return false
}

// ShortCircuit 在“刚被拦过、还没新凭证”时直接返回 Challenge，避免再发请求。
func ShortCircuit(rawURL string) error {
	if !Default.Blocked(rawURL) {
		return nil
	}
	ch := &Challenge{Host: DomainOf(rawURL), URL: rawURL}
	Default.Notify(ch)
	return ch
}

// Check 判断一次响应是不是人机验证页；是则返回 *Challenge 并通知界面。
func DetectChallenge(rawURL string, status int, hdr http.Header, body []byte) error {
	host := DomainOf(rawURL)
	if host == "" || !LooksLikeChallenge(status, hdr, body) {
		return nil
	}
	// 上次的凭证已经不管用了：清掉，避免一直拿失效 cookie 重试
	Default.Clear(host)
	Default.markBlocked(host)
	ch := &Challenge{Host: host, URL: rawURL}
	Default.Notify(ch)
	return ch
}

func (g *Guard) markBlocked(host string) {
	host = normalizeDomain(host)
	if host == "" {
		return
	}
	g.mu.Lock()
	g.blocked[host] = time.Now()
	g.mu.Unlock()
}

// LooksLikeChallenge 按响应头 + 正文特征判断是否为 Cloudflare 拦截页。
func LooksLikeChallenge(status int, hdr http.Header, body []byte) bool {
	if hdr != nil {
		if strings.EqualFold(hdr.Get("cf-mitigated"), "challenge") {
			return true
		}
	}
	bodyHit := looksLikeChallengeBody(body)
	// Cloudflare 对脚本通常直接 403/429/503；带了 cloudflare 头就足以判定
	if status == http.StatusForbidden || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		cf := false
		if hdr != nil {
			srv := strings.ToLower(hdr.Get("Server"))
			cf = strings.Contains(srv, "cloudflare") || hdr.Get("CF-Ray") != ""
		}
		if cf || bodyHit {
			return true
		}
	}
	// 有些挑战页用 200 + 一堆 JS 返回
	return bodyHit && status/100 == 2
}

func looksLikeChallengeBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	head := body
	if len(head) > 64*1024 {
		head = head[:64*1024]
	}
	s := strings.ToLower(string(head))
	for _, m := range []string{
		"__cf_chl", "cf_chl_opt", "challenge-platform", "cf-challenge",
		"just a moment...", "attention required! | cloudflare",
		"checking your browser before accessing",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Snapshot 导出凭证用于持久化。
func (g *Guard) Snapshot() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	raw, err := json.Marshal(g.sessions)
	if err != nil {
		return nil
	}
	return raw
}

// Restore 从持久化数据恢复凭证（进程重启后仍可复用，直到凭证过期）。
func (g *Guard) Restore(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var m map[string]*Session
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	g.mu.Lock()
	for h, s := range m {
		if s != nil && len(s.Cookies) > 0 {
			g.sessions[h] = s
			if s.UserAgent != "" {
				g.defaultUA = s.UserAgent
			}
		}
	}
	g.mu.Unlock()
}
