package ccsubagents

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The SHARED work fixture (testdata/work_parent.jsonl): one parent transcript
// whose Agent calls end every way the ledger knows, in the shapes Claude Code
// 2.1.289 writes (SYNTHETIC: every id, time and text is invented; the repo
// has no recorded transcript with Agent calls in it). internal/mirrorhost's
// and cmd/ccd's tests count the SAME file through their own callers and must
// get 2 (each asserts that literal) — the parity of a mirrored and a dispatched card.
//
//	toolu_fx_old   launched before the CLI started (workFixtureStarted)  — not counted
//	toolu_fx_sync  a sync call with its result                          — done
//	toolu_fx_bg1   a background launch, no word since                   — RUNNING
//	toolu_fx_bg2   a background launch, then its completed notification — done
//	toolu_fx_bg3   a background launch, no word since                   — RUNNING
//	toolu_fx_bg4   a background launch, then a TaskStop naming its agent — killed
const (
	workFixture        = "work_parent.jsonl"
	workFixtureRunning = 2
)

// workFixtureBase is the fixture's time 0; the CLI started 5 s in.
var (
	workFixtureBase    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	workFixtureStarted = workFixtureBase.Add(5 * time.Second).UnixMilli()
)

// copyWorkFixture copies the fixture to dir/name.
func copyWorkFixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", workFixture))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheLedgerCountsTheSharedWorkFixture: the ledger itself, over the shared
// fixture — and without the CLI's start time, the call made before it counts
// too (the cutoff is what drops it).
func TestTheLedgerCountsTheSharedWorkFixture(t *testing.T) {
	p := copyWorkFixture(t, t.TempDir(), "5e55-0002.jsonl")
	now := workFixtureBase.Add(100 * time.Second)
	l := NewLedger(p, workFixtureStarted)
	l.Read()
	if got := l.Running(now); got != workFixtureRunning {
		t.Fatalf("Running = %d, want %d", got, workFixtureRunning)
	}
	all := NewLedger(p, 0)
	all.Read()
	if got := all.Running(now); got != workFixtureRunning+1 {
		t.Fatalf("with no start time: Running = %d, want %d (the earlier call too)", got, workFixtureRunning+1)
	}
}
