package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"viperipper/internal/store"
)

// 下载目录布局（对齐参考程序 downloadDirectory + folderName 的分层）：
//
//	PerPostFolder = false            全部平铺           → BaseDir/<帖子标题>
//	单楼层主题   PerPostFolder = true 每帖一个文件夹       → BaseDir/<主题标题>
//	多楼层主题   PerPostFolder = true 多图册楼层建子文件夹 → BaseDir/<主题标题>/<楼层名>
//
// 主题标题和楼层名都要过一遍文件名消毒（Windows 不允许 \ / : * ? " < > |）。
// 楼层名优先用楼层标题（设置里的“自动获取楼层标题”），关掉时用 F<楼层号>。

var reDirBad = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

// sanitizeDirName 把任意标题变成可用的目录名：非法字符替换成 _，去掉首尾空格和点，
// 过长截断，空结果返回空串由调用方兜底。
func sanitizeDirName(s string) string {
	s = reDirBad.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ". ")
	// Windows 保留名
	switch strings.ToUpper(s) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4",
		"COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3":
		if s != "" {
			s += "_"
		}
	}
	// 按字节截断会在中文上切坏字符，改成按 rune
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	return s
}

// ComputePostDir 纯粹的目录推导（不碰磁盘、不查库），插入帖子时和下载时共用。
//
//	baseDir     下载根目录
//	forum       论坛板块名（开启 SortByForum 时作为最外层分类目录）
//	threadTitle 主题标题（空 = 拿不到，退回帖子标题单层目录）
//	floors      主题下已收录的楼层数（<=1 视为单帖主题）
//	postTitle   帖子/楼层标题
//	floorNo     楼层号（用于关闭“自动获取楼层标题”时的 F<号> 命名）
func ComputePostDir(baseDir, forum, threadTitle string, floors int, postTitle string, floorNo int, perPostFolder, useFloorTitle, sortByForum bool) string {
	if sortByForum && forum != "" {
		// 按板块分类：最外层多一级板块目录（其余布局选项照常生效）
		baseDir = filepath.Join(baseDir, sanitizeDirName(forum))
	}
	thread := sanitizeDirName(threadTitle)
	post := sanitizeDirName(postTitle)
	if !perPostFolder {
		// 全部平铺：不做主题分组，每个帖子自己的文件夹直接放在下载根目录
		// （不把文件混进根目录，避免不同帖子同名文件互相覆盖/跳过）
		if post == "" {
			return baseDir
		}
		return filepath.Join(baseDir, post)
	}
	if thread == "" {
		// 没有主题标题：退回“每帖一个文件夹”的旧行为，至少不会全糊在一起
		if post == "" {
			return baseDir
		}
		return filepath.Join(baseDir, post)
	}
	if floors <= 1 {
		return filepath.Join(baseDir, thread)
	}
	floor := post
	if !useFloorTitle {
		floor = ""
	}
	// 多楼层帖子的标题被统一改写成「<原楼层标题> — 7F」方便列表区分，
	// 当目录名时去掉这个后缀：要么还原成楼层自己的标题，要么直接用 F7。
	floor = TrimFloorSuffix(floor)
	// 楼层标题就是主题标题的变体（"Re <主题>"、加了后缀等）时没必要多套一层同名目录
	if floor == "" || floor == thread || strings.Contains(floor, thread) || strings.Contains(thread, floor) {
		floor = fmt.Sprintf("F%d", floorNo)
	}
	return filepath.Join(baseDir, thread, floor)
}

var reFloorSuffix = regexp.MustCompile(`\s*[—\-]\s*\d+F$`)
var reFloorPrefix = regexp.MustCompile(`^\d+F\s*[—\-]\s*`)

// TrimFloorSuffix 去掉列表为了区分楼层自动加的序号：旧格式“标题 — 7F”的
// 后缀和新格式“7F—标题”的前缀都认，历史条目两种写法都能还原出正文标题。
func TrimFloorSuffix(s string) string {
	s = reFloorPrefix.ReplaceAllString(s, "")
	return reFloorSuffix.ReplaceAllString(s, "")
}

