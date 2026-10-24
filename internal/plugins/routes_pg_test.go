package plugins

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// THE SESSION ROUTE LIFECYCLE (session_routes, migration 0021) against a real
// Postgres: open → answer → relay as an approved draft (allowlisted) or a
// pending one (not), exactly once; the timeout; the refusals; and that the
// plugin's ordinary claim path takes an auto-approved draft unchanged.

var target = RouteTarget{Kind: "mirror", Key: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", Label: "mango:1 @ tower"}

func (w draftWorld) route(t *testing.T, m Message) Route {
	t.Helper()
	r, err := w.s.CreateRoute(session(2), w.chief.ID, 2, m.ID, target, time.Hour)
	if err != nil {
		t.Fatalf("route %d: %v", m.ID, err)
	}
	return r
}

// auditActions is the audit trail for one object, oldest first.
func auditActions(t *testing.T, w draftWorld, object string) []string {
	t.Helper()
	rows, err := w.s.pool.Query(authz.System(context.Background()), `SELECT action FROM audit_log WHERE object = $1 ORDER BY id`, object)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

func TestARoutedAnswerFromAnAllowlistedSenderIsRelayedApprovedAndClaimedOnce(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if r.State != RouteRouted || !ValidRouteID(r.RouteID) || r.TimeoutAt.Sub(r.CreatedAt) < 59*time.Minute {
		t.Fatalf("route = %+v", r)
	}
	a, fresh, err := w.s.AnswerRoute(session(2), r.RouteID, "Noon works for us.")
	if err != nil || !fresh || a.State != RouteAnswered || a.ReplyBody != "Noon works for us." {
		t.Fatalf("answer: %+v %v %v", a, fresh, err)
	}
	got, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, true)
	if err != nil || got.State != RouteRelayed || d.Status != DraftApproved || d.ApprovedAt == nil || got.DraftID == nil || *got.DraftID != d.ID {
		t.Fatalf("relay: %+v / %+v / %v", got, d, err)
	}
	// 🔴 EXACTLY ONCE: relaying again — and again concurrently — files nothing new.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, again, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, true)
			if err != nil || again.ID != d.ID {
				t.Errorf("a repeated relay: draft %d, %v; want the same draft %d", again.ID, err, d.ID)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_reply_drafts WHERE route_id = $1`, r.RouteID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d drafts name the route (%v), want 1", n, err)
	}
	// The plugin's ordinary claim takes it — the path the plugin already uses.
	c, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID)
	if err != nil || !ok || c.DraftID != d.ID || c.Body != "Noon works for us." || c.MessageExternalID != w.m.ExternalID {
		t.Fatalf("claim: %+v %v %v", c, ok, err)
	}
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatal("the relayed draft was claimed twice")
	}
	if got := auditActions(t, w, RouteObject(r.RouteID)); len(got) != 3 || got[0] != "session_route.routed" ||
		got[1] != "session_route.answered" || got[2] != "session_route.relayed" {
		t.Fatalf("route audit = %v", got)
	}
	if got := auditActions(t, w, DraftObject(d.ID)); len(got) < 2 || got[0] != "plugin.draft.filed" || got[1] != "plugin.draft.approved" {
		t.Fatalf("draft audit = %v", got)
	}
}

// TestANonAllowlistedAnswerFallsBackToAPendingDraft: not approved, so the
// plugin claims nothing until the owner decides.
func TestANonAllowlistedAnswerFallsBackToAPendingDraft(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "Let me check."); err != nil {
		t.Fatal(err)
	}
	got, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, false)
	if err != nil || got.State != RouteFallback || d.Status != DraftPending || d.ApprovedAt != nil {
		t.Fatalf("relay: %+v / %+v / %v", got, d, err)
	}
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatal("a fallback draft was claimable before the owner approved it")
	}
	w.approve(t, d)
	if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok || c.DraftID != d.ID {
		t.Fatal("the owner-approved fallback draft was not claimable")
	}
}

func TestAnAnswerIsIdempotentAndARouteClosesAfterIt(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, fresh, err := w.s.AnswerRoute(session(2), r.RouteID, "Yes."); err != nil || !fresh {
		t.Fatal(err)
	}
	if _, fresh, err := w.s.AnswerRoute(session(2), r.RouteID, "Yes."); err != nil || fresh {
		t.Fatalf("the same answer again: fresh=%v err=%v; want the stored route, unchanged", fresh, err)
	}
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "No."); !errors.Is(err, ErrRouteClosed) {
		t.Fatalf("a different second answer: %v, want ErrRouteClosed", err)
	}
	// Another owner cannot see, answer or relay it.
	if _, _, err := w.s.AnswerRoute(session(3), r.RouteID, "Yes."); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("another owner's answer: %v", err)
	}
	if _, _, err := w.s.RelayRoute(session(3), r.RouteID, "x", true); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("another owner's relay: %v", err)
	}
	if _, _, err := w.s.AnswerRoute(session(2), "rt_000000000000000000000000", "Yes."); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("an unknown route: %v", err)
	}
}

func TestARouteRefusesWhatTheChiefMayNotRoute(t *testing.T) {
	w := newDraftWorld(t)
	worker := mustAgent(t, w.dsn, 2, "plg-route-worker")
	if _, err := w.s.CreateRoute(session(2), worker.ID, 2, w.m.ID, target, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a message not delivered to this agent: %v", err)
	}
	noReply := w.delivered(t, "m-noreply", true)
	if _, err := w.s.CreateRoute(session(2), w.chief.ID, 2, noReply.ID, target, time.Hour); !errors.Is(err, ErrNoReply) {
		t.Fatalf("a no_reply message: %v", err)
	}
	w.route(t, w.m)
	if _, err := w.s.CreateRoute(session(2), w.chief.ID, 2, w.m.ID, target, time.Hour); !errors.Is(err, ErrAlreadyRouted) {
		t.Fatalf("a second open route: %v", err)
	}
	inflight := w.delivered(t, "m-inflight", false)
	w.approve(t, w.file(t, inflight, "already answered"))
	if _, err := w.s.CreateRoute(session(2), w.chief.ID, 2, inflight.ID, target, time.Hour); !errors.Is(err, ErrReplyInFlight) {
		t.Fatalf("a message with a reply in flight: %v", err)
	}
}

// TestARouteTimesOutOnlyPastItsDeadlineAndThenTakesNoAnswer.
func TestARouteTimesOutOnlyPastItsDeadlineAndThenTakesNoAnswer(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, timedOut, err := w.s.TimeOutRoute(session(2), r.RouteID); err != nil || timedOut {
		t.Fatalf("before the deadline: timedOut=%v %v", timedOut, err)
	}
	due, err := w.s.ExpiredRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.RouteID == r.RouteID {
			t.Fatal("a route before its deadline is listed as expired")
		}
	}
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+r.RouteID+`'`)
	due, _ = w.s.ExpiredRoutes(context.Background())
	found := false
	for _, d := range due {
		found = found || (d.RouteID == r.RouteID && d.OwnerID == 2)
	}
	if !found {
		t.Fatalf("the overdue route is not listed: %+v", due)
	}
	got, timedOut, err := w.s.TimeOutRoute(session(2), r.RouteID)
	if err != nil || !timedOut || got.State != RouteTimedOut || got.FinishedAt == nil {
		t.Fatalf("time out: %+v %v %v", got, timedOut, err)
	}
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "late"); !errors.Is(err, ErrRouteClosed) {
		t.Fatalf("an answer after the timeout: %v", err)
	}
	if _, _, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, true); !errors.Is(err, ErrRouteNotAnswered) {
		t.Fatalf("a relay after the timeout: %v", err)
	}
	// The message can be routed again once its route is closed.
	w.route(t, w.m)
	if got := auditActions(t, w, RouteObject(r.RouteID)); len(got) != 2 || got[1] != "session_route.timed_out" {
		t.Fatalf("audit = %v", got)
	}
}

