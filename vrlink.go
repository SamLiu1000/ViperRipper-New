package main

// vrlink.go — 论坛「下载按钮」（vr: 协议）接入。
//
// vipergirls.to 每个主题标题右侧有一个官方下载按钮，HTML 形如：
//
//	<a href="vr:t=16433537" title="Download Images (ripper required!)">
//	  <img src="images/viper-red/buttons/dli.png"></a>
//
// 这个 scheme 由原版 ViperRipper 注册。本程序要做两件事：
//
//	1. 首次启动把 vr: 注册到当前 exe（HKCU，用户级，不需要管理员，不弹 UAC）；
//	2. 处理「程序已经开着又点按钮」——Windows 的协议唤起一定会起**新进程**，
//	   而本程序的数据层（bbolt thumbs.db）是独占锁，第二个进程会干等 5 秒
//	   然后报「数据库被另一个实例占用」退出。所以新进程必须在 NewApp 之前
//	   就被识别出来：把参数通过命名管道丢给已运行实例，自己立刻退出。
//	   这条路径不碰数据库、不建窗口、不加载 WebView2，成本约等于一个空进程。
//
// 注意单实例检测必须放在 main() 里、NewApp() 之前；放进 wails.Run 之后就
// 太晚了（那时 Store 已经打开）。

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// vrMutexName 单实例互斥体。用 Local\ 前缀：只在本登录会话内生效，
	// 不会因为另一个 Windows 用户开了同一程序而被误判成“已经在跑”。
	vrMutexName = `Local\ViperRipperNew.VRLink`

	// vrPipePath 参数转发管道。名字固定，避免第二实例还要去读端口文件。
	vrPipePath = `\\.\pipe\viperripper-new-vrlink`

	// vrOffFileName 关掉本功能的标记文件（放在 data 目录下）：
	// 存在 = 不注册协议，并把已有的注册表项清掉。
	// 用文件而不是设置项，是因为前端 collectSettings() 是白名单式保存，
	// 老前端不带这个字段会把 bool 覆盖成 false——那会让功能莫名其妙失效。
	vrOffFileName = "vr-protocol.off"

	// vrCmdShow 不带链接的唤起（用户又双击了一次 exe）：把已有窗口叫到前面。
	vrCmdShow = "show"

	// vrQueueCap 串行队列长度。连点几十下也不会丢，超了宁可丢弃并记日志。
	vrQueueCap = 64
)

// pendingVRArgs 是本次启动自带的 vr: 参数（main 填入，就绪后再处理）。
// 典型场景：程序没开时点论坛按钮，Windows 把 URL 作为 argv 传进来。
var pendingVRArgs []string

// vrMutexHandle 持有整个进程生命周期的互斥体句柄。不能提前关闭，
// 关掉就等于放锁，后面的实例会误以为自己是第一个。
var vrMutexHandle windows.Handle

// ---------- 单实例 ----------

// acquireVRSingleInstance 尝试成为“唯一实例”。
// second=true 表示已经有实例在跑（包括正在启动、还没就绪的那个）。
// 返回的 release 只应在进程退出时调用（一般不用调，系统会回收）。
func acquireVRSingleInstance() (second bool, err error) {
	name, perr := windows.UTF16PtrFromString(vrMutexName)
	if perr != nil {
		return false, perr
	}
	h, cerr := windows.CreateMutex(nil, false, name)
	if cerr == nil {
		vrMutexHandle = h
		return false, nil
	}
	// CreateMutex 在“名字已存在”时仍返回有效句柄，同时把错误设成
	// ERROR_ALREADY_EXISTS，所以这里要认这个错误码而不是当失败处理。
	if errors.Is(cerr, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return true, nil
	}
	return false, cerr
}

// forwardToRunningInstance 把参数写给已运行实例后立即返回。
// 不等回执：抓 vr.php 可能要好几秒，等的话就不是“启动即退出”了。
func forwardToRunningInstance(args []string) error {
	if len(args) == 0 {
		args = []string{vrCmdShow}
	}
	p, err := windows.UTF16PtrFromString(vrPipePath)
	if err != nil {
		return err
	}
	var h windows.Handle
	// 已运行实例可能正在启动、管道还没建好，所以重试一小会儿。
	deadline := time.Now().Add(2 * time.Second)
	for {
		h, err = windows.CreateFile(p,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(120 * time.Millisecond)
	}
	f := os.NewFile(uintptr(h), vrPipePath)
	defer f.Close()
	for _, a := range args {
		if _, err := fmt.Fprintf(f, "%s\n", a); err != nil {
			return err
		}
	}
	return nil
}

// vrArgsFromOS 从命令行里挑出 vr: 参数。Windows 通过协议唤起时会把
// URL 作为一个参数传进来（通常带引号，Go 已经帮我们拆好了）。
func vrArgsFromOS(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" || strings.HasPrefix(a, "--") || strings.HasPrefix(a, "/") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(a), "vr:") {
			out = append(out, a)
		}
	}
	return out
}

