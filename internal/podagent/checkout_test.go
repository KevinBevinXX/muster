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
	cfg := CheckoutConfig{Repo: repo, Dir: dir, Voice: Voice{Who: "fixd", Agent: "the fixture agent", AgentOf: "its fixture agent"}}
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

// TestAnInterruptedMoveIsFinished: a move of the verified clone into the
// workspace interrupted after some entries leaves those entries and no .git.
// The next attempt finishes that move — it does not refuse the workspace
// forever as "holds files" — and leaves no manifest or scratch behind. Files
// the move did not put there still make it refuse.
func TestAnInterruptedMoveIsFinished(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "example-org/tide-charts", map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})
	base := t.TempDir()
	dir := filepath.Join(base, "tide-charts")
	moved := 0
	moveEntry = func(from, to string) error {
		if moved == 1 {
			return errors.New("fixture: interrupted")
		}
		moved++
		return os.Rename(from, to)
	}
	t.Cleanup(func() { moveEntry = os.Rename })
	c := newTestCheckout(t, "example-org/tide-charts", dir, "")
	if err := c.Attempt(context.Background()); err == nil || !strings.Contains(err.Error(), "fixture: interrupted") {
		t.Fatalf("the interrupted move: err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("after the interruption the workspace holds %d entries, want 1", len(ents))
	}
	moveEntry = os.Rename
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("the attempt after an interrupted move: %v", err)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing after the resumed move", f)
		}
	}
	if gitT(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "trunk" || gitT(t, dir, "status", "--porcelain") != "" {
		t.Fatal("the resumed checkout is not a clean trunk checkout")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git", moveManifest)); err == nil {
		t.Fatal("the move manifest was left in the checkout")
	}
	if ents, _ := os.ReadDir(base); len(ents) != 1 {
		t.Fatalf("the scratch clone was left behind: %d entries", len(ents))
	}

	// A file the move did not put there: refused, as before.
	dir2 := filepath.Join(base, "second")
	moved = 0
	moveEntry = func(from, to string) error {
		if moved == 1 {
			return errors.New("fixture: interrupted")
		}
		moved++
		return os.Rename(from, to)
	}
	c2 := newTestCheckout(t, "example-org/tide-charts", dir2, "")
	_ = c2.Attempt(context.Background())
	moveEntry = os.Rename
	if err := os.WriteFile(filepath.Join(dir2, "stranger.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c2.Attempt(context.Background()); err == nil || !strings.Contains(err.Error(), "holds files but is not a git checkout") {
		t.Fatalf("a stranger file: err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir2, ".git")); err == nil {
		t.Fatal("a move was finished over a file it did not put there")
	}
}
