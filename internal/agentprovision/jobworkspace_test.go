package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
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
	got, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, 0, "out/review.md", len(body))
	if err != nil || string(got) != body {
		t.Fatalf("read = %q, %v; want %q", got, err, body)
	}
	// One byte over the limit is refused, not cut.
	if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, 0, "out/review.md", len(body)-1); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("an oversize read = %v, want ErrFileTooLarge", err)
	}
	if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, 0, "out/missing.md", 100); err == nil {
		t.Fatal("a missing file read without error")
	}
	// A path that leaves the workspace never reaches the instance.
	before := d.count()
	for _, p := range []string{"../etc/passwd", "/etc/passwd", "out/../../x"} {
		if _, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, 0, p, 100); err == nil {
			t.Errorf("%q was read", p)
		}
	}
	if d.count() != before {
		t.Fatalf("a refused path ran %d exec(s)", d.count()-before)
	}
}

// overlayAgent is workspaceAgent, whose checkout is <root>/widgets.
const overlayJobID = 41

// overlayPaths are every path the overlay decides, as the fixture lays them
// out: present in the worktree with the DEFAULT branch's bytes when the
// default branch has them, absent when it does not.
var overlayPaths = []string{
	"CLAUDE.md", "AGENTS.md", ".mcp.json",
	".claude/settings.json", ".claude/skills/rev/SKILL.md", ".claude/agents/evil.md",
	"sub/.claude/skills/x/SKILL.md", "deep/.claude/settings.json",
}

// TestAJobWorktreeIsTheCommitWithTheDefaultBranchsAgentConfig: through the
// real script and real git. The job's worktree is created beside the
// checkout, at the commit (fetched: the clone never had it), detached; every
// agent-configuration path holds the DEFAULT branch's bytes or is absent where
// the default branch has none — the commit's hook settings, its MCP server,
// its instructions, its added agent and its nested .claude/ are all gone —
// while a file outside the overlay (f) is the commit's. The agent's checkout
// is not touched: same HEAD, same branch, its untracked file still there.
//
// The POSITIVE CONTROL is the same commit as a plain worktree: there every
// one of those paths holds the commit's own bytes, so the fixture does carry
// the hazard the overlay removes.
func TestAJobWorktreeIsTheCommitWithTheDefaultBranchsAgentConfig(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	markers := filepath.Join(d.root, "markers")
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), markers)
	if err := os.WriteFile(filepath.Join(r.Checkout, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := worktreetest.Git(t, r.Checkout, "rev-parse", "HEAD")

	dir, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, r.Commit)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if want := "/data/workspace/.muster-jobs/41"; dir != want {
		t.Fatalf("worktree = %q, want %q", dir, want)
	}
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	if got := worktreetest.Git(t, wt, "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree HEAD = %s, want the commit %s", got, r.Commit)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "f")); string(b) != "two\n" {
		t.Fatalf("f (not overlaid) = %q, want the commit's %q", b, "two\n")
	}
	def := worktreetest.DefaultFiles(markers)
	for _, p := range overlayPaths {
		b, err := os.ReadFile(filepath.Join(wt, p))
		want, has := def[p]
		switch {
		case has && (err != nil || string(b) != want):
			t.Errorf("%s = %q (%v), want the default branch's %q", p, b, err, want)
		case !has && !os.IsNotExist(err):
			t.Errorf("%s is in the worktree (%q, %v); the default branch has none, so it must be absent", p, b, err)
		}
	}
	for _, p := range []string{"deep/.claude", ".claude/agents"} {
		if _, err := os.Lstat(filepath.Join(wt, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived the overlay (lstat: %v)", p, err)
		}
	}
	// The agent's checkout did not move.
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("the checkout's HEAD moved: %s, was %s", got, headBefore)
	}
	if got := worktreetest.Git(t, r.Checkout, "branch", "--show-current"); got != "main" {
		t.Fatalf("the checkout is on %q, want main", got)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Checkout, "wip.txt")); string(b) != "wip" {
		t.Fatalf("the checkout's untracked file was touched: %q", b)
	}

	// Positive control: the same commit with no overlay holds the commit's
	// own bytes at every one of those paths.
	plain := filepath.Join(d.root, "plain")
	r.PlainWorktree(t, plain)
	commit := worktreetest.CommitFiles(markers)
	for _, p := range overlayPaths {
		want, ok := commit[p]
		if !ok {
			want = def[p]
		}
		if b, err := os.ReadFile(filepath.Join(plain, p)); err != nil || string(b) != want {
			t.Errorf("control: %s = %q (%v), want the commit's %q", p, b, err, want)
		}
	}
}