// DirNameFromTitle 把条目名字变成文件夹名：只做文件名消毒，不再动内容——用户在
// 列表里把条目重命名成什么（包括自己保留的“— 1F”这类序号后缀），文件夹
// 就叫什么。所见即所得，改完名以后的下载/续传位置都由它决定。
func DirNameFromTitle(title string) string {
	return sanitizeDirName(title)
}

// MatchesTitleName 判断磁盘上现成的文件夹名是不是由某个标题推导而来的：
// 两种写法都算——带后缀的完整名字，以及旧版本去掉后缀后的写法。
// 只用于启动时把历史数据固定下来，据此判断绝不猜错的基础上才落库。
func MatchesTitleName(dirBase, title string) bool {
	if dirBase == "" || title == "" {
		return false
	}
	return dirBase == DirNameFromTitle(title) || dirBase == sanitizeDirName(TrimFloorSuffix(title))
}

// DesiredDir 按当前设置推导目录（忽略已落库的 dir）。用于旧记录指向
// 废弃位置（如老版默认的 data\galleries）时按当前下载根重推。
func (e *Engine) DesiredDir(post *store.Post) string {
	if post == nil {
		return e.BaseDir
	}
	return e.wantDir(post)
}

// PlainPostDirFor 只算目录，不碰磁盘（界面展示用：点一下帖子不该搬文件）。
// post.Dir 是用户手工指定/已落库的目录时直接用它。
func (e *Engine) PlainPostDirFor(post *store.Post) string {
	if post == nil {
		return e.BaseDir
	}
	if dir := strings.TrimSpace(post.Dir); dir != "" {
		return dir
	}
	return e.wantDir(post)
}

// wantDir 按当前设置算出这个帖子“应该”落在哪里（忽略 post.Dir）。
// 用户重命名过的条目（post.CustomName）优先用用户给的名字——否则楼层标题
// 常常是主题标题的变体（"Re: <主题>"、"<主题> — 2F"），会被 ComputePostDir
// 的“和主题标题雷同就退回 F<n>”兜底掉，改名就跟不动文件夹了。
func (e *Engine) wantDir(post *store.Post) string {
	if name := DirNameFromTitle(post.CustomName); name != "" {
		return e.customDir(post, name)
	}
	_, grouped := e.derivedDirs(post)
	if e.PerPostFolder {
		return grouped
	}
	base := e.BaseDir
	if f := e.forumDir(post); f != "" {
		base = filepath.Join(base, f)
	}
	return filepath.Join(base, sanitizeDirName(post.Title))
}

// forumDir 按板块分类开启时返回最外层的板块目录段（未开启/查不到为 ""）。
// 先看条目自己记的板块：单帖链接（下载连接）没有 thread 行，只能靠这个；
// 老数据没记的再按 thread_id 反查主题表。
func (e *Engine) forumDir(post *store.Post) string {
	if !e.SortByForum || post == nil {
		return ""
	}
	if f := strings.TrimSpace(post.Forum); f != "" {
		return f
	}
	if e.Store == nil || post.ForumThreadID == 0 {
		return ""
	}
	if f := strings.TrimSpace(e.Store.ThreadForumOf(post.ForumThreadID)); f != "" {
		return f
	}
	// 兜底：库里这条 post 落过论坛名（刚插入时可能还没回填到内存对象）
	return strings.TrimSpace(e.Store.PostForum(post.ID))
}

// customDir 用户起过名的条目的目录：名字固定用用户那个，主题还存在时仍
// 放在主题目录下，避免改名把楼层从主题分组里甩出去。
func (e *Engine) customDir(post *store.Post, name string) string {
	base := e.BaseDir
	if f := e.forumDir(post); f != "" {
		base = filepath.Join(base, f)
	}
	if !e.PerPostFolder {
		return filepath.Join(base, name)
	}
	threadTitle := ""
	if e.Store != nil && post.ForumThreadID != 0 {
		threadTitle, _ = e.Store.ThreadTitleAndPostCount(post.ForumThreadID)
	}
	thread := sanitizeDirName(threadTitle)
	if thread == "" {
		return filepath.Join(base, name)
	}
	// 自定义名和主题名一样时没必要再套一层（勾了“直接使用主页标题”就会这样），
	// 否则会拼出 主题\主题 的重复目录。
	if sanitizeDirName(name) == thread {
		return filepath.Join(base, thread)
	}
	return filepath.Join(base, thread, name)
}