// ---------- 链接归一化 ----------

// normalizeVRLink 把 vr: 短链补成 ParseLink 能认的完整 URL。
//
// 这一步不能省：forum.ParseLink 只认 threads/<id> 和 showthread.php?t=<id>，
// 直接喂 vr:t=16433537 会报“无法识别链接”；而把数字单独提出来更糟——
// 裸数字会命中 reBareNum 被当成**楼层 ID**（postID），语义完全不同。
func normalizeVRLink(s, domain string) string {
	t := strings.TrimSpace(s)
	if len(t) < 3 || !strings.EqualFold(t[:3], "vr:") {
		return t
	}
	rest := strings.TrimSpace(t[3:])
	rest = strings.TrimPrefix(rest, "//")
	if rest == "" {
		return t
	}
	// 已经是完整地址（vr:https://... 或 vr://host/...）：摘掉头就能用
	if strings.Contains(rest, "://") {
		return rest
	}
	if strings.Contains(rest, "/threads/") || strings.Contains(rest, "showthread.php") {
		return "https://" + strings.TrimLeft(rest, "/")
	}
	if domain == "" {
		domain = "vipergirls.to"
	}
	// 裸 id（vr:16433537）按主题处理：比当成楼层更接近用户的意图。
	if !strings.ContainsAny(rest, "=/?&") {
		rest = "t=" + rest
	}
	return "https://" + domain + "/showthread.php?" + rest
}

// ---------- 注册表 ----------

// vrCommand 返回协议处理命令行。exe 名带空格（"ViperRipper New.exe"），
// %1 也带引号——两处引号都不能省，否则路径被空格截断。
func vrCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	return `"` + exe + `" "%1"`, nil
}

// registerVRProtocol 确保 HKCU 下的 vr: 指向当前 exe。
// 返回 changed 表示这次真的写了注册表（用于决定要不要打日志）。
func registerVRProtocol() (changed bool, err error) {
	cmd, err := vrCommand()
	if err != nil {
		return false, err
	}
	const root = `Software\Classes\vr`
	// 已登记且路径一致就不动它：每次启动都写一遍既没必要，
	// 也会让某些安全软件频繁报“程序修改了文件关联”。
	if k, oerr := registry.OpenKey(registry.CURRENT_USER, root+`\shell\open\command`, registry.QUERY_VALUE); oerr == nil {
		cur, _, _ := k.GetStringValue("")
		k.Close()
		if strings.EqualFold(strings.TrimSpace(cur), cmd) {
			return false, nil
		}
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, root, registry.WRITE)
	if err != nil {
		return false, err
	}
	_ = key.SetStringValue("", "URL:ViperRipper 论坛下载按钮")
	_ = key.SetStringValue("URL Protocol", "")
	key.Close()

	ck, _, err := registry.CreateKey(registry.CURRENT_USER, root+`\shell\open\command`, registry.WRITE)
	if err != nil {
		return false, err
	}
	err = ck.SetStringValue("", cmd)
	ck.Close()
	if err != nil {
		return false, err
	}
	return true, nil
}

