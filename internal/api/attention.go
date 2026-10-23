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
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/textplain"
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
	// attentionPushBodyMax bounds a notification body (who and where, never what).
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
	// waitsByAsk counts the attention_wait calls in flight per ask id: while
	// one waits on an ask, its answer is the waiter's and no turn is pushed
	// (attention_delivery.go).
	waitsByAsk map[int64]int
	// deliveries tracks the answer deliveries this process started, so a
	// shutdown (and a test) can wait for each to record its outcome;
	// deliveryBy names this process in a delivery's lease.
	deliveries     sync.WaitGroup
	deliveryByOnce sync.Once
	deliveryBy     string
	// graceOverride, when set (tests, before any delivery starts), replaces
	// attentionDeliveryGrace for this server.
	graceOverride *time.Duration
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
	// diffMu serialises attentionMirrorChanged's read-and-commit, so a
	// recompute that read an older hub state can never commit after a newer one.
	diffMu sync.Mutex
}

// attentionDiffHook, when set (tests), runs between a recompute's read of the
// hub and its commit.
var attentionDiffHook func()

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
	// Text is the item's SUBSTANCE in one line or a few: the question; what a
	// waiting session waits on (its pending action, mirror.Pending.Summary —
	// sessionWaitingText when its host says nothing); a held AskUserQuestion's
	// questions; a reply draft's inbound message ("<sender>: <excerpt>").
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
	// Detail is the second line, "" for none: the last thing a waiting
	// session's Claude said (mirror.Pending.Last); a draft's reply excerpt.
	Detail string `json:"detail,omitempty"`
	// Focus is a mirrored session's focus title (internal/focus), "" for none.
	Focus string `json:"focus,omitempty"`
	// Since is when the item started waiting: an ask's or a draft's creation, a
	// session's status time (when it went waiting), a held question's creation.
	// The queue is NEWEST FIRST by it (sortAttentionItems).
	Since time.Time `json:"since"`
	// Link is where the owner acts on it.
	Link string `json:"link"`
	// AskID is the ask's id ("ask" items only).
	AskID int64 `json:"ask_id,omitempty"`
	// Offline: a session item whose host dropped within attentionOfflineGrace,
	// shown as last seen.
	Offline bool `json:"offline,omitempty"`
	// host is a session item's host (its first observation seeds silently);
	// claudeSession its Claude session id (its focus); tool the NAME of the
	// tool it waits on (mirror.Pending.Tool) — all a push may say of it.
	host, claudeSession, tool string
	// subject is the session the item is about (an asking agent or mirrored
	// session, a waiting window's Claude session); zero for a reply draft.
	// sessions_overview reads it.
	subject focus.Subject
	// card is the Agents-list card the item is counted on (its attention badge,
	// cards_attention.go); zero for none.
	card attentionCard
	// hermes is a waiting HERMES window's ask (Hermes Phase 4): what it waits
	// on, and the approval muster can answer; nil for any other item.
	hermes *hermesAsk
}

// hermesAsk is a waiting Hermes window's ask as an attention item shows it.
type hermesAsk struct {
	// kind is mirror.PendingApproval, PendingClarify or PendingSudo.
	kind string
	// promptID and choices: the approval muster can answer here, "" and nil
	// when it cannot (the host does not answer approvals, or could not read
	// this one) — the item then says to answer in the terminal.
	promptID string
	choices  []string
}

// attentionCard is the card an attention item belongs to: an agent's (an
// asking agent, a reply draft's drafting agent), a mirrored window by its
// address (a waiting window, a held question), or a mirrored Claude session
// on a host (an asking session), which the host's window list resolves to
// its window.
type attentionCard struct {
	agentID int64
	ref     ui.MirrorRef
	// claudeSession, with ref.Host: an asking mirrored session, by its Claude
	// session id.
	claudeSession string
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
	text := a.Question
	if a.HumanOnly {
		// A human-only approval shows EXACTLY what Approve covers: the payload,
		// verbatim, and its digest (migration 0045).
		text += "\n\nApprove queues exactly this text:\n\n" + a.Payload + "\n\nsha256 " + a.PayloadSHA256
	}
	it := attentionItem{Key: attentionAskKey(a.ID), Kind: attentionKindAsk, Source: askerLabel(a), Text: text,
		Options: a.Options, Since: a.CreatedAt, Link: attentionPath + "#" + attentionAskKey(a.ID), AskID: a.ID}
	switch {
	case a.Asker.AgentID > 0:
		it.subject = focus.AgentSubject(a.Asker.AgentID)
		it.card.agentID = a.Asker.AgentID
	case a.Asker.MirrorSession != "":
		it.subject = focus.MirrorSubject(a.Asker.MirrorSession)
		it.card.ref.Host, it.card.claudeSession = a.Asker.MirrorHost, a.Asker.MirrorSession
	}
	return it
}

