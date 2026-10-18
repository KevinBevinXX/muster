package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ATTENTION QUEUE: one place showing everything blocked on the owner.
//
// 🔴 THREE SOURCES, ONE READ MODEL, AND ONLY ONE OF THEM IS STORED.
//   - "ask": an agent's explicit question (attention_ask), stored in
//     attention_asks (internal/attention, migration 0025).
//   - "session": a mirrored Claude Code window whose session reports
//     mirror.StatusWaiting (a permission prompt, a question) — read LIVE from
//     the hub's window list, never copied anywhere.
//   - "plugin": a reply draft awaiting its owner's approval — read LIVE from
//     plugin_reply_drafts.
// attentionItems is the one function that merges them; the page, the badge,
// and the chief's attention_list all read it, so they cannot
// disagree about what is waiting.
//
// 🔴 A NOTIFICATION PER NEW ITEM, CLEARED BY TAG WHEN IT RESOLVES. An ask
// pushes when it is filed and sends the "resolved" control message (same tag)
// when it is answered, cancelled or expires. A session pushes when the hub's
// change nudge first shows it waiting (not on a host's first observation, nor
// within attentionRepushAfter of its last push) and clears when it stops. A plugin draft
// keeps its existing `plugin` push (it IS that item's notification; a second
// one would double-buzz), withdrawn when the draft is decided, superseded or
// expires (attention_drafts.go).
// ---------------------------------------------------------------------------

const (
	attentionPath = "/attention"
	// attentionSweepInterval is how often lapsed asks are expired.
	attentionSweepInterval = 30 * time.Second
	// attentionMaxWait bounds one attention_wait call (the long poll).
	attentionMaxWait = 10 * time.Minute
	// attentionDefaultWait is attention_wait's wait when it names none. Under
	// five minutes on purpose: Claude Code aborts an HTTP MCP call that has
	// answered nothing for CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT (5 minutes by
	// default), and whether the keep-alive whitespace counts as an answer is not
	// documented.
	attentionDefaultWait = 4 * time.Minute
	// attentionPoll is how often a waiter re-reads its ask even without a
	// wake-up: another muster replica may have answered it.
	attentionPoll = 2 * time.Second
	// attentionPushBodyMax bounds a question quoted on a lock screen.
	attentionPushBodyMax = 120
	// attentionNotificationType is the push payload type for a new item.
	attentionNotificationType = "attention"
)

// Item kinds.
const (
	attentionKindAsk     = "ask"
	attentionKindSession = "session"
	attentionKindPlugin  = "plugin"
	// attentionKindQuestion is a session's held AskUserQuestion: answered on
	// the rendered view's question card.
	attentionKindQuestion = "question"
)

// attentionState is the queue's per-process state.
type attentionState struct {
	mu sync.Mutex
	// wakes wakes each OWNER's attention_wait calls when this process changes
	// one of that owner's asks — per owner, so a change for one owner does not
	// make every other owner's waiters re-read the database.
	wakes map[int64]*draftSignal
	// waits counts the attention_wait calls in flight, per asker and per owner
	// (attentionWaitSlot).
	waitsByAsker map[string]int
	waitsByOwner map[int64]int
	// waiting is each owner's waiting sessions as last seen, by item key: what
	// a hub nudge is diffed against to push a new one and clear a gone one.
	waiting map[int64]map[string]attentionItem
	// seenHosts is each owner's hosts this process has already observed: a
	// host's FIRST observation seeds its waiting sessions without pushing, so a
	// restart (or the double rollout, or a host reconnecting) re-notifies none.
	seenHosts map[int64]map[string]bool
	// pushedAt is when each session item was last pushed: a session that goes
	// waiting again within attentionRepushAfter is not pushed again.
	pushedAt map[string]time.Time
	// drafts are the reply drafts whose `plugin` notification may be showing,
	// by draft id: what an expiry or a supersede clears (attention_drafts.go).
	drafts       map[int64]draftNote
	draftsSeeded bool
}

