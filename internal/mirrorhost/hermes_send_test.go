package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// The gated send's order and every one of its refusals, over the screens the
// PINNED Hermes drew (internal/hermeshost/testdata/screens, recorded by `make
// test-livehermes`): a fake pane that answers captures from a script and
// records every key.

func hermesScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "hermeshost", "testdata", "screens", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const fakeSince = int64(1_700_000_000_000)

func idleHermes() AgentPane {
	return AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%7", Gen: "local-root", Status: mirror.StatusIdle, StatusSince: fakeSince,
		Hermes: &hermeshost.PaneSession{Session: "local-root"}}
}

// fakeHermesPane scripts a gated send: statuses and screens are answered in
// order (the last repeats), and every action is recorded.
type fakeHermesPane struct {
	statuses  []AgentPane
	screens   []string
	submitted bool // whether the row ever arrives
	actions   []string
	nStatus   int
	nScreen   int
	polls     int
}

func (f *fakeHermesPane) steps() hermesSendSteps {
	return hermesSendSteps{
		status: func(context.Context) (AgentPane, error) {
			i := min(f.nStatus, len(f.statuses)-1)
			f.nStatus++
			f.actions = append(f.actions, "status")
			return f.statuses[i], nil
		},
		capture: func(context.Context) (string, error) {
			i := min(f.nScreen, len(f.screens)-1)
			f.nScreen++
			f.actions = append(f.actions, "capture")
			return f.screens[i], nil
		},
		escape: func(context.Context) error { f.actions = append(f.actions, "ESCAPE"); return nil },
		paste:  func(_ context.Context, text string) error { f.actions = append(f.actions, "PASTE:"+text); return nil },
		enter:  func(context.Context) error { f.actions = append(f.actions, "ENTER"); return nil },
		cursor: func(context.Context, string) (int64, error) { return 41, nil },
		submitted: func(_ context.Context, root string, after int64, want string) (bool, error) {
			f.polls++
			if root != "local-root" || after != 41 {
				return false, fmt.Errorf("asked for %s past %d", root, after)
			}
			return f.submitted, nil
		},
		sleep: func(_ context.Context, d time.Duration) error { f.actions = append(f.actions, "sleep "+d.String()); return nil },
		logf:  func(string, ...any) {},
	}
}

// keys is the actions that reach the pane.
func (f *fakeHermesPane) keys() []string {
	var out []string
	for _, a := range f.actions {
		if a == "ESCAPE" || a == "ENTER" || strings.HasPrefix(a, "PASTE:") {
			out = append(out, a)
		}
	}
	return out
}

func kindOf(err error) string {
	var se *HermesSendError
	if !errors.As(err, &se) {
		if err == nil {
			return "sent"
		}
		return "other: " + err.Error()
	}
	return se.Refusal()
}

// TestHermesSendInOrder: on an idle pane the send is status → screen → three
// Escapes each followed by a 1.5 s wait and a re-capture → status → paste →
// enterDelay → status and the box → Enter → the database.
func TestHermesSendInOrder(t *testing.T) {
	f := &fakeHermesPane{statuses: []AgentPane{idleHermes()},
		screens:   []string{hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "pasted-short")},
		submitted: true}
	if err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster"); err != nil {
		t.Fatalf("sent: %v", err)
	}
	want := []string{"status", "capture",
		"ESCAPE", "sleep 1.5s", "capture", "ESCAPE", "sleep 1.5s", "capture", "ESCAPE", "sleep 1.5s", "capture",
		"status", "PASTE:hello from muster", "sleep 150ms", "status", "capture", "ENTER"}
	if !slices.Equal(f.actions, want) {
		t.Fatalf("actions\n  %q\nwant\n  %q", f.actions, want)
	}
}

// TestHermesSendNeverPastesIntoABlockingScreen: 🔴 on every screen the
// pinned Hermes draws for a prompt, a question, a password, a confirm, the
// consent prompt, a turn or an overlay, NOTHING reaches the pane — not even
// an Escape — and the answer is "not submitted".
func TestHermesSendNeverPastesIntoABlockingScreen(t *testing.T) {
	blocking := []string{"approval", "approval-de", "clarify-choices", "clarify-choices-de", "clarify-freetext", "sudo", "new-confirm",
		"hook-consent", "busy", "secret", "connection", "palette", "palette-filtered", "palette-over-draft", "model-picker",
		"model-picker-filtered", "model-picker-reasoning", "model-picker-during-turn", "model-picker-over-hidden-draft",
		"stash", "palette-and-stash", "vi-insert", "vi-normal", "vi-insert-de", "voice", "voice-recording", "voice-processing",
		"image-attached", "narrow", "text-in-box", "completions-menu", "vi-hidden-after-resize"}
	for _, name := range blocking {
		f := &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: []string{hermesScreen(t, name)}, submitted: true}
		err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster")
		if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted {
			t.Errorf("%s: %v (%s)", name, err, kindOf(err))
		}
		if k := f.keys(); len(k) != 0 {
			t.Errorf("🔴 %s: keys reached the pane: %q", name, k)
		}
	}
}

