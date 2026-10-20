package pipeline

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"viperipper/internal/cfguard"
	"viperipper/internal/host"
	"viperipper/internal/proxy"
	"viperipper/internal/store"
)

// runCtx bundles a running post's cancellable download context.
type runCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Engine implements REDESIGN §4.3: two-level semaphore (global posts ×
// per-host), exponential backoff retries, atomic temp-file rename, and
// resume (skip done images / existing files of matching size).

type Engine struct {
	Store   *store.Store
	Hosts   *host.Registry
	BaseDir string // download root
	TempDir string

	PostsConcurrent    int
	HostConcurrent     int
	MaxRetries         int
	TimeoutSec         int
	FetchOrigName      bool
	ExistingFilePolicy string // skip | overwrite | rename
	// 目录布局（设置页“每帖一个文件夹 / 全部平铺”“自动获取楼层标题”）
	PerPostFolder   bool
	FetchFloorTitle bool
	SortByForum     bool // 下载目录最外层按论坛板块分类

	// ResolveOnce pre-resolves all image pages of a post into direct URLs
	// with N-way concurrency (收藏后并发预解析, REDESIGN §4.2).
	ResolveWorkers int

	mu       sync.Mutex
	running  map[int64]bool // post row id -> running
	stopFlag map[int64]bool // post row id -> stop requested
	perHost  map[string]*semaphore.Weighted
	global   *semaphore.Weighted
	ctxs     map[int64]*runCtx
	wg       sync.WaitGroup

	// OnImageEvent is called on every image state change (for Hub).
	OnImageEvent func(postRowID, imageID int64)
	// OnPostStatus is called when a post changes status.
	OnPostStatus func(postRowID int64, status string)
	// OnBeforeStart 在开始下载某个帖子前调用（该帖的下载 goroutine 内）。
	// 返回 true 表示图片行在上层被重建过（如检测到文件数量异常），调用方
	// 必须重新 ListImages 拿新数据。
	OnBeforeStart func(postRowID int64) bool
	// ResolveForum 在“按板块分类开启、但这条记录不知道板块”时调用，
	// 由上层去论坛查一次并返回板块名（同步、限速）；返回空串表示查不到。
	ResolveForum func(post *store.Post) string
	// OnPostDone is called once when a post fully completes (auto-thank hook).
	OnPostDone func(postRowID, forumPostID int64)
	// OnLog receives log lines.
	OnLog func(level, msg string)
	// OnImageProgress 报告单张图的下载字节（约每 150ms 一次，total 可能为 -1）
	OnImageProgress func(imgID, done, total int64)

	// proxyFn is snapshotted at Start; in-flight posts keep their proxy
	// config even if the user flips the proxy toggle mid-download.
	proxyFn atomic.Value // func(*http.Request) (*url.URL, error)
}

func New(st *store.Store, hr *host.Registry) *Engine {
	return &Engine{
		Store: st, Hosts: hr,
		PostsConcurrent: 2, HostConcurrent: 2, MaxRetries: 3, TimeoutSec: 10,
		ResolveWorkers: 20,
		running:        map[int64]bool{},
		stopFlag:       map[int64]bool{},
		ctxs:           map[int64]*runCtx{},
		perHost:        map[string]*semaphore.Weighted{},
		global:         semaphore.NewWeighted(2),
	}
}

func (e *Engine) log(level, format string, a ...any) {
	if e.OnLog != nil {
		e.OnLog(level, fmt.Sprintf(format, a...))
	}
}

func (e *Engine) setHostSem(name string) *semaphore.Weighted {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.perHost[name]
	if !ok {
		s = semaphore.NewWeighted(int64(e.HostConcurrent))
		e.perHost[name] = s
	}
	return s
}

// hostKeyOf maps a page/image URL to the per-host throttle bucket: the
// rule name when a rule matches, otherwise the bare host.
func (e *Engine) hostKeyOf(pageURL string) string {
	if name := e.Hosts.RuleNameOf(pageURL); name != "" {
		return name
	}
	if i := strings.Index(pageURL, "://"); i >= 0 {
		u := pageURL[i+3:]
		if j := strings.IndexAny(u, "/?#"); j >= 0 {
			u = u[:j]
		}
		return u
	}
	return pageURL
}

func (e *Engine) ApplySettings(posts, hostConc, retries, timeoutSec int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.PostsConcurrent = posts
	e.HostConcurrent = hostConc
	e.MaxRetries = retries
	e.TimeoutSec = timeoutSec
	e.global = semaphore.NewWeighted(int64(posts))
	// reset per-host semaphores with the new size
	e.perHost = map[string]*semaphore.Weighted{}
}

func (e *Engine) IsRunning(postRowID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[postRowID]
}

func (e *Engine) AnyRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.running) > 0
}

func (e *Engine) RunningCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.running)
}

