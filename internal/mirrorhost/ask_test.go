package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST'S HALF OF ANSWERING A PICKER (ask.go), over a real tunnel client
// against a hand-driven muster: what it advertises, which pane it types into,
// and how each kind of failure is named back.

// askingBackend is windowsBackend that can also type into a picker: it records
// each AnswerAsk and answers with err.
type askingBackend struct {
	*windowsBackend
	mu    sync.Mutex
	calls []string
	err   error
}

func (b *askingBackend) AnswerAsk(_ context.Context, pane string, qs []askq.Question, cs []askq.Choice) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf("%s %s %v", pane, qs[0].Question, cs))
	return b.err
}

func (b *askingBackend) snapshotCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// rawlessBackend is askingBackend whose tmux can (raw) or cannot paste raw
// keys (Tmux.PastesRawKeys).
type rawlessBackend struct {
	*askingBackend
	raw bool
}

func (b *rawlessBackend) PastesRawKeys(context.Context) bool { return b.raw }

var hostAskQs = []askq.Question{{Question: "Which color?", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}}}}

func TestTheHostAdvertisesAnsweringOnlyWhenItCan(t *testing.T) {
	for _, c := range []struct {
		name        string
		b           Backend
		interactive bool
		want        bool
	}{
		{"interactive, can type", wrapped{&askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}, true, true},
		{"read-only", wrapped{&askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}, false, false},
		{"a backend that cannot type", wrapped{&windowsBackend{windows: threeWindows}}, true, false},
		{"a tmux without paste-buffer -S", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}}, true, false},
		{"a tmux with paste-buffer -S", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}, raw: true}}, true, true},
	} {
		d := newDrivenMuster(t)
		d.run(t, c.b, nil, c.interactive)
		waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
		if got := d.of(mirror.TypeHello)[0].AskAnswer; got != c.want {
			t.Errorf("%s: AskAnswer %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAnAnswerIsTypedIntoTheClaudePaneOnly: an answer for the Claude window is
// typed into ITS pane; one for a window whose pane is not Claude Code is
// refused as "no picker" without typing; the driver's own refusals come back
// named; a read-only host types nothing.
func TestAnAnswerIsTypedIntoTheClaudePaneOnly(t *testing.T) {
	b := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{b}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	answer := func(id uint32, window string) mirror.Msg {
		t.Helper()
		d.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "scratch2", Window: window,
			Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}})
		var out mirror.Msg
		waitUntil(t, "the answered", func() bool {
			for _, m := range d.of(mirror.TypeAnswered) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	if a := answer(1, "@7"); a.Error != "" {
		t.Fatalf("the Claude window's answer was refused: %s", a.Error)
	}
	if got := b.snapshotCalls(); !reflect.DeepEqual(got, []string{"%21 Which color? [{[1] }]"}) {
		t.Fatalf("typed %q, want the Claude pane %%21", got)
	}
	if a := answer(2, "@4"); a.Refusal != mirror.AskRefusalNoPicker {
		t.Fatalf("a shell window: %+v, want refused no-picker", a)
	}
	if got := b.snapshotCalls(); len(got) != 1 {
		t.Fatalf("a refused answer was typed: %q", got)
	}
	for i, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", askdrive.ErrNoPicker), mirror.AskRefusalNoPicker},
		{fmt.Errorf("x: %w", askdrive.ErrNotUntouched), mirror.AskRefusalTouched},
		{fmt.Errorf("x: %w", askdrive.ErrLost), mirror.AskRefusalLost},
		{fmt.Errorf("x: %w", askdrive.ErrUnrecognised), mirror.AskRefusalUnrecognised},
		{ErrPaneBusy, mirror.AskRefusalTouched},
		{errors.New("tmux broke"), ""},
	} {
		b.mu.Lock()
		b.err = c.err
		b.mu.Unlock()
		a := answer(uint32(10+i), "@7")
		if a.Error == "" || a.Refusal != c.want {
			t.Errorf("driver error %v: answered %+v, want refusal %q", c.err, a, c.want)
		}
	}
	// A read-only host refuses whatever muster sends.
	rb := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}
	ro := newDrivenMuster(t)
	ro.run(t, wrapped{rb}, nil, false)
	waitUntil(t, "the hello", func() bool { return len(ro.of(mirror.TypeHello)) == 1 })
	ro.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: 1, Session: "scratch2", Window: "@7",
		Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}})
	waitUntil(t, "the refusal", func() bool { return len(ro.of(mirror.TypeAnswered)) == 1 })
	if a := ro.of(mirror.TypeAnswered)[0]; a.Refusal != mirror.AskRefusalUnsupported || len(rb.snapshotCalls()) != 0 {
		t.Fatalf("a read-only host: %+v, typed %q", a, rb.snapshotCalls())
	}
}

// TestOneDrivePerPane: while a drive has a pane, another answer for it is
// refused before tmux is touched (the Tmux here has no server at all); a
// drive of another pane, or of the same pane once released, is not.
func TestOneDrivePerPane(t *testing.T) {
	tm := Tmux{Bin: "/nonexistent/tmux", Socket: "one-drive"}
	release, ok := lockPane(strings.Join(append(tm.server(), "%5"), " "))
	if !ok {
		t.Fatal("the first drive could not take the pane")
	}
	err := tm.AnswerAsk(context.Background(), "%5", hostAskQs, []askq.Choice{{Picks: []int{0}}})
	if !errors.Is(err, ErrPaneBusy) || !errors.Is(err, askdrive.ErrNotUntouched) {
		t.Fatalf("a second drive of a busy pane: %v", err)
	}
	if err := tm.AnswerAsk(context.Background(), "%6", hostAskQs, []askq.Choice{{Picks: []int{0}}}); errors.Is(err, ErrPaneBusy) {
		t.Fatalf("another pane: %v", err)
	}
	release()
	if err := tm.AnswerAsk(context.Background(), "%5", hostAskQs, []askq.Choice{{Picks: []int{0}}}); errors.Is(err, ErrPaneBusy) {
		t.Fatalf("the pane once released: %v", err)
	}
}

// TestTheRawPasteProbeReadsTmuxsAnswer: PastesRawKeys is true only for a tmux
// that refuses the probe as "no buffer" (it knows -S), false for one that
// refuses the flag. A fake tmux plays each.
func TestTheRawPasteProbeReadsTmuxsAnswer(t *testing.T) {
	dir := t.TempDir()
	fake := func(name, says string) Tmux {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+says+"' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return Tmux{Bin: p}
	}
	if !fake("new", "no buffer muster-mirror-ask-probe-x").PastesRawKeys(context.Background()) {
		t.Error("a tmux with -S reads as without")
	}
	if fake("old", "command paste-buffer: unknown flag -S").PastesRawKeys(context.Background()) {
		t.Error("a tmux without -S reads as with")
	}
}
