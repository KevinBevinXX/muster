package mirrorhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

func call(id, tool string, input any) cctranscript.Event {
	b, _ := json.Marshal(input)
	return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id, Tool: tool, Input: string(b)}
}

func result(id string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: true}
}

// TestEachToolIsSummarisedAsItsPendingAction: one line per tool type, from the
// call's input, and nothing more of the input than that line.
func TestEachToolIsSummarisedAsItsPendingAction(t *testing.T) {
	long := strings.Repeat("a", 300)
	for _, c := range []struct {
		name string
		ev   cctranscript.Event
		want string
	}{
		{"Bash: its first non-blank line", call("1", "Bash", map[string]string{"command": "\n  npm publish --tag next  \nnpm dist-tag ls", "description": "publish"}), "Bash: npm publish --tag next"},
		{"Bash: capped at the summary bound", call("1", "Bash", map[string]string{"command": "echo " + long}), "Bash: echo " + strings.Repeat("a", mirror.MaxPendingSummary-len("Bash: echo ")-1) + "…"},
		{"Edit: the path", call("1", "Edit", map[string]string{"file_path": "internal/api/x.go", "old_string": "SECRET", "new_string": "SECRET2"}), "Edit internal/api/x.go"},
		{"Write: the path", call("1", "Write", map[string]string{"file_path": "/tmp/w.txt", "content": "TOKEN=abc"}), "Write /tmp/w.txt"},
		{"Read: the path", call("1", "Read", map[string]string{"file_path": "/etc/hosts"}), "Read /etc/hosts"},
		{"a long path keeps its end", call("1", "Edit", map[string]string{"file_path": "/" + strings.Repeat("d/", 80) + "file.go"}), "Edit …" + ("/" + strings.Repeat("d/", 80) + "file.go")[len("/"+strings.Repeat("d/", 80)+"file.go")-(maxPathRunes-1):]},
		{"WebFetch: the URL's host only", call("1", "WebFetch", map[string]string{"url": "https://docs.example.test/a/b?token=abc", "prompt": "read"}), "WebFetch docs.example.test"},
		{"an MCP tool: server/tool", call("1", "mcp__muster__attention_ask", map[string]string{"question": "q"}), "MCP muster/attention_ask"},
		{"AskUserQuestion: the first question", cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "1", Tool: askq.ToolName,
			Ask: &cctranscript.Ask{Questions: []askq.Question{{Question: "Which database?\nPick one."}, {Question: "Migrate?"}}}}, "Which database?"},
		{"an unknown tool: its name", call("1", "Frobnicate", map[string]string{"secret": "s3cr3t"}), "Frobnicate"},
		{"a Bash with no command: its name", call("1", "Bash", map[string]string{}), "Bash"},
	} {
		p := PendingOf([]cctranscript.Event{c.ev})
		if p == nil || p.Summary != c.want {
			t.Errorf("%s: %+v, want %q", c.name, p, c.want)
		}
		for _, leak := range []string{"SECRET", "TOKEN", "token=abc", "s3cr3t", "dist-tag"} {
			if p != nil && strings.Contains(p.Summary, leak) {
				t.Errorf("%s: the summary carries more of the input than its line: %q", c.name, p.Summary)
			}
		}
	}
}

