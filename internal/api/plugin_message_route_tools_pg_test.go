package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// plugin_messages_list / plugin_message_route (plugin_message_route_tools.go),
// through the real server against a real Postgres: the owner, from one of their
// own MIRRORED Claude Code sessions (POST /mirror/mcp), lists their routable
// inbound plugin messages and routes one — to the calling session when no
// subject is named — through the chief's own route (routeMessage): the same
// session_routes row, the same send-queue injection, the same route_reply
// answer and relay, and the same hand-back to the chief.
//
// The fixture is the session-routing one (routeFixture) armed for the mirror
// door: host "tower" lists routeSID (window @4, title "work:1") and
// otherRouteSID (window @5, title "work:2"); a second member has a mirror token
// for a host of the same name, their own chief and their own plugin.

type msgRouteFixture struct {
	*routeFixture
	tok           string // the owner's mirror token (host "tower")
	strangers     string // another member's mirror token (host "tower" too)
	stranger      int64
	strangerChief agents.Agent
}

func newMsgRouteFixture(t *testing.T) *msgRouteFixture {
	t.Helper()
	f := &msgRouteFixture{routeFixture: newRouteFixture(t)}
	f.tok = f.withRouteReply(t)
	f.stranger = f.newMember(t)
	f.strangers = mirrorTok('s')
	f.srv.ext.MirrorTokens = fakeMirrorTokens{
		f.tok:       mirror.Token{ID: 9, OwnerID: f.owner, Host: "tower"},
		f.strangers: mirror.Token{ID: 10, OwnerID: f.stranger, Host: "tower"},
	}
	f.strangerChief = f.newAgent(t, f.stranger, "rtschief-"+strconv.FormatInt(f.stranger, 10), "chief", "")
	return f
}

// call calls tool on /mirror/mcp as session sid of the host tok names.
func (f *msgRouteFixture) mcpCall(t *testing.T, tok, sid, tool string, args map[string]any) (string, bool) {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range mirrorAs(tok, sid) {
		h[k] = v
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(tool, args)), h)
	var r mcpReply
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("%s as %s: %d %s", tool, sid, w.Code, w.Body)
	}
	if r.Error != nil {
		return "rpc-error " + strconv.Itoa(r.Error.Code) + " " + r.Error.Message, true
	}
	return toolText(t, &r)
}

// routeAs is plugin_message_route as session sid of the owner's host.
func (f *msgRouteFixture) routeAs(t *testing.T, sid string, id int64, subject string) (map[string]any, string, bool) {
	t.Helper()
	args := map[string]any{"message_id": id}
	if subject != "" {
		args["subject"] = subject
	}
	text, isErr := f.mcpCall(t, f.tok, sid, pluginMessageRouteTool, args)
	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	return out, text, isErr
}

// listAs is plugin_messages_list's messages as session sid of tok's host.
func (f *msgRouteFixture) listAs(t *testing.T, tok, sid string) []map[string]any {
	t.Helper()
	text, isErr := f.mcpCall(t, tok, sid, pluginMessagesListTool, map[string]any{})
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil {
		t.Fatalf("plugin_messages_list: %s", text)
	}
	return out.Messages
}

