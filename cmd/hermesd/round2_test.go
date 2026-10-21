package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a settable clock for s.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func withClock(s *server) *testClock {
	c := &testClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s.now = c.now
	return c
}

// diskIntents reads the session map from the volume, as a restart would, and
// returns each session's open intent's input ("" when none).
func diskIntents(t *testing.T, s *server) map[string]string {
	t.Helper()
	st, err := openStore(s.cfg.storePath())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range st.keys() {
		sess, _ := st.get(k)
		out[k] = ""
		if sess.Intent != nil {
			out[k] = string(sess.Intent.Body)
		}
	}
	return out
}

// 🔴 A. WRITTEN IS WRITTEN: the directory fsync fails AFTER the rename put the
// new map in place. Memory and disk must then agree on whether the intent
// exists. Before the fix the store kept the OLD map in memory: the turn
// answered 503 not_ready (nothing sent) while sessions.json held the intent,
// and a restart's recovery POSTed it (the auditor's NEVER-SENT-PROBE).
func TestAnUnsyncedDirectoryStillMeansWritten(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	if code, _ := turn(t, s, "sess-a", "first"); code != http.StatusOK {
		t.Fatalf("control turn: %d", code)
	}
	old := syncDir
	syncDir = func(*os.File) error { return testErr("injected: fsync of the state directory failed") }
	code, body := turn(t, s, "sess-a", "NEVER-SENT-PROBE")
	syncDir = old

	onDisk := strings.Contains(diskIntents(t, s)["sess-a"], "NEVER-SENT-PROBE")
	sess, _ := s.store.get("sess-a")
	inMemory := sess.Intent != nil
	if onDisk != inMemory {
		t.Fatalf("MEMORY AND DISK DISAGREE on the intent: disk %t, memory %t (turn %d %s)", onDisk, inMemory, code, body)
	}
	if code == http.StatusServiceUnavailable && onDisk {
		t.Fatalf("the turn answered not_ready (nothing sent) while its intent is on the volume: %s", body)
	}
	if code != http.StatusOK || replyText(t, body) != "echo: NEVER-SENT-PROBE" {
		t.Fatalf("the turn: %d %s", code, body)
	}
	// A restart: the map is reloaded from disk and recovery runs. It POSTs
	// nothing more: the probe's one run was the turn's own.
	r := newTestServerWith(t, s.cfg, fake, testAPIKey)
	if err := r.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.snapshotPosts()); n != 2 {
		t.Fatalf("%d POST(s); want the control's and the probe's own", n)
	}
}

