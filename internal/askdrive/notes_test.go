package askdrive

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// TestANoteOnAPreviewQuestionIsSomeoneAnswering: the preview picker's notes
// line (measured 2.1.289: n opens it — the footer then offers "ctrl+g to
// edit" and keys, digits too, go INTO the note — and Escape leaves it with
// the note kept) is part of the answer. On the REAL captures: a note being
// opened, typed, or saved is refused as someone answering in the terminal,
// with nothing sent — muster would otherwise submit its option with the
// person's note. The control: the untouched capture matches.
func TestANoteOnAPreviewQuestionIsSomeoneAnswering(t *testing.T) {
	two := []askq.Question{pvColor, pvSize}
	for _, f := range []string{"preview-note-edit-100", "preview-note-typing-100", "preview-note-saved-100"} {
		p := &screenPane{t: t, screen: f}
		err := Drive(context.Background(), p, two, []Choice{{Picks: []int{0}}, {Picks: []int{1}}}, fast)
		if !errors.Is(err, ErrNotUntouched) || len(p.sent) != 0 {
			t.Errorf("%s: %v, sent %q; want refused as touched, nothing sent", f, err, p.sent)
		}
	}
	// The auditor's edit of the untouched capture: only the note differs.
	s := Parse(strings.Replace(fixture(t, "preview-q1-100"), "Notes: press n to add notes", "Notes: ship it friday, NOT red", 1))
	if err := Match(s, Start(), two, nil); err == nil {
		t.Fatal("a saved note read as an untouched picker")
	}
	if err := Match(Parse(fixture(t, "preview-q1-100")), Start(), two, nil); err != nil {
		t.Fatalf("the untouched capture: %v", err)
	}
	// SYNTHETIC: the untouched capture with only the footer of an open note
	// (every real capture of an open note also shows its other text): the
	// footer alone refuses — a key would go into the note.
	editing := strings.Replace(fixture(t, "preview-q1-100"), "Tab to switch questions · Esc to cancel",
		"Tab to switch questions · ctrl+g to edit in \nNvim · Esc to cancel", 1)
	if s := Parse(editing); !s.NoteEditing || Match(s, Start(), two, nil) == nil {
		t.Fatalf("a note being typed read as an untouched picker: %+v", s)
	}
}
