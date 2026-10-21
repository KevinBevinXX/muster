package hermeshost

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

func calls(id, name, args string) string {
	b, _ := json.Marshal([]map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}})
	return string(b)
}

func kinds(evs []cctranscript.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestRenderIsTheDesignsTable(t *testing.T) {
	cases := []struct {
		name string
		row  Row
		want []cctranscript.Event
	}{
		{"a prompt", Row{ID: 7, Role: "user", Content: "fix the tests"},
			[]cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "fix the tests", Line: 7}}},
		{"a reply", Row{ID: 8, Role: "assistant", Content: "Done."},
			[]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Done."}}},
		{"a reply with a tool call", Row{ID: 9, Role: "assistant", Content: "Running it.", ToolCalls: calls("call_1", "terminal", `{"command":"go test ./..."}`)},
			[]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Running it."},
				{Kind: cctranscript.KindToolCall, ToolID: "call_1", Tool: "terminal", Target: "go test ./...", Input: `{"command":"go test ./..."}`}}},
		{"a tool result", Row{ID: 10, Role: "tool", ToolCallID: "call_1", Content: `{"output":"ok","exit_code":0}`},
			[]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "call_1", OK: true, Output: `{"output":"ok","exit_code":0}`}}},
		{"a failing tool result", Row{ID: 11, Role: "tool", ToolCallID: "call_2", Content: `{"output":"","exit_code":2}`},
			[]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "call_2", OK: false, Output: `{"output":"","exit_code":2}`}}},
		{"a failed turn", Row{ID: 12, Role: "assistant", DisplayKind: "failed_turn", Content: "provider timed out"},
			[]cctranscript.Event{{Kind: cctranscript.KindError, Text: "provider timed out"}}},
		{"an error finish", Row{ID: 13, Role: "assistant", FinishReason: "error"},
			[]cctranscript.Event{{Kind: cctranscript.KindError, Text: "The turn failed."}}},
		{"a known notice kind", Row{ID: 14, Role: "user", DisplayKind: "process_complete", Content: "build finished\nexit 0"},
			[]cctranscript.Event{{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeHermes, Text: "process complete: build finished", Output: "build finished\nexit 0"}}},
		// 🔴 An unknown kind on a USER row is a notice naming it, never the
		// person's bubble.
		{"an unknown display kind", Row{ID: 15, Role: "user", DisplayKind: "brand_new_kind", Content: "injected text"},
			[]cctranscript.Event{{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeHermes, Text: "brand_new_kind: injected text", Output: "injected text"}}},
		{"hidden", Row{ID: 16, Role: "user", DisplayKind: "hidden", Content: "x"}, nil},
		{"system", Row{ID: 17, Role: "system", Content: "you are"}, nil},
		{"session_meta", Row{ID: 18, Role: "session_meta", Content: "{}"}, nil},
		{"reasoning only", Row{ID: 19, Role: "assistant", Content: ""}, nil},
		{"a tool row with no call id", Row{ID: 20, Role: "tool", Content: "x"}, nil},
	}
	for _, c := range cases {
		if got := Render(c.row); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestRenderCapsEveryField(t *testing.T) {
	big := strings.Repeat("x", cctranscript.MaxText+500)
	evs := Render(Row{ID: 1, Role: "user", Content: big})
	if len(evs[0].Text) > cctranscript.MaxText {
		t.Fatalf("prompt %d bytes", len(evs[0].Text))
	}
	evs = Render(Row{ID: 2, Role: "tool", ToolCallID: "c", Content: big})
	if len(evs[0].Output) > cctranscript.MaxOutput {
		t.Fatalf("output %d bytes", len(evs[0].Output))
	}
	evs = Render(Row{ID: 3, Role: "assistant", ToolCalls: calls("c", "terminal", `{"command":"`+big+`"}`)})
	if len(evs[0].Input) > cctranscript.MaxInput {
		t.Fatalf("input %d bytes", len(evs[0].Input))
	}
}

func TestBackfillCollapsesCompactionCopiesAndIndexesEveryPrompt(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	var ids []int64
	for i, m := range []hermestest.Msg{
		{Role: "user", Content: "first prompt", UID: "u1"},
		{Role: "assistant", Content: "first reply", UID: "a1"},
		{Role: "user", Content: "second prompt", UID: "u2"},
		{Role: "assistant", Content: "second reply", UID: "a2"},
	} {
		m.Timestamp = hermestest.Now() + float64(i)
		ids = append(ids, h.Message(root, m))
	}
	// In-place compaction (S2): the old rows go active=0 compacted=1, the
	// retained tail is re-inserted as active rows with the same uids.
	h.Exec(`UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ?`, root)
	h.Message(root, hermestest.Msg{Role: "user", Content: "second prompt", UID: "u2"})
	h.Message(root, hermestest.Msg{Role: "assistant", Content: "second reply", UID: "a2"})

	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	p, err := st.Backfill(ctx(t), root, 100)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range p.Rows {
		texts = append(texts, r.Content)
	}
	if want := []string{"first prompt", "first reply", "second prompt", "second reply"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("rows = %q, want each message once, in order", texts)
	}
	// The live copy won for the retained messages.
	if !p.Rows[2].Active || !p.Rows[3].Active || p.Rows[0].Active {
		t.Fatalf("copies chosen: %+v", p.Rows)
	}
	if p.Cursor <= ids[3] || p.Fingerprint.Active != 2 {
		t.Fatalf("cursor %d fingerprint %+v", p.Cursor, p.Fingerprint)
	}
	if len(p.Prompts) != 2 || p.Prompts[0].Text != "first prompt" || p.Prompts[0].Line != ids[0] {
		t.Fatalf("prompts = %+v", p.Prompts)
	}
	// The window keeps the LAST n, and says there was more; the prompt index
	// still has both.
	p, err = st.Backfill(ctx(t), root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 2 || p.Rows[0].Content != "second prompt" || !p.More || len(p.Prompts) != 2 {
		t.Fatalf("n=2: %+v more=%v prompts=%d", p.Rows, p.More, len(p.Prompts))
	}
}

func TestTailReadsPastTheCursorAndTheFingerprintExplainsAppends(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "one"})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	p, err := st.Backfill(ctx(t), root, 100)
	if err != nil {
		t.Fatal(err)
	}
	a := h.Message(root, hermestest.Msg{Role: "assistant", Content: "two"})
	b := h.Message(root, hermestest.Msg{Role: "user", Content: "three"})
	tp, err := st.Tail(ctx(t), root, p.Cursor, 256)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Rows) != 2 || tp.Rows[0].ID != a || tp.Rows[1].ID != b || tp.Cursor != b {
		t.Fatalf("tail = %+v", tp)
	}
	if tp.Fingerprint != p.Fingerprint.After(tp.Rows) {
		t.Fatalf("appends not explained: %+v vs %+v", tp.Fingerprint, p.Fingerprint.After(tp.Rows))
	}
	// An in-place rewrite moves no id but the revision: not explained.
	h.Exec(`UPDATE messages SET content = 'rewritten' WHERE id = ?`, a)
	tp2, err := st.Tail(ctx(t), root, tp.Cursor, 256)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp2.Rows) != 0 || tp2.Fingerprint == tp.Fingerprint.After(nil) {
		t.Fatalf("a rewrite went unnoticed: %+v", tp2.Fingerprint)
	}
	// A rewind (rows deactivated) is not explained either.
	h.Exec(`UPDATE messages SET active = 0 WHERE id = ?`, b)
	tp3, _ := st.Tail(ctx(t), root, tp.Cursor, 256)
	if tp3.Fingerprint == tp2.Fingerprint {
		t.Fatal("a rewind went unnoticed")
	}
}

func TestTailFollowsARotationIntoTheChild(t *testing.T) {
	h := hermestest.New(t)
	root, child := hermestest.LocalID(), hermestest.ChildID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "before"})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	p, _ := st.Backfill(ctx(t), root, 100)
	h.End(root, "compression")
	h.Session(child, hermestest.SessionOpts{Parent: root})
	c := h.Message(child, hermestest.Msg{Role: "user", Content: "after"})
	tp, err := st.Tail(ctx(t), root, p.Cursor, 256)
	if err != nil {
		t.Fatal(err)
	}
	if SameLineage(tp.Lineage, p.Lineage) || len(tp.Lineage) != 2 {
		t.Fatalf("the rotation did not change the lineage: %v", tp.Lineage.IDs())
	}
	if len(tp.Rows) != 1 || tp.Rows[0].ID != c {
		t.Fatalf("the child's row was not read: %+v", tp.Rows)
	}
}
