package mirrorhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Synthetic transcript records in the shapes the CLI writes (field names
// counted on real transcripts; every value invented).

var t0 = time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)

func stamp(off time.Duration) string { return t0.Add(off).Format(time.RFC3339Nano) }

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func tePrompt(off time.Duration, text string) string {
	return `{"type":"user","timestamp":"` + stamp(off) + `","message":{"role":"user","content":` + jsonStr(text) + `}}`
}

func teReply(off time.Duration, text string) string {
	return `{"type":"assistant","timestamp":"` + stamp(off) + `","message":{"model":"claude-x","content":[{"type":"text","text":` + jsonStr(text) + `}]}}`
}

func teToolCall(off time.Duration) string {
	return `{"type":"assistant","timestamp":"` + stamp(off) + `","message":{"model":"claude-x","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`
}

func teToolResult(off time.Duration) string {
	return `{"type":"user","timestamp":"` + stamp(off) + `","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`
}

// teError is a record the CLI wrote itself: code "" is one with no error flag.
func teError(off time.Duration, code, text string, extra string) string {
	flag := ""
	if code != "" {
		flag = `,"error":"` + code + `","isApiErrorMessage":true`
	}
	return `{"type":"assistant","timestamp":"` + stamp(off) + `"` + flag + extra +
		`,"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":` + jsonStr(text) + `}]}}`
}

const limitMsg = "You've hit your session limit · resets 10:30pm (UTC)"

func teLimit(off time.Duration) string { return teError(off, "rate_limit", limitMsg, "") }

func teOf(t *testing.T, lines ...string) *mirror.TurnError {
	t.Helper()
	s := strings.Join(lines, "\n") + "\n"
	return TurnEndOf(strings.NewReader(s), int64(len(s)))
}

