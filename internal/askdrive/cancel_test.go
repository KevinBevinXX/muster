package askdrive

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestCancelSendsOneEscapeIntoAPicker: on a picker — untouched, part
// answered, on its review screen — Cancel sends exactly ONE Escape, and the
// picker goes (the FakePicker, as the CLI measured: one Escape cancels the
// ask).
func TestCancelSendsOneEscapeIntoAPicker(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup string
	}{
		{"untouched", ""},
		{"part answered (question 2, a box ticked)", "4teal, mostly" + KeyEnter + "2"},
		{"the review screen", "4teal, mostly" + KeyEnter + "2" + KeyDown + KeyDown + KeyDown + "a hamster" + KeyDown + KeyEnter + "2"},
	} {
		p := NewFakePicker(three)
		p.Type(c.setup)
		if s := Parse(p.Screen()); s.Kind == None {
			t.Fatalf("%s: setup closed the picker", c.name)
		}
		fp := &fakePane{p: p}
		gone, err := Cancel(context.Background(), fp, fast)
		if err != nil || !gone {
			t.Fatalf("%s: gone %v, %v\n%s", c.name, gone, err, p.Screen())
		}
		if !reflect.DeepEqual(fp.sent, []string{KeyEscape}) {
			t.Fatalf("%s: sent %q, want one Escape", c.name, fp.sent)
		}
		if _, cancelled, done := p.Result(); !done || !cancelled {
			t.Fatalf("%s: the picker was not dismissed", c.name)
		}
	}
}

// TestCancelSendsNothingWithoutAPicker: no picker on screen — an Escape would
// reach Claude Code itself and interrupt it — or a screen like a picker that
// does not parse: each refused with its own error, and NOTHING sent.
func TestCancelSendsNothingWithoutAPicker(t *testing.T) {
	gone := NewFakePicker(three)
	gone.Type(KeyEscape) // dismissed in the terminal already
	for _, c := range []struct {
		name string
		pane interface {
			Pane
			count() int
		}
		want error
	}{
		{"no picker (the session's prompt)", &countPane{Pane: &fakePane{p: gone}}, ErrNoPicker},
		{"a permission prompt", &countPane{Pane: &screenPane{t: t, screen: "not-picker-permission"}}, ErrNoPicker},
		{"prose", &countPane{Pane: &screenPane{t: t, screen: "not-picker-prose"}}, ErrNoPicker},
		{"a picker muster cannot read", &countPane{Pane: &rawPane{s: "←  ☐ Color  ☐ Pets  ☐ Name  ✔ Submit  →\n\nsomething else\n\nEnter to select · Esc to cancel\n"}}, ErrUnrecognised},
	} {
		if _, err := Cancel(context.Background(), c.pane, fast); !errors.Is(err, c.want) || c.pane.count() != 0 {
			t.Errorf("%s: %v, sent %d key(s); want %v and none", c.name, err, c.pane.count(), c.want)
		}
	}
}

// TestCancelAcceptsAnyPicker: a picker taller than the pane (Clipped, its
// question off the top) and another ask's picker are both pickers: Cancel
// cannot read which ask a clipped one is, so it does not check, and sends its
// one Escape.
func TestCancelAcceptsAnyPicker(t *testing.T) {
	for _, f := range []string{"long-q2-clipped-80x20", "preview-q1-100", "three-review"} {
		p := &screenPane{t: t, screen: f}
		if _, err := Cancel(context.Background(), p, fast); err != nil || !reflect.DeepEqual(p.sent, []string{KeyEscape}) {
			t.Errorf("%s: %v, sent %q", f, err, p.sent)
		}
	}
}

// TestCancelSendsNoSecondEscape: a picker still showing after the Escape is
// not gone — and the one Escape is all that was sent.
func TestCancelSendsNoSecondEscape(t *testing.T) {
	p := NewFakePicker(three)
	lp := &lookPane{fakePane: &fakePane{p: p}, drop: func(int) bool { return true }}
	gone, err := Cancel(context.Background(), lp, fast)
	if err != nil || gone {
		t.Fatalf("a picker that stayed: gone %v, %v", gone, err)
	}
	if !reflect.DeepEqual(lp.sent, []string{KeyEscape}) {
		t.Fatalf("sent %q, want exactly one Escape", lp.sent)
	}
}

// countPane counts the keys sent through it.
type countPane struct {
	Pane
	n int
}

func (c *countPane) Send(ctx context.Context, k string) error { c.n++; return c.Pane.Send(ctx, k) }
func (c *countPane) count() int                               { return c.n }

// rawPane shows s, always.
type rawPane struct{ s string }

func (r *rawPane) Capture(context.Context) (string, error) { return r.s, nil }
func (r *rawPane) Send(context.Context, string) error      { return nil }
