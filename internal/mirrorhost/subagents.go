package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
)

// THE SUBAGENT CARDS' host half (internal/mirror's subagents.go): what each
// subagent an open rendered view's session spawned is doing, read from the
// session's own subagents directory and sent on that view's stream.
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

// SubagentQuiet is how long a subagent whose last row is a final reply must go
// without a new row before it is called done with no word from the parent
// (rule 3 of subagentState).
const SubagentQuiet = 15 * time.Second

// subagentTailRead bounds how much of the END of a subagent's transcript one
// summary or tail reads. A tool result can be a whole file, so a few rows can
// be this large; the newest step is what matters.
const subagentTailRead = 256 << 10

// subagentHeadRead bounds the read of a transcript's FIRST line (its start
// time). A first row longer than this leaves the start unknown.
const subagentHeadRead = 64 << 10

// maxTrackedCalls bounds the Agent calls one view remembers.
const maxTrackedCalls = 4 * mirror.MaxSubagents

// subagentsDir is the subagents directory of the session whose transcript is
// transcript: <dir>/<session-id>/subagents.
func subagentsDir(transcript string) string {
	return filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
}

// parentResult is the parent's tool_result for an Agent call.
type parentResult struct {
	at    int64
	ok    bool
	async bool
}

// parentNotice is the parent's latest task notification for an Agent call.
type parentNotice struct {
	at     int64
	status string
}

// subagentFacts is what a subagent's own transcript says.
type subagentFacts struct {
	exists  bool
	mtime   time.Time
	started int64
	updated int64
	// final: its last message row is an assistant reply with text and no tool
	// call — the agent stopped, unless more rows follow.
	final bool
	last  string
}

// subagentState is THE STATUS RULE, in this order:
//
//  1. The parent's tool_result for the Agent call decides — failed when it is
//     an error, else done — unless it is only a background agent's LAUNCH
//     (async), which says nothing about the agent finishing.
//  2. Else the parent's latest task notification for it decides — "completed"
//     (or no status) is done, any other ("failed", "killed", "stopped") failed.
//  3. Else the agent is done when its last row is a final reply (text, no tool
//     call) and its transcript has had no new row for SubagentQuiet.
//  4. Else it is running.
//
// 🔴 A PARENT SIGNAL OLDER THAN THE AGENT'S LAST ROW DECIDES NOTHING (1 and 2):
// a finished agent can be resumed (SendMessage), and its new rows then come
// after the notification that said it finished. Both times are the CLI's own
// timestamps; a signal or a row with none is taken as current.
func subagentState(res *parentResult, note *parentNotice, f subagentFacts, now time.Time) string {
	current := func(at int64) bool { return at == 0 || f.updated == 0 || at >= f.updated }
	if res != nil && !res.async && current(res.at) {
		if !res.ok {
			return mirror.SubagentFailed
		}
		return mirror.SubagentDone
	}
	if note != nil && current(note.at) {
		switch note.status {
		case "", "completed", "success":
			return mirror.SubagentDone
		}
		return mirror.SubagentFailed
	}
	if f.exists && f.final && now.Sub(f.mtime) >= SubagentQuiet {
		return mirror.SubagentDone
	}
	return mirror.SubagentRunning
}

// subagentMeta is what a meta file says.
type subagentMeta struct {
	agentType, description, toolUseID, shape, parent string
}

// readSubagentMeta reads one meta file; ok is false when it cannot be read or
// names no toolUseId. Each field is read by name and only when it is a string.
func readSubagentMeta(path string) (subagentMeta, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 64<<10 {
		return subagentMeta{}, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return subagentMeta{}, false
	}
	str := func(k string) string {
		var s string
		if json.Unmarshal(m[k], &s) != nil {
			return ""
		}
		return s
	}
	out := subagentMeta{agentType: str("agentType"), description: str("description"), toolUseID: str("toolUseId"),
		shape: str("requestShape"), parent: str("parentAgentId")}
	return out, out.toolUseID != ""
}

// readHead is the first line of path, when it is whole within subagentHeadRead.
func readHead(f *os.File) []byte {
	buf := make([]byte, subagentHeadRead)
	n, _ := f.ReadAt(buf, 0)
	buf = buf[:n]
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		return buf[:i]
	}
	return nil
}

