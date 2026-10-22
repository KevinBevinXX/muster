package flows

import (
	"sort"
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

// TestOnlyTheClosedIdentifiersAreExemptFromTheAgentTextScreen pins Param.ID:
// the screen in FileRequest skips exactly the parameters marked as closed
// identifiers. The set is a LEDGER (a new exemption is a decision), and every
// member must carry a Pattern — an ID with no pattern would be free text that
// skips the screen.
func TestOnlyTheClosedIdentifiersAreExemptFromTheAgentTextScreen(t *testing.T) {
	n := 0
	var ids []string
	for _, f := range Catalogue() {
		for _, p := range f.Params {
			n++
			if !p.ID {
				continue
			}
			ids = append(ids, f.Name+"."+p.Name)
			if p.Pattern == nil || !strings.HasPrefix(p.Pattern.String(), "^") || !strings.HasSuffix(p.Pattern.String(), "$") {
				t.Errorf("%s.%s is ID but its pattern %v is not an anchored closed shape", f.Name, p.Name, p.Pattern)
			}
		}
	}
	if n < 9 {
		t.Fatalf("walked %d parameters", n)
	}
	want := "muse.poll.session,muse.poll.task,muse.send.marker,muse.send.session,muse.send.task,muse.send.wait"
	sort.Strings(ids)
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("ID parameters = %s\n want %s. A new exemption from the agent-text screen is a decision.", got, want)
	}
}