// Stop requests a stop for the given posts (or all when ids empty).
func (e *Engine) Stop(ids ...int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stopOne := func(id int64) {
		e.stopFlag[id] = true
		if rc, ok := e.ctxs[id]; ok {
			rc.cancel()
		}
	}
	if len(ids) == 0 {
		for id := range e.running {
			stopOne(id)
		}
		return
	}
	for _, id := range ids {
		if e.running[id] {
			stopOne(id)
		}
	}
}

// Start launches downloads for the given post rows.
func (e *Engine) Start(ids ...int64) {
	// freeze the proxy config for everything launched from now on
	e.proxyFn.Store(proxy.Snapshot())
	for _, id := range ids {
		e.mu.Lock()
		if e.running[id] {
			e.mu.Unlock()
			continue
		}
		e.running[id] = true
		delete(e.stopFlag, id)
		rc := &runCtx{ctx: context.Background()}
		rc.ctx, rc.cancel = context.WithCancel(rc.ctx)
		e.ctxs[id] = rc
		e.mu.Unlock()
		e.wg.Add(1)
		go func(rowID int64, pctx context.Context) {
			defer e.wg.Done()
			defer func() {
				e.mu.Lock()
				delete(e.running, rowID)
				delete(e.stopFlag, rowID)
				delete(e.ctxs, rowID)
				e.mu.Unlock()
			}()
			e.runPost(pctx, rowID)
		}(id, rc.ctx)
	}
}

func (e *Engine) Wait() { e.wg.Wait() }

// proxyFor returns the proxy func snapshotted at Start time, falling back to
// the live global config (tests, or fetches outside a Start batch).
func (e *Engine) proxyFor() func(*http.Request) (*url.URL, error) {
	if f, ok := e.proxyFn.Load().(func(*http.Request) (*url.URL, error)); ok {
		return f
	}
	return proxy.Func()
}

// ResolvePost fills direct URLs (and original names) for a post's pending
// images without starting any downloads — the filenames it produces are
// what disk recovery matches against.
func (e *Engine) ResolvePost(rowID int64) {
	imgs, err := e.Store.ListImages(rowID)
	if err != nil {
		return
	}
	e.preResolve(context.Background(), imgs)
}

// RecoverFromDisk matches files already present in the post's folder back to
// its image rows (DB state lost to a remove/re-add, files kept on disk).
// Images whose expected filename can be computed offline (original name or
// direct URL on record) are marked done; the rest need a resolve pass and
// are counted as unmatched — starting the download will pick them up via
// the disk-level resume check.
func (e *Engine) RecoverFromDisk(rowID int64) (recovered, unmatched int, err error) {
	post, err := e.Store.GetPost(rowID)
	if err != nil {
		return 0, 0, err
	}
	dir := e.EnsurePostDir(post)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, nil // no folder on disk yet — nothing to recover
	}
	imgs, err := e.Store.ListImages(rowID)
	if err != nil {
		return 0, 0, err
	}
	// index valid image files on disk once (name -> full path/size)
	type dfile struct {
		path string
		size int64
	}
	disk := map[string]dfile{}
	for _, en := range dirEntries {
		if en.IsDir() || strings.Contains(en.Name(), ".vripper") {
			continue
		}
		info, err := en.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		p := filepath.Join(dir, en.Name())
		if validImageFile(p) {
			disk[en.Name()] = dfile{p, info.Size()}
		}
	}
	claimed := map[string]bool{}
	for i := range imgs {
		im := imgs[i]
		if im.State == "done" {
			continue
		}
		// candidate filenames, mirroring downloadImage's naming
		names := map[string]bool{}
		if im.OriginalName != "" {
			n := sanitizeFileName(im.OriginalName)
			if reImgExt.MatchString(n) {
				names[n] = true
			} else {
				names[n+extOf(im.DirectURL)] = true
				names[n+".jpg"] = true
			}
		}
		if im.DirectURL != "" {
			names[fmt.Sprintf("%03d%s", im.Seq, extOf(im.DirectURL))] = true
		}
		hit := ""
		for n := range names {
			if df, ok := disk[n]; ok && !claimed[n] {
				hit = df.path
				break
			}
		}
		if hit == "" {
			if len(names) == 0 {
				unmatched++
			}
			continue
		}
		e.Store.QueueImageState(im.ID, "done", disk[filepath.Base(hit)].size, hit, "", 0)
		claimed[filepath.Base(hit)] = true
		if e.OnImageEvent != nil {
			e.OnImageEvent(rowID, im.ID)
		}
		recovered++
	}
	e.Store.Flush() // caller re-reads immediately; don't leave states buffered
	return recovered, unmatched, nil
}

// ctxFor returns the cancellable context of a running post (Background when
// it is not running, e.g. in tests).
func (e *Engine) ctxFor(rowID int64) context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rc, ok := e.ctxs[rowID]; ok {
		return rc.ctx
	}
	return context.Background()
}

