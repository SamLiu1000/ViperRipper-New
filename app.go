package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"

	"viperipper/internal/cfguard"
	"viperipper/internal/forum"
	"viperipper/internal/host"
	"viperipper/internal/hub"
	"viperipper/internal/loginwin"
	"viperipper/internal/pipeline"
	"viperipper/internal/proxy"
	"viperipper/internal/settings"
	"viperipper/internal/store"
)

// App is the bridge between the Vue/JS frontend and the Go core.
type App struct {
	ctx          context.Context
	Store        *store.Store
	Hosts        *host.Registry
	Forum        *forum.Client
	Pipe         *pipeline.Engine
	Hub          *hub.Hub
	Settings     *settings.Manager
	dataDir      string
	webLoginMu   sync.Mutex
	webLoginCmd  *exec.Cmd
	logoutSignal string

	// ready closes once initCore finishes (successfully or not); frontend
	// calls block on it so the window shows instantly while the DB opens.
	ready   chan struct{}
	initErr error
	t0      time.Time

	// background disk-recovery queue with priorities: 0 = downloading now,
	// 1 = post selected in the UI, 2 = bulk add. Capped at 2 workers so a
	// huge AddLinks batch can't spawn thousands of resolve passes.
	recoverQ      [3][]int64
	recoverSeen   map[int64]bool
	recoverMu     sync.Mutex
	recoverNotify chan struct{}

	// extB is the loopback HTTP bridge used by the browser rule-recorder
	// extension (test/save host rules without leaving the page).
	extMu   sync.Mutex
	extB    *extBridge
	thankMu sync.Mutex

	// floorJobs 楼层选择器的分批抓取状态（threadRowID → *floorTextJob）：
	// 楼层骨架与文本名共用一个逐页抓取通道，避免一次全量同步等待。
	floorJobs sync.Map
	// vrQ 论坛下载按钮（vr: 协议）的请求队列，串行消费，见 vrlink.go
	vrQ chan string
	// quitting 由菜单“退出”置位，避免被“最小化到托盘”拦住
	quitting bool
	// lastCloseAsk 上一次弹“还有任务在下载”的时间：确认框没能显示时，
	// 连续再点一次 × 就当作强制退出（兜底，不让窗口永远关不掉）
	closeAskAt    time.Time
	closeAskShown bool
	// 自动关机只安排一次
	shutdownMu    sync.Mutex
	shutdownArmed bool
	// 队列空闲检测：OnPostDone 是在帖子 goroutine 内部回调的，此刻它还在
	// running 表里，直接判定永远“还有任务”。这里延迟复查，并用品号保证
	// 一批任务只发一次“全部完成”（idleFired），StartPosts 时复位。
	idleMu    sync.Mutex
	idleSeq   int
	idleFired bool
}

// ---------- lifecycle ----------

func NewApp(dataDir string) (*App, error) {
	app := &App{
		dataDir:       dataDir,
		Hub:           hub.New(),
		Hosts:         host.NewRegistry(dataDir),
		ready:         make(chan struct{}),
		recoverSeen:   map[int64]bool{},
		recoverNotify: make(chan struct{}, 1),
		t0:            time.Now(),
	}
	go app.initCore()
	return app, nil
}

// initCore does the slow part of startup (SQLite open, migrations, bolt,
// settings, pipeline) after the window is already visible. Everything the
// frontend calls goes through waitReady().
func (a *App) initCore() {
	defer close(a.ready)
	mark := func(what string, since time.Time) {
		a.startupMark(fmt.Sprintf("[startup] %s: %dms", what, time.Since(since).Milliseconds()))
	}

	t := time.Now()
	st, err := store.Open(a.dataDir)
	if err != nil {
		a.initErr = fmt.Errorf("open database: %w", err)
		a.Hub.Log("ERROR", a.initErr.Error())
		return
	}
	a.Store = st
	mark("store.open", t)

	t = time.Now()
	a.Settings = settings.New(st.DB())
	s := a.Settings.Get()
	if s.DownloadDir == "" {
		s.DownloadDir = filepath.Join(a.dataDir, "galleries")
		a.Settings.Set(s)
	}
	mark("settings.load", t)

	// 规则目录可能是用户指定过的位置（例如同步盘），必须在 Seed 之前生效
	// 早期版本里 minimizeTray 只是个空开关（没有托盘实现），存下来的 true
	// 并不代表用户真的想要“关窗不退出”。首次升级到这里时统一关掉，
	// 避免用户点 X 发现程序还在跑。标记只做一次，之后用户手动勾选会保留。
	if a.Store.GetKV("minimizeTrayMigrated") == "" {
		if s.ConfirmOnClose {
			s.ConfirmOnClose = false
			_ = a.Settings.Set(s)
		}
		a.Store.SetKV("minimizeTrayMigrated", "1")
	}
	// 这个开关的含义已改为“下载中关窗 / 退出先弹确认框”（旧值来自没有托盘的
	// 年代，被上面那段迁移强制关过）。一次性重新打开，之后按用户的勾选走。
	if a.Store.GetKV("closeConfirmMigrated") == "" {
		if !s.ConfirmOnClose {
			s.ConfirmOnClose = true
			_ = a.Settings.Set(s)
		}
		a.Store.SetKV("closeConfirmMigrated", "1")
	}

	if s.HostsDir != "" {
		if err := a.Hosts.SetHostsDir(s.HostsDir); err != nil {
			a.Hub.Log("WARN", "规则目录不可用，回退默认目录："+err.Error())
		}
	}

	t = time.Now()
	if err := a.Hosts.Seed(); err != nil {
		a.initErr = fmt.Errorf("seed host rules: %w", err)
		a.Hub.Log("ERROR", a.initErr.Error())
		return
	}
	mark("host.seed", t)

	// 早期版本把接口域名 viper.click 当成默认镜像存下来了；它不是论坛镜像，
	// 留着会让登录探测跑到 API 域名上，这里清掉一次。
	if s.Mirrors == "viper.click" {
		s.Mirrors = ""
		_ = a.Settings.Set(s)
	}
	a.Forum = forum.NewClient("https://viper.click/vr.php", s.ForumDomain, s.RateLimit)
	a.Forum.SetMirrors(s.Mirrors)
	a.Forum.HostResolver = a.Hosts.RuleNameOf
	a.Pipe = pipeline.New(st, a.Hosts)
	a.Pipe.BaseDir = s.DownloadDir
	a.Pipe.OnImageEvent = func(postRowID, imageID int64) {
		a.Hub.MarkImageDirty(postRowID, imageID)
		a.Hub.MarkPostDirty(postRowID)
	}
	a.Pipe.OnBeforeStart = a.ensureImageRows

	// 人机验证（Cloudflare）：被拦住时通知界面，用户过完验证后自动续下。
	// 凭证存 KV，重启后仍可复用，直到过期再被拦一次。
	cfguard.Default.Restore([]byte(a.Store.GetKV("cfsessions")))
	cfguard.Default.OnNeed = a.onChallengeNeed
	cfguard.Default.OnPass = a.onChallengePass
	// 老记录没记板块（早期单帖链接）：下载前按 post_id 去论坛查一次，
	// 结果由 pipeline 写回数据库，之后目录推导就不再缺板块那层。
	a.Pipe.ResolveForum = func(p *store.Post) string {
		if p == nil || p.ForumPostID == 0 {
			return ""
		}
		ref, err := a.forumFetch(p.ForumThreadID, p.ForumPostID)
		if err != nil || ref == nil || ref.Error != "" {
			return ""
		}
		return strings.TrimSpace(ref.Forum)
	}
	a.Pipe.OnPostStatus = func(postRowID int64, status string) {
		a.Hub.MarkPostDirty(postRowID)
		// 下载中改过名的帖子：状态一离开 downloading 就把文件夹归位到新名
		if status != "downloading" && status != "" {
			if p, err := a.Store.GetPost(postRowID); err == nil && p != nil {
				a.Pipe.SyncFolderToTitle(p)
			}
		}
		// 队列结束判定：done / error 都是这条任务的终态（error = 有死链但也结束了）。
		// 全部图片失效的帖子不会走 OnPostDone，只认那里的话就永远不会有提示音；
		// paused 是用户主动停的，不触发“全部完成”。
		if status == "done" || status == "error" {
			go a.handleQueueIdle()
		}
	}
	a.Pipe.OnPostDone = func(postRowID, forumPostID int64) {
		// 完成提示音：前端收到事件后自己发声（不用打包音频文件）
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, "post:done", postRowID)
		}
		// 队列真的空了才提示完成 / 触发自动关机
		go a.handleQueueIdle()
		if !a.Settings.Get().AutoThank {
			return
		}
		// 以前这里“未登录就静默什么都不做”，用户以为开关生效了其实没有；
		// 而且每次重新跑一个已完成的帖子都会再感谢一遍。两件事都修掉。
		if !a.Forum.LoggedIn() {
			a.Store.AddLog("WARN", "自动感谢已开启但当前未登录，已跳过 post #"+fmt.Sprint(forumPostID))
			return
		}
		if a.alreadyThanked(forumPostID) {
			return
		}
		go func() {
			if err := a.Forum.Thank(forumPostID); err != nil {
				a.Store.AddLog("WARN", "自动感谢失败："+err.Error())
				return
			}
			a.markThanked(forumPostID)
			a.Store.AddLog("INFO", "已自动感谢 post #"+fmt.Sprint(forumPostID))
		}()
	}
	a.Pipe.OnImageProgress = func(imgID, done, total int64) {
		a.Hub.MarkProgress(imgID, done, total)
	}
	a.Pipe.OnLog = func(level, msg string) {
		a.Store.AddLog(level, msg)
		a.Hub.Log(level, msg)
	}
	// 启动时也要把设置灌进管道：以前只在“开始下载/保存设置”时才应用，
	// 于是插入帖子后的自动磁盘恢复（autoRecover）用的是默认值，
	// 目录组织、临时目录、已存在文件策略都会和界面显示的不一致。
	a.applyPipeSettings()

	a.pinRenamedDirs()
	a.reconcileStaleDirs()
	a.rebaseImagePaths()

	t = time.Now()
	st.ResetDownloading()
	mark("reset.downloading", t)

	// 清理历史上被误存成“已完成”的防盗链占位图（imagetwist 会返回
	// 一个几 KB 的 PNG 冒充原图），退回 pending 以便重新下载。
	go func() {
		if n, err := a.Pipe.RepairPlaceholders(); err == nil && n > 0 {
			a.Hub.Log("WARN", fmt.Sprintf("启动自检：重置 %d 张防盗链占位图", n))
		}
	}()

	a.startupMark(fmt.Sprintf("[startup] 核心初始化完成，总计 %dms", time.Since(a.t0).Milliseconds()))

	for i := 0; i < 2; i++ {
		go a.recoverWorker()
	}

	if raw := a.Store.GetKV("authcookies"); raw != "" {
		go func() {
			var cookies []*http.Cookie
			if json.Unmarshal([]byte(raw), &cookies) != nil || len(cookies) == 0 {
				return
			}
			if err := a.Forum.ImportCookies(cookies); err != nil {
				a.Store.AddLog("INFO", "恢复论坛登录失败："+err.Error()+"（可重新网页登录）")
				a.Store.SetKV("authcookies", "")
				a.emitLoginChanged()
				return
			}
			a.Store.AddLog("INFO", "已恢复论坛登录（"+a.Forum.CurrentUser()+"）")
			a.emitLoginChanged()
		}()
	} else if s.AutoLogin && s.Username != "" {
		go func() {
			if err := a.Forum.Login(s.Username, s.Password); err != nil {
				a.Store.AddLog("WARN", "自动登录失败："+err.Error())
			} else {
				a.Store.AddLog("INFO", "自动登录成功（"+s.Username+"）")
				a.persistAuthCookies()
			}
			a.emitLoginChanged()
		}()
	}
}

// waitReady blocks until initCore finishes; returns the init error, if any.
func (a *App) waitReady() error {
	<-a.ready
	return a.initErr
}

// pinRenamedDirs 老数据的收尾：以前目录名是按标题自动推导的，楼层标题跟
// 主题标题雷同时（"Re <主题>"、"<主题> — 2F"）会被兜底成 F<n>，用户改过的
// 名字随时可能被搬回去。这里把“文件夹名已经和标题对得上”的条目钉住——
// 它们只可能是改过名留下的，钉住之后现有文件夹一个都不会挪窝。
func (a *App) pinRenamedDirs() {
	posts, err := a.Store.ListPosts()
	if err != nil {
		return
	}
	n := 0
	threadTitles := map[int64]string{} // 每个主题只查一次
	for i := range posts {
		p := &posts[i]
		if p.CustomName != "" || p.Dir == "" {
			continue
		}
		if st, err := os.Stat(p.Dir); err != nil || !st.IsDir() {
			continue
		}
		want := pipeline.DirNameFromTitle(p.Title)
		if want == "" || !pipeline.MatchesTitleName(filepath.Base(p.Dir), p.Title) {
			continue
		}
		// 文件夹名恰好是主题名时，这个目录其实是「主题文件夹」本身（勾了
		// “直接使用主页标题”的条目就是这样），不是用户给条目起的名字。
		// 以前这里照记不误，之后再拼路径就成了 主题\主题 两层。
		if base := filepath.Base(p.Dir); p.ForumThreadID != 0 {
			tt, ok := threadTitles[p.ForumThreadID]
			if !ok {
				tt, _ = a.Store.ThreadTitleAndPostCount(p.ForumThreadID)
				threadTitles[p.ForumThreadID] = tt
			}
			if tt != "" && pipeline.SanitizeDirName(tt) == pipeline.SanitizeDirName(base) {
				continue
			}
		}
		if a.Store.SetPostDirName(p.ID, p.Title) == nil {
			n++
		}
	}
	if n > 0 {
		a.Store.AddLog("INFO", fmt.Sprintf("启动自检：%d 个条目的文件夹名已固定跟随标题", n))
	}
}

// reconcileStaleDirs 老数据的下载目录记录还停在早期默认目录（data\galleries
// 之类）或手动搬走之前的位置，那些路径现在多半已经不存在了，文件却好好地
// 躺在当前下载根目录里。纠正这批记录（只改指针不动文件），改名、续传、打
// 开文件夹才不会对错地方。
func (a *App) reconcileStaleDirs() {
	posts, err := a.Store.ListPosts()
	if err != nil {
		return
	}
	n := 0
	for i := range posts {
		p := &posts[i]
		if a.Pipe.ReconcileStaleDir(p) {
			n++
		}
	}
	if n > 0 {
		a.Store.AddLog("INFO", fmt.Sprintf("启动自检：%d 条记录的下载目录已纠正到实际位置", n))
	}
}

