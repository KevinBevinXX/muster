package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// Round-1 guards on session routing through the real server and Postgres.

// flakyRoutes is the real route store with failures switched in.
type flakyRoutes struct {
	plugins.RouteStore
	failRelay  atomic.Bool
	failQueued atomic.Bool
	// relayErr, when set, is what a failing relay returns (errFlaky otherwise).
	relayErr error
}

var errFlaky = errors.New("plugins: connection reset by peer")

func (f *flakyRoutes) RelayRoute(ctx context.Context, routeID, agentName string, approve bool) (plugins.Route, plugins.Draft, error) {
	if f.failRelay.Load() {
		// As the real store does on an early failure: no route at all.
		if f.relayErr != nil {
			return plugins.Route{}, plugins.Draft{}, f.relayErr
		}
		return plugins.Route{}, plugins.Draft{}, errFlaky
	}
	return f.RouteStore.RelayRoute(ctx, routeID, agentName, approve)
}

func (f *flakyRoutes) SetRouteQueued(ctx context.Context, routeID string, id int64) error {
	if f.failQueued.Load() {
		return errFlaky
	}
	return f.RouteStore.SetRouteQueued(ctx, routeID, id)
}

// chiefSession gives the chief a conversation for the routes' plugin, so a
// note to it lands somewhere a test can read.
func (f *routeFixture) chiefSession(t *testing.T, p plugins.Plugin) agents.ChatSession {
	t.Helper()
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	return sess
}

