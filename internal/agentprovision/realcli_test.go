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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/podagent"
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
		Store: &recordingStore{agent: fixtureAgent()}, Spec: fixtureSpecConfig(), AnonymousGitBase: r.AnonBase})
	if err != nil {
		t.Fatal(err)
	}
	ag := agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, Repo: "acme/widgets"}

	control := agents.JobWorktreeDir(ws, 1)
	r.PlainWorktree(t, control)
	job, err := a.PrepareJobWorktree(context.Background(), ag, 2, worktreetest.RepoName, worktreetest.PR, r.Commit)
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

// The public pull request the proxy test fetches: this repository's own
// PR 182, merged, whose head is publicSHA.
const (
	publicRepo = "ZacxDev/muster"
	publicPR   = 182
	publicSHA  = "6b60e1cc9c580d7bd162bae5ed2831840588cda9"
)

// TestRealCLIPullRequestFetchBypassesTheProxyRouting: in the image, with the
// routing ccd writes for an agent holding an approved grant on the `github`
// connector (podagent.ApplyGitRouting: every https://github.com/ URL rewritten
// to muster's proxy, with the agent's token as a header) pointed at a local
// recorder standing in for the proxy:
//
//   - CONTROL: a plain `git ls-remote` of the public repository goes to the
//     recorder, token header and all — the routing is live;
//   - THE JOB: PrepareJobWorktree with the production base fetches pull
//     request 182 of the public repository from github.com itself — the
//     recorder sees NOTHING — and the worktree is at the pull request's head.
//
// It needs the network (github.com over HTTPS), as an agent's pod has it.
func TestRealCLIPullRequestFetchBypassesTheProxyRouting(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" token="+r.Header.Get("X-Muster-Token"))
		mu.Unlock()
		http.Error(w, "the recorder serves nothing", http.StatusForbidden)
	}))
	defer srv.Close()
	conn := podagent.AgentConnector{Name: "github", Exists: true, Hosts: []string{"github.com"}, Grant: "approved"}
	if _, err := podagent.ApplyGitRouting(srv.URL, "realcli-agent-token", conn); err != nil {
		t.Fatalf("write the proxy routing: %v", err)
	}
	t.Cleanup(func() {
		conn.Grant = ""
		_, _ = podagent.ApplyGitRouting(srv.URL, "realcli-agent-token", conn)
	})
	count := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}

	_ = exec.Command("git", "ls-remote", "https://github.com/"+publicRepo, fmt.Sprintf("refs/pull/%d/head", publicPR)).Run()
	ctl := count()
	if len(ctl) == 0 || !strings.Contains(ctl[0], "/connect/github/github.com/"+publicRepo) || !strings.Contains(ctl[0], "token=realcli-agent-token") {
		t.Fatalf("control: the proxy routing did not catch a plain fetch of github.com: recorder saw %q", ctl)
	}
	t.Logf("control: a plain git ls-remote went to the proxy: %q", ctl)

	main := filepath.Join(agentspec.ClaudeCodeWorkspacePath, "muster")
	worktreetest.Git(t, "", "init", "-q", "-b", "main", main)
	if err := os.WriteFile(filepath.Join(main, "CLAUDE.md"), []byte("# default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, main, "add", "--", "CLAUDE.md")
	worktreetest.Git(t, main, "commit", "-q", "-m", "default")
	worktreetest.Git(t, main, "update-ref", "refs/remotes/origin/main", "HEAD")
	worktreetest.Git(t, main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	a, err := New(Config{Driver: &localExecDriver{Noop: provision.MustNewNoop(), root: agentspec.ClaudeCodeWorkspacePath},
		Store: &recordingStore{agent: fixtureAgent()}, Spec: fixtureSpecConfig()})
	if err != nil {
		t.Fatal(err)
	}
	ag := agents.Agent{ID: 8, Name: "keen-wren", Kind: agents.KindClaudeCode, Repo: publicRepo}
	dir, err := a.PrepareJobWorktree(context.Background(), ag, 3, publicRepo, publicPR, publicSHA)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := count(); len(got) != len(ctl) {
		t.Fatalf("the job's fetch reached the proxy: recorder saw %q after the control's %q", got[len(ctl):], ctl)
	}
	if head := worktreetest.Git(t, dir, "rev-parse", "HEAD"); head != publicSHA {
		t.Fatalf("worktree HEAD = %s, want the pull request's head %s", head, publicSHA)
	}
	t.Logf("the job fetched pull request %d of %s from github.com directly (recorder: %d request(s), all the control's); worktree at %s",
		publicPR, publicRepo, len(count()), publicSHA)
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
	if used := resp.Header.Get(agents.HeaderCCWorkdirUsed); used != workdir {
		t.Errorf("ccd answered %s %q, want the job's worktree %q", agents.HeaderCCWorkdirUsed, used, workdir)
	}
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
