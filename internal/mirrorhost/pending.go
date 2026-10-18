package mirrorhost

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// WHAT A WAITING SESSION WAITS ON (mirror.Pending): read here, on the host,
// from the session's own transcript — muster holds no transcript and no pending
// tool call, so this is the only place it can be derived.
//
// When a window's Claude Code session reports StatusWaiting, the host reads the
// tail of its transcript and sends, beside the status, a one-line summary of the
// last tool call that has no result yet (PendingOf, summarizeTool) and the last
// thing Claude said. Only for a waiting window, only the transcript's tail
// (pendingWindow), and only again when the file changed (pendingReader's cache).

// pendingWindow is how much of a transcript's tail is read for its pending
// call: the call is the newest record but one or two.
const pendingWindow = 256 << 10

// maxPathRunes bounds a path in a summary; a longer one keeps its END (the file
// name), which is the part a reader recognises.
const maxPathRunes = 100

// PendingOf is what a waiting session waits on, from its transcript's events
// (oldest first): the last tool call AFTER THE LAST PROMPT that has no result
// (an earlier one was interrupted, not pending), summarised, and the last text
// Claude wrote, each with what looks like a secret redacted (redactSecrets),
// and the call's tool as a name (toolName). nil when there is neither. All are
// cleaned and capped (mirror.CleanPending).
func PendingOf(events []cctranscript.Event) *mirror.Pending {
	start := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == cctranscript.KindPrompt {
			start = i + 1
			break
		}
	}
	answered := map[string]bool{}
	for _, ev := range events[start:] {
		if ev.Kind == cctranscript.KindToolResult {
			answered[ev.ToolID] = true
		}
	}
	var p mirror.Pending
	for i := len(events) - 1; i >= start; i-- {
		if ev := events[i]; ev.Kind == cctranscript.KindToolCall && !answered[ev.ToolID] {
			p.Summary, p.Tool = summarizeTool(ev.Tool, ev.Input, ev.Ask), toolName(ev.Tool)
			break
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == cctranscript.KindText {
			p.Last = redactSecrets(events[i].Text)
			break
		}
	}
	return mirror.CleanPending(&p)
}

// toolName is a tool as a notification may name it: its name, an MCP tool's
// "MCP server/tool" — never any of its input.
func toolName(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		if server, name, ok := strings.Cut(rest, "__"); ok && server != "" && name != "" {
			return "MCP " + server + "/" + name
		}
	}
	return tool
}

// summarizeTool is one tool call as a pending action: the command a Bash runs
// (its first line), the path a file tool touches, the host a WebFetch reaches,
// an MCP tool's server/tool, the question an AskUserQuestion asks — else the
// tool's name — with what looks like a secret replaced by «redacted»
// (redactSecrets) BEFORE it leaves the host. Never more of the input than that.
func summarizeTool(tool, input string, ask *cctranscript.Ask) string {
	return redactSecrets(summarizeRaw(tool, input, ask))
}

func summarizeRaw(tool, input string, ask *cctranscript.Ask) string {
	str := func(key string) string {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(input), &m) != nil {
			return ""
		}
		var s string
		if json.Unmarshal(m[key], &s) != nil {
			return ""
		}
		return s
	}
	switch tool {
	case "Bash":
		if c := firstNonBlankLine(str("command")); c != "" {
			return "Bash: " + c
		}
	case "Read", "Write", "Edit", "MultiEdit":
		if p := str("file_path"); p != "" {
			return tool + " " + tailRunes(p, maxPathRunes)
		}
	case "NotebookEdit":
		if p := str("notebook_path"); p != "" {
			return tool + " " + tailRunes(p, maxPathRunes)
		}
	case "WebFetch":
		if u, err := url.Parse(strings.TrimSpace(str("url"))); err == nil && u.Hostname() != "" {
			return "WebFetch " + u.Hostname()
		}
	case askq.ToolName:
		if ask != nil && len(ask.Questions) > 0 {
			return firstNonBlankLine(ask.Questions[0].Question)
		}
		if qs, err := askq.ParseInput(json.RawMessage(input)); err == nil && len(qs) > 0 {
			return firstNonBlankLine(qs[0].Question)
		}
	}
	return toolName(tool)
}

// firstNonBlankLine is s's first line with anything on it, trimmed.
func firstNonBlankLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// tailRunes is s's last n runes, a cut marked "…" in front.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-(n-1):])
}

// pendingReader reads PendingOf for transcripts, re-reading one only when its
// size or modification time moved. Safe for concurrent use; the zero value is
// ready.
type pendingReader struct {
	mu    sync.Mutex
	cache map[string]pendingEntry
}

type pendingEntry struct {
	size int64
	mod  time.Time
	p    *mirror.Pending
}

// of is PendingOf for the transcript at path; nil when it cannot be read.
func (r *pendingReader) of(path string) *mirror.Pending {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[path]; ok && e.size == st.Size() && e.mod.Equal(st.ModTime()) {
		r.mu.Unlock()
		return e.p
	}
	r.mu.Unlock()
	lines, _, err := cctranscript.Backfill(f, st.Size(), pendingWindow, mirror.MaxBackfillEvents)
	if err != nil {
		return nil
	}
	p := PendingOf(cctranscript.Flatten(lines))
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]pendingEntry{}
	}
	r.cache[path] = pendingEntry{size: st.Size(), mod: st.ModTime(), p: p}
	r.mu.Unlock()
	return p
}

// keep drops every cached transcript not in paths.
func (r *pendingReader) keep(paths map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range r.cache {
		if !paths[p] {
			delete(r.cache, p)
		}
	}
}

// attachPending sets each WAITING Claude window's Pending from its transcript
// (a copy of its ClaudePane: the backend's value is not changed). A window
// that already carries one (a backend that knows it) keeps it.
func (r *pendingReader) attachPending(windows []WindowInfo) {
	seen := map[string]bool{}
	for i := range windows {
		c := windows[i].Claude
		if c == nil || c.Status != mirror.StatusWaiting || c.Transcript == "" || c.Pending != nil {
			continue
		}
		seen[c.Transcript] = true
		cp := *c
		cp.Pending = r.of(c.Transcript)
		windows[i].Claude = &cp
	}
	r.keep(seen)
}