// hermesWaitingText is a waiting Hermes window's text when its host says
// nothing of what it waits on (a host older than Hermes Phase 4).
const hermesWaitingText = "Hermes is waiting on you in the terminal: an approval, a question or a password."

// sessionWaitingText is a waiting session's text when its host says nothing
// of what it waits on (an older muster-mirror, a transcript it could not read):
// muster's own words — the prompt itself is in the session, one tap away.
const sessionWaitingText = "Waiting on you: a permission prompt or a question in the session."

// attentionOfflineGrace is how long a host that dropped keeps its waiting
// sessions in the queue, as last seen: a blip (a muster-mirror restart, a
// network hiccup) neither drops them from the view nor clears their
// notification — and when it comes back they are not pushed again. A host gone
// longer is gone from the queue and its notifications are cleared.
const attentionOfflineGrace = 5 * time.Minute

// attentionHostBelieved is THE ONE RULE for whose waiting windows the queue
// lists, read by the view and by the push diff alike (attentionMirrorChanged
// takes its items from waitingSessions), so the page and the phone cannot
// disagree: an online host that reports status (HostView.StatusReport), or a
// host that dropped less than attentionOfflineGrace ago (the hub keeps an
// offline host's statuses only when it reported them).
func attentionHostBelieved(h mirror.HostView, now time.Time) bool {
	if h.Online {
		return h.StatusReport
	}
	return !h.OfflineSince.IsZero() && now.Sub(h.OfflineSince) < attentionOfflineGrace
}

// waitingSessions is ctx's principal's mirrored windows whose Claude Code
// session reports StatusWaiting, in the Agents list's order, for every host
// attentionHostBelieved believes.
func (s *Server) waitingSessions(ctx context.Context) []attentionItem {
	if s.ext.Mirror == nil {
		return nil
	}
	// The open questions of a session's AskUserQuestion (internal/askq,
	// registered by the muster-mirror ask hook), by host and Claude session:
	// such a window is ONE item — the question, with its text — never a
	// "session waiting" item beside a question item (Claude Code reports the
	// window waiting while its picker shows).
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
		since   int64
		q       *askq.Entry
		pending *mirror.Pending
		offline bool
		// hermes: a Hermes window; answers: its host answers approvals now.
		hermes, answers bool
	}
	now := s.now()
	waiting := map[string]wait{}
	for _, h := range hs {
		if !attentionHostBelieved(h, now) {
			continue
		}
		for _, w := range h.Windows {
			k := sessionKey(h.Host, w.Session, w.ID)
			if w.AgentRuntime() == mirror.RuntimeHermes {
				// A waiting Hermes window: what its hook says it waits on
				// (Hermes Phase 4). Its Pending names the ask; a host that
				// names none (older) still lists it as waiting.
				if w.Status == mirror.StatusWaiting {
					waiting[k] = wait{since: w.StatusSince, pending: w.Pending, offline: !h.Online, hermes: true, answers: h.Online && h.HermesAnswer}
				}
				continue
			}
			if !w.Claude {
				continue
			}
			if e, ok := held[h.Host+"\x00"+w.ClaudeSession]; ok && w.ClaudeSession != "" {
				waiting[k] = wait{since: e.Created.UnixMilli(), q: &e, offline: !h.Online}
			} else if w.Status == mirror.StatusWaiting {
				waiting[k] = wait{since: w.StatusSince, pending: w.Pending, offline: !h.Online}
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
		ref := ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}
		it := attentionItem{Key: k, Kind: attentionKindSession, Source: e.DisplayTitle() + " @ " + e.Host, Text: sessionWaitingText, host: e.Host,
			claudeSession: e.ClaudeSession, Offline: wt.offline, Link: ref.ViewPath(), card: attentionCard{ref: ref}}
		if e.ClaudeSession != "" {
			it.subject = focus.MirrorSubject(e.ClaudeSession)
		}
		if wt.hermes {
			it.claudeSession = ""
			if key := e.SessionKey(); key != "" {
				// Its focus is keyed runtime-qualified (focus.HermesSubject).
				it.claudeSession, it.subject = key, focus.MirrorSubject(key)
			}
			it.Text, it.hermes = hermesWaitingText, &hermesAsk{}
			if p := wt.pending; p != nil && p.Kind != "" {
				it.hermes.kind = p.Kind
				it.Text, it.Detail, it.tool = p.Summary, p.Detail, p.Tool
				if p.Kind == mirror.PendingApproval && p.PromptID != "" && wt.answers {
					it.hermes.promptID, it.hermes.choices = p.PromptID, strings.Fields(p.Choices)
				}
			}
			if wt.since > 0 {
				it.Since = time.UnixMilli(wt.since)
			}
			out = append(out, it)
			continue
		}
		switch {
		case wt.q != nil:
			it.Kind, it.Text = attentionKindQuestion, heldQuestionText(*wt.q)
		case wt.pending != nil:
			// The host's words, already cleaned and capped on both sides
			// (mirror.CleanPending); the generic line when it names no action.
			if wt.pending.Summary != "" {
				it.Text = wt.pending.Summary
			}
			it.Detail, it.tool = wt.pending.Last, wt.pending.Tool
		}
		if wt.since > 0 {
			it.Since = time.UnixMilli(wt.since)
		}
		out = append(out, it)
	}
	return out
}

