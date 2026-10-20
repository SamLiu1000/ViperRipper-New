package forum

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/antchfx/htmlquery"
	xhtml "golang.org/x/net/html"
	"viperipper/internal/proxy"
)

// ForumClient talks to the vipergirls vr.php XML API (mirrors of the original
// vgapi SAX parser) plus login/thank endpoints.

type ImageRef struct {
	PageURL  string `json:"mainUrl"`
	ThumbURL string `json:"thumbUrl"`
	Host     string `json:"host"`
}

type PostRef struct {
	ForumPostID int64      `json:"postId"`
	FloorNo     int        `json:"number"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Hosts       string     `json:"hosts"`
	Images      []ImageRef `json:"images"`
}

type ThreadRef struct {
	ForumThreadID int64     `json:"threadId"`
	Title         string    `json:"title"`
	Security      string    `json:"-"`
	Forum         string    `json:"forum"`
	Posts         []PostRef `json:"posts"`
	Error         string    `json:"error,omitempty"`
}

type Client struct {
	mu           sync.Mutex
	http         *http.Client
	apiBase      string // e.g. https://viper.click/vr.php
	forum        string // e.g. vipergirls.to
	mirrors      []string
	active       string // 最近一次连通的域名（镜像可用时）
	limiter      *rate.Limiter
	loggedIn     bool
	username     string
	HostResolver func(pageURL string) string // host rule name lookup
}

func NewClient(apiBase, forumDomain string, ratePerSec int) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
			Transport: &http.Transport{
				Proxy: proxy.Func(),
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil
			},
		},
		apiBase: apiBase,
		forum:   normalizeDomain(forumDomain),
		limiter: rate.NewLimiter(rate.Limit(ratePerSec), 1),
	}
}

func (c *Client) SetRate(n int) {
	if n < 1 {
		n = 1
	}
	c.mu.Lock()
	c.limiter = rate.NewLimiter(rate.Limit(n), 1)
	c.mu.Unlock()
}

// ---------- vr.php XML ----------

type xmlImage struct {
	PageURL  string `xml:"main_url,attr"`
	ThumbURL string `xml:"thumb_url,attr"`
	Type     string `xml:"type,attr"`
}

type xmlPost struct {
	ID      int64      `xml:"id,attr"`
	FloorNo int        `xml:"number,attr"`
	Title   string     `xml:"title,attr"`
	Images  []xmlImage `xml:"image"`
}

type xmlEnvelope struct {
	XMLName xml.Name
	Error   struct {
		Details string `xml:"details,attr"`
	} `xml:"error"`
	User struct {
		Hash string `xml:"hash,attr"`
	} `xml:"user"`
	Forum struct {
		Title string `xml:"title,attr"`
	} `xml:"forum"`
	Thread struct {
		ID    int64  `xml:"id,attr"`
		Title string `xml:"title,attr"`
	} `xml:"thread"`
	Posts []xmlPost `xml:"post"`
}

func (c *Client) fetchXML(query string, value string) (*ThreadRef, error) {
	c.mu.Lock()
	lim := c.limiter
	c.mu.Unlock()
	if err := lim.Wait(context.Background()); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	u := c.apiBase + "?" + query + "=" + url.QueryEscape(value)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("vr.php returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env xmlEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("vr.php XML parse: %w", err)
	}
	t := &ThreadRef{
		ForumThreadID: env.Thread.ID,
		Title:         strings.TrimSpace(env.Thread.Title),
		Forum:         env.Forum.Title,
		Security:      env.User.Hash,
	}
	if env.Error.Details != "" {
		t.Error = env.Error.Details
	}
	if t.Title == "" && t.Error == "" {
		t.Error = "empty response from vr.php"
	}
	proxy := c.forumHost()
	for _, xp := range env.Posts {
		title := strings.TrimSpace(xp.Title)
		if title == "" {
			title = t.Title
		}
		pr := PostRef{
			ForumPostID: xp.ID,
			FloorNo:     xp.FloorNo,
			Title:       title,
			URL:         fmt.Sprintf("https://%s/threads/%d?p=%d&viewfull=1#post%d", proxy, t.ForumThreadID, xp.ID, xp.ID),
		}
		hostCount := map[string]int{}
		var order []string
		addImg := func(im ImageRef) {
			h := ""
			if c.HostResolver != nil {
				h = c.HostResolver(im.PageURL)
			}
			if h == "" {
				h = domainOf(im.PageURL)
			}
			if _, ok := hostCount[h]; !ok {
				order = append(order, h)
			}
			hostCount[h]++
			pr.Images = append(pr.Images, im)
		}
		for _, xi := range xp.Images {
			main := strings.TrimSpace(xi.PageURL)
			if main == "" {
				continue
			}
			main = strings.Replace(main, "http://", "https://", 1)
			thumb := strings.TrimSpace(xi.ThumbURL)
			if thumb != "" {
				thumb = strings.Replace(thumb, "http://", "https://", 1)
			}
			if xi.Type != "linked" {
				continue
			}
			// 楼主用"缩略图 + 引用楼层"发图时，vr.php 的 main_url 是论坛楼层
			// 链接而不是图床链接。直接当图片页解析会让所有图共享帖子 slug 文件
			// 名并互相覆盖。被引用的楼层往往是转贴合集（跟进去会带出成百上千
			// 张别的图），所以这里不跟进楼层，而是用缩略图直链推导原图页
			// （pixhost: t1.pixhost.to/thumbs/NNN/x.jpg → pixhost.to/show/NNN/x.jpg）。
			if domainOf(main) == proxy && strings.Contains(main, "p=") {
				if su := pixhostShowOf(thumb); su != "" {
					addImg(ImageRef{PageURL: su, ThumbURL: thumb})
					continue
				}
			}
			addImg(ImageRef{PageURL: main, ThumbURL: thumb})
		}
		if len(pr.Images) == 0 {
			continue // original drops image-less posts
		}
		var parts []string
		for _, h := range order {
			parts = append(parts, fmt.Sprintf("%s (%d)", h, hostCount[h]))
		}
		pr.Hosts = strings.Join(parts, ", ")
		t.Posts = append(t.Posts, pr)
	}
	return t, nil
}

// rePixhostThumb 匹配 pixhost 缩略图直链（tNN.pixhost.to|pixhost.to|pixhost.cc）。
var rePixhostThumb = regexp.MustCompile(`^(?:(?:t\d+|www)\.)?(pixhost\.(?:to|cc))/thumbs/(.+)$`)

// pixhostShowOf 把 pixhost 缩略图直链推导成对应的原图页 URL；
// 非 pixhost 缩略图返回 ""。引用楼层的 vr.php 记录里唯一可靠的信息
// 就是缩略图，原图页按目录号/文件名与缩略图一一对应。
func pixhostShowOf(thumb string) string {
	m := rePixhostThumb.FindStringSubmatch(strings.TrimPrefix(thumb, "https://"))
	if m == nil {
		return ""
	}
	return "https://pixhost.to/show/" + m[2]
}

func (c *Client) forumHost() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != "" {
		return c.active
	}
	if c.forum != "" {
		return c.forum
	}
	return "vipergirls.to"
}

// SetDomain 运行时切换论坛域名（设置里改完立刻生效，不用重启）。
func (c *Client) SetDomain(d string) {
	d = normalizeDomain(d)
	c.mu.Lock()
	if d != c.forum {
		c.forum = d
		c.active = "" // 换主域名后重新探测
	}
	c.mu.Unlock()
}

// SetMirrors 设置镜像域名列表（逗号/分号/空白分隔）。主域名连不上时按顺序尝试。
func (c *Client) SetMirrors(list string) {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' || r == '|'
	}) {
		h := normalizeDomain(f)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	c.mu.Lock()
	c.mirrors = out
	c.active = ""
	c.mu.Unlock()
}

// HostLabel 给日志/界面看的当前域名说明。
func (c *Client) HostLabel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	host := c.active
	if host == "" {
		host = c.forum
	}
	if host == "" {
		host = "vipergirls.to"
	}
	if len(c.mirrors) == 0 {
		return host
	}
	return fmt.Sprintf("%s（备用镜像 %d 个）", host, len(c.mirrors))
}

// hostCandidates 返回 主域名 + 镜像（去重，按顺序）。
func (c *Client) hostCandidates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = normalizeDomain(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	add(c.forum)
	if c.forum == "" {
		add("vipergirls.to")
	}
	for _, m := range c.mirrors {
		add(m)
	}
	return out
}

// normalizeDomain 把用户粘进来的 "https://x.com/" 归一成 "x.com"。
func normalizeDomain(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

func domainOf(rawURL string) string {
	u := strings.TrimPrefix(rawURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// FetchPost resolves a single post by id.
func (c *Client) FetchPost(postID int64) (*ThreadRef, error) {
	return c.fetchXML("p", strconv.FormatInt(postID, 10))
}

// FetchThread resolves a whole thread by id.
func (c *Client) FetchThread(threadID int64) (*ThreadRef, error) {
	return c.fetchXML("t", strconv.FormatInt(threadID, 10))
}

// ---------- 网页版解析（vr.php 被拒时的降级） ----------

// FetchThreadWeb 用当前登录会话直接抓论坛网页版 showthread 页并解析图片。
// vr.php（viper.click）的抓取账号看不了权限帖时，这条路走用户自己的身份：
// 浏览器能看的帖子，这里也能看。未登录时游客可见的内容同样能解析。
// threadID 和 postID 至少给一个；两个都给时优先定位到该楼层。
func (c *Client) FetchThreadWeb(threadID, postID int64) (*ThreadRef, error) {
	if threadID == 0 && postID == 0 {
		return nil, fmt.Errorf("网页解析需要 thread 或 post id")
	}
	var lastErr error
	for _, host := range c.hostCandidates() {
		ref, err := c.fetchThreadWebAt(host, threadID, postID)
		if err == nil {
			c.mu.Lock()
			c.active = host
			c.mu.Unlock()
			return ref, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("网页解析失败")
	}
	return nil, lastErr
}

func (c *Client) fetchThreadWebAt(host string, threadID, postID int64) (*ThreadRef, error) {
	u := "https://" + host + "/showthread.php?"
	if threadID > 0 {
		u += "t=" + strconv.FormatInt(threadID, 10)
		if postID > 0 {
			u += "&p=" + strconv.FormatInt(postID, 10)
		}
	} else {
		u += "p=" + strconv.FormatInt(postID, 10)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("showthread 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	doc, perr := htmlquery.Parse(strings.NewReader(string(body)))
	if perr != nil {
		return nil, fmt.Errorf("showthread 页面解析失败：%w", perr)
	}

	title := ""
	if n := htmlquery.FindOne(doc, "//title"); n != nil {
		title = strings.TrimSpace(htmlquery.InnerText(n))
		title = strings.TrimSpace(strings.TrimSuffix(title, " - vBulletin"))
	}

	// vB5 楼层容器是 li#js-post-<postid>：指定楼层时优先只取该楼层内的图，
	// 找不到容器（结构变了/单楼模式）就退回全页。
	root := doc
	if postID > 0 {
		if n := htmlquery.FindOne(doc, "//li[@id='js-post-"+strconv.FormatInt(postID, 10)+"']"); n != nil {
			root = n
		}
	}
	imgs := extractWebImages(root, c.forumHost())
	if len(imgs) == 0 && webLooksPermissionDenied(doc) {
		return nil, fmt.Errorf("论坛页面显示无权限（请确认 ViperRipper 里的登录仍然有效）")
	}

	// 面包屑里的板块名（指向 forums/ 的链接，取最后一个）：与 XML 通道的
	// env.Forum.Title 对齐。早前这里填的是站点域名，和 XML 通道不一致，
	// 会导致 thread.forum 被覆盖、下载目录推导变化而自动搬迁文件。
	forumName := ""
	{
		nodes := htmlquery.Find(doc, "//a[contains(@href,'/forums/')]")
		for _, n := range nodes {
			txt := strings.TrimSpace(htmlquery.InnerText(n))
			if txt != "" {
				forumName = txt
			}
		}
	}

	ref := &ThreadRef{ForumThreadID: threadID, Title: title, Forum: forumName}
	pr := PostRef{FloorNo: 1, Title: title, URL: u}
	if postID > 0 {
		pr.ForumPostID = postID
	} else {
		pr.ForumPostID = threadID
	}
	hostCount := map[string]int{}
	var order []string
	for _, main := range imgs {
		h := ""
		if c.HostResolver != nil {
			h = c.HostResolver(main)
		}
		if h == "" {
			h = domainOf(main)
		}
		if _, ok := hostCount[h]; !ok {
			order = append(order, h)
		}
		hostCount[h]++
		pr.Images = append(pr.Images, ImageRef{PageURL: main, Host: h})
	}
	var parts []string
	for _, h := range order {
		parts = append(parts, fmt.Sprintf("%s (%d)", h, hostCount[h]))
	}
	pr.Hosts = strings.Join(parts, ", ")
	if len(pr.Images) > 0 {
		ref.Posts = append(ref.Posts, pr)
	}
	return ref, nil
}

// extractWebImages 从帖子 HTML 里提取外链图床图片：优先取“带图链接”的
// a[href]（论坛贴图的标准形态 <a href="图床"><img></a>，href 就是原图），
// 再补充 img 的 src / data-src。论坛自有资源（头像、表情、图标、站内路径）
// 一律跳过。
func extractWebImages(root *xhtml.Node, forumHost string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		u = strings.Replace(u, "http://", "https://", 1)
		if seen[u] || !webIsExternalImage(u, forumHost) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	// 带图链接：href 是图片直链就用它；否则（图床服务主页）用图内的
	// img src。a 内的 img 不再单独收录，避免缩略图混进主图列表。
	inA := map[*xhtml.Node]bool{}
	for _, a := range htmlquery.Find(root, "//a[@href]") {
		href := strings.TrimSpace(attrValue(a, "href"))
		im := htmlquery.FindOne(a, ".//img")
		if href == "" || im == nil {
			continue
		}
		inA[im] = true
		if webIsExternalImage(href, forumHost) && reImageExt.MatchString(href) {
			add(href)
			continue
		}
		for _, at := range []string{"src", "data-src"} {
			if v := attrValue(im, at); v != "" {
				add(v)
				break
			}
		}
	}
	for _, im := range htmlquery.Find(root, "//img") {
		if inA[im] {
			continue
		}
		for _, at := range []string{"src", "data-src"} {
			if v := attrValue(im, at); v != "" {
				add(v)
				break
			}
		}
	}
	return out
}

// webIsExternalImage 判断 URL 是否像“贴给别人的图床图”而不是论坛自己的资源。
func webIsExternalImage(u, forumHost string) bool {
	l := strings.ToLower(u)
	if !strings.HasPrefix(l, "https://") {
		return false
	}
	if h := domainOf(u); h != "" && (h == forumHost || strings.HasSuffix(h, "."+forumHost)) {
		return false
	}
	for _, k := range []string{"/images/", "/core/", "/customdivs/", "/static/", "/misc/",
		"avatar", "smilies", "/favicon", "/css", "/js/", "image.php", "/clientscript/"} {
		if strings.Contains(l, k) {
			return false
		}
	}
	return true
}

// webLooksPermissionDenied 识别 vBulletin 的“无权限”提示页（有权限问题的
// 页面没有任何图床图，且提示文案里带 permission / 权限）。
func webLooksPermissionDenied(doc *xhtml.Node) bool {
	for _, n := range htmlquery.Find(doc, "//body") {
		txt := strings.ToLower(htmlquery.InnerText(n))
		if strings.Contains(txt, "permission") || strings.Contains(txt, "您没有权限") || strings.Contains(txt, "没有权限") {
			return true
		}
	}
	return false
}

// reImageExt 识别“图片直链”扩展名（图床服务主页如 imgbb.com/ 不算）。
var reImageExt = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|webp|bmp|avif)$`)

