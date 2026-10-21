package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 🔴 A FAILED INTENT WRITE LEAVES NOTHING BEHIND. The state directory refuses
// writes for one turn: the turn answers 503 not_ready (nothing sent), and then
// — the auditor's probe, step by step — the session is not busy, a later save
// on ANOTHER session writes no phantom intent to disk, and recovery POSTs
// nothing. Before the fix the intent reached memory before the write failed,
// the next save persisted it, and recovery POSTed a turn muster had been told
// was never sent.
func TestAFailedIntentWriteLeavesNoPhantom(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	if code, _ := turn(t, s, "sess-a", "first"); code != http.StatusOK {
		t.Fatalf("control turn: %d", code)
	}
	if err := os.Chmod(s.cfg.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	code, body := turn(t, s, "sess-a", "never sent")
	if err := os.Chmod(s.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusServiceUnavailable || failureType(t, body) != failNotReady {
		t.Fatalf("a turn whose intent could not be written: %d %s (instrument: is the test running as root?)", code, body)
	}
	posts := len(fake.snapshotPosts())
	if sess, _ := s.store.get("sess-a"); sess.Intent != nil {
		t.Fatalf("A PHANTOM INTENT IN MEMORY after the failed write: %+v", sess.Intent)
	}
	if code, _ := turn(t, s, "sess-b", "another session"); code != http.StatusOK {
		t.Fatalf("turn on another session: %d", code)
	}
	b, err := os.ReadFile(s.cfg.storePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "never sent") {
		t.Fatalf("A LATER SAVE WROTE THE PHANTOM INTENT TO DISK:\n%s", b)
	}
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.snapshotPosts()); n != posts+1 {
		t.Fatalf("recovery POSTed %d turn(s) (only sess-b's own turn was expected)", n-posts-1)
	}
	if code, body := turn(t, s, "sess-a", "now"); code != http.StatusOK {
		t.Fatalf("the session stayed busy: %d %s", code, body)
	}
}

// 🔴 THE TURN BUDGET STOPS THE RUN: POST /v1/runs/{id}/stop, a typed
// turn_timeout, the intent closed — nothing left running unfollowed.
func TestTheTurnBudgetStopsTheRun(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.turnTimeout = 300 * time.Millisecond
	code, body := turn(t, s, "sess-a", "HANG")
	if code != http.StatusGatewayTimeout || failureType(t, body) != failTurnTimeout || !strings.Contains(body, "was stopped") {
		t.Fatalf("a run past the budget: %d %s", code, body)
	}
	fake.mu.Lock()
	stops := append([]string(nil), fake.stops...)
	fake.mu.Unlock()
	if len(stops) != 1 || stops[0] != "run_1" {
		t.Fatalf("stops %v; want run_1 stopped once", stops)
	}
	if sess, _ := s.store.get("sess-a"); sess.Intent != nil || sess.LastRunID != "run_1" {
		t.Fatalf("after the stop: intent %+v last %q", sess.Intent, sess.LastRunID)
	}
	if code, _ := turn(t, s, "sess-a", "next"); code != http.StatusOK {
		t.Fatalf("the next turn: %d", code)
	}
}

// When muster's request goes away mid-turn (muster redeployed), hermesd keeps
// following the run in the same goroutine and closes its intent when it ends —
// no stop, no background watcher, the session free for the next turn.
func TestARunOutlivesItsVanishedRequest(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"input": "SLOW"})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testBearer)
	req.Header.Set(sessionKeyHeader, "sess-a")
	done := make(chan struct{})
	go func() { s.handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	eventually(t, "the run is admitted", func() bool { return fake.runCount() == 1 })
	cancel()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the turn stopped following its run when the request went away")
	default:
	}
	close(fake.release)
	<-done
	fake.mu.Lock()
	stops := len(fake.stops)
	fake.mu.Unlock()
	if sess, _ := s.store.get("sess-a"); sess.Intent != nil || sess.LastRunID != "run_1" || stops != 0 {
		t.Fatalf("intent %+v last %q, %d stop(s)", sess.Intent, sess.LastRunID, stops)
	}
}

