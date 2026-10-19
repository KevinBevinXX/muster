package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is a local stand-in for https://github.com/: a directory of bare
// repositories, reached by git through url.<it>.insteadOf https://github.com/
// in a private GIT_CONFIG_GLOBAL — the same mechanism ccd's own connector
// routing uses to send git through muster's proxy. Nothing here reaches the
// network.
type fakeGitHub struct {
	root      string // holds <owner>/<name>.git
	gitconfig string
}

// gitRun runs git with the fake's environment (env) in dir.
func gitRun(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newFakeGitHub creates the fake under dir. Its environment (env) must reach
// every git ccd runs: the caller sets it on the process (t.Setenv) or the
// child's environment.
func newFakeGitHub(t *testing.T, dir string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{root: filepath.Join(dir, "github"), gitconfig: filepath.Join(dir, "gitconfig")}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[url \"file://%s/\"]\n\tinsteadOf = https://github.com/\n[user]\n\tname = fixture\n\temail = fixture@example.test\n[init]\n\tdefaultBranch = trunk\n", f.root)
	if err := os.WriteFile(f.gitconfig, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeGitHub) env() []string {
	return []string{"GIT_CONFIG_GLOBAL=" + f.gitconfig, "GIT_CONFIG_NOSYSTEM=1"}
}

// publish creates <owner/name> with files on its default branch (trunk), plus
// a branch "feature" adding feature.txt.
func (f *fakeGitHub) publish(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	work := t.TempDir()
	gitRun(t, f.env(), work, "init", "-q")
	for p, body := range files {
		full := filepath.Join(work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, f.env(), work, "add", ".")
	gitRun(t, f.env(), work, "commit", "-q", "-m", "fixture")
	gitRun(t, f.env(), work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("on feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.env(), work, "add", "feature.txt")
	gitRun(t, f.env(), work, "commit", "-q", "-m", "feature")
	gitRun(t, f.env(), work, "checkout", "-q", "trunk")
	bare := filepath.Join(f.root, filepath.FromSlash(repo)+".git")
	gitRun(t, f.env(), work, "clone", "-q", "--bare", work, bare)
}

// fakeConnectors is muster's agent connector surface for the github connector:
// a grant status, and the requests filed.
type fakeConnectors struct {
	mu       sync.Mutex
	exists   bool
	grant    string
	requests []map[string]string
	tokens   []string
	// stayNone: a filed request does not move the grant to pending (muster
	// lost or dropped it), so only ccd's own memory stops a re-request.
	stayNone bool
}

func (fc *fakeConnectors) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		fc.tokens = append(fc.tokens, r.Header.Get("X-Muster-Token"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agent/connectors/github":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "github", "exists": fc.exists, "grant": fc.grant})
		case r.Method == http.MethodGet && r.URL.Path == "/agent/connectors":
			_ = json.NewEncoder(w).Encode(map[string]any{"connectors": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/agent/connectors/github/request":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			fc.requests = append(fc.requests, body)
			if !fc.stayNone {
				fc.grant = "pending"
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "github", "grant": "pending"})
		case r.Method == http.MethodPost && r.URL.Path == "/agent/blocked":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (fc *fakeConnectors) set(exists bool, grant string) {
	fc.mu.Lock()
	fc.exists, fc.grant = exists, grant
	fc.mu.Unlock()
}

func (fc *fakeConnectors) filed() []map[string]string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]map[string]string(nil), fc.requests...)
}

// newTestCheckout is a checkout of repo/branch into dir, offered the connector
// store at base (""; not offered), retrying every 20ms.
func newTestCheckout(t *testing.T, repo, branch, dir, base string) *checkout {
	t.Helper()
	t.Setenv(envRepo, repo)
	t.Setenv(envRepoBranch, branch)
	t.Setenv(envConnectURL, base)
	t.Setenv(envAgentToken, "")
	if base != "" {
		t.Setenv(envAgentToken, "fixture-agent-token-5c2e")
	}
	c, err := checkoutFromEnv(dir)
	if err != nil || c == nil {
		t.Fatalf("checkoutFromEnv: %v %v", c, err)
	}
	c.retry = 20 * time.Millisecond
	c.logf = t.Logf
	return c
}

// waitState polls c until ok, or fails with the last state.
func waitState(t *testing.T, c *checkout, ok func(checkoutState) bool) checkoutState {
	t.Helper()
	var st checkoutState
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st = c.state(); ok(st) {
			return st
		}
	}
	t.Fatalf("checkout never got there; last %+v", st)
	return st
}

// TestTheCheckoutClonesTheBranchIntoTheWorkspace: an absent workspace and an
// EMPTY one (what `ccd seed` leaves) are both cloned into, on the named branch
// — and the CLI's gate (wait) opens only then.
func TestTheCheckoutClonesTheBranchIntoTheWorkspace(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	gh.publish(t, "example-org/tide-charts", map[string]string{"CLAUDE.md": "# project\n", ".claude/skills/x/SKILL.md": "---\nname: x\n---\n"})

	for _, pre := range []bool{false, true} {
		dir := filepath.Join(root, fmt.Sprintf("ws-%v", pre), "tide-charts")
		if pre {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		c := newTestCheckout(t, "example-org/tide-charts", "feature", dir, "")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		go c.run(ctx)
		if !c.wait(ctx) {
			cancel()
			t.Fatalf("pre-existing=%v: never ready: %+v", pre, c.state())
		}
		cancel()
		if st := c.state(); st.State != checkoutReady || st.Dir != dir || st.Branch != "feature" {
			t.Fatalf("state %+v", st)
		}
		if head := gitRun(t, nil, dir, "rev-parse", "--abbrev-ref", "HEAD"); head != "feature" {
			t.Fatalf("HEAD = %q, want feature", head)
		}
		for _, f := range []string{"feature.txt", "CLAUDE.md", ".claude/skills/x/SKILL.md"} {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
				t.Fatalf("pre-existing=%v: %s missing from the checkout: %v", pre, f, err)
			}
		}
	}
}

// TestAnExistingCheckoutIsUsedAsItIs: after a pod restart the volume holds the
// checkout, with the session's work in it; ccd neither re-clones nor touches it
// (a local file and a local commit survive), and opens the gate.
func TestAnExistingCheckoutIsUsedAsItIs(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	gh.publish(t, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	dir := filepath.Join(root, "ws", "tide-charts")
	gitRun(t, nil, root, "clone", "-q", "https://github.com/example-org/tide-charts.git", dir)
	if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nil, dir, "checkout", "-q", "-b", "agent-work")
	before := gitRun(t, nil, dir, "rev-parse", "HEAD")
	// The remote moves on; an existing checkout must not follow it.
	if err := os.RemoveAll(filepath.Join(gh.root, "example-org")); err != nil {
		t.Fatal(err)
	}
	c := newTestCheckout(t, "example-org/tide-charts", "trunk", dir, "")
	if err := c.attempt(context.Background()); err != nil {
		t.Fatalf("an existing checkout was not used: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "wip.txt")); err != nil || string(b) != "uncommitted\n" {
		t.Fatalf("the session's work was touched: %q %v", b, err)
	}
	if head := gitRun(t, nil, dir, "rev-parse", "--abbrev-ref", "HEAD"); head != "agent-work" || gitRun(t, nil, dir, "rev-parse", "HEAD") != before {
		t.Fatalf("HEAD moved: %s", head)
	}
}

// TestADirectoryThatIsNotACheckoutIsNotClonedOver: files that are not a git
// checkout are refused with a sentence that says so, and left as they are.
func TestADirectoryThatIsNotACheckoutIsNotClonedOver(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	gh.publish(t, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	dir := filepath.Join(root, "ws", "tide-charts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestCheckout(t, "example-org/tide-charts", "", dir, "")
	err := c.attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "holds files but is not a git checkout") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "mine\n" {
		t.Fatalf("the files were touched")
	}
}

// TestAFailedCloneFilesOneGithubRequestAndSaysWhatToDo: a repository the pod
// cannot read keeps the gate SHUT and says so; with no grant ccd files the
// github request ONCE (however many retries), with the repository in its
// reason and the agent's own token; and once the operator's approval lets git
// through (here: the repository becomes readable), the next retry clones and
// opens the gate.
func TestAFailedCloneFilesOneGithubRequestAndSaysWhatToDo(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	fc := &fakeConnectors{grant: "none"}
	srv := fc.server(t)
	dir := filepath.Join(root, "ws", "private-app")
	c := newTestCheckout(t, "example-org/private-app", "", dir, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	// 🔴 awaiting_grant, NOT failed: muster's kickoff deliverer holds the first
	// turn on exactly this state (agents.CheckoutAwaitingGrant), so a request ccd
	// just filed must read as one the owner's approval resolves.
	st := waitState(t, c, func(s checkoutState) bool { return s.State == checkoutAwaitingGrant && len(fc.filed()) > 0 })
	for _, w := range []string{"the repository example-org/private-app could not be cloned", "approve the request ccd filed",
		"no github connector exists yet"} {
		if !strings.Contains(st.Detail, w) {
			t.Fatalf("detail %q lacks %q", st.Detail, w)
		}
	}
	// Several more retries (the fake now answers pending): still ONE request.
	time.Sleep(200 * time.Millisecond)
	reqs := fc.filed()
	if len(reqs) != 1 || !strings.Contains(reqs[0]["reason"], "clone example-org/private-app") {
		t.Fatalf("requests filed: %+v", reqs)
	}
	waitState(t, c, func(s checkoutState) bool {
		return strings.Contains(s.Detail, "approve this agent's request for the github connector") && s.State == checkoutAwaitingGrant
	})
	if n := len(fc.filed()); n != 1 {
		t.Fatalf("requests filed after the pending retry: %d", n)
	}
	select {
	case <-c.done:
		t.Fatal("the gate opened with no checkout")
	default:
	}
	fc.mu.Lock()
	for _, tok := range fc.tokens {
		if tok != "fixture-agent-token-5c2e" {
			fc.mu.Unlock()
			t.Fatalf("a call carried token %q", tok)
		}
	}
	fc.mu.Unlock()

	// Approved, and git can now read it.
	gh.publish(t, "example-org/private-app", map[string]string{"README.md": "r\n"})
	fc.set(true, "approved")
	wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	defer wcancel()
	if !c.wait(wctx) {
		t.Fatalf("never cloned after the approval: %+v", c.state())
	}
	if len(fc.filed()) != 1 {
		t.Fatalf("requests filed: %+v", fc.filed())
	}
	if st := c.state(); st.State != checkoutReady {
		t.Fatalf("after the clone: state %q, want ready", st.State)
	}
}

// TestADeniedGrantIsNotAskedAgain: denied (or revoked) is reported with what
// to do, and ccd files nothing.
func TestADeniedGrantIsNotAskedAgain(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	for _, grant := range []string{"denied", "revoked"} {
		fc := &fakeConnectors{exists: true, grant: grant}
		srv := fc.server(t)
		c := newTestCheckout(t, "example-org/private-app", "", filepath.Join(root, grant, "private-app"), srv.URL)
		err := c.attempt(context.Background())
		if err == nil {
			t.Fatal("cloned a repository that does not exist")
		}
		d, awaiting := c.explain(context.Background(), err)
		if !strings.Contains(d, "grant was "+grant+", and ccd does not ask again") || len(fc.filed()) != 0 {
			t.Fatalf("%s: detail %q, filed %+v", grant, d, fc.filed())
		}
		// Not awaiting: no approval is coming, so muster must not hold the turn.
		if awaiting {
			t.Fatalf("%s: reported as awaiting the grant", grant)
		}
	}
}

// TestAPodWithoutTheConnectorStoreSaysOnlyPublicReposClone: no connect URL,
// no request, and the sentence names why.
func TestAPodWithoutTheConnectorStoreSaysOnlyPublicReposClone(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	c := newTestCheckout(t, "example-org/private-app", "", filepath.Join(root, "ws", "private-app"), "")
	err := c.attempt(context.Background())
	if err == nil {
		t.Fatal("cloned a repository that does not exist")
	}
	if d, awaiting := c.explain(context.Background(), err); !strings.Contains(d, "only a public repository can be cloned") || awaiting {
		t.Fatalf("detail %q awaiting %t", d, awaiting)
	}
}

// TestTheCheckoutComesOnlyFromCCDRepo: no CCD_REPO, no checkout (the agent's
// workspace is used as before); a malformed one, or a relative workspace, is an
// error that stops ccd rather than a CLI started somewhere unexpected.
func TestTheCheckoutComesOnlyFromCCDRepo(t *testing.T) {
	t.Setenv(envRepo, "")
	if c, err := checkoutFromEnv("/data/workspace"); c != nil || err != nil {
		t.Fatalf("no CCD_REPO: %v %v", c, err)
	}
	for _, bad := range []string{"tide-charts", "example-org/", "/tide", "example-org/a/b", "example-org/..", "-x/y"} {
		t.Setenv(envRepo, bad)
		if _, err := checkoutFromEnv("/data/workspace/x"); err == nil || !strings.Contains(err.Error(), "is not owner/name") {
			t.Errorf("CCD_REPO=%q: err = %v", bad, err)
		}
	}
	t.Setenv(envRepo, "example-org/tide-charts")
	if _, err := checkoutFromEnv("workspace/tide-charts"); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("relative workspace: %v", err)
	}
}

// TestARequestIsFiledOnceEvenIfTheGrantStaysNone: when muster still answers
// `none` after ccd filed the request, ccd does not file it again on every
// retry — it says the request is no longer pending instead.
func TestARequestIsFiledOnceEvenIfTheGrantStaysNone(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	fc := &fakeConnectors{grant: "none", stayNone: true}
	srv := fc.server(t)
	c := newTestCheckout(t, "example-org/private-app", "", filepath.Join(root, "ws", "private-app"), srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)
	st := waitState(t, c, func(s checkoutState) bool { return strings.Contains(s.Detail, "no longer pending") })
	time.Sleep(200 * time.Millisecond)
	if n := len(fc.filed()); n != 1 {
		t.Fatalf("filed %d requests over the retries (detail %q); want 1", n, st.Detail)
	}
	if st.State != checkoutFailed {
		t.Fatalf("a request no longer pending reads %q, want failed (nothing is coming)", st.State)
	}
}
