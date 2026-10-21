package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedIntent puts an open intent on session key, as a crash between the POST
// and recording its run id leaves it: body exactly as hermesd would have built
// it, under keyFP.
func seedIntent(t *testing.T, s *server, key, keyFP, input string, at time.Time) intent {
	t.Helper()
	sess, err := s.store.ensure(key, keyFP)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runRequest{Input: input, SessionID: sess.HermesID})
	in := intent{IdempotencyKey: newIdempotencyKey(), Body: body, KeyFP: keyFP, At: at}
	if err := s.store.beginIntent(key, in); err != nil {
		t.Fatal(err)
	}
	return in
}

// 🔴 RECOVERY REPLAYS AN UNRECORDED INTENT WITH THE SAME KEY AND THE SAME BYTES.
// Hermes admitted the POST, then hermesd died before recording the run id. On
// the next start the intent is re-posted byte for byte under its own key; Hermes
// answers the run it already admitted, so the turn runs ONCE. Nothing follows
// it in the background: the intent records the run and stays open, and the
// session's next turn settles it (closes it once the run has ended) and runs.
func TestRecoveryReplaysAnIntentWithTheSameKeyAndBytes(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	in := seedIntent(t, s, "sess-a", s.keyFP, "posted before the crash", s.now())
	// The original POST, admitted by Hermes, its answer never recorded.
	if _, _, err := fake.api(testAPIKey).postRun(context.Background(), in.IdempotencyKey, in.Body); err != nil {
		t.Fatal(err)
	}
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	posts := fake.snapshotPosts()
	if len(posts) != 2 || posts[1].Key != in.IdempotencyKey || !bytes.Equal(posts[1].Body, in.Body) {
		t.Fatalf("THE REPLAY IS NOT THE INTENT: %+v", posts)
	}
	if n := fake.runCount(); n != 1 {
		t.Fatalf("THE INTENT RAN %d TIMES", n)
	}
	if sess, _ := s.store.get("sess-a"); sess.Intent == nil || sess.Intent.RunID != "run_1" {
		t.Fatalf("after the replay the intent should record run_1 and stay open: %+v", sess.Intent)
	}
	eventually(t, "the replayed run ends in Hermes", func() bool {
		st, _ := fake.api(testAPIKey).getRun(context.Background(), "run_1")
		return st.Status == "completed"
	})
	code, body := turn(t, s, "sess-a", "next")
	sess, _ := s.store.get("sess-a")
	if code != http.StatusOK || replyText(t, body) != "echo: next" || sess.LastRunID != "run_2" || fake.runCount() != 2 {
		t.Fatalf("the next turn did not settle the replayed intent and run: %d %s, last run %q, %d runs", code, body, sess.LastRunID, fake.runCount())
	}
}

// An intent Hermes never saw is posted once by the replay (and runs once).
func TestRecoveryPostsAnIntentHermesNeverSaw(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	in := seedIntent(t, s, "sess-a", s.keyFP, "never arrived", s.now())
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts := fake.snapshotPosts(); len(posts) != 1 || posts[0].Key != in.IdempotencyKey || !bytes.Equal(posts[0].Body, in.Body) {
		t.Fatalf("posts %+v", posts)
	}
	if s2, _ := s.store.get("sess-a"); s2.Intent == nil || s2.Intent.RunID != "run_1" || fake.runCount() != 1 {
		t.Fatalf("intent %+v, %d run(s)", s2.Intent, fake.runCount())
	}
}

// 🔴 PAST THE 24 h KEY RETENTION AN INTENT IS NEVER RE-POSTED: Hermes may have
// forgotten the key, and a replay would then start a second run. It is resolved
// from the session's messages instead.
func TestAnIntentPastTheKeyRetentionIsNotReposted(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	in := seedIntent(t, s, "sess-a", s.keyFP, "a day old", s.now().Add(-intentRetention-time.Minute))
	sess, _ := s.store.get("sess-a")
	fake.messages[sess.HermesID] = []map[string]any{{"role": "user", "content": "a day old"}}
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts := fake.snapshotPosts(); len(posts) != 0 {
		t.Fatalf("AN INTENT PAST RETENTION WAS RE-POSTED: %+v", posts)
	}
	after, _ := s.store.get("sess-a")
	if after.Intent != nil || !strings.Contains(after.Notice, "reached Hermes") {
		t.Fatalf("intent %+v notice %q (idempotency key %s)", after.Intent, after.Notice, in.IdempotencyKey)
	}
}

