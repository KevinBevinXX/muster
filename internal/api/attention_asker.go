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

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AN ASKER'S OPEN ASKS, ON ITS OWN SESSION PAGE.
//
// A notification about an ask opens the asker's session (pushTargetFor): an
// agent's page, or the rendered view of the window a mirrored Claude session
// runs in. That page shows the asker's OPEN asks with the queue's own answer
// form — attentionItemsHTMLTo, the SAME function /attention draws its items
// with, never a copy — so the ask is answered where the tap lands: its options
// (the chief's task actions included: they are the ask's options), free text,
// Dismiss. The form posts to the queue's answer route with `return` naming
// this page, and the route sends the browser back here (attentionReturn).
//
// 🔴 WHOSE ASKS: an agent page shows the asks whose asker IS that agent
// (Asker.AgentID); a window's view shows the asks of the mirrored Claude
// sessions that window runs, by the rule the Agents-list badges and the push
// use (claudeSessionWindow). Never another asker's, and row-level security keeps
// it to the signed-in owner's.
//
// LIVE: the block re-reads GET /attention/asker on the queue's SSE event
// (EventAttentionChanged) — on the agent page through the shell's htmx SSE, on
// the view (its own document, no htmx) through attentionLiveScript, the
// /attention page's own script — so an ask answered anywhere leaves, and a new
// one arrives, without a reload; neither refresh replaces a half-typed answer.
// ---------------------------------------------------------------------------

// attentionAskerPath is the block's fragment route.
const attentionAskerPath = attentionPath + "/asker"

// askerRef names an asker's session page: an agent (by name) or a mirrored
// window.
type askerRef struct {
	agent string
	// session is the agent page's chat session (its ?session=), 0 for none:
	// the page an answer returns to is the one it was given on.
	session int64
	mirror  ui.MirrorRef
}

// page is the session page itself: where an answer returns to.
func (r askerRef) page() string {
	if r.agent != "" {
		return ui.AgentSessionHref(r.agent, r.session)
	}
	return r.mirror.ViewPath()
}

// src is the block's fragment URL.
func (r askerRef) src() string {
	if r.agent != "" {
		v := url.Values{"agent": {r.agent}}
		if r.session > 0 {
			v.Set("session", strconv.FormatInt(r.session, 10))
		}
		return attentionAskerPath + "?" + v.Encode()
	}
	v := url.Values{"host": {r.mirror.Host}, "session": {r.mirror.Session}}
	if r.mirror.Window != "" {
		v.Set("window", r.mirror.Window)
	}
	return attentionAskerPath + "?" + v.Encode()
}

// askerAsks is ctx's principal's open asks raised by r's asker, newest first;
// nil without the queue's store, for an agent that cannot be read, and on a
// read error (logged): the page then shows no block rather than failing.
func (s *Server) askerAsks(ctx context.Context, r askerRef) []attentionItem {
	if s.ext.Attention == nil {
		return nil
	}
	var agentID int64
	if r.agent != "" {
		if s.ext.Agents == nil {
			return nil
		}
		a, err := s.ext.Agents.GetByName(ctx, r.agent)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.logger.Printf("attention: the asks of agent %q: %v", r.agent, err)
			}
			return nil
		}
		agentID = a.ID
	}
	asks, err := s.ext.Attention.Open(ctx)
	if err != nil {
		s.logger.Printf("attention: the asks for a session page: %v", err)
		return nil
	}
	// The owner's windows, read once and only when a mirrored ask needs them.
	var hosts []mirror.HostView
	listed := false
	hostViews := func() []mirror.HostView {
		if !listed && s.ext.Mirror != nil {
			hosts, listed = s.ext.Mirror.Hosts(ctx), true
		}
		return hosts
	}
	var out []attentionItem
	for _, a := range asks {
		it := askItem(a)
		switch {
		case agentID > 0:
			if it.card.agentID != agentID {
				continue
			}
		case it.card.claudeSession != "":
			ref, ok := claudeSessionWindow(hostViews(), it.card.ref.Host, it.card.claudeSession)
			if !ok || ref.Host != r.mirror.Host || ref.Session != r.mirror.Session || (r.mirror.Window != "" && ref.Window != r.mirror.Window) {
				continue
			}
		default:
			continue
		}
		out = append(out, it)
	}
	sortAttentionItems(out)
	return out
}