// TestARelayThatCannotBeMadeFailsTheRoute: a reply already in flight for the
// message when the answer arrives fails the route, files nothing.
func TestARelayThatCannotBeMadeFailsTheRoute(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	if _, _, err := w.s.AnswerRoute(session(2), r.RouteID, "From the session."); err != nil {
		t.Fatal(err)
	}
	w.approve(t, w.file(t, w.m, "From the chief meanwhile."))
	got, d, err := w.s.RelayRoute(session(2), r.RouteID, w.chief.Name, true)
	if !errors.Is(err, ErrRouteClosed) || got.State != RouteFailed || d.ID != 0 || got.Error == "" {
		t.Fatalf("relay: %+v / %+v / %v", got, d, err)
	}
	if got := auditActions(t, w, RouteObject(r.RouteID)); got[len(got)-1] != "session_route.failed" {
		t.Fatalf("audit = %v", got)
	}
}

// TestCreateRouteFromRecordsWhoRoutedIt: an MCP origin's route is the same row
// and audit action as the chief's, with routed_via "mcp from <host>/<session>";
// a malformed origin is refused before anything is written. CONTROL: the
// chief's (zero) origin records no routed_via.
func TestCreateRouteFromRecordsWhoRoutedIt(t *testing.T) {
	w := newDraftWorld(t)
	const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	for what, o := range map[string]RouteOrigin{
		"mcp without a session":      {Via: RouteViaMCP, Host: "tower"},
		"mcp without a host":         {Via: RouteViaMCP, Session: sid},
		"the chief naming a host":    {Host: "tower"},
		"the chief naming a session": {Session: sid},
		"an unknown origin":          {Via: "browser", Host: "tower", Session: sid},
	} {
		if _, err := w.s.CreateRouteFrom(session(2), w.chief.ID, 2, w.m.ID, target, time.Hour, o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", what, err)
		}
	}
	var n int
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE plugin_message_id = $1`, w.m.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused origin wrote %d route(s) (%v)", n, err)
	}
	via := func(r Route) string {
		var v string
		if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(detail->>'routed_via', '') FROM audit_log
			WHERE object = $1 AND action = 'session_route.routed'`, RouteObject(r.RouteID)).Scan(&v); err != nil {
			t.Fatalf("the routed audit row of %s: %v", r.RouteID, err)
		}
		return v
	}
	r, err := w.s.CreateRouteFrom(session(2), w.chief.ID, 2, w.m.ID, target, time.Hour, RouteOrigin{Via: RouteViaMCP, Host: "tower", Session: sid})
	if err != nil || r.State != RouteRouted || r.AgentID == nil || *r.AgentID != w.chief.ID {
		t.Fatalf("an MCP route: %+v %v", r, err)
	}
	if got := via(r); got != "mcp from tower/"+sid {
		t.Fatalf("the MCP route's routed_via = %q", got)
	}
	if got := via(w.route(t, w.delivered(t, "m-chief", false))); got != "" {
		t.Fatalf("control, the chief's route: routed_via = %q", got)
	}
}