func TestTurnEndOf(t *testing.T) {
	reset := time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC).UnixMilli()
	at := func(off time.Duration) int64 { return t0.Add(off).UnixMilli() }
	cases := []struct {
		name  string
		lines []string
		want  *mirror.TurnError
	}{
		{"a prompt rejected at once by a usage limit: the limit, its reset, rejected, one quick turn",
			[]string{tePrompt(0, "earlier"), teReply(time.Second, "done"), tePrompt(time.Minute, "  queued one\n"), teLimit(time.Minute + 500*time.Millisecond)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(time.Minute + 500*time.Millisecond), ResetsAt: reset,
				Rejected: true, Prompt: mirror.PromptDigest("queued one"), PrevAt: at(time.Second), Quick: 1}},
		{"a prompt answered for real: none",
			[]string{tePrompt(0, "hi"), teLimit(time.Second), tePrompt(time.Minute, "again"), teReply(time.Minute+3*time.Second, "hello")},
			nil},
		{"a prompt whose turn has not ended (the newest message is the prompt): none",
			[]string{tePrompt(0, "hi"), teLimit(time.Second), tePrompt(time.Minute, "again")},
			nil},
		{"the limit hit MID-TURN, after a tool ran: the limit, not rejected — it was processed — and not quick",
			[]string{tePrompt(0, "do work"), teToolCall(time.Second), teToolResult(2 * time.Second), teLimit(3 * time.Second)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(3 * time.Second), ResetsAt: reset}},
		{"a limit after a real text reply in the same turn: not rejected",
			[]string{tePrompt(0, "do work"), teReply(time.Second, "on it"), teLimit(2 * time.Second)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(2 * time.Second), ResetsAt: reset}},
		{"three prompts each failed at once by Not logged in: quick 3, rejected",
			[]string{tePrompt(0, "x"), teReply(time.Second, "ok"),
				tePrompt(time.Minute, "a"), teError(time.Minute+300*time.Millisecond, "authentication_failed", "Not logged in · Please run /login", ""),
				tePrompt(2*time.Minute, "b"), teError(2*time.Minute+300*time.Millisecond, "authentication_failed", "Not logged in · Please run /login", ""),
				tePrompt(3*time.Minute, "c"), teError(3*time.Minute+300*time.Millisecond, "authentication_failed", "Not logged in · Please run /login", "")},
			&mirror.TurnError{Kind: "authentication_failed", At: at(3*time.Minute + 300*time.Millisecond), Rejected: true, Prompt: mirror.PromptDigest("c"), PrevAt: at(2*time.Minute + 300*time.Millisecond), Quick: 3}},
		{"an error that took longer than QuickTurnMS: named, not quick",
			[]string{tePrompt(0, "slow"), teError(30*time.Second, "server_error", "API Error: Request timeout", "")},
			&mirror.TurnError{Kind: "server_error", At: at(30 * time.Second), Rejected: true, Prompt: mirror.PromptDigest("slow")}},
		{"the record before the refused prompt may be a USER record (a meta one here): its time is PrevAt",
			[]string{tePrompt(0, "earlier"), teReply(time.Second, "done"),
				`{"type":"user","isMeta":true,"timestamp":"` + stamp(30*time.Second) + `","message":{"content":"<local-command-stdout>x</local-command-stdout>"}}`,
				tePrompt(time.Minute, "M"), teLimit(time.Minute + 500*time.Millisecond)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(time.Minute + 500*time.Millisecond), ResetsAt: reset, Rejected: true,
				Prompt: mirror.PromptDigest("M"), PrevAt: at(30 * time.Second), Quick: 1}},
		{"a refusal that took longer than QuickTurnMS still reads the turn before it",
			[]string{tePrompt(0, "earlier"), teReply(time.Second, "done"), tePrompt(time.Minute, "slow"), teError(time.Minute+30*time.Second, "server_error", "API Error: Request timeout", "")},
			&mirror.TurnError{Kind: "server_error", At: at(time.Minute + 30*time.Second), Rejected: true, Prompt: mirror.PromptDigest("slow"), PrevAt: at(time.Second)}},
		{"the count stops at the first turn that was not a quick failure (an older quick one is not counted)",
			[]string{tePrompt(-time.Hour, "z"), teLimit(-time.Hour + time.Second),
				tePrompt(0, "a"), teError(20*time.Second, "server_error", "API Error", ""),
				tePrompt(time.Minute, "b"), teLimit(time.Minute + time.Second),
				tePrompt(2*time.Minute, "c"), teLimit(2*time.Minute + time.Second)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(2*time.Minute + time.Second), ResetsAt: reset, Rejected: true, Prompt: mirror.PromptDigest("c"), PrevAt: at(time.Minute + time.Second), Quick: 2}},
		{"a resumed session's meta prompt and 'No response requested.' after the limit are passed over",
			[]string{tePrompt(0, "q"), teLimit(time.Second),
				`{"type":"user","isMeta":true,"timestamp":"` + stamp(time.Hour) + `","message":{"content":"Continue from where you left off."}}`,
				teError(time.Hour+time.Second, "", "No response requested.", "")},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(time.Second), ResetsAt: reset, Rejected: true, Prompt: mirror.PromptDigest("q"), Quick: 1}},
		{"the count stops at a real reply, even one with no prompt of its own (a resumed session's answer to its meta prompt)",
			[]string{tePrompt(0, "z"), teLimit(time.Second),
				`{"type":"user","isMeta":true,"timestamp":"` + stamp(time.Hour) + `","message":{"content":"Continue from where you left off."}}`,
				teReply(time.Hour+time.Second, "continuing"),
				tePrompt(2*time.Hour, "q"), teLimit(2*time.Hour + time.Second)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(2*time.Hour + time.Second), ResetsAt: reset, Rejected: true, Prompt: mirror.PromptDigest("q"), PrevAt: at(time.Hour + time.Second), Quick: 1}},
		{"a subagent's error is not the session's turn",
			[]string{tePrompt(0, "q"), teReply(time.Second, "fine"),
				strings.Replace(teLimit(2*time.Second), `{"type":"assistant"`, `{"type":"assistant","isSidechain":true`, 1)},
			nil},
		{"the CLI retrying within one turn: still one turn",
			[]string{tePrompt(0, "q"), teError(time.Second, "server_error", "API Error: 529", ""), teError(2*time.Second, "server_error", "API Error: 529", "")},
			&mirror.TurnError{Kind: "server_error", At: at(2 * time.Second), Rejected: true, Prompt: mirror.PromptDigest("q"), Quick: 1}},
		{"a limit record carrying quotaLimits.resetsAt: that reset",
			[]string{tePrompt(0, "q"), teError(time.Second, "rate_limit", limitMsg, `,"quotaLimits":{"status":"rejected","resetsAt":1792000000}`)},
			&mirror.TurnError{Kind: mirror.TurnErrorLimit, At: at(time.Second), ResetsAt: 1792000000_000, Rejected: true, Prompt: mirror.PromptDigest("q"), Quick: 1}},
		{"a 429 that does not say a limit was hit is an error, not a usage limit",
			[]string{tePrompt(0, "q"), teError(time.Second, "rate_limit", "API Error: Request rate limited", "")},
			&mirror.TurnError{Kind: "rate_limit", At: at(time.Second), Rejected: true, Prompt: mirror.PromptDigest("q"), Quick: 1}},
		{"no messages at all: none", []string{`{"type":"system","subtype":"turn_duration"}`}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := teOf(t, c.lines...)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("TurnEndOf = %+v; want %+v", got, c.want)
			}
		})
	}
}