// readTailRows is every WHOLE line in the last subagentTailRead bytes of f
// (size bytes long): the first, partial line of the window and a torn last
// line (no newline yet) are left out.
func readTailRows(f *os.File, size int64) [][]byte {
	from := max(size-subagentTailRead, 0)
	buf := make([]byte, size-from)
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil
	}
	buf = buf[:end]
	if from > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return nil
		}
		buf = buf[i+1:]
	}
	return bytes.Split(buf, []byte("\n"))
}

// subagentSteps is what rows contribute as steps, oldest first: each tool call
// (its tool and one-line target) and each reply text (its first line), with
// what looks like a secret redacted. A subagent's prompt, its tool results and
// its thinking are not steps.
func subagentSteps(rows [][]byte) []mirror.SubagentStep {
	var out []mirror.SubagentStep
	for _, row := range rows {
		r, ok := cctranscript.DecodeLine(row)
		if !ok || r.Type != "assistant" {
			continue
		}
		// Events drops a sidechain record — the parent's view must never show
		// one inline — and this IS the sidechain, read on purpose.
		r.IsSidechain = false
		for _, ev := range cctranscript.Events(r) {
			switch ev.Kind {
			case cctranscript.KindToolCall:
				out = append(out, mirror.SubagentStep{Tool: ev.Tool, Text: redact.Secrets(ev.Target), At: r.Time()})
			case cctranscript.KindText, cctranscript.KindError:
				out = append(out, mirror.SubagentStep{Text: redact.Secrets(firstLineOf(ev.Text)), At: r.Time()})
			}
		}
	}
	return out
}

