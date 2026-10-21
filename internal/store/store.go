package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	bolt "go.etcd.io/bbolt"
)

// Post states (REDESIGN §2.2): ready | downloading | paused | error | done
// Image states: pending | downloading | done | error

type Post struct {
	ID            int64  `json:"id"`
	ForumThreadID int64  `json:"threadId"`
	ForumPostID   int64  `json:"postId"`  // forum post number id
	FloorNo       int    `json:"floorNo"` // post number within thread
	Title         string `json:"title"`
	PostURL       string `json:"postUrl"`
	Hosts         string `json:"hosts"` // "imagebam (41), imgbox (28)"
	Dir           string `json:"dir"`   // 最终下载目录（绝对/相对路径，由 pipeline 推导后回写缓存；不是用户覆盖值）
	// CustomName 用户在界面上（重命名）给这个条目起的名字；非空时下载目录名
	// 固定由它推导，不再随楼层号兜底成 F<n>——楼层标题常常是主题标题的
	// 变体（"Re: <主题>"），按标题自动推导会退化成 F<n>，改名就带动不了
	// 文件夹。见 pipeline.Engine.wantDir。
	CustomName string `json:"dirName"`
	// Forum 这条主题所属的板块（分类目录名）。单帖链接（没建 thread 行）
	// 同样要按板块分类，所以板块必须记在 post 自己身上，不能只靠 thread 表反查。
	Forum string `json:"forum"`
	// Mark 用户手动标记的颜色（空串=未标记）。只是列表上的视觉标记，不影响下载。
	Mark         string `json:"mark"`
	State        string `json:"status"`
	CreatedAt    string `json:"createdAt"`
	DownloadedAt string `json:"downloadedAt"`
	// derived (not stored)
	Done   int `json:"done"`
	Failed int `json:"failed"`
	Total  int `json:"total"`
}

