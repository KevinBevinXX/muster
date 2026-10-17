package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Round-1 guards on the route store: the stuck-answered sweep, the two-first-
// relays race, FileDraft's accounting and cap on a relay, and the plugin's
// sender_verified mark.

// 🟡1: an answered route whose relay never happened is listed for the sweep
// once RelayGrace has passed — not before — so it cannot hold the message's one
// open route forever.
func TestAnAnsweredRouteWhoseRelayNeverHappenedIsListedForTheSweep(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "Noon works."); err != nil {
		t.Fatal(err)
	}
	listed := func() (DueRoute, bool) {
		due, err := w.s.ExpiredRoutes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			if d.RouteID == r.RouteID {
				return d, true
			}
		}
		return DueRoute{}, false
	}
	if _, ok := listed(); ok {
		t.Fatal("an answer still inside RelayGrace is listed (a relay merely in progress would be retried)")
	}
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET answered_at = now() - interval '2 minutes' WHERE route_id = '`+r.RouteID+`'`)
	d, ok := listed()
	if !ok || d.State != RouteAnswered || d.OwnerID != 2 {
		t.Fatalf("a stuck answered route is not listed for a relay retry: %+v %v", d, ok)
	}
	// The retry is the ordinary relay: exactly once, and then it is off the list.
	if got, _, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, false); err != nil || got.State != RouteFallback {
		t.Fatalf("the retried relay: %+v %v", got, err)
	}
	if _, ok := listed(); ok {
		t.Fatal("a relayed route is still listed")
	}
}

// 🟡6: two FIRST relays of one answered route at once file exactly one draft,
// and both callers see that same draft.
func TestTwoFirstRelaysFileOneDraft(t *testing.T) {
	for round := 0; round < 5; round++ {
		w := newDraftWorld(t)
		m := w.delivered(t, "race-"+strconv.Itoa(round), false)
		r := w.route(t, m)
		if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "Noon works."); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		ids := make([]int64, 4)
		start := make(chan struct{})
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, i%2 == 0)
				if err != nil {
					t.Errorf("relay %d: %v", i, err)
				}
				ids[i] = d.ID
			}(i)
		}
		close(start)
		wg.Wait()
		for _, id := range ids {
			if id == 0 || id != ids[0] {
				t.Fatalf("round %d: the racing relays saw drafts %v, want one and the same", round, ids)
			}
		}
		var n int
		if err := w.s.pool.QueryRow(authz.System(context.Background()),
			`SELECT count(*) FROM plugin_reply_drafts WHERE message_id = $1`, m.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("round %d: %d drafts for the message (%v), want 1", round, n, err)
		}
	}
}

// 🟡7: a relay supersedes the message's pending draft WITH FileDraft's
// accounting — the count on the filing's audit row.
func TestARelaySupersedingAPendingDraftCountsItLikeFileDraft(t *testing.T) {
	w := newDraftWorld(t)
	old := w.file(t, w.m, "From the chief first.")
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "From the session."); err != nil {
		t.Fatal(err)
	}
	_, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := w.s.Draft(session(2), old.ID); got.Status != DraftSuperseded {
		t.Fatalf("the chief's pending draft is %s, want superseded", got.Status)
	}
	var raw []byte
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT detail FROM audit_log
		WHERE object = $1 AND action = 'plugin.draft.filed'`, DraftObject(d.ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["superseded"] != float64(1) {
		t.Fatalf("the filing's audit row says superseded=%v, want 1 (FileDraft's accounting): %s", detail["superseded"], raw)
	}
}

// 🟡7: a relay that would file a PENDING draft obeys FileDraft's cap,
// MaxPendingDraftsPerAgent: at the cap the route fails, saying so, and nothing
// is filed. An AUTO-APPROVED relay awaits nobody, so the cap is not its.
func TestAPendingRelayObeysTheDraftCap(t *testing.T) {
	w := newDraftWorld(t)
	for i := 0; i < MaxPendingDraftsPerAgent; i++ {
		w.file(t, w.delivered(t, "cap-"+strconv.Itoa(i), false), "pending "+strconv.Itoa(i))
	}
	// FileDraft itself refuses the next one: the parity the relay must match.
	if _, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, "one more"); !errors.Is(err, ErrTooManyPendingDrafts) {
		t.Fatalf("the control: FileDraft at the cap: %v", err)
	}
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "From the session."); err != nil {
		t.Fatal(err)
	}
	got, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, false)
	if !errors.Is(err, ErrRouteClosed) || got.State != RouteFailed || d.ID != 0 {
		t.Fatalf("a pending relay at the cap: %+v / %+v / %v", got, d, err)
	}
	var n int
	_ = w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_reply_drafts WHERE route_id = $1`, r.RouteID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d drafts filed past the cap", n)
	}
	// Approved: not awaiting the owner, so not capped.
	m2 := w.delivered(t, "cap-approved", false)
	r2 := w.route(t, m2)
	if _, _, err := w.s.AnswerRoute(session(2), r2.RouteID, "Approved one."); err != nil {
		t.Fatal(err)
	}
	if got, _, err := w.s.RelayRoute(session(2), r2.RouteID, w.chief.Name, true); err != nil || got.State != RouteRelayed {
		t.Fatalf("an approved relay at the pending cap: %+v %v", got, err)
	}
}

// R1: sender_verified is the plugin's to set at submission, read back as set,
// false by default, and immutable afterwards.
func TestSenderVerifiedIsStoredAndImmutable(t *testing.T) {
	w := newDraftWorld(t)
	if w.m.SenderVerified {
		t.Fatal("a message submitted without sender_verified reads as verified")
	}
	in := msg("verified-1")
	in.SenderVerified = true
	m, _, err := w.s.Submit(plugin(2), w.p.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.s.Message(plugin(2), m.ID)
	if err != nil || !m.SenderVerified || !got.SenderVerified {
		t.Fatalf("sender_verified did not round-trip: submit %v, read %v (%v)", m.SenderVerified, got.SenderVerified, err)
	}
	var wire MessageIn
	if err := json.Unmarshal([]byte(`{"external_id":"x","sender":"a@b","body":"b","sender_verified":true}`), &wire); err != nil || !wire.SenderVerified {
		t.Fatalf("the wire field sender_verified does not decode: %v %v", wire.SenderVerified, err)
	}
	// Control: the same UPDATE touching another column goes through.
	if _, err := w.s.pool.Exec(authz.System(context.Background()), `UPDATE plugin_messages SET subject = subject WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("the control update: %v", err)
	}
	_, err = w.s.pool.Exec(authz.System(context.Background()), `UPDATE plugin_messages SET sender_verified = false WHERE id = $1`, m.ID)
	if err == nil || !strings.Contains(err.Error(), "sender_verified mark is immutable") {
		t.Fatalf("sender_verified was changed after submission (err %v)", err)
	}
}

// 🟡6 (an invariant guard: the state check predates round 1): an ANSWERED
// route past its deadline is not timed out — the answer wins, and the sweep
// leaves it for the relay.
func TestAnAnsweredRoutePastItsDeadlineIsNotTimedOut(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "Just in time."); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+r.RouteID+`'`)
	got, timedOut, err := w.s.TimeOutRoute(session(2), r.RouteID)
	if err != nil || timedOut || got.State != RouteAnswered {
		t.Fatalf("an answered route past its deadline: %+v timedOut=%v %v", got, timedOut, err)
	}
}
