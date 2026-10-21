package podagent

import (
	"context"
	"errors"
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

// These cases are the checkout's own; cmd/ccd's checkout tests cover the clone
// through ccd's variables. git reaches "https://github.com/" through
// url.<local>.insteadOf in a private GIT_CONFIG_GLOBAL: nothing here reaches
// the network.

// localGitHub points https://github.com/ at a directory of bare repositories and
// returns that directory.
func localGitHub(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gh := filepath.Join(root, "github")
	cfg := filepath.Join(root, "gitconfig")
	body := fmt.Sprintf("[url \"file://%s/\"]\n\tinsteadOf = https://github.com/\n[user]\n\tname = fixture\n\temail = fixture@example.test\n[init]\n\tdefaultBranch = trunk\n", gh)
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return gh
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// publish makes <gh>/<repo>.git holding files on trunk (none: an EMPTY repository).
func publish(t *testing.T, gh, repo string, files map[string]string) {
	t.Helper()
	bare := filepath.Join(gh, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, bare, "init", "-q", "--bare")
	if len(files) == 0 {
		return
	}
	work := t.TempDir()
	gitT(t, work, "init", "-q")
	for p, b := range files {
		if err := os.WriteFile(filepath.Join(work, p), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "fixture")
	gitT(t, work, "push", "-q", bare, "trunk")
}

func newTestCheckout(t *testing.T, repo, dir, base string) *Checkout {
	t.Helper()
	return newBranchCheckout(t, repo, "", dir, base)
}

func newBranchCheckout(t *testing.T, repo, branch, dir, base string) *Checkout {
	t.Helper()
	cfg := CheckoutConfig{Repo: repo, Branch: branch, Dir: dir, Voice: Voice{Who: "fixd", Agent: "the fixture agent", AgentOf: "its fixture agent"}}
	if base != "" {
		cfg.ConnectBase, cfg.Token = base, "fixture-agent-token-81d0"
	}
	c := NewCheckout(cfg)
	c.Retry = 20 * time.Millisecond
	c.Logf = t.Logf
	return c
}

// TestAnEmptyRepositoryIsACheckout: a clone of a repository with no commit
// succeeds with no HEAD to verify. It is a checkout (the agent makes the first
// commit), not a clone that "finished with no commit checked out" and is retried
// forever — and the next attempt (a pod restart) uses it as it is.
func TestAnEmptyRepositoryIsACheckout(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/blank-slate", nil)
	dir := filepath.Join(t.TempDir(), "ws", "blank-slate")
	c := newTestCheckout(t, "example-org/blank-slate", dir, "")
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("the clone of an empty repository: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal("no .git in the workspace")
	}
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("the existing checkout of an empty repository was refused: %v", err)
	}
	// A .git with no commit and no mark is still refused: the mark, not the
	// emptiness, is what says a whole clone was moved into place.
	other := filepath.Join(t.TempDir(), "ws", "blank-slate")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, other, "init", "-q")
	err := newTestCheckout(t, "example-org/blank-slate", other, "").Attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no commit checked out") {
		t.Fatalf("an unmarked .git with no commit: err = %v", err)
	}
}

// githubStub is muster's github connector surface: grant none, and the
// requests filed.
type githubStub struct {
	mu    sync.Mutex
	filed int
}

