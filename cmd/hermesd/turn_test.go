package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 🔴 THE INTENT IS ON THE VOLUME BEFORE THE POST LEAVES. At the moment Hermes
// receives POST /v1/runs, the session map ON DISK (not hermesd's memory) holds
// an intent whose idempotency key is the header's and whose body is the POST's
// exact bytes, under the current key's fingerprint. A crash at any point after
// the POST therefore leaves something recovery can replay.
func TestTheIntentIsOnTheVolumeBeforeThePost(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	var (
		mu      sync.Mutex
		onDisk  storeFile
		readErr error
	)
	fake.onPost = func(p fakePost) {
		b, err := os.ReadFile(s.cfg.storePath())
		mu.Lock()
		defer mu.Unlock()
		if readErr = err; err == nil {
			readErr = json.Unmarshal(b, &onDisk)
		}
	}
	code, body := turn(t, s, "sess-a", "hello there")
	if code != http.StatusOK || replyText(t, body) != "echo: hello there" {
		t.Fatalf("turn: %d %s", code, body)
	}
	posts := fake.snapshotPosts()
	mu.Lock()
	defer mu.Unlock()
	if readErr != nil {
		t.Fatalf("NO SESSION MAP ON DISK WHEN THE POST ARRIVED: %v", readErr)
	}
	ss := onDisk.Sessions["sess-a"]
	if ss == nil || ss.Intent == nil {
		t.Fatalf("THE POST LEFT BEFORE ITS INTENT WAS WRITTEN: on disk %+v", onDisk.Sessions)
	}
	if len(posts) != 1 || ss.Intent.IdempotencyKey != posts[0].Key || !bytes.Equal(ss.Intent.Body, posts[0].Body) {
		t.Fatalf("the intent on disk (key %q, body %s) is not the POST (%+v)", ss.Intent.IdempotencyKey, ss.Intent.Body, posts)
	}
	if ss.Intent.KeyFP != keyFingerprint(testAPIKey) || ss.Intent.RunID != "" {
		t.Fatalf("intent fingerprint %q run %q; want %q and no run yet", ss.Intent.KeyFP, ss.Intent.RunID, keyFingerprint(testAPIKey))
	}
	// And once the run settled, the intent is closed and the run is the last.
	after, _ := s.store.get("sess-a")
	if after.Intent != nil || after.LastRunID != "run_1" {
		t.Fatalf("after the turn: intent %+v last run %q", after.Intent, after.LastRunID)
	}
}

// 🔴 A LOST ACCEPTANCE IS REPLAYED WITH THE SAME KEY AND THE SAME BYTES, AND
// HERMES RUNS THE TURN ONCE. The fake admits the run and drops its answer (the
// connection closes), exactly what a crash or network fault after admission
// looks like to hermesd. hermesd re-POSTs; the fake sees the same
// Idempotency-Key with a byte-identical body, answers the ORIGINAL run as a
// replay, and only one run ever exists.
func TestALostAcceptanceIsReplayedWithTheSameKeyAndBytes(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	fake.dropAnswers = 1
	s := newTestServer(t, fake, testAPIKey)
	code, body := turn(t, s, "sess-a", "only once please")
	if code != http.StatusOK || replyText(t, body) != "echo: only once please" {
		t.Fatalf("turn: %d %s", code, body)
	}
	posts := fake.snapshotPosts()
	if len(posts) != 2 {
		t.Fatalf("%d POSTs; want the original and one replay", len(posts))
	}
	if posts[0].Key == "" || posts[1].Key != posts[0].Key || !bytes.Equal(posts[1].Body, posts[0].Body) {
		t.Fatalf("THE REPLAY DIFFERS FROM THE ORIGINAL:\n  %q %s\n  %q %s", posts[0].Key, posts[0].Body, posts[1].Key, posts[1].Body)
	}
	if n := fake.runCount(); n != 1 {
		t.Fatalf("HERMES RAN THE TURN %d TIMES", n)
	}
}

