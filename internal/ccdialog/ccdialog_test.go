package ccdialog

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../cmd/ccd/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInputBoxReadsTheRealCLIsBox: the empty box at the real CLI's idle
// prompt, and the box holding text (two captures of 2.1.296 with text in
// it); a screen whose bottom is not the box — the permission prompt, no rules
// at all — is not found, so a caller that needs it empty refuses.
func TestInputBoxReadsTheRealCLIsBox(t *testing.T) {
	for _, c := range []struct {
		name  string
		text  string
		found bool
	}{
		{"pane_idle_2.1.296.txt", "", true},
		{"pane_idle_quoted_transcript_2.1.296.txt", "", true},
		{"pane_idle_dialogwords_inputbox_2.1.296.txt", "❯ 1. Yes\nEsc to cancel", true},
		{"pane_idle_quoted_inputbox_2.1.296.txt", "[Pasted text #1 +4 lines]", true},
		{"pane_permission_2.1.296.txt", "", false},
	} {
		text, found := InputBox(fixture(t, c.name))
		if found != c.found || (c.found && text != c.text) {
			t.Errorf("%s: InputBox = %q, %t; want %q, %t", c.name, text, found, c.text, c.found)
		}
	}
	if _, found := InputBox("just some output\nand more\n"); found {
		t.Error("a screen with no rules was read as a box")
	}
}
