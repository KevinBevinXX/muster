package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TWO GO SUITES IN CI ARE PIPED INTO tests/verdict.py UNDER `pipefail`.
//
// 🔴 WHAT `set -o pipefail` CARRIES, AND WHY IT IS PINNED: verdict.py refuses a
// skip, zero verdicts, and any `fail`/`build-fail` event, but it cannot see
// `go test`'s exit status. A run that exits non-zero over a stream with no
// failure event in it is refused by pipefail ALONE — and Actions runs a `run:`
// with no `shell:` under `bash -e`, which has none. Delete that one line and
// the step still looks right and still goes red on every ordinary failure.
//
// 🔴 EXACT LINES, NOT SUBSTRINGS, in the style of ci_concurrency_test.go: the
// `set -o pipefail` must be the line directly above the pipe, in the same
// `run: |` block, and the pipe must end in verdict.py. A deliberate change to
// either step is a deliberate change to this test.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the YAML text. It does not prove how the
// runner's shell behaves, and a `shell:` key added to the step is not
// inspected. The behaviour was measured by running the extracted step text
// under `bash -e` with a `go` that exits 7 on a clean stream: exit 7 with the
// line, exit 0 without it.
// ---------------------------------------------------------------------------

var wantVerdictPipes = map[string]string{
	"test":     "go test -race -cover -json ./... | python3 -u tests/verdict.py --echo summary",
	"ccd-tmux": "go test -tags tmuxit -race -run '^TestTmux' -json ./cmd/ccd/ ./internal/mirror/ ./internal/mirrorhost/ | python3 -u tests/verdict.py --echo all",
}

// TestCIGoSuitesPipeIntoTheVerdictUnderPipefail pins both verdict steps.
func TestCIGoSuitesPipeIntoTheVerdictUnderPipefail(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for job, want := range wantVerdictPipes {
		lines := jobLines(string(b), job)
		// Positive control: the extractor must find the job, or "no pipe found"
		// below would say nothing about the step.
		if len(lines) == 0 {
			t.Errorf("positive control FAILED: no `%s:` job in %s", job, path)
			continue
		}
		var at []int
		for i, l := range lines {
			if strings.Contains(l, "tests/verdict.py --echo") && !strings.HasPrefix(l, "#") {
				at = append(at, i)
			}
		}
		if len(at) != 1 {
			t.Errorf("job %q: %d line(s) run tests/verdict.py --echo, want exactly 1", job, len(at))
			continue
		}
		i := at[0]
		if lines[i] != want {
			t.Errorf("job %q: the verdict line is\n  %s\nwant\n  %s", job, lines[i], want)
		}
		if i < 2 || lines[i-1] != "set -o pipefail" || lines[i-2] != "run: |" {
			t.Errorf("job %q: the verdict pipe must be directly under `run: |` and "+
				"`set -o pipefail`; without pipefail a `go test` that exits non-zero over "+
				"a clean stream is reported green.", job)
		}
	}
}

// jobLines returns the trimmed lines of the two-space-indented `job:` block
// under `jobs:`; nil if there is none.
func jobLines(src, job string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && !strings.HasPrefix(line, "  #") {
			in = line == "  "+job+":"
			continue
		}
		if in {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}
