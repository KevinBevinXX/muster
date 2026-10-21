package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// revokeKey makes fake answer every authenticated request 401
// invalid_api_key, as Hermes does for an API key it does not hold: the key
// hermesd has and the gateway's disagree.
func revokeKey(fake *fakeHermes, key string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	delete(fake.keys, key)
}

// 🔴 A 401 IS A REFUSAL OF THE API, NOT OF A SESSION. Every session with an open
// intent is refused 401 (a mismatched API key). Before the fix 401 counted as a
// definite refusal of each session: past the retire floor every one of them was
// retired, recovery then "finished", and /healthz reported ready over an API
// that answers nothing. Now nothing is retired and recovery ends in
// recovery_stuck, as for a 5xx.
func TestAnUnauthorizedAPIRetiresNoSessionAndEndsInRecoveryStuck(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.retireAfter = time.Millisecond // any definite refusal would retire at once
	keys := []string{"open-1", "open-2", "open-3"}
	before := map[string]string{}
	for _, k := range keys {
		sess, _ := s.store.ensure(k, s.keyFP)
		setOpenIntent(t, s, k, fake.admitUnknown(testAPIKey, sess.HermesID, "in flight "+k), "in flight "+k)
		before[k] = sess.HermesID
	}
	revokeKey(fake, testAPIKey)
	sup := fastSupervisor(&fakeStarter{}, s.recover)
	sup.recoveryStuckAfter = 50 * time.Millisecond
	s.sup = sup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx)
	eventually(t, "recovery_stuck (state never left recovering, or became ready by retiring)", func() bool {
		return sup.state().State == stRecoveryStuck || sup.state().State == stReady
	})
	time.Sleep(100 * time.Millisecond)
	retired := 0
	for _, k := range keys {
		if after, _ := s.store.get(k); after.HermesID != before[k] || after.Intent == nil {
			retired++
		}
	}
	if retired != 0 {
		t.Fatalf("%d of %d session(s) retired over a 401 (state %s)", retired, len(keys), sup.state().State)
	}
	if st := sup.state().State; st != stRecoveryStuck {
		t.Fatalf("recovery state %s, want %s: %s", st, stRecoveryStuck, sup.state().Detail)
	}
	if code, h := healthz(t, s); code != http.StatusServiceUnavailable || h.State != stRecoveryStuck {
		t.Fatalf("/healthz: %d %+v", code, h)
	}
	// Positive control: the same sessions under a 400 are retired, so the
	// floor and the fixture can retire — the 401 is what spared them.
	fake.mu.Lock()
	fake.keys[testAPIKey] = true
	fake.getStatus, fake.getCode = http.StatusBadRequest, "invalid_run"
	fake.mu.Unlock()
	eventually(t, "the 400-refused sessions retired", func() bool {
		for _, k := range keys {
			if after, _ := s.store.get(k); after.HermesID == before[k] {
				return false
			}
		}
		return true
	})
}

// 🔴 A 401 ON A REPLAY IS NOT "IT NEVER RAN". An intent left `posting` (no run
// id) is re-posted at recovery; Hermes checks the key BEFORE its idempotency
// store, so a 401 says nothing about whether the first POST was admitted.
// Before the fix the 401 closed the intent as "not run".
func TestAnUnauthorizedReplayKeepsTheIntentOpen(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	setOpenIntent(t, s, "posting", "", "maybe admitted")
	revokeKey(fake, testAPIKey)
	if err := s.recover(context.Background()); err == nil {
		t.Fatal("recovery reported success while every call answered 401")
	}
	if after, _ := s.store.get("posting"); after.Intent == nil || after.Notice != "" {
		t.Fatalf("a 401 replay settled the intent: %+v", after)
	}
	// Positive control: a 400 on the replay IS a refusal, and closes it.
	fake.mu.Lock()
	fake.keys[testAPIKey] = true
	fake.postStatus, fake.postCode = http.StatusBadRequest, "invalid_request"
	fake.mu.Unlock()
	if err := s.recover(context.Background()); err != nil {
		t.Fatalf("a 400 replay: %v", err)
	}
	if after, _ := s.store.get("posting"); after.Intent != nil {
		t.Fatalf("a 400 replay left the intent open: %+v", after)
	}
}