// rebaseImagePaths 启动自检的最后一步：图片级的 file_path 存的是下载完成
// 时的绝对路径，目录搬家（或从旧版 ViperRipper 导入数据）后会全部失效，
// /local/ 端点 404，右侧预览大图和列表预览列就都加载不出来。reconcileStaleDirs
// 只纠正了帖子级目录，这里把每个有效目录下按文件名能找回来的图片路径重写。
func (a *App) rebaseImagePaths() {
	posts, err := a.Store.ListPosts()
	if err != nil {
		return
	}
	n := 0
	for i := range posts {
		p := &posts[i]
		if p.Dir == "" {
			// dir 记录为空的条目（多为旧版数据导入）：从图片旧路径反推
			n += a.rebaseOrphanPost(&posts[i])
			continue
		}
		if st, err := os.Stat(p.Dir); err != nil || !st.IsDir() {
			continue // 帖子目录本身都找不到，无从按名重定位
		}
		n += a.Store.RebaseImagePaths(p.ID, p.Dir)
	}
	if n > 0 {
		a.Store.AddLog("INFO", fmt.Sprintf("启动自检：%d 张已完成图片的路径已重定位到当前下载目录", n))
	}
}

// rebaseOrphanPost 修 dir 为空的条目：取它一张已完成图片的旧路径，拿
// 「帖子文件夹名」（及其上级板块文件夹名）在当前下载根目录下找实际位置；
// 找到就把整帖图片路径按前缀重写过去，并回填 post.dir。
func (a *App) rebaseOrphanPost(p *store.Post) int {
	oldDir, ok := a.Store.FirstDoneImageDir(p.ID)
	if !ok || a.Pipe == nil {
		return 0
	}
	base := strings.TrimSpace(a.Pipe.BaseDir)
	if base == "" {
		return 0
	}
	postFolder := filepath.Base(oldDir)
	parent := filepath.Base(filepath.Dir(oldDir)) // 板块文件夹（SortByForum 布局）
	cands := []string{
		filepath.Join(base, postFolder),
		filepath.Join(base, parent, postFolder),
	}
	for _, cand := range cands {
		if st, err := os.Stat(cand); err != nil || !st.IsDir() {
			continue
		}
		m, err := a.Store.RewriteImagePathPrefix(oldDir, cand)
		if err != nil || m == 0 {
			return 0
		}
		a.Store.SetPostDir(p.ID, cand)
		return m
	}
	return 0
}

// startupMark appends a timestamped line to data/startup.log (startup
// timing diagnostics) and mirrors it to the in-app Log page.
func (a *App) startupMark(line string) {
	ts := time.Now().Format("15:04:05.000")
	f, err := os.OpenFile(filepath.Join(a.dataDir, "startup.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "%s %s\n", ts, line)
		f.Close()
	}
	a.Hub.Log("INFO", line)
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.Hub.Start(ctx)
	// 论坛下载按钮（vr: 协议）：管道先接上（启动瞬间的点击不丢），
	// 注册协议与启动参数等核心就绪后再处理
	a.startVRLink()
	// 恢复上次的主窗口位置与尺寸（设置加载完成后套用；0 = 保持默认）
	go func() {
		<-a.ready
		if a.initErr != nil {
			return
		}
		s := a.Settings.Get()
		if s.WinW >= 980 && s.WinH >= 620 {
			wruntime.WindowSetSize(a.ctx, s.WinW, s.WinH)
			// 位置做基本的合理范围校验（Screen API 无坐标，无法精确匹配
			// 显示器布局；超范围的记录多半来自已拔掉的副屏，回落居中）
			if s.WinX > -10000 && s.WinX < 20000 && s.WinY > -10000 && s.WinY < 20000 {
				wruntime.WindowSetPosition(a.ctx, s.WinX, s.WinY)
			} else {
				wruntime.WindowCenter(a.ctx)
			}
		}
	}()
	// 浏览器扩展桥接：等核心初始化完（Hosts/Store 可用）再按设置起监听
	go func() {
		<-a.ready
		if a.initErr != nil {
			return
		}
		s := a.Settings.Get()
		if !s.ExtBridgeEnabled {
			a.Store.AddLog("INFO", "浏览器扩展桥接已关闭（设置 → 扩展）")
			return
		}
		a.extMu.Lock()
		a.extB = startExtBridge(a, a.dataDir, s.ExtBridgePort)
		a.extMu.Unlock()
	}()
}

func (a *App) shutdown(ctx context.Context) {
	<-a.ready // never tear down a half-initialized core
	a.extMu.Lock()
	a.extB.stop()
	a.extMu.Unlock()
	if a.Pipe != nil {
		a.Pipe.Stop()
		a.Pipe.Wait()
	}
	a.Hub.Stop()
	if a.Store != nil {
		a.Store.Close()
	}
}

// ---------- links & scanning ----------

// ImportItem is the per-link outcome of one AddLinks call.
type ImportItem struct {
	URL    string `json:"url"`
	Status string `json:"status"` // ok | thread | dup | fail
	Note   string `json:"note,omitempty"`
}

// ImportResult summarizes one AddLinks call: how many links became plain
// posts, multi-floor threads, duplicates or failures, plus every item.
type ImportResult struct {
	Posts   int          `json:"posts"`
	Threads int          `json:"threads"`
	Dups    int          `json:"dups"`
	Failed  int          `json:"failed"`
	Message string       `json:"message"`
	Items   []ImportItem `json:"items"`
}

// ImportRecord is one persisted import batch (a single AddLinks call).
type ImportRecord struct {
	At    string       `json:"at"`
	Items []ImportItem `json:"items"`
}

const importLogFile = "imports.json"

// LoadImportLog returns the persisted import history (newest batch first).
func (a *App) LoadImportLog() ([]ImportRecord, error) {
	b, err := os.ReadFile(filepath.Join(a.dataDir, importLogFile))
	if err != nil {
		if os.IsNotExist(err) {
			return []ImportRecord{}, nil
		}
		return []ImportRecord{}, err
	}
	var out []ImportRecord
	if err := json.Unmarshal(b, &out); err != nil {
		// corrupt file: start fresh rather than failing the panel forever
		return []ImportRecord{}, nil
	}
	if out == nil {
		out = []ImportRecord{}
	}
	return out, nil
}

// saveWindowBounds persists the window's position and size, reading the real
// outer geometry via the wails runtime (no border/titlebar guesswork).
func (a *App) saveWindowBounds(ctx context.Context) {
	if a.Settings == nil || ctx == nil {
		return
	}
	x, y := wruntime.WindowGetPosition(ctx)
	w, h := wruntime.WindowGetSize(ctx)
	s := a.Settings.Get()
	if s.WinX == x && s.WinY == y && s.WinW == w && s.WinH == h {
		return
	}
	s.WinX, s.WinY, s.WinW, s.WinH = x, y, w, h
	_ = a.Settings.Set(s)
}

// SaveWindowSize persists the current main-window bounds. The frontend calls
// this (debounced) on window resize.
func (a *App) SaveWindowSize() error {
	if a.ctx == nil {
		return nil
	}
	a.saveWindowBounds(a.ctx)
	return nil
}

// SaveImportLog replaces the persisted import history. Kept in the data dir
// as plain JSON — small, human-inspectable, rewritten wholesale on each add.
func (a *App) SaveImportLog(recs []ImportRecord) error {
	const maxBatches = 200
	if len(recs) > maxBatches {
		recs = recs[:maxBatches]
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.dataDir, importLogFile), b, 0o644)
}

// AddLinks scans multi-line user input: single-post links go straight to
// Download; thread links are saved to Threads (REDESIGN §2.3). A post link
// inside a multi-floor thread is ambiguous — the user may have copied a deep
// link without wanting just that floor — so it's saved as a Thread and the
// frontend is asked to open the floor picker with that floor pre-checked.
// Every link's individual outcome is reported back for the import log.
func (a *App) AddLinks(text string) (ImportResult, error) {
	res := ImportResult{Items: []ImportItem{}}
	if err := a.waitReady(); err != nil {
		return res, err
	}
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	seenThread := map[int64]bool{}
	var floorPicks []map[string]any // {threadId, postId} waiting for the floor picker
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		item := ImportItem{URL: line, Status: "fail"}
		set := func(st, note string) {
			item.Status, item.Note = st, note
			res.Items = append(res.Items, item)
			switch st {
			case "ok":
				res.Posts++
			case "thread":
				res.Threads++
			case "dup":
				res.Dups++
			default:
				res.Failed++
			}
		}
		threadID, postID, err := forum.ParseLink(line)
		if err != nil {
			a.Store.AddLog("WARN", err.Error())
			set("fail", err.Error())
			continue
		}
		if postID > 0 {
			// 纯楼层 ID（没有 thread 上下文）无法探测楼层数，维持直加
			if threadID == 0 {
				set(a.addSinglePost(postID, line), "")
				continue
			}
			if seenThread[threadID] {
				set("dup", "同一主题在本次导入中重复")
				continue
			}
			seenThread[threadID] = true
			ref, err := a.forumFetch(threadID, 0)
			if err != nil {
				a.Store.AddLog("ERROR", "扫描 thread "+fmt.Sprint(threadID)+" 失败："+err.Error())
				set("fail", err.Error())
				continue
			}
			if ref.Error != "" {
				a.Store.AddLog("ERROR", "vr.php 错误："+ref.Error)
				set("fail", ref.Error)
				continue
			}
			if len(ref.Posts) <= 1 {
				// 只有目标楼层有图（或整帖单楼层），没有歧义，直加
				n, dup := 0, false
				for _, p := range ref.Posts {
					id, d, err := a.insertPost(ref, p, line, false)
					if err != nil {
						continue
					}
					if d {
						dup = true
						continue
					}
					if id > 0 {
						n++
					}
				}
				if n > 0 {
					set("ok", "")
				} else if dup {
					set("dup", "该楼层已收藏")
				} else {
					set("fail", "保存失败")
				}
				continue
			}
			dup, err := a.addThreadRef(threadID, line, ref)
			if err != nil {
				set("fail", err.Error())
				continue
			}
			if dup {
				set("dup", tr("该主题已收藏", "theme already saved"))
				continue
			}
			set("thread", fmt.Sprintf(tr("%d 楼层", "%d posts"), len(ref.Posts)))
			floorPicks = append(floorPicks, map[string]any{"threadId": threadID, "postId": postID})
			continue
		}
		if threadID > 0 && !seenThread[threadID] {
			seenThread[threadID] = true
			st, err := a.addThread(threadID, line)
			note := ""
			switch {
			case st == "thread" && err == nil:
				note = tr("多楼层，进入选择", "multiple posts — pick them next")
			case st == "ok" && err == nil:
				note = tr("单楼层直加", "single post — added directly")
			case err != nil:
				note = err.Error()
			}
			set(st, note)
		}
	}
	for _, fp := range floorPicks {
		wruntime.EventsEmit(a.ctx, "links:floorpick", fp)
	}
	msg := fmt.Sprintf(tr("已加入 %d 个主题到 Download（点击进入选择楼层）", "Added %d theme(s) to downloads (click to pick posts)"), res.Threads)
	if res.Posts > 0 {
		msg = fmt.Sprintf(tr("已保存 %d 个帖子；", "Saved %d post(s); "), res.Posts) + msg
	}
	if res.Dups > 0 {
		msg += fmt.Sprintf(tr("，%d 条重复忽略", ", %d duplicate(s) skipped"), res.Dups)
	}
	if res.Failed > 0 {
		msg += fmt.Sprintf(tr("，%d 条失败（见 Log）", ", %d failed (see Log)"), res.Failed)
	}
	if len(floorPicks) > 0 {
		msg += fmt.Sprintf(tr("；%d 条楼层链接所在主题有多个含图楼层，已加入主题列表（请在弹出的楼层选择器中确认）", "; %d post link(s) belong to themes with several image posts and were added as themes — confirm them in the post picker"), len(floorPicks))
	}
	a.Store.AddLog("INFO", msg)
	a.Hub.Log("INFO", msg)
	res.Message = msg
	return res, nil
}

// forumFetch 帖子解析的统一入口：优先走 vr.php API；API 网络失败或返回权限类
// 错误时，降级为用当前登录会话直接解析论坛网页版 showthread 页——vr.php 的
// 抓取账号看不了权限帖，而用户自己的登录能看（浏览器能看的这里也能看）。
func (a *App) forumFetch(threadID, postID int64) (*forum.ThreadRef, error) {
	var (
		ref *forum.ThreadRef
		err error
	)
	if threadID > 0 {
		ref, err = a.Forum.FetchThreadSmart(threadID)
	} else {
		ref, err = a.Forum.FetchPostSmart(postID)
	}
	if err == nil && ref.Error == "" {
		return ref, nil
	}
	apiErr := err
	if apiErr == nil {
		apiErr = fmt.Errorf("%s", ref.Error)
	}
	fb, ferr := a.Forum.FetchThreadWeb(threadID, postID)
	if ferr == nil && len(fb.Posts) > 0 && len(fb.Posts[0].Images) > 0 {
		a.Store.AddLog("INFO", fmt.Sprintf("vr.php 无法解析（%v），已改用论坛网页版（登录会话）解析成功", apiErr))
		return fb, nil
	}
	if ferr != nil {
		a.Store.AddLog("WARN", fmt.Sprintf("网页版解析降级失败：%v", ferr))
	}
	if err != nil {
		return nil, err
	}
	return ref, nil // ref.Error 非空，调用方按原有错误路径处理
}

// addSinglePost fetches one post and inserts it with images (预解析直链可稍后).
// Returns "ok", "dup" (already collected) or "fail".
func (a *App) addSinglePost(postID int64, srcURL string) string {
	ref, err := a.forumFetch(0, postID)
	if err != nil {
		a.Store.AddLog("ERROR", "扫描 post "+fmt.Sprint(postID)+" 失败："+err.Error())
		return "fail"
	}
	if ref.Error != "" {
		a.Store.AddLog("ERROR", "vr.php 错误："+ref.Error)
		return "fail"
	}
	if len(ref.Posts) == 0 {
		a.Store.AddLog("WARN", fmt.Sprintf("post #%d 没有可下载的图片", postID))
		return "fail"
	}
	n, dup := 0, false
	for _, p := range ref.Posts {
		id, d, err := a.insertPost(ref, p, srcURL, false)
		if err != nil {
			continue
		}
		if d {
			dup = true
			continue
		}
		if id > 0 {
			n++
		}
	}
	if n > 0 {
		return "ok"
	}
	if dup {
		return "dup"
	}
	return "fail"
}

