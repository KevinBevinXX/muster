package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// THE SUBAGENT CARDS (tunnel v2): what a mirrored Claude Code session's
// SUBAGENTS are doing, so the rendered view shows more than "Claude is
// working". At each Agent (or Task) call the view draws a card — the agent's
// description and type, running / done / failed, elapsed time and its latest
// step — and tapping it shows the agent's own recent steps.
//
// Two shapes on the wire, both host → muster:
//
//   - SUMMARIES ride the transcript stream they belong to: an events message
//     with SubagentList set carries Subagents, the WHOLE list for that view
//     (the newest MaxSubagents), sent when it changed. The host sets
//     SubagentList on the Reset batch of every backfill too (even with an
//     empty list) — that is how muster knows the host reports subagents at
//     all, and so whether to draw Agent calls as cards.
//   - A TAIL is a request, not a field: muster asks for ONE agent's recent
//     steps when a card is opened (TypeSubagentTail), and the host answers that
//     request alone (TypeSubagentTailed), bounded by MaxSubagentTailBytes.
//
// 🔴 NOT IN THE WINDOW LIST. That list is one message whose worst case already
// measures 96% of MaxFrame (TestTheLargestWindowListFitsOneFrame); a summary
// per subagent does not fit beside it, and it is wanted only while somebody
// has the view open — which is exactly when its transcript stream exists.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like Msg.CatalogReport:
//   - an older host sends no SubagentList, so muster draws every Agent call as
//     the plain tool row it always was, and never asks it for a tail (its hello
//     has no SubagentReport);
//   - an older muster ignores the fields: a summaries-only message relays as an
//     empty batch at an offset it already had (as a prompt index does), and it
//     never sends a tail request.
//
// 🔴 EVERY STRING IN IT IS TRANSCRIPT TEXT, REDACTED ON THE HOST. The host runs
// internal/redact over every line it sends here (a step's text, a tool's
// one-line target) before it leaves the machine; NormalizeSubagents and
// NormalizeSubagentTail — applied by BOTH halves — then drop control
// characters and bound every field, and the page draws them as text.

const (
	// TypeSubagentTail (muster → host): send the recent steps of subagent
	// Msg.Agent of Session/Window's Claude Code session, by ID.
	TypeSubagentTail = "subagent-tail"
	// TypeSubagentTailed (host → muster): the answer to tail request ID —
	// Msg.Tail, or Msg.Error.
	TypeSubagentTailed = "subagent-tailed"
)

// The subagent bounds. Each is a property of the wire, so both halves read it
// here.
const (
	// MaxSubagents bounds one view's summary list: the newest (by their Agent
	// call's place in the transcript) are kept.
	MaxSubagents = 64
	// MaxSubagentText bounds a summary's description, type and latest step, and
	// a tail step's text, in RUNES (CleanText).
	MaxSubagentText = 200
	// MaxSubagentTailSteps bounds a tail; the newest are kept.
	MaxSubagentTailSteps = 20
	// MaxSubagentListBytes and MaxSubagentTailBytes bound one message ENCODED,
	// for the reason MaxBatchBytes gives. TestTheLargestSubagentMessagesFitOneFrame
	// measures both at their field bounds.
	MaxSubagentListBytes = 256 << 10
	MaxSubagentTailBytes = 64 << 10
)

// subagentTools are the tool names whose calls spawn a subagent: "Agent"
// (Claude Code 2.1), and "Task", its earlier name.
var subagentTools = map[string]bool{"Agent": true, "Task": true}

// IsSubagentTool reports whether a tool call of this name spawns a subagent.
// Both halves read it: the host to track the calls, muster to draw them as
// cards.
func IsSubagentTool(name string) bool { return subagentTools[name] }

// The values of Subagent.State.
const (
	SubagentRunning = "running"
	SubagentDone    = "done"
	SubagentFailed  = "failed"
)

