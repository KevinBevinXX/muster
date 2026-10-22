package flows

import (
	"strings"
	"testing"
)

func TestVisibleEscapeIsOneToOne(t *testing.T) {
	for _, v := range []string{"", "plain", `back\slash`, "two\nlines\tand tab", "caf\u00E9 \u043Ek \U0001F600",
		"hello" + strings.Repeat("\U000E0041", 3), "\u202Egnp.exe", `\u{41}`, `\n`} {
		esc := VisibleEscape(v)
		for _, r := range esc {
			if r < 0x20 || r > 0x7e {
				t.Fatalf("VisibleEscape(%q) = %q carries a non-printable-ASCII rune %U", v, esc, r)
			}
		}
		back, ok := UnescapeVisible(esc)
		if !ok || back != v {
			t.Fatalf("round trip of %q: escaped %q, decoded %q ok=%v", v, esc, back, ok)
		}
	}
	// Distinct inputs that LOOK alike must escape differently.
	if VisibleEscape("ok") == VisibleEscape("\u043Ek") {
		t.Fatal("a Cyrillic look-alike escapes the same as its ASCII twin")
	}
	for _, bad := range []string{`\`, `\x`, `\u{`, `\u{zz}`, "raw\nnewline", "caf\u00E9"} {
		if _, ok := UnescapeVisible(bad); ok {
			t.Errorf("UnescapeVisible(%q) accepted something VisibleEscape cannot produce", bad)
		}
	}
}

// TestOnlyTheTaskIDIsExemptFromTheAgentTextScreen pins Param.ID: the screen in
// FileRequest skips exactly the parameters marked as closed identifiers, and
// that mark may sit only on a parameter whose pattern is the task-id pattern.
func TestOnlyTheTaskIDIsExemptFromTheAgentTextScreen(t *testing.T) {
	n := 0
	for _, f := range Catalogue() {
		for _, p := range f.Params {
			if p.ID != (p.Pattern == taskRE) {
				t.Errorf("%s.%s: ID=%v but its pattern is task-id=%v", f.Name, p.Name, p.ID, p.Pattern == taskRE)
			}
			n++
		}
	}
	if n < 5 {
		t.Fatalf("walked %d parameters", n)
	}
}
