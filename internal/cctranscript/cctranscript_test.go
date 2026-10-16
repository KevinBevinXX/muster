package cctranscript

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The fixture is cmd/ccd's: REAL transcript records (CLI 2.1.289) with their
// content replaced — an earlier turn, then a turn with thinking, text, a Bash
// tool_use, its tool_result, more thinking and a closing text.
const toolFixture = "../../cmd/ccd/testdata/turn_success_tools_2.1.289.jsonl"

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(toolFixture)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func allEvents(b []byte) []Event {
	var out []Event
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if r, ok := DecodeLine(line); ok {
			out = append(out, Events(r)...)
		}
	}
	return out
}

func TestEventsFromARealToolTurn(t *testing.T) {
	got := allEvents(readFixture(t))
	want := []Event{
		{Kind: KindPrompt, Text: "an earlier prompt", At: "2000-01-01T00:00:06.000Z"},
		{Kind: KindText, Text: "AN EARLIER REPLY that is not this turn"},
		{Kind: KindPrompt, Text: "list the workspace", At: "2000-01-01T00:00:06.000Z"},
		{Kind: KindText, Text: "First segment: I will list the workspace."},
		{Kind: KindToolCall, ToolID: "toolu_fixture1", Tool: "Bash", Target: "ls", Input: `{"command":"ls","description":"List files"}`},
		{Kind: KindToolResult, ToolID: "toolu_fixture1", OK: true, Output: "file-a\nfile-b"},
		{Kind: KindText, Text: "Second segment: the workspace holds file-a and file-b."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %+v\nwant %+v", got, want)
	}
}

// 🔴 The fixture's thinking blocks say "PRIVATE REASONING"; no event field may
// carry it.
func TestThinkingNeverBecomesAnEvent(t *testing.T) {
	b, _ := json.Marshal(allEvents(readFixture(t)))
	if !bytes.Contains(readFixture(t), []byte("PRIVATE REASONING")) {
		t.Fatal("positive control: the fixture must carry thinking text")
	}
	if bytes.Contains(b, []byte("PRIVATE REASONING")) {
		t.Fatalf("thinking leaked into events: %s", b)
	}
}

func rec(t *testing.T, s string) Record {
	t.Helper()
	r, ok := DecodeLine([]byte(s))
	if !ok {
		t.Fatalf("decode %s", s)
	}
	return r
}

func TestEventsRecordShapes(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []Event
	}{
		{"errored tool result is not ok",
			`{"type":"user","promptId":"p","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"boom","is_error":true}]}}`,
			[]Event{{Kind: KindToolResult, ToolID: "t1", OK: false, Output: "boom"}}},
		{"block-array result content",
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}]}}`,
			[]Event{{Kind: KindToolResult, ToolID: "t2", OK: true, Output: "a\n[image]\nb"}}},
		{"injected context is dropped",
			`{"type":"user","isMeta":true,"message":{"content":[{"type":"text","text":"Base directory for this skill"}]}}`, nil},
		{"a subagent's records are dropped",
			`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"sub"}]}}`, nil},
		{"a prompt written as text blocks",
			`{"type":"user","promptId":"p","message":{"content":[{"type":"text","text":"look"},{"type":"image"}]}}`,
			[]Event{{Kind: KindPrompt, Text: "look"}}},
		{"an API failure is an error, not a reply",
			`{"type":"assistant","isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your limit"}]}}`,
			[]Event{{Kind: KindError, Text: "You've hit your limit"}}},
		{"redacted thinking is dropped",
			`{"type":"assistant","message":{"content":[{"type":"redacted_thinking","data":"x"}]}}`, nil},
		{"non-message records are dropped",
			`{"type":"system","subtype":"turn_duration"}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Events(rec(t, c.line)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestEventsCapEveryField(t *testing.T) {
	big := strings.Repeat("x", MaxOutput*3)
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t", "content": big}}}})
	ev := Events(rec(t, string(line)))
	if len(ev) != 1 || len(ev[0].Output) > MaxOutput || !strings.HasSuffix(ev[0].Output, "…") {
		t.Fatalf("output not capped: len=%d", len(ev[0].Output))
	}
	input, _ := json.Marshal(map[string]any{"command": strings.Repeat("y", MaxInput*2)})
	line, _ = json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "t", "name": "Bash", "input": json.RawMessage(input)}}}})
	ev = Events(rec(t, string(line)))
	if len(ev[0].Input) > MaxInput || len([]rune(ev[0].Target)) > MaxTarget {
		t.Fatalf("input/target not capped: %d %d", len(ev[0].Input), len([]rune(ev[0].Target)))
	}
	// An oversized Edit keeps its path readable: the capped input is still JSON.
	input, _ = json.Marshal(map[string]any{"file_path": "/src/main.go", "old_string": strings.Repeat("o", MaxInput*4), "new_string": "n"})
	capped := CapInput(string(input))
	if len(capped) > MaxInput || ToolTarget("Edit", capped) != "/src/main.go" {
		t.Fatalf("CapInput lost the target: len=%d target=%q", len(capped), ToolTarget("Edit", capped))
	}
	line, _ = json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": strings.Repeat("p", MaxText+10)}})
	if ev = Events(rec(t, string(line))); len(ev[0].Text) > MaxText {
		t.Fatalf("prompt not capped: %d", len(ev[0].Text))
	}
}

