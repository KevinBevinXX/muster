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
	return fakeFrameOn(t, "idle", firstRow, rows...)
}

// fakeFrameOn is fakeFrame on the named fixture.
func fakeFrameOn(t *testing.T, name, firstRow string, rows ...string) Screen {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "screens", name+".ansi"))
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
// HIGHEST same-width rule within the box's rows (of those with the real
// status bar above, TestADraftCannotFakeTheStatusBarAnchor), and rows are trimmed of
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

// TestADraftCannotFakeTheStatusBarAnchor: the upper rule is the highest one
// whose row above is the REAL status bar, by its text and its background.
// While the bar is blank (the command palette or the model picker open) a
// draft that types a status bar's TEXT, a full-width rule and an idle prompt
// must not become the frame: a draft draws no background, so nothing is
// anchored and the highest rule (the real one, its status row blank) is
// refused. Matched by text alone, the draft's rule would be the frame and the
// screen ADMITTED with the palette open.
func TestADraftCannotFakeTheStatusBarAnchor(t *testing.T) {
	for _, name := range []string{"palette", "model-picker"} {
		real, err := frameOf(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: instrument check: %v", name, err)
		}
		rule := strings.Repeat("─", real.width)
		s := fakeFrameOn(t, name, "❯ my draft", " ☤ stub-model │ fake", rule, "❯ \u00a0")
		if err := CheckIdle(s); err == nil {
			t.Errorf("%s: a draft faking the status bar's text under a blank bar is ADMITTED", name)
		} else if rule, _ := IsScreenRefusal(err); rule != ScreenNoStatusBar {
			t.Errorf("%s: refused %q, want the blank bar's refusal %q", name, rule, ScreenNoStatusBar)
		}
	}
}

// TestATranscriptRuleAboveTheStatusBarIsNotTheFrame: a reply line of '─'
// as wide as the pane, within the box's rows above the status bar, is not
// the input's upper rule: the idle screen stays admitted, with the reply's
// rule two rows above the bar (directly above the reply box's bottom, as
// Hermes draws it) and directly above it. The recorded reply-rule-* screens
// are the live half.
func TestATranscriptRuleAboveTheStatusBarIsNotTheFrame(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "screens", "idle.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := frameOf(ParseScreen(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	rule := strings.Repeat("─", f.width)
	for _, above := range []int{1, 2, 3} {
		out := append([]string{}, lines[:f.status-above+1]...)
		out = append(out, rule)
		out = append(out, lines[f.status-above+1:]...)
		// Keep the row count: drop the transcript's first row.
		s := ParseScreen(strings.Join(out[1:], "\n"))
		if err := CheckIdle(s); err != nil {
			t.Errorf("a transcript rule %d row(s) above the status bar: %v", above, err)
		}
	}
}

// TestTheBackgroundIsParsed: a cell carries whether a background colour is
// set — 4x, 10x and 48;… set it, 49, 0 and an empty SGR clear it — and an
// extended FOREGROUND colour numbered 48 (38;5;48) does not.
func TestTheBackgroundIsParsed(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want string // one letter per cell: B background, . none
	}{
		{"\x1b[48;5;234mab\x1b[0mc", "BB."},
		{"\x1b[44ma\x1b[49mb", "B."},
		{"\x1b[104ma\x1b[mb", "B."},
		{"\x1b[48;2;1;2;3ma\x1b[39mb\x1b[0mc", "BB."},
		{"\x1b[38;5;48ma\x1b[38;2;48;48;48mb", ".."},
		{"\x1b[3;48;5;1ma\x1b[23mb", "BB"},
	} {
		var got strings.Builder
		for _, cell := range ParseScreen(c.raw).Rows[0] {
			if cell.Bg {
				got.WriteByte('B')
			} else {
				got.WriteByte('.')
			}
		}
		if got.String() != c.want {
			t.Errorf("%q: background %s, want %s", c.raw, got.String(), c.want)
		}
	}
	// The style carries into the next row, as tmux's capture leaves it.
	if s := ParseScreen("\x1b[48;5;234ma\nb"); !s.Rows[1][0].Bg {
		t.Error("the background does not carry into the next row")
	}
}

