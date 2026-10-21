//go:build tmuxit

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// laneChatIn is laneChat with the session's CLI working in workdir
// (agents.HeaderCCWorkdir), as muster sends a sha job's turn.
func (r *serveRig) laneChatIn(key, workdir, p string) (string, error) {
	reply, _, err := r.laneChatInEcho(key, workdir, p)
	return reply, err
}

// laneChatInEcho is laneChatIn also returning ccd's X-Muster-CC-Workdir-Used
// answer ("" when there was none).
func (r *serveRig) laneChatInEcho(key, workdir, p string) (string, string, error) {
	h := http.Header{}
	h.Set(agents.HeaderCCSession, key)
	h.Set(agents.HeaderCCWorkdir, workdir)
	var used string
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: agents.SessionRuntimeClaudeCode, Header: h,
		OnResponse: func(rh http.Header) { used = rh.Get(agents.HeaderCCWorkdirUsed) }})
	reply, err := mustersGatewayNoT(r.gwAddr).Chat(ctx, contractAgent, key, p, nil)
	return reply, used, err
}

// 🔴 A JOB'S SESSION RUNS ITS OWN CLI IN THE JOB'S WORKTREE: through the whole
// `ccd serve` and muster's own client, with the fake CLI — which, like the real
// one, stops at its trust screen in a directory that is neither trusted nor a
// worktree of a trusted checkout. A turn naming a job worktree is answered by
// a CLI whose cwd IS that worktree, started fresh (--session-id) and writing
// its transcript under that directory's project; ccd wrote no trust for it. The same session cannot move
// to another directory, and a directory that is not a job worktree of this
// workspace — the workspace itself, or one not created — starts no CLI.
func TestTmuxLaneRunsAJobInItsWorktree(t *testing.T) {
	rig := startServe(t)
	rig.waitHealth(t, "primary start", primaryReady)
	// The workspace is a checkout, and the job directory a git worktree of it,
	// as muster makes one; ccd writes no trust for it.
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", rig.ws, "-c", "user.name=t", "-c", "user.email=t@example.test",
			"-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	wt := agents.JobWorktreeDir(rig.ws, 7)
	git("worktree", "add", "-q", "--detach", wt, "HEAD")
	cfgBefore, err := os.ReadFile(filepath.Join(rig.cfg, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	const key, prompt = "job-session-7", "review the commit"
	got, used, err := rig.laneChatInEcho(key, wt, prompt)
	if err != nil || got != wantReply(prompt, "") {
		t.Fatalf("job turn: reply %q err %v; ccd log:\n%s", got, err, rig.logs.String())
	}
	if used != wt {
		t.Fatalf("ccd answered %s %q, want the worktree %q", agents.HeaderCCWorkdirUsed, used, wt)
	}
	argv := rig.argvLog(t)
	var line string
	for _, l := range strings.Split(argv, "\n") {
		if strings.Contains(l, "lane="+laneID(key)) {
			line = l
		}
	}
	if !strings.Contains(line, "cwd="+wt+" ") || !strings.Contains(line, `"--session-id" "`+laneUUID(key)+`"`) {
		t.Fatalf("the job's CLI = %q, want a fresh --session-id %s with cwd=%s; all:\n%s", line, laneUUID(key), wt, argv)
	}
	if !strings.Contains(rig.transcriptPath(t, laneUUID(key)), string(filepath.Separator)+nonAlnum.ReplaceAllString(wt, "-")+string(filepath.Separator)) {
		t.Fatalf("the job's transcript %s is not under the worktree's project", rig.transcriptPath(t, laneUUID(key)))
	}

	if b, _ := os.ReadFile(filepath.Join(rig.cfg, ".claude.json")); !strings.Contains(string(b), `"hasTrustDialogAccepted"`) ||
		strings.Contains(string(b), wt) {
		t.Fatalf("ccd wrote a project entry for the job worktree (.claude.json was %d bytes, is %d):\n%s", len(cfgBefore), len(b), b)
	}
	var rt *agents.RuntimeError
	git("worktree", "add", "-q", "--detach", agents.JobWorktreeDir(rig.ws, 8), "HEAD")
	if _, err := rig.laneChatIn(key, agents.JobWorktreeDir(rig.ws, 8), "again"); !errors.As(err, &rt) || rt.Type != failBadRequest {
		t.Fatalf("the same session in another directory = %v, want %s", err, failBadRequest)
	}
	for _, tc := range []struct{ dir, typ string }{
		{rig.ws, failInvalidInput},
		{agents.JobWorktreeDir(rig.ws, 9), failNotReady},
	} {
		k := "job-refused-" + filepath.Base(tc.dir)
		if _, err := rig.laneChatIn(k, tc.dir, "x"); !errors.As(err, &rt) || rt.Type != tc.typ {
			t.Errorf("workdir %s = %v, want %s", tc.dir, err, tc.typ)
		}
		if rig.hasTmuxSession("cc-" + laneID(k)) {
			t.Errorf("workdir %s started a CLI", tc.dir)
		}
	}
}
