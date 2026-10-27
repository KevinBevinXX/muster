//go:build tmuxit

package main

// The lanes (lanes.go) on a REAL tmux server, through the WHOLE `ccd serve`
// binary and muster's own client — the production wiring end to end, with the
// fake CLI (fakeclaude_test.go) in every pane:
//
//	go test -tags tmuxit -race -count=1 -run TestTmuxLane ./cmd/ccd/

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
)

// laneChat sends p as session key's turn (muster's route headers on muster's
// own client) and returns the reply or the error.
func (r *serveRig) laneChat(key, model, p string) (string, error) {
	h := http.Header{}
	h.Set(agents.HeaderCCSession, key)
	if model != "" {
		h.Set(agents.HeaderCCModel, model)
	}
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: agents.SessionRuntimeClaudeCode, Header: h})
	gw := mustersGatewayNoT(r.gwAddr)
	return gw.Chat(ctx, contractAgent, key, p, nil)
}

// wantReply is the fake CLI's reply to p from a CLI started on model ("" = the
// primary, which names none).
func wantReply(p, model string) string {
	sum := sha256.Sum256([]byte(p))
	first := fmt.Sprintf("received %d bytes", len(p))
	if model != "" {
		first += " on " + model
	}
	return first + "\n\nsha256 " + hex.EncodeToString(sum[:])
}

func (r *serveRig) mustLaneChat(t *testing.T, key, model, p string) {
	t.Helper()
	got, err := r.laneChat(key, model, p)
	if err != nil || got != wantReply(p, model) {
		t.Fatalf("session %q (%s) prompt %q: reply %q err %v; ccd log:\n%s", key, model, p, got, err, r.logs.String())
	}
}

// transcript is the content of session id's transcript ("" when it has none).
func (r *serveRig) transcript(t *testing.T, id string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", id+".jsonl"))
	if len(files) != 1 {
		return ""
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r *serveRig) transcriptPath(t *testing.T, id string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", id+".jsonl"))
	if len(files) != 1 {
		t.Fatalf("transcript %s: %v", id, files)
	}
	return files[0]
}

func (r *serveRig) primaryID(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.cfg, primaryRecordFile))
	if err != nil {
		t.Fatalf("no primary session id recorded: %v; ccd log:\n%s", err, r.logs.String())
	}
	return strings.TrimSpace(string(b))
}

func (r *serveRig) hasTmuxSession(name string) bool {
	return exec.Command(r.tmuxBin, "-L", r.sock, "has-session", "-t", "="+name).Run() == nil
}

func (r *serveRig) argvLog(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.cfg, "fake-argv.log"))
	return string(b)
}

// has reports whether the transcript holds a user record with exactly prompt.
func has(transcript, prompt string) bool {
	return strings.Contains(transcript, `"content":"`+prompt+`"`)
}