func attrValue(n *xhtml.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

// ---------- link parsing ----------

var (
	reThread  = regexp.MustCompile(`(?:threads/(\d+)|showthread\.php(?:\?(?:.*&)?t=|/)(\d+))`)
	rePost    = regexp.MustCompile(`(?:#post-?(\d+)|p=(\d+)|(?:view=full.*p=(\d+)))`)
	reBareNum = regexp.MustCompile(`^(\d+)$`)
)

// ParseLink extracts thread and/or post ids from user input.
func ParseLink(s string) (threadID, postID int64, err error) {
	s = strings.TrimSpace(s)
	if m := reBareNum.FindStringSubmatch(s); m != nil {
		postID, _ = strconv.ParseInt(m[1], 10, 64)
		return 0, postID, nil
	}
	if m := rePost.FindStringSubmatch(s); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				postID, _ = strconv.ParseInt(g, 10, 64)
				break
			}
		}
	}
	tm := reThread.FindStringSubmatch(s)
	if tm != nil {
		for _, g := range tm[1:] {
			if g != "" {
				threadID, _ = strconv.ParseInt(g, 10, 64)
				break
			}
		}
	}
	if threadID == 0 && postID == 0 {
		return 0, 0, fmt.Errorf("无法识别链接：%s", s)
	}
	return threadID, postID, nil
}