func TestCapKeepsRunesWhole(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	for n := 4; n < 20; n++ {
		got := Cap(s, n)
		if len(got) > n || !strings.HasSuffix(got, "…") || !utf8Valid(got) {
			t.Fatalf("Cap(%d) = %q", n, got)
		}
	}
	if Cap("short", 10) != "short" {
		t.Fatal("a short string must be unchanged")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// 🔴 A PARTIAL TRAILING LINE IS NEVER CONSUMED, at EVERY split point of the
// fixture: reading the first k bytes, then the rest once the CLI "finished
// writing", yields exactly the events of one whole read — never a record
// parsed from a prefix, never one twice, never one lost.
func TestReadEventsResumesAtEverySplitPoint(t *testing.T) {
	full := readFixture(t)
	want := allEvents(full)
	for k := 0; k <= len(full); k++ {
		r := bytes.NewReader(full)
		first, off, err := ReadEvents(r, 0, int64(k), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if off > int64(k) || (off > 0 && full[off-1] != '\n') {
			t.Fatalf("split %d: offset %d is not a line boundary within what was written", k, off)
		}
		rest, end, err := ReadEvents(r, off, int64(len(full)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if end != int64(len(full)) {
			t.Fatalf("split %d: ended at %d of %d", k, end, len(full))
		}
		if got := append(first, rest...); !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: events differ:\n got %+v\nwant %+v", k, got, want)
		}
	}
}

func TestReadEventsSkipsALineLongerThanTheWindow(t *testing.T) {
	long := `{"type":"user","message":{"content":"` + strings.Repeat("z", 5000) + `"}}`
	after := `{"type":"user","message":{"content":"after"}}`
	data := []byte(long + "\n" + after + "\n")
	r := bytes.NewReader(data)
	off, events := int64(0), []Event(nil)
	for i := 0; i < 10 && off < int64(len(data)); i++ {
		ev, next, err := ReadEvents(r, off, int64(len(data)), 1024)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev...)
		off = next
	}
	if off != int64(len(data)) || len(events) != 1 || events[0].Text != "after" {
		t.Fatalf("off=%d events=%+v", off, events)
	}
	// Still being written (no newline yet): wait, do not skip.
	r = bytes.NewReader([]byte(long))
	if _, next, _ := ReadEvents(r, 0, int64(len(long)), 1024); next != 0 {
		t.Fatalf("an unterminated long line was consumed (offset %d)", next)
	}
}

func TestBackfillIsBoundedAndStartsOnALine(t *testing.T) {
	var b bytes.Buffer
	for i := 0; i < 500; i++ {
		b.WriteString(`{"type":"user","message":{"content":"prompt ` + strings.Repeat("n", 40) + `"}}` + "\n")
	}
	data := b.Bytes()
	r := bytes.NewReader(data)
	// A window that starts mid-line, and a cap smaller than what it holds.
	lines, off, err := Backfill(r, int64(len(data)), 4000, 20)
	events := Flatten(lines)
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(data)) {
		t.Fatalf("offset %d, want end %d", off, len(data))
	}
	if len(events) != 20 {
		t.Fatalf("%d events, want the newest 20", len(events))
	}
	// Window (no event cap) holds only whole lines: every event decoded cleanly.
	lines, _, _ = Backfill(r, int64(len(data)), 4000, 1000)
	events = Flatten(lines)
	lineLen := len(data) / 500
	if len(events) > 4000/lineLen || len(events) < 4000/lineLen-1 {
		t.Fatalf("window held %d events, want ~%d", len(events), 4000/lineLen)
	}
	// A small file is read from its start.
	lines, _, _ = Backfill(bytes.NewReader(readFixture(t)), int64(len(readFixture(t))), 1<<20, 1000)
	events = Flatten(lines)
	if !reflect.DeepEqual(events, allEvents(readFixture(t))) {
		t.Fatal("a file smaller than the window must backfill whole")
	}
}

func TestToolTarget(t *testing.T) {
	cases := []struct{ tool, input, want string }{
		{"Bash", `{"command":"go test ./...\necho done","description":"Run tests"}`, "go test ./..."},
		{"Read", `{"file_path":"/srv/app/x.go","limit":10}`, "/srv/app/x.go"},
		{"Edit", `{"file_path":"/a/b.go","old_string":"x","new_string":"y"}`, "/a/b.go"},
		{"Grep", `{"pattern":"func   Foo","path":"/src"}`, "func Foo"},
		{"WebFetch", `{"url":"https://example.test","prompt":"summarise"}`, "https://example.test"},
		{"Task", `{"description":"Explore repo","prompt":"long"}`, "Explore repo"},
		{"note_create", `{"title":"Buy milk","body":"2L"}`, "Buy milk"},
		{"mystery", `{"zeta":"z","alpha":"a"}`, "a"},
		{"mystery", `{"n":3}`, ""},
		{"Bash", `not json`, ""},
		{"Bash", `{"command":"` + strings.Repeat("c", 200) + `"}`, strings.Repeat("c", MaxTarget-1) + "…"},
	}
	for _, c := range cases {
		if got := ToolTarget(c.tool, c.input); got != c.want {
			t.Errorf("ToolTarget(%s, %s) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

// Every Line's End is the offset just past ITS OWN newline: reading from any
// End yields exactly the events of the lines after it.
func TestLineEndsAreExactResumePoints(t *testing.T) {
	full := readFixture(t)
	r := bytes.NewReader(full)
	lines, _, err := ReadLines(r, 0, int64(len(full)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 4 {
		t.Fatalf("only %d lines with events", len(lines))
	}
	for i, l := range lines {
		if full[l.End-1] != '\n' {
			t.Fatalf("line %d End %d is not just past a newline", i, l.End)
		}
		rest, _, _ := ReadEvents(r, l.End, int64(len(full)), 1<<20)
		if want := Flatten(lines[i+1:]); !reflect.DeepEqual(rest, want) {
			t.Fatalf("resuming at line %d's End: got %+v want %+v", i, rest, want)
		}
	}
}

func TestBackfillCapSplitsTheOldestLine(t *testing.T) {
	data := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"1"},{"type":"tool_result","tool_use_id":"b","content":"2"}]}}` + "\n" +
		`{"type":"user","message":{"content":"newest"}}` + "\n")
	lines, _, _ := Backfill(bytes.NewReader(data), int64(len(data)), 1<<20, 2)
	got := Flatten(lines)
	if len(got) != 2 || got[0].ToolID != "b" || got[1].Text != "newest" {
		t.Fatalf("got %+v", got)
	}
}