// addThread saves every threads/ link after probing its floor count:
// single-floor threads go straight to Download as a normal post; multi-floor
// ones become a Thread entry (yellow row) for the floor picker. Returns the
// outcome: "ok" (single-floor added directly), "thread" (multi-floor row),
// "dup" (already collected) or "fail".
func (a *App) addThread(threadID int64, srcURL string) (string, error) {
	ref, err := a.forumFetch(threadID, 0)
	if err != nil {
		a.Store.AddLog("ERROR", "扫描 thread "+fmt.Sprint(threadID)+" 失败："+err.Error())
		return "fail", err
	}
	if ref.Error != "" {
		a.Store.AddLog("ERROR", "vr.php 错误："+ref.Error)
		return "fail", fmt.Errorf(tr("vr.php 错误：%s", "vr.php error: %s"), ref.Error)
	}
	if len(ref.Posts) == 1 {
		p := ref.Posts[0]
		if len(p.Images) == 0 {
			a.Store.AddLog("WARN", fmt.Sprintf("thread #%d 唯一楼层没有可下载的图片", threadID))
			return "fail", fmt.Errorf(tr("thread #%d 唯一楼层没有图片", "thread #%d: its only post has no images"), threadID)
		}
		id, dup, err := a.insertPost(ref, p, srcURL, false)
		if err != nil {
			return "fail", err
		}
		if dup {
			return "dup", nil
		}
		if id > 0 {
			a.Store.AddLog("INFO", fmt.Sprintf("主题 #%d 只有单楼层，已直接加入帖子 — %s", threadID, ref.Title))
			return "ok", nil
		}
		return "fail", fmt.Errorf("%s", tr("保存 post 失败", "failed to save the post"))
	}
	dup, err := a.addThreadRef(threadID, srcURL, ref)
	if err != nil {
		return "fail", err
	}
	if dup {
		return "dup", nil
	}
	return "thread", nil
}

// addThreadRef persists an already-fetched ThreadRef as a yellow Thread row.
// dup reports that a Thread row for this forum thread id already existed.
func (a *App) addThreadRef(threadID int64, srcURL string, ref *forum.ThreadRef) (bool, error) {
	if a.Store.HasThreadRow(threadID) {
		return true, nil
	}
	imgCount := 0
	for _, p := range ref.Posts {
		imgCount += len(p.Images)
	}
	t := &store.Thread{
		ForumThreadID: threadID,
		Title:         ref.Title,
		URL:           fmt.Sprintf("https://%s/threads/%d", a.Settings.Get().ForumDomain, threadID),
		Forum:         ref.Forum,
		PostCount:     len(ref.Posts),
		ImgCount:      imgCount,
		CreatedAt:     time.Now().Format("2006-01-02 15:04:05"),
		LastScannedAt: time.Now().Unix(),
	}
	if err := a.Store.UpsertThread(t); err != nil {
		a.Store.AddLog("ERROR", "保存主题失败："+err.Error())
		return false, err
	}
	a.Store.AddLog("INFO", fmt.Sprintf("主题 #%d 已加入 Download — %s（%d 楼层 · %d 图，点击进入选择楼层）", threadID, ref.Title, len(ref.Posts), imgCount))
	a.Hub.Log("INFO", fmt.Sprintf("主题 #%d 已加入 Download — %d 楼层 · %d 图", threadID, len(ref.Posts), imgCount))
	return false, nil
}

// insertPost writes one forum post as a Download row plus its images.
// dup reports that the post was already collected (INSERT OR IGNORE hit);
// err reports a storage failure.
func (a *App) insertPost(ref *forum.ThreadRef, p forum.PostRef, srcURL string, useThreadTitle bool) (int64, bool, error) {
	title := p.Title
	floors := len(ref.Posts)
	if useThreadTitle {
		// 楼层管理器勾选“直接使用主页标题”：条目和目录都直接用帖子名称，
		// 不加楼层号前缀、不建楼层子目录（用户只下载其中一层时的场景）
		title = ref.Title
		floors = 1
	} else if len(ref.Posts) > 1 {
		// 多楼层帖：楼层号放最前（如 6F—xxx），排序/浏览时一眼看出顺序
		title = fmt.Sprintf("%dF—%s", p.FloorNo, p.Title)
	}
	s := a.Settings.Get()
	// 板块分类用的论坛名：楼层选择器走 HTML 逐页通道时，AssembleFloors 组装的
	// ref 不带 Forum（只有 XML 通道才带），算出来的目录就会缺板块那一层，
	// 楼层文件夹直接摊在下载根目录、和板块目录平级（"小类和同大类目录"）。
	// 主题行这时已经存在于库里，按 threadID 补上即可。
	forum := ref.Forum
	if forum == "" && ref.ForumThreadID != 0 && a.Store != nil {
		forum = a.Store.ThreadForumOf(ref.ForumThreadID)
	}
	// 主题行已知板块时，把库里为空的 thread.forum 补上（按板块分类要用）
	if forum != "" && ref.ForumThreadID != 0 && a.Store != nil {
		a.Store.FillThreadForum(ref.ForumThreadID, forum)
	}
	row := &store.Post{
		ForumThreadID: ref.ForumThreadID,
		ForumPostID:   p.ForumPostID,
		FloorNo:       p.FloorNo,
		Title:         title,
		PostURL:       p.URL,
		Hosts:         p.Hosts,
		State:         "ready",
		// 板块名记在条目自己身上：单帖链接没有 thread 行，只靠 thread 表反查
		// 会永远查不到，下载目录就缺了最外层的板块（"小类和大类平级"）
		Forum: forum,
		// 主题总目录 + 多图册楼层的子目录（设置里“每帖一个文件夹/自动获取楼层标题/按板块分类”）
		Dir: pipeline.ComputePostDir(s.DownloadDir, forum, ref.Title, floors, title, p.FloorNo,
			s.PerPostFolder, s.FetchFloorTitle, s.SortByForum),
	}
	rowID, err := a.Store.AddPost(row)
	if err != nil {
		a.Store.AddLog("ERROR", "保存 post 失败："+err.Error())
		return 0, false, err
	}
	if rowID == 0 {
		// already collected
		return 0, true, nil
	}
	for i, img := range p.Images {
		_, err := a.Store.AddImage(&store.Image{
			PostRowID: rowID,
			Seq:       i + 1,
			PageURL:   img.PageURL,
			ThumbURL:  img.ThumbURL,
			Host:      img.Host,
			State:     "pending",
		})
		if err != nil {
			a.Store.AddLog("ERROR", "保存图片失败："+err.Error())
		}
	}
	a.autoRecover(rowID)
	a.Hub.MarkPostDirty(rowID)
	return rowID, false, nil
}

// ---------- background disk-recovery queue ----------

// autoRecover adopts files already on disk for a freshly inserted post, so
// a re-added link shows its real progress immediately instead of only after
// a download pass. Enqueued at bulk (lowest) priority.
func (a *App) autoRecover(rowID int64) {
	a.recoverEnqueue(rowID, 2)
}

// PrioritizeRecover bumps posts to the top of the recovery queue (downloading).
func (a *App) PrioritizeRecover(ids []int64) {
	for _, id := range ids {
		a.recoverEnqueue(id, 0)
	}
}

// PromotePostRecover bumps a post the user is currently viewing to mid priority.
func (a *App) PromotePostRecover(rowID int64) {
	a.recoverEnqueue(rowID, 1)
}

func (a *App) recoverEnqueue(rowID int64, level int) {
	a.recoverMu.Lock()
	defer a.recoverMu.Unlock()
	for l := 0; l < 3; l++ {
		for i, id := range a.recoverQ[l] {
			if id == rowID {
				if level < l { // promote
					a.recoverQ[l] = append(a.recoverQ[l][:i], a.recoverQ[l][i+1:]...)
					a.recoverQ[level] = append(a.recoverQ[level], rowID)
					a.recoverKick()
				}
				return
			}
		}
	}
	if a.recoverSeen[rowID] {
		return // already processed (or being processed)
	}
	a.recoverSeen[rowID] = true
	a.recoverQ[level] = append(a.recoverQ[level], rowID)
	a.recoverKick()
}

func (a *App) recoverKick() {
	select {
	case a.recoverNotify <- struct{}{}:
	default:
	}
}

func (a *App) recoverDequeue() int64 {
	for l := 0; l < 3; l++ {
		if len(a.recoverQ[l]) > 0 {
			id := a.recoverQ[l][0]
			a.recoverQ[l] = a.recoverQ[l][1:]
			return id
		}
	}
	return 0
}

// recoverWorker pulls posts off the priority queue: downloading-now posts
// first, then the post selected in the UI, then the bulk-add backlog.
func (a *App) recoverWorker() {
	for {
		a.recoverMu.Lock()
		rowID := a.recoverDequeue()
		a.recoverMu.Unlock()
		if rowID == 0 {
			<-a.recoverNotify
			continue
		}
		a.doRecover(rowID)
	}
}

func (a *App) doRecover(rowID int64) {
	a.Pipe.ResolvePost(rowID) // record direct URLs + names into the DB
	n, _, err := a.Pipe.RecoverFromDisk(rowID)
	if err != nil || n == 0 {
		a.recoverMu.Lock()
		delete(a.recoverSeen, rowID)
		a.recoverMu.Unlock()
		return
	}
	a.logf("INFO", "post #%d 已从磁盘识别 %d 个已有文件", rowID, n)
	imgs, err := a.Store.ListImages(rowID)
	if err != nil || len(imgs) == 0 {
		return
	}
	all := true
	for _, im := range imgs {
		if im.State != "done" {
			all = false
			break
		}
	}
	if all {
		// don't stomp a live download's status — runPost writes the final
		// state itself when it finishes; otherwise "downloading" flickers
		// to "paused" for as long as the recovery raced it
		if !a.Pipe.IsRunning(rowID) {
			a.Store.SetPostStatus(rowID, "done")
		}
	} else if !a.Pipe.IsRunning(rowID) {
		// partial progress: reflect it in the status instead of "ready",
		// so the list shows the post was resumed from disk without starting
		a.Store.SetPostStatus(rowID, "paused")
	}
	a.Hub.MarkPostDirty(rowID)
}

// logf writes to the log store and the Hub (no-op before init completes).
func (a *App) logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if a.Store != nil {
		a.Store.AddLog(level, msg)
	}
	a.Hub.Log(level, msg)
}

// ---------- local image server (thumbnail cache) ----------

// thumbSem bounds concurrent thumbnail fetches from image hosts.
var thumbSem = make(chan struct{}, 8)

var thumbClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:               proxy.Func(),
		MaxIdleConnsPerHost: 8,
	},
}