// derivedDirs 返回本程序可能为这个帖子生成的两个目录：
// flat = 不做主题分组（BaseDir/帖子标题），grouped = 当前楼层命名策略下的主题分母录。
func (e *Engine) derivedDirs(post *store.Post) (flat, grouped string) {
	forum := e.forumDir(post)
	base := e.BaseDir
	if forum != "" {
		base = filepath.Join(base, forum)
	}
	title := sanitizeDirName(post.Title)
	flat = filepath.Join(base, title)
	threadTitle, floors := "", 0
	if e.Store != nil && post.ForumThreadID != 0 {
		threadTitle, floors = e.Store.ThreadTitleAndPostCount(post.ForumThreadID)
	}
	grouped = ComputePostDir(e.BaseDir, forum, threadTitle, floors, post.Title, post.FloorNo, true, e.FetchFloorTitle, e.SortByForum)
	if grouped == "" {
		grouped = flat
	}
	return flat, grouped
}

// EnsurePostDir 返回帖子真正应该落盘的目录，并处理两类历史/切换情况：
//
//  1. 老数据摊在 BaseDir/<帖子标题>（或旧的主题分母录）→ 整体 rename 进当前布局；
//  2. 改了“目录组织/楼层标题/下载根目录” → 把已有文件搬到现在该在的位置。
//
// 只在“目标目录还没有图片、来源目录有图片”时才搬，搬完立刻落库 post.dir，
// 所以下一次调用直接命中、不会反复来回移动。
// isSameNameDescendant 判断 cur 是否是 want 下面"同名重复"的目录
// （want/主题/主题 → cur）。这种情况是程序自己以前拼出来的，不是用户手工指定的位置。
func isSameNameDescendant(cur, want string) bool {
	rel, err := filepath.Rel(want, cur)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	base := filepath.Base(want)
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if !strings.EqualFold(part, base) {
			return false
		}
	}
	return true
}

// sameNameNested 判断 dir 里是否只套着一个同名子目录（历史遗留的 主题\主题 结构），
// 是的话返回那个子目录的路径，否则返回空串。
func sameNameNested(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return ""
	}
	if !strings.EqualFold(entries[0].Name(), filepath.Base(dir)) {
		return ""
	}
	return filepath.Join(dir, entries[0].Name())
}

