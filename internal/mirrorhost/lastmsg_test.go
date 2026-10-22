package mirrorhost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lmRow is one transcript line in the shape Claude Code writes (synthetic values).
func lmRow(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

const (
	tsPrompt  = "2026-09-03T08:15:11.204Z"
	tsReply   = "2026-09-03T08:16:42.517Z"
	tsResume  = "2026-10-21T14:53:40.003Z"
	tsResume2 = "2026-10-21T14:53:41.877Z"
	tsResume3 = "2026-10-21T14:53:42.410Z"
)

// resumedTranscript is a conversation whose last message is the assistant's
// reply at tsReply, followed by EXACTLY what a resume appended in the real
// transcripts this rule was measured against: attachment and system rows with
// the resume's time, an isMeta user row, and untimestamped bookkeeping rows.
func resumedTranscript(t *testing.T) string {
	return lmRow(t, map[string]any{"type": "user", "timestamp": tsPrompt, "message": map[string]any{"role": "user", "content": "tidy the notes"}}) +
		lmRow(t, map[string]any{"type": "assistant", "timestamp": tsReply, "message": map[string]any{"role": "assistant", "model": "claude-fixture-model", "content": []any{map[string]any{"type": "text", "text": "Done."}}}}) +
		lmRow(t, map[string]any{"type": "system", "subtype": "stop_hook_summary", "timestamp": "2026-09-03T08:16:43.001Z"}) +
		lmRow(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": "2026-09-03T08:16:43.002Z", "isMeta": false}) +
		lmRow(t, map[string]any{"type": "last-prompt", "lastPrompt": "tidy the notes"}) +
		lmRow(t, map[string]any{"type": "cost-state"}) +
		lmRow(t, map[string]any{"type": "bridge-session"}) +
		lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume, "attachment": map[string]any{"type": "deferred_tools_delta"}}) +
		lmRow(t, map[string]any{"type": "user", "isMeta": true, "timestamp": tsResume, "message": map[string]any{"role": "user", "content": "<local-command-caveat>Caveat: …</local-command-caveat>"}}) +
		lmRow(t, map[string]any{"type": "system", "subtype": "informational", "timestamp": tsResume2, "content": "resumed"}) +
		lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume2, "attachment": map[string]any{"type": "skill_listing"}}) +
		// Rows that SPELL "user" and "assistant" without being messages: the
		// type, not the word, decides.
		lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume2, "attachment": map[string]any{"type": "queued_command", "role": "user"}}) +
		lmRow(t, map[string]any{"type": "system", "subtype": "informational", "timestamp": tsResume2, "content": "resumed; the last \"assistant\" turn is above"}) +
		// A resume that is CONTINUED: an isMeta user row, then an assistant row
		// Claude Code wrote itself (model "<synthetic>") — the shape review
		// found reading a window 18 days late.
		lmRow(t, map[string]any{"type": "user", "isMeta": true, "timestamp": tsResume2, "message": map[string]any{"role": "user", "content": "Continue from where you left off."}}) +
		lmRow(t, map[string]any{"type": "assistant", "timestamp": tsResume3, "message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "No response requested."}}}})
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestTheLastMessageIgnoresWhatAResumeAppends: the rows a resume writes —
// attachment and system rows carrying the resume's time, an isMeta user row,
// untimestamped bookkeeping — do not count; the assistant's reply does. And a
// person's prompt after it does.
func TestTheLastMessageIgnoresWhatAResumeAppends(t *testing.T) {
	tr := resumedTranscript(t)
	got, ok := lastMessageOf(strings.NewReader(tr), int64(len(tr)))
	if !ok || !got.Equal(mustTime(t, tsReply)) {
		t.Fatalf("last message = %v (%v), want the reply at %s — not the resume at %s", got, ok, tsReply, tsResume2)
	}
	// A prompt typed after the resume IS a message.
	const tsTyped = "2026-10-21T15:02:09.660Z"
	tr2 := tr + lmRow(t, map[string]any{"type": "user", "timestamp": tsTyped, "origin": map[string]any{"kind": "human"}, "message": map[string]any{"role": "user", "content": "carry on"}})
	if got, ok := lastMessageOf(strings.NewReader(tr2), int64(len(tr2))); !ok || !got.Equal(mustTime(t, tsTyped)) {
		t.Errorf("after a typed prompt: %v (%v), want %s", got, ok, tsTyped)
	}
	// No message at all: not found (the caller falls back to tmux activity).
	only := lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume}) + lmRow(t, map[string]any{"type": "cost-state"})
	if _, ok := lastMessageOf(strings.NewReader(only), int64(len(only))); ok {
		t.Error("a transcript with no message reported one")
	}
}