// serveImage is the AssetServer fallback handler. /thumbs/{imgID} serves a
// thumbnail through the persistent bbolt cache (fetched from the image host
// once, then served locally forever); /local/{imgID} streams an
// already-downloaded original straight from disk. This keeps the side panel
// off the image hosts entirely after the first view.
func (a *App) serveImage(w http.ResponseWriter, r *http.Request) {
	if a.waitReady() != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "floorthumb" {
		// 缩略图 URL 在 query 参数里，不走 /{kind}/{id} 路径
		a.serveFloorThumb(w, r)
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch parts[0] {
	case "thumbs":
		a.serveThumb(w, r, id)
	case "local":
		a.serveLocal(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) serveThumb(w http.ResponseWriter, r *http.Request, id int64) {
	// 缓存键 = 缩略图 URL 哈希，而不是图片行 id：导入/重新添加会重建图片行
	// （id 变、URL 不变），按 id 键存的缓存会全部失联；按 URL 键则永远命中
	img, err := a.Store.GetImage(id)
	if err != nil || img.ThumbURL == "" {
		http.NotFound(w, r)
		return
	}
	sum := sha1.Sum([]byte(img.ThumbURL))
	key := hex.EncodeToString(sum[:])
	if data, err := a.Store.GetThumb("thumb", key); err == nil && len(data) > 0 {
		a.writeThumb(w, data)
		return
	}
	// 旧版缓存按 image 行 id 存——命中就顺手迁移到 URL 键，平滑升级
	if data, err := a.Store.GetThumb("thumb", strconv.FormatInt(id, 10)); err == nil && len(data) > 0 {
		a.Store.PutThumb("thumb", key, data)
		a.writeThumb(w, data)
		return
	}
	thumbSem <- struct{}{}
	defer func() { <-thumbSem }()
	req, err := http.NewRequest("GET", img.ThumbURL, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Referer", a.Hosts.DownloadReferer(img.ThumbURL))
	resp, err := thumbClient.Do(req)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(data) == 0 {
		http.Error(w, "read failed", http.StatusBadGateway)
		return
	}
	a.Store.PutThumb("thumb", key, data)
	a.writeThumb(w, data)
}

// serveFloorThumb 楼层选择器缩略图的本地缓存代理：/floorthumb?url=<图床缩略图>。
// 缓存键与 /thumbs 相同（缩略图 URL 的 SHA1），楼层缩略图与已收录图片的
// 缩略图 URL 相同时直接命中已有缓存；未命中则带防盗链 Referer 抓一次并存库。
func (a *App) serveFloorThumb(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		http.NotFound(w, r)
		return
	}
	sum := sha1.Sum([]byte(u))
	key := hex.EncodeToString(sum[:])
	if data, err := a.Store.GetThumb("thumb", key); err == nil && len(data) > 0 {
		a.writeThumb(w, data)
		return
	}
	thumbSem <- struct{}{}
	defer func() { <-thumbSem }()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Referer", a.Hosts.DownloadReferer(u))
	resp, err := thumbClient.Do(req)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(data) == 0 {
		http.Error(w, "read failed", http.StatusBadGateway)
		return
	}
	a.Store.PutThumb("thumb", key, data)
	a.writeThumb(w, data)
}

func (a *App) writeThumb(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(data)
}

func (a *App) serveLocal(w http.ResponseWriter, r *http.Request, id int64) {
	img, err := a.Store.GetImage(id)
	if err != nil || img.FilePath == "" {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(img.FilePath)
	if err != nil || st.Size() == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, img.FilePath)
}

// ---------- floor picker (REDESIGN §2.4) ----------

// floorBatchPages 每批抓取的 HTML 页数（一页约 50 层，一批 ≈300 层）。
const floorBatchPages = 6

// floorChunkRows 每批"放行"的楼层条数：首屏只给这么多，点一次加载更多
// 再放开同样多条。内存里可以有全量（XML 骨架一次到手），但前端只看得到
// visibleCap 以内的部分。
const floorChunkRows = 300

// floorTextJob 楼层选择器的分批抓取状态。骨架与文本名共用一个逐页通道：
// vr.php XML 成功时骨架一次拿全（快），HTML 页只用于楼层文本命名；
// XML 权限不足时骨架本身也逐页从 HTML 来。每抓完一页 emit "floors:page"
// 让前端增量显示，"加载更多/加载全部"按钮控制后续批次。
type floorTextJob struct {
	mu        sync.Mutex
	rowID     int64
	threadID  int64
	title     string           // HTML 通道组装 ref 用的标题（来自 DB）
	xml       *forum.ThreadRef // XML 通道的全量骨架（成功时非 nil）
	xmlTried  bool
	floors    []forum.Floor    // HTML 通道已抓到的楼层
	texts     map[int64]string // HTML 页解析出的楼层文本（两通道共用命名）
	nextURL   string
	done      bool
	running   bool
	allWanted bool
	fetched   int
	// 分批放行：visibleCap 为当前允许下发到前端的条数上限（-1 = 不限），
	// emitted 为已下发条数。内存 floors 可以是全量（XML 通道一次到手），
	// 但前端只看得到前 visibleCap 条，"加载更多"只是把上限 +300。
	visibleCap int
	emitted    int
}

func (a *App) floorJobFor(rowID, threadID int64) *floorTextJob {
	if v, ok := a.floorJobs.Load(rowID); ok {
		return v.(*floorTextJob)
	}
	j := &floorTextJob{
		rowID:      rowID,
		threadID:   threadID,
		texts:      map[int64]string{},
		nextURL:    a.Forum.ThreadPageURL(threadID),
		visibleCap: floorChunkRows,
	}
	_ = a.Store.DB().QueryRow(`SELECT title FROM thread WHERE id=?`, rowID).Scan(&j.title)
	// 持久化缓存：上次抓过的楼层直接可用（重启/离线也能打开楼层列表）
	rows, meta, err := a.Store.LoadFloorCache(rowID)
	if err != nil {
		a.Store.AddLog("WARN", fmt.Sprintf("读取楼层缓存失败（主题行 #%d）：%v", rowID, err))
	}
	if err == nil {
		for _, r := range rows {
			var imgs []forum.FloorImg
			_ = json.Unmarshal([]byte(r.ImagesJSON), &imgs)
			j.floors = append(j.floors, forum.Floor{
				ID: r.ForumPostID, FloorNo: r.FloorNo, Text: r.Text, SourceTitle: r.SourceTitle, Images: imgs,
			})
			if r.Text != "" {
				j.texts[r.ForumPostID] = r.Text
			}
		}
		if meta.NextURL != "" {
			j.nextURL = a.repairFloorNextURL(threadID, meta.NextURL)
		}
		j.done = meta.Done
	}
	v, _ := a.floorJobs.LoadOrStore(rowID, j)
	return v.(*floorTextJob)
}

// floorSummaryLocked 把缓存快照转成前端楼层行（调用方持有 j.mu）。
// XML 骨架与 HTML 逐页楼层统一存在 j.floors 里；名字优先用正文文字
// （图册名），没有则回退 XML title。
func (a *App) floorSummaryLocked(j *floorTextJob) []map[string]any {
	out := []map[string]any{}
	for i, f := range j.floors {
		no := f.FloorNo
		if no == 0 {
			no = i + 1
		}
		thumb := ""
		hostCount := map[string]int{}
		var order []string
		for _, im := range f.Images {
			if thumb == "" && im.Thumb != "" {
				thumb = im.Thumb
			}
			// 右侧显示楼层图片所属的 host 名（规则名，未知图床显示域名）
			h := a.Hosts.RuleNameOf(im.Main)
			if h == "" {
				h = domainOf(im.Main)
			}
			if _, ok := hostCount[h]; !ok {
				order = append(order, h)
			}
			hostCount[h]++
		}
		name := f.Text
		if name == "" {
			name = f.SourceTitle
		}
		var parts []string
		for _, h := range order {
			parts = append(parts, fmt.Sprintf("%s (%d)", h, hostCount[h]))
		}
		out = append(out, map[string]any{
			"postId": f.ID,
			"no":     no,
			"name":   name,
			"imgs":   len(f.Images),
			"hosts":  strings.Join(parts, ", "),
			"thumb":  thumb,
			"url":    a.Forum.PostURL(j.threadID, f.ID),
		})
	}
	return out
}

// repairFloorNextURL 修掉老版本把「下一页」链接越拼越长的地址。
// vBulletin 的 rel=next 是根相对但省略了开头的 /，旧代码按当前页目录解析，
// 于是每翻一页多叠一段 threads/<id>/，变成 .../threads/x/threads/x/page18。
// 这里取最后一段 threads/<id>/pageN，把页码保住，不用从第 1 页重抓。
func (a *App) repairFloorNextURL(threadID int64, raw string) string {
	if !strings.Contains(raw, "threads/threads/") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return a.Forum.ThreadPageURL(threadID)
	}
	trimmed := strings.TrimPrefix(u.Path, "/")
	i := strings.LastIndex(trimmed, "threads/")
	if i < 0 {
		return a.Forum.ThreadPageURL(threadID)
	}
	out := u.Scheme + "://" + u.Host + "/" + trimmed[i:]
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// domainOf 取 URL 的域名部分（host 规则没覆盖时的兜底显示）。
func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

// floorEmitPending 把"已放行但还没下发"的楼层段 emit 给前端。
// visibleCap 是放行上限（-1 = 不限），emitted 记录已下发条数，因此重复调用
// 不会重复推送。texts 为本次附带的楼层名更新（可为 nil）。
// 返回值表示内存里的楼层是否已全部下发（没有被上限截断）。
func (a *App) floorEmitPending(rowID int64, j *floorTextJob, texts map[int64]string) bool {
	j.mu.Lock()
	rows := a.floorSummaryLocked(j)
	total := len(rows)
	limit := total
	if j.visibleCap >= 0 && j.visibleCap < limit {
		limit = j.visibleCap
	}
	if j.emitted > limit {
		j.emitted = limit
	}
	var out []map[string]any
	if limit > j.emitted {
		out = rows[j.emitted:limit]
		j.emitted = limit
	}
	allVisible := limit >= total
	done := j.done && allVisible
	j.mu.Unlock()

	if len(out) == 0 && len(texts) == 0 && !done {
		return allVisible
	}
	payload := map[string]any{
		"rowID": rowID,
		"done":  done,
		"total": total,
		"cap":   limit,
	}
	if len(out) > 0 {
		payload["floors"] = out
	}
	if len(texts) > 0 {
		payload["texts"] = texts
	}
	wruntime.EventsEmit(a.ctx, "floors:page", payload)
	return allVisible
}

// floorTextKick 若没有 worker 在跑且未抓完，启动一个。
func (a *App) floorTextKick(rowID int64, j *floorTextJob) {
	j.mu.Lock()
	if j.running || j.done {
		j.mu.Unlock()
		return
	}
	j.running = true
	j.mu.Unlock()
	go a.floorWorker(rowID, j)
}

// FetchMoreFloors 继续抓取楼层（骨架/文本名）：all=false 放行下一批
// （再多 300 层），all=true 取消上限。由楼层选择器底部的加载按钮触发。
func (a *App) FetchMoreFloors(threadRowID int64, all bool) {
	if a.waitReady() != nil {
		return
	}
	var threadID int64
	if err := a.Store.DB().QueryRow(`SELECT thread_id FROM thread WHERE id=?`, threadRowID).Scan(&threadID); err != nil {
		return
	}
	j := a.floorJobFor(threadRowID, threadID)
	j.mu.Lock()
	if all {
		j.allWanted = true
		j.visibleCap = -1
	} else {
		// fetched 是跨批累计的，按批截断时必须归零，否则重启的 worker
		// 一进来就命中 "fetched >= floorBatchPages" 立即退出（按钮像没反应）
		j.fetched = 0
		if j.visibleCap >= 0 {
			j.visibleCap += floorChunkRows
		}
	}
	j.mu.Unlock()
	// 上限变大后，内存里已有的楼层（缓存 / XML 骨架）立刻补发，不用等网络
	a.floorEmitPending(threadRowID, j, nil)
	a.floorTextKick(threadRowID, j)
}

func (a *App) floorWorker(rowID int64, j *floorTextJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for {
		// 先把已放行的部分推出去（首屏、点加载更多后补发、收尾的 done 都走这里）
		a.floorEmitPending(rowID, j, nil)

		j.mu.Lock()
		if j.done || ctx.Err() != nil {
			j.running = false
			j.mu.Unlock()
			return
		}
		// 分批预算：非"加载全部"时本批抓够 floorBatchPages 页就收工，
		// 等用户点"加载更多"（fetched 在 FetchMoreFloors 里归零）再继续。
		if !j.allWanted && j.fetched >= floorBatchPages {
			j.running = false
			j.mu.Unlock()
			return
		}
		if !j.xmlTried {
			j.xmlTried = true
			threadID := j.threadID
			j.mu.Unlock()
			ref, err := a.Forum.FetchThread(threadID)
			j.mu.Lock()
			if err == nil && ref.Error == "" && len(ref.Posts) > 0 {
				j.xml = ref
				// XML 通道能拿到板块名：老主题行可能是空板块（HTML 兜底通道加的），
				// 顺手补上，后续楼层才会落进 <板块>/<主题> 目录
				a.Store.FillThreadForum(threadID, ref.Forum)
				// XML 骨架全量替换内存与持久化缓存（保留已抓到的文本名）
				newFloors := make([]forum.Floor, 0, len(ref.Posts))
				newRows := make([]store.FloorRow, 0, len(ref.Posts))
				for _, p := range ref.Posts {
					fl := forum.Floor{ID: p.ForumPostID, FloorNo: p.FloorNo, SourceTitle: p.Title}
					if t, ok := j.texts[p.ForumPostID]; ok {
						fl.Text = t
					}
					for _, im := range p.Images {
						fl.Images = append(fl.Images, forum.FloorImg{Main: im.PageURL, Thumb: im.ThumbURL})
					}
					newFloors = append(newFloors, fl)
					newRows = append(newRows, floorRowOf(fl))
				}
				j.floors = newFloors
				// 骨架顺序可能与 HTML 逐页顺序不同，已下发部分要重发一遍
				// （前端按 postId 合并，不会出现重复行）
				j.emitted = 0
				a.Store.UpsertFloors(rowID, newRows, true)
				a.Store.SaveFloorMeta(rowID, store.FloorMeta{NextURL: j.nextURL, Done: j.done})
				j.mu.Unlock()
				// 循环顶部统一按 visibleCap 分段放行，不会一次灌满前端
				continue
			}
			j.mu.Unlock()
			continue // XML 失败：走 HTML 分页通道（骨架+文本逐页来）
		}
		pageURL := j.nextURL
		if pageURL == "" {
			j.done = true
			j.mu.Unlock()
			continue // 交给循环顶部发收尾的 done
		}
		j.mu.Unlock()

		page, next, err := a.Forum.FetchPage(ctx, pageURL)
		if err != nil {
			// 不要标记 done：限速/超时/临时 5xx 之后用户点「加载更多」还能接着抓。
			// 以前这里直接 done=true，遇到一次失败长主题就永久停在第 N 页。
			j.mu.Lock()
			j.running = false
			fetched := j.fetched
			j.mu.Unlock()
			a.Store.AddLog("WARN", fmt.Sprintf("楼层抓取中断（主题行 #%d，已抓 %d 页）：%v", rowID, fetched, err))
			wruntime.EventsEmit(a.ctx, "floors:page", map[string]any{"rowID": rowID, "error": err.Error(), "done": false})
			return
		}
		parsed := a.Forum.ParseFloorPage(page)
		j.mu.Lock()
		fresh := map[int64]string{}
		pageRows := make([]store.FloorRow, 0, len(parsed))
		for _, f := range parsed {
			if f.Text != "" {
				if _, dup := j.texts[f.ID]; !dup {
					j.texts[f.ID] = f.Text
					fresh[f.ID] = f.Text
				}
				// 同步回内存楼层行：floorSummaryLocked 的行名来自 f.Text，
				// 不同步的话重新拉取会用旧快照把已显示的名字覆盖回标题
				for i := range j.floors {
					if j.floors[i].ID == f.ID {
						j.floors[i].Text = f.Text
						break
					}
				}
			}
			if j.xml == nil {
				dup := false
				for _, ex := range j.floors {
					if ex.ID == f.ID {
						dup = true
						break
					}
				}
				if !dup {
					j.floors = append(j.floors, f)
				}
			}
			pageRows = append(pageRows, floorRowOf(f))
		}
		j.nextURL = next
		j.fetched++
		if next == "" {
			j.done = true
		}
		done := j.done
		a.Store.UpsertFloors(rowID, pageRows, false)
		a.Store.SaveFloorMeta(rowID, store.FloorMeta{NextURL: j.nextURL, Done: done})
		j.mu.Unlock()
		// 只推放行区间内的新楼层（fresh 文本名单独附带）
		a.floorEmitPending(rowID, j, fresh)
	}
}

// floorRowOf 楼层转持久化行。
func floorRowOf(f forum.Floor) store.FloorRow {
	b, _ := json.Marshal(f.Images)
	return store.FloorRow{ForumPostID: f.ID, FloorNo: f.FloorNo, Text: f.Text, SourceTitle: f.SourceTitle, ImagesJSON: string(b)}
}

// ClearFloorCache 清除某主题的楼层缓存（内存 + SQLite），下次打开楼层
// 选择器会从头重新抓取。由楼层管理页的"清除缓存"按钮触发。
func (a *App) ClearFloorCache(threadRowID int64) {
	if err := a.waitReady(); err != nil {
		return
	}
	a.floorJobs.Delete(threadRowID)
	if err := a.Store.DeleteFloorCache(threadRowID); err != nil {
		a.Store.AddLog("WARN", "清除楼层缓存失败："+err.Error())
		return
	}
	a.Store.AddLog("INFO", fmt.Sprintf("主题 #%d 的楼层缓存已清除", threadRowID))
}

// floorTextFor 查某楼层的文本名（缓存没有返回空，回退用 XML title）。
func (a *App) floorTextFor(rowID, postID int64) string {
	if v, ok := a.floorJobs.Load(rowID); ok {
		j := v.(*floorTextJob)
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.texts[postID]
	}
	return ""
}

// floorRefForConfirm 确认楼层时拿到可插入的 ThreadRef：XML 成功直接用
// 全量骨架；HTML 通道等后台抓取覆盖全部选中楼层（或抓完/超时）后组装。
func (a *App) floorRefForConfirm(rowID, threadID int64, want map[int64]bool) *forum.ThreadRef {
	j := a.floorJobFor(rowID, threadID)
	a.FetchMoreFloors(rowID, true)
	deadline := time.Now().Add(60 * time.Second)
	for {
		j.mu.Lock()
		if j.xml != nil {
			ref := j.xml
			j.mu.Unlock()
			return ref
		}
		covered := true
		for id := range want {
			found := false
			for _, f := range j.floors {
				if f.ID == id {
					found = true
					break
				}
			}
			if !found {
				covered = false
				break
			}
		}
		floors := append([]forum.Floor(nil), j.floors...)
		title := j.title
		j.mu.Unlock()
		if covered || j.done || time.Now().After(deadline) {
			return a.Forum.AssembleFloors(threadID, title, floors)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// StartFloorStream 启动某个已保存主题的楼层流（供楼层选择器用）。
// 统一事件驱动：无论楼层来自持久化缓存还是网络抓取，都由 "floors:page"
// 事件按 300 层一段增量推送。返回值恒为空数组，前端只能靠事件渐进渲染，
// 不要指望从这里拿到楼层数据。
// 缓存已抓完（done）时只回放缓存并带上 done=true，加载按钮随事件隐藏。
func (a *App) StartFloorStream(threadRowID int64) ([]map[string]any, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	var threadID int64
	err := a.Store.DB().QueryRow(`SELECT thread_id FROM thread WHERE id=?`, threadRowID).Scan(&threadID)
	if err != nil {
		return nil, err
	}
	j := a.floorJobFor(threadRowID, threadID)
	j.mu.Lock()
	cached := len(j.floors) > 0
	if cached {
		// 重新打开时前端列表是空的，从头发一遍；条数仍受 visibleCap 限制
		j.emitted = 0
	}
	j.mu.Unlock()
	if cached {
		go a.floorEmitPending(threadRowID, j, nil)
		a.floorTextKick(threadRowID, j) // 上次没抓完的继续抓
		return []map[string]any{}, nil
	}
	a.floorTextKick(threadRowID, j)
	return []map[string]any{}, nil
}

func firstThumb(imgs []forum.ImageRef) string {
	for _, im := range imgs {
		if im.ThumbURL != "" {
			return im.ThumbURL
		}
	}
	return ""
}

// ConfirmFloors splits the chosen floors into Download rows (REDESIGN §2.3).
// useThreadTitle：勾选“直接使用主页标题”时为 true —— 选中的楼层用帖子
// 名称命名，不加楼层号前缀、不建楼层子目录。
func (a *App) ConfirmFloors(threadRowID int64, postIDs []int64, useThreadTitle bool) (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	var threadID int64
	err := a.Store.DB().QueryRow(`SELECT thread_id FROM thread WHERE id=?`, threadRowID).Scan(&threadID)
	if err != nil {
		return "", err
	}
	want := map[int64]bool{}
	for _, id := range postIDs {
		want[id] = true
	}
	ref := a.floorRefForConfirm(threadRowID, threadID, want)
	if ref == nil {
		return "", fmt.Errorf("%s", tr("楼层解析失败", "failed to parse the post"))
	}
	n := 0
	for _, p := range ref.Posts {
		if want[p.ForumPostID] {
			// 楼层正文的文字（图册名）优先作为下载行标题/目录名
			if txt := a.floorTextFor(threadRowID, p.ForumPostID); txt != "" {
				p.Title = txt
			}
			if id, _, _ := a.insertPost(ref, p, "", useThreadTitle); id > 0 {
				n++
			}
		}
	}
	// single-floor thread: a Thread wrapper adds nothing — drop it so the
	// floor shows as a plain post in the normal list instead of a yellow
	// row hiding its only floor inside a manage view
	if n > 0 && len(ref.Posts) <= 1 {
		a.Store.DeleteThread(threadRowID)
		a.Store.AddLog("INFO", fmt.Sprintf("主题 #%d 仅 1 个楼层，已转为普通帖子", threadID))
	}
	msg := fmt.Sprintf("已加入 %d 个楼层到 Download", n)
	a.Store.AddLog("INFO", msg)
	a.Hub.Log("INFO", msg)
	return msg, nil
}

// ---------- list APIs ----------

func (a *App) ListPosts() ([]store.Post, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	return a.Store.ListPosts()
}

func (a *App) ListThreads() ([]store.Thread, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	return a.Store.ListThreads()
}

func (a *App) ListImages(postRowID int64) ([]store.Image, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	return a.Store.ListImages(postRowID)
}

func (a *App) ListLogs() ([]store.LogEntry, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	return a.Store.ListLogs(500)
}

// RefreshPosts 重新读取指定条目的最新行（前端收到 posts:dirty 后调用）。
func (a *App) RefreshPosts(ids []int64) ([]store.Post, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	out := []store.Post{}
	for _, id := range ids {
		p, err := a.Store.GetPost(id)
		if err != nil {
			continue
		}
		// counts
		imgs, _ := a.Store.ListImages(id)
		for _, im := range imgs {
			switch im.State {
			case "done":
				p.Done++
			case "error":
				p.Failed++
			}
			p.Total++
		}
		out = append(out, *p)
	}
	return out, nil
}

// ---------- download control ----------

func (a *App) StartPosts(ids []int64) {
	if a.waitReady() != nil {
		return
	}
	a.applyPipeSettings()
	a.resetIdleNotify()
	a.Pipe.Start(ids...)
	a.PrioritizeRecover(ids)
	a.Store.AddLog("INFO", fmt.Sprintf("开始下载 %d 个帖子", len(ids)))
}

func (a *App) StopPosts(ids []int64) {
	if a.waitReady() != nil {
		return
	}
	a.Pipe.Stop(ids...)
}

// StartAll starts every unfinished post, plus finished ones whose folder was
// emptied/cleaned by hand (those need their gaps refilled).
func (a *App) StartAll() {
	if a.waitReady() != nil {
		return
	}
	a.applyPipeSettings()
	posts, _ := a.Store.ListPosts()
	var ids []int64
	for _, p := range posts {
		if p.State != "done" || a.postHasMissingFiles(p.ID) {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) > 0 {
		a.resetIdleNotify()
		a.Pipe.Start(ids...)
		a.PrioritizeRecover(ids)
	}
	a.Store.AddLog("INFO", "Start 全部")
}

// postHasMissingFiles reports whether some image this post already counts as
// downloaded is no longer on disk (folder cleaned by hand, drive moved, …).
// Only stats the files — cheap enough to run for every post on "Start all".
func (a *App) postHasMissingFiles(postRowID int64) bool {
	if a.Pipe == nil || (a.Pipe.ExistingFilePolicy == "overwrite" && a.Pipe.IsRunning(postRowID)) {
		return false
	}
	imgs, err := a.Store.ListImages(postRowID)
	if err != nil {
		return false
	}
	for _, im := range imgs {
		if im.State != "done" {
			continue
		}
		if im.FilePath == "" {
			return true
		}
		if st, err := os.Stat(im.FilePath); err != nil || st.Size() == 0 {
			return true
		}
	}
	return false
}

// ensureImageRows（pipeline.OnBeforeStart）：点击下载时的数量核对规则。
// recheckDone 只能发现"单张图文件没了"，发现不了覆盖类损坏——多条图片
// 记录指向同一个文件（历史 bug：引用楼层的图片全部用帖子标题命名，互相
// 覆盖后每条记录的文件都"还在"）。这里核对：
//   - 图片行为空（记录丢失）；
//   - 声称完成的帖子里，去重后的文件数 < 已完成数（覆盖痕迹）。
//
// 命中则从 vr.php 重新拉取楼层图片列表（新逻辑会把引用楼层展开成图床链
// 接），重建图片行并全部置为待下载，然后返回 true 让 pipeline 重新加载。
func (a *App) ensureImageRows(postRowID int64) bool {
	p, err := a.Store.GetPost(postRowID)
	if err != nil || p == nil {
		return false
	}
	imgs, err := a.Store.ListImages(postRowID)
	if err != nil {
		return false
	}
	bad := false
	if len(imgs) == 0 {
		bad = true // 图片记录为空：目录就算有文件也对不上号，重建
	} else if p.State == "done" {
		doneN := 0
		files := map[string]bool{}
		for _, im := range imgs {
			if im.State != "done" {
				continue
			}
			doneN++
			if im.FilePath != "" {
				files[im.FilePath] = true
			}
		}
		if doneN > 0 && len(files) < doneN {
			// 多条记录指向同一个文件（两张不同的图算出了同一个名字）。
			// 以前这里会整帖重建图片列表 → 已经下完的帖子一点“开始下载”
			// 就全部重下。现在只把重复的那几条退回待下载，其余保持完成。
			if n, rerr := a.Store.ResetDuplicateImageFiles(postRowID); rerr == nil && n > 0 {
				a.Store.AddLog("INFO", fmt.Sprintf("post #%d 有 %d 条记录与其它图共用同一个文件，已退回待下载（其余保持完成）", p.ForumPostID, n))
				a.Hub.MarkPostDirty(postRowID)
				return true // 让调用方重新读取图片列表
			}
			bad = true // 兜底：还是对不上就整帖重建
		}
	}
	if !bad {
		return false
	}
	ref, err := a.Forum.FetchPost(p.ForumPostID)
	if err != nil || ref.Error != "" || len(ref.Posts) == 0 {
		a.Store.AddLog("WARN", fmt.Sprintf("post #%d 数量核对异常但重新拉取失败，按原列表下载", p.ForumPostID))
		return false
	}
	if err := a.Store.DeletePostImages(postRowID); err != nil {
		return false
	}
	n := 0
	for _, fp := range ref.Posts {
		for _, img := range fp.Images {
			a.Store.AddImage(&store.Image{
				PostRowID: postRowID,
				Seq:       n + 1,
				PageURL:   img.PageURL,
				ThumbURL:  img.ThumbURL,
				Host:      img.Host,
				State:     "pending",
			})
			n++
		}
	}
	a.Store.AddLog("INFO", fmt.Sprintf("post #%d 文件数量核对异常（%d 条记录 / %d 个不同文件），已按图床重新解析并重建 %d 张图的下载列表",
		p.ForumPostID, len(imgs), countFilesOf(imgs), n))
	a.Hub.MarkPostDirty(postRowID)
	return n > 0
}

func countFilesOf(imgs []store.Image) int {
	files := map[string]bool{}
	for _, im := range imgs {
		if im.State == "done" && im.FilePath != "" {
			files[im.FilePath] = true
		}
	}
	return len(files)
}

func (a *App) StopAll() {
	if a.waitReady() != nil {
		return
	}
	a.Pipe.Stop()
}

func (a *App) RunningPosts() ([]int64, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	posts, _ := a.Store.ListPosts()
	var out []int64
	for _, p := range posts {
		if a.Pipe.IsRunning(p.ID) {
			out = append(out, p.ID)
		}
	}
	return out, nil
}

func (a *App) applyPipeSettings() {
	s := a.Settings.Get()
	// the toggle decides whether the manual proxy address is used at all;
	// off = system proxy auto-detect
	if s.ProxyEnabled {
		proxy.SetManual(s.Proxy)
	} else {
		proxy.SetManual("")
	}
	a.Pipe.ApplySettings(s.PostsConcurrent, s.HostConcurrent, s.MaxRetries, s.TimeoutSec)
	a.Pipe.FetchOrigName = s.FetchOrigName
	a.Hosts.SetFetchOrigName(s.FetchOrigName) // host 规则里 purpose=filename 的步骤按此开关跳过
	a.Pipe.ExistingFilePolicy = s.ExistingFiles
	a.Pipe.PerPostFolder = s.PerPostFolder
	a.Pipe.FetchFloorTitle = s.FetchFloorTitle
	a.Pipe.SortByForum = s.SortByForum
	a.Pipe.BaseDir = s.DownloadDir
	a.Pipe.TempDir = s.TempDir
	a.Forum.SetRate(s.RateLimit)
}

func (a *App) RetryFailed(postRowID int64) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	imgs, err := a.Store.ListImages(postRowID)
	if err != nil {
		return err
	}
	for _, im := range imgs {
		if im.State == "error" {
			// drop the cached direct url too — it was likely produced by
			// stale/broken rules; re-resolve on the retry pass
			a.Store.ExecImageUpdate(im.ID, "", "")
			a.Store.QueueImageState(im.ID, "pending", 0, "", "", 0)
		}
	}
	a.applyPipeSettings()
	a.PrioritizeRecover([]int64{postRowID})
	a.Pipe.Start(postRowID)
	return nil
}

var reDiskImg = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|webp|bmp)$`)

// DiskStats counts actual image files in the post's folder, independent of
// the database (answers "how many has this folder already got on disk").
func (a *App) DiskStats(postRowID int64) (map[string]any, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	dir := a.PostDir(postRowID)
	count, bytes := 0, int64(0)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, en := range entries {
			if en.IsDir() || strings.HasSuffix(en.Name(), ".vripper.tmp") || !reDiskImg.MatchString(en.Name()) {
				continue
			}
			if info, err := en.Info(); err == nil {
				count++
				bytes += info.Size()
			}
		}
	}
	return map[string]any{"count": count, "bytes": bytes, "dir": dir}, nil
}

// ---------- post actions ----------

// RenamePost 重命名条目并同步磁盘文件夹，返回给人看的结果说明。
func (a *App) RenamePost(rowID int64, title string) (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("%s", tr("名称不能为空", "name cannot be empty"))
	}
	if err := a.Store.RenamePost(rowID, title); err != nil {
		return "", err
	}
	// 本地磁盘目录同步改名：用新标题重推目录并把已有文件整体搬过去
	// （EnsurePostDir/moveDir 失败时保持原目录，不会丢文件）。
	// 正在下载的帖子跳过——文件正在写，搬动有竞态；停止/完成后会自动归位。
	p, err := a.Store.GetPost(rowID)
	if err != nil || p == nil {
		return "", nil
	}
	msg := a.Pipe.SyncFolderToTitle(p)
	a.Hub.MarkPostDirty(rowID)
	if msg == "" {
		// 目录本来就是这个名字（同名/大小写差异）：明确说出来，别让用户以为没生效
		if d := a.PostDir(rowID); d != "" {
			return tr("文件夹已是「"+filepath.Base(d)+"」", "Folder is already “"+filepath.Base(d)+"”"), nil
		}
		return tr("该条目还没有文件夹，下载时会按新名称创建", "This item has no folder yet; it will be created with the new name"), nil
	}
	return msg, nil
}

func (a *App) SetPostDir(rowID int64, dir string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Store.SetPostDir(rowID, dir)
}

// DeleteThread 删除一条主题（thread 行），连同它下面的楼层一起删除。
// 以前只删 thread 行，楼层失去归属后会以“普通帖子”的身份散落回列表里。
func (a *App) DeleteThread(rowID int64) error {
	_, err := a.DeleteThreads([]int64{rowID})
	return err
}

// DeleteThreads 批量删除主题（含其下楼层），返回被删掉的楼层数。
func (a *App) DeleteThreads(rowIDs []int64) (int, error) {
	if err := a.waitReady(); err != nil {
		return 0, err
	}
	if len(rowIDs) == 0 {
		return 0, nil
	}
	// 先停掉这些主题下正在下载的楼层，再删记录：否则任务还在跑，
	// 写回进度时会找不到行（并且楼层选择器的内存任务也要一起丢掉）
	forumIDs := map[int64]bool{}
	for _, id := range rowIDs {
		var tid int64
		if err := a.Store.DB().QueryRow(`SELECT thread_id FROM thread WHERE id=?`, id).Scan(&tid); err == nil {
			forumIDs[tid] = true
		}
		a.floorJobs.Delete(id)
	}
	if len(forumIDs) > 0 {
		posts, _ := a.Store.ListPosts()
		var stop []int64
		for _, p := range posts {
			if forumIDs[p.ForumThreadID] {
				stop = append(stop, p.ID)
			}
		}
		if len(stop) > 0 {
			a.Pipe.Stop(stop...)
		}
	}
	return a.Store.DeleteThreadsCascade(rowIDs)
}

var reDirSanitize = regexp.MustCompile(`[\\/:*?"<>|]`)

// ThreadDir returns the folder a thread's floors download into (derived
// from the thread title, mirroring the pipeline's naming); falls back to
// the download root when it doesn't exist yet.
func (a *App) ThreadDir(threadRowID int64) (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	root := a.Settings.Get().DownloadDir
	var title string
	if err := a.Store.DB().QueryRow(`SELECT title FROM thread WHERE id=?`, threadRowID).Scan(&title); err != nil {
		return root, nil
	}
	name := pipeline.SanitizeDirName(title) // 与引擎推导目录用同一套清洗规则
	if name == "" {
		return root, nil
	}
	dir := filepath.Join(root, name)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return root, nil
	}
	return dir, nil
}

// RenameThread 重命名 threads（黄色行）条目：更新主题标题，并把该主题
// 全部楼层按新标题归位（分组目录名来自主题标题；下载中的楼层自动跳过，
// 状态离开 downloading 时会再归位）。
// markColors 允许的标记颜色（前端菜单用同一套 key）。空串表示清除标记。
var markColors = map[string]bool{
	"red": true, "orange": true, "yellow": true, "green": true, "blue": true, "purple": true,
}

func normalizeMark(color string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(color))
	if c == "" || c == "none" {
		return "", nil
	}
	if !markColors[c] {
		return "", fmt.Errorf(tr("未知的标记颜色：%s", "unknown mark color: %s"), color)
	}
	return c, nil
}

// MarkPosts 给选中的楼层 / 普通条目打标记颜色（color 为空 = 清除标记）。
// 只是列表上的视觉标记，不影响下载与排序。
func (a *App) MarkPosts(ids []int64, color string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	c, err := normalizeMark(color)
	if err != nil {
		return err
	}
	if err := a.Store.SetPostMark(ids, c); err != nil {
		return err
	}
	for _, id := range ids {
		a.Hub.MarkPostDirty(id) // 让界面刷新这一行
	}
	return nil
}

// MarkThread 给主题（thread 行）打标记颜色（color 为空 = 清除标记）。
func (a *App) MarkThread(rowID int64, color string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	c, err := normalizeMark(color)
	if err != nil {
		return err
	}
	return a.Store.SetThreadMark(rowID, c)
}

// RenameThread 重命名 threads（黄色行）条目并同步磁盘目录，返回结果说明。
func (a *App) RenameThread(threadRowID int64, title string) (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("%s", tr("名称不能为空", "name cannot be empty"))
	}
	var tid int64
	var oldThreadTitle string
	if err := a.Store.DB().QueryRow(`SELECT thread_id,title FROM thread WHERE id=?`, threadRowID).Scan(&tid, &oldThreadTitle); err != nil {
		return "", err
	}
	if _, err := a.Store.DB().Exec(`UPDATE thread SET title=? WHERE id=?`, title, threadRowID); err != nil {
		return "", err
	}
	rows, err := a.Store.DB().Query(`SELECT id FROM post WHERE thread_id=?`, tid)
	if err != nil {
		return "", err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var msgs []string
	for _, id := range ids {
		if p, err := a.Store.GetPost(id); err == nil && p != nil {
			if m := a.Pipe.SyncFolderToTitle(p); m != "" {
				msgs = append(msgs, m)
			}
			a.Hub.MarkPostDirty(id)
		}
	}
	// 楼层都搬走后旧主题目录只剩空壳，留着会让人以为改名失败了
	msgs = a.removeLeftoverThreadDir(oldThreadTitle, msgs)
	if len(msgs) == 0 {
		return tr("已更新标题（没有需要改名的文件夹）", "Title updated (no folder needed renaming)"), nil
	}
	return strings.Join(msgs, "；"), nil
}

// removeLeftoverThreadDir 主题改名后清理已经空掉的旧主题目录（只在它真的
// 空了才删，里面还有东西就留着）。
func (a *App) removeLeftoverThreadDir(oldThreadTitle string, msgs []string) []string {
	name := pipeline.SanitizeDirName(oldThreadTitle)
	if name == "" || len(msgs) == 0 {
		return msgs
	}
	root := a.Settings.Get().DownloadDir
	old := filepath.Join(root, name)
	if st, err := os.Stat(old); err != nil || !st.IsDir() {
		return msgs
	}
	if entries, err := os.ReadDir(old); err != nil || len(entries) > 0 {
		return msgs
	}
	// 有任务在跑时不删：别的楼层可能马上要往里写东西
	if a.Pipe != nil && a.Pipe.RunningCount() > 0 {
		return msgs
	}
	if err := os.Remove(old); err == nil {
		msgs = append(msgs, "已清理空的旧主题目录「"+name+"」")
		a.Store.AddLog("INFO", "主题改名后清理空目录："+name)
	}
	return msgs
}

func (a *App) DeletePosts(ids []int64) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Store.DeletePostsByIDs(ids)
}

// ClearResolveCache 清空选中条目的解析缓存（直链/原文件名）。
// 下载时用的是缓存直链、不走 host 规则，所以图床改版或曾被坏规则解析过
// 的条目，重新下载前需要先清缓存——否则坏直链会一直被重复使用。
func (a *App) ClearResolveCache(ids []int64) (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	n, err := a.Store.ClearImageCache(ids)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("已清理 %d 个条目的解析缓存（%d 张图的直链），重新下载即可按当前规则重新解析", len(ids), n)
	a.Store.AddLog("INFO", msg)
	a.Hub.Log("INFO", msg)
	for _, id := range ids {
		a.Hub.MarkPostDirty(id)
	}
	return msg, nil
}

func (a *App) ClearPosts(which string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Store.DeletePostsByStatus(which)
}

func (a *App) PostDir(postRowID int64) string {
	if a.waitReady() != nil {
		return ""
	}
	p, err := a.Store.GetPost(postRowID)
	if err != nil {
		return ""
	}
	if p.Dir != "" {
		if _, err := os.Stat(p.Dir); err == nil {
			return p.Dir
		}
	}
	// dir 为空或指向已废弃的位置（老版默认 data\galleries）：按当前下载根
	// 重推；重推目录也不存在时上溯到最近存在的祖先（主题目录/下载根）。
	d := a.Pipe.DesiredDir(p)
	if d != "" {
		if _, err := os.Stat(d); err == nil {
			return d
		}
		anc := filepath.Dir(d)
		for {
			if st, e := os.Stat(anc); e == nil && st.IsDir() {
				return anc
			}
			up := filepath.Dir(anc)
			if up == anc {
				break
			}
			anc = up
		}
	}
	return a.Settings.Get().DownloadDir
}

// OpenPath 打开文件管理器,返回实际打开的路径(失败返回空串)。
// 返回值用于前端提示——"无反应"没法排查,必须给用户可见结果。
func (a *App) OpenPath(path string) string {
	if path == "" {
		return ""
	}
	// explorer 对不存在的路径会退回打开“文档”目录，令人困惑：
	// 文件夹还没创建时逐级上溯，打开最近一个真实存在的祖先目录
	// （通常是主题分组目录/下载根目录）
	if _, err := os.Stat(path); err != nil {
		ancestor := filepath.Dir(path)
		for {
			if st, e := os.Stat(ancestor); e == nil && st.IsDir() {
				a.Store.AddLog("INFO", "「"+filepath.Base(path)+"」文件夹尚未创建，已打开："+ancestor)
				path = ancestor
				break
			}
			up := filepath.Dir(ancestor)
			if up == ancestor { // 到盘根还不存在：路径本身坏了
				a.Store.AddLog("WARN", "路径不存在，无法打开："+path)
				return ""
			}
			ancestor = up
		}
	}
	switch goruntime.GOOS {
	case "windows":
		exec.Command("explorer", path).Start()
	case "darwin":
		exec.Command("open", path).Start()
	default:
		exec.Command("xdg-open", path).Start()
	}
	return path
}

func (a *App) OpenBrowser(url string) {
	wruntime.BrowserOpenURL(a.ctx, url)
}

func (a *App) WriteClipboard(text string) {
	wruntime.ClipboardSetText(a.ctx, text)
}

// ---------- settings ----------

func (a *App) LoadSettings() settings.Settings {
	if a.waitReady() != nil {
		return *settings.Defaults()
	}
	return a.Settings.Get()
}

func (a *App) SaveSettings(s settings.Settings) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	// 目录类设置先验证：绝对路径、能创建、能写。以前填错了要等下载时才报错。
	if err := ensureWritableDir("下载目录", s.DownloadDir); err != nil {
		return err
	}
	if err := ensureWritableDir("临时目录", s.TempDir); err != nil {
		return err
	}
	old := a.Settings.Get()
	err := a.Settings.Set(s)
	if err != nil {
		return err
	}
	a.applyPipeSettings()
	a.Forum.SetRate(s.RateLimit)
	if old.ExtBridgeEnabled != s.ExtBridgeEnabled || old.ExtBridgePort != s.ExtBridgePort {
		a.restartExtBridge(s)
	}
	// 论坛域名/镜像变了：换掉客户端里的主机，不需要重启
	if old.ForumDomain != s.ForumDomain || old.Mirrors != s.Mirrors {
		if a.Forum != nil {
			a.Forum.SetDomain(s.ForumDomain)
			a.Forum.SetMirrors(s.Mirrors)
			a.Store.AddLog("INFO", "论坛域名/镜像已更新："+a.Forum.HostLabel())
		}
	}
	// 目录布局或下载根目录变了：还没开始下载的帖子立刻按新设置重新推导目录，
	// 已经开始/已完成的保持原位置（文件已经在那儿了，不折腾）。
	// “按板块分类”也算布局的一部分：开关一变，未开始的条目必须重算目录，
	// 否则库里留着的旧路径会让它们继续落在板块目录外面。
	layoutChanged := old.PerPostFolder != s.PerPostFolder || old.FetchFloorTitle != s.FetchFloorTitle || old.SortByForum != s.SortByForum
	if layoutChanged || old.DownloadDir != s.DownloadDir {
		if n, err := a.Store.ClearPostDirsByStatus("ready"); err == nil && n > 0 {
			a.Store.AddLog("INFO", fmt.Sprintf("下载目录设置已更新，%d 个未开始的帖子将按新结构下载", n))
		}
	}
	if old.DownloadDir != s.DownloadDir {
		a.Store.AddLog("INFO", "下载目录已改为 "+s.DownloadDir+"（已下载完成的帖子保持原位置）")
	}
	return nil
}

// ---------- 关闭到托盘 / 自动关机 ----------

// beforeClose 拦截关窗：还有任务在跑时弹确认框（收进任务栏 / 强制退出 / 取消），
// 返回 true 表示拦下这次关闭。注意它不再“最小化到托盘”——关窗只会被拦或放行。
// 从菜单“退出”走 Quit()，那里同样会先询问一次（下载中不允许静默退出）。
func (a *App) beforeClose(ctx context.Context) bool {
	// 真正退出前把窗口位置/尺寸落盘（resize 防抖保存是兜底，这里是准确保存）
	a.saveWindowBounds(ctx)
	if a.quitting || a.Settings == nil {
		return false
	}
	// 有任务在跑：拦住并弹确认框（收进任务栏 / 强制退出 / 取消）。
	// 不看“最小化到托盘”开关：以前这里先判断开关，开关一旦是关的就直接退出，
	// 下载被没有任何提示地掐断（用户以为程序“没提示就关了”）。
	if !a.hasActiveWork() {
		return false
	}
	// 用户明确关掉“关闭前确认”时才直接退出
	if a.Settings.Get().ConfirmOnClose == false {
		return false
	}
	// 兜底：2 秒内又点了一次 ×，而且上次的确认框根本没被前端接住（没回执）
	// → 说明弹窗没出来，这次就当作强制退出，别让窗口永远关不掉。
	if !a.closeAskAt.IsZero() && !a.closeAskShown && time.Since(a.closeAskAt) < 2*time.Second {
		a.quitting = true
		return false
	}
	a.closeAskAt = time.Now()
	a.closeAskShown = false
	a.logClose("关窗被拦截：还有任务在下载，等待选择")
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "app:closebusy")
	}
	return true
}