// wakeFor is owner's wake-up signal.
func (st *attentionState) wakeFor(owner int64) *draftSignal {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.wakes == nil {
		st.wakes = map[int64]*draftSignal{}
	}
	w := st.wakes[owner]
	if w == nil {
		w = &draftSignal{}
		st.wakes[owner] = w
	}
	return w
}

// attentionItem is one entry of the unified read model.
type attentionItem struct {
	// Key is the item's stable id: its DOM id, its notification tag's suffix.
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Source names who is waiting: the asking agent or session, the window,
	// the plugin.
	Source string `json:"source"`
	// Text is the question, or what the item is waiting for.
	Text    string    `json:"text"`
	Options []string  `json:"options,omitempty"`
	Since   time.Time `json:"since"`
	// Link is where the owner acts on it.
	Link string `json:"link"`
	// AskID is the ask's id ("ask" items only).
	AskID int64 `json:"ask_id,omitempty"`
	// host is a session item's host (its first observation seeds silently).
	host string
}

// attentionPrincipal is ctx acting as owner, through authz.ViaAttention: how
// the hub's change nudge (which names only an owner) reads that owner's
// windows. internal/authz's withPrincipalLedger lists it.
func (s *Server) attentionPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaAttention))
}

func attentionAskKey(id int64) string { return "ask-" + strconv.FormatInt(id, 10) }

func draftKey(id int64) string { return "draft-" + strconv.FormatInt(id, 10) }

// sessionKey is a waiting window's key: a hash of its address, so the key is
// DOM- and tag-safe whatever the host's names contain.
func sessionKey(host, session, window string) string {
	sum := sha256.Sum256([]byte(host + "\x00" + session + "\x00" + window))
	return "session-" + hex.EncodeToString(sum[:8])
}

// askerLabel is what the view shows as an ask's source.
func askerLabel(a attention.Ask) string {
	if a.Asker.Label != "" {
		return a.Asker.Label
	}
	if a.Asker.AgentID > 0 {
		return "agent " + strconv.FormatInt(a.Asker.AgentID, 10)
	}
	return a.Asker.MirrorSession + " @ " + a.Asker.MirrorHost
}

func askItem(a attention.Ask) attentionItem {
	return attentionItem{Key: attentionAskKey(a.ID), Kind: attentionKindAsk, Source: askerLabel(a), Text: a.Question,
		Options: a.Options, Since: a.CreatedAt, Link: attentionPath + "#" + attentionAskKey(a.ID), AskID: a.ID}
}

// sessionWaitingText is a waiting session's text: muster's own words — the
// prompt itself is in the session, one tap away.
const sessionWaitingText = "Waiting on you: a permission prompt or a question in the session."

// waitingSessions is ctx's principal's mirrored windows whose Claude Code
// session reports StatusWaiting, in the Agents list's order. Only an online
// host that reports status (HostView.StatusReport) is believed.
func (s *Server) waitingSessions(ctx context.Context) []attentionItem {
	if s.ext.Mirror == nil {
		return nil
	}
	// The questions a session's AskUserQuestion holds (internal/askq, the
	// muster-mirror ask hook), by host and Claude session: such a window is
	// ONE item — the question, with its text — never a "session waiting" item
	// beside a question item (the hub's waiting overlay marks it waiting too).
	held := map[string]askq.Entry{}
	if owner := requestOwner(ctx); owner > 0 && s.asks != nil {
		for _, e := range s.asks.Pending(owner) {
			if e.Key.Source == askSourceMirror {
				held[e.Key.Scope+"\x00"+e.Key.Session] = e
			}
		}
	}
	hs := s.ext.Mirror.Hosts(ctx)
	type wait struct {
		since int64
		q     *askq.Entry
	}
	waiting := map[string]wait{}
	for _, h := range hs {
		if !h.Online || !h.StatusReport {
			continue
		}
		for _, w := range h.Windows {
			if !w.Claude {
				continue
			}
			k := sessionKey(h.Host, w.Session, w.ID)
			if e, ok := held[h.Host+"\x00"+w.ClaudeSession]; ok && w.ClaudeSession != "" {
				waiting[k] = wait{since: e.Created.UnixMilli(), q: &e}
			} else if w.Status == mirror.StatusWaiting {
				waiting[k] = wait{since: w.StatusSince}
			}
		}
	}
	var out []attentionItem
	for _, e := range mirror.Ordered(hs) {
		k := sessionKey(e.Host, e.Session, e.Window)
		wt, ok := waiting[k]
		if !ok {
			continue
		}
		it := attentionItem{Key: k, Kind: attentionKindSession, Source: e.DisplayTitle() + " @ " + e.Host, Text: sessionWaitingText, host: e.Host,
			Link: ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}.ViewPath()}
		if wt.q != nil {
			it.Kind, it.Text = attentionKindQuestion, heldQuestionText(*wt.q)
		}
		if wt.since > 0 {
			it.Since = time.UnixMilli(wt.since)
		}
		out = append(out, it)
	}
	return out
}

