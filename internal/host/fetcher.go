package host

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/antchfx/htmlquery"
	"golang.org/x/net/html"

	"viperipper/internal/cfguard"
	"viperipper/internal/proxy"
)

// userAgent 保留给规则录制等外部引用；实际请求统一用 cfguard 的 UA
// （人机验证的 cf_clearance 与 UA 绑定）。

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// fetcher is a small HTTP client with a cookie jar, used by the rule engine.
type fetcher struct {
	client  *http.Client
	ctx     context.Context
	baseURL *url.URL // cookie domain anchor
}

func newFetcher(ctx context.Context) *fetcher {
	jar, _ := cookiejar.New(nil)
	return &fetcher{ctx: ctx, client: &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			Proxy: proxy.Func(),
		},
	}}
}

func (f *fetcher) anchor(rawURL string) {
	if f.baseURL == nil {
		if u, err := url.Parse(rawURL); err == nil {
			f.baseURL = u
		}
	}
}

func (f *fetcher) get(rawURL string) (*html.Node, string, string, error) {
	f.anchor(rawURL)
	// 刚刚被 Cloudflare 拦过、还没过验证：别再撞，直接报“需要人机验证”
	if err := cfguard.ShortCircuit(rawURL); err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequestWithContext(f.ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", cfguard.Default.UserAgent())
	// 用户手动过一次人机验证后拿到的 cookie（cf_clearance 等）在这里复用
	for _, c := range cfguard.Default.CookiesFor(rawURL) {
		req.AddCookie(c)
	}
	req.Header.Set("Referer", "https://vipergirls.to/")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, string(body), ctype, err
	}
	// 拿到的可能是 Cloudflare 的“你是人吗”拦截页：如实报出来，别当解析失败
	if cerr := cfguard.DetectChallenge(rawURL, resp.StatusCode, resp.Header, body); cerr != nil {
		return nil, string(body), ctype, cerr
	}
	return parseBody(body, ctype)
}

func (f *fetcher) post(rawURL string, form map[string]string) (*html.Node, string, string, error) {
	f.anchor(rawURL)
	if err := cfguard.ShortCircuit(rawURL); err != nil {
		return nil, "", "", err
	}
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	req, err := http.NewRequestWithContext(f.ctx, "POST", rawURL, strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", cfguard.Default.UserAgent())
	for _, c := range cfguard.Default.CookiesFor(rawURL) {
		req.AddCookie(c)
	}
	req.Header.Set("Referer", rawURL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, string(body), ctype, err
	}
	if cerr := cfguard.DetectChallenge(rawURL, resp.StatusCode, resp.Header, body); cerr != nil {
		return nil, string(body), ctype, cerr
	}
	return parseBody(body, ctype)
}

// parseBody 把响应正文解析成 DOM。部分图床（imx.to 等）返回畸形 HTML——
// 未闭合标签层层堆叠，超过 x/net/html 解析器 512 层的硬限制，解析直接报错。
// 这种页面 DOM 虽然建不出来，但 {body} 原文还在：search 类规则靠它照样能
// 解析。所以解析失败不作为错误返回，doc 为 nil，由上层按需处理（extract
// 步骤会得到明确的 "no document" 错误，而不是含糊的解析失败）。
func parseBody(body []byte, ctype string) (*html.Node, string, string, error) {
	if strings.HasPrefix(strings.ToLower(ctype), "image/") {
		return nil, string(body), ctype, nil
	}
	doc, perr := htmlquery.Parse(strings.NewReader(string(body)))
	if perr != nil {
		return nil, string(body), ctype, nil
	}
	return doc, string(body), ctype, nil
}

func (f *fetcher) setCookie(anchorURL, name, value string) {
	if anchorURL == "" {
		return
	}
	f.anchor(anchorURL)
	if f.baseURL == nil {
		return
	}
	cur := f.client.Jar.Cookies(f.baseURL)
	var found bool
	for i := range cur {
		if cur[i].Name == name {
			cur[i].Value = value
			found = true
		}
	}
	if !found {
		cur = append(cur, &http.Cookie{Name: name, Value: value})
	}
	f.client.Jar.SetCookies(f.baseURL, cur)
}

func (f *fetcher) Close() { f.client.CloseIdleConnections() }