// hasActiveWork 是否还有下载任务（正在跑，或库里处于下载中）。
func (a *App) hasActiveWork() bool {
	if a.Pipe != nil && a.Pipe.AnyRunning() {
		return true
	}
	if a.Store != nil {
		var pending int
		if err := a.Store.DB().QueryRow(`SELECT COUNT(*) FROM post WHERE status='downloading'`).Scan(&pending); err == nil && pending > 0 {
			return true
		}
	}
	return false
}

func (a *App) logClose(msg string) {
	if a.Store == nil {
		return
	}
	// 带上开关状态，方便排查“为什么没弹框”
	tray := false
	if a.Settings != nil {
		tray = a.Settings.Get().ConfirmOnClose
	}
	a.Store.AddLog("INFO", fmt.Sprintf("%s（确认框开关=%v）", msg, tray))
}

// Quit 请求退出（菜单 / 托盘）：还有任务在跑就弹确认框，不再直接掐断下载。
func (a *App) Quit() {
	if a.quitting {
		return
	}
	if a.Settings != nil && a.hasActiveWork() && a.Settings.Get().ConfirmOnClose {
		a.logClose("退出请求被拦截：还有任务在下载，等待选择")
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, "app:closebusy")
		}
		return
	}
	a.ForceQuit()
}

