//go:build realcli

package agentprovision

// THE REAL CLI, END TO END: a commit's own hooks and MCP servers do not run in
// its job. This runs INSIDE the claude-code-agent image (the pinned CLI, ccd,
// tmux and git as an agent's pod has them), on a volume at /data:
//
//	CGO_ENABLED=0 go test -c -tags realcli -o realcli.test ./internal/agentprovision
//	docker run --rm --tmpfs /data:rw,exec,mode=2777,uid=0,gid=1000 \
//	  -v "$PWD/realcli.test:/usr/local/bin/realcli.test:ro" claude-code-agent:ci \
//	  realcli.test -test.run '^TestRealCLI' -test.v
//
// (.github/workflows/ci.yml's ccd-image job runs exactly this.) It needs no
// real credential and must never be given one: SessionStart, project hooks and
// MCP servers all run before any request to the provider, so the CLI gets a
// deliberately INVALID token, as the image's smoke test does.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

const realCLIBearer = "realcli-test-bearer"

// TestRealCLIRunsNoHookOrMCPServerFromAJobsCommit: one `ccd serve`, the real
// CLI. A commit's .claude/settings.json defines a SessionStart hook and
// auto-approves its .mcp.json's server, each writing a marker.
//
//   - CONTROL: a plain worktree of the commit (no overlay), started by ccd as a
//     job's session would be. The commit's hook marker AND its MCP server's
//     marker appear: the real CLI does run both from a job directory.
//   - THE JOB: the same commit in a worktree PrepareJobWorktree made (the real
//     overlay script). Neither of the commit's markers appears — while the
//     DEFAULT branch's own SessionStart hook did run there, so the CLI started
//     in it, trusted it, and ran project hooks.
func TestRealCLIRunsNoHookOrMCPServerFromAJobsCommit(t *testing.T) {
	for _, bin := range []string{"claude", "ccd", "tmux", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is not on PATH: this test runs inside the claude-code-agent image", bin)
		}
	}
	ws := filepath.Join(agentspec.ClaudeCodeWorkspacePath, "widgets")
	cfg := "/data/claude-realcli"
	markers := "/tmp/realcli-markers"
	r := worktreetest.New(t, ws, markers)
	a, err := New(Config{Driver: &localExecDriver{Noop: provision.MustNewNoop(), root: agentspec.ClaudeCodeWorkspacePath},
		Store: &recordingStore{agent: fixtureAgent()}, Spec: fixtureSpecConfig()})
	if err != nil {
		t.Fatal(err)
	}
	ag := agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, Repo: "acme/widgets"}

	control := agents.JobWorktreeDir(ws, 1)
	r.PlainWorktree(t, control)
	job, err := a.PrepareJobWorktree(context.Background(), ag, 2, r.Commit)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	run(t, "ccd", "seed", "--config-dir", cfg, "--workspace", ws, "--settings-template", "/etc/ccd/settings.json")
	addr := startCCD(t, cfg, ws)

	// The control first: it shows the CLI runs both from a job directory.
	turn(t, addr, "realcli-control", control)
	waitMarker(t, r, worktreetest.CommitHookMarker, control)
	waitMarker(t, r, worktreetest.CommitMCPMarker, control)
	t.Logf("control: the commit's hook and MCP server ran in %s", control)

	turn(t, addr, "realcli-job", job)
	// Wait for the job's CLI to have started (either branch's SessionStart hook
	// names its directory), then give a server the same time and more before
	// reading what ran — and read BOTH facts, so a run with no overlay reports
	// the commit's markers rather than stopping at the missing default one.
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if strings.Contains(markerLines(t, r, worktreetest.DefaultHookMarker), job+"\n") ||
			strings.Contains(markerLines(t, r, worktreetest.CommitHookMarker), job+"\n") {
			break
		}
	}
	time.Sleep(10 * time.Second)
	for _, m := range []string{worktreetest.CommitHookMarker, worktreetest.CommitMCPMarker, worktreetest.NestedHookMarker} {
		if lines := markerLines(t, r, m); strings.Contains(lines, job+"\n") {
			t.Errorf("the commit's %s ran in the job's worktree:\n%s", m, lines)
		}
	}
	if !strings.Contains(markerLines(t, r, worktreetest.DefaultHookMarker), job+"\n") {
		t.Errorf("the default branch's SessionStart hook never ran in the job's worktree %s: the CLI did not "+
			"start there, trusted, with project hooks", job)
	}
	t.Logf("markers at the end: %q", r.MarkerFiles(t))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %q: %v: %s", name, args, err, out)
	}
}

// startCCD runs `ccd serve` supervising the real CLI in ws, and waits for its
// primary to be ready.
func startCCD(t *testing.T, cfg, ws string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	tmp, err := os.MkdirTemp("", "rc-")
	if err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("realcli-%d", os.Getpid())
	cmd := exec.Command("ccd", "serve")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "MUSTER_GATEWAY_BEARER="+realCLIBearer, "CLAUDE_CONFIG_DIR="+cfg,
		"CCD_WORKSPACE="+ws, "CCD_LISTEN="+addr, "CCD_TMUX_SOCKET="+sock, "CCD_SUPERVISE=1", "TMPDIR="+tmp,
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-REALCLI-DELIBERATELY-INVALID")
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		t.Logf("ccd log:\n%s", logs.String())
	})
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), `"session":"started"`) {
			return addr
		}
	}
	t.Fatalf("ccd's primary never started; ccd log:\n%s", logs.String())
	return ""
}

// turn sends one turn for session key whose CLI works in workdir, as muster
// sends a sha job's. Its OUTCOME does not matter here (the token is invalid,
// so the model turn fails): the lane's CLI starts, in workdir, before it.
func turn(t *testing.T, addr, key, workdir string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/responses",
		strings.NewReader(`{"input":"say hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+realCLIBearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agents.HeaderCCSession, key)
	req.Header.Set(agents.HeaderCCWorkdir, workdir)
	c := &http.Client{Timeout: 3 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		t.Logf("turn %s: %v", key, err)
		return
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("turn %s in %s: %d %s", key, workdir, resp.StatusCode, b)
}

func markerLines(t *testing.T, r worktreetest.Repo, name string) string {
	t.Helper()
	return r.MarkerFiles(t)[name]
}

// waitMarker waits up to a minute for marker name to name dir.
func waitMarker(t *testing.T, r worktreetest.Repo, name, dir string) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if strings.Contains(markerLines(t, r, name), dir+"\n") {
			return
		}
	}
	t.Fatalf("marker %s never named %s; markers: %q", name, dir, r.MarkerFiles(t))
}
