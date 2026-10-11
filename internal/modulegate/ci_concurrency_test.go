package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ONLY A PULL REQUEST'S CI RUNS MAY CANCEL EACH OTHER.
//
// 🔴 THE FINDING: the workflow's top-level group was `ci-${{ github.ref }}`
// with `cancel-in-progress: true`, so a push to `main` cancelled the run of the
// `main` commit before it — that commit was left with no CI verdict at all.
//
// 🔴 TURNING `cancel-in-progress` OFF IS NOT THE FIX, WHICH IS WHY THIS PINS THE
// GROUP AND NOT ONLY THE FLAG. A group holds one running and ONE pending run; a
// newer pending run replaces the older pending one regardless of that flag, so
// a shared group still drops the middle run of a burst. Every non-PR run must
// be in a group of its own (`github.run_id`).
//
// 🔴 EXACT STRING, NOT A SUBSTRING. The obvious substring check — "the group
// mentions run_id" — accepts `github.ref || github.run_id`, which is always the
// ref, i.e. the original bug. The block is three lines; a deliberate change to
// it is a deliberate change to this test.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the YAML text, so it proves the expression
// is spelled as intended, never how GitHub evaluates it. The PR-cancel half was
// observed live on the PR that added this test; the push-to-`main` half can
// only be observed after a merge.
// It also reads ONLY the workflow-level block: a job-level `concurrency:` key is
// not inspected, deliberately — a job may legitimately serialise itself.
// ---------------------------------------------------------------------------

const wantCIConcurrency = `concurrency:
  group: ci-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}`

// TestCIConcurrencyCancelsOnlyPullRequestRuns pins the workflow-level
// concurrency block.
func TestCIConcurrencyCancelsOnlyPullRequestRuns(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := topLevelBlock(string(b), "concurrency")
	// Positive control: the extractor must find the block, or the comparison
	// below fails for a reason that says nothing about the block's content.
	if got == "" {
		t.Fatalf("positive control FAILED: no top-level `concurrency:` block in %s", path)
	}
	if got != wantCIConcurrency {
		t.Errorf("top-level concurrency block in %s is\n%s\n\nwant\n%s\n\n"+
			"PR runs must share a per-ref, cancelling group; every other event must get "+
			"its own `github.run_id` group, or a `main` commit can be cancelled or dropped.",
			path, got, wantCIConcurrency)
	}
}

// topLevelBlock returns the first column-0 `key:` line plus the indented lines
// under it, trailing blank lines trimmed; "" if there is none.
func topLevelBlock(src, key string) string {
	var block []string
	for _, line := range strings.Split(src, "\n") {
		if block == nil {
			if strings.HasPrefix(line, key+":") {
				block = append(block, line)
			}
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		block = append(block, line)
	}
	return strings.TrimRight(strings.Join(block, "\n"), " \n")
}
