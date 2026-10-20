//go:build windows

// Package loginwin opens a standalone WebView2 window hosting the forum
// website so the user can log in manually. The window shares the WebView2
// user-data profile with the main window; whenever vBulletin's persistent
// login cookie (*_userid) appears, the full cookie set for the forum domain
// is handed to onCookies so the downloader's HTTP client can adopt the
// session. One window at a time.
package loginwin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procRegClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWin   = user32.NewProc("CreateWindowExW")
	procDefWndProc  = user32.NewProc("DefWindowProcW")
	procShowWindow  = user32.NewProc("ShowWindow")
	procGetMsg      = user32.NewProc("GetMessageW")
	procTranslate   = user32.NewProc("TranslateMessage")
	procDispatch    = user32.NewProc("DispatchMessageW")
	procDestroyWin  = user32.NewProc("DestroyWindow")
	procPostQuit    = user32.NewProc("PostQuitMessage")
	procGetSysMetr  = user32.NewProc("GetSystemMetrics")
	procSetTimer    = user32.NewProc("SetTimer")
	procKillTimer   = user32.NewProc("KillTimer")
	procSetFG       = user32.NewProc("SetForegroundWindow")
	procSetWindowTx = user32.NewProc("SetWindowTextW")

	kernel32       = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleH = kernel32.NewProc("GetModuleHandleW")

	ole32        = windows.NewLazySystemDLL("ole32.dll")
	procCoInitEx = ole32.NewProc("CoInitializeEx")
)

const (
	WM_CLOSE   = 0x0010
	WM_DESTROY = 0x0002
	WM_SIZE    = 0x0005
	WM_TIMER   = 0x0113
	SW_SHOW    = 5
)

type wndMsg struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PT      struct{ X, Y int32 }
	Private uintptr
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WinExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   uintptr
	ClassName  uintptr
	IconSm     uintptr
}

type loginWindow struct {
	chr         *edge.Chromium
	hwnd        uintptr
	forumHost   string
	proxyAddr   string // optional --proxy-server for the WebView2 window
	title       string
	onState     func([]*http.Cookie, string) // cookies + 窗口真实 UA
	onLog       func(level, msg string)
	lastRaw     string         // last cookie signature already reported
	lastCookies []*http.Cookie // most recent cookie batch seen in the window
	lastDiag    string         // last diagnostic message (only log changes)
	hostOnly    string         // forum host without port, for cookie filtering
	logoutFile  string         // when this file appears, clear the browser cookies
	ua          string         // WebView2 的 navigator.userAgent
	uaBusy      bool
}

// diag logs a message only when it differs from the previous one, so a
// persistent failure does not spam the log every 2 seconds.
func (w *loginWindow) diag(msg string) {
	if msg == w.lastDiag {
		return
	}
	w.lastDiag = msg
	w.log("INFO", "网页登录诊断："+msg)
}

func (w *loginWindow) log(level, msg string) {
	if w.onLog != nil {
		defer func() { recover() }()
		w.onLog(level, msg)
	}
}

const cookieMsgPrefix = "GVC:"

var (
	mu       sync.Mutex
	current  *loginWindow
	procAddr = windows.NewCallback(wndProc)

	// logoutSignalPath: when this file appears the child clears the browser
	// cookies (set by RunStandalone, polled in comHarvest).
	logoutSignalPath string
)

// Open launches the web-login / human-check window on its own OS thread.
// onState is invoked on the window thread whenever the cookie set or the
// browser User-Agent changes; onLog receives diagnostics. A panic in the
// window thread is recovered and logged instead of taking the whole
// application down.
func Open(dataPath, forumHost, startURL, proxyAddr, title string, onState func([]*http.Cookie, string), onLog func(level, msg string)) error {
	mu.Lock()
	if current != nil {
		mu.Unlock()
		return fmt.Errorf("网页窗口已打开，请先完成或关闭它")
	}
	w := &loginWindow{forumHost: forumHost, proxyAddr: proxyAddr, title: title, onState: onState, onLog: onLog}
	current = w
	mu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer func() {
			if r := recover(); r != nil {
				w.log("ERROR", fmt.Sprintf("网页登录窗口异常：%v", r))
			}
			mu.Lock()
			if current == w {
				current = nil
			}
			mu.Unlock()
		}()
		procCoInitEx.Call(0, 2) // COINIT_APARTMENTTHREADED
		defer procCoInitEx.Call(0)
		w.run(dataPath, startURL)
	}()
	return nil
}