// The exact body: the POST is {input, session_id, instructions} and nothing
// else, the session id is the session map's, and muster's instructions are read
// from the file on EVERY run (a change reaches the next turn).
func TestEveryRunCarriesTheInstructionsFile(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	if err := os.MkdirAll(strings.TrimSuffix(s.cfg.Instructions, "/instructions.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfg.Instructions, []byte("FIRST INSTRUCTIONS"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "sess-a", "one")
	if err := os.WriteFile(s.cfg.Instructions, []byte("SECOND INSTRUCTIONS"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "sess-a", "two")
	posts := fake.snapshotPosts()
	sess, _ := s.store.get("sess-a")
	for i, want := range []string{"FIRST INSTRUCTIONS", "SECOND INSTRUCTIONS"} {
		var body map[string]any
		if err := json.Unmarshal(posts[i].Body, &body); err != nil {
			t.Fatal(err)
		}
		if keySet(body) != "input,instructions,session_id" || body["instructions"] != want || body["session_id"] != sess.HermesID {
			t.Fatalf("run %d body %s; want instructions %q on session %s", i+1, posts[i].Body, want, sess.HermesID)
		}
	}
}

// 🔴 409 idempotency_key_conflict IS "MAY HAVE RUN", NEVER A RETRY UNDER A NEW
// KEY, AND NEVER not_ready (which muster re-sends).
func TestAKeyConflictIsReportedAsMayHaveRun(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	fake.postStatus, fake.postCode = http.StatusConflict, "idempotency_key_conflict"
	s := newTestServer(t, fake, testAPIKey)
	code, body := turn(t, s, "sess-a", "hello")
	if code != http.StatusBadGateway || failureType(t, body) != failMayHaveRun || !strings.Contains(body, "may have run") {
		t.Fatalf("a 409 conflict answered %d %s; want 502 %s", code, body, failMayHaveRun)
	}
	if n := len(fake.snapshotPosts()); n != 1 {
		t.Fatalf("%d POSTs after a key conflict; want 1 (no retry)", n)
	}
	if sess, _ := s.store.get("sess-a"); sess.Intent != nil {
		t.Fatalf("the intent of a conflicting key stays open: %+v", sess.Intent)
	}
}

// The approval round trip: muster approves, Hermes receives EXACTLY
// {request_id, execution_generation, choice} with `once`; the card carried the
// command.
func TestAnApprovalIsAnsweredWithTheExactBody(t *testing.T) {
	for _, tc := range []struct{ muster, choice string }{{"approve", "once"}, {"reject", "deny"}} {
		t.Run(tc.muster, func(t *testing.T) {
			fake := newFakeHermes(t, testAPIKey)
			s := newTestServer(t, fake, testAPIKey)
			m := newFakeMuster(t, tc.muster)
			m.wire(s)
			code, body := turn(t, s, "sess-a", "APPROVE rm -r build")
			if code != http.StatusOK || replyText(t, body) != "choice was "+tc.choice {
				t.Fatalf("turn: %d %s", code, body)
			}
			fake.mu.Lock()
			approvals := fake.approvals
			fake.mu.Unlock()
			if len(approvals) != 1 || keySet(approvals[0]) != "choice,execution_generation,request_id" ||
				approvals[0]["choice"] != tc.choice || approvals[0]["request_id"] != "prompt-run_1" || approvals[0]["execution_generation"] != float64(1) {
				t.Fatalf("approval bodies %v", approvals)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.filed) != 1 || m.filed[0]["command"] != "rm -r build" || m.filed[0]["tool"] != "terminal" {
				t.Fatalf("filed with muster: %v", m.filed)
			}
			// The tool call is drawn as a native tool row, failed when denied.
			wantStatus := `"status":"completed"`
			if tc.choice == "deny" {
				wantStatus = `"status":"failed"`
			}
			if !strings.Contains(body, `"type":"native_tool_call"`) || !strings.Contains(body, wantStatus) {
				t.Fatalf("no native tool item (%s) in %s", wantStatus, body)
			}
		})
	}
}

// 🔴 NOBODY ANSWERS: hermesd DENIES BEFORE HERMES WOULD. The relay gives up
// after its timeout and hermesd answers `deny` itself, so the run's outcome is
// the agent's reaction to a deny it was told about — never Hermes's own
// time-out, which ends `run.completed` and reads as success (S6).
func TestAnUnansweredApprovalIsDeniedByHermesd(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.relay.timeout = 150 * time.Millisecond
	m := newFakeMuster(t, "") // pending for ever
	m.wire(s)
	start := time.Now()
	code, body := turn(t, s, "sess-a", "APPROVE rm -r build")
	if code != http.StatusOK || replyText(t, body) != "choice was deny" {
		t.Fatalf("turn: %d %s", code, body)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the deny took %s; the relay timeout is 150ms", time.Since(start))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deleted) != 1 {
		t.Fatalf("the card of an unanswered approval was not cleared: %v", m.deleted)
	}
}

// With no relay offered, an approval is denied at once rather than left to
// Hermes's timeout.
func TestWithoutARelayAnApprovalIsDenied(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	code, body := turn(t, s, "sess-a", "APPROVE rm -r build")
	if code != http.StatusOK || replyText(t, body) != "choice was deny" {
		t.Fatalf("turn: %d %s", code, body)
	}
}

// 🔴 unknown_execution IS QUEUED, NOT FAILED. The session's previous run was
// left unknown (a SIGKILL); the next run's stream reports run.failed
// unknown_execution and closes. hermesd resolves the predecessor with the exact
// body and follows the queued run to its real end: the person gets ITS reply,
// never a failure inviting a resend.
func TestUnknownExecutionIsQueuedNotFailed(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	sess, err := s.store.ensure("sess-a", s.keyFP)
	if err != nil {
		t.Fatal(err)
	}
	prev := fake.admitUnknown(testAPIKey, sess.HermesID, "the turn that was killed")
	setLastRun(t, s, "sess-a", prev)
	code, body := turn(t, s, "sess-a", "after the crash")
	if code != http.StatusOK {
		t.Fatalf("AN unknown_execution WAS REPORTED AS A FAILURE: %d %s", code, body)
	}
	if got := replyText(t, body); got != "after the unknown run: after the crash" {
		t.Fatalf("reply %q", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.resolves) != 1 || fake.resolves[0].RunID != prev ||
		keySet(fake.resolves[0].Body) != "admission_id,execution_generation" || fake.resolves[0].Body["admission_id"] != "adm_"+prev {
		t.Fatalf("resolve-unknown calls %+v", fake.resolves)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("%d POSTs; the queued turn must not be re-posted", len(fake.posts))
	}
}

// A run.failed that is not unknown_execution is a typed failure; a provider
// rate limit is its own type; an empty completion is empty_reply, never a 200
// with no text.
func TestRunFailuresAreTyped(t *testing.T) {
	for _, tc := range []struct {
		input string
		code  int
		typ   string
	}{
		{"FAIL the provider returned 500", http.StatusBadGateway, failRuntime},
		{"FAIL HTTP 429: rate limit exceeded", http.StatusTooManyRequests, failRateLimited},
		{"EMPTY", http.StatusBadGateway, failEmptyReply},
	} {
		fake := newFakeHermes(t, testAPIKey)
		s := newTestServer(t, fake, testAPIKey)
		code, body := turn(t, s, "s", tc.input)
		if code != tc.code || failureType(t, body) != tc.typ {
			t.Fatalf("%q: %d %s; want %d %s", tc.input, code, body, tc.code, tc.typ)
		}
		if sess, _ := s.store.get("s"); sess.Intent != nil || sess.LastRunID != "run_1" {
			t.Fatalf("%q: the failed run's intent is still open: %+v", tc.input, sess)
		}
	}
}

// 409 busy while a run for that session is live, from muster or from an
// intent recovery is still following; another session is not blocked.
func TestASessionWithALiveRunIsBusy(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	done := make(chan int, 1)
	go func() { code, _ := turn(t, s, "sess-a", "SLOW"); done <- code }()
	eventually(t, "the slow run is admitted", func() bool { return fake.runCount() == 1 })
	if code, body := turn(t, s, "sess-a", "again"); code != http.StatusConflict || failureType(t, body) != failBusy {
		t.Fatalf("a second turn on a live session: %d %s", code, body)
	}
	if code, body := turn(t, s, "sess-b", "elsewhere"); code != http.StatusOK {
		t.Fatalf("another session was blocked: %d %s", code, body)
	}
	close(fake.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the slow turn: %d", code)
	}
}

func TestTheBearerIsRequired(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	s.bearer = "something-else"
	if code, body := turn(t, s, "s", "hi"); code != http.StatusUnauthorized || failureType(t, body) != failUnauthorized {
		t.Fatalf("%d %s", code, body)
	}
	if n := len(fake.snapshotPosts()); n != 0 {
		t.Fatalf("an unauthenticated turn reached Hermes")
	}
}

// An intent whose run is still going (recovery replayed it) keeps its session
// busy: a new turn settles it first, finds the run running, and is 409, never
// posted beside it. Once the run has ended, the next turn settles the intent
// and runs — nothing follows the run in the background.
func TestAnUnsettledIntentKeepsTheSessionBusy(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	seedIntent(t, s, "sess-a", s.keyFP, "SLOW", s.now())
	if err := s.recover(context.Background()); err != nil { // replays it; the slow run keeps going
		t.Fatal(err)
	}
	if code, body := turn(t, s, "sess-a", "meanwhile"); code != http.StatusConflict || failureType(t, body) != failBusy {
		t.Fatalf("a turn beside an unsettled intent: %d %s", code, body)
	}
	if n := len(fake.snapshotPosts()); n != 1 {
		t.Fatalf("%d POSTs; the busy turn must not be posted", n)
	}
	close(fake.release)
	eventually(t, "the slow run ends in Hermes", func() bool {
		st, _ := fake.api(testAPIKey).getRun(context.Background(), "run_1")
		return st.Status == "completed"
	})
	if code, body := turn(t, s, "sess-a", "now"); code != http.StatusOK || replyText(t, body) != "echo: now" {
		t.Fatalf("after it ended: %d %s", code, body)
	}
}
