package hermeshost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRowAfterPrompt is fixture name's raw capture with row inserted after
// its input area's first row.
func withRowAfterPrompt(t *testing.T, name, row string) Screen {
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
	at := f.top + 2
	lines = append(lines[:at], append([]string{row}, lines[at:]...)...)
	return ParseScreen(strings.Join(lines, "\n"))
}

// TestEveryInputRowIsChecked: an idle prompt row with text on a LATER input
// row (a multi-line buffer) is refused; the same later row holding only the
// italic placeholder's style, or spaces, is not.
func TestEveryInputRowIsChecked(t *testing.T) {
	if err := CheckIdle(withRowAfterPrompt(t, "idle", "  second line of a draft")); answer(err) != "refuse: Text" {
		t.Fatalf("text on a later input row: %v", err)
	}
	if err := CheckIdle(withRowAfterPrompt(t, "idle", "\x1b[3mitalic placeholder wrap\x1b[0m")); err != nil {
		t.Fatalf("an italic later row: %v", err)
	}
	if err := CheckIdle(withRowAfterPrompt(t, "idle", "      ")); err != nil {
		t.Fatalf("a blank later row: %v", err)
	}
}