// TestThePendingCallIsTheLastOneWithoutAResultSinceThePrompt: an answered call
// is not pending; a call before the last prompt (interrupted) is not either;
// the last text is Claude's last words, collapsed and capped, control
// characters gone.
func TestThePendingCallIsTheLastOneWithoutAResultSinceThePrompt(t *testing.T) {
	text := func(s string) cctranscript.Event { return cctranscript.Event{Kind: cctranscript.KindText, Text: s} }
	prompt := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "go"}
	evs := []cctranscript.Event{
		call("old", "Bash", map[string]string{"command": "rm -rf build"}), // interrupted, before the prompt
		prompt,
		text("Let me check."),
		call("a", "Read", map[string]string{"file_path": "go.mod"}), result("a"),
		call("b", "Bash", map[string]string{"command": "make deploy"}),
		call("c", "Read", map[string]string{"file_path": "x"}), result("c"),
		text("Deploying\x1b[1m now:\n\n  " + strings.Repeat("w ", 150)),
	}
	p := PendingOf(evs)
	if p == nil || p.Summary != "Bash: make deploy" {
		t.Fatalf("pending %+v", p)
	}
	if !strings.HasPrefix(p.Last, "Deploying [1m now: w w") || strings.ContainsAny(p.Last, "\n\x1b") || len([]rune(p.Last)) > mirror.MaxPendingLast {
		t.Fatalf("last %q (%d runes)", p.Last, len([]rune(p.Last)))
	}
	// Everything answered since the prompt: no action, the last text alone.
	p = PendingOf(append(evs, result("b")))
	if p == nil || p.Summary != "" || p.Last == "" {
		t.Fatalf("nothing pending: %+v", p)
	}
	// The interrupted call before the prompt is not resurrected.
	if p := PendingOf([]cctranscript.Event{call("old", "Bash", map[string]string{"command": "rm"}), prompt}); p != nil {
		t.Fatalf("a call before the last prompt: %+v", p)
	}
}

// TestTheHostReadsWhatAWaitingWindowWaitsOnFromItsTranscript: a waiting
// window's transcript tail gives its Pending — on the wire — while a busy one
// gets none and is not read; a re-read happens only when the file changed.
func TestTheHostReadsWhatAWaitingWindowWaitsOnFromItsTranscript(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	write := func(lines ...string) {
		if err := os.WriteFile(tr, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"type":"user","message":{"content":"ship it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Publishing."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"npm publish"}}]}}`)
	// A busy window whose transcript ALSO ends in a call with no result (it
	// is running): it is not waiting, so it is neither read nor sent.
	busy := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(busy, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Bash","input":{"command":"go test ./..."}}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	windows := []WindowInfo{
		{Session: "w", ID: "@1", Index: 1, Active: true, Claude: &ClaudePane{Transcript: tr, Gen: "s", Status: mirror.StatusWaiting, StatusSince: 7}},
		{Session: "w", ID: "@2", Index: 2, Claude: &ClaudePane{Transcript: busy, Gen: "m", Status: mirror.StatusBusy}},
	}
	var r pendingReader
	r.attachPending(windows)
	wire := WireWindows(windows, nil, true, time.Now())
	if p := wire[0].Pending; p == nil || p.Summary != "Bash: npm publish" || p.Last != "Publishing." {
		t.Fatalf("the waiting window's pending: %+v", p)
	}
	if wire[1].Pending != nil {
		t.Fatalf("a busy window carries a pending: %+v", wire[1].Pending)
	}
	if len(r.cache) != 1 {
		t.Fatalf("read %d transcripts, want only the waiting one's", len(r.cache))
	}
	// Unchanged file: served from the cache (a poisoned entry proves it).
	r.mu.Lock()
	e := r.cache[tr]
	e.p = &mirror.Pending{Summary: "cached"}
	r.cache[tr] = e
	r.mu.Unlock()
	if p := r.of(tr); p == nil || p.Summary != "cached" {
		t.Fatalf("an unchanged transcript was re-read: %+v", p)
	}
	// Changed: re-read — the call answered, a question asked.
	write(`{"type":"user","message":{"content":"ship it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"npm publish"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"AskUserQuestion","input":{"questions":[{"question":"Tag it latest?","header":"Tag","options":[{"label":"yes"},{"label":"no"}],"multiSelect":false}]}}]}}`)
	if p := r.of(tr); p == nil || p.Summary != "Tag it latest?" {
		t.Fatalf("a changed transcript: %+v", p)
	}
	// A window that stops waiting is dropped from the cache.
	windows[0].Claude.Status = mirror.StatusIdle
	r.attachPending(windows)
	if len(r.cache) != 0 {
		t.Fatalf("the cache kept %d transcripts of windows no longer waiting", len(r.cache))
	}
}
