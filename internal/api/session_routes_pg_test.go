package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// SESSION ROUTING, through the real server against a real Postgres: the agent's
// route_to_session on the gateway and over /mcp, the injection into the
// window's send queue, the refusals, the session's answer through POST
// /api/routes/{id}/reply (the allowlist split, idempotency), and the deadline.

const routeSID = "5e55a1d0-0000-4000-8000-00000000a11c"

// routeMirror is one owner-agnostic mirrored host whose window @4 runs Claude
// Code with session routeSID. Its knobs flip what the routing gates read.
type routeMirror struct {
	mu           sync.Mutex
	interactive  bool
	statusReport bool
	// claudeSession is the conversation window @4 holds ("" = routeSID); label
	// its host-given display name.
	claudeSession string
	label         string
	pasted        int
	// otherSession, when set, is a second Claude Code window (@5) on the same
	// host running that conversation: one of the owner's sessions that is NOT
	// a route's target.
	otherSession string
}

func (m *routeMirror) Hosts(context.Context) []mirror.HostView {
	m.mu.Lock()
	defer m.mu.Unlock()
	sid := m.claudeSession
	if sid == "" {
		sid = routeSID
	}
	raw := []mirror.Window{{Session: "work", ID: "@4", Index: 1, Active: true, Claude: true,
		ClaudeSession: sid, Label: m.label, Status: mirror.StatusIdle, StatusSince: 1}}
	if m.otherSession != "" {
		raw = append(raw, mirror.Window{Session: "work", ID: "@5", Index: 2, Claude: true,
			ClaudeSession: m.otherSession, Status: mirror.StatusIdle, StatusSince: 1})
	}
	ws := mirror.NormalizeWindows(raw, 0)
	h := mirror.HostView{Host: "tower", Windows: ws, Online: true, Rendered: true, Interactive: m.interactive,
		PanePaste: m.interactive, StatusReport: m.statusReport}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return []mirror.HostView{h}
}
func (m *routeMirror) AgentState(context.Context, string, string, string) mirror.AgentState {
	return mirror.AgentState{Status: mirror.StatusIdle}
}
func (m *routeMirror) ServeConnect(http.ResponseWriter, *http.Request) {}
func (m *routeMirror) HostRanks(context.Context) map[string]int        { return nil }
func (m *routeMirror) AttachWindow(context.Context, string, string, string, bool, io.Reader, io.Writer, <-chan provision.TermSize) error {
	return nil
}
func (m *routeMirror) WatchWindow(context.Context, string, string, string, string, int64, func(mirror.Msg) error) error {
	return nil
}
func (m *routeMirror) PasteWindow(context.Context, string, string, string, string, mirror.PasteTarget, ...mirror.PasteOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pasted++
	return nil
}

// set changes a knob under the lock.
func (m *routeMirror) set(f func(*routeMirror)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

// pastes is how many pastes reached the host.
func (m *routeMirror) pastes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pasted
}
func (m *routeMirror) ScrollWindow(context.Context, string, string, string, int) error { return nil }