// heldQuestionText is a held AskUserQuestion's text as an item shows it: its
// question, or — when it asks several — each question's first line, numbered,
// one per line (a push quotes the first line alone).
func heldQuestionText(e askq.Entry) string {
	switch len(e.Questions) {
	case 0:
		return sessionWaitingText
	case 1:
		return e.Questions[0].Question
	}
	lines := make([]string, 0, len(e.Questions))
	for i, q := range e.Questions {
		lines = append(lines, strconv.Itoa(i+1)+". "+mirror.OneLine(firstLine(strings.TrimSpace(q.Question)), heldQuestionLineMax))
	}
	return strings.Join(lines, "\n")
}

// heldQuestionLineMax bounds each line of a multi-question item.
const heldQuestionLineMax = 120

// Draft excerpts (pendingDraftItems): the inbound message's first
// draftInboundMax runes, the reply's first draftReplyMax.
const (
	draftInboundMax = 160
	draftReplyMax   = 160
)

// draftInboundText is a draft's inbound message as one line: who sent it and
// the start of what they wrote (its subject when the body is empty). A sender
// the plugin did not verify is followed by unverifiedSenderNote: this line
// reaches the chief too (attention_list), not only the Attention view.
func draftInboundText(m plugins.Message) string {
	body := m.Body
	if strings.TrimSpace(body) == "" {
		body = m.Subject
	}
	who := mirror.OneLine(m.Sender, 80)
	if who == "" {
		who = "unknown sender"
	}
	if !m.SenderVerified {
		who += " " + unverifiedSenderNote
	}
	return who + ": " + mirror.OneLine(body, draftInboundMax)
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
		it := attentionItem{Key: draftKey(c.Draft.ID), Kind: attentionKindPlugin, Source: c.PluginLabel,
			Text: draftInboundText(c.Message), Detail: "Draft reply: " + mirror.OneLine(c.Draft.Body, draftReplyMax),
			Since: c.Draft.CreatedAt, Link: pluginsPath + "#" + draftCardAnchor(c.Draft.ID)}
		// The agent that drafted it (none once that agent is deleted).
		if c.Draft.AgentID != nil {
			it.card.agentID = *c.Draft.AgentID
		}
		out = append(out, it)
	}
	return out, nil
}

// attentionItems is the unified read model for ctx's principal, NEWEST first
// (sortAttentionItems).
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
	// Task events the chief has not taken (task_routing.go).
	tasks, err := s.unroutedTaskItems(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("tasks to route: %w", err))
	}
	out = append(out, tasks...)
	sortAttentionItems(out)
	return out, errors.Join(errs...)
}

// sortAttentionItems orders items NEWEST FIRST across every source, by each
// item's own Since (an item with none last), ties broken by Key so the order
// never depends on which source was read first.
func sortAttentionItems(items []attentionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.Since.Equal(b.Since) {
			return a.Since.After(b.Since)
		}
		return a.Key < b.Key
	})
}