// NotifyCloseAskShown 前端确认框真的弹出来时回执一下：这样连续点 × 不会被
// 误判成“弹窗没出来”而直接退出。
func (a *App) NotifyCloseAskShown() {
	a.closeAskShown = true
}

// ForceQuit 真正退出（确认框的“强制退出”走这里，不再询问）。
func (a *App) ForceQuit() {
	a.quitting = true
	if a.ctx != nil {
		wruntime.Quit(a.ctx)
	}
}

// queueBusy 队列里还有没跑完的下载。
//
// 只认“运行表里有任务”或“状态是 downloading”，**不再把 status='ready'
// 算进来**：ready 是“加进列表但没开始下载”的条目，普通用法下一直会有，
// 以前只要列表里有任何 ready 条目，完成事件就永远不会触发——这正是
// “任务完成提示音从来没响过”的根因。同样不把 paused 算作繁忙：那是
// 用户主动停下的，其余任务下完就该提示。
func (a *App) queueBusy() bool {
	if a.Pipe != nil && a.Pipe.AnyRunning() {
		return true
	}
	if a.Store == nil {
		return false
	}
	n := 0
	_ = a.Store.DB().QueryRow(`SELECT COUNT(*) FROM post WHERE status='downloading'`).Scan(&n)
	return n > 0
}

