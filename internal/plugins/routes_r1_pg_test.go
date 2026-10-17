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

// 🟡 round 2: relays racing at the pending cap fail the route ONCE — in the
// relay's own transaction — so exactly one caller sees ErrRouteClosed (and
// hands the message back); the rest find it already failed.
func TestRelaysRacingAtTheCapFailTheRouteOnce(t *testing.T) {
	w := newDraftWorld(t)
	for i := 0; i < MaxPendingDraftsPerAgent; i++ {
		w.file(t, w.delivered(t, "race-cap-"+strconv.Itoa(i), false), "pending "+strconv.Itoa(i))
	}
	for round := 0; round < 25; round++ {
		m := w.delivered(t, "race-cap-route-"+strconv.Itoa(round), false)
		r := w.route(t, m)
		if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "From the session."); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		closed, notAnswered := 0, 0
		start := make(chan struct{})
		const racers = 8
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, false)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case errors.Is(err, ErrRouteClosed):
					closed++
				case errors.Is(err, ErrRouteNotAnswered):
					notAnswered++
				default:
					t.Errorf("a racing relay: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if closed != 1 || notAnswered != racers-1 {
			t.Fatalf("round %d: %d saw the route fail, %d found it failed; want 1 and %d", round, closed, notAnswered, racers-1)
		}
		n := 0
		for _, a := range auditActions(t, w, RouteObject(r.RouteID)) {
			if a == "session_route.failed" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: %d session_route.failed audit rows, want 1", round, n)
		}
	}
}

// 🟡 round 2: every route that ends WITHOUT its chief already knowing owes a
// hand-back — a timeout, a relay that could not be made, a failure that names
// one — and TakeHandBack gives it out exactly once. A failure the chief was
// told about directly (an injection that failed in its own tool call) owes
// nothing.
func TestAnEndedRouteOwesItsChiefOneHandBack(t *testing.T) {
	w := newDraftWorld(t)
	take := func(id string) (string, bool) {
		t.Helper()
		_, reason, ok, err := w.s.TakeHandBack(session(2), id)
		if err != nil {
			t.Fatal(err)
		}
		return reason, ok
	}
	// A timeout.
	r1 := w.route(t, w.m)
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+r1.RouteID+`'`)
	if _, timedOut, err := w.s.TimeOutRoute(session(2), r1.RouteID); err != nil || !timedOut {
		t.Fatal(err)
	}
	if reason, ok := take(r1.RouteID); !ok || !strings.HasPrefix(reason, "no answer came by ") {
		t.Fatalf("a timed-out route owes %q (%v)", reason, ok)
	}
	if _, ok := take(r1.RouteID); ok {
		t.Fatal("the hand-back was given out twice")
	}
	// A relay that cannot be made.
	m2 := w.delivered(t, "owed-relay", false)
	r2 := w.route(t, m2)
	if _, _, err := w.s.AnswerRoute(session(2), r2.RouteID, "From the session."); err != nil {
		t.Fatal(err)
	}
	w.approve(t, w.file(t, m2, "From the chief meanwhile."))
	if _, _, err := w.s.RelayRoute(session(2), r2.RouteID, w.chief.Name, true); !errors.Is(err, ErrRouteClosed) {
		t.Fatal(err)
	}
	if reason, ok := take(r2.RouteID); !ok || !strings.HasPrefix(reason, "its answer could not be sent (") {
		t.Fatalf("a route whose relay failed owes %q (%v)", reason, ok)
	}
	// A failure that names one, and one that does not.
	m3 := w.delivered(t, "owed-moved", false)
	r3 := w.route(t, m3)
	if _, err := w.s.FailRoute(session(2), r3.RouteID, "moved", "the window moved on"); err != nil {
		t.Fatal(err)
	}
	if reason, ok := take(r3.RouteID); !ok || reason != "the window moved on" {
		t.Fatalf("a failure naming a hand-back owes %q (%v)", reason, ok)
	}
	m4 := w.delivered(t, "owed-none", false)
	r4 := w.route(t, m4)
	if _, err := w.s.FailRoute(session(2), r4.RouteID, "inject failed", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := take(r4.RouteID); ok {
		t.Fatal("a failure the chief was told about directly owes a hand-back")
	}
	// The sweep lists exactly the owed ones it has not delivered.
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET handback = 'owed' WHERE route_id = '`+r4.RouteID+`'`)
	due, err := w.s.ExpiredRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, d := range due {
		if d.HandBack {
			listed[d.RouteID] = true
		}
	}
	if !listed[r4.RouteID] || listed[r1.RouteID] || listed[r2.RouteID] || listed[r3.RouteID] {
		t.Fatalf("the sweep lists hand-backs %v; want only %s", listed, r4.RouteID)
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
