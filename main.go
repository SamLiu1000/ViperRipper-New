package main

import (
	"embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"viperipper/internal/loginwin"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// portable data dir: alongside the exe, except in dev mode (go-run/wails-dev
	// builds land in a temp dir) where the project cwd is used
	exeDir, err := os.Executable()
	if err != nil {
		exeDir, _ = os.Getwd()
	}
	dataDir := filepath.Join(filepath.Dir(exeDir), "data")
	exeLower := strings.ToLower(filepath.Dir(exeDir))
	if strings.Contains(exeLower, "temp") || strings.Contains(exeLower, "go-build") {
		dataDir = filepath.Join(".", "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create data dir:", err)
		os.Exit(1)
	}

	// child-process mode: the web-login / human-check window runs in its own
	// process so any WebView2 failure there can never take the main app down (闪退隔离)
	if len(os.Args) > 6 && os.Args[1] == "--weblogin" {
		os.Exit(loginwin.RunStandalone(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6], ""))
	}
	if len(os.Args) > 6 && os.Args[1] == "--webverify" {
		os.Exit(loginwin.RunStandalone(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6],
			"人机验证 — 点完“确认是人类”后关闭本窗口"))
	}

	t0 := time.Now()
	app, err := NewApp(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		os.Exit(1)
	}
	// startup timing: process start → wails.Run (NewApp is async now, so
	// this measures process boot + window creation handoff only)
	app.startupMark(fmt.Sprintf("[startup] NewApp 返回 %dms；即将创建窗口", time.Since(t0).Milliseconds()))

	err = wails.Run(&options.App{
		Title:     "ViperRipper New",
		Width:     1280,
		Height:    860,
		MinWidth:  980,
		MinHeight: 620,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: http.HandlerFunc(app.serveImage),
		},
		BackgroundColour: &options.RGBA{R: 238, G: 238, B: 238, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		// 关窗拦截：下载中弹确认框（收进任务栏继续下载 / 强制退出 / 取消），不静默退出
		OnBeforeClose: app.beforeClose,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			WebviewUserDataPath:  filepath.Join(dataDir, "webview"),
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
