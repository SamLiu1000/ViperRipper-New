package main

// 本地桥接（给浏览器扩展用）：
// 扩展在浏览器里抓取一张图片页的 DOM，生成候选规则；这个接口让 App
// 用真实解析器跑一遍规则、探测直链的真实尺寸，并把规则直接落盘热加载。
// 只监听 127.0.0.1，仅本机可访问。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"viperipper/internal/host"
	"viperipper/internal/proxy"
)

const extBridgePortStart = 47821
const extBridgePortTries = 16

type extBridge struct {
	app     *App
	srv     *http.Server
	port    int
	dataDir string
}

// startExtBridge binds the first free port starting at preferPort (0 → the
// default 47821, scanning up to 16 ports) and writes the chosen port to
// data/ext-port.txt so the extension's auto-detect can find it.
func startExtBridge(a *App, dataDir string, preferPort int) *extBridge {
	b := &extBridge{app: a, dataDir: dataDir}
	start := preferPort
	if start <= 0 || start > 65535 {
		start = extBridgePortStart
	}
	var ln net.Listener
	for p := start; p < start+extBridgePortTries && p <= 65535; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln, b.port = l, p
			break
		}
	}
	if ln == nil {
		a.Store.AddLog("WARN", fmt.Sprintf("浏览器扩展桥接端口被占用（%d 起 %d 个都没空），规则录制器将无法保存", start, extBridgePortTries))
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", b.handlePing)
	mux.HandleFunc("/api/host/test", b.handleTest)
	mux.HandleFunc("/api/host/save", b.handleSave)
	mux.HandleFunc("/api/host/list", b.handleHostList)
	b.srv = &http.Server{
		Handler:           cors(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = b.srv.Serve(ln) }()
	_ = os.WriteFile(filepath.Join(dataDir, "ext-port.txt"), []byte(fmt.Sprint(b.port)), 0o644)
	a.Store.AddLog("INFO", fmt.Sprintf("规则录制器桥接已就绪：http://127.0.0.1:%d", b.port))
	return b
}

func (b *extBridge) stop() {
	if b == nil || b.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.srv.Shutdown(ctx)
	// 让扩展的自动探测看不到过期的端口
	if b.dataDir != "" {
		_ = os.Remove(filepath.Join(b.dataDir, "ext-port.txt"))
	}
}

// cors lets the extension (chrome-extension:// origin) call us. Bound to
// loopback only, so no cross-host exposure.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// exeBuildStamp 返回可执行文件的构建时间，用来区分“跑的是不是最新构建”。
func exeBuildStamp() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	st, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return st.ModTime().Format("01-02 15:04")
}

func (b *extBridge) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "app": "ViperRipper", "port": b.port, "build": exeBuildStamp(),
	})
}

type testRequest struct {
	Rule  *host.Rule `json:"rule"`
	Name  string     `json:"name"`
	Page  string     `json:"page"`
	Thumb string     `json:"thumb"`
}

