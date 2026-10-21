package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a gateway process the test controls: it exits when told to, and
// on SIGTERM it exits 1 — Hermes's measured shutdown (S6: drain, then exit 1).
type fakeProc struct {
	exit    chan int
	mu      sync.Mutex
	signals []os.Signal
}

func newFakeProc() *fakeProc { return &fakeProc{exit: make(chan int, 1)} }

func (p *fakeProc) Wait() int { return <-p.exit }

func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		select {
		case p.exit <- 1:
		default:
		}
	}
	return nil
}

// fakeStarter hands out fake processes and records each start.
type fakeStarter struct {
	mu    sync.Mutex
	procs []*fakeProc
}

func (f *fakeStarter) start() (gatewayProc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := newFakeProc()
	f.procs = append(f.procs, p)
	return p, nil
}

func (f *fakeStarter) proc(i int) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.procs) {
		return nil
	}
	return f.procs[i]
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.procs)
}

func fastSupervisor(st *fakeStarter, recover func(context.Context) error) *supervisor {
	sup := newSupervisor(st.start, func() error { return nil },
		func(context.Context) error { return nil }, recover, func(string, ...any) {})
	sup.backoffMin, sup.backoffMax, sup.healthPoll, sup.recoverRetry = time.Millisecond, 5*time.Millisecond, time.Millisecond, 5*time.Millisecond
	return sup
}

// 🔴 SIGTERM's EXIT 1 DOES NOT COUNT TOWARD THE CRASH LATCH. With a latch of 2,
// one real crash has been counted; hermesd is then told to shut down, sends the
// gateway SIGTERM, and the gateway exits 1 as Hermes does. Were that exit
// counted, the latch would trip — /healthz failing while the pod drains for a
// rollout. It must read `stopped`, with the one crash still the only exit.
func TestSIGTERMsExitOneDoesNotCountTowardTheCrashLatch(t *testing.T) {
	st := &fakeStarter{}
	sup := fastSupervisor(st, func(context.Context) error { return nil })
	sup.crashExits = 2
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.run(ctx); close(done) }()

	eventually(t, "the first gateway is ready", func() bool { return sup.state().State == stReady })
	st.proc(0).exit <- 1 // a real crash: counted
	eventually(t, "the second gateway is ready", func() bool { return st.count() == 2 && sup.state().State == stReady })
	if got := sup.state(); got.LastExit == nil || *got.LastExit != 1 {
		t.Fatalf("the crash was not recorded: %+v", got)
	}

	cancel() // SIGTERM to hermesd
	<-done
	got := sup.state()
	if got.State == stCrashLoop || got.failed() {
		t.Fatalf("SIGTERM's EXIT 1 TRIPPED THE LATCH: %+v", got)
	}
	if got.State != stStopped {
		t.Fatalf("state %q after a requested stop; want %q", got.State, stStopped)
	}
	sup.mu.Lock()
	n := len(sup.exits)
	sup.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d exits counted; want only the one crash", n)
	}
	p := st.proc(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.signals) == 0 || p.signals[0] != syscall.SIGTERM {
		t.Fatalf("the gateway was not sent SIGTERM: %v", p.signals)
	}
}

// Control for the test above: the SAME exit 1, unrequested, IS counted and
// trips the latch, so the test above is not green because exits never count.
func TestAnUnrequestedExitIsCounted(t *testing.T) {
	st := &fakeStarter{}
	sup := fastSupervisor(st, func(context.Context) error { return nil })
	sup.crashExits = 2
	done := make(chan struct{})
	go func() { sup.run(context.Background()); close(done) }()
	eventually(t, "ready", func() bool { return sup.state().State == stReady })
	st.proc(0).exit <- 1
	eventually(t, "restarted", func() bool { return st.count() == 2 && sup.state().State == stReady })
	st.proc(1).exit <- 1
	<-done
	if got := sup.state(); got.State != stCrashLoop || !got.failed() {
		t.Fatalf("two crashes with a latch of 2: %+v", got)
	}
}

// 🔴 RECOVERY RUNS ON EVERY GATEWAY START. The gateway is OOM-killed while
// hermesd lives on; the restart runs recovery again, against the real recovery
// code and the fake API: the session's unknown run (left by the kill) is
// resolved after the SECOND start too.
func TestRecoveryRunsOnEveryGatewayStart(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	sess, _ := s.store.ensure("sess-a", s.keyFP)
	st := &fakeStarter{}
	var mu sync.Mutex
	calls := 0
	sup := fastSupervisor(st, func(ctx context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return s.recover(ctx)
	})
	s.sup = sup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx)
	eventually(t, "first start ready", func() bool { return sup.state().State == stReady })

	// The gateway is SIGKILLed mid-run: the session's last run is unknown.
	setLastRun(t, s, "sess-a", fake.admitUnknown(testAPIKey, sess.HermesID, "in flight at the kill"))
	st.proc(0).exit <- 137
	eventually(t, "second start ready", func() bool { return st.count() == 2 && sup.state().State == stReady })

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 || sup.state().Recoveries != 2 {
		t.Fatalf("recovery ran %d times over 2 gateway starts (state %+v)", n, sup.state())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.resolves) != 1 {
		t.Fatalf("the run the kill left unknown was not resolved by the restart's recovery: %+v", fake.resolves)
	}
}

// 🔴 /healthz REPORTS `recovering` UNTIL RECOVERY IS DONE, AND NO TURN IS TAKEN
// MEANWHILE (503 not_ready, which muster re-sends), while staying 200 so the
// liveness probe never restarts a pod for recovering.
func TestHealthzReportsRecoveringUntilRecoveryIsDone(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	st := &fakeStarter{}
	release := make(chan struct{})
	sup := fastSupervisor(st, func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s.sup = sup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx)
	eventually(t, "recovering", func() bool { return sup.state().State == stRecovering })

	code, h := healthz(t, s)
	if code != http.StatusOK || h.State != stRecovering || !h.OK {
		t.Fatalf("/healthz while recovering: %d %+v", code, h)
	}
	if code, body := turn(t, s, "sess-a", "too early"); code != http.StatusServiceUnavailable ||
		failureType(t, body) != failNotReady || len(fake.snapshotPosts()) != 0 {
		t.Fatalf("a turn during recovery: %d %s", code, body)
	}
	close(release)
	eventually(t, "ready", func() bool { _, h := healthz(t, s); return h.State == stReady })
	if code, _ := turn(t, s, "sess-a", "now"); code != http.StatusOK {
		t.Fatalf("a turn once recovered: %d", code)
	}
}

// A crash loop fails /healthz; so does a .env the supervisor refuses to start
// with (checked again before every restart, since the agent can write one).
func TestHealthzFailsOnlyOnALatch(t *testing.T) {
	st := &fakeStarter{}
	sup := fastSupervisor(st, func(context.Context) error { return nil })
	sup.prepare = func() error { return errDotEnv }
	sup.run(context.Background())
	s := &server{sup: sup}
	code, h := healthz(t, s)
	if code != http.StatusServiceUnavailable || h.State != stEnvForbidden || st.count() != 0 {
		t.Fatalf("a .env: /healthz %d %+v, %d starts", code, h, st.count())
	}
}

func healthz(t *testing.T, s *server) (int, health) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var h health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	return rec.Code, h
}
