package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE MIRRORED SESSIONS' MCP DOOR: POST /mirror/mcp.
//
// A Claude Code session running in a mirrored tmux window is no agent row and
// holds no muster credential. Its host's muster-mirror serves a loopback-only
// MCP endpoint (internal/mirrorhost's MCPProxy) that forwards each call here
// with the HOST's mirror token and the calling session's Claude session id
// (X-Muster-Claude-Session) — so the session gets the "all agents" tools:
// attention_ask/wait/cancel, and the task tools over the tasks THAT SESSION
// created (taskScopeOf) — plus attention_answer, the operator answering one of
// their asks through their own session, and connector_requests_list /
// connector_request_decide, the operator deciding one of their agents'
// connector requests through it (connector_decide_tools.go), which no
// dispatched agent gets — and, with session routing on, route_reply: the
// answer to a message the chief routed to THAT session (route_reply_tool.go).
//
// 🔴 A MIRROR TOKEN NOW OPENS A SECOND ROUTE, AND THIS ONE IS NARROW. Until this
// door, mmt_ opened /mirror/connect and nothing else. Here it opens exactly
// mirrorTools and nothing else: no chief tool (they are not listed, and
// serveMCP refuses an unlisted name), no agent connector tool, no flow. So a LEAKED mmt_
// reaches, beyond what /mirror/connect already gives, the asks of that host's
// sessions (file, wait, cancel), the tasks those sessions created, and — 🔴
// through attention_answer — an answer AS THE OPERATOR to any of the owner's
// open asks not raised by a mirrored session on that host (the chief's
// included — and, by operator decision, as task_dispatch instructions too:
// a leaked mmt_ can write a dispatched agent's prompt; the kickoff and audit
// row name the host it came from) — and 🔴 through connector_request_decide,
// an APPROVAL AS THE OPERATOR of any of the owner's agents' pending connector
// requests (operator decision "Build connector_approve MCP"; the grant and the
// audit line name the host it came from) — and 🔴 through agent_destroy
// (agent_destroy_tool.go, operator decision), the teardown of any agent of
// the owner's but the chief, with NO audit row (the server log names the host). Never another task, another host's session, another owner's ask
// or request, a secret, or a chief tool. The SESSION a call names is the caller's claim (its pane);
// only the HOST is proven, which is why attention_answer's rule is per host.
//
// 🔴 A HOST ACTS ONLY AS ITS OWN SESSIONS. The token names its host; the
// session id is the caller's claim, and it is believed only when THAT host is
// connected now and lists a window running that Claude session in any pane
// (mirrorSessionOf). It is checked at each tools/call (handleMirrorMCP), not
// at connect: another host's session, a session no window shows, or a token
// of another owner gets a tool error and the tool does not run, so one host
// cannot ask, wait or read as another host's session. initialize and
// tools/list need only the token: they say nothing session-specific.
// ---------------------------------------------------------------------------

// mirrorSessionHeader carries the calling Claude Code session's id.
const mirrorSessionHeader = "X-Muster-Claude-Session"

// mirrorHostCtxKey carries the authenticated host token through the request.
type mirrorHostCtxKey struct{}

func (s *Server) registerMirrorMCPRoutes(mux Mux) {
	mux.HandleFunc("POST /mirror/mcp", s.requireMirrorHostToken(s.handleMirrorMCP))
}

// requireMirrorHostToken admits a mirror host's token (Bearer or
// X-Muster-Token) as its owner, through authz.ViaMirrorHost. No other
// credential opens it.
func (s *Server) requireMirrorHostToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.MirrorTokens == nil {
			http.NotFound(w, r)
			return
		}
		// The failure limit, checked BEFORE the lookup: only FAILED token
		// checks count, per (client address, presented token), and that pair
		// past the limit gets 429 without touching the database until its
		// window (mirrorMCPFailWindow) has passed. 🔴 PER TOKEN, NOT PER
		// ADDRESS: hosts behind one NAT or one Cloudflare edge share an
		// address, so a stale token on one host must never refuse another
		// host's valid token.
		tok := hookTokenFromRequest(r)
		key := mirrorMCPFailKey(clientIP(r), tok)
		if s.mirrorMCPFails.count(key, s.now(), mirrorMCPFailWindow) >= mirrorMCPFailLimit {
			s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed checks of this mirror token from this address; wait a minute"})
			return
		}
		refuse := func() {
			s.mirrorMCPFails.add(key, s.now(), mirrorMCPFailWindow)
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "this route takes a mirror host token (mmt_…)"})
		}
		if !mirror.TokenShapeOK(tok) {
			refuse()
			return
		}
		// Looked up on EVERY call: a revoked token is refused at its next call,
		// and a wait already in flight is bounded by attentionMaxWait.
		t, err := s.mirrorHostFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, mirror.ErrBadToken):
			refuse()
			return
		default:
			s.logger.Printf("mirror mcp: token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the mirror token"})
			return
		}
		ctx := context.WithValue(r.Context(), mirrorHostCtxKey{}, t)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), t.OwnerID, authz.ViaMirrorHost))
	}
}

// mirrorHostFor resolves a mirror token to its row.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON pluginFor DOES: it is how the
// request's owner is LEARNED, and mirror_tokens is under FORCE row-level
// security. The System context is used for this one store call and never
// handed on. System-ledger entry.
func (s *Server) mirrorHostFor(ctx context.Context, tok string) (mirror.Token, error) {
	return s.ext.MirrorTokens.Authenticate(authz.System(ctx), tok)
}

