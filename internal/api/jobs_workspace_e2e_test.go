package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/provision"
)

// hostExecDriver runs an exec on THIS machine with the real sh, head, rm and
// git, after moving every argument under the agent workspace path into root. So
// the job runner drives the real lifecycle adapter's real scripts against a
// real git checkout; what is not covered is a container's exec stream.
type hostExecDriver struct {
	*provision.Noop
	root string
}

func (d *hostExecDriver) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

func (d *hostExecDriver) Exec(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := make([]string, len(cmd))
	for i, a := range cmd {
		args[i] = strings.Replace(a, agentspec.ClaudeCodeWorkspacePath, d.root, 1)
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}

// fileWritingGateway plays the agent: its turn writes the result file into the
// checkout, as a review agent asked for a result_path does.
type fileWritingGateway struct {
	path, body string
}

func (g *fileWritingGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	if g.path != "" {
		if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(g.path, []byte(g.body), 0o644); err != nil {
			return "", err
		}
	}
	return "done", nil
}

func (g *fileWritingGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", nil
}

// cliGateway plays ccd AND the CLI it starts, as far as a commit's agent
// configuration can reach them: in the directory the turn names
// (agents.HeaderCCWorkdir, moved under root), it runs every SessionStart hook
// command of EVERY .claude/settings.json in the tree (more than a real CLI
// loads at start: the root one, and nested ones it could reach later) and
// every MCP server command of the root .mcp.json — then plays the turn: it
// writes result into resultPath, and answers reply, or fails with err, or
// panics, or (block) waits for its context.
type cliGateway struct {
	t                *testing.T
	root             string
	resultPath, body string
	err              error
	panics, block    bool
	// dirs records each turn's directory, under root.
	dirs []string
}

func (g *cliGateway) Chat(ctx context.Context, _ agents.Agent, _, _ string, _ func(string)) (string, error) {
	route, _ := agents.TurnRouteFrom(ctx)
	dir := strings.Replace(route.Header.Get(agents.HeaderCCWorkdir), agentspec.ClaudeCodeWorkspacePath, g.root, 1)
	if dir == "" {
		return "", errors.New("cliGateway: the turn named no working directory")
	}
	g.dirs = append(g.dirs, dir)
	runCLIConfig(g.t, dir)
	if g.resultPath != "" {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, g.resultPath)), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, g.resultPath), []byte(g.body), 0o644); err != nil {
			return "", err
		}
	}
	if g.panics {
		panic("cliGateway: a bug in the turn")
	}
	if g.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "done", g.err
}

func (g *cliGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", errors.New("not expected")
}