func (e *Engine) runPost(ctx context.Context, rowID int64) {
	if err := e.global.Acquire(ctx, 1); err != nil {
		return
	}
	defer e.global.Release(1)

	// stopped while queued for a global slot
	e.mu.Lock()
	sf := e.stopFlag[rowID]
	e.mu.Unlock()
	if sf {
		return
	}

	post, err := e.Store.GetPost(rowID)
	if err != nil {
		e.log("ERROR", "post #%d load failed: %v", rowID, err)
		return
	}
	// 数量核对规则：文件夹里实际图片数与图片总数不符（历史覆盖 bug 只留下
	// 一个文件、目录被清空等）时，上层重建图片行，之后重新加载。
	if e.OnBeforeStart != nil && e.OnBeforeStart(rowID) {
		post, err = e.Store.GetPost(rowID)
		if err != nil {
			e.log("ERROR", "post #%d reload failed: %v", rowID, err)
			return
		}
	}
	imgs, err := e.Store.ListImages(rowID)
	if err != nil {
		e.log("ERROR", "post #%d images load failed: %v", rowID, err)
		return
	}
	// 老数据可能没记板块（早期单帖链接或加入时没抓到论坛名）。下载前补一次，
	// 结果落库，之后的目录推导就走正常路径；不发请求的话这个条目会永远
	// 落在板块目录外面。
	if e.SortByForum && strings.TrimSpace(post.Forum) == "" && e.ResolveForum != nil {
		if f := strings.TrimSpace(e.ResolveForum(post)); f != "" {
			post.Forum = f
			_ = e.Store.SetPostForum(post.ID, f)
			e.log("INFO", "补记板块：post #%d → %s", post.ForumPostID, f)
		}
	}
	// 数据库里"已完成"不等于磁盘上还在。用户手动删过图集目录、或清理过
	// 单个文件之后再点下载，以前会整批跳过，缺口永远补不回来；这里按磁盘
	// 实际情况把缺失的 done 记录退回 pending（overwrite 模式则全部重下）。
	imgs = e.recheckDone(rowID, imgs)
	e.Store.SetPostStatus(rowID, "downloading")
	if e.OnPostStatus != nil {
		e.OnPostStatus(rowID, "downloading")
	}
	e.log("INFO", "开始下载 post #%d（%s，共 %d 图）", post.ForumPostID, post.Title, len(imgs))

	// pre-resolve: fill direct URLs for pending images concurrently.
	// 抓取原文件名开启时跳过全量预解析——每张图的解析（页面 GET+POST）很慢，
	// 全部解析完才下载会让第一张图迟迟不落盘；改为在 downloadImage 内逐张
	// 解析（流水线）：解析完一张就下载一张，与原版 vripper 的行为一致。
	if !e.FetchOrigName {
		e.preResolve(ctx, imgs)
	} else {
		e.log("INFO", "已开启抓取原文件名：边解析边下载")
	}

	dir := e.EnsurePostDir(post)
	if post.Dir == "" {
		// 记下来，界面/续传/磁盘统计都按这个目录走
		post.Dir = dir
		_ = e.Store.SetPostDir(post.ID, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.log("ERROR", "创建目录失败 %s: %v", dir, err)
		e.Store.SetPostStatus(rowID, "error")
		if e.OnPostStatus != nil {
			e.OnPostStatus(rowID, "error")
		}
		return
	}

	stopped := false
	var wg sync.WaitGroup
	lim := make(chan struct{}, e.maxResolveWorkers())
	for i := range imgs {
		// stop check
		e.mu.Lock()
		sf := e.stopFlag[rowID]
		e.mu.Unlock()
		if sf {
			stopped = true
			break
		}
		if imgs[i].State == "done" {
			continue
		}
		img := imgs[i]
		wg.Add(1)
		select {
		case lim <- struct{}{}:
		case <-ctx.Done():
			e.mu.Lock()
			stopped = e.stopFlag[rowID]
			e.mu.Unlock()
			wg.Done()
			goto spawnDone
		}
		go func() {
			defer wg.Done()
			defer func() { <-lim }()
			e.downloadImage(ctx, rowID, post, dir, img)
		}()
	}
spawnDone:
	wg.Wait()

	// final state — flush the buffered writer first so the tally below
	// doesn't read stale pending rows and misreport the post as paused
	e.Store.Flush()
	final, _ := e.Store.ListImages(rowID)
	done, failed := 0, 0
	for _, im := range final {
		switch im.State {
		case "done":
			done++
		case "error":
			failed++
		}
	}
	switch {
	case stopped:
		e.Store.SetPostStatus(rowID, "paused")
		if e.OnPostStatus != nil {
			e.OnPostStatus(rowID, "paused")
		}
		e.log("INFO", "post #%d 已暂停（%d/%d/%d）", post.ForumPostID, done, failed, len(final))
	case failed > 0 && done+failed == len(final):
		e.Store.SetPostStatus(rowID, "error")
		if e.OnPostStatus != nil {
			e.OnPostStatus(rowID, "error")
		}
		e.log("WARN", "post #%d 完成但有 %d 个失效链接（%d/%d/%d）", post.ForumPostID, failed, done, failed, len(final))
	case done == len(final):
		e.Store.SetPostStatus(rowID, "done")
		if e.OnPostStatus != nil {
			e.OnPostStatus(rowID, "done")
		}
		if e.OnPostDone != nil {
			e.OnPostDone(rowID, post.ForumPostID)
		}
		e.log("INFO", "post #%d 下载完成（%d 图）", post.ForumPostID, len(final))
	default:
		e.Store.SetPostStatus(rowID, "paused")
		if e.OnPostStatus != nil {
			e.OnPostStatus(rowID, "paused")
		}
	}
}

// recheckDone downgrades "done" rows whose file is no longer on disk (or is
// no longer a valid image) back to "pending", so a re-start refills exactly
// the gaps instead of skipping the whole post. ExistingFilePolicy == "overwrite"
// means the user wants a full re-download, so every done row is reset.
// Returns the (in-memory) refreshed image slice for the caller to iterate.
func (e *Engine) recheckDone(rowID int64, imgs []store.Image) []store.Image {
	reset := 0
	for i := range imgs {
		im := &imgs[i]
		if im.State != "done" {
			continue
		}
		missing := false
		switch {
		case e.ExistingFilePolicy == "overwrite":
			missing = true
		case im.FilePath == "":
			missing = true
		default:
			if st, err := os.Stat(im.FilePath); err != nil || st.Size() == 0 {
				missing = true
			} else if !validImageFile(im.FilePath) || isPlaceholderFile(im.FilePath, im.FilePath) {
				// 占位图/损坏文件不能继续算“已完成”
				missing = true
			}
		}
		if !missing {
			continue
		}
		e.Store.QueueImageState(im.ID, "pending", 0, "", "", 0)
		im.State = "pending"
		im.FilePath = ""
		im.FileSize = 0
		reset++
	}
	if reset > 0 {
		e.Store.Flush() // so downloadImage's re-read sees "pending"
		e.log("INFO", "post #%d 有 %d 张图片在磁盘上已不存在，重新排队下载", rowID, reset)
	}
	return imgs
}

func (e *Engine) maxResolveWorkers() int {
	if e.ResolveWorkers < 1 {
		return 20
	}
	return e.ResolveWorkers
}

// preResolve resolves direct URLs for pending images with bounded concurrency.
// Honors ctx/stopFlag so a stop takes effect immediately instead of after the
// whole resolve batch finishes.
func (e *Engine) preResolve(ctx context.Context, imgs []store.Image) {
	var total, done int32
	for i := range imgs {
		if imgs[i].State == "pending" && imgs[i].DirectURL == "" {
			total++
		}
	}
	if total > 0 {
		e.log("INFO", "预解析 %d 张图片页…", total)
	}
	var wg sync.WaitGroup
	lim := make(chan struct{}, e.maxResolveWorkers())
	for i := range imgs {
		if imgs[i].State != "pending" || imgs[i].DirectURL != "" {
			continue
		}
		img := imgs[i]
		wg.Add(1)
		select {
		case lim <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			goto done
		}
		go func() {
			defer func() {
				if n := atomic.AddInt32(&done, 1); n%20 == 0 || int(n) == int(total) {
					e.log("INFO", "预解析进度 %d/%d", n, total)
				}
			}()
			defer wg.Done()
			defer func() { <-lim }()
			// 页面请求同样要受“每图床并发”约束。参考程序把 resolve 放在
			// 下载任务里，所有请求（图片页 + 原图）共用 maxConcurrentPerHost=2；
			// 我们如果只用 ResolveWorkers(20) 去撞同一个图床，imagetwist 会把
			// 后续的原图请求判成盗链，返回“Hotlinking is disabled”占位图。
			sem := e.setHostSem(e.hostKeyOf(img.PageURL))
			if err := sem.Acquire(ctx, 1); err != nil {
				return
			}
			defer sem.Release(1)
			res, err := e.Hosts.ResolveImageCtx(ctx, img.PageURL, img.ThumbURL)
			// Stop cancels the post ctx; a resolve that raced it is discarded.
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				e.log("WARN", "解析失败 %s: %v", img.PageURL, err)
				return
			}
			name := res.Filename
			if !e.FetchOrigName || name == "" {
				name = ""
			}
			e.Store.ExecImageUpdate(img.ID, res.ImageURL, name)
		}()
	}
done:
	wg.Wait()
}

