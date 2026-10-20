package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"viperipper/internal/pipeline"
	"viperipper/internal/settings"
	"viperipper/internal/store"
)

var reImgExt = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|webp|bmp)$`)

// 只读诊断：假设把每条帖子/楼层改名成一个新名字，算一遍这次改名会不会真的
// 带动磁盘上的文件夹，搬不动时说出卡在哪一步。不写数据库、不碰任何文件。
func main() {
	dataDir := os.Args[1]
	st, err := store.Open(dataDir)
	if err != nil {
		fmt.Println("打开数据目录失败:", err)
		return
	}
	defer st.Close()

	var s settings.Settings
	if raw := st.GetKV("app"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &s)
	}
	root := s.DownloadDir

	e := pipeline.New(st, nil)
	e.BaseDir = root
	e.PerPostFolder = s.PerPostFolder
	e.FetchFloorTitle = s.FetchFloorTitle
	fmt.Printf("下载根目录: %s\n", root)
	fmt.Printf("设置：每帖一个文件夹=%v  自动获取楼层标题=%v\n\n", s.PerPostFolder, s.FetchFloorTitle)

	threads := map[int64]string{}
	if ts, err := st.ListThreads(); err == nil {
		for _, t := range ts {
			threads[t.ForumThreadID] = t.Title
		}
	}
	posts, err := st.ListPosts()
	if err != nil {
		fmt.Println("读取帖子失败:", err)
		return
	}

	const testName = "改名测试"
	blocked := 0
	for i := range posts {
		p := &posts[i]
		p.CustomName = testName // 模拟用户在界面上改名
		want := e.DesiredDir(p)
		cur := strings.TrimSpace(p.Dir)

		kind := "普通帖子"
		if _, ok := threads[p.ForumThreadID]; ok {
			kind = fmt.Sprintf("主题楼层 tid=%d", p.ForumThreadID)
		}
		target, isStale := e.StaleDirTarget(p)
		why := ""
		switch {
		case cur == "":
			why = "没有文件夹 → 改名后下载时按新名创建（正常）"
		case isStale:
			why = "★ 记录的目录已不存在，实际位置是 " + rel(root, target) + " → 重启后自动纠正指针，改名就能生效"
			blocked++
		case !under(root, cur):
			why = "★ 记录的目录既不在下载根目录下、也找不到实际位置 → 需要人工确认（改名带不动）"
			blocked++
		case p.State == "downloading":
			why = "正在下载 → 推迟到停止/完成后自动归位"
		case hasImage(cur):
			why = "有已下载图片 → 会整体搬过去（改名生效）"
		default:
			why = "文件夹为空 → 直接改用新目录名"
		}

		fmt.Printf("#%-4d %-20s %-30s %s\n", p.ID, kind, trunc(p.Title, 26), p.State)
		fmt.Printf("      现位置  : %s\n", rel(root, cur))
		fmt.Printf("      改为  : %s\n", rel(root, want))
		fmt.Printf("      判定  : %s\n\n", why)
	}
	fmt.Printf("合计 %d 条，其中 %d 条因为记录的目录在下载根目录之外而搬不动。\n", len(posts), blocked)
}

func under(base, dir string) bool {
	if strings.TrimSpace(base) == "" {
		return false
	}
	r, err := filepath.Rel(base, dir)
	return err == nil && !strings.HasPrefix(r, "..")
}

func rel(base, dir string) string {
	if dir == "" {
		return "(空)"
	}
	if r, err := filepath.Rel(base, dir); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(dir) + "   ← 不在下载根目录里"
}

func hasImage(dir string) bool {
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

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
