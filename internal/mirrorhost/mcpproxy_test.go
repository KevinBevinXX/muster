package mirrorhost

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/ZacxDev/muster/internal/mirror"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	proxySecret = "local-secret-0123456789abcdef0123456789abcdef"
	proxyToken  = "mmt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	proxySID    = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	splitSID    = "1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c"
	proxySocket = "/run/user/1000/tmux-1000/default"
	// proxyTMUX is $TMUX in a pane of that server.
	proxyTMUX = proxySocket + ",4242,0"
)

// fakeSocket is a ServerSocketer naming proxySocket.
type fakeSocket struct{}

func (fakeSocket) ServerSocket(context.Context) (string, error) { return proxySocket, nil }

// paneWindows is a WindowLister with one Claude pane (%7 → proxySID, the
// window's active pane, with a split whose %9 runs splitSID) and one shell
// pane (%8).
type paneWindows struct{}

func (paneWindows) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID}, Others: []ClaudePane{{Pane: "%9", Gen: splitSID}}},
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
	return &MCPProxy{URL: base, Token: proxyToken, AllowHTTP: true, Secret: proxySecret, Windows: paneWindows{}, Socket: fakeSocket{}}
}

func post(t *testing.T, h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return postBody(t, h, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
}

// proxyCall is a tools/call body.
const proxyCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"attention_ask","arguments":{"question":"q?"}}}`

func postBody(t *testing.T, h http.Handler, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
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
		"no secret":      {map[string]string{MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}, http.StatusUnauthorized},
		"wrong secret":   {map[string]string{"Authorization": "Bearer " + proxySecret + "x", MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}, http.StatusUnauthorized},
		"the mirror tok": {map[string]string{"Authorization": "Bearer " + proxyToken, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}, http.StatusUnauthorized},
		"an Origin":      {map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX, "Origin": "http://evil.example"}, http.StatusForbidden},
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
	if w := post(t, p, map[string]string{"Authorization": "Bearer ", MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}); w.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret: %d", w.Code)
	}
}

// toolErrorText is a tools/call answer's tool-error text, or fails.
func toolErrorText(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var r struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || !r.Result.IsError || len(r.Result.Content) != 1 || string(r.ID) != "7" {
		t.Fatalf("not a tool error for call 7: %d %s", w.Code, w.Body)
	}
	return r.Result.Content[0].Text
}

// TestTheMCPEndpointForwardsTheCallersIdentity: a tools/call's pane resolves
// to its Claude session, which goes to muster with the HOST's token and the
// body unchanged; muster's answer comes back as it was. Anything else is
// forwarded with NO session (it needs none).
func TestTheMCPEndpointForwardsTheCallersIdentity(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	})
	p := proxyFor(srv.URL + "/base/")
	auth := func(h map[string]string) map[string]string { h["Authorization"] = "Bearer " + proxySecret; return h }
	w := postBody(t, p, auth(map[string]string{MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}), proxyCall)
	if w.Code != http.StatusTeapot || w.Body.String() != `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if up.n() != 1 {
		t.Fatalf("%d calls", up.n())
	}
	r := up.calls[0]
	if r.URL.Path != "/base/mirror/mcp" || r.Header.Get("Authorization") != "Bearer "+proxyToken || r.Header.Get(MCPSessionHeader) != proxySID ||
		up.body[0] != proxyCall {
		t.Fatalf("forwarded %s %q %q %s", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(MCPSessionHeader), up.body[0])
	}
	// The local secret never leaves the machine.
	if strings.Contains(r.Header.Get("Authorization"), proxySecret) {
		t.Fatal("the local secret was forwarded")
	}
	// A pane in a split (not the window's active one) is its own session.
	if w := postBody(t, p, auth(map[string]string{MCPPaneHeader: "%9", MCPTmuxHeader: proxyTMUX}), proxyCall); w.Code != http.StatusTeapot {
		t.Fatalf("a split pane: %d %s", w.Code, w.Body)
	}
	if got := up.calls[1].Header.Get(MCPSessionHeader); got != splitSID {
		t.Fatalf("the split pane forwarded session %q, want %q", got, splitSID)
	}
	// A shell pane, an unknown pane, a session id with no pane and no pane at
	// all: the CALL is refused here as a tool error, and not forwarded.
	for name, hdr := range map[string]map[string]string{
		"a shell pane":          {MCPPaneHeader: "%8", MCPTmuxHeader: proxyTMUX},
		"an unknown pane":       {MCPPaneHeader: "%99", MCPTmuxHeader: proxyTMUX},
		"a session id, no pane": {MCPSessionHeader: proxySID, MCPTmuxHeader: proxyTMUX},
		"no pane":               {MCPTmuxHeader: proxyTMUX},
	} {
		if text := toolErrorText(t, postBody(t, p, auth(hdr), proxyCall)); !strings.Contains(strings.ToLower(text), "pane") {
			t.Errorf("%s: %s", name, text)
		}
	}
	// Another tmux server's pane, and no tmux at all, are refused for ANY
	// request: no pane there is this host's.
	for name, hdr := range map[string]map[string]string{
		"another tmux server": {MCPPaneHeader: "%7", MCPTmuxHeader: "/tmp/tmux-1000/other,99,0"},
		"no tmux":             {MCPPaneHeader: "%7"},
		"no name":             {},
	} {
		for _, body := range []string{proxyCall, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`} {
			if w := postBody(t, p, auth(hdr), body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "tmux server") {
				t.Errorf("%s: %d %s", name, w.Code, w.Body)
			}
		}
	}
	if up.n() != 2 {
		t.Fatalf("%d calls after the refusals, want 2", up.n())
	}
	// initialize, tools/list, a notification: forwarded from a pane with no
	// Claude session (yet), with no session header.
	for _, body := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`} {
		if w := postBody(t, p, auth(map[string]string{MCPPaneHeader: "%8", MCPTmuxHeader: proxyTMUX}), body); w.Code != http.StatusTeapot {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
		last := up.calls[up.n()-1]
		if _, set := last.Header[http.CanonicalHeaderKey(MCPSessionHeader)]; set {
			t.Fatalf("%s was forwarded with a session %q", body, last.Header.Get(MCPSessionHeader))
		}
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
		req.Header.Set(MCPTmuxHeader, proxyTMUX)
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

// TestTheMCPSecretIsCreatedOnceUnderConcurrency: hosts starting at once all
// read the ONE secret that won, and none ever reads an empty one.
func TestTheMCPSecretIsCreatedOnceUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muster-mirror", "mcp-secret")
	const n = 16
	got := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := EnsureMCPSecret(path)
			if err != nil {
				errs <- err
				return
			}
			got <- s
		}()
	}
	wg.Wait()
	close(got)
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent create failed: %v", err)
	}
	var first string
	for s := range got {
		if len(s) != 64 {
			t.Fatalf("a reader saw %q", s)
		}
		if first == "" {
			first = s
		}
		if s != first {
			t.Fatalf("two secrets: %q and %q", first, s)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// ensureWithin runs EnsureMCPSecret(path) under a short deadline: a hang is a
// failure, not a stuck suite.
func ensureWithin(t *testing.T, path string) (string, error) {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	done := make(chan res, 1)
	go func() {
		s, err := EnsureMCPSecret(path)
		done <- res{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-time.After(2 * time.Second):
		t.Fatalf("EnsureMCPSecret(%s) did not return within 2s", path)
		return "", nil
	}
}

// TestASymlinkedMCPSecretIsRefusedAtOnce: a dangling symlink (which Stat reads
// as missing, and the create then collides with) and a symlink to a real file
// are both refused, promptly, and no temp file is left behind.
func TestASymlinkedMCPSecretIsRefusedAtOnce(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toReal := filepath.Join(dir, "to-real")
	if err := os.Symlink(real, toReal); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dangling, toReal} {
		if _, err := ensureWithin(t, p); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s: %v", p, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mcp-secret-") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
}

// TestAStrayTempFileDoesNotStopTheSecret: a leftover temp file beside the
// secret (a crash mid-create) neither blocks creating it nor is taken for it.
func TestAStrayTempFileDoesNotStopTheSecret(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, ".mcp-secret-123456")
	if err := os.WriteFile(stray, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ensureWithin(t, filepath.Join(dir, "mcp-secret"))
	if err != nil || len(s) != 64 || s == "partial" {
		t.Fatalf("%q %v", s, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("want the secret and the stray only: %v", entries)
	}
}

// TestASplitPaneBeyondTheBoundIsRefusedWithTheReason: the proxy applies the
// wire's bound (MaxOtherClaudePerWindow other panes per window) and says so,
// rather than forwarding a session muster was never told about.
func TestASplitPaneBeyondTheBoundIsRefusedWithTheReason(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) })
	var others []ClaudePane
	for i := 0; i <= mirror.MaxOtherClaudePerWindow; i++ {
		others = append(others, ClaudePane{Pane: "%" + itoa(20+i), Gen: itoa(i) + strings.Repeat("d", 35)})
	}
	p := proxyFor(srv.URL)
	p.Windows = &windowsBackend{windows: []WindowInfo{{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID}, Others: others}}}
	last := "%" + itoa(20+mirror.MaxOtherClaudePerWindow)
	w := postBody(t, p, map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: last, MCPTmuxHeader: proxyTMUX}, proxyCall)
	if text := toolErrorText(t, w); !strings.Contains(text, "at most") {
		t.Fatalf("the pane past the bound: %s", text)
	}
	if w := postBody(t, p, map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%20", MCPTmuxHeader: proxyTMUX}, proxyCall); w.Code != http.StatusOK {
		t.Fatalf("a pane within the bound: %d %s", w.Code, w.Body)
	}
	if up.n() != 1 {
		t.Fatalf("%d forwards, want 1", up.n())
	}
}