// TestRoutableMessagesIsTheOwnersDeliveredRepliableMessages: newest first, only
// messages delivered to a chief that may be replied to, each with its LATEST
// route — not a queued one, not a no_reply one, not another owner's.
// MessageRoute is that latest route too.
func TestRoutableMessagesIsTheOwnersDeliveredRepliableMessages(t *testing.T) {
	w := newDraftWorld(t)
	noReply := w.delivered(t, "m-noreply", true)
	queued, _, err := w.s.Submit(plugin(2), w.p.ID, msg("m-queued"))
	if err != nil {
		t.Fatal(err)
	}
	later := w.delivered(t, "m-later", false)
	_, p3 := mustCreate(t, w.s, 3, "example-inbox")
	chief3 := mustAgent(t, w.dsn, 3, "plg-chief-3")
	theirs, _, err := w.s.Submit(plugin(3), p3.ID, msg("m-theirs"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := w.s.BeginDelivery(plugin(3), theirs.ID, chief3.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	first := w.route(t, w.m)
	if _, err := w.s.FailRoute(session(2), first.RouteID, "test", ""); err != nil {
		t.Fatal(err)
	}
	second := w.route(t, w.m)

	got, err := w.s.RoutableMessages(session(2), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Message.ID != later.ID || got[1].Message.ID != w.m.ID {
		t.Fatalf("RoutableMessages = %+v; want exactly [%d %d] (no_reply %d, queued %d, another owner's %d left out)",
			got, later.ID, w.m.ID, noReply.ID, queued.ID, theirs.ID)
	}
	if got[0].Route.RouteID != "" || got[1].Route.RouteID != second.RouteID || got[1].Route.State != RouteRouted ||
		got[1].Route.TargetKey != target.Key || got[1].PluginLabel != w.p.Label {
		t.Fatalf("the routes: %+v / %+v", got[0].Route, got[1].Route)
	}
	if one, err := w.s.RoutableMessages(session(2), 1); err != nil || len(one) != 1 || one[0].Message.ID != later.ID {
		t.Fatalf("limit 1: %+v %v", one, err)
	}
	if r, err := w.s.MessageRoute(session(2), w.m.ID); err != nil || r.RouteID != second.RouteID {
		t.Fatalf("MessageRoute = %+v %v; want %s", r, err, second.RouteID)
	}
	if _, err := w.s.MessageRoute(session(2), later.ID); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("MessageRoute of an unrouted message: %v", err)
	}
	// Another owner's view is theirs alone.
	if g3, err := w.s.RoutableMessages(session(3), 10); err != nil || len(g3) != 1 || g3[0].Message.ID != theirs.ID {
		t.Fatalf("owner 3's RoutableMessages = %+v %v", g3, err)
	}
}