type routeFixture struct {
	*gatedFixture
	mirror  *routeMirror
	queue   *sendqueue.PGStore
	routing *Routing
	token   string // the owner's personal API token
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	f := &routeFixture{gatedFixture: &gatedFixture{}, mirror: &routeMirror{interactive: true, statusReport: true}, routing: &Routing{}}
	g := f.gatedFixture
	g.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		g.plugins = plugins.NewPG(pool)
		f.queue = sendqueue.NewPG(pool)
		ext.Plugins, ext.Routes, ext.Routing = g.plugins, g.plugins, f.routing
		ext.Mirror, ext.SendQueue = f.mirror, f.queue
		ext.APITokens = apitokens.NewPG(pool)
	})
	g.owner = g.newMember(t)
	g.asOwner = g.as(g.owner)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	g.chiefTok, g.workerTok = "route-agent-token-"+suffix, "route-worker-token-"+suffix
	g.agent = g.newAgent(t, g.owner, "rtchief-"+suffix, "agent", g.chiefTok)
	g.worker = g.newAgent(t, g.owner, "rtworker-"+suffix, "", g.workerTok)
	tok, _, err := apitokens.NewPG(g.pool).Create(g.asOwner, "route-reply", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM send_queue WHERE owner_id = $1`, `DELETE FROM api_tokens WHERE user_id = $1`} {
			_, _ = g.pool.Exec(sys, q, g.owner)
		}
	})
	return f
}

// routeArgs is route_to_session's arguments for message m and subject sub.
func routeArgs(m plugins.Message, sub string) string {
	return `{"message_id":` + strconv.FormatInt(m.ID, 10) + `,"subject":"` + sub + `"}`
}

// queued is the owner's send-queue rows for the routed window.
func (f *routeFixture) queued(t *testing.T) []sendqueue.Item {
	t.Helper()
	items, err := f.queue.List(f.asOwner, sendqueue.Target{Host: "tower", Session: "work", Window: "@4"})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// pluginMessage stores one message through the owner's plugin named name
// (created on first use) from sender, marked verified or not, and settles it
// as delivered to the agent.
func (f *routeFixture) pluginMessage(t *testing.T, name, external, sender string, verified bool) (plugins.Plugin, plugins.Message) {
	t.Helper()
	ctx := f.as(f.owner)
	ps, err := f.plugins.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	for _, x := range ps {
		if x.Name == name {
			p = x
		}
	}
	if p.ID == 0 {
		if _, p, err = f.plugins.Create(ctx, name, "Example "+name, name+"-data"); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := f.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: external, Sender: sender, Subject: "hello",
		Body: "please do a thing", SenderVerified: verified})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(ctx, m.ID, f.agent.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(ctx, m.ID, f.agent.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	return p, m
}

// allow sets the owner's relay allowlist from entries ("<plugin>:<sender>").
func (f *routeFixture) allow(t *testing.T, entries ...string) {
	t.Helper()
	raw := ""
	for _, e := range entries {
		raw += strconv.FormatInt(f.owner, 10) + ":" + e + ","
	}
	l, err := plugins.ParseRelayAllowlist(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.routing.Allow = l
}

func (f *routeFixture) reply(t *testing.T, token, routeID, body string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"body": body})
	w := f.call(t, "POST", "/api/routes/"+routeID+"/reply", strings.NewReader(string(b)),
		map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestRouteToSessionInjectsTheStoredMessageOnBothSurfaces(t *testing.T) {
	f := newRouteFixture(t)
	for i, surface := range []string{"gateway", "mcp"} {
		m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-"+surface)
		var text string
		if surface == "gateway" {
			text = f.srv.AgentToolDispatch(f.asOwner, f.agent)("route_to_session", routeArgs(m, "mirror:"+routeSID))
		} else {
			r, _ := f.mcp(t, f.chiefTok, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"route_to_session","arguments":`+
				routeArgs(m, "mirror:"+routeSID)+`}}`, nil)
			if r == nil || r.Error != nil {
				t.Fatalf("mcp: %+v", r)
			}
			text, _ = toolText(t, r)
		}
		var res struct {
			RouteID string `json:"route_id"`
			State   string `json:"state"`
			Subject string `json:"subject"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil || !plugins.ValidRouteID(res.RouteID) || res.State != "routed" ||
			res.Subject != "mirror:"+routeSID {
			t.Fatalf("%s: %s", surface, text)
		}
		items := f.queued(t)
		if len(items) != i+1 {
			t.Fatalf("%s: %d queued rows, want %d", surface, len(items), i+1)
		}
		body := items[i].Body
		for _, want := range []string{"UNTRUSTED external message", "route_id " + res.RouteID, "muster route reply " + res.RouteID,
			"|   body: please do a thing", "<<<routed-message untrusted nonce="} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the injected block lacks %q:\n%s", surface, want, body)
			}
		}
		if _, err := mirror.SanitizePaste(body); err != nil {
			t.Errorf("%s: the injected block cannot be pasted: %v", surface, err)
		}
		r, err := f.plugins.Route(f.asOwner, res.RouteID)
		if err != nil || r.SendQueueID == nil || *r.SendQueueID != items[i].ID {
			t.Fatalf("%s: route %+v (%v) does not name its queued row %d", surface, r, err, items[i].ID)
		}
	}
}

// TestRouteToSessionRefusesWithAReason: each refusal is structured (error +
// refused code), flagged an error on /mcp, and writes neither a route nor a
// queued row.
func TestRouteToSessionRefusesWithAReason(t *testing.T) {
	f := newRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-refusals")
	notMine := f.deliveringMessage(t, f.owner, 0, "route-queued-only")
	for _, c := range []struct {
		name, args, refused string
		flip                func()
	}{
		{"a window no host shows", routeArgs(m, "mirror:nosuchsession"), RefusedWindowGone, nil},
		{"a dispatched agent", routeArgs(m, "agent:"+strconv.FormatInt(f.worker.ID, 10)), RefusedUnsupported, nil},
		{"a message not delivered to this agent", routeArgs(notMine, "mirror:"+routeSID), RefusedMessageNotFound, nil},
		{"a bad subject", routeArgs(m, "pod:1"), RefusedBadArguments, nil},
		{"a read-only host", routeArgs(m, "mirror:"+routeSID), RefusedHostNotInteractve, func() { f.mirror.interactive = false }},
		{"a host that does not report status", routeArgs(m, "mirror:"+routeSID), RefusedQueueUnavailable, func() {
			f.mirror.interactive, f.mirror.statusReport = true, false
		}},
	} {
		if c.flip != nil {
			f.mirror.mu.Lock()
			c.flip()
			f.mirror.mu.Unlock()
		}
		for surface, text := range f.surfaces(t, f.agent, f.chiefTok, "route_to_session", c.args) {
			var res struct{ Error, Refused string }
			if err := json.Unmarshal([]byte(text), &res); err != nil || res.Refused != c.refused || res.Error == "" || !isToolErr(text) {
				t.Errorf("%s, %s: %s; want refused=%s", c.name, surface, text, c.refused)
			}
		}
	}
	if n := len(f.queued(t)); n != 0 {
		t.Fatalf("a refused route queued %d message(s)", n)
	}
	var routes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1`, f.owner).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("a refused route wrote %d route row(s) (%v)", routes, err)
	}
	// A second open route for the same message is refused too.
	f.mirror.interactive, f.mirror.statusReport = true, true
	gw := f.srv.AgentToolDispatch(f.asOwner, f.agent)
	if text := gw("route_to_session", routeArgs(m, "mirror:"+routeSID)); !strings.Contains(text, `"state":"routed"`) {
		t.Fatal(text)
	}
	if text := gw("route_to_session", routeArgs(m, "mirror:"+routeSID)); !strings.Contains(text, RefusedAlreadyRouted) {
		t.Fatalf("a second route: %s", text)
	}
}