// TestEvenAnAnchoredDraftIsNotTheFrame: of the anchored rules the HIGHEST is
// the frame, never the closest. At the pin a draft draws no background, but a
// skin may give the input area one; a draft row that then carries a status
// bar's text AND a background, over a full-width rule and an idle prompt,
// must still lose to the real bar above it: the real first row holds the
// draft, and the screen is refused as text in the box.
func TestEvenAnAnchoredDraftIsNotTheFrame(t *testing.T) {
	width := InputWidth(fixture(t, "idle"))
	rule := strings.Repeat("─", width)
	s := fakeFrame(t, "❯ my draft", "\x1b[48;5;234m ☤ stub-model │ fake\x1b[0m", rule, "❯ \u00a0")
	fake := -1
	for i := range s.Rows {
		if strings.Contains(rowText(s.Rows[i]), "│ fake") {
			fake = i
		}
	}
	if fake < 0 || !statusBarAt(s, fake) {
		t.Fatal("instrument check: the draft's fake bar is not anchored, so this tests nothing")
	}
	err := CheckIdle(s)
	if err == nil {
		t.Fatal("an anchored fake frame in the draft is ADMITTED")
	}
	if rule, _ := IsScreenRefusal(err); rule != ScreenText {
		t.Errorf("refused %q, want %q (the real frame, holding the draft)", rule, ScreenText)
	}
}

// withStatusRow is the idle fixture with its status bar row replaced by row
// (raw, with its own SGR).
func withStatusRow(t *testing.T, row string) Screen {
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
	lines[f.status] = row
	return ParseScreen(strings.Join(lines, "\n"))
}

// TestTheSubagentDockIsNotTheStatusBar: with the bar hidden
// (display.statusbar off, or just after a resize) the row above the upper
// rule can be the subagent dock, which Hermes draws on the BAR'S OWN
// background (skin: subagent-dock is bg:{status_bg}) with a goal a model
// wrote. A dock row whose text reads like a bar must not be taken for it:
// the bar starts ` ☤ ` or with the battery label (`🔋 82%`/`⚡ 82%`, then
// ` │ `), and a dock row starts with two spaces (a goal, a process, a queued
// prompt) or a localised heading. Refused NoStatusBar; the real bar with and
// without a battery label is still the bar.
func TestTheSubagentDockIsNotTheStatusBar(t *testing.T) {
	const bg = "\x1b[38;5;250m\x1b[48;5;234m"
	for _, row := range []string{
		bg + "  x │ ☤ m" + "\x1b[0m",
		bg + " 1 live · │ ☤ m" + "\x1b[0m",
		bg + " Subagents │ ☤ m" + "\x1b[0m",
		bg + "   1. x │ ☤ m" + "\x1b[0m",
	} {
		s := withStatusRow(t, row)
		err := CheckIdle(s)
		if err == nil {
			t.Errorf("%q: a dock row reading like the bar is ADMITTED as the bar", row)
			continue
		}
		if rule, _ := IsScreenRefusal(err); rule != ScreenNoStatusBar {
			t.Errorf("%q: refused %q, want %q", row, rule, ScreenNoStatusBar)
		}
	}
	for _, row := range []string{
		bg + " ☤ stub-model │ ctx --" + "\x1b[0m",
		bg + " 🔋 82% │ ☤ stub-model │ ctx --" + "\x1b[0m",
		bg + " ⚡ 7% │ ☤ stub-model" + "\x1b[0m",
	} {
		if err := CheckIdle(withStatusRow(t, row)); err != nil {
			t.Errorf("%q: the real bar is refused: %v", row, err)
		}
	}
}

// TestAPaneWithNoRowAboveTheBarIsRefused: on a pane exactly as tall as the
// input chrome there is no row above the bar, so an open stash panel has no
// visible row at all (measured live: 100x4, 20 drafts, admitted). Refused
// as too short; one row more and the check reads that row again.
func TestAPaneWithNoRowAboveTheBarIsRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "screens", "idle.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := frameOf(ParseScreen(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if err := CheckIdle(ParseScreen(strings.Join(lines[f.status:], "\n"))); err == nil {
		t.Error("no row above the status bar: ADMITTED")
	} else if rule, _ := IsScreenRefusal(err); rule != ScreenTooShort {
		t.Errorf("no row above the status bar: refused %q, want %q", rule, ScreenTooShort)
	}
	if err := CheckIdle(ParseScreen(strings.Join(lines[f.status-1:], "\n"))); err != nil {
		t.Errorf("one row above the status bar: %v", err)
	}
}
