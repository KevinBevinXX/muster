package ccsubagents

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/redact"
)

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

// Quiet is how long a subagent whose last row is a final reply must go
// without a new row before it is called done with no word from the parent
// (rule 3 of State).
const Quiet = 15 * time.Second

// TailRead bounds how much of the END of a subagent's transcript one summary
// or tail reads. A tool result can be a whole file, so a few rows can be this
// large; the newest step is what matters.
const TailRead = 256 << 10

// headRead bounds the read of a transcript's FIRST line (its start time). A
// first row longer than this leaves the start unknown.
const headRead = 64 << 10

// MaxTrackedCalls bounds the Agent calls one tracker remembers.
const MaxTrackedCalls = 4 * MaxSubagents

// Dir is the subagents directory of the session whose transcript is
// transcript: <dir>/<session-id>/subagents.
func Dir(transcript string) string {
	return filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
}

// ParentResult is the parent's tool_result for an Agent call.
type ParentResult struct {
	At    int64
	OK    bool
	Async bool
}

// ParentNotice is the parent's latest task notification for an Agent call.
type ParentNotice struct {
	At     int64
	Status string
}

// Facts is what a subagent's own transcript says.
type Facts struct {
	Exists  bool
	Mtime   time.Time
	Started int64
	Updated int64
	// Final: its last message row is an assistant reply with text and no tool
	// call — the agent stopped, unless more rows follow.
	Final bool
	Last  string
}

// State is THE STATUS RULE, in this order:
//
//  1. The parent's tool_result for the Agent call decides — failed when it is
//     an error, else done — unless it is only a background agent's LAUNCH
//     (async), which says nothing about the agent finishing.
//  2. Else the parent's latest task notification for it decides — "completed"
//     (or no status) is done, any other ("failed", "killed", "stopped") failed.
//  3. Else the agent is done when its last row is a final reply (text, no tool
//     call) and its transcript has had no new row for Quiet.
//  4. Else it is running.
//
// 🔴 A PARENT SIGNAL OLDER THAN THE AGENT'S LAST ROW DECIDES NOTHING (1 and 2):
// a finished agent can be resumed (SendMessage), and its new rows then come
// after the notification that said it finished. Both times are the CLI's own
// timestamps; a signal or a row with none is taken as current.
func State(res *ParentResult, note *ParentNotice, f Facts, now time.Time) string {
	current := func(at int64) bool { return at == 0 || f.Updated == 0 || at >= f.Updated }
	if res != nil && !res.Async && current(res.At) {
		if !res.OK {
			return Failed
		}
		return Done
	}
	if note != nil && current(note.At) {
		switch note.Status {
		case "", "completed", "success":
			return Done
		}
		return Failed
	}
	if f.Exists && f.Final && now.Sub(f.Mtime) >= Quiet {
		return Done
	}
	return Running
}

// Meta is what a meta file says.
type Meta struct {
	AgentType, Description, ToolUseID, Shape, Parent string
}

// ReadMeta reads one meta file; ok is false when it cannot be read or names
// no toolUseId. Each field is read by name and only when it is a string.
func ReadMeta(path string) (Meta, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 64<<10 {
		return Meta{}, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return Meta{}, false
	}
	str := func(k string) string {
		var s string
		if json.Unmarshal(m[k], &s) != nil {
			return ""
		}
		return s
	}
	out := Meta{AgentType: str("agentType"), Description: str("description"), ToolUseID: str("toolUseId"),
		Shape: str("requestShape"), Parent: str("parentAgentId")}
	return out, out.ToolUseID != ""
}

// readHead is the first line of path, when it is whole within headRead.
func readHead(f *os.File) []byte {
	buf := make([]byte, headRead)
	n, _ := f.ReadAt(buf, 0)
	buf = buf[:n]
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		return buf[:i]
	}
	return nil
}

