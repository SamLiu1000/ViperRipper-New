//go:build windows

// logintest drives the out-of-process web-login flow: default against a
// local fake site, or `logintest --live <host> <url>` against the real forum
// to verify the injected script actually reports cookies from it.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"viperipper/internal/loginwin"
)

func main() {
	if len(os.Args) > 6 && os.Args[1] == "--child" {
		os.Exit(loginwin.RunStandalone(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6], ""))
	}
	self, _ := os.Executable()
	resultFile := filepath.Join(os.TempDir(), "logintest-result.json")
	os.Remove(resultFile)

	host, target, profile := "127.0.0.1:8931", "http://127.0.0.1:8931/set", filepath.Join(os.TempDir(), "logintest-profile")
	if len(os.Args) > 3 && os.Args[1] == "--live" {
		host, target = os.Args[2], os.Args[3]
		profile = filepath.Join(os.TempDir(), "logintest-live-profile")
		fmt.Println("LIVE test:", target)
	} else {
		mux := http.NewServeMux()
		mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
			// HttpOnly + session (no MaxAge): mirrors vBulletin's login cookie
			http.SetCookie(w, &http.Cookie{Name: "vg_sessionhash", Value: "abc123", Path: "/", HttpOnly: true})
			fmt.Fprint(w, "<html><body><h1>fake forum</h1></body></html>")
		})
		go http.ListenAndServe("127.0.0.1:8931", mux)
		time.Sleep(300 * time.Millisecond)
	}

	cmd := exec.Command(self, "--child", profile, host, target, "", resultFile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Println("start failed:", err)
		os.Exit(1)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-exited:
			fmt.Println("child exited before cookies")
			os.Exit(2)
		case <-deadline:
			fmt.Println("TIMEOUT - no cookie batch reported")
			cmd.Process.Kill()
			os.Exit(3)
		case <-time.After(400 * time.Millisecond):
			raw, err := os.ReadFile(resultFile)
			if err != nil || len(raw) == 0 {
				continue
			}
			p, ok := loginwin.ParsePayload(raw)
			if !ok {
				continue
			}
			for _, c := range p.Cookies {
				fmt.Printf("  %s=%s\n", c.Name, c.Value)
			}
			fmt.Println("OK, harvested", len(p.Cookies), "cookies; UA =", p.UserAgent)
			cmd.Process.Kill()
			os.Exit(0)
		}
	}
}