// TestAJobWorktreeIsRemovedWhateverItsState: RemoveJobWorktree removes a
// registered worktree (git forgets it), a directory git does not know (a
// half-made one), and nothing at all, without error; the checkout survives.
func TestAJobWorktreeIsRemovedWhateverItsState(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, r.Commit); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	// Dirt the turn left: an untracked file and an edit.
	if err := os.WriteFile(filepath.Join(wt, "out.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "f"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there (lstat: %v)", err)
	}
	if list := worktreetest.Git(t, r.Checkout, "worktree", "list", "--porcelain"); strings.Contains(list, ".muster-jobs") {
		t.Fatalf("git still lists the job's worktree:\n%s", list)
	}
	// A directory git never registered (a crash between mkdir and add).
	if err := os.MkdirAll(filepath.Join(wt, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatalf("remove an unregistered directory: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("the unregistered directory is still there (lstat: %v)", err)
	}
	// Nothing there: not an error.
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatalf("remove nothing: %v", err)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "HEAD"); got != r.Default {
		t.Fatalf("the checkout's HEAD = %s, want %s", got, r.Default)
	}
}

// TestAJobWorktreeIsRefusedWithoutATrustedDefaultBranch: with no origin/HEAD
// (and an origin that cannot say), or with a symlink among the overlaid paths
// on the default branch, nothing is created and the error says why.
func TestAJobWorktreeIsRefusedWithoutATrustedDefaultBranch(t *testing.T) {
	t.Run("no origin/HEAD", func(t *testing.T) {
		a, d := newWorkspaceAdapter(t)
		r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
		worktreetest.Git(t, r.Checkout, "remote", "set-head", "origin", "--delete")
		worktreetest.Git(t, r.Checkout, "remote", "set-url", "origin", filepath.Join(d.root, "nowhere.git"))
		_, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, r.Commit)
		if err == nil || !strings.Contains(err.Error(), NoDefaultBranchMessage) {
			t.Fatalf("prepare = %v, want an error carrying %q", err, NoDefaultBranchMessage)
		}
		if _, err := os.Lstat(filepath.Join(d.root, ".muster-jobs", "41")); !os.IsNotExist(err) {
			t.Fatalf("a worktree was created (lstat: %v)", err)
		}
	})
	t.Run("origin/HEAD missing but origin can say", func(t *testing.T) {
		a, d := newWorkspaceAdapter(t)
		r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
		worktreetest.Git(t, r.Checkout, "remote", "set-head", "origin", "--delete")
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, r.Commit); err != nil {
			t.Fatalf("prepare = %v, want origin/HEAD re-learnt from origin", err)
		}
		if b, _ := os.ReadFile(filepath.Join(d.root, ".muster-jobs", "41", "CLAUDE.md")); string(b) != "# default instructions\n" {
			t.Fatalf("CLAUDE.md = %q, want the default branch's", b)
		}
	})
	// The default branch moves on to a commit carrying a symlink among the
	// overlaid paths — nested in .claude/, or a root file — which the checkout
	// learns with a fetch.
	for _, tc := range []struct{ path, target string }{
		{".claude/skills/link", "../../f"},
		{"AGENTS.md", "f"},
	} {
		t.Run("a symlink on the default branch at "+tc.path, func(t *testing.T) {
			a, d := newWorkspaceAdapter(t)
			r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
			src := filepath.Join(t.TempDir(), "src")
			worktreetest.Git(t, "", "clone", "-q", "file://"+r.Origin, src)
			if err := os.Symlink(tc.target, filepath.Join(src, tc.path)); err != nil {
				t.Fatal(err)
			}
			worktreetest.Git(t, src, "add", "--", tc.path)
			worktreetest.Git(t, src, "commit", "-q", "-m", "link")
			worktreetest.Git(t, src, "push", "-q", "origin", "main")
			worktreetest.Git(t, r.Checkout, "fetch", "-q", "origin")
			_, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, r.Commit)
			if err == nil || !strings.Contains(err.Error(), SymlinkOverlayMessage) || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("prepare = %v, want an error carrying %q naming %s", err, SymlinkOverlayMessage, tc.path)
			}
			if _, err := os.Lstat(filepath.Join(d.root, ".muster-jobs", "41")); !os.IsNotExist(err) {
				t.Fatalf("a worktree was created (lstat: %v)", err)
			}
		})
	}
	t.Run("refusals that never reach the instance", func(t *testing.T) {
		a, d := newWorkspaceAdapter(t)
		before := d.count()
		sha := strings.Repeat("a", 40)
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, "HEAD~1"); err == nil {
			t.Error("a non-hex sha was accepted")
		}
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, sha[:8]); err == nil {
			t.Error("an abbreviated sha was accepted")
		}
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, 0, sha); err == nil {
			t.Error("job id 0 was accepted")
		}
		if _, err := a.PrepareJobWorktree(context.Background(), agents.Agent{Name: "no-repo", Kind: agents.KindClaudeCode}, overlayJobID, sha); err == nil {
			t.Error("an agent with no repository was given a worktree")
		}
		if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, -1); err == nil {
			t.Error("a negative job id was removed")
		}
		if d.count() != before {
			t.Fatalf("refusals ran %d exec(s)", d.count()-before)
		}
	})
}