// withFocus sets each mirrored item's focus title (its Claude session's
// focus, internal/focus): read once, only when an item has a session, and
// only for the surfaces that show it (the page, the list, attention_list).
func (s *Server) withFocus(ctx context.Context, items []attentionItem) {
	need := false
	for _, it := range items {
		need = need || it.claudeSession != ""
	}
	if !need {
		return
	}
	fi := s.focusIndex(ctx)
	for i := range items {
		if v := fi.mirrorFocus(items[i].claudeSession); v != nil {
			items[i].Focus = v.Title
		}
	}
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
	if it.Kind == attentionKindSession {
		title = "A session is waiting on you"
	} else if it.Kind == attentionKindQuestion {
		title = "A session asks you a question"
	}
	s.goNotify(owner, push.EventAttention, RouterNotification{Type: attentionNotificationType, ID: it.Key, Title: title, Body: attentionPushBody(it),
		Tag: attentionTag(it.Key), Data: map[string]string{"url": attentionPath + "#" + it.Key}}, nil)
}

// attentionPushBody is an item's notification body: WHO and WHERE, never WHAT.
//
// 🔴 A LOCK SCREEN SHOWS IT TO ANYONE HOLDING THE PHONE (operator decision
// "Summary only"): no question text, no command, path or URL, no message —
// those are on the queue page, behind the sign-in. An ask or a held question:
// "<asker> is asking". A waiting session: "Waiting: <tool name> · <window> @
// <host>", or "Waiting: <window> @ <host>" when its host names no tool.
//
// The source is a TITLE (a window's or agent's name, often LLM-written), so it
// reads as the title rule makes it — once, here, before the cut — exactly as
// the attention page draws it. goNotify plains a notification's title only,
// never its body, so this is the one application.
func attentionPushBody(it attentionItem) string {
	source := textplain.Title(it.Source)
	var body string
	switch it.Kind {
	case attentionKindSession:
		body = "Waiting: " + source
		if tool := mirror.OneLine(it.tool, mirror.MaxPendingTool); tool != "" {
			body = "Waiting: " + tool + " · " + source
		}
	default:
		body = source + " is asking"
	}
	return truncate(mirror.OneLine(body, attentionPushBodyMax*2), attentionPushBodyMax)
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
	s.attention.diffMu.Lock()
	defer s.attention.diffMu.Unlock()
	now := map[string]attentionItem{}
	hosts := map[string]bool{} // online now
	for _, h := range s.ext.Mirror.Hosts(s.attentionPrincipal(context.Background(), owner)) {
		if h.Online {
			hosts[h.Host] = true
		}
	}
	for _, it := range s.waitingSessions(s.attentionPrincipal(context.Background(), owner)) {
		now[it.Key] = it
	}
	if attentionDiffHook != nil {
		attentionDiffHook()
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
	// 🔴 A HOST THAT DROPPED (a blip, a restart of its muster-mirror) is not a
	// session that stopped waiting: waitingSessions keeps its items for
	// attentionOfflineGrace (attentionHostBelieved — the SAME rule the view
	// reads, so the page and the notification agree), so they are not cleared
	// here; and it is forgotten as seen, so when it comes back its waiting
	// sessions are seeded silently again. Past the grace they leave both.
	for h := range seen {
		if !hosts[h] {
			delete(seen, h)
		}
	}
	s.attention.waiting[owner] = now
	for k, it := range now {
		if _, ok := before[k]; ok {
			continue
		}
		if !seen[it.host] {
			// The host's first observation: seed, do not push — and stamp it, so
			// the re-push window covers a seeded session too.
			s.attention.pushedAt[k] = at
			continue
		}
		if last, ok := s.attention.pushedAt[k]; ok && at.Sub(last) < attentionRepushAfter {
			continue // the same episode, back again
		}
		s.attention.pushedAt[k] = at
		push = append(push, it)
	}
	moved := false // an item still waiting whose substance changed
	for k, old := range before {
		cur, ok := now[k]
		if !ok {
			clear = append(clear, k)
		} else if cur.Text != old.Text || cur.Detail != old.Detail || cur.Offline != old.Offline {
			moved = true
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
	if len(push) > 0 || len(clear) > 0 || len(now) != len(before) || moved {
		s.attentionChanged(owner, "")
	}
}

// recheckOfflineHosts re-runs the diff for every owner with an item of a host
// that dropped: nothing nudges for a host that is gone, so without this its
// items would outlive attentionOfflineGrace on the owner's devices. On the
// attention sweep.
func (s *Server) recheckOfflineHosts() {
	var owners []int64
	s.attention.mu.Lock()
	for owner, items := range s.attention.waiting {
		for _, it := range items {
			if it.Offline {
				owners = append(owners, owner)
				break
			}
		}
	}
	s.attention.mu.Unlock()
	for _, o := range owners {
		s.attentionMirrorChanged(o)
	}
}

// RunAttentionSweep expires lapsed asks until ctx ends (interval <= 0: every
// 30s): each is cleared from its owner's devices and views, and its waiters
// woken. It also delivers every owed answer (deliverDueAnswers: after a
// restart, a busy agent, a retry). It also clears the notification of a reply draft that expired
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
		s.deliverDueAnswers(ctx)
		s.expireDraftNotes(ctx)
		s.recheckOfflineHosts()
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
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/redeliver", s.requireOperatorWrite(s.handleAttentionRedeliverForm))
	mux.HandleFunc("POST "+attentionHermesPath, s.requireOperatorWrite(s.handleAttentionHermesApprove))
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
	s.withFocus(r.Context(), items)
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
	b.WriteString(attentionNotice(r))
	b.WriteString(`<ul id="attention-items" class="flex flex-col gap-2" data-testid="attention-items" aria-live="polite">`)
	b.WriteString(attentionItemsHTML(items, time.Now()))
	b.WriteString(answeredAsksHTML(s.recentAnswers(r.Context()), time.Now()))
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
	s.withFocus(r.Context(), items)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(attentionItemsHTML(items, time.Now()) + answeredAsksHTML(s.recentAnswers(r.Context()), time.Now())))
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
			attentionKindQuestion: "Session question", attentionKindTask: "Task to route"}[it.Kind]
		age := ""
		if !it.Since.IsZero() {
			age = ` · <time datetime="` + it.Since.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(it.Since))) + `</time>`
		}
		offline := ""
		if it.Offline {
			offline = ` · <span data-attention-offline>host offline</span>`
		}
		b.WriteString(`<li id="` + esc(it.Key) + `" class="` + flowsCardClass + `" data-attention-item data-kind="` + esc(it.Kind) + `">
<span class="text-xs text-fg2"><span class="font-semibold">` + esc(kind) + `</span> · <span data-attention-source>` + esc(textplain.Title(it.Source)) + `</span>` + age + offline + `</span>
`)
		// Source (an agent's or window's name) and Focus (an LLM summary) are
		// titles, so they read as their plain text (textplain).
		if focus := textplain.Title(it.Focus); focus != "" {
			b.WriteString(`<span class="break-words text-xs font-semibold text-fg2" data-attention-focus>` + esc(focus) + `</span>
`)
		}
		text, detail := esc(it.Text), esc(it.Detail)
		if it.hermes != nil {
			// A Hermes ask quotes a command: drawn left to right and isolated
			// (<bdi>), so right-to-left text in it cannot reorder what the
			// person reads around it.
			text, detail = `<bdi dir="ltr">`+text+`</bdi>`, `<bdi dir="ltr">`+detail+`</bdi>`
		}
		b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg" data-attention-text>` + text + `</p>
