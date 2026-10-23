package textplain

import "testing"

// titleCases is the title rule's specification. Every input is distinct from
// every other, and every expected output is distinct from every other (checked
// by TestTitleCasesArePairwiseDistinct), so a mutant that returns another
// case's answer, or a constant, cannot pass.
//
// Two halves: markdown that must come out as the text a reader of the rendered
// markdown sees, and ordinary text that is NOT markdown and must come out
// unchanged. The second half is the half the previous stripper failed most: it
// removed every ` * _ # > anywhere in the string.
var titleCases = []struct {
	name, in, want string
}{
	// --- markdown → visible text ---
	{"bold and code", "**Fix** the `login` flow", "Fix the login flow"},
	{"link", "See [the docs](https://example.com/docs) now", "See the docs now"},
	{"strikethrough", "~~old~~ new plan", "old new plan"},
	{"image", "![diagram](img/a.png) overview", "diagram overview"},
	{"autolink", "<https://example.org/x> autolink", "https://example.org/x autolink"},
	{"email autolink", "mail <dev@example.org> today", "mail dev@example.org today"},
	{"html tags", `<b>Bold</b> and <span class="x">span</span>`, "Bold and span"},
	{"bold inside link text", "[**strong** link](https://example.net)", "strong link"},
	{"escaped chars", `\*not emphasis\* here`, "*not emphasis* here"},
	{"ordered list number", "1. First step", "First step"},
	{"table pipes", "| Col A | Col B |\n|---|:---:|\n| one | two |", "Col A Col B one two"},
	{"atx heading with closer", "## Heading text ##", "Heading text"},
	{"blockquote", "> quoted line", "quoted line"},
	{"task list item", "- [x] checked item", "checked item"},
	{"underscore italic", "_italic_ word", "italic word"},
	{"triple star", "***both*** marks", "both marks"},
	{"nested emphasis", "**bold _and_ italic**", "bold and italic"},
	{"entity decoded", "Tom &amp; Jerry", "Tom & Jerry"},
	{"entity tag stays literal", "&lt;b&gt; literal", "<b> literal"},
	{"entities decoded once", "&amp;lt; once", "&lt; once"},
	{"whitespace collapsed", "multi\n\nline   title", "multi line title"},
	{"fenced block dropped", "```go\ncode()\n```\nafter fence", "after fence"},
	{"code span is literal", "`**kwargs` stays", "**kwargs stays"},
	{"reference link", "[ref text][r1] and more\n[r1]: https://x.example", "ref text and more"},
	{"double underscore strong", "__dunder strong__", "dunder strong"},
	{"html comment", "<!-- hidden -->visible", "visible"},
	{"block tag is a break", "line<br>break", "line break"},
	{"bullet", "* bulleted entry", "bulleted entry"},
	{"bold holding a star", "**2*3** done", "2*3 done"},
	{"table without outer pipes", "name | count\n---|---\nalpha | 1", "name count alpha 1"},
	{"a pipe far above a table", "a | b\n\ntext\n\n| x |\n|---|", "a | b text x"},
	{"link with a title", `see [the guide](https://example.com/g "Guide") now`, "see the guide now"},
	{"link in parentheses", "(see [notes](https://example.com/n))", "(see notes)"},
	{"CJK bold", "**太字**テスト", "太字テスト"},
	{"CJK italic", "これは*強調*です", "これは強調です"},
	{"code span keeps entities", "`&amp;` and &amp;", "&amp; and &"},

	// --- NOT markdown: must not change ---
	{"snake_case", "update snake_case_names in config_loader", "update snake_case_names in config_loader"},
	{"a*b math", "compute a*b for the matrix", "compute a*b for the matrix"},
	{"intraword stars", "2*3*4 equals 24", "2*3*4 equals 24"},
	{"power operator", "x**2 + y**2", "x**2 + y**2"},
	{"path with underscores", "internal/ui/agent_card_view.go cleanup", "internal/ui/agent_card_view.go cleanup"},
	{"dunder file", "fix __init__.py imports", "fix __init__.py imports"},
	{"C# and F#", "port the parser to C# / F#", "port the parser to C# / F#"},
	{"issue refs", "#123 follow-up for issue #456", "#123 follow-up for issue #456"},
	{"email", "mail first_last@example.com about it", "mail first_last@example.com about it"},
	{"url is the whole title", "https://github.com/acme/repo/pull/12_a?q=*y*", "https://github.com/acme/repo/pull/12_a?q=*y*"},
	{"generics", "List<String> to Map<K, V>", "List<String> to Map<K, V>"},
	{"comparison", "a < b > c comparison", "a < b > c comparison"},
	{"bracket tag", "[WIP] refactor queue", "[WIP] refactor queue"},
	{"inline pipe", "muster | dispatch form", "muster | dispatch form"},
	{"decimal", "3.14 is roughly pi", "3.14 is roughly pi"},
	{"spaced star", "rate * 2 hours", "rate * 2 hours"},
	{"windows path", `C:\Users\dev\notes`, `C:\Users\dev\notes`},
	{"go generics signature", "func Map[K comparable, V any](m map[K]V)", "func Map[K comparable, V any](m map[K]V)"},
	{"index then call", "call fns[0](arg) once", "call fns[0](arg) once"},
	{"double index", "read grid[0][1] value", "read grid[0][1] value"},
	{"type params", "type List[T any](x T) helper", "type List[T any](x T) helper"},
	{"bracket then spaced parens", "generic [K comparable, V any](m map[K]V) form", "generic [K comparable, V any](m map[K]V) form"},
	{"spaced image-like", "![not an image](a b c)", "![not an image](a b c)"},
	{"plain", "Plain words only", "Plain words only"},
	{"empty", "", ""},
}