// TestReadWorkspaceFileReadsTheJobsWorktree: with a job id, rel is resolved in
// that job's worktree, not in the agent's checkout.
func TestReadWorkspaceFileReadsTheJobsWorktree(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	for dir, body := range map[string]string{
		filepath.Join(d.root, "widgets"):            "checkout",
		filepath.Join(d.root, ".muster-jobs", "41"): "worktree",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "r.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, overlayJobID, "r.md", 100); err != nil || string(got) != "worktree" {
		t.Fatalf("job read = %q, %v; want %q", got, err, "worktree")
	}
	if got, err := a.ReadWorkspaceFile(context.Background(), workspaceAgent, 0, "r.md", 100); err != nil || string(got) != "checkout" {
		t.Fatalf("checkout read = %q, %v; want %q", got, err, "checkout")
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

// TestRemoveUntrackedWorkspaceFileFailsClosedWhenGitCannotAnswer: in a real
// checkout with a TRACKED file, a git that cannot answer — exiting 128 (a
// "dubious ownership" refusal, a fatal error) or 127 (git missing) — must not
// be read as "untracked". The file stays, byte for byte, and the call fails
// saying so. Only an exit of exactly 1 ("did not match") deletes.
func TestRemoveUntrackedWorkspaceFileFailsClosedWhenGitCannotAnswer(t *testing.T) {
	for _, rc := range []int{128, 127, 2} {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			a, d := newWorkspaceAdapter(t)
			git, repo, _, _ := newCheckoutRepo(t, d)
			if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("tracked"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "notes.md")
			git("commit", "-q", "-m", "notes")
			// A git that cannot answer, first on PATH for the script's shell.
			bin := t.TempDir()
			fake := "#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit " + strconv.Itoa(rc) + "\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			removed, err := a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "notes.md")
			want := "could not tell whether notes.md is tracked (git ls-files exited " + strconv.Itoa(rc) + "), so it was left in place"
			if err == nil || removed || !strings.Contains(err.Error(), want) {
				t.Fatalf("with git exiting %d = %v, %v; want false and an error carrying %q", rc, removed, err, want)
			}
			if b, err := os.ReadFile(filepath.Join(repo, "notes.md")); err != nil || string(b) != "tracked" {
				t.Fatalf("the tracked file was deleted or changed: %q %v", b, err)
			}
		})
	}
}
