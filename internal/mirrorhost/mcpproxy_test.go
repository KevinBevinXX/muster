package mirrorhost

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	proxySecret = "local-secret-0123456789abcdef0123456789abcdef"
	proxyToken  = "mmt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	proxySID    = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
)

// paneWindows is a WindowLister with one Claude pane (%7 → proxySID) and one
// shell pane (%8).
type paneWindows struct{}

func (paneWindows) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID}},
		{Session: "work", ID: "@2", Pane: "%8"},
	}, nil
}

// upstream records what muster was sent and answers with body.
type upstream struct {
	mu    sync.Mutex
	calls []*http.Request
	body  []string
}

func newUpstream(t *testing.T, answer func(w http.ResponseWriter)) (*upstream, *httptest.Server) {
	t.Helper()
	u := &upstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls = append(u.calls, r.Clone(context.Background()))
		u.body = append(u.body, string(b))
		u.mu.Unlock()
		answer(w)
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

func (u *upstream) n() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func proxyFor(base string) *MCPProxy {
	return &MCPProxy{URL: base, Token: proxyToken, AllowHTTP: true, Secret: proxySecret, Windows: paneWindows{}}
}

func post(t *testing.T, h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestTheMCPEndpointListensOnLoopbackOnly: a non-loopback (or non-literal)
// address is refused; 127.0.0.1 is served.
func TestTheMCPEndpointListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.10:0", "localhost:0", "[::]:0", "example.com:0"} {
		if l, err := ListenMCP(addr); err == nil {
			l.Close()
			t.Errorf("%s was accepted", addr)
		} else if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "-mcp-listen") {
			t.Errorf("%s: %v", addr, err)
		}
	}
	l, err := ListenMCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("listening on %v", ip)
	}
}

// TestTheMCPEndpointRefusesWithoutTheSecret: no secret, a wrong one, or a
// browser Origin — refused, and muster is never called.
func TestTheMCPEndpointRefusesWithoutTheSecret(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) })
	p := proxyFor(srv.URL)
	for name, c := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"no secret":      {map[string]string{MCPPaneHeader: "%7"}, http.StatusUnauthorized},
		"wrong secret":   {map[string]string{"Authorization": "Bearer " + proxySecret + "x", MCPPaneHeader: "%7"}, http.StatusUnauthorized},
		"the mirror tok": {map[string]string{"Authorization": "Bearer " + proxyToken, MCPPaneHeader: "%7"}, http.StatusUnauthorized},
		"an Origin":      {map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", "Origin": "http://evil.example"}, http.StatusForbidden},
	} {
		if w := post(t, p, c.hdr); w.Code != c.want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if up.n() != 0 {
		t.Fatalf("muster was called %d time(s)", up.n())
	}
	// An empty configured secret refuses everyone, an empty presented one included.
	p.Secret = ""
	if w := post(t, p, map[string]string{"Authorization": "Bearer ", MCPPaneHeader: "%7"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret: %d", w.Code)
	}
}

// TestTheMCPEndpointForwardsTheCallersIdentity: the pane resolves to its
// Claude session, which goes to muster with the HOST's token and the body
// unchanged; muster's answer comes back as it was.
func TestTheMCPEndpointForwardsTheCallersIdentity(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	})
	p := proxyFor(srv.URL + "/base/")
	w := post(t, p, map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7"})
	if w.Code != http.StatusTeapot || w.Body.String() != `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if up.n() != 1 {
		t.Fatalf("%d calls", up.n())
	}
	r := up.calls[0]
	if r.URL.Path != "/base/mirror/mcp" || r.Header.Get("Authorization") != "Bearer "+proxyToken || r.Header.Get(MCPSessionHeader) != proxySID ||
		up.body[0] != `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` {
		t.Fatalf("forwarded %s %q %q %s", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(MCPSessionHeader), up.body[0])
	}
	// The local secret never leaves the machine.
	if strings.Contains(r.Header.Get("Authorization"), proxySecret) {
		t.Fatal("the local secret was forwarded")
	}
	// A session header is forwarded as given; a pane with no Claude, a bad id
	// and no name at all are refused here.
	if w := post(t, p, map[string]string{"Authorization": "Bearer " + proxySecret, MCPSessionHeader: proxySID}); w.Code != http.StatusTeapot {
		t.Fatalf("session header: %d", w.Code)
	}
	for name, hdr := range map[string]map[string]string{
		"a shell pane":    {MCPPaneHeader: "%8"},
		"an unknown pane": {MCPPaneHeader: "%99"},
		"a bad id":        {MCPSessionHeader: "../x"},
		"no name":         {},
	} {
		hdr["Authorization"] = "Bearer " + proxySecret
		if w := post(t, p, hdr); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if up.n() != 2 {
		t.Fatalf("%d calls after the refusals, want 2", up.n())
	}
}

// TestTheMCPEndpointPassesAKeepAliveThrough: muster's keep-alive newline
// reaches the caller before muster's answer does.
func TestTheMCPEndpointPassesAKeepAliveThrough(t *testing.T) {
	release := make(chan struct{})
	_, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
	local := httptest.NewServer(proxyFor(srv.URL))
	defer local.Close()
	defer close(release)
	// The request and the first read run off the test's goroutine: without the
	// keep-alive passing through, even the response headers never arrive.
	got := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, local.URL+"/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+proxySecret)
		req.Header.Set(MCPPaneHeader, "%7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := bufio.NewReader(resp.Body).ReadByte()
		got <- string(b)
	}()
	select {
	case b := <-got:
		if b != "\n" {
			t.Fatalf("first byte %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the keep-alive did not reach the caller while muster was still answering")
	}
}

func TestTheMCPSecretIsCreatedPrivateAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "muster-mirror", "mcp-secret")
	s1, err := EnsureMCPSecret(path)
	if err != nil || len(s1) != 64 {
		t.Fatalf("%q %v", s1, err)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	if s2, err := EnsureMCPSecret(path); err != nil || s2 != s1 {
		t.Fatalf("second read %q %v", s2, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMCPSecret(path); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a world-readable secret: %v", err)
	}
}