// TestHermesSendAbortsAtTheFirstBadRecapture: a screen that turns non-idle
// after Escape n ends the sequence there — exactly n Escapes, no paste.
func TestHermesSendAbortsAtTheFirstBadRecapture(t *testing.T) {
	for n := 1; n <= 3; n++ {
		screens := []string{hermesScreen(t, "idle")}
		for i := 1; i < n; i++ {
			screens = append(screens, hermesScreen(t, "idle"))
		}
		screens = append(screens, hermesScreen(t, "text-in-box")) // a palette's restored draft
		f := &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: screens, submitted: true}
		err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster")
		if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || !strings.Contains(err.Error(), fmt.Sprintf("after Escape %d of 3", n)) {
			t.Errorf("abort at %d: %v", n, err)
		}
		esc := 0
		for _, k := range f.keys() {
			if k != "ESCAPE" {
				t.Errorf("abort at %d: %q reached the pane", n, k)
			}
			esc++
		}
		if esc != n {
			t.Errorf("abort at %d: %d Escapes sent", n, esc)
		}
	}
}

// TestHermesSendStatusGates: not idle, idle at another time, no known
// conversation, or not Hermes — before anything, and again before the paste.
func TestHermesSendStatusGates(t *testing.T) {
	busy, other, noGen, notHermes, waiting, unknown := idleHermes(), idleHermes(), idleHermes(), idleHermes(), idleHermes(), idleHermes()
	busy.Status = mirror.StatusBusy
	waiting.Status = mirror.StatusWaiting
	unknown.Status = ""
	other.StatusSince = fakeSince + 1
	noGen.Gen = ""
	notHermes.Runtime = ""
	for _, c := range []struct {
		name string
		ap   AgentPane
		kind string
	}{
		{"busy", busy, mirror.PasteRefusalHermesSuperseded},
		{"waiting", waiting, mirror.PasteRefusalHermesSuperseded},
		{"unknown", unknown, mirror.PasteRefusalHermesSuperseded},
		{"idle since another time", other, mirror.PasteRefusalHermesSuperseded},
		{"no conversation", noGen, mirror.PasteRefusalHermesNotSubmitted},
		{"not Hermes", notHermes, mirror.PasteRefusalHermesSuperseded},
	} {
		// Before anything.
		f := &fakeHermesPane{statuses: []AgentPane{c.ap}, screens: []string{hermesScreen(t, "idle")}, submitted: true}
		err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello")
		if kindOf(err) != c.kind || len(f.keys()) != 0 {
			t.Errorf("%s first: %v %q", c.name, err, f.keys())
		}
		// Changed during the Escapes: refused before the paste.
		f = &fakeHermesPane{statuses: []AgentPane{idleHermes(), c.ap}, screens: []string{hermesScreen(t, "idle")}, submitted: true}
		err = runHermesSend(context.Background(), f.steps(), fakeSince, "hello")
		if kindOf(err) != c.kind {
			t.Errorf("%s before the paste: %v", c.name, err)
		}
		for _, k := range f.keys() {
			if k != "ESCAPE" {
				t.Errorf("%s before the paste: %q reached the pane", c.name, k)
			}
		}
		// Changed after the paste: pasted, no Enter — with the box showing
		// exactly the message, so the status is the ONLY thing that refuses.
		f = &fakeHermesPane{statuses: []AgentPane{idleHermes(), idleHermes(), c.ap},
			screens: []string{hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "pasted-short")}, submitted: true}
		err = runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster")
		if kindOf(err) != mirror.PasteRefusalHermesPasted || slices.Contains(f.keys(), "ENTER") {
			t.Errorf("%s after the paste: %v %q", c.name, err, f.keys())
		}
	}
	// A server that sent no time.
	f := &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: []string{hermesScreen(t, "idle")}}
	if err := runHermesSend(context.Background(), f.steps(), 0, "hello"); kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || len(f.actions) != 0 {
		t.Errorf("since 0: %v %q", err, f.actions)
	}
}

