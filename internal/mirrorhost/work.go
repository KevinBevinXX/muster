package mirrorhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE LIVE WORK COUNTS' host half (docs/design/agent-cards.md (a)): per
// Claude Code window, how many subagents and background shells its CLI has
// running now, sent on a work message of its own (mirror/work.go).
//
// The shells come from the CLI's process tree (internal/cctasks, read by
// Tmux.Windows); the rule that combines them with the CLI's own status is
// cctasks.Decide. What this file adds is the SUBAGENT count while the CLI is
// busy: a ledger of the parent transcript's Agent calls, followed from a byte
// cursor, applying the subagent cards' own state rule (subagentState) — the
// same rule, so a card and its rendered view's subagent list cannot disagree.
//
// 🔴 THE LEDGER IS READ ONLY WHILE THE STATUS IS busy OR waiting. An idle
// window costs no transcript read: the CLI's own "idle"/"shell" already says
// no subagent runs.

// mayRunWork is whether a CLI whose raw status is raw may have shells or
// subagents running — what makes reading its process tree worth it.
func mayRunWork(raw string) bool {
	switch raw {
	case cctasks.StatusShell, cctasks.StatusBusy, cctasks.StatusWaiting:
		return true
	}
	return false
}

// workLedger follows ONE parent transcript's Agent calls: the calls and the
// parent's signals (subagentTracker.observe), when each call was made, and
// which were killed.
type workLedger struct {
	path   string
	offset int64
	// started is the CLI's startedAt (Unix ms): a call made before it belongs
	// to a previous process, which cannot be hosting it now.
	started int64
	tracker *subagentTracker
	callAt  map[string]int64
	closed  map[string]bool
}

func newWorkLedger(path string, started int64) *workLedger {
	return &workLedger{path: path, offset: -1, started: started, tracker: newSubagentTracker(path),
		callAt: map[string]int64{}, closed: map[string]bool{}}
}

// read takes every WHOLE line written since the last read. The first read —
// and any read that finds more than mirror.BackfillWindow new bytes — starts
// at most that far from the end, at a line boundary; a torn last line (the CLI
// mid-write) is left for the next read. A file that shrank is read again from
// its new end's window.
func (l *workLedger) read() {
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
	if from < 0 || from > size || size-from > mirror.BackfillWindow {
		from = max(size-mirror.BackfillWindow, 0)
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
func (l *workLedger) observe(line []byte) {
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
		l.tracker.observe(cctranscript.Events(n))
		return
	case r.Type == "system" && r.Subtype == "agents_killed":
		// Every agent launched before it is gone.
		at := r.Time()
		for _, tool := range l.tracker.calls {
			if c := l.callAt[tool]; at == 0 || c <= at {
				l.closed[tool] = true
			}
		}
		return
	}
	events := cctranscript.Events(r)
	for _, ev := range events {
		if ev.Kind == cctranscript.KindToolCall && mirror.IsSubagentTool(ev.Tool) && mirror.ValidSubagentID(ev.ToolID) {
			if _, seen := l.callAt[ev.ToolID]; !seen {
				l.callAt[ev.ToolID] = r.Time()
			}
		}
	}
	l.tracker.observe(events)
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
	if len(l.callAt) > 2*maxTrackedCalls {
		for tool := range l.callAt {
			if !containsString(l.tracker.calls, tool) {
				delete(l.callAt, tool)
				delete(l.closed, tool)
			}
		}
	}
}

// toolOf is the Agent call whose agent is agent, "" when none is known.
func (l *workLedger) toolOf(agent string) string {
	for tool, a := range l.tracker.byTool {
		if a == agent {
			return tool
		}
	}
	l.tracker.readMetas()
	if m, ok := l.tracker.metas[agent]; ok && m.parent == "" && containsString(l.tracker.calls, m.toolUseID) {
		return m.toolUseID
	}
	return ""
}

// running is how many of the ledger's calls are running now: not killed, not
// older than the CLI, and running by the subagent cards' state rule. A call
// whose agent is not known yet (no meta, no result) is running — the rule's
// own verdict for no signal and no file.
func (l *workLedger) running(now time.Time) int {
	l.tracker.readMetas()
	byTool := map[string]string{}
	for id, m := range l.tracker.metas {
		if m.parent == "" {
			byTool[m.toolUseID] = id
		}
	}
	n := 0
	for _, tool := range l.tracker.calls {
		if l.closed[tool] {
			continue
		}
		if at := l.callAt[tool]; l.started > 0 && at > 0 && at < l.started {
			continue
		}
		agent := byTool[tool]
		if agent == "" {
			agent = l.tracker.byTool[tool]
		}
		var f subagentFacts
		if agent != "" {
			f = l.tracker.factsOf(agent)
		}
		var res *parentResult
		if r, ok := l.tracker.results[tool]; ok {
			res = &r
		}
		var note *parentNotice
		if x, ok := l.tracker.notices[tool]; ok {
			note = &x
		}
		if subagentState(res, note, f, now) == mirror.SubagentRunning {
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

// workCounter is the client's ledgers, one per Claude Code session (Gen) the
// listing shows: a session that is no longer listed — a /clear, a restart, a
// closed window — drops its ledger, and a new one starts from the backfill
// window.
type workCounter struct {
	mu      sync.Mutex
	ledgers map[string]*workLedger
	// now is time.Now; a test moves it.
	now func() time.Time
}

// windowWork is the work map for windows (each window's Claude pane, by window
// id): cctasks.Decide over each pane's raw status, its shells and — read only
// when the status needs it — its ledger's running count. A window with no
// Claude pane, or no status, has no entry.
func (w *workCounter) windowWork(windows []WindowInfo) map[string]mirror.WindowWork {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ledgers == nil {
		w.ledgers = map[string]*workLedger{}
	}
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	seen := map[string]bool{}
	out := map[string]mirror.WindowWork{}
	for _, win := range windows {
		c := win.Claude
		if c == nil {
			continue
		}
		if c.Gen != "" {
			seen[c.Gen] = true
		}
		work, ok := cctasks.Decide(c.RawStatus, c.Shells, func() int {
			if c.Gen == "" || c.Transcript == "" {
				return 0
			}
			l := w.ledgers[c.Gen]
			if l == nil || l.path != c.Transcript || l.started != c.StartedAt {
				l = newWorkLedger(c.Transcript, c.StartedAt)
				w.ledgers[c.Gen] = l
			}
			l.read()
			return l.running(now())
		})
		if !ok {
			continue
		}
		out[win.ID] = mirror.WindowWork{Subagents: work.Subagents, Shells: work.Shells, ShellsAtLeast: work.ShellsAtLeast}
	}
	for gen := range w.ledgers {
		if !seen[gen] {
			delete(w.ledgers, gen)
		}
	}
	return out
}

// windowColors is each window's identity colour by window id (WindowInfo.Color),
// for every window that has one.
func windowColors(windows []WindowInfo) map[string]string {
	out := map[string]string{}
	for _, w := range windows {
		if w.Color != "" {
			out[w.ID] = w.Color
		}
	}
	return out
}