// ReadTailRows is every WHOLE line in the last TailRead bytes of f (size
// bytes long): the first, partial line of the window and a torn last line (no
// newline yet) are left out.
func ReadTailRows(f *os.File, size int64) [][]byte {
	from := max(size-TailRead, 0)
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

// Steps is what rows contribute as steps, oldest first: each tool call (its
// tool and one-line target) and each reply text (its first line), with what
// looks like a secret redacted. A subagent's prompt, its tool results and its
// thinking are not steps.
func Steps(rows [][]byte) []Step {
	var out []Step
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
				out = append(out, Step{Tool: ev.Tool, Text: redact.Secrets(ev.Target), At: r.Time()})
			case cctranscript.KindText, cctranscript.KindError:
				out = append(out, Step{Text: redact.Secrets(firstLineOf(ev.Text)), At: r.Time()})
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

// StepLine is a step on one line: "Bash: go test" or the text.
func StepLine(s Step) string {
	if s.Tool == "" {
		return s.Text
	}
	if s.Text == "" {
		return s.Tool
	}
	return s.Tool + ": " + s.Text
}

// ReadFacts reads what path says about its agent (zero facts when it does not
// exist yet).
func ReadFacts(path string) Facts {
	f, err := os.Open(path)
	if err != nil {
		return Facts{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Facts{}
	}
	out := Facts{Exists: true, Mtime: st.ModTime()}
	if r, ok := cctranscript.DecodeLine(readHead(f)); ok {
		out.Started = r.Time()
	}
	rows := ReadTailRows(f, st.Size())
	for i := len(rows) - 1; i >= 0; i-- {
		r, ok := cctranscript.DecodeLine(rows[i])
		if !ok {
			continue
		}
		if out.Updated == 0 {
			out.Updated = r.Time()
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
			out.Final = text && !call
		}
		break
	}
	if steps := Steps(rows); len(steps) > 0 {
		out.Last = StepLine(steps[len(steps)-1])
	}
	return out
}

// Tracker is one parent transcript's subagents: the Agent calls and the
// parent's signals about them, as the reader observes its transcript
// (Observe), and what each agent's own files say (ReadMetas, FactsOf).
//
// 🔴 IN MEMORY, BOUNDED: it lives as long as its reader and is replaced when
// the reader's transcript is (a /clear, a restart).
type Tracker struct {
	Dir     string
	Calls   []string // Agent tool_use ids, in transcript order
	Results map[string]ParentResult
	Notices map[string]ParentNotice
	ByTool  map[string]string // tool_use id → agent id, from results
	Metas   map[string]Meta
	// facts caches each agent file's facts by its size and mtime.
	facts map[string]cachedFacts
}

type cachedFacts struct {
	size  int64
	mtime time.Time
	f     Facts
}

// NewTracker is a tracker for the session whose transcript is transcript.
func NewTracker(transcript string) *Tracker {
	return &Tracker{Dir: Dir(transcript), Results: map[string]ParentResult{}, Notices: map[string]ParentNotice{},
		ByTool: map[string]string{}, Metas: map[string]Meta{}, facts: map[string]cachedFacts{}}
}

// Observe takes the parent's events, in transcript order.
func (t *Tracker) Observe(events []cctranscript.Event) {
	for _, ev := range events {
		switch ev.Kind {
		case cctranscript.KindToolCall:
			if !IsSubagentTool(ev.Tool) || !ValidID(ev.ToolID) || slices.Contains(t.Calls, ev.ToolID) {
				continue
			}
			t.Calls = append(t.Calls, ev.ToolID)
			if len(t.Calls) > MaxTrackedCalls {
				gone := t.Calls[0]
				t.Calls = t.Calls[1:]
				delete(t.Results, gone)
				delete(t.Notices, gone)
				delete(t.ByTool, gone)
			}
		case cctranscript.KindToolResult:
			if !slices.Contains(t.Calls, ev.ToolID) {
				continue
			}
			t.Results[ev.ToolID] = ParentResult{At: ev.At, OK: ev.OK, Async: ev.Async}
			if ValidID(ev.Agent) {
				t.ByTool[ev.ToolID] = ev.Agent
			}
		case cctranscript.KindNotice:
			if ev.TaskToolID != "" && slices.Contains(t.Calls, ev.TaskToolID) {
				t.Notices[ev.TaskToolID] = ParentNotice{At: ev.At, Status: ev.Status}
			}
		}
	}
}

// ReadMetas reads every meta file in the directory not read yet. A meta, once
// written, does not change.
func (t *Tracker) ReadMetas() {
	ents, err := os.ReadDir(t.Dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
		if _, done := t.Metas[id]; done || !ValidID(id) {
			continue
		}
		if m, ok := ReadMeta(filepath.Join(t.Dir, name)); ok {
			t.Metas[id] = m
		}
	}
}

// FactsOf is agent's facts, re-read only when its file changed.
func (t *Tracker) FactsOf(agent string) Facts {
	path := filepath.Join(t.Dir, "agent-"+agent+".jsonl")
	st, err := os.Stat(path)
	if err != nil {
		delete(t.facts, agent)
		return Facts{}
	}
	if c, ok := t.facts[agent]; ok && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.f
	}
	f := ReadFacts(path)
	t.facts[agent] = cachedFacts{size: st.Size(), mtime: st.ModTime(), f: f}
	return f
}