func (g *githubStub) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agent/connectors/github":
			_, _ = w.Write([]byte(`{"name":"github","exists":true,"grant":"none"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/agent/connectors/github/request":
			g.filed++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"name":"github","grant":"pending"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestALocalFailureIsNotACloneFailure: a workspace holding files that are not a
// checkout is the checkout's own problem — no github grant changes it — so it is
// reported as such (state failed, not "could not be cloned") and files NO
// github connector request.
func TestALocalFailureIsNotACloneFailure(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	dir := filepath.Join(t.TempDir(), "ws", "tide-charts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := &githubStub{}
	c := newTestCheckout(t, "example-org/tide-charts", dir, stub.server(t).URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	var st CheckoutState
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st = c.State(); st.State != CheckoutCloning {
			break
		}
	}
	time.Sleep(150 * time.Millisecond) // several retries
	st = c.State()
	if st.State != CheckoutFailed || !strings.Contains(st.Detail, "holds files but is not a git checkout") ||
		strings.Contains(st.Detail, "could not be cloned") {
		t.Fatalf("state %+v", st)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.filed != 0 {
		t.Fatalf("a local failure filed %d github connector request(s)", stub.filed)
	}
}

// TestAnEmptyRepositoryWithABranchIsACheckout (#209 audit 🔴1): muster always
// names a branch (resolveRepoBranch: the repo's default, else "main"), and
// `git clone --branch=X` of an empty repository fails "Remote branch X not
// found". An empty remote (no refs) is cloned without --branch, its unborn
// HEAD pointed at the branch asked for.
func TestAnEmptyRepositoryWithABranchIsACheckout(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/blank-slate", nil)
	dir := filepath.Join(t.TempDir(), "ws", "blank-slate")
	c := newBranchCheckout(t, "example-org/blank-slate", "trunk", dir, "")
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("the clone of an empty repository on a named branch: %v", err)
	}
	if head := gitT(t, dir, "symbolic-ref", "HEAD"); head != "refs/heads/trunk" {
		t.Fatalf("HEAD = %q, want the branch asked for (refs/heads/trunk)", head)
	}
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("the existing checkout: %v", err)
	}
}

// TestAMissingBranchOfARealRepositoryStillFails: the empty-repository fallback
// is for a remote with NO refs. A repository with commits whose branch does
// not exist fails as before — git's own error, not a local one, and nothing is
// checked out.
func TestAMissingBranchOfARealRepositoryStillFails(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	dir := filepath.Join(t.TempDir(), "ws", "tide-charts")
	err := newBranchCheckout(t, "example-org/tide-charts", "no-such-branch", dir, "").Attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no-such-branch") {
		t.Fatalf("err = %v; want git's missing-branch error", err)
	}
	var le *localError
	if errors.As(err, &le) {
		t.Fatalf("a missing branch was classed local: %v", err)
	}
	if _, serr := os.Lstat(filepath.Join(dir, ".git")); serr == nil {
		t.Fatal("a missing branch left a checkout")
	}
}

// TestAnEmptyCheckoutTheAgentFetchedIntoIsStillACheckout (#209 audit 🟡2):
// after the agent fetched (refs exist) and staged work, still with no commit
// of its own, a restart must use the checkout — not refuse it and tell the
// operator to delete the .git holding that work.
func TestAnEmptyCheckoutTheAgentFetchedIntoIsStillACheckout(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/blank-slate", nil)
	dir := filepath.Join(t.TempDir(), "ws", "blank-slate")
	c := newBranchCheckout(t, "example-org/blank-slate", "trunk", dir, "")
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Someone pushes to the remote; the agent fetches and stages a file.
	other := t.TempDir()
	gitT(t, other, "init", "-q")
	if err := os.WriteFile(filepath.Join(other, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, other, "add", "x.txt")
	gitT(t, other, "commit", "-q", "-m", "elsewhere")
	gitT(t, other, "push", "-q", filepath.Join(gh, "example-org", "blank-slate.git"), "trunk:refs/heads/elsewhere")
	gitT(t, dir, "fetch", "-q", "origin")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "work.txt")
	if refs := gitT(t, dir, "for-each-ref"); refs == "" {
		t.Fatal("instrument: the fetch left no refs")
	}
	if err := newBranchCheckout(t, "example-org/blank-slate", "trunk", dir, "").Attempt(context.Background()); err != nil {
		t.Fatalf("a restart after the agent fetched: %v", err)
	}
}

// TestALocalCloneFailureFilesNoGithubRequest (#209 audit 🟢5): the clone
// command failing on THIS side (here: a parent it cannot write) is local — no
// github grant fixes it — so no connector request is filed.
func TestALocalCloneFailureFilesNoGithubRequest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere: no unwritable parent to fail on")
	}
	gh := localGitHub(t)
	publish(t, gh, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	dir := filepath.Join(parent, "tide-charts")
	stub := &githubStub{}
	c := newTestCheckout(t, "example-org/tide-charts", dir, stub.server(t).URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	var st CheckoutState
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st = c.State(); st.State != CheckoutCloning {
			break
		}
	}
	time.Sleep(150 * time.Millisecond)
	st = c.State()
	if st.State != CheckoutFailed || !strings.Contains(st.Detail, "failed on this pod") || strings.Contains(st.Detail, "could not be cloned") {
		t.Fatalf("state %+v", st)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.filed != 0 {
		t.Fatalf("a local clone failure filed %d github connector request(s)", stub.filed)
	}
}

// TestAnInterruptedMoveIsRefusedNamingTheScratchClone (#209 audit D2): a move
// of the clone into place that was cut short leaves files in the workspace
// and the clone's .git in the scratch directory. It is refused — not finished —
// and the refusal names the scratch directory to remove; once both are
// removed the next attempt clones.
func TestAnInterruptedMoveIsRefusedNamingTheScratchClone(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/tide-charts", map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	base := t.TempDir()
	dir := filepath.Join(base, "tide-charts")
	scratch := filepath.Join(base, ".tide-charts.fixd-clone")
	gitT(t, base, "clone", "-q", "https://github.com/example-org/tide-charts.git", scratch)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(scratch, "a.txt"), filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	c := newTestCheckout(t, "example-org/tide-charts", dir, "")
	err := c.Attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), scratch) {
		t.Fatalf("err = %v; want a refusal naming the scratch clone %s", err, scratch)
	}
	if _, serr := os.Lstat(filepath.Join(dir, ".git")); serr == nil {
		t.Fatal("the interrupted move was finished")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(scratch); err != nil {
		t.Fatal(err)
	}
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("after removing both: %v", err)
	}
}

// TestAMarkedCheckoutWhoseBranchLostItsCommitIsRefused (#209 round-2 🟢2): the
// empty-repository exception is for an UNBORN HEAD only. A marked checkout
// whose branch exists but names a commit whose object is gone (corruption) is
// not unborn, and is refused like any .git with no commit checked out.
func TestAMarkedCheckoutWhoseBranchLostItsCommitIsRefused(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/blank-slate", nil)
	dir := filepath.Join(t.TempDir(), "ws", "blank-slate")
	c := newBranchCheckout(t, "example-org/blank-slate", "trunk", dir, "")
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	head := gitT(t, dir, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", head[:2], head[2:])); err != nil {
		t.Fatal(err)
	}
	err := newBranchCheckout(t, "example-org/blank-slate", "trunk", dir, "").Attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no commit checked out") {
		t.Fatalf("a marked checkout whose branch names a missing commit: err = %v; want refused", err)
	}
}
