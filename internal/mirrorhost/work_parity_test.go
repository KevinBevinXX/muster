package mirrorhost

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAMirroredWindowCountsTheSharedWorkFixture: the PARITY fixture
// (internal/ccsubagents/testdata/work_parent.jsonl), counted through
// muster-mirror's own caller — a busy window's windowWork. cmd/ccd's
// TestADispatchedCardCountsTheSharedWorkFixtureAsTheMirrorDoes counts the same
// file through ccd's and must agree: 2 subagents.
func TestAMirroredWindowCountsTheSharedWorkFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "ccsubagents", "testdata", "work_parent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "5e55-0003.jsonl")
	if err := os.WriteFile(tr, b, 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w := &workCounter{now: func() time.Time { return base.Add(100 * time.Second) }}
	got := w.windowWork([]WindowInfo{{Session: "s", ID: "@1", Claude: &ClaudePane{PID: 9, Gen: "sess-fx", Transcript: tr,
		RawStatus: cctasks.StatusBusy, StartedAt: base.Add(5 * time.Second).UnixMilli()}}})
	if want := (mirror.WindowWork{Subagents: 2}); got["@1"] != want {
		t.Fatalf("the shared fixture counts %+v on a mirrored window, want %+v", got["@1"], want)
	}
}
