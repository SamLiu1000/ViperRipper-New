package settings

import (
	"database/sql"
	"encoding/json"
	"sync"
)

// Settings mirrors REDESIGN.md §6 (four pages, minus the removed host page).
type Settings struct {
	// General
	ExistingFiles  string `json:"existingFiles"` // skip | overwrite | rename
	PerPostFolder  bool   `json:"perPostFolder"`
	ShowPreview    bool   `json:"showPreview"`
	ConfirmOnClose bool   `json:"minimizeTray"`
	SoundOnDone    bool   `json:"soundOnDone"`
	ShutdownOnDone bool   `json:"shutdownOnDone"`
	// Download
	DownloadDir     string `json:"downloadDir"`
	TempDir         string `json:"tempDir"`
	HostsDir        string `json:"hostsDir"` // host 规则目录；空 = <data>/hosts
	PostsConcurrent int    `json:"postsConcurrent"`
	HostConcurrent  int    `json:"hostConcurrent"`
	MaxRetries      int    `json:"maxRetries"`
	TimeoutSec      int    `json:"timeoutSec"`
	FetchOrigName   bool   `json:"fetchOrigName"`
	FetchFloorTitle bool   `json:"fetchFloorTitle"`
	SortByForum     bool   `json:"sortByForum"` // 下载目录按论坛板块（Softcore Photo Sets 等）分类
	// Connection
	ForumDomain  string `json:"forumDomain"`
	Mirrors      string `json:"mirrors"`
	RateLimit    int    `json:"rateLimit"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	AutoLogin    bool   `json:"autoLogin"`
	AutoThank    bool   `json:"autoThank"`
	Proxy        string `json:"proxy"`        // host:port, empty = auto-detect system proxy
	ProxyEnabled bool   `json:"proxyEnabled"` // toggle: use the manual proxy above; off = system proxy auto-detect
	// 浏览器扩展桥接：只监听 127.0.0.1，供规则录制器保存规则用
	ExtBridgeEnabled bool `json:"extBridgeEnabled"`
	ExtBridgePort    int  `json:"extBridgePort"` // 0 = 自动（47821 起顺延）
	// 主窗口位置与尺寸持久化；0 = 用程序默认
	WinX int `json:"winX"`
	WinY int `json:"winY"`
	WinW int `json:"winW"`
	WinH int `json:"winH"`
	// Download 列表的排序（表头点出来的列号与方向），-1 = 默认按添加日期。
	// 普通列表和“主题管理”里的楼层列表各记一份，重启后分别保持。
	SortCol       int `json:"sortCol"`
	SortDir       int `json:"sortDir"`
	ThreadSortCol int `json:"threadSortCol"` // 主题管理视图（楼层列表）
	ThreadSortDir int `json:"threadSortDir"`
}

func Defaults() *Settings {
	return &Settings{
		ExistingFiles:    "skip",
		PerPostFolder:    true,
		ShowPreview:      true,
		ConfirmOnClose:   true,
		SoundOnDone:      false,
		ShutdownOnDone:   false,
		DownloadDir:      "",
		TempDir:          "",
		PostsConcurrent:  2,
		HostConcurrent:   5, // 每图床并发默认（原版 vripper 各 host 为 5~8）
		MaxRetries:       3,
		TimeoutSec:       10,
		FetchOrigName:    false, // 与原版 vripper 默认一致：不为文件名访问页面（要真名在设置里开）
		FetchFloorTitle:  true,
		ForumDomain:      "vipergirls.to",
		Mirrors:          "", // 论坛备用域名；留空 = 只用主域名
		ProxyEnabled:     false,
		RateLimit:        2,
		AutoLogin:        false,
		AutoThank:        false,
		ExtBridgeEnabled: true,
		ExtBridgePort:    47821,
		SortCol:          -1, // 默认：按添加日期（倒序）
		SortDir:          1,
		ThreadSortCol:    -1, // 主题管理视图默认：按楼层号
		ThreadSortDir:    1,
	}
}

type Manager struct {
	mu   sync.RWMutex
	cur  *Settings
	db   *sql.DB
	once sync.Once
}

func New(db *sql.DB) *Manager {
	m := &Manager{cur: Defaults(), db: db}
	m.load()
	return m
}

func (m *Manager) load() {
	var raw string
	err := m.db.QueryRow(`SELECT value FROM setting WHERE key='app'`).Scan(&raw)
	if err == nil && raw != "" {
		s := Defaults()
		if json.Unmarshal([]byte(raw), s) == nil {
			if s.PostsConcurrent < 1 {
				s.PostsConcurrent = 1
			}
			if s.HostConcurrent < 1 {
				s.HostConcurrent = 1
			}
			if s.MaxRetries < 1 {
				s.MaxRetries = 1
			}
			m.cur = s
		}
	}
}

func (m *Manager) Get() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return *m.cur
}

func (m *Manager) Set(s Settings) error {
	if s.PostsConcurrent < 1 {
		s.PostsConcurrent = 1
	}
	if s.HostConcurrent < 1 {
		s.HostConcurrent = 1
	}
	if s.MaxRetries < 1 {
		s.MaxRetries = 1
	}
	if s.TimeoutSec < 1 {
		s.TimeoutSec = 10
	}
	if s.RateLimit < 1 {
		s.RateLimit = 1
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err = m.db.Exec(`INSERT INTO setting(key,value) VALUES('app',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(raw))
	if err != nil {
		return err
	}
	m.cur = &s
	return nil
}