type Thread struct {
	ID            int64  `json:"id"` // local row id (0 = keyed by threadId)
	ForumThreadID int64  `json:"threadId"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	Forum         string `json:"forum"` // 论坛板块名（Softcore Photo Sets 等），按板块分类用
	PostCount     int    `json:"postCount"`
	ImgCount      int    `json:"imgCount"`
	CreatedAt     string `json:"createdAt"`
	LastScannedAt int64  `json:"scanned"` // unix ts of last vr.php scan
	Mark          string `json:"mark"`    // 用户手动标记的颜色（空串=未标记）
}

type Image struct {
	ID           int64  `json:"id"`
	PostRowID    int64  `json:"postRowId"`
	Seq          int    `json:"seq"`
	PageURL      string `json:"pageUrl"`
	ThumbURL     string `json:"thumbUrl"`
	DirectURL    string `json:"directUrl"`
	OriginalName string `json:"originalName"`
	State        string `json:"state"`
	FileSize     int64  `json:"fileSize"`
	FilePath     string `json:"filePath"`
	ErrorMsg     string `json:"errorMsg"`
	DlMs         int64  `json:"dlMs"`
	Host         string `json:"host"`
}

type LogEntry struct {
	ID    int64  `json:"id"`
	Time  string `json:"time"`
	Level string `json:"level"` // INFO|WARN|ERROR
	Msg   string `json:"msg"`
}

type Store struct {
	db      *sql.DB
	bolt    *bolt.DB
	dataDir string

	bufMu  sync.Mutex
	buf    map[int64]imgPatch
	stopCh chan struct{}
	wg     sync.WaitGroup
}

type imgPatch struct {
	state string
	size  int64
	path  string
	err   string
	dlMs  int64
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "vripper.db")+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	bdb, err := bolt.Open(filepath.Join(dataDir, "thumbs.db"), 0o644, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		db.Close()
		if strings.Contains(err.Error(), "timeout") {
			return nil, fmt.Errorf("数据库被另一个实例占用——请先关闭已打开的 ViperRipper New 窗口再启动")
		}
		return nil, err
	}
	s := &Store{db: db, bolt: bdb, dataDir: dataDir, buf: map[int64]imgPatch{}, stopCh: make(chan struct{})}
	s.wg.Add(1)
	go s.flushLoop()
	return s, nil
}

func (s *Store) Close() error {
	close(s.stopCh)
	s.wg.Wait()
	err1 := s.db.Close()
	err2 := s.bolt.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *Store) DB() *sql.DB { return s.db }

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS thread(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			thread_id INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			forum TEXT NOT NULL DEFAULT '',
			post_count INTEGER NOT NULL DEFAULT 0,
			img_count INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			scanned INTEGER NOT NULL DEFAULT 0,
			mark TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_tid ON thread(thread_id)`,
		`CREATE TABLE IF NOT EXISTS post(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			thread_id INTEGER NOT NULL,
			post_id INTEGER NOT NULL,
			floor_no INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			post_url TEXT NOT NULL DEFAULT '',
			hosts TEXT NOT NULL DEFAULT '',
			dir TEXT NOT NULL DEFAULT '',
			forum TEXT NOT NULL DEFAULT '',
			mark TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'ready',
			created_at TEXT NOT NULL,
			downloaded_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_post_pid ON post(post_id)`,
		`CREATE INDEX IF NOT EXISTS idx_post_thread ON post(thread_id)`,
		`CREATE INDEX IF NOT EXISTS idx_post_status ON post(status)`,
		`CREATE TABLE IF NOT EXISTS image(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			post_row_id INTEGER NOT NULL REFERENCES post(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL DEFAULT 0,
			page_url TEXT NOT NULL DEFAULT '',
			thumb_url TEXT NOT NULL DEFAULT '',
			direct_url TEXT NOT NULL DEFAULT '',
			original_name TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'pending',
			file_size INTEGER NOT NULL DEFAULT 0,
			file_path TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT '',
			dl_ms INTEGER NOT NULL DEFAULT 0,
			host TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_image_post ON image(post_row_id)`,
		`CREATE INDEX IF NOT EXISTS idx_image_state ON image(state)`,
		`CREATE TABLE IF NOT EXISTS setting(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS log(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TEXT NOT NULL,
			level TEXT NOT NULL,
			msg TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_log_time ON log(time)`,
		`CREATE TABLE IF NOT EXISTS floor_cache(
			row_id INTEGER NOT NULL,
			post_id INTEGER NOT NULL,
			no INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			xml_title TEXT NOT NULL DEFAULT '',
			images TEXT NOT NULL DEFAULT '[]',
			PRIMARY KEY (row_id, post_id)
		)`,
		`CREATE TABLE IF NOT EXISTS floor_cache_meta(
			row_id INTEGER PRIMARY KEY,
			next_url TEXT NOT NULL DEFAULT '',
			done INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// 旧库升级：这一列是后加的（用户重命名要带动文件夹才有意义）
	if err := addColumn(db, "post", "dir_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 旧库升级：thread.forum（板块名，按板块分类下载目录用）
	if err := addColumn(db, "thread", "forum", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 旧库升级：post.forum（单帖链接也要按板块分类，见 Post.Forum）
	if err := addColumn(db, "post", "forum", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 旧库升级：mark（用户标记颜色，列表视觉标记）
	if err := addColumn(db, "post", "mark", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumn(db, "thread", "mark", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 老数据回填：有 thread 行的按主题表补板块（纯本地操作，不发请求）
	if _, err := db.Exec(`UPDATE post SET forum=COALESCE((SELECT NULLIF(forum,'') FROM thread WHERE thread.thread_id=post.thread_id),'')
		WHERE COALESCE(forum,'')=''`); err != nil {
		return fmt.Errorf("migrate: backfill post.forum: %w", err)
	}
	return nil
}

// addColumn 幂等地给表加列：列已存在时直接返回。
func addColumn(db *sql.DB, table, col, decl string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("migrate: read %s columns: %w", table, err)
	}
	have := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("migrate: scan %s columns: %w", table, err)
		}
		if strings.EqualFold(name, col) {
			have = true
			break
		}
	}
	rows.Close()
	if have {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, decl)); err != nil {
		return fmt.Errorf("migrate: add %s.%s: %w", table, col, err)
	}
	return nil
}

// ---------- threads ----------

func (s *Store) UpsertThread(t *Thread) error {
	// forum 用 COALESCE 保留旧值：某次抓取（如 HTML 兜底通道）解析不出板块时
	// 不冲掉数据库里已有的正确板块名，否则下载目录推导会变、触发文件自动搬迁
	_, err := s.db.Exec(`INSERT INTO thread(thread_id,title,url,forum,post_count,img_count,created_at,scanned)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(thread_id) DO UPDATE SET title=excluded.title, url=excluded.url,
		forum=COALESCE(NULLIF(excluded.forum,''), thread.forum),
		post_count=excluded.post_count, img_count=excluded.img_count, scanned=excluded.scanned`,
		t.ForumThreadID, t.Title, t.URL, t.Forum, t.PostCount, t.ImgCount, t.CreatedAt, t.LastScannedAt)
	return err
}

// FillThreadForum 只在库里还没记录板块名时补上（XML 通道能解析出板块，
// HTML 兜底通道拿不到）。空值不覆盖，避免把已有的正确值冲掉。
// 补上之后“按板块分类”的目录推导才会把楼层放进 <板块>/<主题> 里。
func (s *Store) FillThreadForum(forumThreadID int64, forum string) {
	if forum == "" || forumThreadID == 0 {
		return
	}
	s.db.Exec(`UPDATE thread SET forum=? WHERE thread_id=? AND COALESCE(forum,'')=''`, forum, forumThreadID)
}

// HasThreadRow reports whether a Thread row already exists for this forum
// thread id (used to flag re-added links as duplicates).
func (s *Store) HasThreadRow(forumThreadID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM thread WHERE thread_id=?`, forumThreadID).Scan(&n)
	return n > 0
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(`SELECT id,thread_id,title,url,forum,post_count,img_count,created_at,scanned,mark FROM thread ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.ForumThreadID, &t.Title, &t.URL, &t.Forum, &t.PostCount, &t.ImgCount, &t.CreatedAt, &t.LastScannedAt, &t.Mark); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteThread(rowID int64) error {
	_, err := s.db.Exec(`DELETE FROM thread WHERE id=?`, rowID)
	return err
}

// DeleteThreadsCascade 删除主题（thread 行），并连带删掉它下面的所有楼层与图片记录
// （楼层缓存也一起清）。返回被删除的楼层数。
//
// 楼层是按 forum thread_id 归属的，所以删主题 = 删这个 thread_id 下的全部楼层；
// 以前只删 thread 行，楼层就变成没有归属的“普通帖子”散落回列表。
func (s *Store) DeleteThreadsCascade(rowIDs []int64) (int, error) {
	if len(rowIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	rollback := func(e error) (int, error) {
		tx.Rollback()
		return 0, e
	}
	posts := 0
	for _, id := range rowIDs {
		var tid int64
		if err := tx.QueryRow(`SELECT thread_id FROM thread WHERE id=?`, id).Scan(&tid); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return rollback(err)
		}
		if _, err := tx.Exec(`DELETE FROM image WHERE post_row_id IN (SELECT id FROM post WHERE thread_id=?)`, tid); err != nil {
			return rollback(err)
		}
		res, err := tx.Exec(`DELETE FROM post WHERE thread_id=?`, tid)
		if err != nil {
			return rollback(err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			posts += int(n)
		}
		if _, err := tx.Exec(`DELETE FROM floor_cache WHERE row_id=?`, id); err != nil {
			return rollback(err)
		}
		if _, err := tx.Exec(`DELETE FROM floor_cache_meta WHERE row_id=?`, id); err != nil {
			return rollback(err)
		}
		if _, err := tx.Exec(`DELETE FROM thread WHERE id=?`, id); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return posts, nil
}

// ThreadForumOf 按论坛 thread id 取板块名（按板块分类下载目录用）；查不到为空。
func (s *Store) ThreadForumOf(forumThreadID int64) string {
	var f string
	s.db.QueryRow(`SELECT forum FROM thread WHERE thread_id=?`, forumThreadID).Scan(&f)
	return f
}

// ThreadTitleAndPostCount 按论坛 thread id 取主题标题和已收录的楼层数，用于推导下载目录。
// 查不到主题时 title 为空、floors 退回 post 表里的真实数量。
func (s *Store) ThreadTitleAndPostCount(forumThreadID int64) (title string, floors int) {
	_ = s.db.QueryRow(`SELECT title FROM thread WHERE thread_id=?`, forumThreadID).Scan(&title)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM post WHERE thread_id=?`, forumThreadID).Scan(&floors)
	return title, floors
}

