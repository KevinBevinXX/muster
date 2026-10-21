package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testHookSecret is the hook credential the unit tests give ccd and `ccd hook`.
const testHookSecret = "test-hook-secret-not-real-3f1a"

// hookPoster is THE way a unit test posts a hook: srv's real hook handler,
// with srv's credential set to testHookSecret, reached as `ccd hook` reaches it —
// every request carries the credential. TestOnlyTheHookHelperServesTheHookHandler
// keeps a test from serving hookHandler itself and forgetting the credential
// (which is how #164's test, written before the credential existed, broke on
// the merged tree: its hooks were refused 401 and changed nothing).
type hookPoster struct{ url string }

func hooks(t *testing.T, srv *server) *hookPoster {
	t.Helper()
	srv.cfg.HookSecret = testHookSecret
	hs := httptest.NewServer(srv.hookHandler())
	t.Cleanup(hs.Close)
	return &hookPoster{url: hs.URL}
}

// post sends body to /hook/<pathAndQuery> with the credential. Safe off the
// test goroutine (it never calls t.Fatal).
func (p *hookPoster) post(pathAndQuery, body string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, p.url+"/hook/"+pathAndQuery, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testHookSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// mustPost is post, failing the test on a transport error or a refusal.
func (p *hookPoster) mustPost(t *testing.T, pathAndQuery, body string) {
	t.Helper()
	code, err := p.post(pathAndQuery, body)
	if err != nil {
		t.Fatal(err)
	}
	if code/100 != 2 {
		t.Fatalf("POST /hook/%s: %d", pathAndQuery, code)
	}
}

// 🔴 ONLY THE HELPER SERVES hookHandler IN A UNIT TEST. A test that serves it
// itself and posts without the credential gets 401s that change nothing, and
// then fails — or, worse, passes — for a reason that has nothing to do with
// what it tests. The exceptions are this file (the helper, and the test that
// posts WITHOUT the credential on purpose) and the tmux suite's rig, which
// serves it over the socket to the real `ccd hook`.
//
// 🔴 IT READS THE SYNTAX TREE, NOT THE SPELLING. It used to count the text
// ".hookHandler()", which `(*server).hookHandler(srv)`, a method value
// `h := srv.hookHandler`, or a line break before the parentheses all walk past.
// Every selector naming hookHandler is a site, however it is reached.
func TestOnlyTheHookHelperServesTheHookHandler(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("instrument check: no test files found (%v)", err)
	}
	allowed := map[string]bool{"hookauth_test.go": true, "tmux_integration_test.go": true}
	seen := 0
	for _, f := range files {
		n := hookHandlerSites(t, f)
		seen += n
		if n > 0 && !allowed[f] {
			t.Errorf("%s serves hookHandler itself (%d site(s)); post hooks with hooks(t, srv).post / mustPost, "+
				"which carry the hook credential", f, n)
		}
	}
	if seen == 0 {
		t.Fatal("instrument check: no hookHandler site found at all; the scan is reading nothing")
	}
}

// hookHandlerSites counts the selectors naming hookHandler in the Go file at
// path: x.hookHandler, (*server).hookHandler, called or not.
func hookHandlerSites(t *testing.T, path string) int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "hookHandler" {
			n++
		}
		return true
	})
	return n
}

// TestTheHookHandlerScanSeesEverySpelling is the scan's own control: each way of
// reaching hookHandler is one site, and the identifier in a string or a comment
// is none.
func TestTheHookHandlerScanSeesEverySpelling(t *testing.T) {
	src := `package main

func a(srv *server) { _ = srv.hookHandler() }
func b(srv *server) { _ = (*server).hookHandler(srv) }
func c(srv *server) { h := srv.hookHandler; _ = h() }
func d(srv *server) { _ = srv.
	hookHandler() }
// srv.hookHandler() in a comment is not a site
var e = "srv.hookHandler()"
`
	path := filepath.Join(t.TempDir(), "x_test.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := hookHandlerSites(t, path); n != 4 {
		t.Fatalf("the scan found %d sites, want 4 (a call, a method expression, a method value, a split call)", n)
	}
}

// unixHookTarget serves h on a unix socket and points `ccd hook` at it, with
// testHookSecret as its credential, as ccd does for the CLIs it starts.
func unixHookTarget(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ccdh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "hook.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	t.Setenv(envHookSocket, path)
	t.Setenv(envHookSecret, testHookSecret)
	return path
}