// Subagent is one subagent's summary (Msg.Subagents).
type Subagent struct {
	// Agent is the subagent's id (its transcript is agent-<Agent>.jsonl).
	Agent string `json:"agent"`
	// ToolID is the parent's Agent tool_use id: the card it belongs to.
	ToolID string `json:"toolId"`
	// Type is the agent type ("general-purpose"), Description the call's
	// description: from the agent's meta file, "" when it has none.
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	// Background: launched to run in the background.
	Background bool `json:"background,omitempty"`
	// State is SubagentRunning, SubagentDone or SubagentFailed (the host's
	// rule: internal/mirrorhost's subagentState).
	State string `json:"state"`
	// Started and Updated are the agent's first and last row (Unix ms, 0 =
	// unknown): its elapsed time is Updated-Started once it ended, now-Started
	// while it runs.
	Started int64 `json:"started,omitempty"`
	Updated int64 `json:"updated,omitempty"`
	// Last is the agent's latest step on one line: a tool call ("Bash: go
	// test ./...") or its latest text. Redacted.
	Last string `json:"last,omitempty"`
}

// SubagentStep is one step of a tail.
type SubagentStep struct {
	// Tool is the tool a call step ran; "" for a text step.
	Tool string `json:"tool,omitempty"`
	// Text is the call's one-line target, or the text's first line. Redacted.
	Text string `json:"text"`
	At   int64  `json:"at,omitempty"`
}

// SubagentTail is a tail request's answer: the newest steps, oldest first, and
// whether earlier ones were left out.
type SubagentTail struct {
	Agent string         `json:"agent"`
	Steps []SubagentStep `json:"steps"`
	More  bool           `json:"more,omitempty"`
}

// subagentIDPattern is a subagent id or a tool_use id as the wire carries one:
// the CLI's are hex (a3f…) and toolu_… — bounded so a summary's worst case is
// known, and safe as one path element on the host.
var subagentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidSubagentID reports whether id is a subagent (or tool_use) id the wire
// carries.
func ValidSubagentID(id string) bool { return subagentIDPattern.MatchString(id) }

// NormalizeSubagents is in as the wire carries it: entries with a valid agent
// and tool id, one per tool id (the first), a known state (else running), text
// fields cleaned and bounded, no time before 1970, at most MaxSubagents (the
// LAST ones, which the host orders oldest first) and within
// MaxSubagentListBytes encoded. Both halves apply it.
func NormalizeSubagents(in []Subagent) []Subagent {
	out := make([]Subagent, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if !ValidSubagentID(s.Agent) || !ValidSubagentID(s.ToolID) || seen[s.ToolID] {
			continue
		}
		seen[s.ToolID] = true
		switch s.State {
		case SubagentRunning, SubagentDone, SubagentFailed:
		default:
			s.State = SubagentRunning
		}
		s.Type = CleanText(s.Type, MaxSubagentText)
		s.Description = CleanText(s.Description, MaxSubagentText)
		s.Last = CleanText(s.Last, MaxSubagentText)
		s.Started, s.Updated = max(s.Started, 0), max(s.Updated, 0)
		out = append(out, s)
	}
	if len(out) > MaxSubagents {
		out = out[len(out)-MaxSubagents:]
	}
	size, keep := 0, 0
	for i := len(out) - 1; i >= 0; i-- {
		b, _ := json.Marshal(out[i])
		if size+len(b)+1 > MaxSubagentListBytes {
			break
		}
		size += len(b) + 1
		keep++
	}
	return out[len(out)-keep:]
}

// NormalizeSubagentTail is t as the wire carries it: its steps cleaned and
// bounded, the newest MaxSubagentTailSteps within MaxSubagentTailBytes (More
// set when any were dropped). Both halves apply it.
func NormalizeSubagentTail(t SubagentTail) SubagentTail {
	steps := make([]SubagentStep, 0, len(t.Steps))
	for _, s := range t.Steps {
		s.Tool = CleanText(s.Tool, MaxCatalogName)
		s.Text = CleanText(s.Text, MaxSubagentText)
		s.At = max(s.At, 0)
		if s.Tool == "" && s.Text == "" {
			continue
		}
		steps = append(steps, s)
	}
	more := t.More
	if len(steps) > MaxSubagentTailSteps {
		steps, more = steps[len(steps)-MaxSubagentTailSteps:], true
	}
	size, keep := 0, 0
	for i := len(steps) - 1; i >= 0; i-- {
		b, _ := json.Marshal(steps[i])
		if size+len(b)+1 > MaxSubagentTailBytes {
			more = true
			break
		}
		size += len(b) + 1
		keep++
	}
	return SubagentTail{Agent: t.Agent, Steps: steps[len(steps)-keep:], More: more}
}

