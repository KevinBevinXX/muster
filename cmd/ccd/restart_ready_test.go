package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// killablePane is a laneTerminal whose CLI runs until kill(), then reports an
// exit by SIGKILL (137) — with no SessionEnd, as a SIGKILL or an OOM kill
// leaves it. Its Respawn waits for gate, so a test can look at the restart
// before and after it; it counts the pastes it is given.
type killablePane struct {
	mu         sync.Mutex
	dead       bool
	starts     int
	pastes     int
	gate       chan struct{} // Respawn returns only once this is closed
	respawning chan struct{} // closed when Respawn is first entered
	once       sync.Once
}

func newKillablePane() *killablePane {
	return &killablePane{gate: make(chan struct{}), respawning: make(chan struct{})}
}

func (p *killablePane) Start(context.Context, string, []string) (paneRef, error) {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	return fakeRef, nil
}

func (p *killablePane) Respawn(ctx context.Context, _ paneRef, _ string, _ []string) error {
	p.once.Do(func() { close(p.respawning) })
	select {
	case <-p.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.dead = false
	p.starts++
	p.mu.Unlock()
	return nil
}

func (p *killablePane) Exited(context.Context, paneRef) (paneExit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return paneExit{Dead: true, Known: true, Status: 137, Signal: 9, SignalText: "9"}, nil
	}
	return paneExit{}, nil
}

func (p *killablePane) Paste(context.Context, paneRef, string) error {
	p.mu.Lock()
	p.pastes++
	p.mu.Unlock()
	return nil
}

func (p *killablePane) kill() {
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
}

func (p *killablePane) pasted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pastes
}

func (p *killablePane) Reap(context.Context) error           { return nil }
func (p *killablePane) Enter(context.Context, paneRef) error { return nil }
func (p *killablePane) Alive(context.Context, paneRef) error { return nil }
func (p *killablePane) KillSession(context.Context) error    { return nil }
func fastRestarts(s *supervisor) {
	s.poll, s.backoffMin, s.backoffMax = 5*time.Millisecond, time.Millisecond, time.Millisecond
}
func noPrimaryPID(context.Context, paneRef) (int, error) { return 0, errors.New("no pid in this test") }
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// turnStatus posts one turn to s's /v1/responses — a lane's when key is set,
// else the primary's — and returns the status and body.
func turnStatus(t *testing.T, s *server, key string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hello","stream":false}`))
	r.Header.Set("Authorization", "Bearer b")
	if key != "" {
		r.Header.Set(headerCCSession, key)
	}
	w := httptest.NewRecorder()
	s.gatewayHandler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// killAndRespawn kills pane's CLI (no SessionEnd), lets the supervisor's
// respawn through and waits until it records the second start as running —
// the moment a turn could first reach the NEW CLI's pane.
func killAndRespawn(t *testing.T, pane *killablePane, sup *supervisor) {
	t.Helper()
	pane.kill()
	select {
	case <-pane.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed CLI")
	}
	close(pane.gate)
	waitUntil(t, "the respawned CLI recorded as running", func() bool {
		st := sup.state()
		_, running := sup.inputPane()
		return st.Starts == 2 && running
	})
}

// 🔴 A KILLED CLI'S SessionStart IS NOT THE NEXT CLI'S. A lane's CLI killed
// with no SessionEnd (SIGKILL, OOM) is respawned; once the supervisor records
// that respawn running, a turn must still be refused 503 not_ready — which
// muster re-sends — with nothing pasted, until the NEW CLI's SessionStart.
// Pasting into the booting CLI is a 504 not_submitted, which muster does not
// re-send. Control: after the new SessionStart the turn is pasted.
func TestALaneWhoseCLIWasKilledIsNotReadyUntilTheNewCLIsSessionStart(t *testing.T) {
	pane := newKillablePane()
	l := testLanes(t, 1)
	l.startTimeout = 300 * time.Millisecond
	l.newTerm = func(string) laneTerminal { return pane }
	cfg := l.configDir
	l.newServer = func(term terminal) *server {
		return newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, term, newAuthTracker())
	}
	l.tweak = fastRestarts
	s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg}, nil, newAuthTracker())
	s.lanes = l

	ctx := context.Background()
	ln, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(ln.stop)
	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	if f := ln.waitStarted(ctx, 5*time.Second); f != nil {
		t.Fatalf("control: the first CLI never became ready: %+v", f)
	}
	l.release(ln)

	killAndRespawn(t, pane, ln.sup)
	if code, body := turnStatus(t, s, "sess-a"); code != http.StatusServiceUnavailable || !strings.Contains(body, failNotReady) || pane.pasted() != 0 {
		t.Fatalf("STALE SessionStart: a lane turn after its CLI was killed (no SessionEnd) and respawned, before "+
			"the NEW CLI's SessionStart: status %d, %d paste(s), body %s; want 503 not_ready and no paste",
			code, pane.pasted(), body)
	}

	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	if code, body := turnStatus(t, s, "sess-a"); code != http.StatusGatewayTimeout || pane.pasted() != 1 {
		t.Fatalf("control: after the new CLI's SessionStart the turn is pasted (and, unsubmitted, times out): "+
			"status %d, %d paste(s), body %s", code, pane.pasted(), body)
	}
}