// 🟢 "REFUSED CONTINUOUSLY FOR retireAfter" MEANS CONTINUOUSLY. One 400, then
// 500s for six minutes, then one 400: two refusals six minutes apart with
// transient failures between them are not five minutes of refusal. Before the
// fix the first 400 started the clock and nothing stopped it, so the second
// 400 retired the session.
func TestATransientFailureRestartsTheRefusalClock(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	clock := withClock(s)
	sess, _ := s.store.ensure("open", s.keyFP)
	setOpenIntent(t, s, "open", fake.admitUnknown(testAPIKey, sess.HermesID, "in flight"), "in flight")
	setGet := func(status int, fail bool) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.getStatus, fake.getCode, fake.failGets = status, "invalid_run", fail
	}
	setGet(http.StatusBadRequest, false)
	if err := s.recover(context.Background()); err == nil {
		t.Fatal("the first 400 retired the session at once")
	}
	setGet(0, true)
	for i := 0; i < 6; i++ {
		clock.advance(time.Minute)
		if err := s.recover(context.Background()); err == nil {
			t.Fatalf("a 500 at +%dm: recovery reported success", i+1)
		}
	}
	setGet(http.StatusBadRequest, false)
	if err := s.recover(context.Background()); err == nil {
		t.Fatal("the second 400 retired the session")
	}
	if after, _ := s.store.get("open"); after.HermesID != sess.HermesID || after.Intent == nil {
		t.Fatalf("retired after two refusals %s apart with 500s between them: %+v", 6*time.Minute, after)
	}
	// Positive control: refusals that DO persist past the floor retire it.
	clock.advance(s.retireAfter)
	if err := s.recover(context.Background()); err != nil {
		t.Fatalf("a refusal held past the floor: %v", err)
	}
	if after, _ := s.store.get("open"); after.HermesID == sess.HermesID {
		t.Fatalf("not retired after %s of continuous refusal: %+v", s.retireAfter, after)
	}
}

// 🟢 A recovery that SUCCEEDS between two refusals (the run is still going, so
// its intent stays open) restarts the clock too.
func TestASuccessfulRecoveryRestartsTheRefusalClock(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	clock := withClock(s)
	sess, _ := s.store.ensure("open", s.keyFP)
	runID := fake.admitUnknown(testAPIKey, sess.HermesID, "in flight")
	setOpenIntent(t, s, "open", runID, "in flight")
	fake.mu.Lock()
	run := fake.runs[runID]
	fake.mu.Unlock()
	run.setStatus("running")
	setGet := func(status int) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.getStatus, fake.getCode = status, "invalid_run"
	}
	setGet(http.StatusBadRequest)
	if err := s.recover(context.Background()); err == nil {
		t.Fatal("the first 400 retired the session at once")
	}
	setGet(0)
	for i := 0; i < 6; i++ {
		clock.advance(time.Minute)
		if err := s.recover(context.Background()); err != nil {
			t.Fatalf("a running run at +%dm: %v", i+1, err)
		}
	}
	setGet(http.StatusBadRequest)
	if err := s.recover(context.Background()); err == nil {
		t.Fatal("the second 400 retired the session")
	}
	if after, _ := s.store.get("open"); after.HermesID != sess.HermesID || after.Intent == nil {
		t.Fatalf("retired after two refusals with successful recoveries between them: %+v", after)
	}
}

// 🟢 A SESSION IS RETIRED BEFORE THE POD IS RESTARTED FOR IT. A definitely
// refused session is retired at retireAfter; recovery_stuck fails /healthz at
// recoveryStuckAfter and the liveness probe restarts the pod
// livenessWindow later. Were retirement not first by more than that window, a
// session refused from the start would restart the pod before it could be
// retired, and every restart starts its refusal clock again: a restart loop.
func TestRetirementComesBeforeTheStuckRestart(t *testing.T) {
	// The liveness probe muster renders for a pod kind: 3 failures, 30 s apart
	// (internal/provision/k8s/render.go).
	const livenessWindow = 90 * time.Second
	sup := newSupervisor(nil, nil, nil, nil, func(string, ...any) {})
	if margin := sup.recoveryStuckAfter - retireAfter; margin <= livenessWindow {
		t.Fatalf("retireAfter %s is only %s before recoveryStuckAfter %s; want more than the %s liveness window",
			retireAfter, margin, sup.recoveryStuckAfter, livenessWindow)
	}
}