// mergeDirUp 把 from 里的内容提到 to 里，然后删掉空的 from。同名的文件以 to 里已有的为准。
func mergeDirUp(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, en := range entries {
		src := filepath.Join(from, en.Name())
		dst := filepath.Join(to, en.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return os.Remove(from)
}

func (e *Engine) EnsurePostDir(post *store.Post) string {
	if post == nil {
		return e.BaseDir
	}
	want := e.wantDir(post)
	cur := strings.TrimSpace(post.Dir)
	if cur != "" && samePath(cur, want) {
		return want
	}
	// 目标目录已存在时的三种情况：
	//   有图片 → 直接认它（用户可能手动放回过）；
	//   空目录 → 删掉让位，把有文件的位置搬进来；
	//   有别的文件 → 不能占，放弃迁移（保持原目录，谁有文件用谁）。
	targetOK := true
	if st, err := os.Stat(want); err == nil {
		switch {
		case !st.IsDir():
			e.log("WARN", "目标路径被同名文件占用：%s（保持原目录）", want)
			targetOK = false
		case hasImageFile(want):
			return want
		case !isEmptyDir(want):
			// 历史遗留的 主题\主题：外层只套着一个同名子目录，图片都在里层。
			// 这时把里层内容提上来、删掉空壳，目录就只剩一层了。
			if inner := sameNameNested(want); inner != "" && hasImageFile(inner) {
				if err := mergeDirUp(inner, want); err == nil {
					if e.Store != nil && post.ID != 0 {
						_ = e.Store.SetPostDir(post.ID, want)
						post.Dir = want
					}
					return want
				}
				e.log("WARN", "整理重复目录失败：%s", relUnder(e.BaseDir, want))
			}
			e.log("WARN", "%s 已存在且含非图片文件，不自动搬动（保持原目录）", relUnder(e.BaseDir, want))
			targetOK = false
		default:
			_ = os.Remove(want) // 空壳目录，让位
		}
	}
	// 依次看：已落库的目录、平铺目录、分主题目录；谁有图就把谁搬到 want
	cands := e.dirCandidates(post, cur)
	// cur 是不是本程序自己推导出来的目录（在候选里）。不在候选里的说明是
	// 用户手工指定的位置，要原样保留。
	curOurs := false
	for _, cand := range cands {
		if samePath(cand, cur) {
			curOurs = true
			break
		}
	}
	// 历史遗留的 主题\主题：这种路径不在候选里，但显然是程序自己拼出来的，
	// 不该当成"用户手工指定的目录"而保留（否则删掉目录重新下载还会建两层）。
	if !curOurs && isSameNameDescendant(cur, want) {
		curOurs = true
	}
	for _, cand := range cands {
		if !hasImageFile(cand) {
			continue
		}
		if samePath(cand, want) {
			return want
		}
		if !targetOK {
			return cand // 目标被占用，继续用有文件的那个目录
		}
		return e.moveDir(post, cand, want)
	}
	// 任何位置都没有文件：按新布局来（下载时创建）。以前这里一律返回 cur，
	// 于是“库里还记着旧布局、但目录里还没有图”的条目会一直沿用旧目录，
	// 楼层文件夹永远建在板块目录外面（点了下载也不会自己归位）。
	if cur != "" && !curOurs {
		return cur // 用户手工指定的目录，保持
	}
	// 顺手把库里的旧目录指针改成新布局，界面上的“下载目录”也不再显示错的位置
	if !samePath(cur, want) && e.Store != nil && post.ID != 0 {
		_ = e.Store.SetPostDir(post.ID, want)
		post.Dir = want
	}
	return want
}

// dirCandidates 列出可以整体搬动的候选目录（去重，跳过 want 自己）。
// 用户手工指定的“下载目录”只要不在下载根目录下，就不参与搬动。
func (e *Engine) dirCandidates(post *store.Post, cur string) []string {
	flat, grouped := e.derivedDirs(post)
	want := e.wantDir(post)
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || samePath(dir, want) {
			return
		}
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dir)
	}
	if cur != "" && (samePath(cur, flat) || samePath(cur, grouped)) {
		// 只搬本程序自己生成过的目录；用户手工指到别处的目录保持原样
		add(cur)
	}
	add(flat)
	add(grouped)
	// 早期“按板块分类”在楼层选择器链路上没生效时，目录是 want 去掉板块那一段
	// 的样子（BaseDir/<主题>/<楼层>，与板块目录平级）。这种位置也允许整体搬到
	// 板块目录下，否则这批已经下到错层的老条目会一直留在根目录。
	if f := e.forumDir(post); f != "" {
		if rel, err := filepath.Rel(filepath.Join(e.BaseDir, f), want); err == nil && !strings.HasPrefix(rel, "..") {
			add(filepath.Join(e.BaseDir, rel))
		}
	}
	return out
}