// ---------- auth ----------

// Login performs a vBulletin login against the forum domain. vBulletin returns
// HTTP 200 even on bad credentials, so success is verified by re-checking the
// forum index afterwards (CheckLogin parses the navbar username).
func (c *Client) Login(username, password string) error {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return fmt.Errorf("请先填写用户名和密码")
	}
	c.mu.Lock()
	c.username = username
	c.mu.Unlock()
	form := url.Values{}
	form.Set("vb_login_username", username)
	form.Set("vb_login_password", password)
	form.Set("vb_login_password_hint", "")
	form.Set("cookieuser", "1")
	form.Set("do", "login")
	form.Set("s", "")
	form.Set("securitytoken", "guest")
	form.Set("vb_login_md5password", "")
	form.Set("vb_login_md5password_utf", "")
	// 主域名连不上就依次试镜像（连不上 ≠ 密码错，要区分开）
	var netErr error
	for _, host := range c.hostCandidates() {
		name, err := c.loginAt(host, form)
		if err == nil {
			c.mu.Lock()
			c.active = host
			c.loggedIn = true
			c.username = name
			c.mu.Unlock()
			return nil
		}
		if err == errBadCredentials {
			return fmt.Errorf("登录失败：用户名或密码错误")
		}
		netErr = err
	}
	if netErr != nil {
		return fmt.Errorf("登录失败：%v", netErr)
	}
	return fmt.Errorf("登录失败：用户名或密码错误")
}