// 🟡5 THE POLLED-APPROVAL PATH: the run's stream drops before the approval is
// announced; hermesd polls the run, finds it in pending_controls (the pin's
// field names: kind, prompt_id, execution_generation, command, description),
// relays it, and answers with the exact body.
func TestAnApprovalSeenOnlyByPollingIsRelayed(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	m := newFakeMuster(t, "approve")
	m.wire(s)
	code, body := turn(t, s, "sess-a", "POLLAPPROVE rm -r build")
	if code != http.StatusOK || replyText(t, body) != "polled choice was once" {
		t.Fatalf("turn: %d %s", code, body)
	}
	fake.mu.Lock()
	approvals := fake.approvals
	fake.mu.Unlock()
	if len(approvals) != 1 || keySet(approvals[0]) != "choice,execution_generation,request_id" || approvals[0]["choice"] != "once" {
		t.Fatalf("approval bodies %v", approvals)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.filed) != 1 || m.filed[0]["command"] != "rm -r build" || m.filed[0]["context"].([]any)[0] != "recursive delete" {
		t.Fatalf("filed with muster: %v", m.filed)
	}
}

// tool.failed draws the tool row failed.
func TestToolFailedIsDrawnFailed(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	code, body := turn(t, s, "sess-a", "TOOLFAIL")
	if code != http.StatusOK || !strings.Contains(body, `"call_id":"call_tf"`) || !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("%d %s", code, body)
	}
}

// 🟡4 Every recovery call has a deadline: a gateway that accepts and never
// answers cannot hold recovery for ever.
func TestARecoveryCallHasADeadline(t *testing.T) {
	old := recoveryCallTimeout
	recoveryCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { recoveryCallTimeout = old })
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	sess, _ := s.store.ensure("sess-a", s.keyFP)
	setOpenIntent(t, s, "sess-a", fake.admitUnknown(testAPIKey, sess.HermesID, "x"), "x")
	fake.hangGets = true
	start := time.Now()
	err := s.recover(context.Background())
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("recovery against a hanging gateway: %v after %s", err, time.Since(start))
	}
}

// 🟡4 /healthz fails once recovery has been stuck past its bound, so the
// liveness probe can act — and recovers if recovery later finishes.
func TestHealthzFailsWhenRecoveryIsStuck(t *testing.T) {
	st := &fakeStarter{}
	var mu sync.Mutex
	stuck := true
	sup := fastSupervisor(st, func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if stuck {
			return errTestStuck
		}
		return nil
	})
	sup.recoveryStuckAfter = 50 * time.Millisecond
	s := &server{sup: sup}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx)
	eventually(t, "stuck", func() bool { return sup.state().State == stRecoveryStuck })
	if code, h := healthz(t, s); code != http.StatusServiceUnavailable || h.OK || h.State != stRecoveryStuck {
		t.Fatalf("/healthz while recovery is stuck: %d %+v", code, h)
	}
	mu.Lock()
	stuck = false
	mu.Unlock()
	eventually(t, "ready", func() bool { return sup.state().State == stReady })
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errTestStuck = testErr("the gateway's store is unavailable")

// An approval's relay window counts from when hermesd first saw it: a prompt
// seen longer ago than the window is denied without filing a card.
func TestTheRelayWindowCountsFromWhenThePromptWasSeen(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	m := newFakeMuster(t, "approve")
	m.wire(s)
	d := s.relay.decide(context.Background(), approvalRequest{PromptID: "p1"}, time.Now().Add(-2*s.relay.timeout), "/w", "sess")
	m.mu.Lock()
	defer m.mu.Unlock()
	if d != relayDeny || len(m.filed) != 0 {
		t.Fatalf("a prompt past its window: decision %v, %d card(s) filed", d, len(m.filed))
	}
}