// TestTheSecretReadChecksTheOpenedFile: readMCPSecret is what runs after
// EnsureMCPSecret's Lstat, so a path swapped in between must still be refused
// by the read itself — a symlink (even to a private, valid secret) does not
// open, a FIFO is not read (and does not hang), a file readable by others is
// refused, and a private regular file is read.
func TestTheSecretReadChecksTheOpenedFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte(strings.Repeat("c", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{link: "symlink", open: "readable by others", fifo: "not a regular file"} {
		done := make(chan error, 1)
		go func() { _, err := readMCPSecret(path); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", filepath.Base(path), err, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the read hung", filepath.Base(path))
		}
	}
	if s, err := readMCPSecret(good); err != nil || s != strings.Repeat("b", 64) {
		t.Fatalf("the private file: %q %v", s, err)
	}
}

// claudeCodeShows is how Claude Code 2.1.289 renders an MCP server's error
// text (measured: "%110" showed as "pane 0"): every "%" + two hex digits is
// decoded to that byte; anything else is left alone.
func claudeCodeShows(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			n, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b.WriteByte(byte(n))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool { return strings.IndexByte("0123456789abcdefABCDEF", c) >= 0 }

// hundredPanes lists %110 running proxySID and %111 a shell.
type hundredPanes struct{}

func (hundredPanes) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{
		{Session: "work", ID: "@40", Pane: "%110", Claude: &ClaudePane{Pane: "%110", Gen: proxySID}},
		{Session: "work", ID: "@41", Pane: "%111"},
	}, nil
}