var errBadCredentials = fmt.Errorf("账号或密码不正确")

// loginAt 在指定域名上提交登录表单，并用同一个域名校验会话
// （cookie 是按域名存的，拿主域名验镜像的登录必然失败）。
func (c *Client) loginAt(host string, form url.Values) (string, error) {
	u := "https://" + host + "/login.php?do=login"
	req, err := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 密码错也返回 200，只能靠首页导航栏确认
	name, cerr := c.checkLoginAt(host)
	if cerr != nil {
		return "", errBadCredentials
	}
	return name, nil
}

// ImportCookies installs cookies captured from the web-login window (or saved
// settings) into the client jar, then verifies and records the session.
func (c *Client) ImportCookies(cookies []*http.Cookie) error {
	if len(cookies) == 0 {
		return fmt.Errorf("没有可导入的登录凭据")
	}
	u, err := url.Parse("https://" + c.forumHost() + "/")
	if err != nil {
		return err
	}
	if j := c.http.Jar; j != nil {
		j.SetCookies(u, cookies)
	}
	name, err := c.CheckLogin()
	if err != nil {
		return fmt.Errorf("登录凭据无效或已过期")
	}
	c.mu.Lock()
	c.loggedIn = true
	c.username = name
	c.mu.Unlock()
	return nil
}