// unregisterVRProtocol 递归删掉 vr: 那一棵子树（DeleteKey 不删有子键的键）。
func unregisterVRProtocol() error {
	const root = `Software\Classes\vr`
	paths := []string{root + `\shell\open\command`, root + `\shell\open`, root + `\shell`, root}
	for _, p := range paths {
		if err := registry.DeleteKey(registry.CURRENT_USER, p); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ---------- App 侧 ----------

// startVRLink 由 startup() 调用：先把管道接起来（启动瞬间的点击才不会丢），
// 注册协议与首次参数等就绪后再处理。
func (a *App) startVRLink() {
	a.vrQ = make(chan string, vrQueueCap)
	go a.vrWorker()
	go a.vrAcceptLoop()
	go func() {
		<-a.ready
		if a.initErr != nil {
			return
		}
		a.vrSetupProtocol()
		for _, s := range pendingVRArgs {
			a.vrQ <- s
		}
		pendingVRArgs = nil
	}()
}

// vrSetupProtocol 按标记文件决定注册还是清理。
func (a *App) vrSetupProtocol() {
	off := filepath.Join(a.dataDir, vrOffFileName)
	if _, err := os.Stat(off); err == nil {
		if err := unregisterVRProtocol(); err != nil {
			a.vrLog("WARN", "vr: 已按标记文件关闭论坛按钮，但清理注册表失败："+err.Error())
			return
		}
		a.vrLog("INFO", "vr: 论坛下载按钮已关闭（存在 data/"+vrOffFileName+"，删除该文件即可恢复）")
		return
	}
	changed, err := registerVRProtocol()
	if err != nil {
		a.vrLog("WARN", "vr: 注册论坛下载按钮失败："+err.Error())
		return
	}
	if changed {
		cmd, _ := vrCommand()
		a.vrLog("INFO", "vr: 已注册论坛下载按钮 → "+cmd)
	}
}

// vrAcceptLoop 一次只服务一个连接；读完一行就把句柄关掉继续接下一个，
// 真正的添加到队列里串行处理——否则抓 vr.php 的几秒会把后续点击堵死。
func (a *App) vrAcceptLoop() {
	name, err := windows.UTF16PtrFromString(vrPipePath)
	if err != nil {
		return
	}
	first := true
	for {
		flags := uint32(windows.PIPE_ACCESS_DUPLEX)
		if first {
			// 只允许一个服务端实例：万一互斥体因故失效，也不会出现两个服务端
			flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		h, err := windows.CreateNamedPipe(name, flags,
			windows.PIPE_TYPE_MESSAGE|windows.PIPE_READMODE_MESSAGE|windows.PIPE_WAIT,
			1, 4096, 4096, 0, nil)
		if err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				a.vrLog("WARN", "vr: 转发管道名已被占用，论坛按钮转发不可用")
				return
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		first = false
		if cerr := windows.ConnectNamedPipe(h, nil); cerr != nil && !errors.Is(cerr, windows.ERROR_PIPE_CONNECTED) {
			_ = windows.CloseHandle(h)
			continue
		}
		f := os.NewFile(uintptr(h), vrPipePath)
		line, _ := bufio.NewReader(f).ReadString('\n')
		f.Close() // 关闭即断开；客户端不等回执，所以不会卡住它
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		select {
		case a.vrQ <- line:
		default:
			a.vrLog("WARN", "vr: 请求排队已满，丢弃 "+line)
		}
	}
}

// vrWorker 串行消费请求。等就绪再开始，避免在 Store/Settings 还没建好时动手。
func (a *App) vrWorker() {
	<-a.ready
	if a.initErr != nil {
		return
	}
	for line := range a.vrQ {
		a.handleVRRequest(line)
	}
}

func (a *App) handleVRRequest(line string) {
	if line == vrCmdShow {
		a.vrRestoreWindow(true)
		return
	}
	domain := "vipergirls.to"
	if a.Settings != nil {
		if d := a.Settings.Get().ForumDomain; strings.TrimSpace(d) != "" {
			domain = d
		}
	}
	link := normalizeVRLink(line, domain)
	res, err := a.AddLinks(link)
	if err != nil {
		a.vrLog("ERROR", "vr: "+line+" 添加失败："+err.Error())
		a.vrRestoreWindow(true)
		return
	}
	lvl := "INFO"
	if res.Failed > 0 {
		lvl = "WARN"
	}
	a.vrLog(lvl, fmt.Sprintf("vr: %s → 新增 %d 帖 / %d 主题 / 重复 %d / 失败 %d",
		line, res.Posts, res.Threads, res.Dups, res.Failed))
	// 结果发给前端：这次 AddLinks 不是它调的，它不知道要刷新。
	// 前端拿到后走手动粘贴的同款收尾——摘要条提示 + 导入历史 +
	// refreshAll() 按当前排序重排（否则新条目要重启才排到最上面）；
	// beep 是“添加后播放提示音”设置，声音由前端放（playDoneSound，
	// 与下载完成音同款，后端 MessageBeep 只作 webview 拒音时的兜底）。
	var jump, beep bool
	if a.Settings != nil {
		s := a.Settings.Get()
		jump, beep = s.VRJumpOnAdd, s.VRBeepOnAdd
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "links:imported", res, beep)
	}
	// 窗口行为完全交给“每次添加时跳转”开关：
	// 勾了就置前；没勾就一下都不碰——之前没勾也会跳，是因为失败兜底和
	// “最小化时恢复”这两条会绕过开关。失败仍然置前，否则出错没人会知道。
	switch {
	case jump:
		a.vrRestoreWindow(true)
		a.vrLog("INFO", "vr: 按设置“每次添加时跳转”已前置窗口")
	case res.Failed > 0:
		a.vrRestoreWindow(true)
		a.vrLog("INFO", "vr: 添加失败，已前置窗口（原因见上方日志）")
	default:
		a.vrLog("INFO", "vr: 未勾选“每次添加时跳转”，不触碰窗口")
	}
}

// vrRestoreWindow 只在窗口最小化时恢复，不抢前台——用户可能正在浏览器里
// 连续点十个主题。force=true 才主动置前（失败、或需要楼层确认时）。
func (a *App) vrRestoreWindow(force bool) {
	if a.ctx == nil {
		return
	}
	if wruntime.WindowIsMinimised(a.ctx) {
		wruntime.WindowUnminimise(a.ctx)
		wruntime.WindowShow(a.ctx)
		return
	}
	if force {
		wruntime.WindowShow(a.ctx)
	}
}

// vrLog 早期（Store 还没建好）也要能落日志，不能让 nil 把流程打断。
func (a *App) vrLog(level, msg string) {
	if a.Store != nil {
		a.Store.AddLog(level, msg)
		return
	}
	if a.Hub != nil {
		a.Hub.Log(level, msg)
	}
}