// askerAsksHTML is the block's CONTENT for items: the queue's own items
// (attentionItemsHTMLTo), each ask's forms returning to returnTo; "" when
// none, so the block (empty:hidden) takes no room.
func askerAsksHTML(items []attentionItem, returnTo string, now time.Time) string {
	if len(items) == 0 {
		return ""
	}
	n := "1 question"
	if len(items) > 1 {
		n = strconv.Itoa(len(items)) + " questions"
	}
	// The heading row carries "Hide until next ask" (ui.AskerAsksHeading).
	return ui.AskerAsksHeading(n) + `<ul class="flex flex-col gap-2" data-testid="asker-asks">` + attentionItemsHTMLTo(items, now, returnTo) + `</ul>`
}

// askerAsksBlock is ctx's principal's block for r: its content and its
// fragment URL; both "" when the queue is not served (the page draws no block).
func (s *Server) askerAsksBlock(ctx context.Context, r askerRef) (string, string) {
	if s.ext.Attention == nil {
		return "", ""
	}
	items := s.askerAsks(ctx, r)
	s.withFocus(ctx, items)
	return askerAsksHTML(items, r.page(), time.Now()), r.src()
}

// handleAttentionAsker is GET /attention/asker?agent=<name> or
// ?host=&session=[&window=]: the block's content, for its live refresh.
func (s *Server) handleAttentionAsker(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ref := askerRef{agent: q.Get("agent"), mirror: ui.MirrorRef{Host: q.Get("host"), Session: q.Get("session"), Window: q.Get("window")}}
	if ref.agent != "" {
		// An agent's ?session= is its chat session's id (the page's), not a
		// mirrored session's name.
		ref.mirror = ui.MirrorRef{}
		ref.session, _ = strconv.ParseInt(q.Get("session"), 10, 64)
	}
	if ref.agent == "" && (ref.mirror.Host == "" || ref.mirror.Session == "") {
		http.Error(w, "name an agent, or a host and session", http.StatusBadRequest)
		return
	}
	body, _ := s.askerAsksBlock(r.Context(), ref)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// attentionReturn is where an answer or a dismissal given on a session page
// sends the browser back to: the form's `return`, when it is one of the two
// pages that draw the form (an agent's page, a window's rendered view) on this
// origin (safeReturnTo — never an open redirect); otherwise the queue.
func attentionReturn(r *http.Request) string {
	if v, ok := attentionReturnPage(r); ok {
		return v
	}
	return attentionPath
}

// attentionReturnPage is the form's `return` when it is a session page
// (attentionReturn's rule), and whether it is.
func attentionReturnPage(r *http.Request) (string, bool) {
	v := r.PostFormValue("return")
	if v == "" || safeReturnTo(v) != v {
		return "", false
	}
	return v, strings.HasPrefix(v, "/agents/") || strings.HasPrefix(v, "/mirror/view?")
}

// attentionFormFailed answers a refused answer or dismissal: given on a
// session page, that page again with a notice saying why (it may be a framed
// view, which must not fill with the queue page); given on the queue, the
// queue page with its problem, as before.
func (s *Server) attentionFormFailed(w http.ResponseWriter, r *http.Request, err error, cur attention.Ask) {
	if page, ok := attentionReturnPage(r); ok {
		code := noticeAskFailed
		switch {
		case errors.Is(err, attention.ErrNotOpen), errors.Is(err, attention.ErrNotFound):
			code = noticeAskClosed
		case errors.Is(err, attention.ErrInvalid):
			code = noticeAskInvalid
		}
		http.Redirect(w, r, withNotice(page, code), http.StatusSeeOther)
		return
	}
	s.renderAttentionPage(w, r, attentionErrCode(err), attentionErrText(err, cur))
}

// An answer or a dismissal given on a session page that was not recorded: the
// page is shown again with one of these composer notices (composerNotices),
// saying why.
const (
	noticeAskClosed  = "ask-closed"
	noticeAskInvalid = "ask-invalid"
	noticeAskFailed  = "ask-failed"
)

func init() {
	composerNotices[noticeAskClosed] = struct {
		text string
		ok   bool
	}{"Not recorded: that question is no longer open — it was answered, dismissed or expired elsewhere.", false}
	composerNotices[noticeAskInvalid] = struct {
		text string
		ok   bool
	}{"Not recorded: an answer is non-blank text, up to 8000 characters, or one of the question's options.", false}
	composerNotices[noticeAskFailed] = struct {
		text string
		ok   bool
	}{"Not recorded: the answer could not be saved. Try again, or answer on the Asks page.", false}
}