// 🔴 C. A DEFINITE REFUSAL NEVER BECOMES A REPLAY. Hermes answers 429 and the
// close of the intent cannot be written. Before the fix muster was told
// not_ready (nothing sent), the intent stayed open, and the next settle POSTed
// it, which Hermes then admitted (the auditor's REFUSED-429-PROBE). Now the
// answer is may_have_run (a crash before the close lands WOULD replay it), a
// tombstone keeps this process from re-posting it, and a retry writes the
// close once the volume takes writes again.
func TestARefusalWhoseCloseCannotBeWrittenIsNeverReplayed(t *testing.T) {
	old := tombstoneRetry
	tombstoneRetry = 20 * time.Millisecond
	t.Cleanup(func() { tombstoneRetry = old })
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	if code, _ := turn(t, s, "sess-a", "first"); code != http.StatusOK {
		t.Fatalf("control turn: %d", code)
	}
	var once sync.Once
	fake.mu.Lock()
	fake.postStatus, fake.postCode = http.StatusTooManyRequests, "concurrency_limit"
	fake.onPost = func(fakePost) {
		once.Do(func() {
			if err := os.Chmod(s.cfg.StateDir, 0o500); err != nil {
				t.Error(err)
			}
		})
	}
	fake.mu.Unlock()
	code, body := turn(t, s, "sess-a", "REFUSED-429-PROBE")
	// Hermes would admit it now, and the next turn on the session settles the
	// intent it finds open — while the volume still refuses writes.
	fake.mu.Lock()
	fake.postStatus, fake.onPost = 0, nil
	fake.mu.Unlock()
	before := len(fake.snapshotPosts())
	next, _ := turn(t, s, "sess-a", "while the close is unwritten")
	recErr := s.recover(context.Background())
	for _, p := range fake.snapshotPosts()[before:] {
		if strings.Contains(string(p.Body), "REFUSED-429-PROBE") {
			t.Fatalf("THE REFUSED TURN WAS RE-POSTED (the turn had answered %d %s): %s", code, body, p.Body)
		}
	}
	if code != http.StatusBadGateway || failureType(t, body) != failMayHaveRun {
		t.Fatalf("a refusal whose close could not be written: %d %s (instrument: is the test running as root?)", code, body)
	}
	if next != http.StatusConflict {
		t.Fatalf("the session with an unwritten refusal: %d (want 409 busy)", next)
	}
	if recErr == nil {
		t.Fatal("recovery reported success while the close could not be written")
	}
	if err := os.Chmod(s.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the close is written", func() bool { return diskIntents(t, s)["sess-a"] == "" })
	n := 0
	for _, p := range fake.snapshotPosts() {
		if strings.Contains(string(p.Body), "REFUSED-429-PROBE") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the refused turn was POSTed %d times; want once (the refused POST itself)", n)
	}
	if code, body := turn(t, s, "sess-a", "next"); code != http.StatusOK {
		t.Fatalf("the next turn: %d %s", code, body)
	}
}

// 🔴 B. TRANSIENT FAILURES RETIRE NOTHING. Every GET answers 500 across ten
// minutes of recovery attempts: the idle session (a finished last run, no
// intent) makes no recovery call at all, and the session with an open intent
// keeps its history. Before the fix both were retired after 3 attempts.
func TestATransientRecoveryFailureRetiresNothing(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	clock := withClock(s)
	if code, _ := turn(t, s, "idle", "done already"); code != http.StatusOK {
		t.Fatal("control turn")
	}
	idle, _ := s.store.get("idle")
	open, _ := s.store.ensure("open", s.keyFP)
	setOpenIntent(t, s, "open", fake.admitUnknown(testAPIKey, open.HermesID, "in flight"), "in flight")
	fake.mu.Lock()
	fake.failGets, fake.gets = true, nil
	fake.mu.Unlock()
	for i := 0; i < 6; i++ {
		if err := s.recover(context.Background()); err == nil {
			t.Fatalf("attempt %d: recovery reported success while every GET failed", i)
		}
		clock.advance(2 * time.Minute)
	}
	// The other transient shape: no HTTP answer at all (each call times out).
	oldTimeout := recoveryCallTimeout
	recoveryCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { recoveryCallTimeout = oldTimeout })
	fake.mu.Lock()
	fake.failGets, fake.hangGets = false, true
	fake.mu.Unlock()
	for i := 0; i < 3; i++ {
		if err := s.recover(context.Background()); err == nil {
			t.Fatalf("attempt %d: recovery reported success while every GET hung", i)
		}
		clock.advance(4 * time.Minute)
	}
	afterIdle, _ := s.store.get("idle")
	afterOpen, _ := s.store.get("open")
	if afterIdle.HermesID != idle.HermesID || afterIdle.LastRunID != idle.LastRunID || afterIdle.Notice != "" {
		t.Fatalf("the idle session was touched: %+v -> %+v", idle, afterIdle)
	}
	if afterOpen.HermesID != open.HermesID || afterOpen.Intent == nil {
		t.Fatalf("a transient failure retired the open session: %+v", afterOpen)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, id := range fake.gets {
		if id == idle.LastRunID {
			t.Fatalf("recovery called Hermes about the idle session's finished run %s", id)
		}
	}
}

// 🔴 B. A PERSISTENT FAILURE ENDS IN recovery_stuck, NOT IN FORGETTING: the run
// API keeps answering 500, so /healthz goes non-200 at the stuck bound and the
// session is not retired to make recovery "succeed".
func TestAPersistentRecoveryFailureEndsInRecoveryStuck(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.retireAfter = time.Millisecond // even a tiny floor retires nothing transient
	sess, _ := s.store.ensure("open", s.keyFP)
	setOpenIntent(t, s, "open", fake.admitUnknown(testAPIKey, sess.HermesID, "in flight"), "in flight")
	fake.mu.Lock()
	fake.failGets = true
	fake.mu.Unlock()
	sup := fastSupervisor(&fakeStarter{}, s.recover)
	sup.recoveryStuckAfter = 50 * time.Millisecond
	s.sup = sup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx)
	eventually(t, "recovery_stuck", func() bool { return sup.state().State == stRecoveryStuck })
	time.Sleep(100 * time.Millisecond)
	if st := sup.state().State; st != stRecoveryStuck {
		t.Fatalf("recovery left stuck for %s: %s", st, sup.state().Detail)
	}
	if code, h := healthz(t, s); code != http.StatusServiceUnavailable || h.State != stRecoveryStuck {
		t.Fatalf("/healthz: %d %+v", code, h)
	}
	if after, _ := s.store.get("open"); after.HermesID != sess.HermesID {
		t.Fatalf("the session was retired to end recovery: %+v", after)
	}
}

// B. Only a DEFINITE refusal of an open intent's recovery, persisting past the
// floor, retires its session — and its notice reads as a sentence.
func TestADefinitelyRefusedRecoveryRetiresOnlyAfterTheFloor(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	clock := withClock(s)
	sess, _ := s.store.ensure("open", s.keyFP)
	setOpenIntent(t, s, "open", fake.admitUnknown(testAPIKey, sess.HermesID, "in flight"), "in flight")
	fake.mu.Lock()
	fake.getStatus, fake.getCode = http.StatusBadRequest, "invalid_run"
	fake.mu.Unlock()
	for _, at := range []time.Duration{0, 2 * time.Minute, 2 * time.Minute} {
		clock.advance(at)
		if err := s.recover(context.Background()); err == nil {
			t.Fatalf("retired before the floor (at +%s)", at)
		}
	}
	if after, _ := s.store.get("open"); after.HermesID != sess.HermesID {
		t.Fatalf("retired before %s: %+v", retireAfter, after)
	}
	clock.advance(2 * time.Minute)
	if err := s.recover(context.Background()); err != nil {
		t.Fatalf("past the floor the session is retired and recovery finishes: %v", err)
	}
	after, _ := s.store.get("open")
	if after.HermesID == sess.HermesID || after.Intent != nil {
		t.Fatalf("not retired: %+v", after)
	}
	want := "this turn starts a new session without its history. Your previous message was not confirmed: " + mayHaveRunText + "."
	if !strings.Contains(after.Notice, want) || strings.Contains(after.Notice, "message the turn") {
		t.Fatalf("notice %q", after.Notice)
	}
}

// 🟢 A run that COMPLETED between the budget running out and the stop is the
// reply, not a turn_timeout.
func TestARunThatCompletedAtTheBudgetIsTheReply(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.turnTimeout = 200 * time.Millisecond
	code, body := turn(t, s, "sess-a", "LATE")
	if code != http.StatusOK || replyText(t, body) != "late answer" {
		t.Fatalf("%d %s", code, body)
	}
	if sess, _ := s.store.get("sess-a"); sess.Intent != nil {
		t.Fatalf("intent left open: %+v", sess.Intent)
	}
}