// firstLineOf is s's first non-blank line.
func firstLineOf(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// stepLine is a step on one line: "Bash: go test" or the text.
func stepLine(s mirror.SubagentStep) string {
	if s.Tool == "" {
		return s.Text
	}
	if s.Text == "" {
		return s.Tool
	}
	return s.Tool + ": " + s.Text
}

// readSubagentFacts reads what path says about its agent (zero facts when it
// does not exist yet).
func readSubagentFacts(path string) subagentFacts {
	f, err := os.Open(path)
	if err != nil {
		return subagentFacts{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return subagentFacts{}
	}
	out := subagentFacts{exists: true, mtime: st.ModTime()}
	if r, ok := cctranscript.DecodeLine(readHead(f)); ok {
		out.started = r.Time()
	}
	rows := readTailRows(f, st.Size())
	for i := len(rows) - 1; i >= 0; i-- {
		r, ok := cctranscript.DecodeLine(rows[i])
		if !ok {
			continue
		}
		if out.updated == 0 {
			out.updated = r.Time()
		}
		if r.Type != "user" && r.Type != "assistant" {
			continue
		}
		// The last MESSAGE row: a reply with text and no tool call is final.
		if r.Type == "assistant" {
			text, call := false, false
			for _, b := range r.Blocks() {
				switch b.Type {
				case "text":
					text = text || strings.TrimSpace(b.Text) != ""
				case "tool_use":
					call = true
				}
			}
			out.final = text && !call
		}
		break
	}
	if steps := subagentSteps(rows); len(steps) > 0 {
		out.last = stepLine(steps[len(steps)-1])
	}
	return out
}

// subagentTracker is one view's subagents: the Agent calls and the parent's
// signals about them, as the view's stream reads its transcript (observe), and
// what each agent's own files say (scan).
//
// 🔴 IN MEMORY, PER VIEW, BOUNDED: it lives as long as the view's stream and is
// replaced when the view's transcript is (a /clear, a restart).
type subagentTracker struct {
	dir     string
	calls   []string // Agent tool_use ids, in transcript order
	results map[string]parentResult
	notices map[string]parentNotice
	byTool  map[string]string // tool_use id → agent id, from results
	metas   map[string]subagentMeta
	// facts caches each agent file's facts by its size and mtime.
	facts map[string]cachedFacts
	sent  []mirror.Subagent
	once  bool // a list was sent
	now   func() time.Time
}

type cachedFacts struct {
	size  int64
	mtime time.Time
	f     subagentFacts
}

func newSubagentTracker(transcript string) *subagentTracker {
	return &subagentTracker{dir: subagentsDir(transcript), results: map[string]parentResult{}, notices: map[string]parentNotice{},
		byTool: map[string]string{}, metas: map[string]subagentMeta{}, facts: map[string]cachedFacts{}, now: time.Now}
}

// observe takes the parent's events, in transcript order.
func (t *subagentTracker) observe(events []cctranscript.Event) {
	for _, ev := range events {
		switch ev.Kind {
		case cctranscript.KindToolCall:
			if !mirror.IsSubagentTool(ev.Tool) || !mirror.ValidSubagentID(ev.ToolID) || slices.Contains(t.calls, ev.ToolID) {
				continue
			}
			t.calls = append(t.calls, ev.ToolID)
			if len(t.calls) > maxTrackedCalls {
				gone := t.calls[0]
				t.calls = t.calls[1:]
				delete(t.results, gone)
				delete(t.notices, gone)
				delete(t.byTool, gone)
			}
		case cctranscript.KindToolResult:
			if !slices.Contains(t.calls, ev.ToolID) {
				continue
			}
			t.results[ev.ToolID] = parentResult{at: ev.At, ok: ev.OK, async: ev.Async}
			if mirror.ValidSubagentID(ev.Agent) {
				t.byTool[ev.ToolID] = ev.Agent
			}
		case cctranscript.KindNotice:
			if ev.TaskToolID != "" && slices.Contains(t.calls, ev.TaskToolID) {
				t.notices[ev.TaskToolID] = parentNotice{at: ev.At, status: ev.Status}
			}
		}
	}
}

// readMetas reads every meta file in the directory not read yet. A meta, once
// written, does not change.
func (t *subagentTracker) readMetas() {
	ents, err := os.ReadDir(t.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
		if _, done := t.metas[id]; done || !mirror.ValidSubagentID(id) {
			continue
		}
		if m, ok := readSubagentMeta(filepath.Join(t.dir, name)); ok {
			t.metas[id] = m
		}
	}
}

// factsOf is agent's facts, re-read only when its file changed.
func (t *subagentTracker) factsOf(agent string) subagentFacts {
	path := filepath.Join(t.dir, "agent-"+agent+".jsonl")
	st, err := os.Stat(path)
	if err != nil {
		delete(t.facts, agent)
		return subagentFacts{}
	}
	if c, ok := t.facts[agent]; ok && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.f
	}
	f := readSubagentFacts(path)
	t.facts[agent] = cachedFacts{size: st.Size(), mtime: st.ModTime(), f: f}
	return f
}

// scan is every tracked Agent call's summary that has an agent to describe,
// oldest call first, normalised for the wire.
func (t *subagentTracker) scan() []mirror.Subagent {
	if len(t.calls) == 0 {
		return []mirror.Subagent{}
	}
	t.readMetas()
	byTool := map[string]string{}
	for id, m := range t.metas {
		if m.parent == "" {
			byTool[m.toolUseID] = id
		}
	}
	now := t.now()
	calls := t.calls
	if len(calls) > mirror.MaxSubagents {
		calls = calls[len(calls)-mirror.MaxSubagents:]
	}
	out := make([]mirror.Subagent, 0, len(calls))
	for _, tool := range calls {
		agent := byTool[tool]
		if agent == "" {
			agent = t.byTool[tool]
		}
		if agent == "" {
			continue
		}
		m := t.metas[agent]
		f := t.factsOf(agent)
		var res *parentResult
		if r, ok := t.results[tool]; ok {
			res = &r
		}
		var note *parentNotice
		if n, ok := t.notices[tool]; ok {
			note = &n
		}
		out = append(out, mirror.Subagent{Agent: agent, ToolID: tool, Type: m.agentType, Description: redact.Secrets(m.description),
			Background: m.shape == "background" || (res != nil && res.async), State: subagentState(res, note, f, now),
			Started: f.started, Updated: f.updated, Last: f.last})
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
	f, err := os.Open(filepath.Join(subagentsDir(transcript), "agent-"+agent+".jsonl"))
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
	steps := subagentSteps(readTailRows(f, st.Size()))
	return mirror.NormalizeSubagentTail(mirror.SubagentTail{Agent: agent, Steps: steps, More: st.Size() > subagentTailRead}), nil
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