// moveDir 把 from 整体移动到 to（同盘 rename，瞬完），并改写库里已记录的路径。
// 失败时保持 from 继续用，绝不丢文件。
func (e *Engine) moveDir(post *store.Post, from, to string) string {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		e.log("WARN", "创建主题目录失败 %s: %v（继续用旧目录）", filepath.Dir(to), err)
		return from
	}
	if err := os.Rename(from, to); err != nil {
		e.log("WARN", "把 %s 移到 %s 失败：%v（继续用旧目录）",
			filepath.Base(from), filepath.Base(to), err)
		return from
	}
	if e.Store != nil {
		if n, err := e.Store.RewriteImagePathPrefix(from, to); err == nil && n > 0 {
			e.log("INFO", "已把 %s 的 %d 条记录改到新位置", filepath.Base(to), n)
		}
		_ = e.Store.SetPostDir(post.ID, to)
	}
	if post != nil {
		post.Dir = to
	}
	e.log("INFO", "目录归位：%s → %s", filepath.Base(from), relUnder(e.BaseDir, to))
	return to
}

// SyncFolderToTitle 重命名条目后的目录归位：已有文件 → 整体搬到新标题推导的
// 目录（EnsurePostDir/moveDir 负责）；新旧目录都没有文件 → 直接切到新名，
// 下载时按新名创建。任一目录有文件但搬不动（被占用）时保持原目录。
// SyncFolderToTitle 重命名条目后把磁盘目录对齐新标题，返回给人看的结果说明
// （空串=无需变化）。失败不会丢文件，只会保持原目录并说明原因。
func (e *Engine) SyncFolderToTitle(post *store.Post) string {
	if post == nil {
		return ""
	}
	if post.State == "downloading" {
		return "正在下载，文件夹改名推迟到停止/完成后自动进行"
	}
	// 记录的位置已经不在了（早期默认目录、手动搬过家）→ 先把它纠正到实际
	// 位置，否则下面一律以“不在下载根目录下”为由拒绝改名。
	e.ReconcileStaleDir(post)
	want := e.wantDir(post)
	cur := strings.TrimSpace(post.Dir)
	if cur == "" {
		// 还没下载过：直接用新标题推导的目录（下载时按新名创建）
		post.Dir = want
		if e.Store != nil {
			_ = e.Store.SetPostDir(post.ID, want)
		}
		return "该条目还没有文件夹，下载时会按新名称创建"
	}
	if samePath(cur, want) {
		return ""
	}
	// 重命名场景不能用 EnsurePostDir/dirCandidates：旧目录名是按旧标题推导
	// 的，永远不等于新标题推导的候选。这里直接搬旧目录——但仅限本程序
	// 管理范围内（BaseDir 下）的目录；用户手工指向外部的保持原样。
	if !underBase(e.BaseDir, cur) {
		e.log("WARN", "目录 %s 不在下载根目录下，重命名不搬动（保持原目录）", cur)
		return "文件夹在下载根目录之外，未改名（保持原目录）"
	}
	// 目标被占的三种情况：空壳目录→让位；同名非目录/非空目录→明确告诉用户，
	// 不能含糊地说“可能被占用”（绝大多数情况其实是重名）
	if st, err := os.Stat(want); err == nil {
		if !st.IsDir() {
			e.log("WARN", "目标位置被同名文件占用：%s（保持原目录）", want)
			return "目标位置已有同名文件「" + filepath.Base(want) + "」，未改名（保持原目录）"
		}
		if !isEmptyDir(want) {
			e.log("WARN", "目标目录已存在且非空：%s（保持原目录）", want)
			return "「" + filepath.Base(want) + "」已存在且不为空，未改名（保持原目录）"
		}
		_ = os.Remove(want) // 空壳目录，让位
	}
	if hasImageFile(cur) {
		got := e.moveDir(post, cur, want)
		if samePath(got, want) {
			return "文件夹已改名为「" + filepath.Base(got) + "」"
		}
		return "文件夹改名失败（可能正被其它程序占用），保持原名「" + filepath.Base(cur) + "」"
	}
	// 旧目录没有文件（下载过又清空/从未下载）：直接切到新名
	post.Dir = want
	if e.Store != nil {
		_ = e.Store.SetPostDir(post.ID, want)
	}
	return "原文件夹为空，已更新下载位置，之后按新名称创建"
}

