package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stillPane is a laneTerminal whose CLI starts and never exits; it counts the
// sessions killed.
type stillPane struct {
	mu     sync.Mutex
	killed int
}

func (p *stillPane) Start(context.Context, string, []string) (paneRef, error) { return fakeRef, nil }
func (p *stillPane) Respawn(context.Context, paneRef, string, []string) error { return nil }
func (p *stillPane) Exited(context.Context, paneRef) (paneExit, error)        { return paneExit{}, nil }
func (p *stillPane) Reap(context.Context) error                               { return nil }
func (p *stillPane) Paste(context.Context, paneRef, string) error             { return nil }
func (p *stillPane) Enter(context.Context, paneRef) error                     { return nil }
func (p *stillPane) Alive(context.Context, paneRef) error                     { return nil }
func (p *stillPane) KillSession(context.Context) error {
	p.mu.Lock()
	p.killed++
	p.mu.Unlock()
	return nil
}

// gatedPane is a stillPane whose Start returns only once release is closed: a
// tmux new-session that is slow to print the pane id, while the CLI it started
// is already running (and hooking) — see startedRef.
type gatedPane struct {
	stillPane
	release chan struct{}
}

func (p *gatedPane) Start(ctx context.Context, _ string, _ []string) (paneRef, error) {
	select {
	case <-p.release:
		return fakeRef, nil
	case <-ctx.Done():
		return paneRef{}, ctx.Err()
	}
}

func (p *stillPane) kills() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// testLanes is a lanes set on stillPanes: every lane's CLI "runs" at once.
func testLanes(t *testing.T, max int) *lanes {
	t.Helper()
	cfg := t.TempDir()
	return &lanes{
		newTerm:   func(string) laneTerminal { return &stillPane{} },
		newServer: func(term terminal) *server { return newServer(serverConfig{ConfigDir: cfg}, term, newAuthTracker()) },
		bin:       "claude", configDir: cfg, workspace: "/data/workspace", max: max,
		startTimeout: time.Second, now: time.Now, set: map[string]*lane{},
		tweak: func(s *supervisor) { s.poll = 10 * time.Millisecond },
	}
}

// laneTurn posts one lane turn to s's /v1/responses and returns the status.
func laneTurn(t *testing.T, s *server, key string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hello","stream":false}`))
	r.Header.Set("Authorization", "Bearer b")
	r.Header.Set(headerCCSession, key)
	w := httptest.NewRecorder()
	s.gatewayHandler().ServeHTTP(w, r)
	return w.Code
}

func reservedOf(l *lanes, id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln := l.set[id]; ln != nil {
		return ln.reserved
	}
	return -1
}

// A turn RELEASES its lane however it ends: a lane whose CLI never reached its
// prompt (laneFor's waitStarted failure) and a turn that failed after the paste
// (runTurn's not_submitted) both leave the lane unreserved, so it stays
// evictable — a leaked reservation would pin a lane for the pod's life.
func TestALaneTurnReleasesItsLaneHoweverItEnds(t *testing.T) {
	l := testLanes(t, 2)
	l.startTimeout = 100 * time.Millisecond
	cfg := l.configDir
	l.newServer = func(term terminal) *server {
		return newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 100 * time.Millisecond}, term, newAuthTracker())
	}
	s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg}, nil, newAuthTracker())
	s.lanes = l
	id := laneID("sess-a")
	t.Cleanup(func() {
		if ln := l.get(id); ln != nil {
			ln.stop()
		}
	})

	if code := laneTurn(t, s, "sess-a"); code != http.StatusServiceUnavailable {
		t.Fatalf("a lane that never starts: status %d, want 503", code)
	}
	if n := reservedOf(l, id); n != 0 {
		t.Fatalf("after a lane that never started: reserved=%d, want 0", n)
	}

	l.get(id).srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	if code := laneTurn(t, s, "sess-a"); code != http.StatusGatewayTimeout {
		t.Fatalf("a turn nobody submits: status %d, want 504", code)
	}
	if n := reservedOf(l, id); n != 0 {
		t.Fatalf("after a turn that ran: reserved=%d, want 0", n)
	}
}

// 🔴 A STARTING LANE IS NOT IDLE. Between acquire and runTurn taking the lane's
// slot (waitStarted included) the lane's server looks idle; a second session
// at the cap must not evict it then — it is refused 503 capacity until the
// first turn releases its lane, and only then may the second evict it.
func TestALaneHeldByATurnIsNeverEvicted(t *testing.T) {
	l := testLanes(t, 1)
	ctx := context.Background()
	a, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(a.stop)
	if !a.srv.idle() {
		t.Fatal("precondition: a starting lane's server reports idle (that is the hazard)")
	}
	if b, f := l.acquire(ctx, "sess-b", ""); f == nil || f.Type != failCapacity {
		if b != nil {
			b.stop()
		}
		t.Fatalf("a second session while the first's turn holds its lane: %v; want a %s refusal (a evicted=%v)",
			f, failCapacity, l.get(a.id) == nil)
	}
	if l.get(a.id) != a {
		t.Fatal("the held lane is gone from the set")
	}
	// The same session's next turn reserves it again; one release is not enough.
	if again, f := l.acquire(ctx, "sess-a", ""); f != nil || again != a {
		t.Fatalf("the same session again: %v %v", again, f)
	}
	l.release(a)
	if _, f := l.acquire(ctx, "sess-b", ""); f == nil || f.Type != failCapacity {
		t.Fatalf("one of two reservations released: %v; want still refused", f)
	}
	l.release(a)
	b, f := l.acquire(ctx, "sess-b", "")
	if f != nil {
		t.Fatalf("both released: %v; want the idle lane evicted", f)
	}
	t.Cleanup(b.stop)
	if l.get(a.id) != nil {
		t.Fatal("the released idle lane was not evicted")
	}
}

// 🔴 A LANE IS READY WHEN ITS CLI HOOKED *AND* ITS SUPERVISOR RECORDED THE
// START. tmux new-session runs the CLI before it returns the pane, so the
// lane's SessionStart can come first; a turn let through then had no input pane
// and was refused 503 not_ready. Red when waitStarted waits on SessionStart
// alone.
func TestALaneIsNotReadyUntilItsSupervisorRecordsTheStart(t *testing.T) {
	l := testLanes(t, 1)
	gate := make(chan struct{})
	l.newTerm = func(string) laneTerminal { return &gatedPane{release: gate} }
	ctx := context.Background()
	ln, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(ln.stop)

	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	if st := ln.sup.state(); st.CLI != cliStarting {
		t.Fatalf("precondition: the supervisor is %q, want %q (Start has not returned)", st.CLI, cliStarting)
	}
	if f := ln.waitStarted(ctx, 300*time.Millisecond); f == nil || f.Type != failNotReady {
		t.Fatalf("SessionStart seen, the start not recorded: waitStarted = %+v; want a %s refusal", f, failNotReady)
	}
	close(gate)
	if f := ln.waitStarted(ctx, 5*time.Second); f != nil {
		t.Fatalf("after the start was recorded: %+v", f)
	}
	if _, ok := ln.sup.inputPane(); !ok {
		t.Fatal("waitStarted returned with no input pane")
	}
}