// CheckLogin fetches the forum index and decides login state from the navbar:
// guests see the vb_login_username form field; members' navbar carries their
// member.php profile link with the display name.
func (c *Client) CheckLogin() (string, error) {
	var lastErr error
	for _, host := range c.hostCandidates() {
		name, err := c.checkLoginAt(host)
		if err == nil {
			c.mu.Lock()
			c.active = host
			c.mu.Unlock()
			return name, nil
		}
		lastErr = err
	}
	c.mu.Lock()
	c.loggedIn = false
	c.mu.Unlock()
	if lastErr == nil {
		lastErr = fmt.Errorf("未登录")
	}
	return "", lastErr
}

// checkLoginAt 针对指定域名判断登录状态（镜像的 cookie 是按域名存的，
// 所以必须用同一个域名校验，不能拿主域名去验镜像的会话）。
func (c *Client) checkLoginAt(host string) (string, error) {
	req, err := http.NewRequest("GET", "https://"+host+"/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	page := string(body)
	if resp.StatusCode != 200 || strings.Contains(page, `name="vb_login_username"`) {
		return "", fmt.Errorf("未登录")
	}
	name := reNavName.FindStringSubmatch(page)
	c.mu.Lock()
	c.loggedIn = true
	if len(name) > 1 {
		display := strings.TrimSpace(reTag.ReplaceAllString(name[1], ""))
		display = strings.TrimSpace(html.UnescapeString(display))
		if display != "" {
			c.username = display
		}
	}
	user := c.username
	c.mu.Unlock()
	return user, nil
}

func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedIn
}

