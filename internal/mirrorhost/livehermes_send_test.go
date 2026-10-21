//go:build livehermes

package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestLiveHermesSend runs the REAL gated send (Tmux.SendHermes: the real
// locator, the hook's real status files, the real screen, real Escapes, the
// paste, Enter and the state.db confirm) against the pinned Hermes:
//
//   - an idle CLI: "sent", confirmed — the message is in state.db once;
//   - a collapsed paste (8 lines): "sent", confirmed whole;
//   - 🔴 every blocking state — approval, clarify (choices and free text),
//     sudo, /new's confirm, the command palette, the model picker over a
//     hidden draft, the stash, a running turn — is refused, and NOTHING is
//     pasted: no user row with the text, and the state is still on screen;
//   - the text rules: a leading `/` is refused before anything.
func TestLiveHermesSend(t *testing.T) {
	r := newLiveRun(t)
	home := r.makeHome(homeOpts{name: "send", hook: true})
	tm := Tmux{SocketPath: r.tmux.SocketPath, Hermes: NewHermesSource(home, r.statusDir())}

	statusOf := func(pane string) func(ctx context.Context) (AgentPane, error) {
		return func(ctx context.Context) (AgentPane, error) {
			ws, err := tm.Windows(ctx)
			if err != nil {
				return AgentPane{}, err
			}
			for _, w := range ws {
				if w.Pane == pane && w.Agent != nil {
					return *w.Agent, nil
				}
			}
			return AgentPane{}, errors.New("no Hermes in the pane")
		}
	}
	send := func(pane, text string) (error, AgentPane) {
		ctx, cancel := context.WithTimeout(context.Background(), hermesSendTimeout)
		defer cancel()
		ap, err := statusOf(pane)(ctx)
		if err != nil {
			t.Fatalf("the pane is not a Hermes window: %v", err)
		}
		return tm.SendHermes(ctx, HermesSendRequest{Pane: pane, Since: ap.StatusSince, Text: text, Status: statusOf(pane),
			Logf: func(f string, a ...any) { t.Logf(f, a...) }}), ap
	}
	fresh := func() string {
		p := r.cli(home, 160, 45, []string{"chat"})
		if !r.waitIdle(p, 60*time.Second) {
			t.Fatal("never idle:\n" + showScreen(r.capture(p), 10))
		}
		// The status must be believed idle: the breadcrumb is written at the
		// first gateway attach, the first idle at the first turn's end.
		typed(r, p, "warm up")
		if !waitFor(40*time.Second, func() bool {
			ap, err := statusOf(p)(context.Background())
			return err == nil && ap.Status == mirror.StatusIdle && ap.Gen != ""
		}) {
			ap, _ := statusOf(p)(context.Background())
			t.Fatalf("the window never read idle with a session: %+v %+v", ap, ap.Hermes)
		}
		// The hook says idle before the CLI has finished drawing the turn's
		// end: the screen check refuses until it has (measured), so wait for
		// the screen too.
		if !r.waitIdle(p, 20*time.Second) {
			t.Fatal("the screen never settled idle:\n" + showScreen(r.capture(p), 10))
		}
		return p
	}
	var lines []string
	report := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	defer func() { fmt.Printf("\nLIVEHERMES SEND (pin %s)\n%s\n", hermesPin[:7], strings.Join(lines, "\n")) }()

	t.Run("idle: sent and confirmed", func(t *testing.T) {
		p := fresh()
		defer r.killWindow(p)
		start := time.Now()
		err, _ := send(p, "hello from the gated send")
		report("  idle, one line:              %v (%.1f s)", errText(err), time.Since(start).Seconds())
		if err != nil {
			t.Fatal(err)
		}
		if n := countUserRows(t, home, "hello from the gated send"); n != 1 {
			t.Fatalf("the message is in state.db %d times", n)
		}
	})

	t.Run("collapsed: sent and confirmed", func(t *testing.T) {
		p := fresh()
		defer r.killWindow(p)
		text := "eight line message 1\neight line message 2\neight line message 3\neight line message 4\neight line message 5\neight line message 6\neight line message 7\neight line message 8"
		err, _ := send(p, text)
		report("  idle, 8 lines (collapsed):   %v", errText(err))
		if err != nil {
			t.Fatal(err)
		}
		if n := countUserRows(t, home, text); n != 1 {
			t.Fatalf("the collapsed message is in state.db %d times", n)
		}
	})

	t.Run("a tab: sent and confirmed", func(t *testing.T) {
		p := fresh()
		defer r.killWindow(p)
		text := "a tab\there, and another\tthere"
		err, _ := send(p, text)
		report("  a message with tabs:         %v", errText(err))
		if err != nil {
			t.Fatal(err)
		}
		if n := countUserRows(t, home, text); n != 1 {
			t.Fatalf("the message with tabs is in state.db %d times", n)
		}
	})

	t.Run("text rules", func(t *testing.T) {
		p := fresh()
		defer r.killWindow(p)
		for _, text := range []string{"/model", "!ls", `trailing \`, "42"} {
			err, _ := send(p, text)
			report("  %-28q %s", text, errText(err))
			if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted {
				t.Errorf("%q: %v", text, err)
			}
		}
		if hermeshost.CheckIdle(hermeshost.ParseScreen(r.capture(p))) != nil {
			t.Error("the box is not empty after the refused texts")
		}
	})

	// 🔴 The blocking states. reach drives a fresh CLI there; marker is on
	// screen while it is up.
	for _, c := range []struct {
		name, marker string
		reach        func(p string) bool
	}{
		{"approval", "Dangerous Command", func(p string) bool { typed(r, p, "APPROVE"); return inputStarts(r, p, "⚠", 30*time.Second) }},
		{"clarify with choices", "Which colour", func(p string) bool { typed(r, p, "CLARIFY"); return inputStarts(r, p, "?", 30*time.Second) }},
		{"free-text clarify", "What name", func(p string) bool { typed(r, p, "CLARIFYFREE"); return inputStarts(r, p, "✎", 30*time.Second) }},
		{"sudo", "Sudo Password Required", func(p string) bool { typed(r, p, "SUDO"); return shows(r, p, "Sudo Password Required", 30*time.Second) }},
		{"/new confirm", "destroys", func(p string) bool { typed(r, p, "/new"); return inputStarts(r, p, "⚠", 30*time.Second) }},
		{"command palette", "Command Palette", func(p string) bool { r.keys(p, "\x10"); time.Sleep(time.Second); return true }},
		{"model picker over a hidden draft", "Model Picker", func(p string) bool {
			r.keys(p, "my hidden draft")
			time.Sleep(500 * time.Millisecond)
			r.injectState(home, "model-picker")
			return shows(r, p, "Model Picker", 20*time.Second)
		}},
		{"stash panel", "Stash (2 items)", func(p string) bool {
			for _, k := range []string{"draft one", "\x13", "draft two", "\x13", "\x13"} {
				r.keys(p, k)
				time.Sleep(400 * time.Millisecond)
			}
			return shows(r, p, "Stash (2 items)", 10*time.Second)
		}},
		{"a running turn", "", func(p string) bool { typed(r, p, "SLOW12"); return inputStarts(r, p, "☤", 30*time.Second) }},
	} {
		t.Run("refused: "+c.name, func(t *testing.T) {
			p := fresh()
			defer r.killWindow(p)
			if !c.reach(p) {
				t.Fatal("never reached:\n" + showScreen(r.capture(p), 12))
			}
			time.Sleep(1500 * time.Millisecond)
			text := "MUST-NOT-ARRIVE " + c.name
			err, ap := send(p, text)
			report("  %-34s status %-8s -> %s", c.name+":", orUnknown(ap.Status), errText(err))
			if k := kindOf(err); k != mirror.PasteRefusalHermesNotSubmitted && k != mirror.PasteRefusalHermesSuperseded {
				t.Fatalf("🔴 not refused before the paste: %v", err)
			}
			time.Sleep(time.Second)
			raw := r.capture(p)
			if strings.Contains(stripSGR(raw), "MUST-NOT-ARRIVE") {
				t.Fatalf("🔴 the text reached the pane:\n%s", showScreen(raw, 12))
			}
			if n := countUserRows(t, home, text); n != 0 {
				t.Fatalf("🔴 the text is in state.db %d times", n)
			}
			if c.marker != "" && !strings.Contains(stripSGR(raw), c.marker) {
				t.Errorf("the state is no longer on screen (an Escape reached it?):\n%s", showScreen(raw, 12))
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return "sent (confirmed in state.db)"
	}
	return fmt.Sprintf("[%s] %s", kindOf(err), err.Error())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