`)
		if it.Detail != "" {
			b.WriteString(`<p class="break-words text-sm text-fg2" data-attention-detail>` + detail + `</p>
`)
		}
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
			b.WriteString(hermesAskHTML(it))
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Open the session</a>
`)
		case attentionKindQuestion:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Answer in the session</a>
`)
		case attentionKindPlugin:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Review the draft</a>
`)
		case attentionKindTask:
			b.WriteString(`<a href="` + esc(it.Link) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Open the task</a>
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

// answerAsk records the operator's answer given on the Attention view.
func (s *Server) answerAsk(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	return s.answerAskFrom(ctx, id, answer, attention.AnswerSource{})
}

// answerAskFrom records the operator's answer — given on the Attention view
// (src zero) or through their mirrored session's attention_answer (src names
// it) — and settles it: its notification cleared, its waiters woken, and, for an
// agent's ask, its delivery into the agent's session started once the grace
// has passed (attention_delivery.go; the sweep delivers it if this process stops
// first). The ONE answer path both doors take.
func (s *Server) answerAskFrom(ctx context.Context, id int64, answer string, src attention.AnswerSource) (attention.Ask, error) {
	var a attention.Ask
	var err error
	if src.ViaSession() {
		a, err = s.ext.Attention.AnswerFromSession(ctx, id, answer, src.Host, src.Session)
	} else {
		a, err = s.ext.Attention.Answer(ctx, id, answer)
	}
	if err == nil {
		s.askSettled(a)
		if a.Delivery == attention.DeliveryQueued {
			s.startAnswerDelivery(a.Owner, a.ID, s.deliveryGrace()+500*time.Millisecond)
		}
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

// handleAttentionRedeliverForm is an unconfirmed delivery's Retry: back to
// queued (the caller's own ask only — row-level security), then delivered now.
func (s *Server) handleAttentionRedeliverForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	a, err := s.ext.Attention.RetryDelivery(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, attention.ErrNotRetryable):
		s.renderAttentionPage(w, r, http.StatusConflict, "That answer's delivery has not failed, so there is nothing to retry.")
		return
	default:
		if attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: retry the delivery of %d: %v", id, err)
		}
		s.renderAttentionPage(w, r, attentionErrCode(err), attentionErrText(err, a))
		return
	}
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	s.startAnswerDelivery(a.Owner, a.ID, 0)
	http.Redirect(w, r, attentionPath, http.StatusSeeOther)
}