// downloadImage resolves (if needed) and downloads one image with retries.
func (e *Engine) downloadImage(ctx context.Context, rowID int64, post *store.Post, dir string, img store.Image) {
	// pick up the pre-resolved direct URL / original name written meanwhile
	if fresh, err := e.Store.GetImage(img.ID); err == nil {
		img = *fresh
	}
	// per-host throttle (REDESIGN §4.3 双级信号量)
	hostName := e.hostKeyOf(img.PageURL)
	hostSem := e.setHostSem(hostName)
	// Acquire with the post's cancellable ctx so queued images are released
	// immediately on stop, then bail out before touching the DB.
	if err := hostSem.Acquire(ctx, 1); err != nil {
		return
	}
	e.mu.Lock()
	sf := e.stopFlag[rowID]
	e.mu.Unlock()
	if sf {
		hostSem.Release(1)
		e.Store.QueueImageState(img.ID, "pending", 0, "", "", 0)
		if e.OnImageEvent != nil {
			e.OnImageEvent(rowID, img.ID)
		}
		return
	}
	defer hostSem.Release(1)

	// resume: an image we previously wrote (path recorded in db) and the file
	// is still there → done. Never treats a same-named stranger as done.
	// Only "skip" short-circuits here; "rename" must keep downloading (to a
	// unique name) and "overwrite" re-downloads.
	if e.ExistingFilePolicy == "skip" && img.FilePath != "" {
		if st, err := os.Stat(img.FilePath); err == nil && st.Size() > 0 && validImageFile(img.FilePath) && !isPlaceholderFile(img.FilePath, img.FilePath) {
			e.Store.QueueImageState(img.ID, "done", st.Size(), img.FilePath, "", 0)
			if e.OnImageEvent != nil {
				e.OnImageEvent(rowID, img.ID)
			}
			return
		}
	}

	e.Store.QueueImageState(img.ID, "downloading", 0, "", "", 0)
	if e.OnImageEvent != nil {
		e.OnImageEvent(rowID, img.ID)
	}

	direct, origName := img.DirectURL, img.OriginalName
	if direct == "" {
		res, err := e.Hosts.ResolveImageCtx(ctx, img.PageURL, img.ThumbURL)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			e.fail(rowID, img, err.Error())
			return
		}
		direct = res.ImageURL
		if e.FetchOrigName {
			origName = res.Filename
		}
		e.Store.ExecImageUpdate(img.ID, direct, origName)
	}

	name := origName
	if name == "" {
		// no original name captured → numbered fallback (REDESIGN §5)
		name = fmt.Sprintf("%03d%s", img.Seq, extOf(direct))
	} else if !reImgExt.MatchString(name) {
		// original name without an extension (e.g. page <title>) — append
		// the one implied by the direct URL
		name = name + extOf(direct)
	}
	target := filepath.Join(dir, sanitizeFileName(name))

	// disk-level resume ("skip" only): the post may have been removed and
	// re-added (DB image rows gone, files still on disk). If the target
	// already exists with content, adopt it instead of re-downloading.
	if e.ExistingFilePolicy == "skip" {
		if st, err := os.Stat(target); err == nil && st.Size() > 0 && validImageFile(target) && !isPlaceholderFile(target, target) {
			// 两张不同的图算出同一个文件名时，后来的不能认领前一张的文件：
			// 否则两条记录共用一个路径，真实的那张永远缺失（而且会触发整帖重建、
			// 已经下完的帖子一点下载就全部重下）。给它换一个唯一名字继续下载。
			if e.Store != nil && e.Store.ImagePathClaimed(rowID, target, img.ID) {
				target = uniqueTargetPath(target)
			} else {
				e.Store.QueueImageState(img.ID, "done", st.Size(), target, "", 0)
				if e.OnImageEvent != nil {
					e.OnImageEvent(rowID, img.ID)
				}
				return
			}
		}
	}
	// "rename": keep the existing file, save this download under a unique name
	if e.ExistingFilePolicy == "rename" {
		if _, err := os.Stat(target); err == nil {
			ext := filepath.Ext(target)
			base := target[:len(target)-len(ext)]
			for i := 1; ; i++ {
				cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
				if _, err := os.Stat(cand); err != nil {
					target = cand
					break
				}
			}
		}
	}

	var lastErr error
	referer := e.Hosts.DownloadReferer(img.PageURL)
	for attempt := 0; attempt < e.MaxRetries; attempt++ {
		e.mu.Lock()
		sf := e.stopFlag[rowID]
		e.mu.Unlock()
		if sf {
			e.Store.QueueImageState(img.ID, "pending", 0, "", "", 0)
			if e.OnImageEvent != nil {
				e.OnImageEvent(rowID, img.ID)
			}
			return
		}
		if attempt > 0 {
			var backoff time.Duration
			var ph *placeholderError
			if errors.As(lastErr, &ph) {
				// 被判盗链时快重试没用：imagetwist 的拦截按时间窗生效，
				// 于是拉长间隔，并改用图片页地址当 Referer 再试。
				backoff = time.Duration(8*attempt) * time.Second
				if img.PageURL != "" {
					referer = img.PageURL
				}
			} else {
				backoff = time.Duration(1<<uint(attempt-1)) * time.Second
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				e.Store.QueueImageState(img.ID, "pending", 0, "", "", 0)
				if e.OnImageEvent != nil {
					e.OnImageEvent(rowID, img.ID)
				}
				return
			}
		}
		n, err := e.fetchToFile(ctx, direct, target, img.ID, post.ForumPostID, referer)
		if err == nil {
			e.Store.QueueImageState(img.ID, "done", n, target, "", 0)
			if e.OnImageEvent != nil {
				e.OnImageEvent(rowID, img.ID)
			}
			return
		}
		lastErr = err
		e.log("WARN", "post #%d 第 %d 次尝试失败 %s: %v", post.ForumPostID, attempt+1, name, err)
		// 人机验证：重试只会被同样挡住（还可能把 IP 关更久），立刻收手等用户处理
		var chal *cfguard.Challenge
		if errors.As(err, &chal) {
			break
		}
	}
	// dead link (404/410): the cached direct url is stale — clear it so a
	// later retry re-resolves with current rules
	if lastErr != nil && (strings.Contains(lastErr.Error(), "HTTP 404") || strings.Contains(lastErr.Error(), "HTTP 410")) {
		e.Store.ExecImageUpdate(img.ID, "", "")
	}
	msg := "未知错误"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	e.fail(rowID, img, msg)
}