// 🔴 TWO SESSIONS, TWO CLIs, TWO MODELS, AT THE SAME TIME — and the primary
// untouched:
//
//   - two explicit sessions' turns run CONCURRENTLY (each holds its CLI busy for
//     1.5s); routed to one CLI, the second would be refused 409 busy;
//   - each reply names its own model, and each CLI was started with its own
//     --model and --session-id (the fake records its argv);
//   - each session's prompts are in its own transcript and nowhere else, and a
//     second turn lands in the same one;
//   - a request with no session header still goes to the primary, whose
//     transcript holds only its own prompt;
//   - /healthz lists both lanes.
func TestTmuxLaneTwoSessionsRunInTheirOwnCLIsOnTheirOwnModels(t *testing.T) {
	// ccd itself runs with an inherited `claude -p`-style stamp; no CLI it
	// starts may see it (execArgv), or the interactive primary's transcript
	// would read as sdk-cli.
	r := startServe(t, "CLAUDE_CODE_ENTRYPOINT=sdk-cli")
	r.waitHealth(t, "primary start", primaryReady)
	r.chat(t, "primary first")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct{ key, model, p string }{{"sess-a", "opus", "SLOW alpha"}, {"sess-b", "sonnet", "SLOW beta"}} {
		wg.Add(1)
		go func(i int, key, model, p string) {
			defer wg.Done()
			got, err := r.laneChat(key, model, p)
			if err == nil && got != wantReply(p, model) {
				err = fmt.Errorf("reply %q, want %q", got, wantReply(p, model))
			}
			errs[i] = err
		}(i, c.key, c.model, c.p)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent session %d: %v; ccd log:\n%s", i, err, r.logs.String())
		}
	}
	r.mustLaneChat(t, "sess-a", "opus", "alpha again")

	a, b, p := r.transcript(t, laneUUID("sess-a")), r.transcript(t, laneUUID("sess-b")), r.transcript(t, r.primaryID(t))
	for _, c := range []struct {
		name, text   string
		want, absent []string
	}{
		{"sess-a", a, []string{"SLOW alpha", "alpha again"}, []string{"SLOW beta", "primary first"}},
		{"sess-b", b, []string{"SLOW beta"}, []string{"SLOW alpha", "alpha again", "primary first"}},
		{"primary", p, []string{"primary first"}, []string{"SLOW alpha", "SLOW beta", "alpha again"}},
	} {
		for _, w := range c.want {
			if !has(c.text, w) {
				t.Errorf("%s's transcript lacks %q", c.name, w)
			}
		}
		for _, w := range c.absent {
			if has(c.text, w) {
				t.Errorf("%s's transcript holds another session's %q", c.name, w)
			}
		}
	}
	argv := r.argvLog(t)
	if n := strings.Count(argv, " entrypoint= cwd="); n < 3 || strings.Contains(argv, "entrypoint=sdk-cli") {
		t.Errorf("CLAUDE_CODE_ENTRYPOINT REACHED A CLI: %d start(s) without it (want >= 3: the primary and two lanes); argv log:\n%s", n, argv)
	}
	for _, c := range []struct{ key, model string }{{"sess-a", "opus"}, {"sess-b", "sonnet"}} {
		want := fmt.Sprintf("sid=%s source=startup model=%s lane=%s args=[\"--model\" %q \"--session-id\" %q]",
			laneUUID(c.key), c.model, laneID(c.key), c.model, laneUUID(c.key))
		if !strings.Contains(argv, want) {
			t.Errorf("no CLI started as\n  %s\nargv log:\n%s", want, argv)
		}
		if !r.hasTmuxSession("cc-" + laneID(c.key)) {
			t.Errorf("no tmux session cc-%s for %s", laneID(c.key), c.key)
		}
	}
	h := r.waitHealth(t, "two lanes", func(h health) bool { return len(h.Lanes) == 2 })
	models := map[string]string{}
	for _, l := range h.Lanes {
		models[l.ID] = l.Model
		if l.CLI != cliRunning || l.Session != sessionStartedState {
			t.Errorf("lane %+v is not running and started", l)
		}
	}
	if models[laneID("sess-a")] != "opus" || models[laneID("sess-b")] != "sonnet" {
		t.Errorf("/healthz lanes: %+v", h.Lanes)
	}
}

// 🔴 EACH CLI RESUMES ITS OWN CONVERSATION:
//
//   - a lane's CLI that exits is restarted with --resume <its id>, and its next
//     turn lands in its one transcript;
//   - THE PRIMARY, restarted AFTER a lane's transcript became the newest in the
//     directory, resumes ITS OWN session by id — `--continue` (what it ran
//     before lanes) would have resumed the lane's.
func TestTmuxLaneEachCLIResumesItsOwnConversation(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "primary start", primaryReady)
	r.chat(t, "p1")
	primary := r.primaryID(t)
	r.mustLaneChat(t, "sess-a", "haiku", "a1")
	lane := laneUUID("sess-a")

	// The lane's CLI exits (an operator's /exit in its window).
	target := "=cc-" + laneID("sess-a") + ":"
	r.tmux(t, "send-keys", "-t", target, "-l", "/exit")
	r.tmux(t, "send-keys", "-t", target, "Enter")
	resumed := fmt.Sprintf("sid=%s source=resume model=haiku lane=%s args=[\"--model\" \"haiku\" \"--resume\" %q]", lane, laneID("sess-a"), lane)
	waitLane(t, "the lane's restart", func() bool { return strings.Contains(r.argvLog(t), resumed) }, r)
	r.waitHealth(t, "lane restarted", func(h health) bool {
		return len(h.Lanes) == 1 && h.Lanes[0].Session == sessionStartedState
	})
	r.mustLaneChat(t, "sess-a", "haiku", "a2")

	// Another conversation, not a lane's by name, is the newest in the
	// directory — what a lane's /clear, or a `claude` someone ran in the pod,
	// leaves. Only the RECORD tells the primary's own from it.
	stray := filepath.Join(filepath.Dir(r.transcriptPath(t, primary)), "0f0e0d0c-0b0a-4908-8706-050403020100.jsonl")
	// An interactive CLI's transcript, like the primary's: only the record
	// tells them apart (not the entrypoint filter, newestPrimaryTranscript).
	if err := os.WriteFile(stray, []byte(cliTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(stray, later, later); err != nil {
		t.Fatal(err)
	}

	// The primary exits; a lane's transcript and the stray are both newer than its own.
	r.tmux(t, "send-keys", "-t", "=cc:", "-l", "/exit")
	r.tmux(t, "send-keys", "-t", "=cc:", "Enter")
	r.waitHealth(t, "primary restart", func(h health) bool {
		return primaryReady(h) && h.Supervisor.Starts == 2
	})
	if want := fmt.Sprintf("sid=%s source=resume model= lane= args=[\"--resume\" %q]", primary, primary); !strings.Contains(r.argvLog(t), want) {
		t.Fatalf("the primary did not resume its own session:\n  want %s\nargv log:\n%s", want, r.argvLog(t))
	}
	r.chat(t, "p2")

	pt, lt := r.transcript(t, primary), r.transcript(t, lane)
	if !has(pt, "p1") || !has(pt, "p2") || has(pt, "a1") || has(pt, "a2") {
		t.Errorf("the primary's transcript: p1=%v p2=%v a1=%v a2=%v; want only p1 and p2", has(pt, "p1"), has(pt, "p2"), has(pt, "a1"), has(pt, "a2"))
	}
	if !has(lt, "a1") || !has(lt, "a2") || has(lt, "p2") {
		t.Errorf("the lane's transcript: a1=%v a2=%v p2=%v; want a1 and a2 only", has(lt, "a1"), has(lt, "a2"), has(lt, "p2"))
	}
	if files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl")); len(files) != 3 {
		t.Errorf("want the two transcripts and the stray (resumed, never forked), have %v", files)
	}
}

