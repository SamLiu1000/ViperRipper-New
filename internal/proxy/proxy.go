package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/sys/windows/registry"
)

// Func returns a http.Proxy function that honours environment variables
// first and then the Windows system proxy (WinINET settings). The WebView2
// frontend automatically uses the system proxy — without this the Go
// backend would bypass it and time out on blocked hosts.
var mu sync.Mutex
var manual *url.URL

// SetManual sets an explicit proxy ("host:port", empty disables). It wins
// over everything — useful when a local proxy client runs without the
// system-proxy toggle enabled.
func SetManual(addr string) {
	mu.Lock()
	defer mu.Unlock()
	if strings.TrimSpace(addr) == "" {
		manual = nil
		return
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	manual, _ = url.Parse(addr)
}

func Func() func(*http.Request) (*url.URL, error) {
	sys := systemProxyURL()
	return func(req *http.Request) (*url.URL, error) {
		mu.Lock()
		m := manual
		mu.Unlock()
		if m != nil {
			return m, nil
		}
		if u, err := http.ProxyFromEnvironment(req); err == nil && u != nil {
			return u, nil
		}
		return sys, nil
	}
}

// Snapshot captures the proxy configuration as of the call — the manual
// proxy if it is set at this moment (frozen), with the environment/system
// proxy resolved dynamically — so a long-running download job is immune to
// later SetManual switches (proxy toggle flipped mid-download).
func Snapshot() func(*http.Request) (*url.URL, error) {
	mu.Lock()
	m := manual
	mu.Unlock()
	sys := systemProxyURL()
	return func(req *http.Request) (*url.URL, error) {
		if m != nil {
			return m, nil
		}
		if u, err := http.ProxyFromEnvironment(req); err == nil && u != nil {
			return u, nil
		}
		return sys, nil
	}
}

func systemProxyURL() *url.URL {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return nil
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		return nil
	}
	// ProxyServer may be "host:port" or protocol-scoped "http=host:port;https=..."
	if !strings.Contains(server, "=") {
		return parseProxy(server)
	}
	for _, part := range strings.Split(server, ";") {
		if strings.HasPrefix(strings.ToLower(part), "http=") {
			return parseProxy(strings.TrimPrefix(part, "http="))
		}
	}
	return nil
}

func parseProxy(server string) *url.URL {
	if server == "" {
		return nil
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil
	}
	return u
}
