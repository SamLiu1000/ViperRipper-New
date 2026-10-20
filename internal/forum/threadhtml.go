package forum

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// threadhtml.go — vr.php XML 对部分主题返回 "User Does Not Have Enough
// Permission"（游客权限不足）时的回退通道：直接抓论坛主题 HTML 页解析
// 楼层文字与图片链接，组装出与 XML 相同的 ThreadRef。

const (
	maxHTMLPages     = 24
	htmlFetchTimeout = 90 * time.Second
)

// Floor 是单个楼层的解析结果。HTML 回退通道和楼层选择器的分批加载
// 通道（app 侧 floors:page 事件）共用。
type Floor struct {
	ID          int64
	FloorNo     int
	Text        string // 楼层正文文字（图册名）
	SourceTitle string // vr.php title 回退（XML 骨架只有这个）
	Images      []FloorImg
}

type FloorImg struct {
	Main  string
	Thumb string
}

var (
	// <a href="原图/图床页"><img src="缩略图"></a> —— vB 里 [url][img] 组合
	reAnchor = regexp.MustCompile(`(?is)<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reImgSrc = regexp.MustCompile(`(?is)<img[^>]*\ssrc="([^"]+)"[^>]*>`)
	// 楼层号：name="postcount<pid>" 后的第一个纯数字
	// vB4 楼层号：<a name="post267251807" href="..." class="postcounter">#2</a>
	rePostCount = regexp.MustCompile(`name="post(\d+)"[^>]*class="postcounter">#(\d+)</a>`)
	reNextPage  = regexp.MustCompile(`<a[^>]+href="([^"]+)"[^>]*rel="next"|<a[^>]+rel="next"[^>]*href="([^"]+)"`)
	reH1        = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	reTitleTag  = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	reThreadsID = regexp.MustCompile(`/threads/(\d+)`)
)

// floorImages 提取楼层里的图片链接：a 包裹的取 href 为主图、img src 为缩略图，
// 裸 img 直接作主图。表情/头像/论坛 UI 图标按路径特征排除。
func floorImages(seg string) []FloorImg {
	var out []FloorImg
	var aRanges [][]int
	anchors := reAnchor.FindAllStringSubmatchIndex(seg, -1)
	for _, m := range anchors {
		aRanges = append(aRanges, []int{m[0], m[1]})
		href := cleanImgURL(seg[m[2]:m[3]])
		inner := seg[m[4]:m[5]]
		if is := reImgSrc.FindStringSubmatch(inner); is != nil {
			thumb := cleanImgURL(is[1])
			if !isUIImg(href) && !isUIImg(thumb) {
				out = append(out, FloorImg{Main: href, Thumb: thumb})
			}
		}
	}
	inA := func(pos int) bool {
		for _, r := range aRanges {
			if pos >= r[0] && pos < r[1] {
				return true
			}
		}
		return false
	}
	for _, m := range reImgSrc.FindAllStringSubmatchIndex(seg, -1) {
		if inA(m[0]) {
			continue
		}
		src := cleanImgURL(seg[m[2]:m[3]])
		if src == "" || isUIImg(src) {
			continue
		}
		out = append(out, FloorImg{Main: src})
	}
	return out
}

// cleanImgURL 去转义、补协议。
func cleanImgURL(u string) string {
	u = strings.TrimSpace(html.UnescapeString(u))
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	u = strings.Replace(u, "http://", "https://", 1)
	return u
}