// TestRouteToSessionIsOfferedToEveryAgentButStillNeedsItsOwnMessage: the tool
// was the labelled agent's alone (the gateway refused by rank, /mcp called it
// unknown). muster has no privileged agent, so a worker is OFFERED it — and is
// still refused this particular call, because the message was delivered to
// ANOTHER agent and DeliveredTo is what the handler checks. That is the
// distinction the removal rests on: the gate is the message's own delivery, not
// the caller's rank, so nothing a worker may now reach widens what it can route.
func TestRouteToSessionIsOfferedToEveryAgentButStillNeedsItsOwnMessage(t *testing.T) {
	f := newRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-worker")
	got := f.surfaces(t, f.worker, f.workerTok, "route_to_session", routeArgs(m, "mirror:"+routeSID))
	if strings.Contains(got["gateway"], "agent agent only") {
		t.Errorf("gateway refused by rank: %s", got["gateway"])
	}
	if strings.HasPrefix(got["mcp"], "rpc-error -32602 unknown tool") {
		t.Errorf("mcp: the tool was not offered to a worker: %s", got["mcp"])
	}
	// Refused on the message, not on the rank — and nothing is routed.
	for surface, text := range got {
		if !strings.Contains(text, "is not a plugin message delivered to you") {
			t.Errorf("%s: want the delivered-to-you refusal, got %s", surface, text)
		}
	}
	if n := len(f.queued(t)); n != 0 {
		t.Fatalf("a worker's call queued %d message(s)", n)
	}
}

