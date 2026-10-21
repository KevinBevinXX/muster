package hermeshost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// screenRuleNames names the screen check's refusals as screens.json does.
var screenRuleNames = map[string]string{
	"NoLayout": ScreenNoLayout, "Below": ScreenBelow, "NoStatusBar": ScreenNoStatusBar, "Stash": ScreenStash,
	"Vi": ScreenVi, "Images": ScreenImages, "NotIdle": ScreenNotIdle, "Text": ScreenText, "NotPasted": ScreenNotPasted,
}

func fixture(t *testing.T, name string) Screen {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "screens", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return ParseScreen(string(b))
}

// answer is a check's answer as screens.json spells it.
func answer(err error) string {
	if err == nil {
		return "admit"
	}
	rule, ok := IsScreenRefusal(err)
	if !ok {
		return "error: " + err.Error()
	}
	for k, v := range screenRuleNames {
		if v == rule {
			return "refuse: " + k
		}
	}
	return "refuse: " + rule
}

// TestScreenFixtures: the screen check on every screen the pinned Hermes
// drew (recorded by `make test-livehermes`, docs/design/hermes.md Phase 3)
// gives the verdict screens.json — written by hand, never from the check —
// expects. 🔴 Every blocking state is refused: a paste never reaches an
// approval, a clarify, a sudo prompt, /new's confirm or the consent prompt.
func TestScreenFixtures(t *testing.T) {
	exp := screenExpectations(t)
	counts := map[string]int{}
	for name, e := range exp {
		s := fixture(t, name)
		// A rule may name alternatives ("NoLayout|Below") for a transient
		// state; the recorded fixture must give one of them.
		want, got := e.Expect, answer(CheckIdle(s))
		ok := got == want
		if want == "refuse" {
			want += ": " + e.Rule
			for _, alt := range strings.Split(e.Rule, "|") {
				ok = ok || got == "refuse: "+alt
			}
		}
		if !ok {
			t.Errorf("%s (%s): CheckIdle says %q, want %q", name, e.State, got, want)
		}
		counts[e.Expect]++
		if e.Pasted != "" {
			if got := answer(CheckPasted(s, e.Pasted)); got != e.PastedExpect {
				t.Errorf("%s: CheckPasted says %q, want %q", name, got, e.PastedExpect)
			}
		}
	}
	// The pair, never the zero: the admits and the refusals both counted.
	if counts["admit"] < 3 || counts["refuse"] < 20 {
		t.Fatalf("instrument check: %v", counts)
	}
	for _, blocking := range []string{"approval", "approval-de", "clarify-choices", "clarify-freetext", "sudo", "new-confirm", "hook-consent", "busy", "secret", "connection"} {
		if err := CheckIdle(fixture(t, blocking)); err == nil {
			t.Errorf("🔴 %s is ADMITTED: a paste could reach it", blocking)
		}
	}
}

// TestCheckPastedComparesTheWholeBox: on the recorded box holding "hello
// from muster", only that text passes; a text differing in one character, a
// prefix of it, and a longer one are refused.
func TestCheckPastedComparesTheWholeBox(t *testing.T) {
	s := fixture(t, "pasted-short")
	if err := CheckPasted(s, "hello from muster"); err != nil {
		t.Fatalf("the exact text: %v", err)
	}
	if err := CheckPasted(s, "  hello from muster\n"); err != nil {
		t.Fatalf("the text with surrounding whitespace (the box shows no trailing newline): %v", err)
	}
	for _, other := range []string{"hello from mister", "hello from", "hello from muster!", ""} {
		if got := answer(CheckPasted(s, other)); got != "refuse: NotPasted" {
			t.Errorf("CheckPasted(%q) = %q, want refuse: NotPasted", other, got)
		}
	}
}

// TestCheckPastedPlaceholderNeedsTheLineCount: the collapsed paste's
// placeholder says 8 lines; a message of 7 or 9 lines is refused, and so is
// the placeholder for a message the box does not show.
func TestCheckPastedPlaceholderNeedsTheLineCount(t *testing.T) {
	s := fixture(t, "pasted-collapsed")
	lines := func(n int) string {
		var b []string
		for i := 1; i <= n; i++ {
			b = append(b, "x")
		}
		return strings.Join(b, "\n")
	}
	if err := CheckPasted(s, lines(8)); err != nil {
		t.Fatalf("8 lines: %v", err)
	}
	for _, n := range []int{1, 7, 9} {
		if err := CheckPasted(s, lines(n)); err == nil {
			t.Errorf("%d lines admitted against an 8-line placeholder", n)
		}
	}
}