// 🔴 A HOOK WITHOUT THE CREDENTIAL IS REFUSED AND CHANGES NOTHING. The session
// is mid-turn at a permission prompt (busy, awaitingApproval); a Stop with no
// credential, a wrong one, or sent to a ccd that holds none is 401 and leaves
// both set — a Stop accepted here is what made the next turn's paste answer
// the prompt. The control: the same Stop with the credential clears both.
func TestAHookWithoutTheCredentialIsRefusedAndChangesNothing(t *testing.T) {
	post := func(srv *server, cred string) int {
		hs := httptest.NewServer(srv.hookHandler())
		defer hs.Close()
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/hook/Stop", strings.NewReader(`{}`))
		if cred != "" {
			req.Header.Set("Authorization", "Bearer "+cred)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	blockedSession := func(secret string) *server {
		srv := newServer(serverConfig{HookSecret: secret, ConfigDir: t.TempDir()}, nil, newAuthTracker())
		srv.onHook(hookEvent{Event: "UserPromptSubmit", PromptID: "p"})
		srv.onHook(hookEvent{Event: "Notification", NotificationType: "permission_prompt"})
		if !srv.busy || !srv.awaitingApproval {
			t.Fatal("instrument check: the session is not mid-turn at a prompt")
		}
		return srv
	}
	for _, c := range []struct{ name, secret, cred string }{
		{"no credential", testHookSecret, ""},
		{"a wrong credential", testHookSecret, "not-the-credential"},
		{"a ccd holding no credential", "", ""},
	} {
		srv := blockedSession(c.secret)
		if code := post(srv, c.cred); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", c.name, code)
		}
		if !srv.busy || !srv.awaitingApproval || srv.idle() {
			t.Errorf("%s: the refused Stop changed the session (busy=%v awaiting=%v)", c.name, srv.busy, srv.awaitingApproval)
		}
	}
	srv := blockedSession(testHookSecret)
	if code := post(srv, testHookSecret); code != http.StatusNoContent || srv.busy || srv.awaitingApproval {
		t.Fatalf("control: with the credential: %d busy=%v awaiting=%v; want 204, both cleared", code, srv.busy, srv.awaitingApproval)
	}
}

// The hook channel serve makes: a socket only ccd's user can reach, a fresh
// credential per process, and both — and only both — in the CLI's environment.
func TestTheHookChannelIsAPrivateSocketWithAPerProcessCredential(t *testing.T) {
	a, err := newHookChannel()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(a.dir)
	b, err := newHookChannel()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(b.dir)
	if a.secret == b.secret || len(a.secret) != 64 || a.path == b.path {
		t.Fatalf("two channels share a credential or socket, or the credential is short: %q %q", a.secret, b.secret)
	}
	ln, err := a.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "unix" {
		t.Fatalf("the hook listener is %s, want a unix socket", ln.Addr().Network())
	}
	for path, want := range map[string]os.FileMode{a.dir: 0o700, a.path: 0o600} {
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
	if got := strings.Join(a.cliEnv(), " "); got != envHookSocket+"="+a.path+" "+envHookSecret+"="+a.secret {
		t.Errorf("the CLI's environment: %q", got)
	}
}

// tmux is told to put the hook channel in the environment of every CLI it
// starts: the primary's and each lane's (Start), and every restart (Respawn).
// (That the CLI then HAS it is the tmux suite's: no hook would be accepted.)
func TestEveryCLIStartCarriesTheHookChannel(t *testing.T) {
	term, dir := fakeTermScreen(t, "")
	term.env = []string{envHookSocket + "=/s/hook.sock", envHookSecret + "=sekrit"}
	if _, err := term.Start(context.Background(), "/w", []string{"claude"}); err == nil {
		t.Log("(fake new-session printed nothing parseable; only its argv matters here)")
	}
	_ = term.Respawn(context.Background(), paneRef{Server: "4242 1791600000", Pane: "%1"}, "/w", []string{"claude"})
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	var starts int
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.Contains(l, "new-session") || strings.Contains(l, "respawn-pane") {
			starts++
			if !strings.Contains(l, "-e "+envHookSocket+"=/s/hook.sock -e "+envHookSecret+"=sekrit") {
				t.Errorf("a CLI start without the hook channel: %s", l)
			}
		}
	}
	if starts != 2 {
		t.Fatalf("instrument check: %d starts seen, want 2:\n%s", starts, calls)
	}
}
