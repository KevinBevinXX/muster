package api

import (
	"context"
	"encoding/json"
	"net/http"
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

// route_reply — a routed session's answer over POST /mirror/mcp — through the
// real server against a real Postgres: the target session answers and the
// answer is relayed exactly as the CLI's would be; every other caller is
// refused; a route that is not waiting is refused, naming its state.

// otherRouteSID is a second Claude Code conversation on the routed host: one of
// the owner's own sessions, listed by the same host, that a route is NOT sent to.
const otherRouteSID = "0dd5e551-0000-4000-8000-0000000000b2"

// withRouteReply arms the mirrored sessions' MCP door for the route fixture:
// a mirror token of the owner's host "tower" (the host routeMirror lists),
// and that host's second window running otherRouteSID. It returns the token.
func (f *routeFixture) withRouteReply(t *testing.T) string {
	t.Helper()
	tok := mirrorTok('r')
	f.srv.ext.MirrorTokens = fakeMirrorTokens{tok: mirror.Token{ID: 9, OwnerID: f.owner, Host: "tower"}}
	f.mirror.set(func(m *routeMirror) { m.otherSession = otherRouteSID })
	return tok
}

// routeReplyCall calls route_reply on /mirror/mcp as Claude session sid of the
// host tok names. It returns the tool's text and whether it was flagged an
// error, or the JSON-RPC error as "rpc-error <code> <message>".
func (f *routeFixture) routeReplyCall(t *testing.T, tok, sid string, args any) (string, bool) {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range mirrorAs(tok, sid) {
		h[k] = v
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(routeReplyToolName, args)), h)
	if w.Code != http.StatusOK {
		t.Fatalf("/mirror/mcp: %d %s", w.Code, w.Body)
	}
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %s", w.Body)
	}
	if r.Error != nil {
		return "rpc-error " + strconv.Itoa(r.Error.Code) + " " + r.Error.Message, true
	}
	return toolText(t, &r)
}