func (w *loginWindow) run(dataPath, startURL string) {
	inst, _, _ := procGetModuleH.Call(0)
	clsName, _ := windows.UTF16PtrFromString("ViperRipperLoginWin")
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   procAddr,
		Instance:  inst,
		ClassName: uintptr(unsafe.Pointer(clsName)),
	}
	procRegClassEx.Call(uintptr(unsafe.Pointer(&wc)))

	winTitle := w.title
	if winTitle == "" {
		winTitle = "网页登录 — 登录完成后直接关闭本窗口"
	}
	title, _ := windows.UTF16PtrFromString(winTitle)
	sx, _, _ := procGetSysMetr.Call(0)
	sy, _, _ := procGetSysMetr.Call(1)
	const style = 0x00CF0000 // WS_OVERLAPPEDWINDOW
	ww, wh := uintptr(1000), uintptr(760)
	if sx < uintptr(ww) || sy < uintptr(wh) {
		ww, wh = uintptr(sx)*4/5, uintptr(sy)*4/5
	}
	hwnd, _, _ := procCreateWin.Call(
		0, uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(title)),
		style,
		(uintptr(sx)-ww)/2, (uintptr(sy)-wh)/2, ww, wh,
		0, 0, inst, 0)
	if hwnd == 0 {
		return
	}
	w.hwnd = hwnd

	chr := edge.NewChromium()
	chr.DataPath = dataPath
	if w.proxyAddr != "" {
		// the downloader's HTTP client uses this proxy; the login window must
		// reach the forum the same way or the page stays blank behind a firewall
		chr.AdditionalBrowserArgs = append(chr.AdditionalBrowserArgs,
			"--proxy-server="+w.proxyAddr)
	}
	chr.SetErrorCallback(func(err error) {
		w.log("ERROR", "WebView2 环境错误："+err.Error())
	})
	chr.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
	}
	if !chr.Embed(hwnd) {
		procDestroyWin.Call(hwnd)
		return
	}
	w.chr = chr
	// Hide the forum's top ad banners (they crowd out the login form).
	// Init injects on every navigation, so the rules survive redirects.
	chr.Init(`(function(){
try{
var css=document.createElement('style');
css.textContent=[
'iframe[src*="ads."]','iframe[src*="/ads/"]','iframe[src*="adserver"]','iframe[src*="/ad/"]',
'iframe[src*="jlist"]','iframe[src*="syndication"]','iframe[src*="popads"]',
'iframe[width="468"]','iframe[width="728"]','iframe[width="300"]',
'iframe[height="60"]','iframe[height="90"]','iframe[height="100"]','iframe[height="125"]',
'div[id^="ad_"]','div[id$="_ad"]','div[id*="advert"]','div[id*="banner"]',
'div[class*="advertisement"]','div[class*="sponsor"]','div[class*="banner"]',
'a[href*="/click"]>img','a[href*="sponsor"]>img','img[src*="banner"]','img[src*="/ads/"]',
'table[id*="ad_"]'
].join(',')+'{display:none!important}';
(document.head||document.documentElement).appendChild(css);
}catch(e){}
})();`)
	// vBulletin marks its login cookies HttpOnly, so document.cookie cannot
	// see them; cookies are pulled via the COM cookie manager on a window
	// timer instead (WM_TIMER -> comHarvest, off the COM event-call stack).
	procSetTimer.Call(hwnd, 1, 2000, 0)
	chr.Resize()
	chr.Show() // WebView2 controller starts hidden; without this the window is blank
	procShowWindow.Call(hwnd, SW_SHOW)
	procSetFG.Call(hwnd)
	chr.Navigate(startURL)
	w.fetchUA() // 尽早读 UA：首次 cookie 批次到达时就能一起交出去

	var msg wndMsg
	for {
		r, _, _ := procGetMsg.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		procTranslate.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatch.Call(uintptr(unsafe.Pointer(&msg)))
		if msg.Message == 0x0012 /* WM_QUIT */ {
			break
		}
	}
	chr.ShuttingDown()
}

func wndProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	mu.Lock()
	w := current
	mu.Unlock()
	if w != nil && w.hwnd == hwnd {
		switch msg {
		case WM_SIZE:
			if w.chr != nil {
				w.chr.Resize()
			}
			return 0
		case WM_CLOSE:
			procDestroyWin.Call(hwnd)
			return 0
		case WM_TIMER:
			if wp == 1 {
				w.comHarvest()
			}
			return 0
		case WM_DESTROY:
			procKillTimer.Call(hwnd, 1)
			procPostQuit.Call(0)
			return 0
		}
	}
	r, _, _ := procDefWndProc.Call(hwnd, uintptr(msg), wp, lp)
	return r
}

// comHarvest asks the WebView2 for every cookie it holds for the forum
// domain — including HttpOnly ones, which document.cookie cannot see — via
// the DevTools protocol (Network.getAllCookies). The COM CookieManager route
// crashed inside WebView2Loader, so CDP is the reliable path. Called from
// WM_TIMER every 2 seconds; each changed batch is reported to the host.
func (w *loginWindow) comHarvest() {
	defer func() {
		if r := recover(); r != nil {
			w.log("ERROR", fmt.Sprintf("网页登录 cookie 读取异常：%v", r))
		}
	}()
	if w.chr == nil || w.onState == nil {
		return
	}
	if logoutSignalPath != "" {
		if _, err := os.Stat(logoutSignalPath); err == nil {
			os.Remove(logoutSignalPath)
			if err := w.chr.CallDevToolsProtocol("Network.clearBrowserCookies", "{}", func(errCode int, result string) {
				w.log("INFO", "已清除网页登录 cookie")
			}); err != nil {
				w.diag("清除网页 cookie 失败：" + err.Error())
			}
			w.lastRaw = ""
		}
	}
	hostOnly := w.forumHost
	if i := strings.IndexByte(hostOnly, ':'); i >= 0 {
		hostOnly = hostOnly[:i]
	}
	w.hostOnly = hostOnly
	if w.lastRaw == "" {
		w.diag("cookie 轮询已启动")
	}
	methods := []string{"Network.getAllCookies", "Storage.getCookies", "Network.getCookies"}
	var runQuery func(idx int)
	runQuery = func(idx int) {
		if idx >= len(methods) {
			w.diag("所有 CDP cookie 查询方法均失败")
			return
		}
		err := w.chr.CallDevToolsProtocol(methods[idx], "{}", func(errCode int, result string) {
			if errCode != 0 && idx < len(methods)-1 {
				w.diag("CDP " + methods[idx] + " 不可用(code=" + fmt.Sprint(errCode) + ")，尝试下一个")
				runQuery(idx + 1)
				return
			}
			w.handleCDPResult(errCode, result)
		})
		if err != nil {
			w.diag("CDP " + methods[idx] + " 发起失败：" + err.Error())
			runQuery(idx + 1)
		}
	}
	runQuery(0)
}

func (w *loginWindow) handleCDPResult(errCode int, result string) {
	defer func() {
		if r := recover(); r != nil {
			w.log("ERROR", fmt.Sprintf("网页登录 CDP 回调异常：%v", r))
		}
	}()
	if errCode != 0 {
		w.diag(fmt.Sprintf("CDP 调用失败 code=%d", errCode))
		return
	}
	var resp struct {
		Cookies []struct {
			Name     string `json:"name"`
			Value    string `json:"value"`
			Domain   string `json:"domain"`
			Path     string `json:"path"`
			HTTPOnly bool   `json:"httpOnly"`
		} `json:"cookies"`
	}
	if json.Unmarshal([]byte(result), &resp) != nil {
		return
	}
	var cookies []*http.Cookie
	var names []string
	for _, c := range resp.Cookies {
		d := strings.TrimPrefix(c.Domain, ".")
		// 域名双向匹配：cookie 挂在父域（.example.com）时要能交给 www.example.com 用，
		// 反过来 www 下发的 cookie 也一样。cf_clearance 通常就挂在父域上。
		if d != "" && d != w.hostOnly &&
			!strings.HasSuffix(d, "."+w.hostOnly) &&
			!strings.HasSuffix(w.hostOnly, "."+d) {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: path})
		names = append(names, c.Name+"="+c.Value)
	}
	if len(cookies) == 0 {
		w.diag("profile 里还没有论坛 cookie")
		return
	}
	sort.Strings(names)
	sig := strings.Join(names, ";")
	if sig == w.lastRaw {
		return
	}
	w.lastRaw = sig
	w.lastCookies = cookies
	// cf_clearance 这类凭证与 UA 绑定，必须把窗口的真实 UA 一起交出去
	w.fetchUA()
	w.onState(cookies, w.ua)
}