// The /mirror/mcp failure limit: past this many failed checks of one token
// from one address within the window, that token from that address is
// answered 429.
const (
	mirrorMCPFailLimit  = 10
	mirrorMCPFailWindow = time.Minute
)

// mirrorMCPFailKey is the failure counter's key: the client address and a
// hash of the presented token (never the token itself, so the map holds no
// credential). The map stays capped (boundedWindow).
func mirrorMCPFailKey(addr, tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return addr + " " + hex.EncodeToString(sum[:16])
}

// mirrorSessionOf is the window of the principal's host `host` running Claude
// session sid now — in its active pane or in another pane of it (a split) —
// and whether there is one: the host must be online and list it.
func (s *Server) mirrorSessionOf(ctx context.Context, host, sid string) (mirror.SessionView, bool) {
	if s.ext.Mirror == nil || !mirror.ValidClaudeSession(sid) {
		return mirror.SessionView{}, false
	}
	hosts := s.ext.Mirror.Hosts(ctx)
	var window string
	found := false
	for _, h := range hosts {
		if h.Host != host || !h.Online {
			continue
		}
		for _, w := range h.Windows {
			if (w.Claude && w.ClaudeSession == sid) || slices.Contains(w.OtherClaude, sid) {
				window, found = h.Host+"\x00"+w.Session+"\x00"+w.ID, true
			}
		}
	}
	if !found {
		return mirror.SessionView{}, false
	}
	for _, e := range mirror.Ordered(hosts) {
		if e.Host+"\x00"+e.Session+"\x00"+e.Window == window {
			return e, true
		}
	}
	return mirror.SessionView{}, false
}

// mirrorTools is the mirrored sessions' tool set: the all-agents attention and
// task tools, and attention_answer — the operator answering an ask through
// their own session — with the attention store; and connector_requests_list /
// connector_request_decide — the operator deciding a connector request through
// their own session — with the connector store; and agent_destroy — the
// operator tearing down one of their agents — with the attention tools. No
// dispatched agent is offered any operator tool, and their handlers refuse any caller but a mirrored
// session.
func (s *Server) mirrorTools() []agents.ToolDef {
	var out []agents.ToolDef
	if s.attentionToolsOn() {
		out = append(append(append(append(out, attentionToolDefs()...), taskToolDefs()...), attentionAnswerToolDef()), agentDestroyToolDef())
	}
	if s.connectorDecideToolsOn() {
		out = append(out, connectorDecideToolDefs()...)
	}
	// route_reply — a routed session's answer (route_reply_tool.go) — with
	// session routing on.
	if routingOn(s.ext) {
		out = append(out, routeReplyToolDef())
	}
	return out
}

func (s *Server) handleMirrorMCP(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	sid := strings.TrimSpace(r.Header.Get(mirrorSessionHeader))
	instructions := "muster's tools for a mirrored Claude Code session."
	if s.attentionToolsOn() {
		instructions += " " + mirrorAttentionInstruction
	}
	// 🔴 THE SESSION IS BOUND AT tools/call, NOT AT CONNECT. initialize,
	// tools/list, ping and notifications say nothing session-specific — the
	// host's token (checked above) is all they need. Claude Code connects at
	// startup, before its session has a transcript, so no host lists it yet;
	// binding there failed the connection, and Claude Code never retries one.
	// A call is bound to a window this host lists running that session — or
	// refused as a TOOL error the model sees.
	s.serveMCP(w, r, mcpSurface{
		tools: s.mirrorTools,
		call: func(name, args string) string {
			e, ok := s.awaitMirrorSession(r.Context(), t.Host, sid)
			if !ok {
				return toolErr("host %s lists no window running Claude session %q now; a host acts only as its own sessions (its MCP endpoint sends %s)",
					t.Host, sid, mirrorSessionHeader)
			}
			c := toolCaller{asker: attention.Asker{MirrorHost: t.Host, MirrorSession: sid, Label: e.DisplayTitle() + " @ " + t.Host}}
			// route_reply is this door's alone: it is not in dispatchCallerTool,
			// which a gateway agent reaches naming any tool.
			if name == routeReplyToolName {
				return s.toolRouteReply(r.Context(), c, args)
			}
			if text, ok := s.dispatchCallerTool(r.Context(), c, name, args); ok {
				return text
			}
			return toolErr("unknown tool %q", name)
		},
		instructions: instructions,
	})
}

// mirrorBindWait bounds how long a tools/call waits for its host's listing to
// show the calling session (a var so a test can shorten it). The host pushes
// a fresh listing right before it forwards a call (mirrorhost.MCPProxy's
// Refresh), so this covers only that message and this request crossing.
var mirrorBindWait = 3 * time.Second

// awaitMirrorSession is mirrorSessionOf, re-read until it holds or
// mirrorBindWait (or the request) ends. A malformed or empty session id is
// refused at once: no listing can ever show it.
func (s *Server) awaitMirrorSession(ctx context.Context, host, sid string) (mirror.SessionView, bool) {
	if !mirror.ValidClaudeSession(sid) {
		return mirror.SessionView{}, false
	}
	deadline := time.Now().Add(mirrorBindWait)
	for {
		if e, ok := s.mirrorSessionOf(ctx, host, sid); ok {
			return e, true
		}
		if !time.Now().Before(deadline) {
			return mirror.SessionView{}, false
		}
		select {
		case <-ctx.Done():
			return mirror.SessionView{}, false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