// TestThePaneIsTheValueClaudeCodeSends: "X-Muster-Pane: %110", exactly as
// Claude Code 2.1.289 sent it (captured from a real `claude` through its
// headersHelper), names pane %110 — nothing decodes it on this side. A refusal
// names the value as received AND its number without the "%", so the message
// still reads right after Claude Code percent-decodes it for display.
func TestThePaneIsTheValueClaudeCodeSends(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) })
	p := proxyFor(srv.URL)
	p.Windows = hundredPanes{}
	hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%110", MCPTmuxHeader: proxyTMUX}
	if w := postBody(t, p, hdr, proxyCall); w.Code != http.StatusOK || up.n() != 1 || up.calls[0].Header.Get(MCPSessionHeader) != proxySID {
		t.Fatalf("pane %%110: %d %s, %d forwards", w.Code, w.Body, up.n())
	}
	hdr[MCPPaneHeader] = "%111"
	text := toolErrorText(t, postBody(t, p, hdr, proxyCall))
	if !strings.Contains(text, `pane \"%111\" (tmux pane number 111)`) {
		t.Fatalf("the refusal does not name the pane as received: %s", text)
	}
	if shown := claudeCodeShows(text); !strings.Contains(shown, "(tmux pane number 111)") {
		t.Fatalf("as Claude Code shows it: %q", shown)
	}
	// CONTROL: the decoder really does what Claude Code did to "%110".
	if got := claudeCodeShows("pane %110"); got != "pane \x110" {
		t.Fatalf("instrument: %q", got)
	}
	// A value that is not a pane id is quoted as received, odd bytes visible.
	hdr[MCPPaneHeader] = "x\x01y"
	if text := toolErrorText(t, postBody(t, p, hdr, proxyCall)); !strings.Contains(text, `\"x\\x01y\"`) {
		t.Fatalf("an odd value: %s", text)
	}
}
