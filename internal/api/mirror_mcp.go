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
// created (taskScopeOf).
//
// 🔴 A MIRROR TOKEN NOW OPENS A SECOND ROUTE, AND THIS ONE IS NARROW. Until this
// door, mmt_ opened /mirror/connect and nothing else. Here it opens exactly the
// all-agents tool set (mirrorTools) and nothing else: no chief tool (they are
// not listed, and serveMCP refuses an unlisted name), no connector, no flow.
// So a LEAKED mmt_ reaches, beyond what /mirror/connect already gives, the
// asks of that host's sessions (file, wait, cancel) and the tasks those
// sessions created — never another task, another host's session or the chief.
//
// 🔴 A HOST ACTS ONLY AS ITS OWN SESSIONS. The token names its host; the
// session id is the caller's claim, and it is believed only when THAT host is
// connected now and lists a window running that Claude session in any pane
// (mirrorSessionOf). Another host's session, a session no window shows, or a
// token of another owner is refused before any tool runs (403), so one host
// cannot ask, wait or read as another host's session.
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
// task tools, and nothing else.
func (s *Server) mirrorTools() []agents.ToolDef {
	if !s.attentionToolsOn() {
		return nil
	}
	return append(attentionToolDefs(), taskToolDefs()...)
}

func (s *Server) handleMirrorMCP(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	sid := strings.TrimSpace(r.Header.Get(mirrorSessionHeader))
	e, ok := s.mirrorSessionOf(r.Context(), t.Host, sid)
	if !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "host " + t.Host + " lists no window running Claude session " +
			strings.TrimSpace(sid) + " now; a host acts only as its own sessions (send " + mirrorSessionHeader + ")"})
		return
	}
	c := toolCaller{asker: attention.Asker{MirrorHost: t.Host, MirrorSession: sid, Label: e.DisplayTitle() + " @ " + t.Host}}
	instructions := "muster's tools for a mirrored Claude Code session."
	if s.attentionToolsOn() {
		instructions += " " + attentionInstruction
	}
	s.serveMCP(w, r, mcpSurface{
		tools: s.mirrorTools,
		call: func(name, args string) string {
			if text, ok := s.dispatchCallerTool(r.Context(), c, name, args); ok {
				return text
			}
			return toolErr("unknown tool %q", name)
		},
		instructions: instructions,
	})
}