// TestHermesSendChecksTheBoxBeforeEnter: a box that does not show exactly the
// message after the paste — another text, a draft before it, a screen gone
// non-idle — is "pasted, not submitted", and Enter is never pressed.
func TestHermesSendChecksTheBoxBeforeEnter(t *testing.T) {
	for _, after := range []string{"pasted-after-draft", "text-in-box", "approval", "idle", "palette"} {
		f := &fakeHermesPane{statuses: []AgentPane{idleHermes()},
			screens: []string{hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, after)}, submitted: true}
		err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster")
		if kindOf(err) != mirror.PasteRefusalHermesPasted {
			t.Errorf("%s: %v", after, err)
		}
		if slices.Contains(f.keys(), "ENTER") {
			t.Errorf("🔴 %s: Enter was pressed", after)
		}
	}
	// The collapsed placeholder for an 8-line message is the message.
	text := "collapsed line 1\ncollapsed line 2\ncollapsed line 3\ncollapsed line 4\ncollapsed line 5\ncollapsed line 6\ncollapsed line 7\ncollapsed line 8"
	f := &fakeHermesPane{statuses: []AgentPane{idleHermes()},
		screens: []string{hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "pasted-collapsed")}, submitted: true}
	if err := runHermesSend(context.Background(), f.steps(), fakeSince, text); err != nil {
		t.Errorf("collapsed: %v", err)
	}
}

// TestHermesSendConfirmsByTheDatabase: Enter with no row in time is "may have
// been submitted" — never "not submitted" — after polling for the whole wait.
func TestHermesSendConfirmsByTheDatabase(t *testing.T) {
	f := &fakeHermesPane{statuses: []AgentPane{idleHermes()},
		screens: []string{hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "idle"), hermesScreen(t, "pasted-short")}}
	err := runHermesSend(context.Background(), f.steps(), fakeSince, "hello from muster")
	if kindOf(err) != mirror.PasteRefusalHermesUnconfirmed || !strings.Contains(err.Error(), "May have been submitted") {
		t.Fatalf("no row: %v", err)
	}
	if !slices.Contains(f.keys(), "ENTER") {
		t.Fatal("Enter was not pressed")
	}
	if want := int(hermesConfirmWait/hermesConfirmPoll) + 1; f.polls != want {
		t.Errorf("polled %d times, want %d", f.polls, want)
	}
}

// TestHermesTextRules: a leading / or !, a trailing \ (stripped) and an
// empty message are refused before anything — no status read, no capture.
func TestHermesTextRules(t *testing.T) {
	for _, text := range []string{"/model", "!ls", "  /tmp/notes.txt please", "\t!x", `continue \`, "end with \\  \n", "   ", ""} {
		f := &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: []string{hermesScreen(t, "idle")}, submitted: true}
		err := runHermesSend(context.Background(), f.steps(), fakeSince, text)
		if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || len(f.actions) != 0 {
			t.Errorf("%q: %v, actions %q", text, err, f.actions)
		}
	}
	for _, text := range []string{"use a/b", "path /x in the middle", `a \ b`, "wow!"} {
		if why := HermesTextRefusal(text); why != "" {
			t.Errorf("%q refused: %s", text, why)
		}
	}
}

// TestHermesSendRefusesWhatTheBoxCouldNotShowWhole: a single-line message the
// box would scroll (the 1,599-character paste that hid the prompt row) is
// refused before any key; the same text as 6+ lines (Hermes collapses it) is
// not.
func TestHermesSendRefusesWhatTheBoxCouldNotShowWhole(t *testing.T) {
	var words []string
	for i := 0; i < 200; i++ {
		words = append(words, fmt.Sprintf("word%03d", i))
	}
	long := strings.Join(words, " ")
	f := &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: []string{hermesScreen(t, "idle")}, submitted: true}
	err := runHermesSend(context.Background(), f.steps(), fakeSince, long)
	if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || !strings.Contains(err.Error(), "rows of Hermes's input box") || len(f.keys()) != 0 {
		t.Fatalf("long: %v %q", err, f.keys())
	}
	f = &fakeHermesPane{statuses: []AgentPane{idleHermes()}, screens: []string{hermesScreen(t, "idle")}, submitted: true}
	err = runHermesSend(context.Background(), f.steps(), fakeSince, strings.Join(words[:6], "\n"))
	if err != nil && strings.Contains(err.Error(), "rows of Hermes's input box") {
		t.Fatalf("six lines refused as too tall: %v", err)
	}
}