// ReconcileStaleDir 纠正陈旧的目录记录：老数据里 post.dir 可能还停在早期
// 的下载根目录（如 data\galleries）或已被手动搬走的位置，而文件其实早就
// 在当前下载根目录下了。这时下载旁边的显示、改名时的搬动都会以“不在下载
// 根目录下”为由被拒绝，后续续传还会照着不存在的路径重新下载。
// 只在“记录的位置已经不存在、按当前设置推导的位置确实有图片”时才纠正，
// 不挪任何文件，只改数据库指针。返回是否纠正。
func (e *Engine) ReconcileStaleDir(post *store.Post) bool {
	target, ok := e.StaleDirTarget(post)
	if !ok {
		return false
	}
	return e.adoptDir(post, strings.TrimSpace(post.Dir), target)
}

// StaleDirTarget 判断条目的目录记录是否陈旧（记录的位置在磁盘上已经不在
// 了），是的话给出它现在的实际位置。只查磁盘、不写任何东西。
func (e *Engine) StaleDirTarget(post *store.Post) (string, bool) {
	if post == nil {
		return "", false
	}
	cur := strings.TrimSpace(post.Dir)
	if cur == "" {
		return "", false
	}
	if _, err := os.Stat(cur); err == nil {
		return "", false
	}
	cands := e.staleCandidates(post, cur)
	for _, want := range cands {
		if hasImageFile(want) {
			return want, true
		}
	}
	for _, want := range cands {
		if st, err := os.Stat(want); err == nil && st.IsDir() {
			return want, true
		}
	}
	return "", false
}

// adoptDir 把条目的目录记录从 cur 改成实际位置 want 并同步图片路径。
// 只改数据库指针，不移动任何文件。
func (e *Engine) adoptDir(post *store.Post, cur, want string) bool {
	if n, err := e.Store.RewriteImagePathPrefix(cur, want); err == nil && n > 0 {
		e.log("INFO", "已把 %d 条图片记录改到实际位置", n)
	}
	_ = e.Store.SetPostDir(post.ID, want)
	post.Dir = want
	e.log("INFO", "目录记录已纠正：%s", relUnder(e.BaseDir, want))
	return true
}

// staleCandidates 陈旧目录记录的可能“真实位置”，按把握从大到小排：
// 当前推导位置 → 文件名照旧只换下载根目录（早期默认目录就是这种情况）→
// 忽略用户新起名字后的自动布局。
func (e *Engine) staleCandidates(post *store.Post, cur string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dir)
	}
	add(e.wantDir(post))
	name := filepath.Base(cur)
	thread := ""
	if e.Store != nil && post.ForumThreadID != 0 {
		thread, _ = e.Store.ThreadTitleAndPostCount(post.ForumThreadID)
	}
	if t := sanitizeDirName(thread); t != "" && strings.EqualFold(filepath.Base(filepath.Dir(cur)), t) {
		add(filepath.Join(e.BaseDir, t, name))
	}
	add(filepath.Join(e.BaseDir, name))
	// 用户在界面上刚改的名字推导出的目录还不存在，按自动布局再试一次
	saved := post.CustomName
	post.CustomName = ""
	add(e.wantDir(post))
	post.CustomName = saved
	return out
}

// underBase 判断 dir 是否在 base 管理范围内（不区分大小写，Windows 盘符）。
func underBase(base, dir string) bool {
	if strings.TrimSpace(base) == "" {
		return false
	}
	rel, err := filepath.Rel(base, dir)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// relUnder 返回相对下载根目录的路径（日志更好读）。
func relUnder(base, dir string) string {
	if rel, err := filepath.Rel(base, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(dir)
}

// isEmptyDir 目录存在且没有任何条目。
func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// hasImageFile 判断目录里有没有至少一张图片（子目录不看）。
func hasImageFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, en := range entries {
		if en.IsDir() || strings.Contains(en.Name(), ".vripper") {
			continue
		}
		if reImgExt.MatchString(en.Name()) {
			return true
		}
	}
	return false
}

// SanitizeDirName 导出目录名清洗（ThreadDir 等外部推导目录名时与引擎保持一致）。
func SanitizeDirName(s string) string { return sanitizeDirName(s) }
