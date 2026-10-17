package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SESSION FOCUS's server half (internal/focus, migration 0020): the refresher's
// two sources (the agents table and the mirror hub), its periodic job, and the
// card's focus chip. The tools are in focus_tools.go.
//
// 🔴 THE REFRESH RUNS IN MUSTER, NOT IN THE CHIEF (operator decision). A pass
// (RunFocusRefresh) and the focus_refresh tool both run focus.Refresher here,
// acting as each owner through focusPrincipal; the chief only ever READS focus.
//
// 🔴 A MIRRORED SESSION'S TAIL IS READ THROUGH THE HOST'S OWN WATCH. muster
// never holds a transcript path or file: it opens the same transcript stream
// the rendered view opens (Mirror.WatchWindow), takes the bounded backfill the
// host sends — at most mirror.MaxBackfillEvents, from the last
// mirror.BackfillWindow bytes, each field capped — and closes it. So a refresh
// costs the host what opening the rendered view costs, once.
// ---------------------------------------------------------------------------

// focusTailTimeout bounds reading one mirrored session's backfill, and
// focusTailQuiet is how long the stream must stay quiet after its first batch
// for the backfill to count as complete when the host sends no prompt index
// (an older muster-mirror).
const (
	focusTailTimeout = 15 * time.Second
	focusTailQuiet   = 750 * time.Millisecond
	// focusAgentMessages is how many of an agent's newest chat messages its
	// tail is read from (focus.RenderTail then bounds them again).
	focusAgentMessages = 80
)

// focusPrincipal is ctx acting as owner, through authz.ViaFocus: how the
// refresher reads one owner's sessions and writes their focus. Its callers
// are the refresher and the retention sweep (their AsOwner), which learned the
// owner ids from focus.PGStore.Owners; internal/authz's withPrincipalLedger lists it.
func (s *Server) focusPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaFocus))
}

// refresher is the server's one focus.Refresher, or nil with no focus store or
// no summarizer.
func (s *Server) refresher() *focus.Refresher {
	s.focusOnce.Do(func() {
		if s.ext.Focus == nil || s.ext.FocusSummarizer == nil {
			return
		}
		s.focusRefresher = &focus.Refresher{Store: s.ext.Focus, Sessions: focusSessions{s}, Summarizer: s.ext.FocusSummarizer,
			AsOwner: s.focusPrincipal, Logger: s.logger}
	})
	return s.focusRefresher
}

// RunFocusRefresh runs the periodic focus pass until ctx ends (interval <= 0:
// focus.DefaultInterval). It does nothing without a focus store and a
// summarizer. Started by cmd/muster-server beside the other background loops.
func (s *Server) RunFocusRefresh(ctx context.Context, interval time.Duration) {
	r := s.refresher()
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = focus.DefaultInterval
	}
	s.logger.Printf("focus: refresh every %s (MUSTER_FOCUS_INTERVAL): at most %d summarizer calls per owner and %d in all per hour, "+
		"%d at once; a session whose transcript has not moved is not summarized again",
		interval, focus.PerOwnerPerHour, focus.GlobalPerHour, focus.MaxConcurrent)
	r.Run(ctx, interval)
}

// RunFocusRetention runs the focus retention sweep (focus.Retainer) now and
// every focus.RetentionSweepInterval until ctx ends. It needs only the focus
// store — not a summarizer, not MUSTER_FOCUS_INTERVAL — so a mirrored
// session's focus is deleted MUSTER_FOCUS_RETENTION after it was last seen
// whether or not the refresh pass runs. Started by cmd/muster-server.
func (s *Server) RunFocusRetention(ctx context.Context) {
	r := s.focusRetainer()
	if r == nil {
		return
	}
	s.logger.Printf("focus: a mirrored session's focus is deleted %s after the session was last seen (MUSTER_FOCUS_RETENTION), "+
		"and focus history %s after it was written (MUSTER_FOCUS_HISTORY_RETENTION); swept every %s",
		r.Retention, r.HistoryRetention, focus.RetentionSweepInterval)
	r.Run(ctx, focus.RetentionSweepInterval)
}

// focusRetainer is the retention sweep over this server, or nil with no focus
// store.
func (s *Server) focusRetainer() *focus.Retainer {
	if s.ext.Focus == nil {
		return nil
	}
	ret := s.ext.FocusRetention
	if ret <= 0 {
		ret = focus.DefaultRetention
	}
	hist := s.ext.FocusHistoryRetention
	if hist <= 0 {
		hist = focus.DefaultHistoryRetention
	}
	return &focus.Retainer{Store: s.ext.Focus, Sessions: focusSessions{s}, AsOwner: s.focusPrincipal,
		Retention: ret, HistoryRetention: hist, Logger: s.logger}
}

