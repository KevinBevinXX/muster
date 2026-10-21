package hermeshost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFrame is the idle fixture with its prompt row replaced by
// firstRow and rows inserted after it: what a multi-line draft in the box
// draws (continuation rows carry no prompt).
func fakeFrame(t *testing.T, firstRow string, rows ...string) Screen {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "screens", "idle.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := frameOf(ParseScreen(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	at := f.top + 1
	out := append([]string{}, lines[:at]...)
	out = append(out, firstRow)
	out = append(out, rows...)
	out = append(out, lines[at+1:]...)
	return ParseScreen(strings.Join(out, "\n"))
}

// TestTextInTheBoxCannotFakeAFrame: a draft whose continuation rows draw a
// status bar, a full-width rule and an idle prompt must not be taken for the
// input area. Two holes made it work: the frame's upper rule was the CLOSEST
// same-width rule above the lower one (the draft's own), and rows were
// trimmed of ASCII spaces only, so a typed "❯ " row ended by an NBSP or a
// U+3000 kept its prefix and read as an empty idle prompt — while a plain
// trailing space was trimmed, and the row refused. Now the frame is the
// HIGHEST same-width rule within the box's rows, and rows are trimmed of
// every Unicode space.
func TestTextInTheBoxCannotFakeAFrame(t *testing.T) {
	width := InputWidth(fixture(t, "idle"))
	if width < 40 {
		t.Fatalf("instrument check: width %d", width)
	}
	rule := strings.Repeat("─", width)
	for _, c := range []struct{ name, end string }{
		{"plain space (the control)", " "},
		{"NBSP", " \u00a0"},
		{"U+3000", " \u3000"},
	} {
		s := fakeFrame(t, "❯ my draft", " ☤ stub-model │ fake", rule, "❯"+c.end)
		if err := CheckIdle(s); err == nil {
			t.Errorf("%s: a draft faking a frame is ADMITTED", c.name)
		}
	}
	// The draft's own rule is not the frame's: the first row checked is the
	// real one, which holds the draft.
	s := fakeFrame(t, "❯ my draft", " ☤ stub-model │ fake", rule, "❯ \u00a0")
	if f, err := frameOf(s); err != nil || !strings.HasPrefix(InputRow(s), "❯ my draft") {
		t.Errorf("frame %+v %v: the first input row is %q, not the real one", f, err, InputRow(s))
	}
	// A rule row padded with NBSP or U+3000 is still a rule.
	for _, pad := range []string{"\u00a0", "\u3000", " "} {
		row := []Cell{}
		for _, r := range strings.Repeat("─", 30) + pad {
			row = append(row, Cell{R: r})
		}
		if _, ok := isRule(row); !ok {
			t.Errorf("a rule padded with %q is not a rule", pad)
		}
	}
	// The real idle screen is still idle.
	if err := CheckIdle(fixture(t, "idle")); err != nil {
		t.Fatalf("idle: %v", err)
	}
}