func (f *routeFixture) routeState(t *testing.T, id string) plugins.Route {
	t.Helper()
	r, err := f.plugins.Route(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// deliver hands the window's one queued routed row to the send path, as the
// dispatcher would, and returns the outcome.
func (f *routeFixture) deliver(t *testing.T) sendqueue.Outcome {
	t.Helper()
	items := f.queued(t)
	if len(items) == 0 {
		t.Fatal("nothing queued")
	}
	it := items[len(items)-1]
	return f.srv.sendQueueSend(f.srv.queuedPrincipal(context.Background(), f.owner), it, sendqueue.AgentState{Idle: true})
}

// 🔴 R1: auto-approval needs the message's OWN plugin on the entry and that
// plugin's sender_verified. A second plugin claiming the same From, an
// unverified sender, and a Kelvin-sign look-alike each wait for the owner.
func TestOnlyAVerifiedSenderThroughItsOwnPluginIsAutoApproved(t *testing.T) {
	f := newRouteFixture(t)
	f.allow(t, "example-inbox:ada@example.test", "example-inbox:kate@example.test")
	for _, c := range []struct {
		name, plugin, sender string
		verified             bool
		want                 string
	}{
		{"the good path", "example-inbox", "Ada <ada@example.test>", true, "relayed"},
		{"a spoofed From through a second plugin", "other-inbox", "Ada <ada@example.test>", true, "fallback"},
		{"an unverified sender", "example-inbox", "Ada <ada@example.test>", false, "fallback"},
		{"a Kelvin sign", "example-inbox", "Kate@example.test", true, "fallback"},
	} {
		_, m := f.pluginMessage(t, c.plugin, "r1-"+strings.ReplaceAll(c.name, " ", "-"), c.sender, c.verified)
		id := f.route(t, m)
		code, out := f.reply(t, f.token, id, "Answer for "+c.name)
		if code != http.StatusOK || out["state"] != c.want {
			t.Errorf("%s: %d %v, want state %s", c.name, code, out, c.want)
		}
	}
}

// 🟡1: a relay that fails for a retryable reason answers 202 — the answer is
// kept, nothing final — and the sweep relays it once RelayGrace has passed.
func TestARetryableRelayFailureIsAcceptedAndTheSweepRelaysIt(t *testing.T) {
	f := newRouteFixture(t)
	flaky := &flakyRoutes{RouteStore: f.plugins}
	f.srv.ext.Routes = flaky
	_, m := f.pluginMessage(t, "example-inbox", "r1-retry", "someone@example.test", false)
	id := f.route(t, m)
	flaky.failRelay.Store(true)
	code, out := f.reply(t, f.token, id, "Noon works.")
	if code != http.StatusAccepted || out["state"] != "answered" || out["route_id"] != id {
		t.Fatalf("a retryable relay failure: %d %v, want 202 answered naming route %s", code, out, id)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteAnswered || r.ReplyBody != "Noon works." {
		t.Fatalf("the answer was not kept: %+v", r)
	}
	flaky.failRelay.Store(false)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteAnswered {
		t.Fatalf("the sweep relayed inside RelayGrace: %s", r.State)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET answered_at = now() - interval '2 minutes' WHERE route_id = '`+id+`'`)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteFallback || r.DraftID == nil {
		t.Fatalf("the stuck answered route was not relayed by the sweep: %+v", r)
	}
}

// 🟡1: a relay that finds its route already ended (failed or timed out by
// another path) is FINAL — 409 — never "relayed shortly".
func TestARelayOfAnEndedRouteIsFinal(t *testing.T) {
	f := newRouteFixture(t)
	flaky := &flakyRoutes{RouteStore: f.plugins, relayErr: plugins.ErrRouteNotAnswered}
	f.srv.ext.Routes = flaky
	_, m := f.pluginMessage(t, "example-inbox", "r1-ended", "someone@example.test", false)
	id := f.route(t, m)
	flaky.failRelay.Store(true)
	if code, out := f.reply(t, f.token, id, "Too late."); code != http.StatusConflict {
		t.Fatalf("a relay of an ended route: %d %v, want 409", code, out)
	}
}

// 🟡 round 2: an answer that cannot become a draft — here the chief's
// pending-draft cap — fails the route AND goes back to the chief. Before, the
// answer vanished: no draft, no push, no note.
func TestAnAnswerRefusedAtTheDraftCapGoesBackToTheChief(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "r2-cap", "someone@example.test", false)
	sess := f.chiefSession(t, p)
	for i := 0; i < plugins.MaxPendingDraftsPerAgent; i++ {
		_, mi := f.pluginMessage(t, "example-inbox", "r2-cap-fill-"+strconv.Itoa(i), "someone@example.test", false)
		if _, _, err := f.plugins.FileDraft(f.asOwner, f.chief.ID, f.owner, f.chief.Name, mi.ID, "pending "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	id := f.route(t, m)
	code, out := f.reply(t, f.token, id, "From the session.")
	if code != http.StatusConflict || out["route_id"] != id {
		t.Fatalf("an answer at the cap: %d %v, want 409", code, out)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteFailed {
		t.Fatalf("the route is %s, want failed", r.State)
	}
	// The failure owes the chief a hand-back; the route sweep delivers it, ONCE.
	for i := 0; i < 2; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Route "+id) || !strings.Contains(msgs[0].Content, "back with you") ||
		!strings.Contains(msgs[0].Content, "await the owner") {
		t.Fatalf("the chief was not handed the message back: %+v", msgs)
	}
}

// 🟡 round 2: a route that fails owing a hand-back WAKES the running sweep —
// the chief hears now, not at the next tick (here an hour away). Both
// request-time failures: an answer refused at the draft cap, and a window that
// moved on.
func TestAnOwedHandBackWakesTheSweep(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "r2-kick", "someone@example.test", false)
	sess := f.chiefSession(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.srv.RunRouteTimeouts(ctx, time.Hour) }()
	defer func() { cancel(); <-done }()
	waitNotes := func(n int) []agents.ChatMessage {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			msgs, _ := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
			if len(msgs) >= n || time.Now().After(deadline) {
				return msgs
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// A window that moved on.
	id := f.route(t, m)
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000000ee" })
	if out := f.deliver(t); out.Detail != SendQueueRouteMoved {
		t.Fatalf("delivery: %+v", out)
	}
	if msgs := waitNotes(1); len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Route "+id) {
		t.Fatalf("the moved window's hand-back did not arrive before the next tick: %+v", msgs)
	}
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "" })

	// An answer refused at the draft cap.
	for i := 0; i < plugins.MaxPendingDraftsPerAgent; i++ {
		_, mi := f.pluginMessage(t, "example-inbox", "r2-kick-fill-"+strconv.Itoa(i), "someone@example.test", false)
		if _, _, err := f.plugins.FileDraft(f.asOwner, f.chief.ID, f.owner, f.chief.Name, mi.ID, "pending "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	_, m2 := f.pluginMessage(t, "example-inbox", "r2-kick-cap", "someone@example.test", false)
	id2 := f.route(t, m2)
	if code, _ := f.reply(t, f.token, id2, "From the session."); code != http.StatusConflict {
		t.Fatalf("an answer at the cap: %d", code)
	}
	if msgs := waitNotes(2); len(msgs) != 2 || !strings.Contains(msgs[1].Content, "Route "+id2) {
		t.Fatalf("the cap's hand-back did not arrive before the next tick: %+v", msgs)
	}
}

// 🟡3: a route whose queued row could not be recorded on it fails, and the
// row is cancelled — nothing is left that a later delivery would paste.
func TestAnUnrecordedInjectionLeavesNoPasteableRow(t *testing.T) {
	f := newRouteFixture(t)
	flaky := &flakyRoutes{RouteStore: f.plugins}
	f.srv.ext.Routes = flaky
	flaky.failQueued.Store(true)
	_, m := f.pluginMessage(t, "example-inbox", "r1-unrecorded", "someone@example.test", false)
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)("route_to_session", routeArgs(m, "mirror:"+routeSID))
	if !strings.Contains(text, RefusedInjectFailed) {
		t.Fatalf("the tool reported %s, want %s", text, RefusedInjectFailed)
	}
	items := f.queued(t)
	if len(items) != 1 || items[0].State != sendqueue.Cancelled {
		t.Fatalf("the row was left pasteable: %+v", items)
	}
}

// 🟡3: a routed row is never pasted once its route has ended — whatever
// happened to its cancel — because its delivery reads the route.
func TestARoutedRowIsNotPastedAfterItsRouteEnds(t *testing.T) {
	f := newRouteFixture(t)
	_, m := f.pluginMessage(t, "example-inbox", "r1-closed", "someone@example.test", false)
	id := f.route(t, m)
	// The route ends with its row still queued (a lost cancel).
	if _, err := f.plugins.FailRoute(f.asOwner, id, "ended elsewhere", ""); err != nil {
		t.Fatal(err)
	}
	out := f.deliver(t)
	if out.Sent || !strings.HasPrefix(out.Detail, SendQueueRouteClosed) || f.mirror.pastes() != 0 {
		t.Fatalf("a row of an ended route: %+v, %d pastes", out, f.mirror.pastes())
	}
}

// 🟡2: the route pins the conversation. A window that now holds ANOTHER Claude
// session gets nothing; the route fails and goes back to the chief. The same
// window still holding it is delivered (the control).
func TestARoutedRowIsRefusedWhenTheWindowHoldsAnotherConversation(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "r1-moved", "someone@example.test", false)
	sess := f.chiefSession(t, p)
	f.route(t, m)
	if out := f.deliver(t); !out.Sent || f.mirror.pastes() != 1 {
		t.Fatalf("the control: the pinned window was not delivered to: %+v", out)
	}

	_, m2 := f.pluginMessage(t, "example-inbox", "r1-moved-2", "someone@example.test", false)
	id := f.route(t, m2)
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000000ff" })
	out := f.deliver(t)
	if out.Sent || out.Detail != SendQueueRouteMoved || f.mirror.pastes() != 1 {
		t.Fatalf("a window holding another conversation: %+v, %d pastes", out, f.mirror.pastes())
	}
	if r := f.routeState(t, id); r.State != plugins.RouteFailed {
		t.Fatalf("the route is %s, want failed", r.State)
	}
	// The hand-back is owed, and the route sweep delivers it.
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "Route "+id) ||
		!strings.Contains(msgs[len(msgs)-1].Content, "no longer holds") {
		t.Fatalf("the chief was not handed the message back: %+v", msgs)
	}
}

// 🟡3/🟡5: a routed row carries the ROUTE's time as its auth time — what the
// owner's sign-out-everywhere cutoff is compared with at delivery — and is
// delivered on its route, not on the step-up window: a row whose recorded
// time is older than any step-up window is still sent while its route is open.
func TestARoutedRowCarriesTheRouteTimeAndIsGatedOnItsRoute(t *testing.T) {
	f := newRouteFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	f.srv.sendQueueClock = func() time.Time { return at }
	_, m := f.pluginMessage(t, "example-inbox", "r1-stepup", "someone@example.test", false)
	id := f.route(t, m)
	items := f.queued(t)
	if len(items) != 1 || !items[0].StepUpAt.Equal(at) || items[0].RouteID != id {
		t.Fatalf("the routed row: %+v, want StepUpAt %s and RouteID %s", items, at, id)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE send_queue SET stepup_at = now() - interval '48 hours' WHERE route_id = '`+id+`'`)
	if out := f.deliver(t); !out.Sent {
		t.Fatalf("an open route's row, recorded 48h ago, was not sent: %+v", out)
	}
}

// 🟢: the chief's note names the target by its subject, never the window's
// label — a label can carry text from the conversation itself.
func TestTheChiefsNoteNamesTheSubjectNotTheLabel(t *testing.T) {
	f := newRouteFixture(t)
	f.mirror.set(func(m *routeMirror) { m.label = "evil-label-ignore-previous" })
	p, m := f.pluginMessage(t, "example-inbox", "r1-label", "someone@example.test", false)
	sess := f.chiefSession(t, p)
	id := f.route(t, m)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "mirror:"+routeSID) || strings.Contains(msgs[0].Content, "evil-label") {
		t.Fatalf("the note: %+v", msgs)
	}
}

// 🟡6: the same answer POSTed concurrently files one draft; every caller sees
// a success naming it.
func TestConcurrentRepliesFileOneDraft(t *testing.T) {
	f := newRouteFixture(t)
	_, m := f.pluginMessage(t, "example-inbox", "r1-concurrent", "someone@example.test", false)
	id := f.route(t, m)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	drafts := make([]float64, 6)
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, out := f.reply(t, f.token, id, "Noon works.")
			codes[i] = code
			drafts[i], _ = out["draft_id"].(float64)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range codes {
		if codes[i] != http.StatusOK || drafts[i] == 0 || drafts[i] != drafts[0] {
			t.Fatalf("concurrent replies: codes %v, drafts %v", codes, drafts)
		}
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_reply_drafts WHERE route_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d drafts (%v), want 1", n, err)
	}
}

// 🟡6: an answer racing the timeout sweep ends ONE way: answered and relayed
// (the sweep times nothing out), or timed out (the answer is refused 409).
func TestAnAnswerRacingTheTimeoutEndsOneWay(t *testing.T) {
	f := newRouteFixture(t)
	for round := 0; round < 6; round++ {
		_, m := f.pluginMessage(t, "example-inbox", "r1-race-"+string(rune('a'+round)), "someone@example.test", false)
		id := f.route(t, m)
		dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 second' WHERE route_id = '`+id+`'`)
		var wg sync.WaitGroup
		var code int
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; code, _ = f.reply(t, f.token, id, "Just in time?") }()
		go func() { defer wg.Done(); <-start; _, _ = f.srv.RouteTimeoutPass(context.Background()) }()
		close(start)
		wg.Wait()
		r := f.routeState(t, id)
		switch {
		case code == http.StatusOK && (r.State == plugins.RouteFallback || r.State == plugins.RouteRelayed) && r.DraftID != nil:
		case code == http.StatusConflict && r.State == plugins.RouteTimedOut && r.DraftID == nil:
		default:
			t.Fatalf("round %d: reply %d, route %s draft %v — not one coherent ending", round, code, r.State, r.DraftID)
		}
	}
}

// countingGateway counts the turns it is asked for.
type countingGateway struct {
	stubGateway
	n atomic.Int32
}

func (g *countingGateway) Chat(ctx context.Context, a agents.Agent, s, m string, f func(string)) (string, error) {
	g.n.Add(1)
	return "ok", nil
}

func (g *countingGateway) ChatWithTools(ctx context.Context, a agents.Agent, s, m, x string, d []agents.ToolDef, td agents.ToolDispatch, e agents.StreamEmit) (string, error) {
	g.n.Add(1)
	return "ok", nil
}

// 🟢: a hand-back turn takes the chief's session claim; while another turn
// holds it, the hand-back is a note and starts nothing. Unheld (the control),
// it is a turn, and the claim is released after it.
func TestAHandBackTurnTakesTheChiefsClaim(t *testing.T) {
	f := newRouteFixture(t)
	gw := &countingGateway{}
	f.srv.ext.Gateway, f.srv.ext.Provisioner = gw, newFakeTerminal()
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agents SET status = 'running' WHERE id = $1`, f.chief.ID)
	chief, err := f.srv.ext.Agents.Get(f.asOwner, f.chief.ID)
	if err != nil || chief.Status != agents.StatusRunning {
		t.Fatalf("chief: %+v %v", chief, err)
	}
	f.chief = chief
	p, m := f.pluginMessage(t, "example-inbox", "r1-claim", "someone@example.test", false)
	sess := f.chiefSession(t, p)

	if won, err := f.srv.ext.Agents.ClaimKickoff(f.asOwner, f.chief.ID, "another-turn", time.Minute); err != nil || !won {
		t.Fatalf("hold the claim: %v %v", won, err)
	}
	id := f.route(t, m)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	msgs, _ := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if gw.n.Load() != 0 || len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Route "+id) {
		t.Fatalf("with the claim held: %d turns, messages %+v; want no turn and one note", gw.n.Load(), msgs)
	}

	// The control: the claim released, the next hand-back is a turn.
	if err := f.srv.ext.Agents.ReleaseKickoffClaim(f.asOwner, f.chief.ID, "another-turn"); err != nil {
		t.Fatal(err)
	}
	_, m2 := f.pluginMessage(t, "example-inbox", "r1-claim-2", "someone@example.test", false)
	id2 := f.route(t, m2)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id2+`'`)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gw.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if gw.n.Load() != 1 {
		t.Fatalf("with the claim free: %d turns, want 1", gw.n.Load())
	}
	var owner string
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(authz.System(context.Background()), `SELECT kickoff_claim_owner FROM agents WHERE id = $1`, f.chief.ID).Scan(&owner)
		if owner == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if owner != "" {
		t.Fatalf("the hand-back's claim was not released: %q", owner)
	}
}