// resetIdleNotify 让下一批任务结束后还能再触发一次“全部完成”。
func (a *App) resetIdleNotify() {
	a.idleMu.Lock()
	a.idleFired = false
	a.idleMu.Unlock()
}

// beepProc 直接调 user32!MessageBeep（x/sys/windows 没有导出这个函数）。
var (
	user32DLL       = windows.NewLazySystemDLL("user32.dll")
	procMessageBeep = user32DLL.NewProc("MessageBeep")
)

// BeepDone 前端放不出声音时（AudioContext 被自动播放策略挡住、没音频设备等）
// 由前端兜底调用：用系统提示音响两下。
func (a *App) BeepDone() {
	go func() {
		for i := 0; i < 2; i++ {
			procMessageBeep.Call(0x00000040) // MB_ICONASTERISK
			time.Sleep(220 * time.Millisecond)
		}
	}()
}

// handleQueueIdle 在整条队列真的空了时发“全部完成”事件，并按设置安排关机。
//
// 两个坑：
//  1. OnPostDone 是在帖子自己的 goroutine 里回调的，这时候它还在 running 表里，
//     立刻判断会永远认为“还有任务在跑”，于是事件永远发不出去（提示音不响）。
//     所以延迟复查，直到队列真的空了为止。
//  2. 一批任务会有多次 OnPostDone 并发触发，用品号只让最后一次负责发事件，
//     避免连着响好几声。
func (a *App) handleQueueIdle() {
	if a.Settings == nil || a.Pipe == nil || a.Store == nil {
		return
	}
	a.idleMu.Lock()
	a.idleSeq++
	seq := a.idleSeq
	a.idleMu.Unlock()

	for i := 0; i < 24; i++ { // 最多等 6 秒
		time.Sleep(250 * time.Millisecond)
		a.idleMu.Lock()
		stale := seq != a.idleSeq
		a.idleMu.Unlock()
		if stale {
			return // 有更晚的调用在等，交给它
		}
		if !a.queueBusy() {
			break
		}
	}

	a.idleMu.Lock()
	if seq != a.idleSeq || a.queueBusy() {
		a.idleMu.Unlock()
		return
	}
	if a.idleFired {
		a.idleMu.Unlock()
		return
	}
	a.idleFired = true
	a.idleMu.Unlock()

	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "app:finished", "")
	}
	if !a.Settings.Get().ShutdownOnDone {
		return
	}
	a.shutdownMu.Lock()
	if a.shutdownArmed {
		a.shutdownMu.Unlock()
		return
	}
	a.shutdownArmed = true
	a.shutdownMu.Unlock()

	// 留 60 秒反悔时间：shutdown /a 可取消
	if err := exec.Command("shutdown", "/s", "/t", "60", "/c", "ViperRipper New: 下载任务已全部完成").Run(); err != nil {
		if a.Store != nil {
			a.Store.AddLog("WARN", "自动关机失败："+err.Error())
		}
		a.shutdownMu.Lock()
		a.shutdownArmed = false
		a.shutdownMu.Unlock()
		return
	}
	msg := "全部任务已完成：60 秒后自动关机（在命令行执行 shutdown /a 可取消）"
	if a.Store != nil {
		a.Store.AddLog("INFO", msg)
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "app:shutdown", msg)
	}
}

// restartExtBridge 按新配置重启（或关闭）本机桥接服务。
func (a *App) restartExtBridge(s settings.Settings) {
	a.extMu.Lock()
	defer a.extMu.Unlock()
	a.extB.stop()
	a.extB = nil
	if !s.ExtBridgeEnabled {
		if a.Store != nil {
			a.Store.AddLog("INFO", "浏览器扩展桥接已关闭")
		}
		return
	}
	a.extB = startExtBridge(a, a.dataDir, s.ExtBridgePort)
}

// ExtBridgeStatus 返回桥接服务的当前状态（供设置页展示）。
func (a *App) ExtBridgeStatus() map[string]any {
	if a.waitReady() != nil {
		return map[string]any{"enabled": false, "running": false, "port": 0, "url": ""}
	}
	s := a.Settings.Get()
	a.extMu.Lock()
	running := a.extB != nil && a.extB.port > 0
	port := 0
	if running {
		port = a.extB.port
	}
	a.extMu.Unlock()
	out := map[string]any{
		"enabled": s.ExtBridgeEnabled,
		"running": running,
		"port":    port,
		"url":     "",
		"note":    "仅监听 127.0.0.1（本机回环），外网无法访问",
	}
	if running {
		out["url"] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return out
}

// SetExtBridge 立即启用/停用桥接并记住配置（不必重启程序）。
func (a *App) SetExtBridge(enabled bool, port int) (map[string]any, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("%s", tr("端口必须在 0-65535 之间（0 表示自动）", "port must be 0–65535 (0 = automatic)"))
	}
	s := a.Settings.Get()
	s.ExtBridgeEnabled = enabled
	s.ExtBridgePort = port
	if err := a.Settings.Set(s); err != nil {
		return nil, err
	}
	a.restartExtBridge(s)
	return a.ExtBridgeStatus(), nil
}

// uiLang 界面语言（"zh" / "en"）。前端启动时通过 SetUILang 同步；
// 窗口内文案由前端 i18n 处理，这里只管后端生成、会显示到界面的文案
// （原生对话框标题、体检报告、错误提示、导入回执等）。
var uiLang = "zh"

// tr 按当前语言挑中文或英文。
func tr(zh, en string) string {
	if uiLang == "en" {
		return en
	}
	return zh
}

// SetUILang 记录界面语言，供后端弹的原生对话框选标题。
func (a *App) SetUILang(lang string) {
	if lang != "en" && lang != "zh" {
		return
	}
	uiLang = lang
	host.SetLang(lang) // 规则体检/校验消息也在后端拼
}

// tr 按当前界面语言挑中文或英文文案。
func (a *App) tr(zh, en string) string { return tr(zh, en) }

func (a *App) PickDownloadDir(title string) (string, error) {
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{Title: title})
}

// ---------- host management (REDESIGN §4.2) ----------

func (a *App) ListHosts() []host.Rule {
	if a.waitReady() != nil {
		return nil
	}
	return a.Hosts.List()
}

func (a *App) HostSave(rule host.Rule) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	// 编辑器保存也先做一次语法自检（选择器/正则写错就别落盘）
	if err := host.ValidateRule(&rule); err != nil {
		return fmt.Errorf(tr("规则自检未通过：%w", "rule self-check failed: %w"), err)
	}
	// 带 id 表示“编辑已有的规则文件”：写回它自己，而不是再生成一条
	// <name>.custom.json。否则改完内置规则会凭空多出一条同名规则，
	// 而且看起来像“刚才取消的勾选又被恢复了”。
	if rule.ID != "" {
		if cur, err := a.Hosts.GetByID(rule.ID); err == nil {
			rule.File = cur.File
			rule.Builtin = cur.Builtin
			if rule.Name == "" {
				rule.Name = cur.Name
			}
		}
	}
	return a.Hosts.Save(&rule)
}

// HostToggle enables/disables one rule file. The argument is the rule ID
// (file name), not the rule name: a custom rule and a builtin rule can share
// a name, and toggling by name would always hit the first one.
func (a *App) HostToggle(ruleID string, enabled bool) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Hosts.SetEnabledByID(ruleID, enabled)
}

// HostToggleMany 批量启停选中的规则（界面里 Ctrl/Shift 多选之后一次改完，
// 全部写盘后只热加载一次，而不是每条扫一遍目录）。
func (a *App) HostToggleMany(ids []string, enabled bool) map[string]any {
	if a.waitReady() != nil {
		return map[string]any{"ok": 0, "failed": len(ids), "error": "core not initialized"}
	}
	ok, bad := a.Hosts.SetEnabledByIDs(ids, enabled)
	if ok > 0 {
		a.Store.AddLog("INFO", fmt.Sprintf("批量%s %d 条规则（失败 %d）", enableWord(enabled), ok, bad))
	}
	return map[string]any{"ok": ok, "failed": bad}
}

// HostDeleteMany 批量删除选中的规则文件（内置规则下次启动会被 Seed 补回来）。
func (a *App) HostDeleteMany(ids []string) map[string]any {
	if a.waitReady() != nil {
		return map[string]any{"ok": 0, "failed": len(ids), "error": "core not initialized"}
	}
	ok, bad := a.Hosts.DeleteByIDs(ids)
	if ok > 0 {
		a.Store.AddLog("WARN", fmt.Sprintf("批量删除 %d 条规则（失败 %d）", ok, bad))
	}
	return map[string]any{"ok": ok, "failed": bad}
}

func enableWord(b bool) string {
	if b {
		return "启用"
	}
	return "停用"
}

func (a *App) HostDelete(ruleID string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Hosts.DeleteByID(ruleID)
}

// HostLint 对所有规则做静态体检（不联网）：语法、变量、步骤顺序、域名。
func (a *App) HostLint() []host.LintIssue {
	if err := a.waitReady(); err != nil {
		return nil
	}
	return a.Hosts.Lint()
}

func (a *App) HostReload() error {
	if err := a.waitReady(); err != nil {
		return err
	}
	return a.Hosts.Reload()
}

// HostsDir 返回当前规则目录。
func (a *App) HostsDir() string {
	if a.waitReady() != nil {
		return ""
	}
	return a.Hosts.HostsDir()
}

// PickHostsDir 弹系统文件夹选择框，返回用户选的目录（未选则空）。
func (a *App) PickHostsDir() (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:            a.tr("选择 host 规则存放目录", "Choose the rules folder"),
		DefaultDirectory: a.Hosts.HostsDir(),
	})
}

// SetHostsDir 切换规则目录：可选把现有 *.json 复制过去，然后把选择写进设置
// （下次启动沿用）。传空字符串表示回到默认的 <data>/hosts。
func (a *App) SetHostsDir(dir string, migrate bool) (map[string]any, error) {
	if err := a.waitReady(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(a.dataDir, "hosts")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf(tr("目录不可创建：%w", "cannot create the folder: %w"), err)
	}
	// 写权限自检（只读目录/网络盘权限问题时尽早报错）
	probe, err := os.CreateTemp(abs, ".vrwrite-*")
	if err != nil {
		return nil, fmt.Errorf(tr("目录不可写：%w", "the folder is not writable: %w"), err)
	}
	probeName := probe.Name()
	probe.Close()
	os.Remove(probeName)

	old := a.Hosts.HostsDir()
	copied, skipped := 0, 0
	if migrate && abs != old {
		entries, err := os.ReadDir(old)
		if err == nil {
			for _, en := range entries {
				if en.IsDir() || !strings.HasSuffix(en.Name(), ".json") {
					continue
				}
				dst := filepath.Join(abs, en.Name())
				if _, err := os.Stat(dst); err == nil {
					skipped++ // 目标已有同名文件，绝不覆盖用户数据
					continue
				}
				raw, err := os.ReadFile(filepath.Join(old, en.Name()))
				if err != nil {
					continue
				}
				if err := os.WriteFile(dst, raw, 0o644); err != nil {
					continue
				}
				copied++
			}
		}
	}

	if err := a.Hosts.SetHostsDir(abs); err != nil {
		return nil, err
	}
	// 新目录里补齐内置规则（已存在的不覆盖），再从那里重新加载
	if err := a.Hosts.Seed(); err != nil {
		return nil, err
	}

	s := a.Settings.Get()
	if abs == filepath.Join(a.dataDir, "hosts") {
		s.HostsDir = "" // 默认目录不写死，保持便携性
	} else {
		s.HostsDir = abs
	}
	if err := a.Settings.Set(s); err != nil {
		return nil, err
	}
	a.Store.AddLog("INFO", fmt.Sprintf("规则目录已切换到 %s（复制 %d，跳过 %d）", abs, copied, skipped))
	return map[string]any{
		"dir":     abs,
		"copied":  copied,
		"skipped": skipped,
		"rules":   len(a.Hosts.List()),
	}, nil
}

// HostTestByName runs a rule against a real image-page URL (即写即测).
// thumbURL 可选：靠 {thumb_url} 推导直链的规则（imx 这类）必须把论坛里的
// 缩略图地址填进来才能测——它们的直链是从缩略图路径改写出来的，页面链接里
// 没有那些路径段。普通页面解析规则留空即可。
func (a *App) HostTestByName(ruleName, pageURL, thumbURL string) map[string]any {
	if a.waitReady() != nil {
		return map[string]any{"ok": false, "error": "core not initialized"}
	}
	thumbURL = strings.TrimSpace(thumbURL)
	// 规则要 thumb 却没给 thumb：不是规则坏，是测试输入缺一半，说清楚补哪。
	if rule, rerr := a.Hosts.Get(ruleName); rerr == nil && host.RuleNeedsThumb(rule) && thumbURL == "" {
		return map[string]any{
			"ok":    false,
			"error": "该规则靠 {thumb_url}（论坛缩略图）推导直链，请把帖子里的缩略图地址填到第二个输入框再测试；留空时只能直接下载一张看真实结果",
		}
	}
	res, err := a.Hosts.ResolveWith(ruleName, pageURL, thumbURL)
	out := map[string]any{"ok": err == nil}
	if err != nil {
		out["error"] = err.Error()
		if res != nil {
			out["trace"] = res.Trace
		}
		return out
	}
	out["imageUrl"] = res.ImageURL
	out["filename"] = res.Filename
	out["trace"] = res.Trace
	return out
}

// ---------- import / export (用户心血必须可导出) ----------

