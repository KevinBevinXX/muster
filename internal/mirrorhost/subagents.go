package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ZacxDev/muster/internal/ccsubagents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
)

// THE SUBAGENT CARDS' host half (internal/mirror's subagents.go): what each
// subagent an open rendered view's session spawned is doing, read from the
// session's own subagents directory and sent on that view's stream.
//
// What a session's subagents are, and the status rule, are
// internal/ccsubagents' (the ONE reading, shared with ccd's work count); this
// file is the rendered view's half: the summaries a view's stream sends and a
// card's tail.
//
// The layout, measured on Claude Code 2.1.289 (workstation) and 2.1.220
// (laptop) — the same on both:
//
//	<projects>/<slug>/<session-id>.jsonl                       the parent
//	<projects>/<slug>/<session-id>/subagents/agent-<id>.jsonl  each subagent
//	<projects>/<slug>/<session-id>/subagents/agent-<id>.meta.json
//
// The meta is {"agentType","description","toolUseId",…}: toolUseId is the
// parent's Agent tool_use, which is how a subagent finds its card. 2.1.289
// adds "requestShape":"background" for a background agent; 2.1.220 writes no
// requestShape at all, so "background" is also read off the parent's result
// (toolUseResult.isAsync). A NESTED subagent (one a subagent spawned: meta
// "parentAgentId") lives in the same flat directory, but its toolUseId is in
// its parent agent's transcript, never the session's — so it has no card here.
// Every field is read by NAME from a decoded object, never by position, and
// one of another JSON type is simply not read.
//
// 🔴 A MISSING META IS NOT AN UNKNOWN AGENT: the parent's tool_result names
// the agent too (toolUseResult.agentId), so a subagent whose meta was never
// written is linked once its call has a result (a background agent's comes at
// once). Until then its card shows the call alone.
//
// 🔴 A TORN LAST LINE (the CLI mid-write) is never read: only lines ending in
// a newline are, here as in the parent's tailer.

// SubagentQuiet is ccsubagents.Quiet: how long a subagent whose last row is a
// final reply must go without a new row before it is called done with no word
// from the parent.
const SubagentQuiet = ccsubagents.Quiet

// subagentTracker is one view's subagents: the shared tracker
// (ccsubagents.Tracker) plus what this view last sent.
//
// 🔴 IN MEMORY, PER VIEW, BOUNDED: it lives as long as the view's stream and is
// replaced when the view's transcript is (a /clear, a restart).
type subagentTracker struct {
	*ccsubagents.Tracker
	sent []mirror.Subagent
	once bool // a list was sent
	now  func() time.Time
}

func newSubagentTracker(transcript string) *subagentTracker {
	return &subagentTracker{Tracker: ccsubagents.NewTracker(transcript), now: time.Now}
}

// observe takes the parent's events, in transcript order.
func (t *subagentTracker) observe(events []cctranscript.Event) { t.Observe(events) }

// scan is every tracked Agent call's summary that has an agent to describe,
// oldest call first, normalised for the wire.
func (t *subagentTracker) scan() []mirror.Subagent {
	if len(t.Calls) == 0 {
		return []mirror.Subagent{}
	}
	t.ReadMetas()
	byTool := map[string]string{}
	for id, m := range t.Metas {
		if m.Parent == "" {
			byTool[m.ToolUseID] = id
		}
	}
	now := t.now()
	calls := t.Calls
	if len(calls) > mirror.MaxSubagents {
		calls = calls[len(calls)-mirror.MaxSubagents:]
	}
	out := make([]mirror.Subagent, 0, len(calls))
	for _, tool := range calls {
		agent := byTool[tool]
		if agent == "" {
			agent = t.ByTool[tool]
		}
		if agent == "" {
			continue
		}
		m := t.Metas[agent]
		f := t.FactsOf(agent)
		var res *ccsubagents.ParentResult
		if r, ok := t.Results[tool]; ok {
			res = &r
		}
		var note *ccsubagents.ParentNotice
		if n, ok := t.Notices[tool]; ok {
			note = &n
		}
		out = append(out, mirror.Subagent{Agent: agent, ToolID: tool, Type: m.AgentType, Description: redact.Secrets(m.Description),
			Background: m.Shape == "background" || (res != nil && res.Async), State: ccsubagents.State(res, note, f, now),
			Started: f.Started, Updated: f.Updated, Last: f.Last})
	}
	return mirror.NormalizeSubagents(out)
}

// changed is the list to send now, when it differs from the last one sent (or
// none was sent yet); ok false otherwise.
func (t *subagentTracker) changed() ([]mirror.Subagent, bool) {
	list := t.scan()
	if t.once && slices.Equal(list, t.sent) {
		return nil, false
	}
	t.sent, t.once = list, true
	return list, true
}

// ReadSubagentTail is the recent steps of agent in the subagents directory of
// transcript, redacted and bounded for the wire.
func ReadSubagentTail(transcript, agent string) (mirror.SubagentTail, error) {
	if !mirror.ValidSubagentID(agent) {
		return mirror.SubagentTail{}, mirror.ErrBadSubagent
	}
	f, err := os.Open(filepath.Join(ccsubagents.Dir(transcript), "agent-"+agent+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mirror.SubagentTail{}, fmt.Errorf("this session has no subagent %s", agent)
		}
		return mirror.SubagentTail{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return mirror.SubagentTail{}, err
	}
	steps := ccsubagents.Steps(ccsubagents.ReadTailRows(f, st.Size()))
	return mirror.NormalizeSubagentTail(mirror.SubagentTail{Agent: agent, Steps: steps, More: st.Size() > ccsubagents.TailRead}), nil
}

// subagentReport is what this host advertises as Msg.SubagentReport.
func (h *hostSide) subagentReport() bool {
	_, ok := h.claudeBackend()
	return ok && !h.c.cfg.NoSubagents
}

// subagentTail answers a tail request: the steps of m.Agent in the subagents
// directory of the Claude Code session in m's window, resolved NOW.
//
// 🔴 ONLY A LISTED SESSION, and only the subagents of the transcript its pane
// runs — the same allowlist a watch passes; the agent id is a single path
// element (mirror.ValidSubagentID), so nothing outside that directory is read.
func (h *hostSide) subagentTail(m mirror.Msg) {
	reply := func(t *mirror.SubagentTail, err error) {
		out := mirror.Msg{Type: mirror.TypeSubagentTailed, ID: m.ID, Tail: t}
		if err != nil {
			out.Error = err.Error()
			h.c.log.Printf("muster-mirror: subagent steps in %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		}
		_ = h.send(out)
	}
	cb, ok := h.claudeBackend()
	if !ok {
		reply(nil, errors.New("this host cannot read Claude Code transcripts"))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	p, ok := h.claudePaneOf(cb, m)(ctx)
	if !ok {
		reply(nil, errNotClaudeNow)
		return
	}
	t, err := ReadSubagentTail(p.Transcript, m.Agent)
	if err != nil {
		reply(nil, err)
		return
	}
	reply(&t, nil)
}
