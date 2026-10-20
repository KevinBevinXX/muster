package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// localExecDriver runs an exec on THIS machine with the real `sh`, `head` and
// `git`, after moving every argument under the agent's workspace path into a
// temporary directory. So what is tested is the scripts the adapter sends, not
// a model of them; what is NOT tested is a real container's exec stream.
type localExecDriver struct {
	*provision.Noop
	root string

	mu    sync.Mutex
	execs int
}

func (d *localExecDriver) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

func (d *localExecDriver) Exec(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	d.mu.Lock()
	d.execs++
	d.mu.Unlock()
	args := make([]string, len(cmd))
	for i, a := range cmd {
		args[i] = strings.Replace(a, agentspec.ClaudeCodeWorkspacePath, d.root, 1)
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}

func (d *localExecDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execs
}

func newWorkspaceAdapter(t *testing.T) (*Adapter, *localExecDriver) {
	t.Helper()
	d := &localExecDriver{Noop: provision.MustNewNoop(), root: t.TempDir()}
	a, err := New(Config{Driver: d, Store: &recordingStore{agent: fixtureAgent()}, Spec: fixtureSpecConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}

var workspaceAgent = agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, Repo: "acme/widgets"}

func TestReadWorkspaceFileReturnsTheBytesVerbatim(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	dir := filepath.Join(d.root, "widgets", "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Review\r\n\n\t- \"quoted\" é ✓\n\n"
	if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, "out/review.md", len(body))
	if err != nil || string(got) != body {
		t.Fatalf("read = %q, %v; want %q", got, err, body)
	}
	// One byte over the limit is refused, not cut.
	if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, "out/review.md", len(body)-1); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("an oversize read = %v, want ErrFileTooLarge", err)
	}
	if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, "out/missing.md", 100); err == nil {
		t.Fatal("a missing file read without error")
	}
	// A path that leaves the workspace never reaches the instance.
	before := d.count()
	for _, p := range []string{"../etc/passwd", "/etc/passwd", "out/../../x"} {
		if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, p, 100); err == nil {
			t.Errorf("%q was read", p)
		}
	}
	if d.count() != before {
		t.Fatalf("a refused path ran %d exec(s)", d.count()-before)
	}
}

func TestCheckoutCommitDetachesAtTheCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	a, d := newWorkspaceAdapter(t)
	repo := filepath.Join(d.root, "widgets")
	git := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		c.Stdout, c.Stderr = &out, &out
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "one")
	first := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "two")

	if err := a.CheckoutCommit(context.Background(), workspaceAgent, first[:8]); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if head := git("rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want %s", head, first)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "f")); string(b) != "one" {
		t.Fatalf("worktree f = %q, want one", b)
	}

	// A commit that is nowhere (no origin to fetch it from) fails, saying so.
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, "0000000"); err == nil {
		t.Fatal("a checkout of an absent commit succeeded")
	}
	// Refusals that never reach the instance.
	before := d.count()
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, "HEAD~1"); err == nil {
		t.Error("a non-hex sha was accepted")
	}
	if err := a.CheckoutCommit(context.Background(), agents.Agent{Name: "no-repo", Kind: agents.KindClaudeCode}, first); err == nil {
		t.Error("an agent with no repository was checked out")
	}
	if d.count() != before {
		t.Fatalf("refused checkouts ran %d exec(s)", d.count()-before)
	}
}