// TestTheLastMessageIsFoundAcrossChunks: the reply sits more than a chunk
// before the end, behind long resume rows, and a line straddles a chunk
// boundary; the backwards read still finds it — and gives up past
// lastMsgMaxScan rather than reading a huge file whole.
func TestTheLastMessageIsFoundAcrossChunks(t *testing.T) {
	pad := strings.Repeat("x", 3*lastMsgChunk/2)
	tr := resumedTranscript(t) + lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume, "blob": pad}) +
		lmRow(t, map[string]any{"type": "system", "subtype": "informational", "timestamp": tsResume2, "blob": pad[:lastMsgChunk-37]})
	if len(tr) < 2*lastMsgChunk {
		t.Fatalf("control: the fixture is %d bytes, under two chunks", len(tr))
	}
	got, ok := lastMessageOf(strings.NewReader(tr), int64(len(tr)))
	if !ok || !got.Equal(mustTime(t, tsReply)) {
		t.Fatalf("across chunks: %v (%v), want %s", got, ok, tsReply)
	}
	// The reply's own line STRADDLES the boundary of the last chunk: the read
	// must join its two halves (without, it finds the prompt before it).
	long := lmRow(t, map[string]any{"type": "assistant", "timestamp": tsReply, "text": strings.Repeat("r", 4000)})
	head := lmRow(t, map[string]any{"type": "user", "timestamp": tsPrompt, "message": "go"})
	tailRow := lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume, "blob": ""})
	tail := lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume, "blob": strings.Repeat("z", lastMsgChunk-2000-len(tailRow))})
	straddle := head + long + tail
	if cut := len(straddle) - lastMsgChunk; cut <= len(head) || cut >= len(head)+len(long) {
		t.Fatalf("control: the chunk boundary (%d) is not inside the reply's line [%d,%d)", cut, len(head), len(head)+len(long))
	}
	if got, ok := lastMessageOf(strings.NewReader(straddle), int64(len(straddle))); !ok || !got.Equal(mustTime(t, tsReply)) {
		t.Errorf("a reply straddling a chunk boundary: %v (%v), want %s", got, ok, tsReply)
	}
	// A message only past the scan limit is not looked for.
	far := resumedTranscript(t) + lmRow(t, map[string]any{"type": "attachment", "timestamp": tsResume, "blob": strings.Repeat("y", lastMsgMaxScan)})
	if _, ok := lastMessageOf(strings.NewReader(far), int64(len(far))); ok {
		t.Error("a message past lastMsgMaxScan was read")
	}
}

// TestTheLastMessageReaderReadsAChangedTranscriptOnly: a listing every few
// seconds over unchanged transcripts reads none of them again; an appended
// message is read, and attach sets it on a COPY of the pane.
func TestTheLastMessageReaderReadsAChangedTranscriptOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(resumedTranscript(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	var r lastMessageReader
	pane := &ClaudePane{Pane: "%3", Transcript: path}
	windows := []WindowInfo{{Session: "s", ID: "@1", Claude: pane}, {Session: "s", ID: "@2"}}
	for i := 0; i < 3; i++ {
		w := append([]WindowInfo(nil), windows...)
		r.attach(w)
		if got := w[0].Claude.LastMessage; got != mustTime(t, tsReply).Unix() {
			t.Fatalf("pass %d: LastMessage = %d, want %d", i, got, mustTime(t, tsReply).Unix())
		}
	}
	if r.reads != 1 {
		t.Errorf("three listings of an unchanged transcript read it %d times, want 1", r.reads)
	}
	if pane.LastMessage != 0 {
		t.Error("attach changed the backend's own pane")
	}
	const tsNew = "2026-10-21T16:00:00.250Z"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(lmRow(t, map[string]any{"type": "assistant", "timestamp": tsNew}))
	f.Close()
	w := append([]WindowInfo(nil), windows...)
	r.attach(w)
	if got := w[0].Claude.LastMessage; got != mustTime(t, tsNew).Unix() || r.reads != 2 {
		t.Errorf("after a reply: LastMessage %d (reads %d), want %d (2)", got, r.reads, mustTime(t, tsNew).Unix())
	}
	// WireWindows carries it for a Claude window, floored to the bucket.
	wire := WireWindows(w, nil, true, mustTime(t, tsNew).Add(time.Hour))
	if want := mustTime(t, tsNew).Unix(); wire[0].LastMessage != want-want%60 || wire[1].LastMessage != 0 {
		t.Errorf("wire LastMessage = %d / %d, want %d / 0", wire[0].LastMessage, wire[1].LastMessage, want-want%60)
	}
	// A tunnel that carries no rendered view carries no Claude claim, and no last message.
	if wire := WireWindows(w, nil, false, time.Now()); wire[0].LastMessage != 0 {
		t.Error("a v1 tunnel was sent a last message")
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(WireWindows(windows, nil, true, time.Now())[1])
	if strings.Contains(buf.String(), "lastMessage") {
		t.Errorf("a window with none still spells the field: %s", buf.String())
	}
}