// The same for the PRIMARY, whose runTurn reads the same flag. And a NEW
// SessionStart survives: the exit it follows was cleared before the respawn,
// so no later poll wipes it.
func TestThePrimaryWhoseCLIWasKilledIsNotReadyUntilTheNewCLIsSessionStart(t *testing.T) {
	pane := newKillablePane()
	cfg := t.TempDir()
	s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, pane, newAuthTracker())
	sup := newSupervisor(pane, "claude", cfg, "/data/workspace")
	fastRestarts(sup)
	s.supervisePrimary(sup, noPrimaryPID)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); _ = sup.run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })

	waitUntil(t, "the first start", func() bool { _, ok := sup.inputPane(); return ok })
	s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})

	killAndRespawn(t, pane, sup)
	if code, body := turnStatus(t, s, ""); code != http.StatusServiceUnavailable || !strings.Contains(body, failNotReady) || pane.pasted() != 0 {
		t.Fatalf("STALE SessionStart: a primary turn after its CLI was killed (no SessionEnd) and respawned, before "+
			"the NEW CLI's SessionStart: status %d, %d paste(s), body %s; want 503 not_ready and no paste",
			code, pane.pasted(), body)
	}

	s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})
	time.Sleep(20 * sup.poll) // many polls of the running CLI after the new SessionStart
	if !s.started() {
		t.Fatal("the new CLI's SessionStart was wiped after the respawn")
	}
	if code, body := turnStatus(t, s, ""); code != http.StatusGatewayTimeout || pane.pasted() != 1 {
		t.Fatalf("control: after the new CLI's SessionStart the turn is pasted (and, unsubmitted, times out): "+
			"status %d, %d paste(s), body %s", code, pane.pasted(), body)
	}
}

// 🔴 CLEARED ON THE EXIT, NOT ON THE START. The new CLI can send SessionStart
// while the respawn has not returned yet (tmux runs it first; startedRef), so a
// clear at the start would wipe the one SessionStart that CLI sends, and every
// turn would be not_ready until the next restart. Here it lands mid-Respawn,
// and the turn after the respawn is pasted.
func TestANewCLIsSessionStartBeforeItsRespawnIsRecordedIsKept(t *testing.T) {
	pane := newKillablePane()
	cfg := t.TempDir()
	s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, pane, newAuthTracker())
	sup := newSupervisor(pane, "claude", cfg, "/data/workspace")
	fastRestarts(sup)
	s.supervisePrimary(sup, noPrimaryPID)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); _ = sup.run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })

	waitUntil(t, "the first start", func() bool { _, ok := sup.inputPane(); return ok })
	s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})
	pane.kill()
	select {
	case <-pane.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed CLI")
	}
	s.onHook(hookEvent{Event: "SessionStart", SessionID: "11111111-2222-4333-8444-555555555555"})
	close(pane.gate)
	waitUntil(t, "the respawned CLI recorded as running", func() bool {
		_, running := sup.inputPane()
		return sup.state().Starts == 2 && running
	})
	if code, body := turnStatus(t, s, ""); code != http.StatusGatewayTimeout || pane.pasted() != 1 {
		t.Fatalf("WIPED SessionStart: the new CLI's SessionStart, sent before its respawn was recorded, was lost: "+
			"status %d, %d paste(s), body %s; want the turn pasted", code, pane.pasted(), body)
	}
}