// 🔴 A `claude` RUN INSIDE THE PRIMARY IS NOT THE PRIMARY. The agent's own shell
// (here the fake's `!` shell mode, typed in the pane) starts a nested CLI that
// inherits CLAUDE_CONFIG_DIR and the hook settings and has no CCD_LANE; its
// SessionStart reaches ccd as a primary hook. Its session id must not replace
// the primary's record — which the next primary restart resumes.
func TestTmuxLaneANestedCLIIsNotRecordedAsThePrimary(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "primary start", primaryReady)
	r.chat(t, "p1")
	primary := r.primaryID(t)

	r.tmux(t, "send-keys", "-t", "=cc:", "-l", "!claude")
	r.tmux(t, "send-keys", "-t", "=cc:", "Enter")
	var nested string
	waitLane(t, "the nested CLI's start", func() bool {
		for _, line := range strings.Split(r.argvLog(t), "\n") {
			if f := strings.Fields(line); len(f) > 2 && f[1] == "source=startup" && f[3] == "lane=" &&
				strings.TrimPrefix(f[0], "sid=") != primary {
				nested = strings.TrimPrefix(f[0], "sid=")
				return true
			}
		}
		return false
	}, r)
	// Wait until ccd has HANDLED the nested CLI's SessionStart — it either
	// replaced the record (the bug) or logged declining it — so an unchanged
	// record below is never a hook that simply had not arrived yet.
	waitLane(t, "the nested CLI's SessionStart reaching ccd", func() bool {
		return r.primaryID(t) != primary || strings.Contains(r.logs.String(), "not recording session "+nested)
	}, r)
	if got := r.primaryID(t); got != primary {
		t.Fatalf("the primary's record is now %q (the nested CLI's is %q); want the primary's own %q", got, nested, primary)
	}
}

