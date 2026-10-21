package api

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
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

// TestAResultPathJobDoesNotLeaveTheTreeDirtyForTheNextSHAJob: through the real
// adapter and a real git checkout. Job 1 checks out a commit, the turn writes
// out/review.md into the checkout (untracked), and the job returns it. Job 2
// asks for another commit. The checkout refuses a dirty tree, so unless the
// runner deleted job 1's own result file, job 2 fails checkout_failed.
func TestAResultPathJobDoesNotLeaveTheTreeDirtyForTheNextSHAJob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "widgets")
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
	second := git("rev-parse", "HEAD")

	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: &hostExecDriver{Noop: provision.MustNewNoop(), root: root},
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newJobFixture(t, testServiceToken)
	f.s.ext.JobWorkspace = adapter
	result := filepath.Join(repo, "out", "review.md")
	f.s.ext.Gateway = &fileWritingGateway{path: result, body: "# Review of one\n"}

	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "sha": first, "result_path": "out/review.md"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Review of one\n" {
		t.Fatalf("job 1 = %v/%v %q (%v)", out["status"], out["reason"], out["result"], out["error"])
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Errorf("job 1's result file is still in the checkout (stat: %v)", err)
	}

	f.s.ext.Gateway = &fileWritingGateway{}
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "sha": second}))
	if out["status"] != jobs.StatusSucceeded {
		t.Fatalf("job 2 = %v/%v %q; want succeeded (the tree must not be dirty from job 1)", out["status"], out["reason"], out["error"])
	}
	if head := git("rev-parse", "HEAD"); head != second {
		t.Fatalf("HEAD = %s, want %s", head, second)
	}
}

// TestAResultFileIsKeptWhenTheReadFailsOrTheRepositoryTracksIt: through the
// real adapter and a real git checkout. (1) A result file over the 1 MiB limit
// fails the job result_read_failed, and the file — the only copy of the
// agent's output — is still there. (2) A result_path naming a TRACKED file is
// returned and left in place (deleting it would dirty the tree), with a note
// in error; the tree stays clean.
func TestAResultFileIsKeptWhenTheReadFailsOrTheRepositoryTracksIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "widgets")
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
	const changelog = "# Changelog\n\n- one\n"
	if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "CHANGELOG.md")
	git("commit", "-q", "-m", "one")

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
	if b, err := os.ReadFile(filepath.Join(repo, "CHANGELOG.md")); err != nil || string(b) != changelog {
		t.Errorf("the tracked result file was deleted or changed: %q %v", b, err)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Errorf("the tree is dirty after a tracked result_path job: %q", st)
	}
}