// runCLIConfig runs, in dir, what a CLI started there would run of its project
// configuration: the SessionStart hooks of every .claude/settings.json under
// dir, and the MCP servers of dir/.mcp.json.
func runCLIConfig(t *testing.T, dir string) {
	t.Helper()
	run := func(name string, args ...string) {
		c := exec.Command(name, args...)
		c.Dir = dir
		var out bytes.Buffer
		c.Stdout, c.Stderr = &out, &out
		if err := c.Run(); err != nil {
			t.Errorf("running %s %q in %s: %v: %s", name, args, dir, err, out.String())
		}
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || filepath.Base(p) != "settings.json" || filepath.Base(filepath.Dir(p)) != ".claude" {
			return nil
		}
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"hooks"`
		}
		b, _ := os.ReadFile(p)
		if json.Unmarshal(b, &s) != nil {
			t.Errorf("%s is not a settings object: %s", p, b)
			return nil
		}
		for _, m := range s.Hooks["SessionStart"] {
			for _, h := range m.Hooks {
				run("sh", "-c", h.Command)
			}
		}
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(dir, ".mcp.json")); err == nil {
		var m struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if json.Unmarshal(b, &m) != nil {
			t.Errorf(".mcp.json is not an object: %s", b)
		}
		for _, srv := range m.MCPServers {
			run(srv.Command, srv.Args...)
		}
	}
}

// newE2EJobs is the job fixture over the REAL adapter and a worktreetest repo
// at the agent's checkout (<root>/widgets).
func newE2EJobs(t *testing.T) (*jobFixture, worktreetest.Repo, string) {
	t.Helper()
	root := t.TempDir()
	r := worktreetest.New(t, filepath.Join(root, "widgets"), filepath.Join(root, "markers"))
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: &hostExecDriver{Noop: provision.MustNewNoop(), root: root},
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newJobFixture(t, testServiceToken)
	f.s.ext.JobWorkspace = adapter
	return f, r, root
}

// TestACommitsOwnHooksAndMCPServersDoNotRunInItsJob: through the job runner,
// the real adapter's scripts and real git. A commit whose .claude/settings.json
// defines a SessionStart hook (and auto-approves its MCP servers), whose
// .mcp.json defines a server and whose deep/.claude/settings.json defines
// another hook — each writing a marker file — is reviewed by a job; the turn's
// "CLI" (cliGateway) runs everything the job's directory configures. After the
// job: none of the commit's three markers exists, while the DEFAULT branch's
// own hook did run there (the CLI ran at all). The agent's dirty checkout is
// untouched and the worktree is gone; the result came from it.
//
// The POSITIVE CONTROL: the same CLI run in a plain worktree of the same commit
// (no overlay) writes all three markers.
func TestACommitsOwnHooksAndMCPServersDoNotRunInItsJob(t *testing.T) {
	f, r, root := newE2EJobs(t)
	// The agent's checkout is dirty: a sha job used to be refused for it.
	if err := os.WriteFile(filepath.Join(r.Checkout, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Checkout, "f"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := &cliGateway{t: t, root: root, resultPath: "out/review.md", body: "# Review\n"}
	f.s.ext.Gateway = cli
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets",
		"sha": r.Commit, "result_path": "out/review.md"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Review\n" {
		t.Fatalf("job = %v/%v %q (%v), want succeeded with the file", out["status"], out["reason"], out["result"], out["error"])
	}
	wt := filepath.Join(root, ".muster-jobs", "1")
	if len(cli.dirs) != 1 || cli.dirs[0] != wt {
		t.Fatalf("the turn ran in %q, want [%s]", cli.dirs, wt)
	}
	want := map[string]string{worktreetest.DefaultHookMarker: wt + "\n"}
	if got := r.MarkerFiles(t); !equalMaps(got, want) {
		t.Fatalf("markers after the job = %q, want only the default branch's hook %q", got, want)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("the job's worktree is still there (lstat: %v)", err)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "HEAD"); got != r.Default {
		t.Fatalf("the checkout's HEAD = %s, want %s (never moved)", got, r.Default)
	}
	// (Git trims the porcelain's leading space.)
	if got := worktreetest.Git(t, r.Checkout, "status", "--porcelain"); got != "M f\n?? wip.txt" {
		t.Fatalf("the checkout's state = %q, want its own edit and untracked file, untouched", got)
	}

	// Positive control: the same CLI in the same commit with no overlay.
	if err := os.Remove(filepath.Join(r.Markers, worktreetest.DefaultHookMarker)); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, ".muster-jobs", "999")
	r.PlainWorktree(t, plain)
	runCLIConfig(t, plain)
	want = map[string]string{
		worktreetest.CommitHookMarker: plain + "\n",
		worktreetest.CommitMCPMarker:  plain + "\n",
		worktreetest.NestedHookMarker: plain + "\n",
	}
	if got := r.MarkerFiles(t); !equalMaps(got, want) {
		t.Fatalf("control markers = %q, want the commit's three %q", got, want)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestAJobsWorktreeIsRemovedHoweverTheJobEnds: through the real adapter, a sha
// job whose turn fails, times out, or panics, and one whose worktree could not
// be fully made, all leave no worktree behind and no registration in git.
func TestAJobsWorktreeIsRemovedHoweverTheJobEnds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cli        func(*cliGateway)
		sha        func(r worktreetest.Repo) string
		wantStatus string
		wantReason string
	}{
		{"failed", func(g *cliGateway) { g.err = &agents.RuntimeError{Status: 429, Type: "rate_limited", Message: "limit"} },
			nil, jobs.StatusFailed, "rate_limited"},
		{"timed out", func(g *cliGateway) { g.block = true }, nil, jobs.StatusTimedOut, jobs.ReasonTimeout},
		{"panicked", func(g *cliGateway) { g.panics = true }, nil, jobs.StatusFailed, jobs.ReasonInternal},
		// A commit nobody has: the worktree add fails after its directory's
		// parent was made.
		{"commit missing", func(*cliGateway) {}, func(worktreetest.Repo) string { return strings.Repeat("ab", 20) },
			jobs.StatusFailed, jobs.ReasonCheckoutFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r, root := newE2EJobs(t)
			f.s.jobSecond = 200 * time.Millisecond
			cli := &cliGateway{t: t, root: root}
			tc.cli(cli)
			f.s.ext.Gateway = cli
			sha := r.Commit
			if tc.sha != nil {
				sha = tc.sha(r)
			}
			out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets",
				"sha": sha, "timeout_s": 5}))
			if out["status"] != tc.wantStatus || out["reason"] != tc.wantReason {
				t.Fatalf("job = %v/%v (%v), want %s/%s", out["status"], out["reason"], out["error"], tc.wantStatus, tc.wantReason)
			}
			if _, err := os.Lstat(filepath.Join(root, ".muster-jobs", "1")); !os.IsNotExist(err) {
				t.Fatalf("the job's worktree is still there (lstat: %v)", err)
			}
			if list := worktreetest.Git(t, r.Checkout, "worktree", "list", "--porcelain"); strings.Contains(list, ".muster-jobs") {
				t.Fatalf("git still lists the job's worktree:\n%s", list)
			}
		})
	}
}

// TestAResultFileIsKeptWhenTheReadFailsOrTheRepositoryTracksIt: a job WITHOUT
// a sha runs in the agent's checkout, through the real adapter and a real git
// checkout. (1) A result file over the 1 MiB limit fails the job
// result_read_failed, and the file — the only copy of the agent's output — is
// still there. (2) A result_path naming a TRACKED file is returned and left in
// place (deleting it would dirty the tree), with a note in error; the tree
// stays clean. (3) A successfully read untracked one is deleted.
func TestAResultFileIsKeptWhenTheReadFailsOrTheRepositoryTracksIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "widgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, repo, "init", "-q")
	const changelog = "# Changelog\n\n- one\n"
	if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, repo, "add", "CHANGELOG.md")
	worktreetest.Git(t, repo, "commit", "-q", "-m", "one")

	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: &hostExecDriver{Noop: provision.MustNewNoop(), root: root},
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newJobFixture(t, testServiceToken)
	f.s.ext.JobWorkspace = adapter

	// (1) Too large to return: the job fails, the file stays.
	big := filepath.Join(repo, "out", "big.md")
	f.s.ext.Gateway = &fileWritingGateway{path: big, body: strings.Repeat("x", jobs.MaxResultBytes+1)}
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/big.md"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonResultReadFailed {
		t.Errorf("oversize job = %v/%v, want failed/%s", out["status"], out["reason"], jobs.ReasonResultReadFailed)
	}
	if st, err := os.Stat(big); err != nil || st.Size() != int64(jobs.MaxResultBytes+1) {
		t.Errorf("after a failed read the result file is gone or changed (stat: %v, %v); it is the only copy", st, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "out")); err != nil {
		t.Fatal(err)
	}

	// (2) A tracked file: returned, kept, the tree clean.
	f.s.ext.Gateway = &fileWritingGateway{}
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "CHANGELOG.md"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != changelog {
		t.Errorf("tracked job = %v/%v %q (%v), want succeeded with the file", out["status"], out["reason"], out["result"], out["error"])
	}
	// The note says what was left and why, through the real adapter's script.
	const wantNote = "the result file CHANGELOG.md is tracked by the repository, so it was left in place, as the turn left it, in the agent's checkout"
	if out["error"] != wantNote {
		t.Errorf("tracked job error = %q, want the note %q", out["error"], wantNote)
	}
	if b, err := os.ReadFile(filepath.Join(repo, "CHANGELOG.md")); err != nil || string(b) != changelog {
		t.Errorf("the tracked result file was deleted or changed: %q %v", b, err)
	}
	if st := worktreetest.Git(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("the tree is dirty after a tracked result_path job: %q", st)
	}

	// (3) An untracked file read successfully is deleted.
	review := filepath.Join(repo, "out", "review.md")
	f.s.ext.Gateway = &fileWritingGateway{path: review, body: "# Review\n"}
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/review.md"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Review\n" {
		t.Errorf("job = %v/%v %q (%v), want succeeded", out["status"], out["reason"], out["result"], out["error"])
	}
	if _, err := os.Stat(review); !os.IsNotExist(err) {
		t.Errorf("the read result file is still in the checkout (stat: %v)", err)
	}
}