// routeRows is the number of session_routes rows of owner.
func (f *msgRouteFixture) routeRows(t *testing.T, owner int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// routedAudit is route id's session_route.routed audit row: its user, via and
// routed_via detail.
func (f *msgRouteFixture) routedAudit(t *testing.T, id string) (int64, string, string, string) {
	t.Helper()
	var uid int64
	var via, routedVia, target string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT user_id, via, coalesce(detail->>'routed_via', ''), detail->>'target'
		FROM audit_log WHERE object = $1 AND action = 'session_route.routed'`, plugins.RouteObject(id)).Scan(&uid, &via, &routedVia, &target); err != nil {
		t.Fatalf("the routed audit row of %s: %v", id, err)
	}
	return uid, via, routedVia, target
}

func byMessageID(ms []map[string]any, id int64) map[string]any {
	for _, m := range ms {
		if int64(m["message_id"].(float64)) == id {
			return m
		}
	}
	return nil
}

// TestPluginMessagesListIsTheOwnersRoutableMessages: the listing is the
// owner's DELIVERED inbound messages, newest first, with the sender's
// UNVERIFIED mark when the plugin did not verify it, a bounded preview, and
// each message's route state — none, pending, relayed, handed_back — all four
// seen. A queued (undelivered) message and another member's message are not
// listed; the other member's own session lists theirs (positive control).
func TestPluginMessagesListIsTheOwnersRoutableMessages(t *testing.T) {
	f := newMsgRouteFixture(t)
	p, plain := f.pluginMessage(t, "example-inbox", "pml-plain", "Ada <ada@example.invalid>", false)
	_, pending := f.pluginMessage(t, "example-inbox", "pml-pending", "Bob <bob@example.invalid>", true)
	_, relayed := f.pluginMessage(t, "example-inbox", "pml-relayed", "Cy <cy@example.invalid>", true)
	_, back := f.pluginMessage(t, "example-inbox", "pml-back", "Di <di@example.invalid>", true)
	long := strings.Repeat("x", 300)
	lm, _, err := f.plugins.Submit(f.asOwner, p.ID, plugins.MessageIn{ExternalID: "pml-long", Sender: "Ed <ed@example.invalid>", Subject: "long one", Body: long})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(f.asOwner, lm.ID, f.chief.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queued := f.deliveredMessage(t, f.owner, 0, "pml-queued")
	theirs := f.deliveredMessage(t, f.stranger, f.strangerChief.ID, "pml-theirs")

	pendingRoute := f.route(t, pending)
	relayedRoute := f.route(t, relayed)
	if text, isErr := f.routeReplyCall(t, f.tok, routeSID, map[string]any{"route_id": relayedRoute, "body": "Answered."}); isErr {
		t.Fatal(text)
	}
	backRoute := f.route(t, back)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+backRoute+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("the sweep timed out %d (%v)", n, err)
	}

	ms := f.listAs(t, f.tok, otherRouteSID)
	var ids []int64
	for _, m := range ms {
		ids = append(ids, int64(m["message_id"].(float64)))
	}
	want := []int64{lm.ID, back.ID, relayed.ID, pending.ID, plain.ID}
	if sprintIDs(ids) != sprintIDs(want) {
		t.Fatalf("listed %v, want exactly %v newest first (queued %d and another member's %d left out)", ids, want, queued.ID, theirs.ID)
	}
	for id, st := range map[int64][2]string{plain.ID: {"none", ""}, pending.ID: {"pending", pendingRoute}, relayed.ID: {"relayed", relayedRoute}, back.ID: {"handed_back", backRoute}} {
		r := byMessageID(ms, id)["route"].(map[string]any)
		if r["state"] != st[0] || (st[1] != "" && r["route_id"] != st[1]) || (st[1] == "" && r["route_id"] != nil) {
			t.Errorf("message %d's route: %v, want %s %s", id, r, st[0], st[1])
		}
	}
	if r := byMessageID(ms, pending.ID)["route"].(map[string]any); r["target"] != "mirror:"+routeSID || r["route_state"] != "routed" || r["timeout_at"] == nil {
		t.Errorf("the pending route: %v", r)
	}
	if got := byMessageID(ms, plain.ID); got["sender"] != "Ada <ada@example.invalid> "+plugins.UnverifiedSenderMark || got["sender_verified"] != false ||
		got["subject"] != "hello" || got["body_preview"] != "please do a thing" || got["plugin"] != "Example example-inbox" {
		t.Errorf("the unverified message: %v", got)
	}
	if got := byMessageID(ms, pending.ID); got["sender"] != "Bob <bob@example.invalid>" || got["sender_verified"] != true {
		t.Errorf("control, a verified sender: %v", got)
	}
	if got := byMessageID(ms, lm.ID)["body_preview"]; got != strings.Repeat("x", pluginMessagePreviewRunes)+"…" {
		t.Errorf("a 300-character body's preview is %d bytes: %v", len(got.(string)), got)
	}
	// Positive control: the other member's session lists their own message.
	if theirsListed := f.listAs(t, f.strangers, routeSID); len(theirsListed) != 1 || byMessageID(theirsListed, theirs.ID) == nil {
		t.Fatalf("another member's session lists %v, want only their message %d", theirsListed, theirs.ID)
	}
}

func sprintIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, " ")
}

// TestPluginMessageRouteWithNoSubjectRoutesTheCallingSession is the feature:
// with no subject the message goes to THE CALLING session — a session_routes
// row whose target is that session and whose agent is the chief the message was
// delivered to, the stored message queued into that session's window naming its
// route_id, and an audit row that is the owner's, via the mirror host, with
// routed_via mcp from tower/<session>. Then that session is offered route_reply
// and answers, and the answer is relayed exactly as for the chief's route: a
// pending reply draft filed as the chief's. CONTROL: the chief's own
// route_to_session's audit row carries no routed_via.
func TestPluginMessageRouteWithNoSubjectRoutesTheCallingSession(t *testing.T) {
	f := newMsgRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-self")
	out, text, isErr := f.routeAs(t, routeSID, m.ID, "")
	if isErr || out["this_session"] != true || out["subject"] != "mirror:"+routeSID || out["state"] != "routed" ||
		out["routed_via"] != "mcp from tower/"+routeSID {
		t.Fatalf("plugin_message_route with no subject: %s", text)
	}
	id, _ := out["route_id"].(string)
	r, err := f.plugins.Route(f.asOwner, id)
	if err != nil || r.TargetKind != "mirror" || r.TargetKey != routeSID || r.AgentID == nil || *r.AgentID != f.chief.ID ||
		r.MessageID != m.ID || r.State != plugins.RouteRouted {
		t.Fatalf("the route row: %+v %v", r, err)
	}
	items := f.queued(t)
	if len(items) != 1 || r.SendQueueID == nil || *r.SendQueueID != items[0].ID || items[0].RouteID != id {
		t.Fatalf("queued %+v for route %+v", items, r)
	}
	for _, want := range []string{"Routed to you by " + routedByOwner + ": route_id " + id, "call the route_reply tool",
		"UNTRUSTED external message", "|   body: please do a thing"} {
		if !strings.Contains(items[0].Body, want) {
			t.Errorf("the queued block lacks %q:\n%s", want, items[0].Body)
		}
	}
	if uid, via, routedVia, target := f.routedAudit(t, id); uid != f.owner || via != string(authz.ViaMirrorHost) ||
		routedVia != "mcp from tower/"+routeSID || target != "mirror:"+routeSID {
		t.Fatalf("the routed audit row: user %d via %s routed_via %q target %s", uid, via, routedVia, target)
	}

	// The same path onward: the routed session answers with route_reply.
	if names := f.mirrorToolNames(t, f.tok); !strings.Contains(" "+strings.Join(names, " ")+" ", " route_reply ") {
		t.Fatalf("the routed session is not offered route_reply: %v", names)
	}
	text, isErr = f.routeReplyCall(t, f.tok, routeSID, map[string]any{"route_id": id, "body": "On it."})
	var ans struct {
		State       string `json:"state"`
		DraftID     int64  `json:"draft_id"`
		DraftStatus string `json:"draft_status"`
	}
	if err := json.Unmarshal([]byte(text), &ans); isErr || err != nil || ans.State != "fallback" || ans.DraftStatus != "pending" || ans.DraftID == 0 {
		t.Fatalf("route_reply on the owner's route: %s", text)
	}
	d, err := f.plugins.Draft(f.asOwner, ans.DraftID)
	if err != nil || d.AgentID == nil || *d.AgentID != f.chief.ID || d.Body != "On it." || d.MessageID != m.ID {
		t.Fatalf("the relayed draft: %+v %v", d, err)
	}

	// CONTROL: the chief's route of another message has no routed_via.
	cid := f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-chief"))
	if _, via, routedVia, _ := f.routedAudit(t, cid); routedVia != "" || via == string(authz.ViaMirrorHost) {
		t.Fatalf("the chief's routed audit row: via %s routed_via %q", via, routedVia)
	}
}

// TestPluginMessageRouteResolvesASubject: from otherRouteSID, a subject naming
// routeSID — in the chief's form, by a session id prefix, or by its window
// title — routes to routeSID (not to the caller); each lands in routeSID's
// window queue.
func TestPluginMessageRouteResolvesASubject(t *testing.T) {
	f := newMsgRouteFixture(t)
	for i, sub := range []string{"mirror:" + routeSID, routeSID[:8], "WORK:1"} {
		m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-sub-"+strconv.Itoa(i))
		out, text, isErr := f.routeAs(t, otherRouteSID, m.ID, sub)
		if isErr || out["subject"] != "mirror:"+routeSID || out["this_session"] != false || out["session"] != "work:1 @ tower" {
			t.Fatalf("subject %q: %s", sub, text)
		}
		r, _ := f.plugins.Route(f.asOwner, out["route_id"].(string))
		if r.TargetKey != routeSID {
			t.Fatalf("subject %q routed to %s", sub, r.TargetKey)
		}
		if n := len(f.queued(t)); n != i+1 {
			t.Fatalf("subject %q: %d rows queued in routeSID's window, want %d", sub, n, i+1)
		}
	}
	// The caller naming itself is a route to itself.
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-sub-self")
	if out, text, _ := f.routeAs(t, otherRouteSID, m.ID, otherRouteSID[:6]); out["subject"] != "mirror:"+otherRouteSID || out["this_session"] != true {
		t.Fatalf("the caller naming itself: %s", text)
	}
}

// TestPluginMessageRouteRefusesAnUnknownOrAmbiguousSubject: a subject matching
// both sessions is ambiguous and one matching none is not found — each listing
// the candidates, flagged an error — and none writes a route or queues a row.
func TestPluginMessageRouteRefusesAnUnknownOrAmbiguousSubject(t *testing.T) {
	f := newMsgRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-bad-subject")
	both := []string{"mirror:" + otherRouteSID, "mirror:" + routeSID}
	for _, c := range []struct {
		subject, refused string
		cands            []string
	}{
		{"work", RefusedSubjectAmbiguous, both},
		{"tower", RefusedSubjectAmbiguous, both},
		{"nowhere", RefusedSubjectNotFound, both},
		{"mirror:5e55a1d0-0000-4000-8000-0000000dead0", RefusedSubjectNotFound, both},
		{"5e5", RefusedSubjectNotFound, both}, // shorter than a usable prefix
	} {
		_, text, isErr := f.routeAs(t, routeSID, m.ID, c.subject)
		var res struct {
			Error, Refused string
			Candidates     []routeCandidate
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil || !isErr || res.Refused != c.refused || res.Error == "" {
			t.Errorf("subject %q: %s (error %v); want refused=%s", c.subject, text, isErr, c.refused)
			continue
		}
		var got []string
		for _, cd := range res.Candidates {
			got = append(got, cd.Subject)
		}
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(c.cands, " ") {
			t.Errorf("subject %q: candidates %v, want %v", c.subject, got, c.cands)
		}
	}
	if n := f.routeRows(t, f.owner); n != 0 || len(f.queued(t)) != 0 {
		t.Fatalf("a refused subject wrote %d route(s) and queued %d", n, len(f.queued(t)))
	}
}

// TestPluginMessageRouteRefusesAMessageWithAWaitingRoute: a message the chief
// routed to routeSID cannot be routed again while that route waits — the
// refusal names the route and its target — and once the route timed out back
// to the chief it can be. A message not yet delivered to the chief is refused
// as such.
func TestPluginMessageRouteRefusesAMessageWithAWaitingRoute(t *testing.T) {
	f := newMsgRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-twice")
	first := f.route(t, m)
	out, text, isErr := f.routeAs(t, otherRouteSID, m.ID, "")
	if !isErr || out["refused"] != RefusedAlreadyRouted || !strings.Contains(text, first) || !strings.Contains(text, "mirror:"+routeSID) {
		t.Fatalf("a second route of a waiting message: %s", text)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+first+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("the sweep timed out %d (%v)", n, err)
	}
	if out, text, isErr := f.routeAs(t, otherRouteSID, m.ID, ""); isErr || out["state"] != "routed" {
		t.Fatalf("control, after the hand-back: %s", text)
	}

	queued := f.deliveredMessage(t, f.owner, 0, "pmr-queued")
	if out, text, isErr := f.routeAs(t, routeSID, queued.ID, ""); !isErr || out["refused"] != RefusedNotDelivered {
		t.Fatalf("an undelivered message: %s", text)
	}
}

// otherOwnerPlugins is the real plugin store whose Message reads every owner's
// rows (as a store without row-level security would), so the tool's own owner
// check is what refuses another member's message.
type otherOwnerPlugins struct{ plugins.Store }

func (o otherOwnerPlugins) Message(ctx context.Context, id int64) (plugins.Message, error) {
	return o.Store.Message(authz.System(ctx), id)
}

// TestPluginMessageRouteRefusesAnotherOwnersMessage: another member's message
// reads exactly as a missing id — through row-level security, AND through the
// tool's own owner check when the store does not scope (otherOwnerPlugins) — and
// another member's session cannot route the owner's. Nothing is routed.
// CONTROL: the owner routes their own.
func TestPluginMessageRouteRefusesAnotherOwnersMessage(t *testing.T) {
	f := newMsgRouteFixture(t)
	mine := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-mine")
	theirs := f.deliveredMessage(t, f.stranger, f.strangerChief.ID, "pmr-theirs")
	missing := int64(9_000_000_321)
	want := func(id int64) string {
		return routeRefusal(RefusedMessageNotFound, "no such plugin message: "+strconv.FormatInt(id, 10))
	}
	for _, unscoped := range []bool{false, true} {
		if unscoped {
			f.srv.ext.Plugins = otherOwnerPlugins{f.plugins}
		}
		for _, id := range []int64{theirs.ID, missing} {
			if _, text, isErr := f.routeAs(t, routeSID, id, ""); !isErr || text != want(id) {
				t.Errorf("unscoped=%v: routing message %d: %s; want %s", unscoped, id, text, want(id))
			}
		}
	}
	f.srv.ext.Plugins = f.plugins
	text, isErr := f.mcpCall(t, f.strangers, routeSID, pluginMessageRouteTool, map[string]any{"message_id": mine.ID})
	if !isErr || text != want(mine.ID) {
		t.Errorf("another member's session routing the owner's message: %s", text)
	}
	if n := f.routeRows(t, f.owner) + f.routeRows(t, f.stranger); n != 0 {
		t.Fatalf("%d routes written", n)
	}
	if _, text, isErr := f.routeAs(t, routeSID, mine.ID, ""); isErr {
		t.Fatalf("control, the owner's own message: %s", text)
	}
}

// TestOnlyMirroredSessionsGetThePluginMessageRouteTools: the mirror door offers
// both with routing on and neither with it off; no dispatched agent — the chief
// (which has route_to_session) and a worker — is offered either or can call it
// through the gateway dispatcher or POST /mcp; the handlers refuse a caller
// that is not a mirrored session however it got there; a session the host does
// not list runs nothing. Nothing is routed.
func TestOnlyMirroredSessionsGetThePluginMessageRouteTools(t *testing.T) {
	f := newMsgRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "pmr-callers")
	names := " " + strings.Join(f.mirrorToolNames(t, f.tok), " ") + " "
	for _, tool := range []string{pluginMessagesListTool, pluginMessageRouteTool} {
		if !strings.Contains(names, " "+tool+" ") {
			t.Fatalf("the mirror door does not offer %s with routing on: %s", tool, names)
		}
	}
	args := `{"message_id":` + strconv.FormatInt(m.ID, 10) + `}`
	for _, a := range []struct {
		name  string
		tok   string
		agent agents.Agent
	}{{"chief", f.chiefTok, f.chief}, {"worker", f.workerTok, f.worker}} {
		for _, tool := range []string{pluginMessagesListTool, pluginMessageRouteTool} {
			got := f.surfaces(t, a.agent, a.tok, tool, args)
			if got["gateway"] != toolErr("unknown tool %q", tool) {
				t.Errorf("%s calling %s through the gateway dispatcher: %s", a.name, tool, got["gateway"])
			}
			if !strings.HasPrefix(got["mcp"], "rpc-error -32602 unknown tool") {
				t.Errorf("%s calling %s over POST /mcp: %s", a.name, tool, got["mcp"])
			}
			for _, d := range f.srv.AgentToolDefsFor(f.asOwner, a.agent) {
				if d.Name == tool {
					t.Errorf("%s is offered %s as a gateway tool", a.name, tool)
				}
			}
		}
	}
	for what, c := range map[string]toolCaller{
		"an agent caller":            callerOfAgent(f.chief),
		"a system caller naming one": {asker: attention.Asker{System: true, MirrorHost: "tower", MirrorSession: routeSID}},
	} {
		if got := f.srv.toolPluginMessageRoute(f.asOwner, c, args); got != toolErr(pluginMessageRouteMirrorOnly, pluginMessageRouteTool) {
			t.Errorf("plugin_message_route's handler, %s: %s", what, got)
		}
		if got := f.srv.toolPluginMessagesList(f.asOwner, c); got != toolErr(pluginMessageRouteMirrorOnly, pluginMessagesListTool) {
			t.Errorf("plugin_messages_list's handler, %s: %s", what, got)
		}
	}
	if text, isErr := f.mcpCall(t, f.tok, "5e55a1d0-0000-4000-8000-0000000dead0", pluginMessageRouteTool, map[string]any{"message_id": m.ID}); !isErr ||
		!strings.Contains(text, "lists no window running Claude session") {
		t.Errorf("a session the host does not list: %s", text)
	}
	if n := f.routeRows(t, f.owner); n != 0 {
		t.Fatalf("a refused caller wrote %d route(s)", n)
	}
	// Routing off: neither is offered, and a call by name is refused.
	f.srv.ext.Routing = nil
	names = " " + strings.Join(f.mirrorToolNames(t, f.tok), " ") + " "
	for _, tool := range []string{pluginMessagesListTool, pluginMessageRouteTool} {
		if strings.Contains(names, " "+tool+" ") {
			t.Errorf("%s is offered with routing off: %s", tool, names)
		}
	}
	if text, _ := f.mcpCall(t, f.tok, routeSID, pluginMessageRouteTool, map[string]any{"message_id": m.ID}); !strings.HasPrefix(text, "rpc-error -32602 unknown tool") {
		t.Errorf("a call with routing off: %s", text)
	}
}