func (e *Engine) fail(rowID int64, img store.Image, msg string) {
	e.Store.QueueImageState(img.ID, "error", 0, "", msg, 0)
	if e.OnImageEvent != nil {
		e.OnImageEvent(rowID, img.ID)
	}
	e.log("ERROR", "post #%d 图片 %d 失败：%s（%s）", rowID, img.Seq, img.PageURL, msg)
}

var reImgExt = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|webp|bmp)(\?.*)?$`)

// placeholderError 表示服务端返回的不是原图，而是防盗链/错误占位图
// （imagetwist 在认定调用方盗链时会给 HTTP 200 + 一张几百像素的小 PNG，
// 文案是 “Hotlinking is disabled…”）。这种响应以前会被当成下载成功，
// 结果整个图集里存下一堆一模一样的小图。
type placeholderError struct {
	detail string
}

func (e *placeholderError) Error() string {
	return "图床返回了防盗链占位图而非原图（" + e.detail + "），已丢弃并重试"
}

// kindOfHead sniffs the container format from magic bytes.
func kindOfHead(head []byte) string {
	switch {
	case len(head) >= 2 && head[0] == 0xFF && head[1] == 0xD8:
		return "jpg"
	case len(head) >= 3 && string(head[0:3]) == "GIF":
		return "gif"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "webp"
	case len(head) >= 8 && string(head[0:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(head) >= 2 && head[0] == 'B' && head[1] == 'M':
		return "bmp"
	}
	return ""
}

// kindOfExt maps a target filename's extension into the same vocabulary
// ("jpeg"→"jpg", empty when unknown).
func kindOfExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "jpg"
	case ".png":
		return "png"
	case ".gif":
		return "gif"
	case ".webp":
		return "webp"
	case ".bmp":
		return "bmp"
	}
	return ""
}

func extOf(u string) string {
	if m := reImgExt.FindStringSubmatch(u); m != nil {
		return "." + strings.ToLower(m[1])
	}
	return ".jpg"
}

func (e *Engine) fetchToFile(ctx context.Context, directURL, target string, imgID int64, postID int64, referer string) (int64, error) {
	// 该站刚拦过、还没过验证：直接返回，不再发请求（重试只会关更久）
	if err := cfguard.ShortCircuit(directURL); err != nil {
		return 0, err
	}
	if e.ExistingFilePolicy == "overwrite" {
		os.Remove(target)
	}
	client := &http.Client{
		Timeout:   time.Duration(e.TimeoutSec) * time.Second,
		Transport: &http.Transport{Proxy: e.proxyFor()},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", directURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", cfguard.Default.UserAgent())
	// 用户手动过一次人机验证后拿到的 cookie（cf_clearance 等）
	for _, c := range cfguard.Default.CookiesFor(directURL) {
		req.AddCookie(c)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// 先看是不是“人机验证”拦截页：是的话要给用户明确的提示，而不是 HTTP 403
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if cerr := cfguard.DetectChallenge(directURL, resp.StatusCode, resp.Header, prefix); cerr != nil {
			return 0, cerr
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			return 0, fmt.Errorf("HTTP %d（链接已失效）", resp.StatusCode)
		}
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 挑战页有时也用 200 + 一坨 JS 返回：正文当图片存下去必然是坏文件
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if cerr := cfguard.DetectChallenge(directURL, resp.StatusCode, resp.Header, prefix); cerr != nil {
			return 0, cerr
		}
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), resp.Body))
	}
	if dir := filepath.Dir(target); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	tmp := fmt.Sprintf("%s.vripper%d.tmp", target, imgID)
	// 设置了临时目录就先下到那里，成功后再搬过去（跨盘 rename 会失败，下面有 copy 兜底）
	if td := strings.TrimSpace(e.TempDir); td != "" {
		if err := os.MkdirAll(td, 0o755); err == nil {
			tmp = filepath.Join(td, fmt.Sprintf("%s.vripper%d.tmp", filepath.Base(target), imgID))
		}
	}
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	total := resp.ContentLength
	buf := make([]byte, 32*1024)
	var n int64
	lastReport := time.Now()
	for {
		r, rerr := resp.Body.Read(buf)
		if r > 0 {
			if _, werr := f.Write(buf[:r]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return 0, werr
			}
			n += int64(r)
			if e.OnImageProgress != nil && time.Since(lastReport) > 150*time.Millisecond {
				e.OnImageProgress(imgID, n, total)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return 0, rerr
		}
	}
	if e.OnImageProgress != nil {
		e.OnImageProgress(imgID, n, total)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	// server said more than it sent → truncated download
	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(tmp)
		return 0, fmt.Errorf("下载不完整（%d/%d 字节）", n, resp.ContentLength)
	}
	// 有些图床会把完整的图片后面再拼一段 HTML（中间页/反爬提示），浏览器
	// 会忽略尾巴照常显示，但严格校验会判成“格式损坏”。这里先把多余的尾巴
	// 截掉，再按正常流程校验（文件也因此保持干净）。
	if sz, cut := trimTrailingJunk(tmp); cut > 0 {
		n = sz
		e.log("INFO", "%s 响应尾部多了 %d 字节非图片数据，已截断", filepath.Base(target), cut)
	}
	// 防盗链占位图识别：扩展名要的是 JPEG，服务端却回了一小块 PNG/GIF，
	// 那基本就是 imagetwist 这类图床的拦截图。判成失败而不是“下载成功”，
	// 否则占位图会以原图名义落盘，且以后 “跳过已存在文件” 会一直跳过它。
	if isPlaceholderFile(tmp, target) {
		kind, want := fileKind(tmp), kindOfExt(target)
		os.Remove(tmp)
		return 0, &placeholderError{detail: fmt.Sprintf("期望 %s，实收 %s，%d 字节", want, kind, n)}
	}
	// corrupt-image self-check: a file that fails the head/tail validation
	// would be re-skipped forever, so treat it as a failed download
	if !validImageFile(tmp) {
		os.Remove(tmp)
		return 0, fmt.Errorf("图片数据不完整或格式损坏")
	}
	// atomic rename onto final name
	if err := os.Rename(tmp, target); err != nil {
		// 临时目录和下载目录不在同一个盘时 rename 会失败，退化为复制
		if cerr := copyFile(tmp, target); cerr != nil {
			os.Remove(tmp)
			return 0, err
		}
		os.Remove(tmp)
	}
	return n, nil
}

// copyFile 把 src 复制到 dst（先写 .tmp 再改名，尽量保持原子性）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".copy.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

var reSanitize = regexp.MustCompile(`[\\/:*?"<>|]`)

// validImageFile reports whether path looks like a complete image:
// magic bytes at the head plus a proper end marker for formats that have
// one. Reads ~24 bytes total — cheap enough to run on every candidate
// before trusting an existing file (skip/resume/recover paths).
func validImageFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 12 {
		return false
	}
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	var kind string
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		kind = "jpg"
	case string(head[0:3]) == "GIF":
		kind = "gif"
	case string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		kind = "webp"
	case string(head[0:8]) == "\x89PNG\r\n\x1a\n":
		kind = "png"
	case head[0] == 'B' && head[1] == 'M':
		return true // BMP has no end marker worth checking
	default:
		return false // not an image at all (HTML error page etc.)
	}
	var tail [12]byte
	if _, err := f.ReadAt(tail[:], st.Size()-12); err != nil {
		return false
	}
	switch kind {
	case "jpg":
		if tail[10] == 0xFF && tail[11] == 0xD9 {
			return true
		}
		// 有些图床（pixhost 等）会把完整的 JPEG 后面再追加一段 HTML
		// （中间页/反爬提示）。图片本身是好的，浏览器也照常显示，
		// 所以只要末尾 8KB 内能找到 EOI 标记就认（保存时会截掉尾巴）。
		return hasTailMarker(path, st.Size(), []byte{0xFF, 0xD9})
	case "png":
		if bytes.Contains(tail[:], []byte("IEND")) {
			return true
		}
		return hasTailMarker(path, st.Size(), []byte("IEND"))
	case "gif":
		if tail[11] == 0x3B {
			return true
		}
		return hasTailMarker(path, st.Size(), []byte{0x3B})
	case "webp":
		return true // RIFF size field is unreliable across encoders; head is enough
	}
	return true
}