// heldQuestionText is a held AskUserQuestion's text as an item shows it: its
// first question, and how many more it asks.
func heldQuestionText(e askq.Entry) string {
	if len(e.Questions) == 0 {
		return sessionWaitingText
	}
	t := e.Questions[0].Question
	if n := len(e.Questions) - 1; n > 0 {
		t += " (+" + strconv.Itoa(n) + " more)"
	}
	return t
}

// pendingDraftItems is ctx's principal's reply drafts awaiting approval.
func (s *Server) pendingDraftItems(ctx context.Context) ([]attentionItem, error) {
	if s.ext.Plugins == nil {
		return nil, nil
	}
	cards, err := s.ext.Plugins.DraftCards(ctx, pluginDraftsShown)
	if err != nil {
		return nil, err
	}
	var out []attentionItem
	now := time.Now()
	for _, c := range cards {
		if c.Draft.Status != plugins.DraftPending || !c.Draft.ExpiresAt.After(now) {
			continue
		}
		out = append(out, attentionItem{Key: draftKey(c.Draft.ID), Kind: attentionKindPlugin, Source: c.PluginLabel,
			Text: DraftPushTitle, Since: c.Draft.CreatedAt, Link: pluginsPath + "#" + draftCardAnchor(c.Draft.ID)})
	}
	return out, nil
}

// attentionItems is the unified read model for ctx's principal, oldest first.
// A source that cannot be read is left out and named in the error; the rest
// are still returned.
func (s *Server) attentionItems(ctx context.Context) ([]attentionItem, error) {
	var out []attentionItem
	var errs []error
	if s.ext.Attention != nil {
		asks, err := s.ext.Attention.Open(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("asks: %w", err))
		}
		for _, a := range asks {
			out = append(out, askItem(a))
		}
	}
	out = append(out, s.waitingSessions(ctx)...)
	drafts, err := s.pendingDraftItems(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("reply drafts: %w", err))
	}
	out = append(out, drafts...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, errors.Join(errs...)
}

// --- changes: SSE, push, waiters ---------------------------------------------

// attentionChanged tells owner's open views the queue moved and wakes that
// owner's waiters in this process.
func (s *Server) attentionChanged(owner int64, key string) {
	if owner <= 0 {
		return
	}
	s.attention.wakeFor(owner).signal()
	s.broadcastTo(owner, EventAttentionChanged, key)
}

// attentionNotify pushes one new item to owner's devices, under the
// `attention` toggle. goNotify keeps it off the router: the router has no
// attention card, and its notifications are its own.
func (s *Server) attentionNotify(owner int64, it attentionItem) {
	title := "An agent needs your input"
	body := truncate(notificationText(it.Source+": "+firstLine(it.Text)), attentionPushBodyMax)
	switch it.Kind {
	case attentionKindSession:
		title = "A session is waiting on you"
		body = truncate(it.Source, attentionPushBodyMax)
	case attentionKindQuestion:
		title = "A session asks you a question"
	}
	s.goNotify(owner, push.EventAttention, RouterNotification{Type: attentionNotificationType, ID: it.Key, Title: title, Body: body,
		Tag: attentionTag(it.Key), Data: map[string]string{"url": attentionPath + "#" + it.Key}}, nil)
}

// attentionClear closes an item's notification on owner's devices.
func (s *Server) attentionClear(owner int64, key string) {
	s.goNotify(owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: key, Tag: attentionTag(key)}, nil)
}