// attentionAnsweredWindow / attentionAnsweredShown bound the "Answered" list:
// the answers of the last day, at most this many.
const (
	attentionAnsweredWindow = 24 * time.Hour
	attentionAnsweredShown  = 20
)

// recentAnswers is ctx's principal's recent answers with a delivery to show.
func (s *Server) recentAnswers(ctx context.Context) []attention.Ask {
	if s.ext.Attention == nil {
		return nil
	}
	out, err := s.ext.Attention.Answered(ctx, time.Now().Add(-attentionAnsweredWindow), attentionAnsweredShown)
	if err != nil {
		s.logger.Printf("attention: recent answers: %v", err)
	}
	return out
}

// answerDeliveryText is where an answer went, as the owner reads it, and
// whether it offers Retry.
func answerDeliveryText(a attention.Ask) (string, bool) {
	who := askerLabel(a)
	switch a.Delivery {
	case attention.DeliveryQueued:
		if a.DeliveryDetail != "" {
			return "Queued for " + who + " — waiting: " + a.DeliveryDetail + ". muster keeps trying.", false
		}
		return "Queued for " + who + ": delivering it as a new message in its session.", false
	case attention.DeliveryDelivering:
		return "Delivering to " + who + " now.", false
	case attention.DeliveryDelivered:
		return "Delivered to " + who + " as a new message in its session.", false
	case attention.DeliveryWaited:
		return who + " read it while waiting for it.", false
	case attention.DeliveryUnconfirmed:
		return "Delivery failed: " + firstNonBlank(a.DeliveryDetail, "unknown") + ".", true
	case attention.DeliveryPull:
		return "Waiting for " + who + " to read it: a mirrored session reads its answer itself (attention_wait); muster does not type into it.", false
	}
	if a.Asker.System {
		return "Recorded. muster raised this question itself, so there is nobody to deliver it to.", false
	}
	return "Not delivered.", false
}