// 🔴 A LANE'S PERMISSION PROMPT SHOWS ON THE AGENT'S BADGE, through the whole
// binary: ccd posts blocked=true to muster's /agent/blocked when a LANE's CLI
// stops at the dialog — not only the primary's — and /healthz names the lane as
// waiting.
func TestTmuxLaneAPermissionPromptInALaneReachesTheBadge(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/blocked":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			posts = append(posts, strings.TrimSpace(string(b)))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/agent/connectors":
			_, _ = io.WriteString(w, `{"connectors":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)
	r := startServe(t, "MUSTER_CONNECT_URL="+fake.URL, "MUSTER_HOOK_TOKEN=agent-own-token",
		"HOME="+t.TempDir(), "CCD_TURN_TIMEOUT=2s")
	r.waitHealth(t, "primary start", primaryReady)
	r.mustLaneChat(t, "sess-p", "opus", "first")

	if _, err := r.laneChat("sess-p", "opus", "ASK-PERMISSION run it"); err == nil {
		t.Fatal("a turn stopped at a permission dialog returned a reply")
	}
	waitLane(t, "blocked=true posted for the lane's prompt", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(posts) > 0 && posts[len(posts)-1] == `{"blocked":true}`
	}, r)
	h := r.waitHealth(t, "the lane waiting", func(h health) bool { return len(h.Lanes) == 1 })
	if !h.Lanes[0].AwaitingApproval || h.Lanes[0].AwaitingSince == "" || h.Lanes[0].Stuck {
		t.Errorf("/healthz lane: %+v; want awaiting approval, not yet stuck", h.Lanes[0])
	}
}

// 🔴 THE CAP: with CCD_MAX_SESSIONS=1 a second session stops the first (idle)
// one's CLI, whose next turn starts it again and resumes its conversation; and
// while the running one is mid-turn, another session is refused 503 capacity
// rather than stopping a turn or queueing.
func TestTmuxLaneTheCapStopsTheIdleLaneAndRefusesWhenAllAreBusy(t *testing.T) {
	r := startServe(t, "CCD_MAX_SESSIONS=1")
	r.waitHealth(t, "primary start", primaryReady)
	r.mustLaneChat(t, "sess-a", "opus", "a1")
	r.mustLaneChat(t, "sess-b", "sonnet", "b1")
	if r.hasTmuxSession("cc-"+laneID("sess-a")) || !r.hasTmuxSession("cc-"+laneID("sess-b")) {
		t.Fatalf("after b at the cap: a running=%v b running=%v; want a stopped, b running",
			r.hasTmuxSession("cc-"+laneID("sess-a")), r.hasTmuxSession("cc-"+laneID("sess-b")))
	}
	r.mustLaneChat(t, "sess-a", "opus", "a2")
	if a := r.transcript(t, laneUUID("sess-a")); !has(a, "a1") || !has(a, "a2") {
		t.Errorf("sess-a after its restart: a1=%v a2=%v; want both in one transcript", has(a, "a1"), has(a, "a2"))
	}

	done := make(chan error, 1)
	go func() { _, err := r.laneChat("sess-a", "opus", "SLOW a3"); done <- err }()
	waitLane(t, "sess-a mid-turn", func() bool {
		h := r.waitHealth(t, "health", func(health) bool { return true })
		return len(h.Lanes) == 1 && h.Lanes[0].Busy
	}, r)
	_, err := r.laneChat("sess-b", "sonnet", "b2")
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) || rt.Type != failCapacity {
		t.Errorf("a second session while the only lane is mid-turn: %v; want a typed %s refusal", err, failCapacity)
	}
	// BOUNDED: an unbounded receive hung here until the binary's 10-minute
	// timeout panicked, which skips every t.Cleanup — the tmux server and the
	// ccd it serves were left running. A Fatalf returns, so they run.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the turn in progress was disturbed: %v", err)
		}
	case <-time.After(laneTurnWait):
		t.Fatalf("the turn in progress never returned within %s; ccd log:\n%s", laneTurnWait, r.logs.String())
	}
}

// laneTurnWait bounds waiting on a turn the test started in a goroutine. The
// turn takes 1.5s (SLOW); the bound is generous for a loaded host and far
// below the binary's -timeout.
const laneTurnWait = 30 * time.Second

// A model ccd will not pass to --model is refused before any CLI starts.
func TestTmuxLaneABadModelIsRefusedAndStartsNothing(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "primary start", primaryReady)
	_, err := r.laneChat("sess-x", "opus --dangerously-skip-permissions", "x")
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) || rt.Type != failInvalidInput {
		t.Fatalf("a bad model: %v; want %s", err, failInvalidInput)
	}
	if r.hasTmuxSession("cc-" + laneID("sess-x")) {
		t.Fatal("a refused model still started a CLI")
	}
}

// mustersGatewayNoT is mustersGateway for a goroutine (no *testing.T to fail).
func mustersGatewayNoT(addr string) *agentgateway.Gateway {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{Driver: staticEndpoint{Host: host, Port: p},
		Runtime: agentgateway.HooksSHA256(), Model: "ccd-ignores-this"})
	if err != nil {
		panic(err)
	}
	return gw
}

func waitLane(t *testing.T, what string, ok func() bool, r *serveRig) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s: never happened; ccd log:\n%s", what, r.logs.String())
}

// 🔴 A SLOW tmux new-session: every CLI runs, and sends SessionStart, ~700ms
// before ccd learns its pane. The primary's first turn after primaryReady, and
// a lane's first turn (lane.waitStarted), must both be served — not refused
// 503 not_ready for a pane the supervisor has not recorded yet — and the
// primary's SessionStart is still recorded as its own (supervisePrimary).
func TestTmuxLaneASlowNewSessionStillServesTheFirstTurns(t *testing.T) {
	r := startServeAt(t, "workspace", slowNewSession(t, 700*time.Millisecond))
	r.waitHealth(t, "primary start", primaryReady)
	r.chat(t, "primary first")
	r.mustLaneChat(t, "sess-a", "opus", "alpha")
	if got := r.primaryID(t); !has(r.transcript(t, got), "primary first") {
		t.Fatalf("recorded primary session %s does not hold the primary's turn", got)
	}
}
