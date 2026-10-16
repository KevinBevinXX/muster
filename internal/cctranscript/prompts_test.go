package cctranscript

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func jsonLine(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Prompts names every prompt in the WHOLE file by its place — the offset just
// past its line, the Line a reader gives its event — with its first line, and
// nothing that is not a prompt: not a reply, a tool result, a harness notice,
// a subagent's prompt or injected context. A slash command is the human's
// "/name args", and a trailing line the CLI is still writing is not read.
func TestPromptsIndexesEveryPromptInTheFile(t *testing.T) {
	user := func(content any, extra map[string]any) string {
		m := map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"role": "user", "content": content}}
		for k, v := range extra {
			m[k] = v
		}
		return jsonLine(t, m)
	}
	recs := []struct {
		line string
		want string // "" = not a prompt
	}{
		{user("first question", nil), "first question"},
		{jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "a reply"}}}}), ""},
		{user([]any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "out"}}, nil), ""},
		{user("<task-notification><status>completed</status><summary>done</summary></task-notification>", map[string]any{"origin": map[string]any{"kind": "task-notification"}}), ""},
		{user("<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>sonnet</command-args>", nil), "/model sonnet"},
		{user("a subagent's prompt", map[string]any{"isSidechain": true}), ""},
		{user("injected context", map[string]any{"isMeta": true}), ""},
		{user("\n   \n  the second, multi-line\nand its body", nil), "the second, multi-line"},
		{user([]any{map[string]any{"type": "text", "text": "a prompt with an image"}, map[string]any{"type": "image"}}, nil), "a prompt with an image"},
		{user("mentions \"tool_result\" and \"user\" in its text", nil), `mentions "tool_result" and "user" in its text`},
		{user(strings.Repeat("w", 600), nil), strings.Repeat("w", MaxPromptRefText-len("…")) + "…"},
	}
	var file string
	var want []PromptRef
	for _, r := range recs {
		file += r.line
		if r.want != "" {
			want = append(want, PromptRef{Line: int64(len(file)), Text: r.want})
		}
	}
	whole := file
	file += strings.TrimSuffix(user("still being written", nil), "\n")

	got, err := Prompts(strings.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts:\n got %+v\nwant %+v", got, want)
	}
	// The seam: a reader's prompt events are named by the very same places.
	evs, _, err := ReadEvents(strings.NewReader(whole), 0, int64(len(whole)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var places []int64
	for _, ev := range evs {
		if ev.Kind == KindPrompt {
			places = append(places, ev.Line)
		}
	}
	var wantPlaces []int64
	for _, w := range want {
		wantPlaces = append(wantPlaces, w.Line)
	}
	if !reflect.DeepEqual(places, wantPlaces) {
		t.Fatalf("reader places %v, index places %v", places, wantPlaces)
	}
}

// A line longer than the scanner's own buffer is read whole (a prompt with a
// large pasted body is still a prompt), and one past maxPromptLine is scanned
// past without being decoded — the prompts after it are still found, at the
// right places.
func TestPromptsReadsLongLinesAndSkipsHugeOnes(t *testing.T) {
	big := jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"content": "big one\n" + strings.Repeat("b", 200<<10)}})
	huge := jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"content": strings.Repeat("h", maxPromptLine)}})
	after := jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"content": "after"}})
	file := big + huge + after
	got, err := Prompts(bytes.NewReader([]byte(file)), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	want := []PromptRef{{Line: int64(len(big)), Text: "big one"}, {Line: int64(len(file)), Text: "after"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts %+v, want %+v", got, want)
	}
}