// TestTurnEndOfReadsAcrossChunkBoundaries: the prompt sits more than a chunk
// (lastMsgChunk) before the limit record, behind rows that do not count, and
// is still found — and the line cut by the chunk boundary is read whole.
func TestTurnEndOfReadsAcrossChunkBoundaries(t *testing.T) {
	pad := `{"type":"attachment","x":"` + strings.Repeat("p", 3000) + `"}`
	lines := []string{tePrompt(0, "far back")}
	for i := 0; i < 3*lastMsgChunk/len(pad); i++ {
		lines = append(lines, pad)
	}
	lines = append(lines, teLimit(time.Second))
	got := teOf(t, lines...)
	if got == nil || !got.Rejected || got.Quick != 1 {
		t.Fatalf("TurnEndOf = %+v; want the limit, rejected, its prompt %d bytes back", got, 3*lastMsgChunk)
	}
}

// TestTurnEndIsReadForAnIdleClaudeWindowOnly: a busy or waiting window carries
// none (its turn has not ended), whatever its transcript says; an idle one
// does; and the wire keeps it only on an idle Claude window.
func TestTurnEndIsReadForAnIdleClaudeWindowOnly(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(tr, []byte(tePrompt(0, "q")+"\n"+teLimit(time.Second)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mk := func(status string) WindowInfo {
		return WindowInfo{Session: "s", ID: "@1", Claude: &ClaudePane{Transcript: tr, Status: status, StatusSince: 1}}
	}
	var r turnEndReader
	ws := []WindowInfo{mk(mirror.StatusIdle), mk(mirror.StatusBusy), mk(mirror.StatusWaiting)}
	ws[1].ID, ws[2].ID = "@2", "@3"
	r.attach(ws)
	if ws[0].Claude.TurnError == nil || ws[0].Claude.TurnError.Kind != mirror.TurnErrorLimit {
		t.Fatalf("idle window: %+v; want the limit", ws[0].Claude.TurnError)
	}
	if ws[1].Claude.TurnError != nil || ws[2].Claude.TurnError != nil {
		t.Fatalf("busy %+v, waiting %+v; want none", ws[1].Claude.TurnError, ws[2].Claude.TurnError)
	}
	wire := WireWindows(ws, nil, true, time.Now())
	for _, w := range wire {
		if (w.ID == "@1") != (w.TurnError != nil) {
			t.Fatalf("on the wire, %s carries %+v; want only the idle window's", w.ID, w.TurnError)
		}
	}
}

// TestARefusalWhoseTurnBeforeIsOutOfReachIsNotRejected: the record before the
// refused prompt lies further back than the host reads (turnEndMaxScan), so
// whether anything came between a paste and the refusal is unknown — and the
// turn is reported NOT Rejected (the queue then leaves the message sent). The
// control: the same transcript with the padding gone, Rejected, PrevAt read.
func TestARefusalWhoseTurnBeforeIsOutOfReachIsNotRejected(t *testing.T) {
	pad := `{"type":"attachment","x":"` + strings.Repeat("p", 4000) + `"}`
	lines := []string{tePrompt(0, "earlier"), teReply(time.Second, "done")}
	for i := 0; i < turnEndMaxScan/len(pad)+2; i++ {
		lines = append(lines, pad)
	}
	lines = append(lines, tePrompt(time.Minute, "M"), teLimit(time.Minute+time.Second))
	if got := teOf(t, lines...); got == nil || got.Rejected || got.Prompt != "" || got.Kind != mirror.TurnErrorLimit {
		t.Fatalf("TurnEndOf = %+v; want the limit, NOT Rejected", got)
	}
	short := []string{tePrompt(0, "earlier"), teReply(time.Second, "done"), tePrompt(time.Minute, "M"), teLimit(time.Minute + time.Second)}
	if got := teOf(t, short...); got == nil || !got.Rejected || got.PrevAt != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("CONTROL: TurnEndOf = %+v; want Rejected, PrevAt the reply", got)
	}
}