// fetchUA 读一次 WebView2 的真实 User-Agent。拿到后通知上层，让纯 HTTP
// 链路换成同一个 UA（Cloudflare 的 cf_clearance 与 UA 绑定，UA 不一致等于没验证）。
func (w *loginWindow) fetchUA() {
	if w.chr == nil || w.ua != "" || w.uaBusy {
		return
	}
	w.uaBusy = true
	err := w.chr.CallDevToolsProtocol("Runtime.evaluate",
		`{"expression":"navigator.userAgent","returnByValue":true}`,
		func(errCode int, result string) {
			w.uaBusy = false
			if errCode != 0 || result == "" {
				return
			}
			var resp struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal([]byte(result), &resp) != nil {
				return
			}
			ua := strings.TrimSpace(resp.Result.Value)
			if ua == "" {
				return
			}
			w.ua = ua
			w.log("INFO", "窗口 UA："+ua)
			if w.onState != nil && len(w.lastCookies) > 0 {
				w.onState(w.lastCookies, ua)
			}
		})
	if err != nil {
		w.uaBusy = false
	}
}

// ResultPayload 是子进程写给主进程的凭证：cookie + 窗口真实 UA。
type ResultPayload struct {
	Cookies   []*http.Cookie `json:"cookies"`
	UserAgent string         `json:"userAgent"`
}

// ParsePayload 兼容两种历史格式：对象 {cookies,userAgent} 或纯 cookie 数组。
func ParsePayload(raw []byte) (ResultPayload, bool) {
	var p ResultPayload
	if len(raw) == 0 {
		return p, false
	}
	if json.Unmarshal(raw, &p) == nil && len(p.Cookies) > 0 {
		return p, true
	}
	var arr []*http.Cookie
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return ResultPayload{Cookies: arr}, true
	}
	return p, false
}

// RunStandalone serves the `--weblogin` / `--webverify` child-process mode: it
// opens the window, writes every harvested cookie batch (plus the browser UA)
// to resultFile as JSON, and blocks until the window closes. Returns the
// process exit code. Running the window in a child process isolates the main
// app from any WebView2 failure (the edge package exits the process on
// environment errors).
func RunStandalone(dataPath, forumHost, startURL, proxyAddr, resultFile, title string) int {
	// diagnostics must survive the GUI process having no stderr: append them
	// to a log file next to the result file so the main app can show them
	logFile := resultFile + ".log"
	logf := func(level, msg string) {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		fmt.Fprintf(f, "%s [%s] %s\n", time.Now().Format("15:04:05"), level, msg)
		f.Close()
	}
	done := make(chan struct{})
	logoutSignalPath = logFile + ".logout"
	err := Open(dataPath, forumHost, startURL, proxyAddr, title, func(cookies []*http.Cookie, ua string) {
		raw, err := json.Marshal(ResultPayload{Cookies: cookies, UserAgent: ua})
		if err != nil {
			return
		}
		tmp := resultFile + ".tmp"
		if os.WriteFile(tmp, raw, 0o644) != nil {
			return
		}
		os.Rename(tmp, resultFile)
	}, logf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[ERROR] "+err.Error())
		return 1
	}
	mu.Lock()
	w := current
	mu.Unlock()
	// block until the window thread finishes (current is cleared on exit)
	for w != nil {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		w = current
		mu.Unlock()
	}
	close(done)
	_ = done
	return 0
}