// 🔴 A KEY CHANGE: THE OLD KEY'S INTENT IS NEVER RE-POSTED, IT IS RESOLVED FROM
// THE MESSAGES OF THE SESSION IT WAS POSTED TO, AND ONLY THEN IS EVERY OLD-KEY
// SESSION RETIRED. The key file was replaced: runs, idempotency keys and
// resolve-unknown are scoped by the key, so a re-post would admit a SECOND run,
// and the new key cannot see whether an old run blocks its session.
func TestAKeyChangeResolvesThenRetires(t *testing.T) {
	const oldKey = "old-api-key-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fake := newFakeHermes(t, testAPIKey)
	fake.keys[oldKey] = true
	s := newTestServer(t, fake, testAPIKey)
	oldFP := keyFingerprint(oldKey)
	in := seedIntent(t, s, "sess-a", oldFP, "posted under the old key", s.now())
	before, _ := s.store.get("sess-a")
	fake.messages[before.HermesID] = []map[string]any{{"role": "user", "content": "posted under the old key"}}
	// A second old-key session with no intent, whose last run blocks it.
	other, _ := s.store.ensure("sess-b", oldFP)
	setLastRun(t, s, "sess-b", fake.admitUnknown(oldKey, other.HermesID, "blocked"))
	// And one already under the current key, untouched.
	cur, _ := s.store.ensure("sess-c", s.keyFP)

	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts := fake.snapshotPosts(); len(posts) != 0 {
		t.Fatalf("AN OLD-KEY INTENT WAS RE-POSTED UNDER THE NEW KEY: %+v (intent key %s)", posts, in.IdempotencyKey)
	}
	fake.mu.Lock()
	read := append([]string(nil), fake.readMsgs...)
	fake.mu.Unlock()
	if len(read) == 0 || read[0] != before.HermesID {
		t.Fatalf("THE INTENT WAS NOT RESOLVED AGAINST THE SESSION IT WAS POSTED TO (%s): messages read %v", before.HermesID, read)
	}
	a, _ := s.store.get("sess-a")
	b, _ := s.store.get("sess-b")
	c, _ := s.store.get("sess-c")
	for _, x := range []struct {
		name    string
		got     session
		was     string
		retired bool
	}{{"sess-a", a, before.HermesID, true}, {"sess-b", b, other.HermesID, true}, {"sess-c", c, cur.HermesID, false}} {
		switch {
		case x.retired && (x.got.HermesID == x.was || x.got.KeyFP != s.keyFP || x.got.LastRunID != "" || !strings.Contains(x.got.Notice, retiredNotice)):
			t.Fatalf("%s was not retired: %+v", x.name, x.got)
		case !x.retired && (x.got.HermesID != x.was || x.got.Notice != ""):
			t.Fatalf("%s was retired though its key is current: %+v", x.name, x.got)
		}
	}
	if !strings.Contains(a.Notice, "reached Hermes") || a.Intent != nil {
		t.Fatalf("sess-a: the intent's outcome is not reported: %+v", a)
	}
	// The next turn on a retired session starts with the retirement notice.
	code, body := turn(t, s, "sess-b", "hello again")
	if code != http.StatusOK || !strings.HasPrefix(replyText(t, body), retiredNotice) {
		t.Fatalf("next turn on a retired session: %d %s", code, body)
	}
}

// 🔴 RESOLVE-UNKNOWN AT RECOVERY: a session whose last run was left unknown by a
// SIGKILL is resolved before any turn is accepted, with the exact body.
func TestRecoveryResolvesAnUnknownLastRun(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	sess, _ := s.store.ensure("sess-a", s.keyFP)
	prev := fake.admitUnknown(testAPIKey, sess.HermesID, "killed")
	setLastRun(t, s, "sess-a", prev)
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.resolves) != 1 || fake.resolves[0].RunID != prev || keySet(fake.resolves[0].Body) != "admission_id,execution_generation" {
		t.Fatalf("resolves %+v", fake.resolves)
	}
}

// A replayed intent answered 409 idempotency_key_conflict closes with "may
// have run", and is not retried.
func TestARecoveryKeyConflictIsMayHaveRun(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	in := seedIntent(t, s, "sess-a", s.keyFP, "conflicting", s.now())
	// The key was used with a different body.
	if _, _, err := fake.api(testAPIKey).postRun(context.Background(), in.IdempotencyKey, []byte(`{"input":"other","session_id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := s.store.get("sess-a")
	if after.Intent != nil || !strings.Contains(after.Notice, "may have run") {
		t.Fatalf("%+v", after)
	}
	if n := len(fake.snapshotPosts()); n != 2 {
		t.Fatalf("%d POSTs; want the original and one replay, no retry", n)
	}
}