func (a *App) ExportData() (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	data, err := a.Store.ExportAll()
	if err != nil {
		return "", err
	}
	// 导入历史（原始链接记录）一并带出，换机/重装后可完整恢复
	if b, err := os.ReadFile(filepath.Join(a.dataDir, importLogFile)); err == nil && len(b) > 0 {
		data["importLog"] = json.RawMessage(b)
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	path, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{
		Title:           a.tr("导出全部数据", "Export all data"),
		DefaultFilename: "viperripper-export.json",
		Filters: []wruntime.FileFilter{
			{DisplayName: "JSON (*.json)", Pattern: "*.json"},
		},
	})
	if path == "" {
		return "", nil
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	a.Store.AddLog("INFO", "已导出数据到 "+path)
	return path, nil
}

func (a *App) ImportData() (string, error) {
	if err := a.waitReady(); err != nil {
		return "", err
	}
	paths, err := wruntime.OpenMultipleFilesDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: a.tr("导入数据 (JSON)", "Import data (JSON)"),
		Filters: []wruntime.FileFilter{
			{DisplayName: "JSON (*.json)", Pattern: "*.json"},
		},
	})
	if err != nil || len(paths) == 0 {
		return "", nil
	}
	total := 0
	totalPosts := 0
	var lastImportLog json.RawMessage
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(raw, &data) == nil {
			if t, ok := data["threads"]; ok {
				n, _ := a.Store.ImportThreadRows(t)
				total += n
			}
			if ps, ok := data["posts"]; ok {
				var imgs json.RawMessage
				if im, ok := data["images"]; ok {
					imgs = im
				}
				n, err := a.Store.ImportPosts(ps, imgs)
				if err != nil {
					a.Store.AddLog("ERROR", "导入下载条目失败（"+filepath.Base(p)+"）："+err.Error())
				}
				totalPosts += n
			}
			if il, ok := data["importLog"]; ok {
				lastImportLog = il
			}
		} else {
			n, _ := a.Store.ImportThreadRows(raw)
			total += n
		}
	}
	// 导入历史写回数据目录（多个文件时以后导入的为准），恢复"导入历史"页
	if lastImportLog != nil {
		if err := os.WriteFile(filepath.Join(a.dataDir, importLogFile), lastImportLog, 0o644); err != nil {
			a.Store.AddLog("ERROR", "导入历史写回失败："+err.Error())
		}
	}
	msg := fmt.Sprintf("已导入 %d 个主题、%d 个下载条目", total, totalPosts)
	a.Store.AddLog("INFO", msg)
	a.Hub.Log("INFO", msg)
	return msg, nil
}

// ---------- misc ----------

func (a *App) Version() string { return "0.1.0" }

// LogFromFrontend records JS-side errors into the Log tab so nothing fails silently.
func (a *App) LogFromFrontend(level, msg string) {
	if a.waitReady() == nil {
		a.Store.AddLog(level, "前端: "+msg)
	}
	a.Hub.Log(level, "前端: "+msg)
}

func (a *App) DataDir() string { return a.dataDir }

// LoginNow performs an explicit form login (设置连接页 fallback).
func (a *App) LoginNow(username, password string) error {
	if err := a.waitReady(); err != nil {
		return err
	}
	s := a.Settings.Get()
	if strings.TrimSpace(username) == "" {
		username = s.Username
		password = s.Password
	}
	err := a.Forum.Login(username, password)
	if err == nil {
		if u := a.Forum.CurrentUser(); u != "" {
			a.Store.AddLog("INFO", "登录成功（"+u+"）")
		} else {
			a.Store.AddLog("INFO", "登录成功")
		}
		// 用户名跟着登录一起记下来：否则用户改了输入框直接点"立即登录"，
		// 界面上登进去了，设置里还是旧名字。
		if cur := a.Settings.Get(); cur.Username != username {
			cur.Username = username
			_ = a.Settings.Set(cur)
		}
		a.persistAuthCookies()
	} else {
		a.Hub.Log("ERROR", "登录失败："+err.Error())
	}
	a.emitLoginChanged()
	return err
}

// OpenWebLogin starts a child process of this exe hosting the forum login
// website (its own WebView2 profile). The child writes harvested cookies to a
// temp result file; the main app polls it, adopts the session and persists
// it. Running the window out-of-process means any WebView2 failure there can
// never crash the main app.
func (a *App) OpenWebLogin() string {
	if err := a.waitReady(); err != nil {
		return err.Error()
	}
	if !a.webLoginMu.TryLock() {
		return tr("网页登录窗口已打开，请先完成或关闭它", "The web login window is already open — finish or close it first")
	}
	domain := a.Settings.Get().ForumDomain
	if domain == "" {
		domain = "vipergirls.to"
	}
	profileDir := filepath.Join(a.dataDir, "webview-login")
	resultFile := filepath.Join(os.TempDir(), fmt.Sprintf("vr-weblogin-%d.json", time.Now().UnixNano()))
	os.Remove(resultFile)
	exe, err := os.Executable()
	if err != nil {
		a.webLoginMu.Unlock()
		return err.Error()
	}
	proxyAddr := strings.TrimSpace(a.Settings.Get().Proxy)
	cmd := exec.Command(exe, "--weblogin", profileDir, domain,
		"https://"+domain+"/login.php?do=login", proxyAddr, resultFile)
	if err := cmd.Start(); err != nil {
		a.webLoginMu.Unlock()
		return tr("启动登录窗口失败：", "Failed to open the login window: ") + err.Error()
	}
	a.webLoginCmd = cmd
	a.logf("INFO", "已打开网页登录窗口（%s）", domain)
	go func() {
		defer func() {
			os.Remove(resultFile)
			a.webLoginCmd = nil
			a.webLoginMu.Unlock()
			a.emitLoginChanged()
		}()
		seen := ""
		logFile := resultFile + ".log"
		logSeen := 0
		pollLogs := func() {
			raw, err := os.ReadFile(logFile)
			if err != nil || len(raw) <= logSeen {
				return
			}
			for _, line := range strings.Split(string(raw[logSeen:]), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					a.Store.AddLog("INFO", "登录窗口 "+line)
				}
			}
			logSeen = len(raw)
		}
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		exited := make(chan struct{})
		go func() { cmd.Wait(); close(exited) }()
		for {
			select {
			case <-exited:
				pollLogs()
				return
			case <-tick.C:
				pollLogs()
				raw, err := os.ReadFile(resultFile)
				if err != nil || len(raw) == 0 || string(raw) == seen {
					continue
				}
				seen = string(raw)
				payload, ok := loginwin.ParsePayload(raw)
				if !ok || len(payload.Cookies) == 0 {
					continue
				}
				cookies := payload.Cookies
				// Every batch is verified against the forum server: the login
				// session may live entirely in an HttpOnly session cookie, so
				// we cannot decide from cookie names whether it is a login.
				if err := a.Forum.ImportCookies(cookies); err != nil {
					a.emitLoginChanged()
					continue
				}
				if u := a.Forum.CurrentUser(); u != "" {
					a.Store.AddLog("INFO", "网页登录成功（"+u+"）")
				} else {
					// 登录本身成功了，只是论坛主题的导航栏没匹配到用户名
					a.Store.AddLog("INFO", "网页登录成功（未能识别用户名，不影响下载/点赞）")
				}
				a.persistAuthCookies()
				a.emitLoginChanged()
			}
		}
	}()
	return ""
}

// ---------- Cloudflare 人机验证 ----------

// onChallengeNeed 在某个图床返回“你是人吗”拦截页时调用（cfguard 已按域名
// 做 20 秒防抖，一批图不会刷屏）。
func (a *App) onChallengeNeed(ch *cfguard.Challenge) {
	if ch == nil {
		return
	}
	a.logf("WARN", "%s 需要人机验证，点提示条可打开验证窗口", ch.Host)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "cf:need", map[string]any{
			"host": ch.Host,
			"url":  ch.URL,
		})
	}
}

// onChallengePass 用户过完验证、凭证落到 cfguard 后：持久化 + 通知界面 +
// 自动把该站之前失败的图重新排队下载。
func (a *App) onChallengePass(host string) {
	if a.Store != nil {
		if raw := cfguard.Default.Snapshot(); len(raw) > 0 {
			a.Store.SetKV("cfsessions", string(raw))
		}
	}
	a.logf("INFO", "已取得 %s 的验证凭证，准备重新下载之前失败的图片", host)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "cf:passed", map[string]any{"host": host})
	}
	a.retryHostFailures(host)
}

// retryHostFailures 把该域名下失败的图片退回待下载，并重新启动它们所在的帖子。
func (a *App) retryHostFailures(host string) {
	if a.Store == nil || a.Pipe == nil || host == "" {
		return
	}
	like := "%" + strings.ToLower(host) + "%"
	rows, err := a.Store.DB().Query(
		`SELECT DISTINCT post_row_id FROM image
		 WHERE state='error' AND (lower(page_url) LIKE ? OR lower(direct_url) LIKE ?)`, like, like)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	if _, err := a.Store.DB().Exec(
		`UPDATE image SET state='pending', error_msg='' 
		 WHERE state='error' AND (lower(page_url) LIKE ? OR lower(direct_url) LIKE ?)`, like, like); err != nil {
		a.logf("WARN", "重置失败图片时出错：%v", err)
		return
	}
	a.logf("INFO", "重新下载 %d 个帖子中被人机验证拦下的图片", len(ids))
	a.StartPosts(ids)
}

// OpenChallengeWindow 打开人机验证小窗口：用户在窗口里点完“确认是人类”后，
// 子进程把该域名的 cookie 与窗口真实 UA 写回，主程序收下并自动续下失败的图。
func (a *App) OpenChallengeWindow(host, targetURL string) string {
	if err := a.waitReady(); err != nil {
		return err.Error()
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return tr("缺少域名", "missing domain")
	}
	if strings.TrimSpace(targetURL) == "" {
		targetURL = "https://" + host + "/"
	}
	if !a.webLoginMu.TryLock() {
		return tr("网页窗口已打开，请先完成或关闭它", "The web window is already open — finish or close it first")
	}
	profileDir := filepath.Join(a.dataDir, "webview-login")
	resultFile := filepath.Join(os.TempDir(), fmt.Sprintf("vr-webverify-%d.json", time.Now().UnixNano()))
	os.Remove(resultFile)
	exe, err := os.Executable()
	if err != nil {
		a.webLoginMu.Unlock()
		return err.Error()
	}
	proxyAddr := strings.TrimSpace(a.Settings.Get().Proxy)
	cmd := exec.Command(exe, "--webverify", profileDir, host, targetURL, proxyAddr, resultFile)
	if err := cmd.Start(); err != nil {
		a.webLoginMu.Unlock()
		return tr("启动验证窗口失败：", "Failed to open the verify window: ") + err.Error()
	}
	a.logf("INFO", "已打开 %s 的人机验证窗口", host)
	go func() {
		defer func() {
			os.Remove(resultFile)
			a.webLoginMu.Unlock()
		}()
		seen := ""
		logFile := resultFile + ".log"
		logSeen := 0
		pollLogs := func() {
			raw, err := os.ReadFile(logFile)
			if err != nil || len(raw) <= logSeen {
				return
			}
			for _, line := range strings.Split(string(raw[logSeen:]), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "诊断") {
					a.Store.AddLog("INFO", "验证窗口 "+line)
				}
			}
			logSeen = len(raw)
		}
		exited := make(chan struct{})
		go func() { cmd.Wait(); close(exited) }()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-exited:
				pollLogs()
				os.Remove(logFile)
				return
			case <-tick.C:
				pollLogs()
				raw, err := os.ReadFile(resultFile)
				if err != nil || len(raw) == 0 || string(raw) == seen {
					continue
				}
				payload, ok := loginwin.ParsePayload(raw)
				if !ok || len(payload.Cookies) == 0 {
					continue
				}
				seen = string(raw)
				a.logf("INFO", "收到 %s 的验证凭证（%d 个 cookie）", host, len(payload.Cookies))
				// Set 会触发 onChallengePass：持久化凭证并自动重下失败的图
				cfguard.Default.Set(host, payload.Cookies, payload.UserAgent)
			}
		}
	}()
	return ""
}

// LogoutNow drops the forum session everywhere: the downloader's cookies,
// the persisted login, and (if the web-login window is open) the browser
// profile cookies via a signal file the child process polls.
func (a *App) LogoutNow() {
	a.Forum.Logout()
	a.Store.SetKV("authcookies", "")
	a.Store.AddLog("INFO", "已注销论坛登录")
	if a.webLoginCmd != nil {
		signal := filepath.Join(os.TempDir(), fmt.Sprintf("vr-weblogin-%d.logout", time.Now().UnixNano()))
		os.WriteFile(signal, []byte("logout"), 0o644)
		a.logoutSignal = signal
	}
	a.emitLoginChanged()
}

// persistAuthCookies saves the forum session cookies so the login survives
// restarts (stored as JSON in the settings KV table).
func (a *App) persistAuthCookies() {
	cookies := a.Forum.SessionCookies()
	if len(cookies) == 0 {
		return
	}
	raw, err := json.Marshal(cookies)
	if err == nil {
		a.Store.SetKV("authcookies", string(raw))
	}
}

func (a *App) emitLoginChanged() {
	wruntime.EventsEmit(a.ctx, "login:changed", map[string]interface{}{
		"loggedIn": a.Forum.LoggedIn(),
		"username": a.Forum.CurrentUser(),
	})
}

// LoginState reports the current forum session for the settings page.
func (a *App) LoginState() map[string]interface{} {
	if a.waitReady() != nil {
		return map[string]interface{}{"loggedIn": false, "username": ""}
	}
	return map[string]interface{}{
		"loggedIn": a.Forum.LoggedIn(),
		"username": a.Forum.CurrentUser(),
	}
}

// ---------- 自动感谢去重 ----------

// alreadyThanked reports whether this post was already thanked (persisted so a
// restart doesn't re-thank everything that gets re-run).
func (a *App) alreadyThanked(forumPostID int64) bool {
	a.thankMu.Lock()
	defer a.thankMu.Unlock()
	var list []int64
	if raw := a.Store.GetKV("thankedposts"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	for _, v := range list {
		if v == forumPostID {
			return true
		}
	}
	return false
}

func (a *App) markThanked(forumPostID int64) {
	a.thankMu.Lock()
	defer a.thankMu.Unlock()
	var list []int64
	if raw := a.Store.GetKV("thankedposts"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	for _, v := range list {
		if v == forumPostID {
			return
		}
	}
	list = append(list, forumPostID)
	if len(list) > 2000 { // 只保留最近 2000 条，避免 KV 无限膨胀
		list = list[len(list)-2000:]
	}
	if raw, err := json.Marshal(list); err == nil {
		a.Store.SetKV("thankedposts", string(raw))
	}
}

// ensureWritableDir 校验目录设置是否可用（空 = 用默认，跳过校验）。
func ensureWritableDir(kind, dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf(tr("%s必须是绝对路径：%s", "%s must be an absolute path: %s"), kind, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf(tr("无法创建%s %s：%v", "cannot create %s %s: %v"), kind, dir, err)
	}
	f, err := os.CreateTemp(dir, ".vr-write-test-*")
	if err != nil {
		return fmt.Errorf(tr("%s不可写（%s）：%v", "%s is not writable (%s): %v"), kind, dir, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
