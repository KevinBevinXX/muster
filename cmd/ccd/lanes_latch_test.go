package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// latchRig is a supervised primary whose lanes are wired the way main.go wires
// them (server.runLanes), each CLI on a killablePane of its own.
type latchRig struct {
	s        *server
	sup      *supervisor
	primary  *killablePane
	lanePane *killablePane
	l        *lanes
}

func newLatchRig(t *testing.T) *latchRig {
	t.Helper()
	cfg := t.TempDir()
	r := &latchRig{primary: newKillablePane(), lanePane: newKillablePane()}
	r.s = newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, r.primary, newAuthTracker())
	r.sup = newSupervisor(r.primary, "claude", cfg, "/data/workspace")
	fastRestarts(r.sup)
	r.s.supervisePrimary(r.sup, noPrimaryPID)
	r.l = testLanes(t, 2)
	r.l.startTimeout = 300 * time.Millisecond
	r.l.newTerm = func(string) laneTerminal { return r.lanePane }
	r.l.newServer = func(term terminal) *server {
		return newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, term, newAuthTracker())
	}
	r.l.tweak = fastRestarts
	r.s.runLanes(r.l)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); _ = r.sup.run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })
	waitUntil(t, "the primary's first start", func() bool { _, ok := r.sup.inputPane(); return ok })
	return r
}

func (r *latchRig) laneStarts() int {
	r.lanePane.mu.Lock()
	defer r.lanePane.mu.Unlock()
	return r.lanePane.starts
}

// 🔴 A HEALTHY LANE IS NOT HOSTAGE TO THE PRIMARY'S RESTART. A lane may not
// START before the primary has started once in this process (lanes.go), but
// once it has, the primary's CLI dying (SIGKILL, OOM: no SessionEnd) and
// sitting in its respawn must not refuse a turn to a lane that is up — muster's
// interactive chat does not re-send a 503 not_ready, so the user's message was
// lost. Here the primary is killed and HELD in its respawn; the lane's turn
// must reach the lane's pane (pasted; unsubmitted, so it times out 504).
func TestALaneTurnIsServedWhileThePrimaryRestarts(t *testing.T) {
	r := newLatchRig(t)
	r.s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})

	ctx := context.Background()
	ln, f := r.l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatalf("control: a lane starts once the primary has started: %+v", f)
	}
	t.Cleanup(ln.stop)
	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	if f := ln.waitStarted(ctx, 5*time.Second); f != nil {
		t.Fatalf("control: the lane never became ready: %+v", f)
	}
	r.l.release(ln)

	r.primary.kill()
	select {
	case <-r.primary.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed primary")
	}
	// Held in Respawn (gate not closed): the primary is down, its SessionStart cleared.
	if r.s.started() {
		t.Fatal("control: the killed primary's SessionStart was not cleared (#156)")
	}
	if code, body := turnStatus(t, r.s, "sess-a"); code != http.StatusGatewayTimeout || r.lanePane.pasted() != 1 {
		t.Fatalf("LANE HOSTAGE TO THE PRIMARY: a turn to a running, ready lane while the primary CLI restarts: "+
			"status %d, %d paste(s) into the lane, body %s; want the turn pasted into the lane (504 unsubmitted here)",
			code, r.lanePane.pasted(), body)
	}
	close(r.primary.gate)
}

// The gate the latch keeps: no lane STARTS before the primary's first
// SessionStart in this process (lanes.go — the primary's first start may be
// `--continue`, which must not find a lane's transcript). The primary's CLI is
// running here but has not sent SessionStart; the lane is refused 503 not_ready
// and never started. Control: after the SessionStart it starts.
func TestNoLaneStartsBeforeThePrimarysFirstSessionStart(t *testing.T) {
	r := newLatchRig(t)
	code, body := turnStatus(t, r.s, "sess-a")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, failNotReady) ||
		!strings.Contains(body, "the original Claude Code session has not started yet") || r.laneStarts() != 0 {
		t.Fatalf("a lane turn before the primary's first SessionStart: status %d, %d lane start(s), body %s; "+
			"want 503 not_ready (the original session has not started) and no lane started", code, r.laneStarts(), body)
	}
	r.s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})
	ln, f := r.l.acquire(context.Background(), "sess-a", "")
	if f != nil {
		t.Fatalf("control: after the primary's SessionStart a lane starts: %+v", f)
	}
	t.Cleanup(ln.stop)
	r.l.release(ln)
	waitUntil(t, "the lane's CLI started", func() bool { return r.laneStarts() == 1 })
}

// 🟢 cliExited clears busy too. A lane whose CLI is killed mid-turn — the
// prompt submitted, no Stop, and no ccd turn holding it (typed in its terminal,
// or the ccd turn already gave up) — is idle while its CLI restarts, so a new
// session at the cap may stop it (its conversation is on disk) and /healthz
// does not report a dead CLI as busy. Without the clear the lane stayed busy
// until the NEW CLI's SessionStart, refusing the new session 503 capacity.
func TestALaneKilledMidTurnIsEvictableWhileItRestarts(t *testing.T) {
	pane := newKillablePane()
	l := testLanes(t, 1)
	l.newTerm = func(string) laneTerminal { return pane }
	l.tweak = fastRestarts
	ln, f := l.acquire(context.Background(), "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(ln.stop)
	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	ln.srv.onHook(hookEvent{Event: "UserPromptSubmit", Prompt: "typed in the terminal"})
	l.release(ln)
	if ln.srv.idle() {
		t.Fatal("control: a lane with a submitted prompt and no Stop is not idle")
	}

	pane.kill()
	select {
	case <-pane.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed lane CLI")
	}
	waitUntil(t, "the killed lane CLI's exit recorded", func() bool { return ln.sup.state().Starts == 1 && !ln.srv.started() })
	if _, _, busy := ln.srv.snapshot(); busy || !ln.srv.idle() {
		t.Fatalf("BUSY AFTER DEATH: a lane killed mid-turn, held in its respawn: busy=%v idle=%v; want not busy and idle", busy, ln.srv.idle())
	}
	l.mu.Lock()
	evict, stuck := l.pickEvictLocked()
	l.mu.Unlock()
	if evict != ln || stuck {
		t.Fatalf("a lane killed mid-turn is not chosen for eviction while it restarts (got %v, stuck=%v)", evict, stuck)
	}
	close(pane.gate)
}