// handleTest resolves the page with the posted rule (or a saved rule by
// name), then probes the resulting direct URL for real dimensions — the
// missing feedback loop of the old recorder UI.
func (b *extBridge) handleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	var req testRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad json: " + err.Error()})
		return
	}
	if req.Page == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "page 不能为空"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	var res *host.Result
	var err error
	if req.Rule != nil {
		res, err = b.app.Hosts.ResolveRule(ctx, req.Rule, req.Page, req.Thumb)
	} else if req.Name != "" {
		res, err = b.app.Hosts.ResolveWithCtx(ctx, req.Name, req.Page, req.Thumb)
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "需要 rule 或 name"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	referer := host.DownloadRefererFor(req.Rule)
	img := probeImage(ctx, res.ImageURL, referer)
	// 直链必须真的返回图片文件（probeImage 从 magic bytes 解析宽高），
	// 拿到的是网页/空响应（宽高全 0）一律算失败——否则“解析出页面地址”
	// 会被误判成功
	if img == nil || (img.Width == 0 && img.Height == 0) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "page": req.Page,
			"error": "直链未能下载到图片文件（返回的不是图片）",
		})
		return
	}
	// 直链等于输入的缩略图本身 = 规则没做任何推导，判失败
	if req.Thumb != "" && res.ImageURL == req.Thumb {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "page": req.Page,
			"error": "规则直接返回了缩略图本身，没有推导出原图直链",
		})
		return
	}
	out := map[string]any{
		"ok":       true,
		"direct":   res.ImageURL,
		"page":     req.Page,
		"filename": res.Filename,
		"trace":    res.Trace,
		"image":    img,
	}
	if img.Width < 400 || img.Height < 400 {
		out["warn"] = fmt.Sprintf("直链只有 %d×%d，可能是缩略图或占位图", img.Width, img.Height)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHostList 列出全部规则（名字/域名/启用状态）。规则录制器用它把
// 同一图床家族的域名（如 pixhost.to + pixhost.cc）合并进新规则的域名列表
// ——这些站点常识内置规则里早就写着，分析器没理由不知道。
func (b *extBridge) handleHostList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET only"})
		return
	}
	type ruleBrief struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Enabled bool     `json:"enabled"`
	}
	out := []ruleBrief{}
	for _, rule := range b.app.Hosts.List() {
		out = append(out, ruleBrief{Name: rule.Name, Domains: rule.Domains, Enabled: rule.Enabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": out})
}

// handleSave writes the rule to data/hosts as a *.custom.json and reloads.
func (b *extBridge) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	var req struct {
		Rule *host.Rule `json:"rule"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad json: " + err.Error()})
		return
	}
	if req.Rule == nil || req.Rule.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少规则或规则名"})
		return
	}
	shadowed, wasBuiltin := b.app.Hosts.NameStatus(req.Rule.Name)
	file, err := b.app.Hosts.SaveCustom(req.Rule)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	resp := map[string]any{"ok": true, "file": file}
	if shadowed && wasBuiltin {
		resp["warn"] = "同名内建规则已存在，这条自定义规则会遮蔽它（如需恢复请删除该 .custom.json）"
	}
	b.app.Store.AddLog("INFO", "已保存 host 规则 "+req.Rule.Name+" → "+filepath.Base(file))
	writeJSON(w, http.StatusOK, resp)
}

// ---------- image probe ----------

type imageInfo struct {
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

var probeClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &http.Transport{Proxy: proxy.Func()},
}

// probeImage fetches just the head of the image and parses its dimensions.
func probeImage(ctx context.Context, rawURL, referer string) *imageInfo {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	info := &imageInfo{
		ContentType: resp.Header.Get("Content-Type"),
		Bytes:       resp.ContentLength,
	}
	info.Width, info.Height = imageDims(head)
	if info.Bytes <= 0 {
		info.Bytes = int64(len(head))
	}
	return info
}

// imageDims sniffs width/height from the first bytes of a JPEG/PNG/GIF/WEBP.
func imageDims(d []byte) (int, int) {
	if len(d) < 16 {
		return 0, 0
	}
	switch {
	case d[0] == 0xFF && d[1] == 0xD8: // JPEG
		i := 2
		for i+9 < len(d) {
			if d[i] != 0xFF {
				i++
				continue
			}
			m := d[i+1]
			if m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
				i += 2
				continue
			}
			segLen := int(binary.BigEndian.Uint16(d[i+2 : i+4]))
			if segLen < 2 {
				return 0, 0
			}
			if (m >= 0xC0 && m <= 0xCF) && m != 0xC4 && m != 0xC8 && m != 0xCC {
				h := int(binary.BigEndian.Uint16(d[i+5 : i+7]))
				w := int(binary.BigEndian.Uint16(d[i+7 : i+9]))
				return w, h
			}
			i += 2 + segLen
		}
	case string(d[0:8]) == "\x89PNG\r\n\x1a\n": // PNG
		return int(binary.BigEndian.Uint32(d[16:20])), int(binary.BigEndian.Uint32(d[20:24]))
	case string(d[0:3]) == "GIF":
		return int(binary.LittleEndian.Uint16(d[6:8])), int(binary.LittleEndian.Uint16(d[8:10]))
	case string(d[0:4]) == "RIFF" && string(d[8:12]) == "WEBP":
		switch string(d[12:16]) {
		case "VP8X":
			w := int(uint32(d[24]) | uint32(d[25])<<8 | uint32(d[26])<<16)
			h := int(uint32(d[27]) | uint32(d[28])<<8 | uint32(d[29])<<16)
			return w + 1, h + 1
		case "VP8 ":
			if len(d) < 30 {
				return 0, 0
			}
			w := int(binary.LittleEndian.Uint16(d[26:28]) & 0x3FFF)
			h := int(binary.LittleEndian.Uint16(d[28:30]) & 0x3FFF)
			return w, h
		case "VP8L":
			if len(d) < 25 {
				return 0, 0
			}
			b0, b1, b2, b3 := d[21], d[22], d[23], d[24]
			w := 1 + int((uint32(b1)&0x3F)<<8|uint32(b0))
			h := 1 + int((uint32(b3)&0x0F)<<10|uint32(b2)<<2|(uint32(b1)&0xC0)>>6)
			return w, h
		}
	}
	return 0, 0
}