// attentionTag is an item's notification tag.
func attentionTag(key string) string { return "attention-" + key }

// askSettled is every path that moved an ask out of open: clear its
// notification, update the views, wake the waiters.
func (s *Server) askSettled(a attention.Ask) {
	s.attentionClear(a.Owner, attentionAskKey(a.ID))
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
}

// attentionRepushAfter: a session that goes waiting again within this long of
// its last push is not pushed again (waiting → busy → waiting is one episode).
const attentionRepushAfter = 2 * time.Minute

// AttentionMirrorChanged is attentionMirrorChanged for the mirror hub's
// waiting-set hook (mirror.Service.SetWaitingNotify; cmd/muster-server).
func (s *Server) AttentionMirrorChanged(owner int64) {
	if s == nil {
		return
	}
	s.attentionMirrorChanged(owner)
}

// attentionMirrorChanged diffs owner's waiting sessions against the last
// nudge's. A session that started waiting is pushed — unless its host is
// observed for the first time in this process (seeded silently: a restart or a
// reconnect re-notifies nothing) or it was pushed within attentionRepushAfter;
// one that stopped is cleared. Called on the hub's change nudge
// (BroadcastAgentsChangedFor).
func (s *Server) attentionMirrorChanged(owner int64) {
	if s.ext.Mirror == nil || owner <= 0 {
		return
	}
	now := map[string]attentionItem{}
	hosts := map[string]bool{}
	for _, h := range s.ext.Mirror.Hosts(s.attentionPrincipal(context.Background(), owner)) {
		if h.Online {
			hosts[h.Host] = true
		}
	}
	for _, it := range s.waitingSessions(s.attentionPrincipal(context.Background(), owner)) {
		now[it.Key] = it
	}
	at := s.now()
	var push []attentionItem
	var clear []string
	s.attention.mu.Lock()
	if s.attention.waiting == nil {
		s.attention.waiting = map[int64]map[string]attentionItem{}
		s.attention.seenHosts = map[int64]map[string]bool{}
		s.attention.pushedAt = map[string]time.Time{}
	}
	before := s.attention.waiting[owner]
	seen := s.attention.seenHosts[owner]
	if seen == nil {
		seen = map[string]bool{}
		s.attention.seenHosts[owner] = seen
	}
	s.attention.waiting[owner] = now
	for k, it := range now {
		if _, ok := before[k]; ok {
			continue
		}
		if !seen[it.host] {
			continue // the host's first observation: seed, do not push
		}
		if last, ok := s.attention.pushedAt[k]; ok && at.Sub(last) < attentionRepushAfter {
			continue // the same episode, back again
		}
		s.attention.pushedAt[k] = at
		push = append(push, it)
	}
	for k := range before {
		if _, ok := now[k]; !ok {
			clear = append(clear, k)
		}
	}
	for h := range hosts {
		seen[h] = true
	}
	s.attention.mu.Unlock()
	for _, it := range push {
		s.attentionNotify(owner, it)
	}
	for _, k := range clear {
		s.attentionClear(owner, k)
	}
	if len(push) > 0 || len(clear) > 0 || len(now) != len(before) {
		s.attentionChanged(owner, "")
	}
}

