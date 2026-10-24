package mirrorhost

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAPasteIntoAClaudePaneWhoseInputBoxHoldsTextTypesNothing: text left in
// Claude Code's input box (a message pasted and not submitted, something the
// person typed) would be submitted WITH a new paste as one turn. The host
// reads the box at the moment of typing and refuses (claude-box), as Hermes's
// send does; a screen with no box it can read is refused too. The real CLI's
// captures: its box holding text, its idle prompt (the control: typed).
// Written against what the host had before the box check (screenBackend), so
// it runs, and fails, there.
func TestAPasteIntoAClaudePaneWhoseInputBoxHoldsTextTypesNothing(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../cmd/ccd/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	screen := read("pane_idle_quoted_inputbox_2.1.296.txt")
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
	d.run(t, wrapped{screenBackend{windowsBackend: b, screen: func(string) (string, error) { return screen, nil }}}, nil, true)
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }

	a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "next"})
	if a.Refusal != "claude-box" || !strings.Contains(a.Error, "input box already has text") {
		t.Fatalf("a paste into a pane whose box holds text was answered %+v; want refused as claude-box", a)
	}
	screen = "some output with no box at all\n"
	if a := pasteAnswer(t, d, mirror.Msg{ID: 2, Session: "scratch2", Window: "@7", Text: "next"}); a.Error == "" {
		t.Fatalf("a paste into a pane whose box could not be read was answered %+v; want refused", a)
	}
	if got := pasted(); len(got) != 0 {
		t.Fatalf("typed %q into a pane the guard refused", got)
	}
	screen = read("pane_idle_2.1.296.txt")
	if a := pasteAnswer(t, d, mirror.Msg{ID: 3, Session: "scratch2", Window: "@7", Text: "hello"}); a.Error != "" {
		t.Fatalf("a paste into an empty box was refused: %s", a.Error)
	}
	if got := pasted(); !slices.Equal(got, []string{"%21 hello"}) {
		t.Fatalf("typed %q; want the one paste into the empty box", got)
	}
}