// TestInputRowsMatchesTheRecordedBoxes: the estimate is above the box's 8
// rows for the paste that scrolled its prompt away, and within it for the
// ones that showed whole, at the recorded pane's width and prompt.
func TestInputRowsMatchesTheRecordedBoxes(t *testing.T) {
	exp := screenExpectations(t)
	idle := fixture(t, "idle")
	w, pw := InputWidth(idle), PromptWidth(idle)
	if w < 100 || pw != 2 {
		t.Fatalf("instrument check: width %d, prompt width %d", w, pw)
	}
	for _, name := range []string{"pasted-short", "pasted-multiline", "pasted-wrapped"} {
		if n := InputRows(exp[name].Pasted, pw, w); n > MaxInputRows || n < 1 {
			t.Errorf("%s: estimated %d rows; it showed whole", name, n)
		}
	}
	if n := InputRows(exp["pasted-wrapped"].Pasted, pw, w); n < 2 {
		t.Errorf("pasted-wrapped wrapped on screen but is estimated at %d row", n)
	}
	if n := InputRows(exp["pasted-scrolled"].Pasted, pw, w); n <= MaxInputRows {
		t.Errorf("pasted-scrolled: estimated %d rows; its prompt row scrolled out of the box", n)
	}
	if Collapses(exp["pasted-multiline"].Pasted) || !Collapses(exp["pasted-collapsed"].Pasted) {
		t.Error("Collapses disagrees with what Hermes did to the recorded pastes")
	}
}

// TestLooksLikeHermes: the guard's chrome check sees Hermes's idle and its
// modals, and NEVER Claude Code — whose idle is also a ❯ between two rules —
// in any screen internal/askdrive recorded, nor a shell or the consent
// prompt (before the UI).
func TestLooksLikeHermes(t *testing.T) {
	for _, name := range []string{"idle", "approval", "busy", "idle-profile", "idle-de", "text-in-box"} {
		if !LooksLikeHermes(fixture(t, name)) {
			t.Errorf("%s: not recognised as Hermes", name)
		}
	}
	claude, err := filepath.Glob(filepath.Join("..", "askdrive", "testdata", "*.txt"))
	if err != nil || len(claude) < 10 {
		t.Fatalf("instrument check: %d Claude Code screens (%v)", len(claude), err)
	}
	framed := 0
	for _, f := range claude {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "❯") && strings.Contains(string(b), "────────") {
			framed++
		}
		if LooksLikeHermes(ParseScreen(string(b))) {
			t.Errorf("Claude Code's %s is recognised as Hermes: the paste guard would refuse Claude Code", filepath.Base(f))
		}
	}
	if framed < 5 {
		t.Fatalf("instrument check: only %d Claude Code screens have the ❯-between-rules shape", framed)
	}
	// The ☤ is what tells them apart: Claude Code's idle screen with its
	// footer rows cut off — the frame found, a ❯ in it — is still not Hermes.
	// (Without the cut the footer below the lower rule refuses first, and the
	// ☤ is never consulted.)
	for _, f := range claude {
		b, _ := os.ReadFile(f)
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		last := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "────────") {
				last = i
			}
		}
		if last < 2 {
			continue
		}
		cut := ParseScreen(strings.Join(lines[:last+1], "\n"))
		if _, err := frameOf(cut); err != nil || !strings.ContainsRune(InputRow(cut), '❯') {
			continue
		}
		framed++
		if LooksLikeHermes(cut) {
			t.Errorf("Claude Code's %s, footer cut, is recognised as Hermes", filepath.Base(f))
		}
	}
	if framed < 10 {
		t.Fatalf("instrument check: only %d Claude Code screens reached the ☤ check", framed)
	}
	for _, raw := range []string{"$ ls\nfile\n$ \n", "", "─────────────────────────\n$ \n"} {
		if LooksLikeHermes(ParseScreen(raw)) {
			t.Errorf("%q recognised as Hermes", raw)
		}
	}
}

// TestHasViLabelWholeWords: a label counts as a whole word only.
func TestHasViLabelWholeWords(t *testing.T) {
	for _, s := range []string{" ☤ m │ INSERT ", "x │ EINFÜGEN", "NORMAL", " │ 插入 "} {
		if !HasViLabel(s) {
			t.Errorf("%q: label not seen", s)
		}
	}
	for _, s := range []string{" ☤ m │ INSERTED", "ABNORMAL", " ☤ stub-model │ 3s"} {
		if HasViLabel(s) {
			t.Errorf("%q: a label seen", s)
		}
	}
}