// RunAttentionSweep expires lapsed asks until ctx ends (interval <= 0: every
// 30s): each is cleared from its owner's devices and views, and its waiters
// woken. It also clears the notification of a reply draft that expired
// (expireDraftNotes). Started by cmd/muster-server beside the other background loops; it
// does nothing without a store.
func (s *Server) RunAttentionSweep(ctx context.Context, interval time.Duration) {
	if s.ext.Attention == nil {
		return
	}
	if interval <= 0 {
		interval = attentionSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.expireAsks(ctx)
		s.expireDraftNotes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// expireAsks is one sweep.
func (s *Server) expireAsks(ctx context.Context) {
	due, err := s.ext.Attention.ExpireDue(ctx)
	if err != nil {
		s.logger.Printf("attention: expire: %v", err)
		return
	}
	for _, a := range due {
		s.askSettled(a)
	}
}

// --- routes ------------------------------------------------------------------

func (s *Server) registerAttentionRoutes(mux Mux) {
	mux.HandleFunc("GET "+attentionPath, s.requireSession(s.handleAttentionPage))
	mux.HandleFunc("GET "+attentionPath+"/items", s.requireSession(s.handleAttentionItems))
	mux.HandleFunc("GET "+ui.AttentionBadgePath, s.requireSession(s.handleAttentionBadge))
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/answer", s.requireOperatorWrite(s.handleAttentionAnswerForm))
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/cancel", s.requireOperatorWrite(s.handleAttentionCancelForm))
}

func (s *Server) handleAttentionPage(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	s.renderAttentionPage(w, r, http.StatusOK, "")
}

func (s *Server) renderAttentionPage(w http.ResponseWriter, r *http.Request, code int, problem string) {
	items, err := s.attentionItems(r.Context())
	if err != nil {
		s.logger.Printf("attention: page: %v", err)
		if problem == "" {
			problem = "Part of the queue could not be read."
		}
	}
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="attention-page">
<h1 class="text-balance text-base font-semibold text-fg">Attention</h1>
<p class="text-sm text-fg2">Everything waiting on you: questions your agents asked, sessions waiting at a prompt, and reply drafts awaiting approval.</p>
`)
	b.WriteString(problemNode(problem))
	b.WriteString(`<ul id="attention-items" class="flex flex-col gap-2" data-testid="attention-items" aria-live="polite">`)
	b.WriteString(attentionItemsHTML(items, time.Now()))
	b.WriteString(`</ul>
<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>`)
	b.WriteString(attentionPageScript)
	writeAuthDocument(w, code, "Attention", b.String())
}

// attentionPageScript keeps the list live: on the queue's SSE event it
// re-fetches the items — unless the owner is typing an answer, in which case
// it waits until they leave the field, so a refresh never eats a half-typed
// answer.
const attentionPageScript = `<script>
(function () {
  var list = document.getElementById('attention-items');
  if (!list || !window.EventSource) return;
  var pending = false;
  function busy() {
    var a = document.activeElement;
    if (a && list.contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) return true;
    var fields = list.querySelectorAll('textarea');
    for (var i = 0; i < fields.length; i++) if (fields[i].value.trim() !== '') return true;
    return false;
  }
  function refresh() {
    if (busy()) { pending = true; return; }
    pending = false;
    fetch('/attention/items', { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (html) {
      if (html !== null && !busy()) list.innerHTML = html; else if (html !== null) pending = true;
    }).catch(function () {});
  }
  list.addEventListener('focusout', function () { if (pending) setTimeout(refresh, 0); });
  var es;
  try { es = new EventSource('/events'); } catch (e) { return; }
  es.addEventListener('` + EventAttentionChanged + `', refresh);
  setInterval(refresh, 60000);
})();
</script>`

func (s *Server) handleAttentionItems(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	items, err := s.attentionItems(r.Context())
	if err != nil {
		s.logger.Printf("attention: items: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(attentionItemsHTML(items, time.Now())))
}

func (s *Server) handleAttentionBadge(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	items, err := s.attentionItems(r.Context())
	if err != nil {
		s.logger.Printf("attention: badge: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ui.AttentionBadgeHTML(len(items))))
}

// attentionItemsHTML is the list's <li>s.
func attentionItemsHTML(items []attentionItem, now time.Time) string {
	esc := html.EscapeString
	if len(items) == 0 {
		return `<li class="text-sm text-fg2" data-testid="attention-empty">Nothing is waiting on you.</li>`
	}
	var b strings.Builder
	for _, it := range items {
		kind := map[string]string{attentionKindAsk: "Question", attentionKindSession: "Session waiting", attentionKindPlugin: "Reply draft",
			attentionKindQuestion: "Session question"}[it.Kind]
		age := ""
		if !it.Since.IsZero() {
			age = ` · <time datetime="` + it.Since.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(it.Since))) + `</time>`
		}
		b.WriteString(`<li id="` + esc(it.Key) + `" class="` + flowsCardClass + `" data-attention-item data-kind="` + esc(it.Kind) + `">
<span class="text-xs text-fg2"><span class="font-semibold">` + esc(kind) + `</span> · <span data-attention-source>` + esc(it.Source) + `</span>` + age + `</span>
<p class="whitespace-pre-wrap break-words text-sm text-fg" data-attention-text>` + esc(it.Text) + `</p>
`)
		switch it.Kind {
		case attentionKindAsk:
			id := strconv.FormatInt(it.AskID, 10)
			b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + id + `/answer" class="flex flex-col gap-2" data-attention-answer>
`)
			if len(it.Options) > 0 {
				b.WriteString(`<div class="flex flex-wrap gap-2">`)
				for _, o := range it.Options {
					b.WriteString(`<button type="submit" name="option" value="` + esc(o) + `" class="` + flowsQuietButton + `" data-attention-option>` + esc(o) + `</button>`)
				}
				b.WriteString("</div>\n")
			}
			b.WriteString(`<label for="answer-` + id + `" class="text-xs text-fg2">Your answer</label>
<textarea id="answer-` + id + `" name="answer" rows="2" maxlength="` + strconv.Itoa(attention.MaxAnswerRunes) + `" class="` + flowsInputClass + `"></textarea>
<button type="submit" class="` + flowsButtonClass + `">Answer</button>
</form>
<form method="POST" action="` + attentionPath + `/asks/` + id + `/cancel"><button type="submit" class="` + flowsQuietButton + `">Dismiss without answering</button></form>
`)
		case attentionKindSession:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Open the session</a>
`)
		case attentionKindQuestion:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Answer in the session</a>
`)
		case attentionKindPlugin:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Review the draft</a>
`)
		}
		b.WriteString("</li>\n")
	}
	return b.String()
}

// attentionAge is a short age.
func attentionAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

// askIDParam is the {id} path value.
func askIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// answerAsk records the operator's answer and settles it.
func (s *Server) answerAsk(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	a, err := s.ext.Attention.Answer(ctx, id, answer)
	if err == nil {
		s.askSettled(a)
	}
	return a, err
}

// cancelAsk withdraws an ask and settles it.
func (s *Server) cancelAsk(ctx context.Context, id int64) (attention.Ask, error) {
	a, err := s.ext.Attention.Cancel(ctx, id)
	if err == nil {
		s.askSettled(a)
	}
	return a, err
}

// attentionErrText is a store error as the owner reads it.
func attentionErrText(err error, cur attention.Ask) string {
	switch {
	case errors.Is(err, attention.ErrNotFound):
		return "No such question."
	case errors.Is(err, attention.ErrNotOpen):
		return "That question is no longer open (it is " + string(cur.Status) + ")."
	case errors.Is(err, attention.ErrInvalid):
		return strings.TrimPrefix(err.Error(), attention.ErrInvalid.Error()+": ")
	}
	return "That could not be recorded."
}

func attentionErrCode(err error) int {
	switch {
	case errors.Is(err, attention.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, attention.ErrNotOpen):
		return http.StatusConflict
	case errors.Is(err, attention.ErrInvalid):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) handleAttentionAnswerForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	// An option button wins over the free text: it is what was tapped.
	answer := r.PostFormValue("option")
	if strings.TrimSpace(answer) == "" {
		answer = r.PostFormValue("answer")
	}
	a, err := s.answerAsk(r.Context(), id, answer)
	if err != nil {
		if attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: answer %d: %v", id, err)
		}
		s.renderAttentionPage(w, r, attentionErrCode(err), attentionErrText(err, a))
		return
	}
	http.Redirect(w, r, attentionPath, http.StatusSeeOther)
}

func (s *Server) handleAttentionCancelForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	a, err := s.cancelAsk(r.Context(), id)
	if err != nil {
		s.renderAttentionPage(w, r, attentionErrCode(err), attentionErrText(err, a))
		return
	}
	http.Redirect(w, r, attentionPath, http.StatusSeeOther)
}

func nonNilItems(items []attentionItem) []attentionItem {
	if items == nil {
		return []attentionItem{}
	}
	return items
}
