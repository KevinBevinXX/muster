package hermeshost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// 🔴 NEVER THE PERSON's BUBBLE (round-1 audit, #190 🟡1): a compaction
// summary (_compressed_summary=1, a user-role row Hermes writes) and a
// model-only row (display_metadata.model_only: a micro-compaction merge the
// model reads but nobody typed) are never drawn — upstream never renders
// them (P:hermes_state_messages.py:1090, P:gateway/session_controls.py:106).
// On the pinned schema, through the backfill, the prompt index and the tail;
// the metadata in both encodings Hermes has written (object, and the
// pre-guard double-encoded string).
func TestModelOnlyAndSummaryRowsAreNeverDrawn(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "a real prompt"})
	h.Message(root, hermestest.Msg{Role: "user", Content: "SUMMARY-ROW of the earlier turns", CompressedSummary: true})
	h.Message(root, hermestest.Msg{Role: "user", Content: "MERGED-ROW one", DisplayMetadata: `{"model_only": true}`})
	h.Message(root, hermestest.Msg{Role: "user", Content: "MERGED-ROW two", DisplayMetadata: `"{\"model_only\": true}"`})
	h.Message(root, hermestest.Msg{Role: "user", Content: "a prompt with other metadata", DisplayMetadata: `{"task_count": 2}`})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	p, err := st.Backfill(ctx(t), root, 100)
	if err != nil {
		t.Fatal(err)
	}
	var drawn []string
	for _, r := range p.Rows {
		for _, ev := range Render(r) {
			drawn = append(drawn, ev.Kind+":"+ev.Text)
		}
	}
	got := strings.Join(drawn, "|")
	if strings.Contains(got, "SUMMARY-ROW") || strings.Contains(got, "MERGED-ROW") {
		t.Fatalf("the backfill drew a row nobody typed: %s", got)
	}
	if !strings.Contains(got, "prompt:a real prompt") || !strings.Contains(got, "prompt:a prompt with other metadata") {
		t.Fatalf("a real prompt was dropped: %s", got)
	}
	for _, ref := range p.Prompts {
		if strings.Contains(ref.Text, "SUMMARY") || strings.Contains(ref.Text, "MERGED") {
			t.Fatalf("the prompt index lists %q", ref.Text)
		}
	}
	if len(p.Prompts) != 2 {
		t.Fatalf("prompt index = %+v, want the two real prompts", p.Prompts)
	}
	// The tail too.
	h.Message(root, hermestest.Msg{Role: "user", Content: "SUMMARY-ROW live", CompressedSummary: true})
	tp, err := st.Tail(ctx(t), root, p.Cursor, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tp.Rows {
		if evs := Render(r); len(evs) != 0 {
			t.Fatalf("the tail drew a summary row: %+v", evs)
		}
	}
	if len(tp.Rows) != 1 {
		t.Fatalf("instrument check: the tail read %d rows", len(tp.Rows))
	}
}