// route makes one route through the agent's tool and returns its id.
func (f *routeFixture) route(t *testing.T, m plugins.Message) string {
	t.Helper()
	text := f.srv.AgentToolDispatch(f.asOwner, f.agent)("route_to_session", routeArgs(m, "mirror:"+routeSID))
	var res struct {
		RouteID string `json:"route_id"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || res.RouteID == "" {
		t.Fatalf("route: %s", text)
	}
	return res.RouteID
}

// TestTheAnswerIsRelayedPerTheAllowlist: a VERIFIED sender allowlisted for the
// message's own plugin gets an APPROVED draft the plugin's claim takes; anyone
// else a PENDING one. The same answer again is the stored result; a different
// one is refused; another user's credential cannot answer.
func TestTheAnswerIsRelayedPerTheAllowlist(t *testing.T) {
	f := newRouteFixture(t)
	f.allow(t, "example-inbox:someone@example.test")
	p, allowed := f.pluginMessage(t, "example-inbox", "route-allowed", "someone@example.test", true)
	id := f.route(t, allowed)
	code, out := f.reply(t, f.token, id, "Noon works for us.")
	if code != 200 || out["state"] != "relayed" || out["draft_status"] != "approved" {
		t.Fatalf("allowlisted answer: %d %v", code, out)
	}
	draftID := int64(out["draft_id"].(float64))
	c, ok, err := f.plugins.ClaimDraft(f.as(f.owner), p.ID)
	if err != nil || !ok || c.DraftID != draftID || c.Body != "Noon works for us." || c.MessageExternalID != "route-allowed" {
		t.Fatalf("the plugin's claim: %+v %v %v", c, ok, err)
	}
	// Idempotent: the same answer again is the stored result, and files nothing.
	if code, again := f.reply(t, f.token, id, "Noon works for us."); code != 200 || int64(again["draft_id"].(float64)) != draftID {
		t.Fatalf("the same answer again: %d %v", code, again)
	}
	if code, _ := f.reply(t, f.token, id, "Actually, no."); code != http.StatusConflict {
		t.Fatalf("a different second answer: %d, want 409", code)
	}

	// Not allowlisted: the same path ends in a pending draft.
	f.allow(t, "example-inbox:someone-else@example.test")
	_, other := f.pluginMessage(t, "example-inbox", "route-fallback", "someone@example.test", true)
	id2 := f.route(t, other)
	code, out = f.reply(t, f.token, id2, "Let me check and get back to you.")
	if code != 200 || out["state"] != "fallback" || out["draft_status"] != "pending" {
		t.Fatalf("non-allowlisted answer: %d %v", code, out)
	}
	if _, ok, _ := f.plugins.ClaimDraft(f.as(f.owner), p.ID); ok {
		t.Fatal("a fallback draft was claimable before the owner approved it")
	}

	// Another user's credential: the route is not theirs, so it is not found.
	stranger := f.newMember(t)
	tok, _, err := apitokens.NewPG(f.pool).Create(f.as(stranger), "stranger", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	third := f.deliveredMessage(t, f.owner, f.agent.ID, "route-stranger")
	id3 := f.route(t, third)
	if code, _ := f.reply(t, tok, id3, "hijack"); code != http.StatusNotFound {
		t.Fatalf("a stranger's answer: %d, want 404", code)
	}
	if r, _ := f.plugins.Route(f.asOwner, id3); r.State != plugins.RouteRouted {
		t.Fatalf("a stranger moved the route to %s", r.State)
	}
}

// TestATimedOutRouteCancelsItsInjectionAndGoesBackToTheChief.
func TestATimedOutRouteCancelsItsInjectionAndGoesBackToTheChief(t *testing.T) {
	f := newRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-timeout")
	ps, _ := f.plugins.List(f.asOwner)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.agent.ID, f.agent.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetDeliverySession(f.asOwner, ps[0].ID, f.agent.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	id := f.route(t, m)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n != 0 {
		t.Fatalf("before the deadline: %d timed out (%v)", n, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	r, _ := f.plugins.Route(f.asOwner, id)
	if r.State != plugins.RouteTimedOut {
		t.Fatalf("state %s, want timed_out", r.State)
	}
	if items := f.queued(t); len(items) != 1 || items[0].State != sendqueue.Cancelled {
		t.Fatalf("the injection was not cancelled: %+v", items)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Route "+id) || !strings.Contains(msgs[0].Content, "back with you") {
		t.Fatalf("the agent was not handed the message back: %+v %v", msgs, err)
	}
	if code, _ := f.reply(t, f.token, id, "too late"); code != http.StatusConflict {
		t.Fatalf("an answer after the timeout: %d, want 409", code)
	}
	// The pass is idempotent: a second one times nothing else out.
	if n, _ := f.srv.RouteTimeoutPass(context.Background()); n != 0 {
		t.Fatalf("a second pass timed out %d more", n)
	}
}

// TestARouteTakesItsDeadlineFromRoutingTimeout: the deadline a route is created
// with is Routing.Timeout (MUSTER_ROUTE_TIMEOUT) when set — two values distinct
// from each other and from the 30m default — and the default when it is 0.
func TestARouteTakesItsDeadlineFromRoutingTimeout(t *testing.T) {
	f := newRouteFixture(t)
	for i, c := range []struct {
		set, want time.Duration
	}{{7 * time.Minute, 7 * time.Minute}, {3 * time.Hour, 3 * time.Hour}, {0, DefaultRouteTimeout}} {
		f.routing.Timeout = c.set
		id := f.route(t, f.deliveredMessage(t, f.owner, f.agent.ID, "route-deadline-"+strconv.Itoa(i)))
		r, err := f.plugins.Route(f.asOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.TimeoutAt.Sub(r.CreatedAt); got < c.want-5*time.Second || got > c.want+5*time.Second {
			t.Errorf("Routing.Timeout %v: the route's deadline is %v after it was made, want %v", c.set, got, c.want)
		}
	}
}

// TestRoutingOffOffersNothing: with no Routing configured the tool is not in
// the registry and the reply door is 404.
func TestRoutingOffOffersNothing(t *testing.T) {
	f := newRouteFixture(t)
	f.srv.ext.Routing = nil
	for _, d := range f.srv.AgentToolDefsFor(f.asOwner, f.agent) {
		if d.Name == "route_to_session" {
			t.Fatal("route_to_session is offered with routing off")
		}
	}
	if code, _ := f.reply(t, f.token, "rt_000000000000000000000000", "x"); code != http.StatusNotFound {
		t.Fatalf("the reply door with routing off: %d", code)
	}
}
