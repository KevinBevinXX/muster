package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// WHERE TAPPING AN ATTENTION NOTIFICATION LANDS (operator ask: "When tapping on
// a notification, go straight to the session instead of to the attention queue
// item").
//
// 🔴 RESOLVED WHEN TAPPED, NOT WHEN SENT. Every `attention` push carries ONE
// url, attentionOpenURL(key) — GET /attention/open/<key> — and that route
// decides the target at the moment of the tap (pushTargetFor) and redirects.
// A target fixed at send time goes stale: the window closes, the host drops,
// the agent is destroyed, and the tap lands on a 404 where the queue item
// would still have answered. The url is the tap target on BOTH devices — the
// service worker opens it (web/static/sw.js focusOrOpen) and a paired phone's
// payload v2 carries it as its `url` (internal/push PayloadV2) — so neither
// app decides anything. And the send path reads nothing: attentionNotify
// builds the url from the item's key alone, with no database or hub read
// inline in attention_ask or a task ask.
//
// 🔴 ONE RULE, ONE PLACE: pushTargetFor. Something that HAS a session opens
// that session's page:
//   - a waiting mirrored window and a held question: the window's rendered
//     view (it.card.ref, the address the queue's own "Open the session" link
//     uses);
//   - a dispatched agent's attention_ask: the agent's page, /agents/<name>
//     (the owner's agent only);
//   - a mirrored Claude session's attention_ask: the rendered view of the
//     window running that Claude session on its host (claudeSessionWindow, the
//     rule the Agents-list badges use).
//
// EVERYTHING ELSE OPENS ITS ITEM ON THE QUEUE PAGE (attentionItemPath) — an
// explicit fallback, never an empty link and never a 404: an ask that is no
// longer open (answered, dismissed, expired), another owner's or a missing
// one (the same answer, so nothing tells them apart), an ask muster raised
// itself (Asker.System), a Hermes window that names no conversation, an
// asking agent that is gone or not the owner's, an asking mirrored session no
// listed window runs now, a window that stopped waiting, and any kind this
// rule does not name.
// ---------------------------------------------------------------------------

// attentionOpenPath is the tap route's prefix.
const attentionOpenPath = attentionPath + "/open/"

// attentionOpenURL is what an item's notification opens.
func attentionOpenURL(key string) string { return attentionOpenPath + key }

// attentionOpenKey is a key the tap route takes: an ask's, or a waiting
// window's (sessionKey). Anything else is a 404 — it never reaches a redirect.
var attentionOpenKey = regexp.MustCompile(`^(ask-[1-9][0-9]{0,18}|session-[0-9a-f]{16})$`)

// pushTargetLookupTimeout bounds the reads a tap's resolution makes (the ask,
// the asking agent's row), so a slow database delays the tap only briefly
// before it falls back to the queue.
const pushTargetLookupTimeout = 3 * time.Second

// attentionItemPath is an item's place on the queue page: the fallback target.
func attentionItemPath(key string) string { return attentionPath + "#" + key }

// handleAttentionOpen is GET /attention/open/{key}: the item's target now.
func (s *Server) handleAttentionOpen(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if s.ext.Attention == nil || !attentionOpenKey.MatchString(key) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushTargetLookupTimeout)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.openTarget(ctx, key), http.StatusSeeOther)
}

// openTarget is where key's notification opens for ctx's principal now.
func (s *Server) openTarget(ctx context.Context, key string) string {
	owner := requestOwner(ctx)
	if owner <= 0 {
		return attentionItemPath(key)
	}
	var it attentionItem
	found := false
	if rest, ok := strings.CutPrefix(key, "ask-"); ok {
		id, _ := strconv.ParseInt(rest, 10, 64)
		a, err := s.ext.Attention.Get(ctx, id)
		switch {
		case err == nil && a.Owner == owner && a.Status == attention.Open:
			it, found = askItem(a), true
		case err != nil && !errors.Is(err, attention.ErrNotFound):
			s.logger.Printf("attention: the tap target of %s: %v (opening the queue instead)", key, err)
		}
	} else {
		for _, x := range s.waitingSessions(ctx) {
			if x.Key == key {
				it, found = x, true
				break
			}
		}
	}
	if !found {
		return attentionItemPath(key)
	}
	return s.pushTargetFor(ctx, owner, it)
}

// pushTargetFor is where owner's item it opens: its session's page when it has
// one (attentionSessionPath), else its item on the queue page. ctx is owner's
// principal (the tap's request).
func (s *Server) pushTargetFor(ctx context.Context, owner int64, it attentionItem) string {
	if p := s.attentionSessionPath(ctx, owner, it); p != "" {
		return p
	}
	return attentionItemPath(it.Key)
}

// attentionSessionPath is the page of the session it is about, "" when it has
// none muster can name now.
func (s *Server) attentionSessionPath(ctx context.Context, owner int64, it attentionItem) string {
	c := it.card
	switch it.Kind {
	case attentionKindSession, attentionKindQuestion:
		if it.hermes != nil && it.claudeSession == "" {
			return "" // a Hermes window that names no conversation
		}
		if c.ref.Host == "" || c.ref.Session == "" {
			return ""
		}
		return c.ref.ViewPath()
	case attentionKindAsk:
		switch {
		case c.agentID > 0:
			return s.askingAgentPath(ctx, owner, c.agentID)
		case c.claudeSession != "" && c.ref.Host != "":
			if s.ext.Mirror == nil {
				return ""
			}
			ref, ok := claudeSessionWindow(s.ext.Mirror.Hosts(ctx), c.ref.Host, c.claudeSession)
			if !ok {
				return ""
			}
			return ref.ViewPath()
		}
	}
	return ""
}

// askingAgentPath is agent id's page when it is owner's agent, else "". The
// explicit owner check is not only row-level security's: with RLS rolled back
// the read sees every owner's agents, and a tap must never open another
// owner's agent.
func (s *Server) askingAgentPath(ctx context.Context, owner, id int64) string {
	if s.ext.Agents == nil {
		return ""
	}
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Printf("attention: the tap target of agent %d's ask: %v (opening the queue instead)", id, err)
		}
		return ""
	}
	if a.OwnerID != owner {
		return ""
	}
	// The BARE page, on purpose (#237): the tap is resolved when tapped, and the
	// bare URL resolves the agent's latest session at that moment too.
	return ui.AgentPath(a.Name)
}