func TestTitle(t *testing.T) {
	for _, c := range titleCases {
		t.Run(c.name, func(t *testing.T) {
			if got := Title(c.in); got != c.want {
				t.Errorf("Title(%q)\n     got %q\n    want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTitleCasesArePairwiseDistinct holds the table to its own premise: if two
// cases shared an input or an expected output, a mutant returning the other
// case's value could pass one of them.
func TestTitleCasesArePairwiseDistinct(t *testing.T) {
	ins, wants, names := map[string]string{}, map[string]string{}, map[string]bool{}
	for _, c := range titleCases {
		if names[c.name] {
			t.Errorf("duplicate case name %q", c.name)
		}
		names[c.name] = true
		if o, ok := ins[c.in]; ok {
			t.Errorf("cases %q and %q share an input", o, c.name)
		}
		ins[c.in] = c.name
		if o, ok := wants[c.want]; ok {
			t.Errorf("cases %q and %q share an expected output", o, c.name)
		}
		wants[c.want] = c.name
	}
}

// TestTitleIsIdempotent: a title already plain stays put, so a sink that is
// (harmlessly) reached twice, or a stored title that happens to be plain, reads
// the same either way.
func TestTitleIsIdempotent(t *testing.T) {
	for _, c := range titleCases {
		once := Title(c.in)
		// The documented exceptions turn an escape into the literal character
		// it stood for, which a second pass then reads as markup: entities are
		// decoded ONCE ("&lt; once"), and a backslash escape becomes the bare
		// marker ("*not emphasis*").
		switch c.name {
		case "entities decoded once", "entity tag stays literal", "escaped chars", "code span keeps entities":
			continue
		}
		if twice := Title(once); twice != once {
			t.Errorf("%s: Title is not idempotent: %q → %q → %q", c.name, c.in, once, twice)
		}
	}
}

// TestTitleCannotForgeAStashReference: the sentinel byte is removed from the
// input, so a title cannot smuggle a reference to another span's stash.
func TestTitleCannotForgeAStashReference(t *testing.T) {
	if got := Title("`a` \x000\x00 b"); got != "a 0 b" {
		t.Errorf("got %q, want %q", got, "a 0 b")
	}
}