// RewriteImagePathPrefix 把 file_path 以 oldPrefix 开头的记录改到 newPrefix，
// 目录整体搬迁后调用，免得数据库里还是旧路径。
func (s *Store) RewriteImagePathPrefix(oldPrefix, newPrefix string) (int, error) {
	if oldPrefix == "" || newPrefix == "" || oldPrefix == newPrefix {
		return 0, nil
	}
	rows, err := s.db.Query(`SELECT id, file_path FROM image WHERE file_path LIKE ? || '%'`, oldPrefix)
	if err != nil {
		return 0, err
	}
	type pair struct {
		id   int64
		path string
	}
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.path); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range todo {
		// 只替换前缀本身（按字符切，兼容中文/反斜杠），不动后面的相对路径
		if !strings.HasPrefix(p.path, oldPrefix) {
			continue
		}
		next := newPrefix + p.path[len(oldPrefix):]
		if _, err := s.db.Exec(`UPDATE image SET file_path=? WHERE id=?`, next, p.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------- posts ----------

func (s *Store) AddPost(p *Post) (int64, error) {
	if p.CreatedAt == "" {
		p.CreatedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO post(thread_id,post_id,floor_no,title,post_url,hosts,dir,dir_name,forum,mark,status,created_at,downloaded_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ForumThreadID, p.ForumPostID, p.FloorNo, p.Title, p.PostURL, p.Hosts, p.Dir, p.CustomName, p.Forum, p.Mark, p.State, p.CreatedAt, p.DownloadedAt)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil // already collected
	}
	return res.LastInsertId()
}

func (s *Store) ListPosts() ([]Post, error) {
	rows, err := s.db.Query(`SELECT p.id,p.thread_id,p.post_id,p.floor_no,p.title,p.post_url,p.hosts,p.dir,p.dir_name,p.forum,p.mark,p.status,p.created_at,p.downloaded_at,
		COALESCE(SUM(CASE WHEN i.state='done' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN i.state='error' THEN 1 ELSE 0 END),0),
		COUNT(i.id)
		FROM post p LEFT JOIN image i ON i.post_row_id=p.id
		GROUP BY p.id ORDER BY p.created_at DESC, p.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.ForumThreadID, &p.ForumPostID, &p.FloorNo, &p.Title, &p.PostURL, &p.Hosts, &p.Dir, &p.CustomName, &p.Forum, &p.Mark, &p.State, &p.CreatedAt, &p.DownloadedAt,
			&p.Done, &p.Failed, &p.Total); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPost(rowID int64) (*Post, error) {
	var p Post
	err := s.db.QueryRow(`SELECT id,thread_id,post_id,floor_no,title,post_url,hosts,dir,dir_name,forum,mark,status,created_at,downloaded_at FROM post WHERE id=?`, rowID).
		Scan(&p.ID, &p.ForumThreadID, &p.ForumPostID, &p.FloorNo, &p.Title, &p.PostURL, &p.Hosts, &p.Dir, &p.CustomName, &p.Forum, &p.Mark, &p.State, &p.CreatedAt, &p.DownloadedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// RenamePost 重命名条目（Thread 下的楼层行、普通帖子都可以），同时把用户
// 给的名字记进 dir_name——此后下载目录名就固定跟着这个名字走（见
// pipeline.Engine.wantDir），不会因“楼层标题像主题标题”被兜底成 F<n>。
func (s *Store) RenamePost(rowID int64, title string) error {
	_, err := s.db.Exec(`UPDATE post SET title=?, dir_name=? WHERE id=?`, title, title, rowID)
	return err
}

// SetPostDirName 单独设置/清空条目的目录名（清空后回到按标题自动推导）。
// 传空串表示“恢复自动命名”。
func (s *Store) SetPostDirName(rowID int64, name string) error {
	_, err := s.db.Exec(`UPDATE post SET dir_name=? WHERE id=?`, name, rowID)
	return err
}

// ClearPostDirsByStatus 把还没开始下载的帖子的目录清空，让它们按新设置重新推导
// （改“每帖一个文件夹/楼层子文件夹”后，未下载的帖子应立刻跟随新布局）。
func (s *Store) ClearPostDirsByStatus(status string) (int, error) {
	res, err := s.db.Exec(`UPDATE post SET dir='' WHERE status=?`, status)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) SetPostDir(rowID int64, dir string) error {
	_, err := s.db.Exec(`UPDATE post SET dir=? WHERE id=?`, dir, rowID)
	return err
}

// SetPostForum 记录条目所属板块（按板块分类下载目录用）。单帖链接没有
// thread 行，板块只能存在 post 上。
func (s *Store) SetPostForum(rowID int64, forum string) error {
	_, err := s.db.Exec(`UPDATE post SET forum=? WHERE id=?`, forum, rowID)
	return err
}

// PostForum 取条目记录的板块；没有记录时返回空串。
func (s *Store) PostForum(postRowID int64) string {
	var f string
	_ = s.db.QueryRow(`SELECT COALESCE(forum,'') FROM post WHERE id=?`, postRowID).Scan(&f)
	return f
}

// SetPostMark 批量设置条目的标记颜色（空串 = 清除标记）。纯视觉标记。
func (s *Store) SetPostMark(ids []int64, mark string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE post SET mark=? WHERE id=?`, mark, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SetThreadMark 设置主题（thread 行）的标记颜色（空串 = 清除标记）。
func (s *Store) SetThreadMark(rowID int64, mark string) error {
	_, err := s.db.Exec(`UPDATE thread SET mark=? WHERE id=?`, mark, rowID)
	return err
}

func (s *Store) SetPostStatus(rowID int64, status string) error {
	dl := ""
	if status == "done" {
		dl = time.Now().Format("2006-01-02 15:04:05")
	}
	_, err := s.db.Exec(`UPDATE post SET status=?, downloaded_at=CASE WHEN ?<>'' THEN ? ELSE downloaded_at END WHERE id=?`, status, dl, dl, rowID)
	return err
}

// DeletePostImages deletes all image rows of one post (the post row itself
// is kept) — used by the pre-download consistency check to rebuild the list.
func (s *Store) DeletePostImages(postRowID int64) error {
	_, err := s.db.Exec(`DELETE FROM image WHERE post_row_id=?`, postRowID)
	return err
}

// ImagePathClaimed 该文件路径是否已经被同一帖子的另一条图片记录占用。
// 用来避免两张不同的图重名时，后来者把前者的文件“认领”成自己的（那样
// 两条记录共用同一个文件，真实的那张图就永远丢了）。
func (s *Store) ImagePathClaimed(postRowID int64, path string, excludeImgID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM image WHERE post_row_id=? AND id<>? AND file_path=? AND state='done'`,
		postRowID, excludeImgID, path).Scan(&n)
	return n > 0
}

// ResetDuplicateImageFiles 同一帖子内多条已完成记录指向同一个文件时，除第一条
// 之外的记录退回待下载（清掉路径），让它们重新下载到自己的唯一文件名上。
// 返回被退回的记录条数。以前遇到这种情况会整帖重建图片列表，导致“已经下完的
// 帖子一点开始下载就全部重下”。
func (s *Store) ResetDuplicateImageFiles(postRowID int64) (int, error) {
	res, err := s.db.Exec(`UPDATE image SET state='pending', file_path='', file_size=0
		WHERE post_row_id=? AND state='done' AND file_path<>'' AND id NOT IN (
			SELECT MIN(id) FROM image WHERE post_row_id=? AND state='done' AND file_path<>'' GROUP BY file_path
		)`, postRowID, postRowID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ClearImageCache 清空一批帖子的解析缓存（直链 + 原文件名），并把失败行
// 重置为待下载。已下载的 done 行不受影响（磁盘文件保留，续传照常跳过）。
// 图床改版或曾被坏规则解析过的旧数据，用这个代替"删掉重加"。
func (s *Store) ClearImageCache(ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := s.db.Exec(`UPDATE image SET direct_url='', original_name='' WHERE post_row_id IN (`+ph+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	s.db.Exec(`UPDATE image SET state='pending' WHERE state='error' AND post_row_id IN (`+ph+`)`, args...)
	return n, nil
}

func (s *Store) DeletePostsByIDs(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM image WHERE post_row_id=?`, id); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM post WHERE id=?`, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeletePostsByStatus 按状态删除条目（连带其图片行）；status: done | error | all
func (s *Store) DeletePostsByStatus(status string) error {
	var cond string
	switch status {
	case "done":
		cond = `WHERE status='done'`
	case "error":
		cond = `WHERE status='error'`
	default:
		cond = ""
	}
	_, err := s.db.Exec(`DELETE FROM image WHERE post_row_id IN (SELECT id FROM post ` + cond + `)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM post ` + cond)
	return err
}

// ---------- images ----------

func (s *Store) AddImage(img *Image) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO image(post_row_id,seq,page_url,thumb_url,direct_url,original_name,state,file_size,file_path,error_msg,dl_ms,host)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		img.PostRowID, img.Seq, img.PageURL, img.ThumbURL, img.DirectURL, img.OriginalName, img.State,
		img.FileSize, img.FilePath, img.ErrorMsg, img.DlMs, img.Host)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	img.ID = id
	return id, nil
}

func (s *Store) ListImages(postRowID int64) ([]Image, error) {
	rows, err := s.db.Query(`SELECT id,post_row_id,seq,page_url,thumb_url,direct_url,original_name,state,file_size,file_path,error_msg,dl_ms,host
		FROM image WHERE post_row_id=? ORDER BY seq`, postRowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var i Image
		if err := rows.Scan(&i.ID, &i.PostRowID, &i.Seq, &i.PageURL, &i.ThumbURL, &i.DirectURL, &i.OriginalName, &i.State,
			&i.FileSize, &i.FilePath, &i.ErrorMsg, &i.DlMs, &i.Host); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) GetImage(id int64) (*Image, error) {
	var i Image
	err := s.db.QueryRow(`SELECT id,post_row_id,seq,page_url,thumb_url,direct_url,original_name,state,file_size,file_path,error_msg,dl_ms,host
		FROM image WHERE id=?`, id).
		Scan(&i.ID, &i.PostRowID, &i.Seq, &i.PageURL, &i.ThumbURL, &i.DirectURL, &i.OriginalName, &i.State,
			&i.FileSize, &i.FilePath, &i.ErrorMsg, &i.DlMs, &i.Host)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// ExecImageUpdate stores a direct URL and optional original name immediately
// (pre-resolve results, not progress — safe outside the batch writer).
func (s *Store) ExecImageUpdate(id int64, directURL, origName string) {
	if origName != "" {
		s.db.Exec(`UPDATE image SET direct_url=?, original_name=? WHERE id=?`, directURL, origName, id)
	} else {
		s.db.Exec(`UPDATE image SET direct_url=? WHERE id=?`, directURL, id)
	}
}

// QueueImageState queues a buffered state update (200ms batch write, REDESIGN §3).
func (s *Store) QueueImageState(id int64, state string, size int64, path, errMsg string, dlMs int64) {
	s.bufMu.Lock()
	s.buf[id] = imgPatch{state: state, size: size, path: path, err: errMsg, dlMs: dlMs}
	s.bufMu.Unlock()
}

func (s *Store) flushLoop() {
	defer s.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			s.flush()
			return
		case <-t.C:
			s.flush()
		}
	}
}

// Flush writes any buffered image-state updates immediately (used before
// reading back final states, e.g. a post's completion tally).
func (s *Store) Flush() { s.flush() }

func (s *Store) flush() {
	s.bufMu.Lock()
	if len(s.buf) == 0 {
		s.bufMu.Unlock()
		return
	}
	batch := s.buf
	s.buf = make(map[int64]imgPatch)
	s.bufMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	for id, p := range batch {
		if p.state == "done" {
			tx.Exec(`UPDATE image SET state=?, file_size=?, file_path=?, error_msg='', dl_ms=? WHERE id=?`,
				p.state, p.size, p.path, p.dlMs, id)
		} else if p.state == "error" {
			// 失败/重排的图不该再挂着上一次的文件路径与大小，否则界面和
			// “跳过已存在文件”的续传判断会读到过期信息。
			tx.Exec(`UPDATE image SET state=?, error_msg=?, file_size=0, file_path='' WHERE id=?`, p.state, p.err, id)
		} else {
			tx.Exec(`UPDATE image SET state=?, file_size=0, file_path='' WHERE id=?`, p.state, id)
		}
	}
	tx.Commit()
}

// FirstDoneImageDir 返回该帖第一张已完成图片所在的目录（用于从图片旧路径
// 反推帖子的实际下载位置），找不到已完成图片时 ok=false。
func (s *Store) FirstDoneImageDir(postRowID int64) (dir string, ok bool) {
	var fp string
	err := s.db.QueryRow(`SELECT file_path FROM image
		WHERE post_row_id=? AND state='done' AND file_path<>'' ORDER BY seq LIMIT 1`, postRowID).Scan(&fp)
	if err != nil || fp == "" {
		return "", false
	}
	return filepath.Dir(fp), true
}

// RebaseImagePaths 把一个帖子下指向失效位置（目录搬过家、数据从旧版导入）
// 的已完成图片路径重写到当前帖子目录下。只按文件名重定位，磁盘上也真的
// 找得到才改；找不到就保留原路径不动。返回修复条数。
func (s *Store) RebaseImagePaths(postRowID int64, dir string) int {
	rows, err := s.db.Query(`SELECT id, file_path FROM image
		WHERE post_row_id=? AND state='done' AND file_path<>''`, postRowID)
	if err != nil {
		return 0
	}
	type fix struct {
		id   int64
		next string
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var fp string
		if rows.Scan(&id, &fp) != nil {
			continue
		}
		if _, err := os.Stat(fp); err == nil {
			continue // 原路径还好好的
		}
		cand := filepath.Join(dir, filepath.Base(fp))
		if st, err := os.Stat(cand); err != nil || st.IsDir() {
			continue // 新位置也找不到，不动
		}
		fixes = append(fixes, fix{id, cand})
	}
	rows.Close()
	n := 0
	for _, f := range fixes {
		if _, err := s.db.Exec(`UPDATE image SET file_path=? WHERE id=?`, f.next, f.id); err == nil {
			n++
		}
	}
	return n
}

// ResetDownloading marks all downloading images/posts as pending/ready at startup.
func (s *Store) ResetDownloading() {
	s.db.Exec(`UPDATE image SET state='pending' WHERE state='downloading'`)
	// paused is real progress - keep it across restarts; only downloading
	// (an interrupted run) is reset so no post looks stuck
	s.db.Exec(`UPDATE post SET status='ready' WHERE status='downloading'`)
}

// ---------- key-value settings (login cookies etc.) ----------

func (s *Store) GetKV(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM setting WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetKV(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO setting(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------- logs ----------

func (s *Store) AddLog(level, msg string) {
	s.db.Exec(`INSERT INTO log(time,level,msg) VALUES(?,?,?)`,
		time.Now().Format("15:04:05"), level, msg)
}

func (s *Store) ListLogs(limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id,time,level,msg FROM log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.ID, &l.Time, &l.Level, &l.Msg); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------- thumbs (bbolt) ----------

func (s *Store) PutThumb(collection, key string, data []byte) error {
	return s.bolt.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(collection))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
}

func (s *Store) GetThumb(collection, key string) ([]byte, error) {
	var out []byte
	err := s.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(collection))
		if b == nil {
			return nil
		}
		out = append([]byte(nil), b.Get([]byte(key))...)
		return nil
	})
	return out, err
}

// ---------- export / import ----------

func (s *Store) ExportAll() (map[string]any, error) {
	out := map[string]any{}
	threads, err := s.ListThreads()
	if err != nil {
		return nil, err
	}
	posts, err := s.ListPosts()
	if err != nil {
		return nil, err
	}
	postImgs := map[int64][]Image{}
	for _, p := range posts {
		imgs, err := s.ListImages(p.ID)
		if err != nil {
			return nil, err
		}
		postImgs[p.ID] = imgs
	}
	out["threads"] = threads
	out["posts"] = posts
	out["images"] = postImgs
	return out, nil
}

func (s *Store) ImportThreadRows(data []byte) (int, error) {
	var threads []Thread
	if err := json.Unmarshal(data, &threads); err != nil {
		return 0, err
	}
	n := 0
	for i := range threads {
		if threads[i].ForumThreadID == 0 || threads[i].CreatedAt == "" {
			continue
		}
		if err := s.UpsertThread(&threads[i]); err == nil {
			n++
		}
	}
	return n, nil
}

// ImportPosts restores Download rows plus their images from an export.
// Posts are keyed by forum post_id (unique index), so re-importing is
// idempotent. Image rows are keyed to the local post row id, which differs
// between machines — images[exportRowID] is remapped onto the actual row id.
// After import, any half-active "downloading" state is normalized the same
// way ResetDownloading does at startup.
func (s *Store) ImportPosts(postsData, imagesData []byte) (int, error) {
	var posts []Post
	if err := json.Unmarshal(postsData, &posts); err != nil {
		return 0, err
	}
	imgs := map[int64][]Image{}
	if len(imagesData) > 0 {
		if err := json.Unmarshal(imagesData, &imgs); err != nil {
			return 0, err
		}
	}
	n := 0
	for i := range posts {
		p := &posts[i]
		if p.ForumPostID == 0 {
			continue
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO post(thread_id,post_id,floor_no,title,post_url,hosts,dir,dir_name,status,created_at,downloaded_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			p.ForumThreadID, p.ForumPostID, p.FloorNo, p.Title, p.PostURL, p.Hosts, p.Dir, p.CustomName, p.State, p.CreatedAt, p.DownloadedAt); err != nil {
			continue
		}
		var rowID int64
		if err := s.db.QueryRow(`SELECT id FROM post WHERE post_id=?`, p.ForumPostID).Scan(&rowID); err != nil {
			continue
		}
		n++
		list := imgs[p.ID] // p.ID = the exporting machine's post row id
		if _, err := s.db.Exec(`DELETE FROM image WHERE post_row_id=?`, rowID); err != nil {
			continue
		}
		for _, im := range list {
			im.ID = 0
			im.PostRowID = rowID
			s.db.Exec(`INSERT INTO image(post_row_id,seq,page_url,thumb_url,direct_url,original_name,state,file_size,file_path,error_msg,dl_ms,host)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
				im.PostRowID, im.Seq, im.PageURL, im.ThumbURL, im.DirectURL, im.OriginalName, im.State,
				im.FileSize, im.FilePath, im.ErrorMsg, im.DlMs, im.Host)
		}
	}
	// 与启动时的 ResetDownloading 一致：中断态不保留，paused 是真实进度保留
	s.db.Exec(`UPDATE image SET state='pending' WHERE state='downloading'`)
	s.db.Exec(`UPDATE post SET status='ready' WHERE status='downloading'`)
	return n, nil
}
