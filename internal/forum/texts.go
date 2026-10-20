package forum

import (
	"context"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// 楼层正文解析。vr.php XML 只有 title（主题标题的变体，如 "xxx RE"），且对
// 部分主题直接返回权限错误；直接抓论坛 HTML 页才能拿到楼层文字（一般是
// 图册名）和图片链接。本文件负责单页解析，threadhtml.go 负责分页抓取组装。

var (
	// 楼层起点：vB4 的 <li id="post_123">，部分皮肤/旧版是 post_message_123
	reFloorStart = regexp.MustCompile(`id="post_(\d+)"|id="post_message_(\d+)"`)
	// vB4 正文块；vB5 是 <article class="b-post__content">
	rePostBody = regexp.MustCompile(`(?s)<blockquote class="postcontent[^"]*"[^>]*>(.*?)</blockquote>`)
	reVB5Body  = regexp.MustCompile(`(?s)class="b-post__content"[^>]*>(.*?)</article>`)
	reScript   = regexp.MustCompile(`(?is)<script.*?</script>`)
	reImgTag   = regexp.MustCompile(`(?is)<img[^>]*>`)
	reComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	// vB4 引用块整体是三层 div：container > quote > quote_container
	reQuoteBlock  = regexp.MustCompile(`(?is)<div class="bbcode_container">.*?</div>\s*</div>\s*</div>`)
	reBr          = regexp.MustCompile(`(?i)<br\s*/?>`)
	reTagAll      = regexp.MustCompile(`<[^>]*>`)
	rePostedBy    = regexp.MustCompile(`(?i)originally posted by`)
	reTextPageNav = regexp.MustCompile(`(?i)^page \d+ of \d+$`)
)

// parseFloorPage 按楼层起点把页面切成段，逐段提取正文文字与图片链接。
// vB4 每楼有两个锚点：id="post_<pid>"（楼层容器）和 id="post_message_<pid>"
// （正文 div）。容器锚点到正文锚点之间的头部段只有按钮图标（声誉加分等）
// 没有正文，必须按楼层聚合：post_message 段优先，只有容器段时才兜底。
func parseFloorPage(page string) []Floor {
	locs := reFloorStart.FindAllStringSubmatchIndex(page, -1)
	type seg struct {
		f      Floor
		msg    bool
		hasTxt bool
	}
	var segs []seg
	for i, loc := range locs {
		var pid int64
		isMsg := false
		for g := 0; g < 2; g++ {
			s, e := loc[2+2*g], loc[3+2*g]
			if s >= 0 {
				pid = parseInt64(page[s:e])
				isMsg = g == 1
				break
			}
		}
		if pid == 0 {
			continue
		}
		start := loc[1]
		end := len(page)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		pseg := page[start:end]
		num := 0
		if m := rePostCount.FindStringSubmatch(pseg); m != nil && parseInt64(m[1]) == pid {
			num = int(parseInt64(m[2]))
		}
		txt := segText(pseg)
		segs = append(segs, seg{f: Floor{ID: pid, FloorNo: num, Text: txt, Images: floorImages(pseg)}, msg: isMsg, hasTxt: txt != ""})
	}
	byID := map[int64]int{}
	out := make([]Floor, 0, len(segs))
	msgMark := make([]bool, 0, len(segs))
	for _, sg := range segs {
		if idx, ok := byID[sg.f.ID]; ok {
			better := sg.msg && !msgMark[idx]
			if !better && sg.msg == msgMark[idx] {
				better = (sg.hasTxt && out[idx].Text == "") || len(sg.f.Images) > len(out[idx].Images)
			}
			if better {
				if sg.f.FloorNo == 0 {
					sg.f.FloorNo = out[idx].FloorNo
				}
				out[idx] = sg.f
				msgMark[idx] = sg.msg
			}
			continue
		}
		byID[sg.f.ID] = len(out)
		out = append(out, sg.f)
		msgMark = append(msgMark, sg.msg)
	}
	return out
}

// segText 提取一个楼层里正文块的第一段可读文字（一般就是图册名）。
func segText(seg string) string {
	m := rePostBody.FindStringSubmatch(seg)
	if m == nil {
		m = reVB5Body.FindStringSubmatch(seg)
	}
	if m == nil {
		return ""
	}
	t := reScript.ReplaceAllString(m[1], "")
	t = reQuoteBlock.ReplaceAllString(t, "")
	t = reImgTag.ReplaceAllString(t, "")
	t = reComment.ReplaceAllString(t, "")
	t = reBr.ReplaceAllString(t, "\n")
	t = reTagAll.ReplaceAllString(t, " ")
	t = html.UnescapeString(t)
	for _, line := range strings.Split(t, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if len([]rune(line)) < 3 {
			continue
		}
		if rePostedBy.MatchString(line) || reTextPageNav.MatchString(line) {
			continue
		}
		r := []rune(line)
		if len(r) > 80 {
			line = string(r[:80])
		}
		return line
	}
	return ""
}

// fetchHTMLPages 从 startURL（相对论坛根）开始顺着 rel="next" 翻页（最多
// maxPages 页），每页调用 once。任何失败静默停止。限速/超时/大小与 XML
// 通道一致。
func (c *Client) fetchHTMLPages(startURL string, once func(page, finalURL string), maxPages int) {
	ctx, cancel := context.WithTimeout(context.Background(), htmlFetchTimeout)
	defer cancel()
	pageURL := c.forumURL(startURL)
	seen := map[string]bool{pageURL: true}
	for i := 0; i < maxPages && pageURL != ""; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.http.Do(req)
		if err != nil {
			return
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		finalURL := ""
		if resp.Request != nil {
			finalURL = resp.Request.URL.String()
		}
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			return
		}
		page := string(body)
		once(page, finalURL)
		next := nextPageURL(finalURL, page)
		if next == "" || seen[next] {
			return
		}
		seen[next] = true
		pageURL = next
	}
}