// answeredAsksHTML is the "Answered" part of the list: each recent answer and
// where it went, with Retry on a failed delivery.
func answeredAsksHTML(asks []attention.Ask, now time.Time) string {
	if len(asks) == 0 {
		return ""
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<li class="pt-2 text-xs font-semibold uppercase tracking-wide text-fg2" data-attention-answered-heading>Answered</li>` + "\n")
	for _, a := range asks {
		id := strconv.FormatInt(a.ID, 10)
		text, retry := answerDeliveryText(a)
		age := ""
		if !a.ResolvedAt.IsZero() {
			age = ` · <time datetime="` + a.ResolvedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(a.ResolvedAt))) + `</time>`
		}
		b.WriteString(`<li id="answered-` + id + `" class="` + flowsCardClass + `" data-attention-answered data-ask-id="` + id + `" data-delivery="` + esc(firstNonBlank(string(a.Delivery), "none")) + `">
<span class="text-xs text-fg2"><span class="font-semibold">Answered</span> · <span data-attention-source>` + esc(askerLabel(a)) + `</span>` + age + `</span>
<p class="break-words text-sm text-fg2" data-attention-text>` + esc(mirror.OneLine(a.Question, 200)) + `</p>
<p class="whitespace-pre-wrap break-words text-sm text-fg" data-attention-answer-text>Your answer: ` + esc(a.Answer) + `</p>
` + answeredViaHTML(a) + `<p class="break-words text-sm text-fg" data-attention-delivery>` + esc(text) + `</p>
`)
		if retry {
			b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + id + `/redeliver"><button type="submit" class="` + flowsQuietButton + `" data-attention-redeliver>Retry delivery</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	return b.String()
}

// answeredViaHTML is an Answered item's provenance line: "" for an answer given
// on this view; for attention_answer, the mirrored session it came from.
func answeredViaHTML(a attention.Ask) string {
	if !a.AnsweredVia.ViaSession() {
		return ""
	}
	return `<p class="break-words text-xs text-fg2" data-attention-answered-via>Given via MCP from your mirrored session ` +
		html.EscapeString(a.AnsweredVia.String()) + `</p>
`
}

func nonNilItems(items []attentionItem) []attentionItem {
	if items == nil {
		return []attentionItem{}
	}
	return items
}

// --- answered in the session -------------------------------------------------

// askerAddressed is the operator writing to an asker DIRECTLY — a message
// pasted into a mirrored session's window (mirrorDeliver), queued for it
// (sendQueueAdd), or sent to a dispatched agent's chat (handleAgentSend,
// handleAgentWS): every ask that asker raised before the message that is still
// open leaves the queue as
// answered_in_session, the message (capped) its answer — its notification
// cleared, its waiters woken (askSettled), so the asker's attention_wait
// returns {status: "answered_in_session", answer}.
//
// 🔴 ONLY THE OPERATOR'S OWN MESSAGES CALL THIS. A routed message (session
// routing's send-queue rows), the chief's hand-back turn, a machine-token
// message and anything an agent sends are not the operator answering, and
// none of those paths call it. ctx is the operator's principal (row-level
// security keeps it to their own asks).
func (s *Server) askerAddressed(ctx context.Context, asker attention.Asker, message string) {
	s.askerAddressedBy(ctx, asker, message, 0)
}

// askerAddressedBy is askerAddressed, recording the send-queue row (queueID,
// 0 = none) the message is, so that row can say it answered a question.
func (s *Server) askerAddressedBy(ctx context.Context, asker attention.Asker, message string, queueID int64) {
	if s.ext.Attention == nil || asker.Valid() != nil {
		return
	}
	asks, err := s.ext.Attention.AnswerInSession(context.WithoutCancel(ctx), asker, message, queueID)
	if err != nil {
		s.logger.Printf("attention: answered in session by a message to %s: %v", askerName(asker), err)
		return
	}
	for _, a := range asks {
		s.logger.Printf("attention: ask %d answered in session (a message to %s)", a.ID, askerName(asker))
		s.askSettled(a)
	}
}

// mirrorAddressed is askerAddressed for a mirrored window's Claude Code
// session — ONLY for a message pasted into that session's CLAUDE PANE
// (mirror.PasteClaude: the rendered view's Send, a queued message). A Terminal
// send goes to the window's ACTIVE pane, which may be a shell beside Claude
// Code, so it answers nothing. queueID is the send-queue row the message is,
// 0 for a direct Send. Nothing for a window that runs no Claude Code.
func (s *Server) mirrorAddressed(ctx context.Context, host string, e mirror.SessionView, target mirror.PasteTarget, message string, queueID int64) {
	if target != mirror.PasteClaude || !e.Claude || e.ClaudeSession == "" {
		return
	}
	s.askerAddressedBy(ctx, attention.Asker{MirrorHost: host, MirrorSession: e.ClaudeSession}, message, queueID)
}

// askerName names an asker in the log: its ids, never its label.
func askerName(a attention.Asker) string {
	if a.AgentID > 0 {
		return "agent " + strconv.FormatInt(a.AgentID, 10)
	}
	return "session " + a.MirrorSession + " @ " + a.MirrorHost
}