// isUIImg 排除论坛自带的表情、头像、按钮、图标。
func isUIImg(u string) bool {
	if u == "" {
		return true
	}
	low := strings.ToLower(u)
	if !strings.Contains(low, "://") {
		return true // 相对路径一律是论坛资源（reputation 图标、表情等）
	}
	for _, s := range []string{"smilies", "smiley", "avatar", "/images/", "statusicon", "buttons", "clear.gif", "misc.php", "/icons/", "reputation", "/styles/"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// threadTitleFromPage 从页面取主题标题：h1 优先，退 <title> 首段。
func threadTitleFromPage(page string) string {
	if m := reH1.FindStringSubmatch(page); m != nil {
		t := strings.TrimSpace(html.UnescapeString(reTagAll.ReplaceAllString(m[1], "")))
		if t != "" {
			return t
		}
	}
	if m := reTitleTag.FindStringSubmatch(page); m != nil {
		t := strings.TrimSpace(html.UnescapeString(reTagAll.ReplaceAllString(m[1], "")))
		if i := strings.Index(t, " - "); i > 0 {
			t = t[:i]
		}
		return strings.TrimSpace(t)
	}
	return ""
}

// assembleThread 把解析出的楼层组装成 ThreadRef（与 XML 通道同样的
// host 解析、hosts 统计、无图楼层丢弃逻辑）。
func (c *Client) assembleThread(threadID int64, title string, floors []Floor) *ThreadRef {
	t := &ThreadRef{ForumThreadID: threadID, Title: title}
	for _, f := range floors {
		pr := PostRef{
			ForumPostID: f.ID,
			FloorNo:     f.FloorNo,
			Title:       f.Text,
			URL:         fmt.Sprintf("https://%s/threads/%d?p=%d&viewfull=1#post%d", c.forumHost(), threadID, f.ID, f.ID),
		}
		// 与 XML 通道一致：hosts 统计按楼层独立算
		hostCount := map[string]int{}
		var order []string
		for _, im := range f.Images {
			main, thumb := im.Main, im.Thumb
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
			pr.Images = append(pr.Images, ImageRef{PageURL: main, ThumbURL: thumb, Host: h})
		}
		if len(pr.Images) == 0 {
			continue // 与 XML 通道一致：丢弃无图楼层
		}
		var parts []string
		for _, h := range order {
			parts = append(parts, fmt.Sprintf("%s (%d)", h, hostCount[h]))
		}
		pr.Hosts = strings.Join(parts, ", ")
		t.Posts = append(t.Posts, pr)
	}
	for i := range t.Posts {
		if t.Posts[i].FloorNo == 0 {
			t.Posts[i].FloorNo = i + 1
		}
	}
	return t
}

// ParseFloorPage 导出单页楼层解析，供楼层选择器的分批加载通道使用。
func (c *Client) ParseFloorPage(page string) []Floor { return parseFloorPage(page) }

// FetchThreadHTML 抓主题 HTML 页解析楼层（vr.php 无权限时的回退）。
func (c *Client) FetchThreadHTML(threadID int64) *ThreadRef {
	var floors []Floor
	title := ""
	first := true
	c.fetchHTMLPages("threads/"+itoa(threadID), func(page, _ string) {
		if first {
			title = threadTitleFromPage(page)
			first = false
		}
		floors = append(floors, parseFloorPage(page)...)
	}, maxHTMLPages)
	if title == "" {
		title = fmt.Sprintf("Thread #%d", threadID)
	}
	sort.SliceStable(floors, func(i, j int) bool { return floors[i].ID < floors[j].ID })
	t := c.assembleThread(threadID, title, floors)
	if len(t.Posts) == 0 {
		t.Error = "HTML 回退未解析到楼层"
	}
	return t
}

// FetchPostHTML 抓单帖所在页解析（showthread.php?p= 会跳到包含该帖的分页）。
func (c *Client) FetchPostHTML(postID int64) *ThreadRef {
	var floors []Floor
	threadID := int64(0)
	c.fetchHTMLPages(fmt.Sprintf("showthread.php?p=%d&viewfull=1", postID), func(page, finalURL string) {
		if threadID == 0 {
			if m := reThreadsID.FindStringSubmatch(finalURL); m != nil {
				threadID = parseInt64(m[1])
			}
		}
		floors = append(floors, parseFloorPage(page)...)
	}, maxHTMLPages)
	t := c.assembleThread(threadID, "", floors)
	for i := range t.Posts {
		if t.Posts[i].ForumPostID == postID {
			t.Posts = t.Posts[i : i+1]
			return t
		}
	}
	t.Error = "HTML 回退未找到该楼层"
	return t
}

// FetchThreadSmart 楼层获取的统一入口：vr.php XML 优先，权限不足/为空时
// 回退直接抓论坛 HTML。
func (c *Client) FetchThreadSmart(threadID int64) (*ThreadRef, error) {
	ref, err := c.FetchThread(threadID)
	if err == nil && ref.Error == "" && len(ref.Posts) > 0 {
		return ref, nil
	}
	h := c.FetchThreadHTML(threadID)
	if h.Error == "" && len(h.Posts) > 0 {
		return h, nil
	}
	return ref, err
}

// FetchPostSmart 同上，单帖版。
func (c *Client) FetchPostSmart(postID int64) (*ThreadRef, error) {
	ref, err := c.FetchPost(postID)
	if err == nil && ref.Error == "" && len(ref.Posts) > 0 {
		return ref, nil
	}
	h := c.FetchPostHTML(postID)
	if h.Error == "" && len(h.Posts) > 0 {
		return h, nil
	}
	return ref, err
}

func (c *Client) forumURL(rel string) string {
	return "https://" + c.forumHost() + "/" + rel
}

// PostURL 楼层页链接（viewfull=1 直达该楼），供楼层选择器右键打开/复制。
func (c *Client) PostURL(threadID, postID int64) string {
	return fmt.Sprintf("https://%s/threads/%d?p=%d&viewfull=1#post%d", c.forumHost(), threadID, postID, postID)
}

// ThreadPageURL 返回主题第一页地址（无 slug，由论坛 301 到完整地址）。
func (c *Client) ThreadPageURL(threadID int64) string {
	return c.forumURL("threads/" + itoa(threadID))
}

// FetchPage 抓一页论坛 HTML（限速），返回页面原文与 rel="next" 下一页地址。
// 供楼层选择器的分批加载通道逐页调用；失败返回错误，调用方决定是否继续。
func (c *Client) FetchPage(ctx context.Context, pageURL string) (string, string, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return "", "", fmt.Errorf("rate limiter: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("thread page HTTP %d", resp.StatusCode)
	}
	page := string(body)
	finalURL := pageURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return page, nextPageURL(finalURL, page), nil
}

// AssembleFloors 把已解析的楼层组装成 ThreadRef（与 FetchThreadHTML 同样的
// host 解析、hosts 统计、无图楼层丢弃逻辑），供分批加载通道确认楼层时用。
func (c *Client) AssembleFloors(threadID int64, title string, floors []Floor) *ThreadRef {
	return c.assembleThread(threadID, title, floors)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// nextPageURL 从 rel="next" 链接解析下一页地址。vBulletin 的 next 链接是
// 不带前导斜杠的相对路径（如 "threads/xxx/page2"），必须按当前页所在目录
// 解析，否则每翻一页路径就累积一层（/threads/threads/threads/...）。
func nextPageURL(current, page string) string {
	m := reNextPage.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	href := m[1]
	if href == "" {
		href = m[2]
	}
	href = html.UnescapeString(href)
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	base, err := url.Parse(current)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(href, "/") {
		i := strings.LastIndex(base.Path, "/")
		if i < 0 {
			return ""
		}
		base.Path = base.Path[:i+1]
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}
