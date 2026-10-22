package ccsubagents

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// THE WORK LEDGER: how many of a parent transcript's subagents are running
// NOW — the subagent half of a card's live work count
// (docs/design/agent-cards.md (a)). It follows the parent transcript from a
// byte cursor and applies the subagent cards' own status rule (State), so a
// card's count and its rendered view's subagent list cannot disagree.
// muster-mirror (internal/mirrorhost) and ccd (cmd/ccd) both count with it.

// Ledger follows ONE parent transcript's Agent calls: the calls and the
// parent's signals (Tracker.Observe), when each call was made, and
// which were killed.
type Ledger struct {
	path   string
	offset int64
	// started is the CLI's startedAt (Unix ms): a call made before it belongs
	// to a previous process, which cannot be hosting it now.
	started int64
	tracker *Tracker
	callAt  map[string]int64
	closed  map[string]bool
}

func NewLedger(path string, started int64) *Ledger {
	return &Ledger{path: path, offset: -1, started: started, tracker: NewTracker(path),
		callAt: map[string]int64{}, closed: map[string]bool{}}
}

// Path is the transcript the ledger follows.
func (l *Ledger) Path() string { return l.path }

// Started is the CLI start time (Unix ms) the ledger was made for.
func (l *Ledger) Started() int64 { return l.started }

// Read takes every WHOLE line written since the last read. The first read —
// and any read that finds more than BackfillWindow new bytes — starts
// at most that far from the end, at a line boundary; a torn last line (the CLI
// mid-write) is left for the next read. A file that shrank is read again from
// its new end's window.
func (l *Ledger) Read() {
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	size := st.Size()
	from := l.offset
	skipPartial := false
	if from < 0 || from > size || size-from > BackfillWindow {
		from = max(size-BackfillWindow, 0)
		skipPartial = from > 0
	}
	buf := make([]byte, size-from)
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		if !skipPartial {
			l.offset = from
		}
		return
	}
	body := buf[:end]
	start := 0
	if skipPartial {
		i := bytes.IndexByte(body, '\n')
		if i < 0 {
			l.offset = from + int64(end) + 1
			return
		}
		start = i + 1
	}
	for _, line := range bytes.Split(body[start:], []byte("\n")) {
		l.observe(line)
	}
	l.offset = from + int64(end) + 1
}

// queueOp is a queue-operation record: how most task notifications reach the
// transcript (written when the task FINISHES, not when it is delivered).
type queueOp struct {
	Type      string `json:"type"`
	Operation string `json:"operation"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// taskStop is a TaskStop / KillShell result's toolUseResult.
type taskStop struct {
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
}

// observe takes one transcript line.
func (l *Ledger) observe(line []byte) {
	r, ok := cctranscript.DecodeLine(line)
	if !ok || r.IsSidechain {
		return
	}
	switch {
	case r.Type == "queue-operation":
		var q queueOp
		if json.Unmarshal(line, &q) != nil || q.Operation != "enqueue" || !strings.Contains(q.Content, "<task-notification>") {
			return
		}
		// The notification as the user record it is delivered as, so the
		// transcript decoder reads it exactly as it reads an inline one.
		content, _ := json.Marshal(q.Content)
		var n cctranscript.Record
		n.Type, n.Timestamp, n.Message.Content = "user", q.Timestamp, content
		l.tracker.Observe(cctranscript.Events(n))
		return
	case r.Type == "system" && r.Subtype == "agents_killed":
		// Every agent launched before it is gone.
		at := r.Time()
		for _, tool := range l.tracker.Calls {
			if c := l.callAt[tool]; at == 0 || c <= at {
				l.closed[tool] = true
			}
		}
		return
	}
	events := cctranscript.Events(r)
	for _, ev := range events {
		if ev.Kind == cctranscript.KindToolCall && IsSubagentTool(ev.Tool) && ValidID(ev.ToolID) {
			if _, seen := l.callAt[ev.ToolID]; !seen {
				l.callAt[ev.ToolID] = r.Time()
			}
		}
	}
	l.tracker.Observe(events)
	// A TaskStop that names an agent closes its call.
	if len(events) == 1 && events[0].Kind == cctranscript.KindToolResult && len(r.ToolUseResult) > 0 {
		var ts taskStop
		if json.Unmarshal(r.ToolUseResult, &ts) == nil && ts.TaskID != "" && ts.TaskType != "local_bash" {
			if tool := l.toolOf(ts.TaskID); tool != "" {
				l.closed[tool] = true
			}
		}
	}
	// The tracker forgets its oldest calls past its bound; so does the ledger.
	if len(l.callAt) > 2*MaxTrackedCalls {
		for tool := range l.callAt {
			if !containsString(l.tracker.Calls, tool) {
				delete(l.callAt, tool)
				delete(l.closed, tool)
			}
		}
	}
}

// toolOf is the Agent call whose agent is agent, "" when none is known.
func (l *Ledger) toolOf(agent string) string {
	for tool, a := range l.tracker.ByTool {
		if a == agent {
			return tool
		}
	}
	l.tracker.ReadMetas()
	if m, ok := l.tracker.Metas[agent]; ok && m.Parent == "" && containsString(l.tracker.Calls, m.ToolUseID) {
		return m.ToolUseID
	}
	return ""
}

// Running is how many of the ledger's calls are running now: not killed, not
// older than the CLI, and running by the subagent cards' state rule. A call
// whose agent is not known yet (no meta, no result) is running — the rule's
// own verdict for no signal and no file.
func (l *Ledger) Running(now time.Time) int {
	l.tracker.ReadMetas()
	byTool := map[string]string{}
	for id, m := range l.tracker.Metas {
		if m.Parent == "" {
			byTool[m.ToolUseID] = id
		}
	}
	n := 0
	for _, tool := range l.tracker.Calls {
		if l.closed[tool] {
			continue
		}
		if at := l.callAt[tool]; l.started > 0 && at > 0 && at < l.started {
			continue
		}
		agent := byTool[tool]
		if agent == "" {
			agent = l.tracker.ByTool[tool]
		}
		var f Facts
		if agent != "" {
			f = l.tracker.FactsOf(agent)
		}
		var res *ParentResult
		if r, ok := l.tracker.Results[tool]; ok {
			res = &r
		}
		var note *ParentNotice
		if x, ok := l.tracker.Notices[tool]; ok {
			note = &x
		}
		if State(res, note, f, now) == Running {
			n++
		}
	}
	return n
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
