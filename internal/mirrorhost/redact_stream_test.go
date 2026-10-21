package mirrorhost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/uploads"
)

// TestEveryFreeTextFieldOfAnEventIsRedacted: redactEvent reaches each field a
// view draws — a notice's summary and detail, a call's target and input, a
// result's output, an ask's questions, options and answers (keys too) — and
// the input stays JSON a subagent card can read its description out of.
func TestEveryFreeTextFieldOfAnEventIsRedacted(t *testing.T) {
	ev := cctranscript.Event{Kind: cctranscript.KindNotice, Text: "TOKEN=ghp_NoticeText0001", Output: "detail ghp_NoticeOut0002",
		Target: "curl -H 'Authorization: Bearer abc123Target'",
		Input:  `{"description":"use ghp_InputDesc0003","n":12345678901234567890,"nested":[{"k":"sk-InputNested0004"}]}`,
		Ask: &cctranscript.Ask{
			Questions: []askq.Question{{Question: "Push with ghp_AskQuestion0005?", Header: "h",
				Options: []askq.Option{{Label: "ghp_AskLabel0006", Description: "ghp_AskDesc0007"}}}},
			Answers: map[string]string{"Push with ghp_AskQuestion0005?": "yes, PASSWORD=hunter2x"},
		}}
	orig := ev.Ask
	redactEvent(&ev)
	b, _ := json.Marshal(ev)
	for _, s := range []string{"ghp_NoticeText0001", "ghp_NoticeOut0002", "abc123Target", "ghp_InputDesc0003", "sk-InputNested0004",
		"ghp_AskQuestion0005", "ghp_AskLabel0006", "ghp_AskDesc0007", "hunter2x"} {
		if strings.Contains(string(b), s) {
			t.Errorf("%q survived: %s", s, b)
		}
	}
	var in struct {
		Description string      `json:"description"`
		N           json.Number `json:"n"`
	}
	if err := json.Unmarshal([]byte(ev.Input), &in); err != nil || in.Description != "use «redacted»" || in.N != "12345678901234567890" {
		t.Errorf("the input after redaction: %q (%v); want JSON, its description redacted and its number as written", ev.Input, err)
	}
	if orig.Questions[0].Question != "Push with ghp_AskQuestion0005?" {
		t.Error("the redaction wrote through to the Ask it was handed (a copy is redacted)")
	}
	// CONTROL: an input with nothing secret in it is passed through byte for byte.
	clean := cctranscript.Event{Input: `{"b":1,  "a":"x"}`}
	redactEvent(&clean)
	if clean.Input != `{"b":1,  "a":"x"}` {
		t.Errorf("a clean input was rewritten: %q", clean.Input)
	}
}

// TestARedactedFieldStaysWithinItsBound: a replacement longer than what it
// replaced cannot carry a field past its cap, so every frame bound reasoned
// from the caps still holds.
func TestARedactedFieldStaysWithinItsBound(t *testing.T) {
	// "-p a" is four bytes and becomes "-p «redacted»": the worst growth.
	grow := func(n int) string { return strings.Repeat("-p a ", n/5) }
	ev := cctranscript.Event{Text: grow(cctranscript.MaxText), Output: grow(cctranscript.MaxOutput), Target: grow(cctranscript.MaxText),
		Input: grow(cctranscript.MaxInput)}
	redactEvent(&ev)
	if len(ev.Text) > cctranscript.MaxText || len(ev.Output) > cctranscript.MaxOutput || len(ev.Target) > cctranscript.MaxText ||
		len(ev.Input) > cctranscript.MaxInput {
		t.Fatalf("a redacted field outgrew its bound: text %d, output %d, target %d, input %d", len(ev.Text), len(ev.Output), len(ev.Target), len(ev.Input))
	}
	if !strings.Contains(ev.Text, "«redacted»") {
		t.Fatal("CONTROL: nothing was redacted")
	}
	refs := []cctranscript.PromptRef{{Text: grow(cctranscript.MaxPromptRefText)}}
	redactPrompts(refs)
	if len(refs[0].Text) > cctranscript.MaxPromptRefText || !strings.Contains(refs[0].Text, "«redacted»") {
		t.Fatalf("a prompt index line: %d bytes %q", len(refs[0].Text), refs[0].Text)
	}
}