// focusSessions is focus.Sessions over this server: the owner's agents and
// the Claude Code windows its mirror hosts list. ctx acts as one owner.
type focusSessions struct{ s *Server }

// Candidates is every session of the owner focus can describe, with when it
// was last active.
func (f focusSessions) Candidates(ctx context.Context) ([]focus.Candidate, error) {
	var out []focus.Candidate
	if f.s.ext.Agents != nil {
		list, err := f.s.ext.Agents.List(ctx)
		if err != nil {
			return nil, err
		}
		last := f.s.lastMessageByAgents(ctx, list)
		for _, a := range list {
			out = append(out, focus.Candidate{Subject: focus.AgentSubject(a.ID), ActiveAt: agents.LastActivity(a, last[a.ID])})
		}
	}
	seen := map[string]bool{}
	for _, e := range f.s.mirrorClaudeEntries(ctx) {
		if seen[e.ClaudeSession] {
			continue
		}
		seen[e.ClaudeSession] = true
		out = append(out, focus.Candidate{Subject: focus.MirrorSubject(e.ClaudeSession), ActiveAt: e.ActiveAt()})
	}
	return out, nil
}

// mirrorClaudeEntries is every ONLINE mirrored window of ctx's principal whose
// host reports a Claude session id for it, most recently active first.
func (s *Server) mirrorClaudeEntries(ctx context.Context) []mirror.SessionView {
	if s.ext.Mirror == nil {
		return nil
	}
	var out []mirror.SessionView
	for _, e := range mirror.Ordered(s.ext.Mirror.Hosts(ctx)) {
		if e.Online && e.Claude && e.ClaudeSession != "" {
			out = append(out, e)
		}
	}
	return out
}

// mirrorEntryForSession is the online window whose conversation is Claude
// session sid (the most recently active, should two list it), and whether
// there is one.
func (s *Server) mirrorEntryForSession(ctx context.Context, sid string) (mirror.SessionView, bool) {
	for _, e := range s.mirrorClaudeEntries(ctx) {
		if e.ClaudeSession == sid {
			return e, true
		}
	}
	return mirror.SessionView{}, false
}

// errNoSession is a subject focus cannot read: an agent the owner does not
// have, or a Claude session no online window shows.
var errNoSession = fmt.Errorf("%w (an agent you do not have, or a Claude session no online mirrored window shows)", focus.ErrNoSession)

// Visible is nil when sub can be read now: an agent of the owner's, or a
// Claude session an online mirrored window shows.
func (f focusSessions) Visible(ctx context.Context, sub focus.Subject) error {
	switch sub.Kind {
	case focus.KindAgent:
		if f.s.ext.Agents == nil {
			return errNoSession
		}
		if _, err := f.s.ext.Agents.Get(ctx, sub.AgentID()); err != nil {
			return errNoSession
		}
		return nil
	case focus.KindMirror:
		if _, ok := f.s.mirrorEntryForSession(ctx, sub.Key); !ok {
			return errNoSession
		}
		return nil
	}
	return focus.ErrBadSubject
}

// Tail is sub's conversation, newest last.
func (f focusSessions) Tail(ctx context.Context, sub focus.Subject) ([]focus.Entry, error) {
	switch sub.Kind {
	case focus.KindAgent:
		return f.s.agentTail(ctx, sub.AgentID())
	case focus.KindMirror:
		e, ok := f.s.mirrorEntryForSession(ctx, sub.Key)
		if !ok {
			return nil, errNoSession
		}
		return f.s.mirrorTail(ctx, e, sub.Key)
	}
	return nil, focus.ErrBadSubject
}

// agentTail is agent id's newest conversation's last messages.
func (s *Server) agentTail(ctx context.Context, id int64) ([]focus.Entry, error) {
	if s.ext.Agents == nil {
		return nil, errNoSession
	}
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		return nil, errNoSession
	}
	sessions, err := s.ext.Agents.ListSessions(ctx, a.ID)
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, sessions[0].ID, focusAgentMessages)
	if err != nil {
		return nil, err
	}
	out := make([]focus.Entry, 0, len(msgs))
	for _, m := range msgs {
		switch m.Kind {
		case "tool_call":
			// Name and file path only: m.Content is the raw argument JSON.
			out = append(out, focus.Entry{Role: "tool", Text: focus.ToolLine(m.ToolName, m.Content)})
		case "tool_result":
		default:
			out = append(out, focus.Entry{Role: m.Role, Text: m.Content})
		}
	}
	return out, nil
}