// Logout drops the forum session: a fresh cookie jar, cleared state.
func (c *Client) Logout() {
	c.mu.Lock()
	defer c.mu.Unlock()
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar
	c.loggedIn = false
	c.username = ""
}

// CurrentUser returns the recorded display name ("" when logged out).
func (c *Client) CurrentUser() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.username
}

// SessionCookies returns the forum cookies currently in the client jar.
func (c *Client) SessionCookies() []*http.Cookie {
	u, err := url.Parse("https://" + c.forumHost() + "/")
	if err != nil || c.http.Jar == nil {
		return nil
	}
	return c.http.Jar.Cookies(u)
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// vBulletin navbar profile link: <a href="member.php?u=123" ...>Name</a>;
// the display name may be wrapped in extra tags, so capture the raw inner
// HTML and strip tags afterwards.
var reNavName = regexp.MustCompile(`(?i)member\.php\?u=\d+"[^>]*>(.*?)</a>`)
var reTag = regexp.MustCompile(`<[^>]*>`)

// Thank leaves a post-thanks; requires Login first.
func (c *Client) Thank(postID int64) error {
	var lastErr error
	for _, host := range c.hostCandidates() {
		err := c.thankAt(host, postID)
		if err == nil {
			c.mu.Lock()
			c.active = host
			c.mu.Unlock()
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (c *Client) thankAt(host string, postID int64) error {
	u := fmt.Sprintf("https://%s/thanks.php?do=post_thanks_add&p=%d", host, postID)
	req, err := http.NewRequest("POST", u, strings.NewReader("securitytoken="+url.QueryEscape("guest")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://"+host+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("thank failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---------- export ----------

func MarshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
