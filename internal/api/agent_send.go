package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A MESSAGE TO A DISPATCHED AGENT, AS A FORM POST: POST /agents/{name}/send.
//
// The agent's chat page sends over its socket (handleAgentWS), which streams
// the reply back as it is written. This is the door for everything that has no
// such socket:
//
//   - the composer on the agent's TERMINAL page, where the reply is watched in
//     the terminal above it;
//   - the chat page's own form, submitted by the BROWSER because its script did
//     not run or its socket is not open (ui.agentChatPane) — before this, Send
//     did nothing at all in that state.
//
// It hands the text to the SAME turn the socket runs (runUserTurn), in the
// background: the request answers as soon as the message is accepted, the turn
// carries on, and the stored reply reaches open pages through chat.reply.
//
// 🔴 SAME DOOR AS EVERY OPERATOR WRITE: a session and same-origin (the two
// gates requireOperatorWrite composes; here the gateway check sits between
// them, as on the chat socket), and row-level security decides whose agent
// {name} is. There is no step-up here — an agent is not the operator's own terminal,
// and its chat has never had one.
// ---------------------------------------------------------------------------

// agentTurnTimeout bounds a turn started by a POST. The socket's turn is bounded
// by the socket; this one has no connection to end it.
const agentTurnTimeout = 30 * time.Minute

// agentTurnKindSlack is how far past a kind's own turn budget a muster-side turn
// waits — plugin delivery's margin (internal/plugins kindSlack).
const agentTurnKindSlack = 2 * time.Minute

// agentTurnBudget is the budget for a turn to an agent of kind: agentTurnTimeout,
// or — for a kind with its own turn budget (claude-code: agents.ClaudeCodeTurnTimeout,
// which itself outlasts ccd's 30m) — that budget plus agentTurnKindSlack, so the
// typed answer ccd gives at ITS deadline always reaches muster before muster's
// own deadline cuts the turn (agents/kind.go). 37m for claude-code, 30m otherwise.
func agentTurnBudget(kind string) time.Duration {
	if k := agents.KindTurnTimeout(kind); k > 0 && k+agentTurnKindSlack > agentTurnTimeout {
		return k + agentTurnKindSlack
	}
	return agentTurnTimeout
}

// agentSendMax bounds the form: the text, generously, and nothing else of size.
const agentSendMax = 256 << 10

// AgentSendEmpty answers a send with no text.
const AgentSendEmpty = "nothing to send: type a message first"

// agentSendBack is where a send returns the browser to: the agent's Terminal
// when it came from there, else its chat page (on the session it was sent in).
func agentSendBack(name, from, session string) string {
	if from == ui.AgentSendFromTerminal {
		return ui.AgentTerminalPath(name)
	}
	back := "/agents/" + url.PathEscape(name)
	if id, err := strconv.ParseInt(session, 10, 64); err == nil && id > 0 {
		back += "?session=" + strconv.FormatInt(id, 10)
	}
	return back
}

func (s *Server) handleAgentSendAgain(w http.ResponseWriter, r *http.Request) {
	// Only for an agent the principal can see: anyone else's is not found, like
	// its pages.
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	http.Redirect(w, r, withNotice(agentSendBack(a.Name, q.Get("from"), q.Get("session")), noticeAgain), http.StatusSeeOther)
}

// handleAgentSend is POST /agents/{name}/send (text, session, from, nonce).
func (s *Server) handleAgentSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.ext.Agents.GetByName(ctx, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: send get %q: %v", r.PathValue("name"), err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, agentSendMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	text, sessionParam, nonce := r.PostFormValue("text"), r.PostFormValue("session"), r.PostFormValue("nonce")
	nav := isDocumentNavigation(r)
	back := agentSendBack(a.Name, r.PostFormValue("from"), sessionParam)
	refuse := func(status int, msg, notice string) {
		if nav {
			http.Redirect(w, r, withNotice(back, notice), http.StatusSeeOther)
			return
		}
		http.Error(w, msg, status)
	}
	sent := func() {
		if nav {
			http.Redirect(w, r, withNotice(back, noticeSent), http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	if strings.TrimSpace(text) == "" {
		refuse(http.StatusBadRequest, AgentSendEmpty, noticeEmpty)
		return
	}
	// 🔴 ONLY A RUNNING AGENT. A turn handed to an agent that is stopped, failed
	// or still starting is not queued anywhere: the gateway refuses it, in the
	// background, after this request has answered "sent". Refusing here is the
	// only place the person can be told.
	if !agentCanBeSent(s.liveStatus(ctx, a)) {
		refuse(http.StatusConflict, AgentNotRunningNotice, noticeNotRunning)
		return
	}
	session, err := s.resolveSession(ctx, a, sessionParam)
	if err != nil {
		s.logger.Printf("agents: send session for %q: %v", a.Name, err)
		refuse(http.StatusInternalServerError, "could not open a chat session for that agent", noticeFailed)
		return
	}
	repeat, finish := s.sends.begin(requestOwner(ctx), nonce, text)
	if repeat {
		s.logger.Printf("agents: a repeat of a message to %s was not sent again", a.Name)
		sent()
		return
	}
	// Accepted: from here the message is the turn's. The claim is kept whatever
	// the turn then does — a repeat of this post must not start a second turn.
	finish(true)
	s.startAgentTurn(ctx, a, session, text)
	s.logger.Printf("agents: sent a message to %s (session %d)", a.Name, session.ID)
	sent()
}

// agentCanBeSent reports whether an agent whose displayed status is status can
// be handed a message: only a running one.
func agentCanBeSent(status string) bool { return status == agents.StatusRunning }

// startAgentTurn runs a turn for text in the background, detached from the
// request that started it: the request's principal (whose rows the turn reads
// and writes), without its cancellation, bounded by agentTurnBudget.
//
// A turn that fails has no socket to say so on, so the failure is stored in the
// conversation where the reply would have been — the chat page shows it — and
// logged.
func (s *Server) startAgentTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, text string) {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentTurnBudget(a.Kind))
	safeGo(s.logger, "agent turn (send)", func() {
		defer cancel()
		parts, err := s.runUserTurn(tctx, a, session, text, nil)
		if err != nil {
			s.logger.Printf("agents: a turn sent to %s failed: %v", a.Name, err)
			parts = []agents.TurnPart{{Kind: "text", Content: AgentTurnFailedPrefix + err.Error()}}
		}
		s.persistTurn(tctx, a, session, parts)
	})
}

// AgentTurnFailedPrefix starts the stored line that stands in for a reply when
// a turn started by POST /agents/{name}/send failed.
const AgentTurnFailedPrefix = "⚠ This message was not answered: "