// tailScanWindow 在文件末尾这段范围内找格式结束标记（覆盖“图片后面被追加了
// 一段 HTML 提示页”的情况；正常图片的结束标记就在最后几个字节内）。
const tailScanWindow = 16 << 10

// hasTailMarker 在文件末尾 tailScanWindow 字节内从后往前找 marker。
func hasTailMarker(path string, size int64, marker []byte) bool {
	if size <= int64(len(marker)) || len(marker) == 0 {
		return false
	}
	n := int64(tailScanWindow)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, size-n); err != nil {
		return false
	}
	return bytes.Contains(buf, marker)
}

// trimTrailingJunk 把“图片后面追加的 HTML/文本”截掉，返回新大小与被删的字节数。
// 只在末尾 tailScanWindow 内找到结束标记、且后面确实还有多余字节时才动手。
func trimTrailingJunk(path string) (int64, int64) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 12 {
		return 0, 0
	}
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return 0, 0
	}
	var marker []byte
	var extra int // 标记之后还应有的固定字节（PNG 的 IEND 后跟 4 字节 CRC）
	switch kindOfHead(head[:]) {
	case "jpg":
		marker = []byte{0xFF, 0xD9}
	case "png":
		marker, extra = []byte("IEND"), 4
	case "gif":
		marker = []byte{0x3B}
	default:
		return 0, 0
	}
	size := st.Size()
	n := int64(tailScanWindow)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil {
		return 0, 0
	}
	idx := bytes.LastIndex(buf, marker)
	if idx < 0 {
		return 0, 0
	}
	end := size - n + int64(idx) + int64(len(marker)) + int64(extra)
	if end >= size || end < 12 {
		return 0, 0
	}
	if err := f.Truncate(end); err != nil {
		return 0, 0
	}
	return end, size - end
}