// mirrorToolNames is tools/list on /mirror/mcp for tok.
func (f *routeFixture) mirrorToolNames(t *testing.T, tok string) []string {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range mirrorAs(tok, routeSID) {
		h[k] = v
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), h)
	var r mcpReply
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.Error != nil {
		t.Fatalf("tools/list: %d %s", w.Code, w.Body)
	}
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestRouteReplyFromTheTargetSessionIsTheCLIsAnswer: the session a message was
// routed to answers it with route_reply, and the answer takes the CLI's own
// path — a verified, allowlisted sender's answer is relayed as an APPROVED
// draft the plugin's next claim takes, with the body as written.
func TestRouteReplyFromTheTargetSessionIsTheCLIsAnswer(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)
	if names := f.mirrorToolNames(t, tok); !strings.Contains(" "+strings.Join(names, " ")+" ", " route_reply ") {
		t.Fatalf("route_reply is not offered to a mirrored session with routing on: %v", names)
	}
	f.allow(t, "example-inbox:someone@example.test")
	p, m := f.pluginMessage(t, "example-inbox", "route-reply-ok", "someone@example.test", true)
	id := f.route(t, m)

	text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "Noon works for us."})
	if isErr {
		t.Fatalf("the target session's route_reply: %s", text)
	}
	var out struct {
		RouteID     string `json:"route_id"`
		State       string `json:"state"`
		DraftID     int64  `json:"draft_id"`
		DraftStatus string `json:"draft_status"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.RouteID != id || out.State != "relayed" || out.DraftStatus != "approved" || out.DraftID == 0 {
		t.Fatalf("route_reply result: %s", text)
	}
	if r, _ := f.plugins.Route(f.asOwner, id); r.State != plugins.RouteRelayed || r.ReplyBody != "Noon works for us." {
		t.Fatalf("the route is %s with body %q", r.State, r.ReplyBody)
	}
	c, ok, err := f.plugins.ClaimDraft(f.as(f.owner), p.ID)
	if err != nil || !ok || c.DraftID != out.DraftID || c.Body != "Noon works for us." || c.MessageExternalID != "route-reply-ok" {
		t.Fatalf("the plugin's claim: %+v %v %v", c, ok, err)
	}
}

// TestRouteReplyIsRefusedToEveryCallerButTheTargetSession: another of the
// owner's sessions on the SAME host, holding the route id, is refused with the
// binding's own error; a dispatched agent and the agent are not offered the
// tool on either transport, and the handler refuses a non-mirrored caller even
// if one reached it. None of them moves the route, and the target can still
// answer afterwards.
func TestRouteReplyIsRefusedToEveryCallerButTheTargetSession(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)
	m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-reply-binding")
	id := f.route(t, m)
	args := map[string]any{"route_id": id, "body": "hijack"}

	// Another session the same host lists now.
	text, isErr := f.routeReplyCall(t, tok, otherRouteSID, args)
	if !isErr || text != toolErr("route %s was not routed to this session: only the session it was sent to may answer it", id) {
		t.Errorf("another session of the same host: %s (error %v)", text, isErr)
	}
	// A session no window shows: refused by the door's own binding first.
	if text, isErr := f.routeReplyCall(t, tok, "5e55a1d0-0000-4000-8000-0000000dead0", args); !isErr || !strings.Contains(text, "lists no window running Claude session") {
		t.Errorf("an unlisted session: %s", text)
	}
	// Dispatched agents, the agent included: not offered, on either transport.
	rawArgs, _ := json.Marshal(args)
	for _, a := range []struct {
		name  string
		tok   string
		agent agents.Agent
	}{{"worker", f.workerTok, f.worker}, {"agent", f.chiefTok, f.agent}} {
		got := f.surfaces(t, a.agent, a.tok, routeReplyToolName, string(rawArgs))
		if got["gateway"] != toolErr("unknown tool %q", routeReplyToolName) {
			t.Errorf("%s, gateway: %s", a.name, got["gateway"])
		}
		if !strings.HasPrefix(got["mcp"], "rpc-error -32602 unknown tool") {
			t.Errorf("%s, mcp: %s", a.name, got["mcp"])
		}
	}
	// The handler's own refusal of a caller that is not a mirrored session.
	if got := f.srv.toolRouteReply(f.asOwner, callerOfAgent(f.worker), string(rawArgs)); got != toolErr(routeReplyMirrorOnly) {
		t.Errorf("the handler, an agent caller: %s", got)
	}
	if got := f.srv.toolRouteReply(f.asOwner, toolCaller{asker: attention.Asker{System: true, MirrorHost: "tower", MirrorSession: routeSID}}, string(rawArgs)); got != toolErr(routeReplyMirrorOnly) {
		t.Errorf("the handler, a system caller naming the target: %s", got)
	}

	if r, _ := f.plugins.Route(f.asOwner, id); r.State != plugins.RouteRouted || r.ReplyBody != "" {
		t.Fatalf("a refused caller moved the route: %s %q", r.State, r.ReplyBody)
	}
	// The target still answers.
	if text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "Thanks, will do."}); isErr || !strings.Contains(text, `"route_id":"`+id+`"`) {
		t.Fatalf("the target after the refusals: %s", text)
	}
}

// TestRouteReplyIsRefusedOnARouteThatIsNotWaiting: once a route is answered,
// a second route_reply — the same body included — is refused naming the state;
// a route that timed out back to the agent refuses a late answer; an id that
// names no route of this owner is "no such route".
func TestRouteReplyIsRefusedOnARouteThatIsNotWaiting(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)

	answered := f.route(t, f.deliveredMessage(t, f.owner, f.agent.ID, "route-reply-answered"))
	if text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": answered, "body": "Done."}); isErr {
		t.Fatalf("first answer: %s", text)
	}
	r, _ := f.plugins.Route(f.asOwner, answered)
	for _, body := range []string{"Done.", "Something else."} {
		text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": answered, "body": body})
		want := toolErr("route %s is no longer waiting for an answer (it is %s): it has already been answered", answered, r.State)
		if !isErr || text != want {
			t.Errorf("a second answer %q: %s; want %s", body, text, want)
		}
	}

	timedOut := f.route(t, f.deliveredMessage(t, f.owner, f.agent.ID, "route-reply-late"))
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+timedOut+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("the sweep timed out %d (%v)", n, err)
	}
	text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": timedOut, "body": "Late."})
	if want := toolErr("route %s is no longer waiting for an answer (it is timed_out): its deadline passed and the message went back to the agent", timedOut); !isErr || text != want {
		t.Errorf("a late answer: %s; want %s", text, want)
	}
	if r, _ := f.plugins.Route(f.asOwner, timedOut); r.State != plugins.RouteTimedOut || r.ReplyBody != "" {
		t.Errorf("the late answer moved the route: %s %q", r.State, r.ReplyBody)
	}

	missing := "rt_000000000000000000000000"
	if text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": missing, "body": "x"}); !isErr || text != toolErr("no such route: %s", missing) {
		t.Errorf("no such route: %s", text)
	}
	if text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": "not-a-route", "body": "x"}); !isErr || !strings.Contains(text, "arguments are {route_id, body}") {
		t.Errorf("a malformed id: %s", text)
	}
}

// TestRouteReplyAnswersAnotherOwnersRouteAsNoSuchRoute pins the OWNER filter
// at this door: a host of ANOTHER owner whose window names the route's own
// target session — so the session binding would pass — holding a real route id
// gets exactly what a route id that exists nowhere gets, and the route does not
// move. The filter is row-level security on the Route() lookup under the
// request's principal; a lookup under authz.System would answer the route here.
func TestRouteReplyAnswersAnotherOwnersRouteAsNoSuchRoute(t *testing.T) {
	f := newRouteFixture(t)
	f.withRouteReply(t)
	stranger := f.newMember(t)
	strangerTok := mirrorTok('s')
	f.srv.ext.MirrorTokens.(fakeMirrorTokens)[strangerTok] = mirror.Token{ID: 10, OwnerID: stranger, Host: "tower"}

	id := f.route(t, f.deliveredMessage(t, f.owner, f.agent.ID, "route-reply-stranger"))
	missing := "rt_0000000000000000000000ff"
	gotReal, errReal := f.routeReplyCall(t, strangerTok, routeSID, map[string]any{"route_id": id, "body": "hijack"})
	gotMissing, errMissing := f.routeReplyCall(t, strangerTok, routeSID, map[string]any{"route_id": missing, "body": "hijack"})
	if !errReal || gotReal != toolErr("no such route: %s", id) {
		t.Errorf("another owner's route: %s (error %v); want %s", gotReal, errReal, toolErr("no such route: %s", id))
	}
	// Same shape: the two answers differ only by the id they echo.
	if !errMissing || strings.ReplaceAll(gotReal, id, "ID") != strings.ReplaceAll(gotMissing, missing, "ID") {
		t.Errorf("another owner's route and a missing one answer differently:\n  %s\n  %s", gotReal, gotMissing)
	}
	if r, _ := f.plugins.Route(f.asOwner, id); r.State != plugins.RouteRouted || r.ReplyBody != "" {
		t.Fatalf("another owner moved the route: %s %q", r.State, r.ReplyBody)
	}
}

// TestRouteReplyIsNotOfferedWithRoutingOff: no Routing, no route_reply on the
// list, and a call by name is refused before any handler runs.
func TestRouteReplyIsNotOfferedWithRoutingOff(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)
	f.srv.ext.Routing = nil
	if names := f.mirrorToolNames(t, tok); strings.Contains(" "+strings.Join(names, " ")+" ", " route_reply ") {
		t.Fatalf("route_reply is offered with routing off: %v", names)
	}
	if text, _ := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": "rt_000000000000000000000000", "body": "x"}); !strings.HasPrefix(text, "rpc-error -32602 unknown tool") {
		t.Fatalf("a call with routing off: %s", text)
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1`, f.owner).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d routes (%v)", n, err)
	}
}