// mirrorTail is the bounded backfill of e's transcript, read through the host's
// watch and closed.
//
// 🔴 THE STREAM MUST BE CONVERSATION sid. The window was chosen because it
// listed sid, but a /clear or a restart between that listing and this watch
// puts ANOTHER conversation in it — whose summary would then be stored under
// mirror:sid. Every batch names its transcript (Msg.Gen); one that is not sid
// (or names none) ends the read as errNoSession, and nothing is stored.
func (s *Server) mirrorTail(ctx context.Context, e mirror.SessionView, sid string) ([]focus.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, focusTailTimeout)
	defer cancel()
	var mu sync.Mutex
	var out []focus.Entry
	var otherConversation bool
	got := make(chan struct{}, 1)
	done := make(chan error, 1)
	safeGo(s.logger, "focus: read a mirrored tail", func() {
		done <- s.ext.Mirror.WatchWindow(ctx, e.Host, e.Session, e.Window, "", 0, func(m mirror.Msg) error {
			mu.Lock()
			if m.Gen != sid {
				otherConversation = true
				mu.Unlock()
				cancel()
				return nil
			}
			if m.Reset {
				out = out[:0]
			}
			for _, ev := range m.Events {
				if en, ok := focusEntryOf(ev); ok {
					out = append(out, en)
				}
			}
			index := m.PromptIndex
			mu.Unlock()
			if index {
				// The prompt index follows the backfill: it is complete.
				cancel()
				return nil
			}
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	})
	quiet := time.NewTimer(focusTailTimeout)
	defer quiet.Stop()
	var err error
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-got:
			quiet.Reset(focusTailQuiet)
		case <-quiet.C:
			cancel()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if otherConversation {
		s.logger.Printf("focus: %s@%s %s%s holds another conversation now, not %s; nothing stored", e.Session, e.Host, e.Session, e.Window, sid)
		return nil, errNoSession
	}
	if len(out) == 0 && err != nil {
		return nil, err
	}
	return append([]focus.Entry(nil), out...), nil
}

// focusEntryOf is one transcript event as a tail entry: a prompt, a reply, a
// tool call by name and target, an error. Tool output and notices are left out.
func focusEntryOf(ev cctranscript.Event) (focus.Entry, bool) {
	switch ev.Kind {
	case cctranscript.KindPrompt:
		return focus.Entry{Role: "user", Text: ev.Text}, true
	case cctranscript.KindText:
		return focus.Entry{Role: "assistant", Text: ev.Text}, true
	case cctranscript.KindToolCall:
		// Name and file path only (focus.ToolLine), from the call's input:
		// never ev.Target, which for Bash IS the command line.
		return focus.Entry{Role: "tool", Text: focus.ToolLine(ev.Tool, ev.Input)}, true
	case cctranscript.KindError:
		return focus.Entry{Role: "error", Text: ev.Text}, true
	}
	return focus.Entry{}, false
}

// focusIndex is the viewer's focus by subject, read once per render; empty
// with no focus store or on a read error (a card then shows no focus).
type focusIndex map[focus.Subject]focus.Focus

func (s *Server) focusIndex(ctx context.Context) focusIndex {
	if s.ext.Focus == nil {
		return nil
	}
	list, err := s.ext.Focus.List(ctx)
	if err != nil {
		s.logger.Printf("focus: list for the cards: %v", err)
		return nil
	}
	out := focusIndex{}
	for _, f := range list {
		// A row with no focus is kept when it says WHY (its last error): the
		// card shows the reason instead of nothing.
		if f.Has() || f.Problem() != "" {
			out[f.Subject] = f
		}
	}
	return out
}

// view is sub's focus as a card shows it — with why it is missing or stale,
// when the latest refresh failed — nil for neither.
func (fi focusIndex) view(sub focus.Subject) *ui.FocusView {
	f, ok := fi[sub]
	if !ok {
		return nil
	}
	v := &ui.FocusView{Title: f.Title, Description: f.Description, UpdatedAt: f.UpdatedAt}
	if !f.Has() {
		v.UpdatedAt = time.Time{}
	}
	if p := f.Problem(); p != "" {
		v.Problem, v.ProblemAt = p, f.ErrorAt
	}
	return v
}

// mirrorFocus is a mirrored entry's focus view: its Claude session's, nil
// when it reports none.
func (fi focusIndex) mirrorFocus(sid string) *ui.FocusView {
	if sid == "" {
		return nil
	}
	return fi.view(focus.MirrorSubject(sid))
}
