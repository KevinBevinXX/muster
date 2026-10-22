package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/textplain"
)

// snippetBodies are representative task bodies: a drafter's report, a
// ClickUp-mirrored task with a rule and a footer, one that opens with a fence,
// one with a table, a one-line title-ish body, a body whose first lines are all
// markup, a long body (the bench input), and ones whose visible text runs past
// the snippet on line one or only after many short lines.
var snippetBodies = map[string]string{
	"drafter":     "**🤖 task-drafter** · `id`\n> ⚠ verify first\n\n```json\n{\"goal\":\"\"}\n```\n- do a\n- do b",
	"clickup":     "## Rotate the [build key](https://example.com/k)\n\nThe key in `ci/secrets` expires on **Friday**.\n\n---\nMirrored from ClickUp · task 868abc",
	"fence first": "```sh\nmake deploy\n```\nThen check the **dashboard** and [the logs](https://example.com/l).",
	"table":       "Results:\n\n| host | status |\n|---|---|\n| a1 | ok |\n| b2 | **down** |\n\nFollow up with the on-call.",
	"bare table":  "name | count\n---|---\nalpha | 1\nbeta | 2",
	"one line":    "Fix the [login page](https://example.com/login) redirect for C# clients",
	"all markup":  "---\n\n***\n\n```\nx\n```\n\nfinally some text",
	"long line":   strings.Repeat("A very long first line with **bold** and `code` words. ", 20),
	"many short":  strings.Repeat("- ok\n", 60) + "the end",
	"big":         bigBody(),
	// A table header as the LAST line of the first doubling step (line 8), its
	// separator on line 9: plaining only lines 1-8 would read the header's pipe
	// as text. The snippet must plain line 9 as well.
	"header at the cut": strings.Repeat("a\n", 7) + "h1 | " + strings.Repeat("long header words ", 10) + "\n---|---\nc1 | c2",
}

func bigBody() string {
	var b strings.Builder
	for b.Len() < 100_000 {
		b.WriteString("## Step\n\nRun `make check` in **internal/ui** and read [the guide](https://example.com/g).\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nfunc x() {}\n```\n\n")
	}
	return b.String()
}

// TestTheSnippetEqualsTheWholeBodysPlainText: the snippet plains only the
// first lines of a body, enough to fill it, and must show EXACTLY what plaining
// the whole body and cutting it shows — for every representative body, at the
// card's width and at a few others (a boundary, a middle, a wide one).
func TestTheSnippetEqualsTheWholeBodysPlainText(t *testing.T) {
	for name, body := range snippetBodies {
		for _, n := range []int{1, 40, 120, 500} {
			want := truncate(textplain.Title(body), n)
			if got := markdownSnippet(body, n); got != want {
				t.Errorf("%s @%d:\n got %q\nwant %q", name, n, got, want)
			}
		}
	}
}

// BenchmarkMarkdownSnippet is the per-card cost on the board: a ~100KB body.
func BenchmarkMarkdownSnippet(b *testing.B) {
	body := bigBody()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		_ = markdownSnippet(body, 120)
	}
}
