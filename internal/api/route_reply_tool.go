package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// route_reply: a ROUTED SESSION's answer over the mirrored sessions' MCP door
// (mirror_mcp.go) — reply_to_chief without a shell. The routed block tells the
// session to call it (renderRoutedMessage); `muster route reply` (POST
// /api/routes/{id}/reply) stays the fallback for a session with no muster MCP
// server.
//
// 🔴 ONE CORE, TWO DOORS. The answer is recorded and relayed by answerRoute —
// the very function handleRouteReply calls — so the screen, the idempotency,
// the allowlist split and the hand-back on a terminal relay failure are the
// CLI's, not a copy of them.
//
// 🔴 BOUND TO THE ROUTE'S TARGET SESSION, WHICH THE HTTP DOOR IS NOT. POST
// /api/routes/{id}/reply authenticates the OWNER's credential plus the route
// id. This door knows more: the caller is a mirrored session its host lists now
// (handleMirrorMCP's binding), so it answers only a route whose target is THAT
// session ("mirror:<claude session id>") — another of the owner's sessions,
// even on the same host and holding the route id, is refused, and so is every
// caller that is not a mirrored session (a dispatched agent, the chief: the
// tool is not on their lists, and the handler refuses them too).
// ⚠ WHAT THE BINDING PROVES is this door's usual: the HOST, by its token; the
// session is the one the caller's pane names (mirror_mcp.go, attention.md's
// "The host is the principal, not the session"). So it stops one session
// answering another's route by mistake and keeps every other host and every
// agent out; it is not a boundary between two sessions of one host, which can
// name each other's panes.
//
// 🔴 ONLY A ROUTE STILL WAITING. A route that is answered, relayed, fell back,
// timed out or failed is refused up front, naming its state, rather than given
// the HTTP door's same-body idempotency: a session that sees "already answered"
// knows its answer is in, and a late one after a hand-back is not silently
// taken. The state is checked again, under the row lock, by AnswerRoute.
// ---------------------------------------------------------------------------

const routeReplyToolName = "route_reply"

func routeReplyToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: routeReplyToolName,
		Description: "Answer a plugin message your owner's chief ROUTED to this session (the pasted block names its route_id). " +
			"body is your reply to the outside sender, exactly as it should be sent — it goes back as the message's reply: " +
			"at once when your owner allowlisted the sender, otherwise after your owner approves it. Only the session the " +
			"route was sent to may answer it, only once, and only while the route is still waiting (not after it timed out " +
			"back to the chief). Never put credentials in it.",
		Parameters: objSchema(map[string]any{
			"route_id": strProp("The route_id from the routed block (rt_…)."),
			"body":     strProp("Your answer, as it should reach the sender."),
		}, "route_id", "body")}
}

// routeReplyMirrorOnly is route_reply's refusal of a caller that is not a
// mirrored session.
const routeReplyMirrorOnly = "route_reply is available only to the mirrored Claude Code session a message was routed to"

// toolRouteReply is route_reply for caller c (handleMirrorMCP's bound session).
func (s *Server) toolRouteReply(ctx context.Context, c toolCaller, arguments string) string {
	if !c.mirroredSession() {
		return toolErr(routeReplyMirrorOnly)
	}
	if !routingOn(s.ext) {
		return toolErr("session routing is off on this server")
	}
	var in struct {
		RouteID string `json:"route_id"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || !plugins.ValidRouteID(in.RouteID) {
		return toolErr("arguments are {route_id, body}: route_id as the routed block names it (rt_…)")
	}
	r, err := s.ext.Routes.Route(ctx, in.RouteID)
	if err != nil {
		// Another owner's route reads exactly as a missing one (row-level security).
		return toolErr("no such route: %s", in.RouteID)
	}
	if !r.SentToMirrorSession(c.asker.MirrorSession) {
		s.logger.Printf("routes: %s: route_reply REFUSED from %s/%s: the route was sent to %s:%s",
			r.RouteID, c.asker.MirrorHost, c.asker.MirrorSession, r.TargetKind, r.TargetKey)
		return toolErr("route %s was not routed to this session: only the session it was sent to may answer it", r.RouteID)
	}
	if r.State != plugins.RouteRouted {
		return toolErr("route %s is no longer waiting for an answer (it is %s)%s", r.RouteID, r.State, routeClosedHint(r.State))
	}
	out, status := s.answerRoute(ctx, r, in.Body)
	if status != http.StatusOK && status != http.StatusAccepted {
		msg, _ := out["error"].(string)
		if msg == "" {
			msg = "the answer could not be recorded"
		}
		return toolErr("%s", msg)
	}
	s.logger.Printf("routes: %s: answered through route_reply by %s/%s", r.RouteID, c.asker.MirrorHost, c.asker.MirrorSession)
	return toolJSON(out)
}

// routeClosedHint is what a session can make of a route that is not waiting.
func routeClosedHint(st plugins.RouteState) string {
	switch st {
	case plugins.RouteAnswered, plugins.RouteRelayed, plugins.RouteFallback:
		return ": it has already been answered"
	case plugins.RouteTimedOut:
		return ": its deadline passed and the message went back to the chief"
	case plugins.RouteFailed:
		return ": it failed and the message went back to the chief"
	}
	return ""
}