// fileKind reads the first bytes of path and returns the sniffed format.
func fileKind(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return ""
	}
	return kindOfHead(head[:])
}

// isPlaceholderFile reports whether path looks like an anti-hotlink /
// error placeholder: a small file whose real format contradicts the format
// implied by nameRef's extension (imagetwist serves a ~8KB PNG at .jpg urls
// when it decides the caller is leeching). Real photos never trip this.
// nameRef is needed because the download target is written to a ".tmp"
// file while it is still being fetched.
func isPlaceholderFile(path, nameRef string) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() >= 64*1024 {
		return false
	}
	kind, want := fileKind(path), kindOfExt(nameRef)
	return kind != "" && want != "" && kind != want
}

// RepairPlaceholders 扫描“已完成”的图片记录，把那些扩展名与真实内容
// 不符的小文件（典型是 imagetwist 的防盗链占位图，8KB 左右的 PNG 伪装成
// jpg）删掉并退回 pending，让下次下载重新解析。只检查 <64KB 的文件，
// 启动开销可忽略。
func (e *Engine) RepairPlaceholders() (int, error) {
	rows, err := e.Store.DB().Query(
		`SELECT id, file_path, file_size FROM image
		 WHERE state = 'done' AND file_path <> '' AND file_size > 0 AND file_size < 65536`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id   int64
		path string
	}
	var victims []row
	for rows.Next() {
		var r row
		var size int64
		if err := rows.Scan(&r.id, &r.path, &size); err != nil {
			continue
		}
		kind := fileKind(r.path)
		want := kindOfExt(r.path)
		if kind != "" && want != "" && kind != want {
			victims = append(victims, r)
		}
	}
	rows.Close()
	for _, v := range victims {
		os.Remove(v.path)
		e.Store.QueueImageState(v.id, "pending", 0, "", "", 0)
	}
	if len(victims) > 0 {
		e.log("WARN", "已识别并重置 %d 张防盗链占位图（内容与扩展名不符），下次下载会重新抓取", len(victims))
	}
	return len(victims), nil
}

func sanitizeFileName(s string) string {
	s = reSanitize.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120]
	}
	if s == "" {
		s = "unnamed"
	}
	return s
}

// uniqueTargetPath 目标文件已存在（或已被同一帖子的另一条记录占用）时，
// 生成 "base(1).ext" 这样的唯一名，避免覆盖/共用文件。
func uniqueTargetPath(target string) string {
	ext := filepath.Ext(target)
	base := strings.TrimSuffix(target, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
	return fmt.Sprintf("%s(%d)%s", base, time.Now().UnixNano(), ext)
}

// ThumbKey derives a stable bbolt key from a URL.
func ThumbKey(u string) string {
	h := sha1.Sum([]byte(u))
	return hex.EncodeToString(h[:])
}

var ErrStopped = errors.New("stopped")
