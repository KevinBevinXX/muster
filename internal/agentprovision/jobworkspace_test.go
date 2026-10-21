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

	if err := a.CheckoutCommit(context.Background(), workspaceAgent, first); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if head := git("rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want %s", head, first)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "f")); string(b) != "one" {
		t.Fatalf("worktree f = %q, want one", b)
	}

	// A commit that is nowhere (no origin to fetch it from) fails, saying so.
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, strings.Repeat("0", 40)); err == nil {
		t.Fatal("a checkout of an absent commit succeeded")
	}
	// Refusals that never reach the instance.
	before := d.count()
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, "HEAD~1"); err == nil {
		t.Error("a non-hex sha was accepted")
	}
	// An abbreviation, though it names a present commit: `git fetch origin`
	// could not fetch it on an agent that lacks it.
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, first[:8]); err == nil {
		t.Error("an abbreviated sha was accepted")
	}
	if err := a.CheckoutCommit(context.Background(), agents.Agent{Name: "no-repo", Kind: agents.KindClaudeCode}, first); err == nil {
		t.Error("an agent with no repository was checked out")
	}
	if d.count() != before {
		t.Fatalf("refused checkouts ran %d exec(s)", d.count()-before)
	}
}

// newCheckoutRepo is a repository at the agent's working directory with two
// commits; it returns the git runner and the two commit ids.
func newCheckoutRepo(t *testing.T, d *localExecDriver) (git func(args ...string) string, repo, first, second string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	repo = filepath.Join(d.root, "widgets")
	git = func(args ...string) string {
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
	if err := os.WriteFile(filepath.Join(repo, "g"), []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f", "g")
	git("commit", "-q", "-m", "one")
	first = git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "two")
	second = git("rev-parse", "HEAD")
	return git, repo, first, second
}

// TestCheckoutCommitRefusesADirtyTree: an uncommitted edit that does NOT
// conflict with the move, or an untracked file, would be carried onto the
// job's commit by `git checkout --detach`. Either refuses the checkout with
// DirtyTreeMessage, HEAD does not move, and nothing is discarded. An ignored
// file is not dirt.
func TestCheckoutCommitRefusesADirtyTree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, repo string)
		check func(t *testing.T, repo string)
	}{
		{"a non-conflicting edit", func(t *testing.T, repo string) {
			// g is the same in both commits, so git would carry this edit over.
			if err := os.WriteFile(filepath.Join(repo, "g"), []byte("wip"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, repo string) {
			if b, _ := os.ReadFile(filepath.Join(repo, "g")); string(b) != "wip" {
				t.Fatalf("the edit was discarded: g = %q", b)
			}
		}},
		{"an untracked file", func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, repo string) {
			if b, _ := os.ReadFile(filepath.Join(repo, "notes.txt")); string(b) != "n" {
				t.Fatalf("the untracked file was discarded: %q", b)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d := newWorkspaceAdapter(t)
			git, repo, first, second := newCheckoutRepo(t, d)
			tc.dirty(t, repo)
			err := a.CheckoutCommit(context.Background(), workspaceAgent, first)
			if err == nil || !strings.Contains(err.Error(), DirtyTreeMessage) {
				t.Fatalf("checkout of a dirty tree = %v, want an error carrying %q", err, DirtyTreeMessage)
			}
			if head := git("rev-parse", "HEAD"); head != second {
				t.Fatalf("HEAD moved to %s on a refused checkout, want %s", head, second)
			}
			tc.check(t, repo)
		})
	}
	// An ignored file is not dirt: the checkout proceeds.
	a, d := newWorkspaceAdapter(t)
	git, repo, first, _ := newCheckoutRepo(t, d)
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "out", "review.md"), []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckoutCommit(context.Background(), workspaceAgent, first); err != nil {
		t.Fatalf("a tree with only an ignored file was refused: %v", err)
	}
	if head := git("rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want %s", head, first)
	}
}

// TestRemoveUntrackedWorkspaceFileDeletesOnlyThatFile: outside any git
// repository (nothing is tracked) the file goes, a missing file is not an
// error, a symlink is removed without touching its target, and a path that
// leaves the workspace never reaches the instance.
func TestRemoveUntrackedWorkspaceFileDeletesOnlyThatFile(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	dir := filepath.Join(d.root, "widgets", "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.md")
	for _, f := range []string{"review.md", "keep.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "out/review.md"); err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review.md")); !os.IsNotExist(err) {
		t.Fatalf("out/review.md is still there (stat: %v)", err)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "keep.md" {
		t.Fatalf("a sibling was touched: %q %v", b, err)
	}
	if removed, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "out/review.md"); err != nil || !removed {
		t.Fatalf("removing a missing file = %v, %v, want true, nil", removed, err)
	}
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if removed, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "out/link.md"); err != nil || !removed {
		t.Fatalf("remove link = %v, %v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link.md")); !os.IsNotExist(err) {
		t.Fatalf("the symlink is still there (lstat: %v)", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "t" {
		t.Fatalf("the symlink's target was touched: %q %v", b, err)
	}
	before := d.count()
	for _, p := range []string{"../etc/passwd", "/etc/passwd", "out/../../x"} {
		if _, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, p); err == nil {
			t.Errorf("%q was removed", p)
		}
	}
	if d.count() != before {
		t.Fatalf("a refused path ran %d exec(s)", d.count()-before)
	}
}

// TestRemoveUntrackedWorkspaceFileLeavesATrackedFile: in a real checkout a file
// the repository tracks is reported (false, nil) and left byte for byte; an
// untracked one goes. Names are literal, not pathspecs: an untracked file
// literally named "*.md" is deleted, and the tracked notes.md the glob would
// match is not mistaken for it.
func TestRemoveUntrackedWorkspaceFileLeavesATrackedFile(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	git, repo, _, _ := newCheckoutRepo(t, d)
	if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "notes.md")
	git("commit", "-q", "-m", "notes")
	removed, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "notes.md")
	if err != nil || removed {
		t.Fatalf("tracked notes.md = %v, %v; want false, nil", removed, err)
	}
	if b, err := os.ReadFile(filepath.Join(repo, "notes.md")); err != nil || string(b) != "tracked" {
		t.Fatalf("a tracked file was touched: %q %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "*.md"), []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err = a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "*.md")
	if err != nil || !removed {
		t.Fatalf("untracked \"*.md\" = %v, %v; want true, nil", removed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "*.md")); !os.IsNotExist(err) {
		t.Fatalf("the untracked file \"*.md\" is still there (stat: %v)", err)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("the tree is dirty: %q", st)
	}
}