// largeBackfill writes a transcript the size of a full backfill window
// (mirror.BackfillWindow) of realistic rows — prompts, replies, Bash calls
// and their results, some carrying a token — and returns its backfilled lines.
func largeBackfill(tb testing.TB) []cctranscript.Line {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "big.jsonl")
	var sb strings.Builder
	pad := strings.Repeat("the quick brown fox jumps over the lazy dog ", 60) // ~2.6 KiB
	for i := 0; sb.Len() < mirror.BackfillWindow; i++ {
		row := func(v any) { b, _ := json.Marshal(v); sb.Write(b); sb.WriteByte('\n') }
		id := fmt.Sprintf("tu%d", i)
		row(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": fmt.Sprintf("step %d: %s", i, pad)}})
		row(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": pad + " GITHUB_TOKEN=ghp_Bench" + fmt.Sprint(100000+i)},
			map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "curl -H 'Authorization: Bearer abc" + fmt.Sprint(100000+i) + "' https://example.test/" + pad[:200]}}}}})
		row(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat(pad, 2), "is_error": false}}}})
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		tb.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	lines, _, err := cctranscript.Backfill(f, st.Size(), mirror.BackfillWindow, mirror.MaxBackfillEvents)
	if err != nil {
		tb.Fatal(err)
	}
	return lines
}

// clone is lines with their events copied (redactLines works in place).
func clone(lines []cctranscript.Line) []cctranscript.Line {
	out := make([]cctranscript.Line, len(lines))
	for i, l := range lines {
		out[i] = l
		out[i].Events = append([]cctranscript.Event(nil), l.Events...)
	}
	return out
}

// TestALargeBackfillIsRedactedWithinItsFrames: a full backfill window,
// redacted, still frames into messages within MaxBatchBytes (so within
// MaxFrame), and the redaction's cost on it is logged (go test -v; the
// benchmark below measures it properly).
func TestALargeBackfillIsRedactedWithinItsFrames(t *testing.T) {
	lines := largeBackfill(t)
	n, bytes := 0, 0
	for _, l := range lines {
		for _, ev := range l.Events {
			n++
			bytes += len(ev.Text) + len(ev.Target) + len(ev.Input) + len(ev.Output)
		}
	}
	work := clone(lines)
	start := time.Now()
	redactLines(work)
	took := time.Since(start)
	t.Logf("redacted a full backfill: %d events, %d KiB of text, in %s", n, bytes>>10, took)
	for _, m := range mirror.BatchLines(work, 0) {
		b, _ := json.Marshal(m)
		if len(b) > mirror.MaxBatchBytes+4<<10 || len(b) > mirror.MaxFrame {
			t.Fatalf("a batch of %d bytes", len(b))
		}
		if strings.Contains(string(b), "ghp_Bench") || strings.Contains(string(b), "Bearer abc1") {
			t.Fatal("a token survived the backfill's redaction")
		}
	}
	if n < mirror.MaxBackfillEvents/2 {
		t.Fatalf("CONTROL: the backfill has only %d events", n)
	}
}

// BenchmarkRedactBackfill is redactLines on a full backfill window: what a
// fresh view's first frame waits for.
func BenchmarkRedactBackfill(b *testing.B) {
	lines := largeBackfill(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		work := clone(lines)
		b.StartTimer()
		redactLines(work)
	}
}

// TestAnAttachmentLineIsKeptForItsChip: a message sent with a file carries
// muster's own attachment line, whose signed link has a sig= the redactor
// would take. The line is kept exactly, so the view still finds it and draws
// the chip (uploads.FindRefs), while a secret elsewhere in the same message
// is redacted — before and after the line.
func TestAnAttachmentLineIsKeptForItsChip(t *testing.T) {
	url := "https://m.example.test/f/" + strings.Repeat("A", 22) + "?exp=946684800&sig=" + strings.Repeat("b", 43)
	line := uploads.AgentLine("shot.png", "image/png", 2048, url)
	ev := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "TOKEN=ghp_Before0001 look\n" + line + "\nand PASSWORD=after0002"}
	redactEvent(&ev)
	refs := uploads.FindRefs(ev.Text)
	if len(refs) != 1 || refs[0].URL != url {
		t.Fatalf("the attachment line did not survive: %q", ev.Text)
	}
	if strings.Contains(ev.Text, "ghp_Before0001") || strings.Contains(ev.Text, "after0002") {
		t.Fatalf("a secret beside the attachment line survived: %q", ev.Text)
	}
	// The prompt index's line too.
	refsIdx := []cctranscript.PromptRef{{Text: line}}
	redactPrompts(refsIdx)
	if refsIdx[0].Text != cctranscript.Cap(line, cctranscript.MaxPromptRefText) {
		t.Fatalf("the index line: %q", refsIdx[0].Text)
	}
}

