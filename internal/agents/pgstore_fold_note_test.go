package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestTheFoldBoundIsOnTheEncodedSize (#209 round-2 🟢4): ccd's read limit
// applies to the JSON-encoded turn, where `<`, `>` and `&` take six bytes
// each. A `<>&`-dense text that fits the bound in raw bytes but not encoded is
// NOT added (FoldOverLimit; the over line is, once), and a plain text of the
// same raw size is (FoldAdded) — the control that the bound is not simply
// smaller.
func TestTheFoldBoundIsOnTheEncodedSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)
	mk := func(suffix string) Agent {
		t.Helper()
		name := "fold-note-" + suffix + "-" + time.Now().Format("150405.000000")
		a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning, PendingNote: "first"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { cleanupAgent(ctx, store.pool, a.ID) })
		return a
	}
	const over = " [over the limit]"
	dense := strings.Repeat("<>&", 100) // 300 bytes raw, 1800 encoded
	if b, _ := json.Marshal(dense); len(b)-2 != 1800 {
		t.Fatalf("instrument: the dense text encodes to %d bytes, want 1800", len(b)-2)
	}
	const bound = 1000 // raw "first"+300 fits; encoded 5+1800 does not

	html := mk("html")
	res, err := store.FoldIntoPendingNote(ctx, html.ID, dense, "", bound, over)
	if err != nil || res != FoldOverLimit {
		t.Fatalf("a <>&-dense text past the ENCODED bound: %v %v; want FoldOverLimit", res, err)
	}
	if got, _ := store.Get(ctx, html.ID); strings.Contains(got.PendingNote, "<>&") || !strings.HasSuffix(got.PendingNote, over) {
		t.Fatalf("note %q: want the over line, not the text", got.PendingNote)
	}

	plain := mk("plain")
	if res, err := store.FoldIntoPendingNote(ctx, plain.ID, strings.Repeat("abc", 100), "", bound, over); err != nil || res != FoldAdded {
		t.Fatalf("control: a plain text of the same raw size: %v %v; want FoldAdded", res, err)
	}
}