var (
	// ErrSubagentsUnsupported: the host's muster-mirror predates the subagent
	// cards (no Msg.SubagentReport in its hello).
	ErrSubagentsUnsupported = errors.New("mirror: this host's muster-mirror does not report subagents; upgrade it")
	// ErrSubagentTailRefused wraps the host's own reason for refusing a tail.
	ErrSubagentTailRefused = errors.New("mirror: the host refused the subagent's steps")
	// ErrSubagentTailTimeout: the host did not answer in time.
	ErrSubagentTailTimeout = errors.New("mirror: the host did not send the subagent's steps in time")
	// ErrBadSubagent: the agent id is not one the wire carries.
	ErrBadSubagent = errors.New("mirror: not a subagent id")
)

// subagentTailTimeout bounds how long a tail request waits for the host: one
// bounded read of the end of one file.
const subagentTailTimeout = 5 * time.Second

// tailAnswer is a host's subagent-tailed message, as the waiting request gets
// it.
type tailAnswer struct {
	t   SubagentTail
	err error
}

// SubagentTail asks owner's host for the recent steps of subagent agent of the
// Claude Code session in (session, window) — window "" being the session's
// active window — and waits for the answer.
//
// 🔴 owner IS THE VIEWING PRINCIPAL'S USER: the lookup is keyed by (owner,
// host), so another owner's host answers ErrHostOffline exactly as one that
// does not exist. The host resolves the window's Claude pane NOW and reads only
// that session's own subagents directory.
func (h *Hub) SubagentTail(ctx context.Context, owner int64, host, session, window, agent string) (SubagentTail, error) {
	if !ValidSubagentID(agent) {
		return SubagentTail{}, ErrBadSubagent
	}
	hc, windowID, err := h.lookupClaude(owner, host, session, window)
	if err != nil {
		return SubagentTail{}, err
	}
	if !hc.subagentReport {
		return SubagentTail{}, fmt.Errorf("%w (host %q)", ErrSubagentsUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan tailAnswer, 1)
	hc.tails[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.tails, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeSubagentTail, ID: id, Session: session, Window: windowID, Agent: agent}); err != nil {
		return SubagentTail{}, fmt.Errorf("mirror: ask the host for the subagent's steps: %w", err)
	}
	t := time.NewTimer(subagentTailTimeout)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.t, a.err
	case <-ctx.Done():
		return SubagentTail{}, ctx.Err()
	case <-t.C:
		return SubagentTail{}, ErrSubagentTailTimeout
	}
}

// subagentTailed hands the host's answer to the request waiting on m.ID — the
// tail normalised again here, whatever the host did.
func (hc *hostConn) subagentTailed(m Msg) {
	hc.mu.Lock()
	ch := hc.tails[m.ID]
	delete(hc.tails, m.ID)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != "" {
		ch <- tailAnswer{err: fmt.Errorf("%w: %s", ErrSubagentTailRefused, CleanText(m.Error, 200))}
		return
	}
	var t SubagentTail
	if m.Tail != nil {
		t = *m.Tail
	}
	ch <- tailAnswer{t: NormalizeSubagentTail(t)}
}

// SubagentTail is Hub.SubagentTail for the viewing principal (ctx's). No
// step-up: it reads, and types nothing.
func (s *Service) SubagentTail(ctx context.Context, host, session, window, agent string) (SubagentTail, error) {
	o, err := owner(ctx)
	if err != nil {
		return SubagentTail{}, err
	}
	return s.hub.SubagentTail(ctx, o, host, session, window, agent)
}