// TestOnlyMustersOwnAttachmentLineIsExempt (#147): the attachment parser
// accepts more than the line muster writes — any text in the curl command's
// quotes, any userinfo or path before /f/ — so a line SHAPED like an
// attachment could carry a credential past the redactor. Only AgentLine's
// exact line, whose only redactable part is its sig, is kept; every other
// shape is redacted, and the credential does not survive.
func TestOnlyMustersOwnAttachmentLineIsExempt(t *testing.T) {
	// Built at run time: a literal shaped like a token would itself be a
	// finding of the repository's leakscan.
	tok := "gh" + "p_" + strings.Repeat("a1B2", 9)
	id, q := strings.Repeat("A", 22), "?exp=946684800&sig="+strings.Repeat("b", 43)
	good := "https://m.example.test/f/" + id + q
	for _, c := range []struct{ name, line string }{
		{"a credential in the curl quotes", "[attached: shot.png (image/png, 2 KB) " + good +
			" — download with curl -sSfo shot.png '" + tok + "']"},
		{"userinfo in the link", uploads.AgentLine("shot.png", "image/png", 2048, "https://u:"+tok+"@m.example.test/f/"+id+q)},
		{"a credential in the link's path", uploads.AgentLine("shot.png", "image/png", 2048, "https://m.example.test/"+tok+"/f/"+id+q)},
	} {
		if len(uploads.FindRefs(c.line)) != 1 {
			t.Fatalf("%s: the fixture is not an attachment line to the parser: %q", c.name, c.line)
		}
		ev := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "see\n" + c.line}
		redactEvent(&ev)
		if strings.Contains(ev.Text, tok) {
			t.Errorf("%s: the credential survived: %q", c.name, ev.Text)
		}
	}
	// The real line is still kept whole.
	line := uploads.AgentLine("shot.png", "image/png", 2048, good)
	ev := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: line}
	redactEvent(&ev)
	if ev.Text != line {
		t.Fatalf("muster's own line was changed: %q", ev.Text)
	}
}

// TestACredentialInTheSigSlotIsRedacted (#209 audit): an attachment-shaped
// line from any host with a GitHub token where muster's signature goes is not
// muster's line; the token does not survive. Control: muster's own line, whose
// sig is an HMAC-shaped value, is kept whole.
func TestACredentialInTheSigSlotIsRedacted(t *testing.T) {
	// Built at run time: a literal shaped like a token would itself be a
	// leakscan finding. 43 characters, the sig slot's length.
	tok := "gh" + "p_" + strings.Repeat("a1B2", 9) + "c3D"
	if len(tok) != 43 {
		t.Fatalf("instrument: the token is %d characters, the sig slot takes 43", len(tok))
	}
	id := strings.Repeat("A", 22)
	evil := uploads.AgentLine("shot.png", "image/png", 2048, "https://elsewhere.example.test/f/"+id+"?exp=946684800&sig="+tok)
	if len(uploads.FindRefs(evil)) != 1 {
		t.Fatalf("instrument: the fixture is not an attachment line to the parser: %q", evil)
	}
	ev := cctranscript.Event{Kind: cctranscript.KindPrompt, Text: evil}
	redactEvent(&ev)
	if strings.Contains(ev.Text, tok) {
		t.Fatalf("a credential in the sig slot survived: %q", ev.Text)
	}
	good := uploads.AgentLine("shot.png", "image/png", 2048, "https://m.example.test/f/"+id+"?exp=946684800&sig="+strings.Repeat("b", 43))
	ev = cctranscript.Event{Kind: cctranscript.KindPrompt, Text: good}
	redactEvent(&ev)
	if ev.Text != good {
		t.Fatalf("control: muster's own line was changed: %q", ev.Text)
	}
}
