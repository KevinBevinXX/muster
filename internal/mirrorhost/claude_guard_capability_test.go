package mirrorhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The Claude prompt guard FAILS CLOSED on a backend that lacks either of its
// capabilities: no screen read (ClaudeScreenBackend), or no checked paste
// (ClaudeCheckedPaster). Such a backend is refused every Claude paste — never
// the status check alone, never a plain Paste whose Enter nothing checked.
// Written against what the package had before (windowsBackend, runCheckedPaste),
// so it runs, and fails, against the version that fell back.

// claudeOnly is a per-window backend with a Claude pane and a plain Paste,
// nothing else.
type claudeOnly struct{ b *windowsBackend }

func (c claudeOnly) List(ctx context.Context) ([]string, error) { return c.b.List(ctx) }
func (c claudeOnly) Attach(ctx context.Context, s string, ro bool, cols, rows uint16) (Terminal, error) {
	return c.b.Attach(ctx, s, ro, cols, rows)
}
func (c claudeOnly) Windows(ctx context.Context) ([]WindowInfo, error) { return c.b.Windows(ctx) }
func (c claudeOnly) ClaudePanes(ctx context.Context, s []string) map[string]ClaudePane {
	return c.b.ClaudePanes(ctx, s)
}
func (c claudeOnly) Paste(ctx context.Context, p ClaudePane, text string) error {
	return c.b.PastePane(ctx, p.Pane, text)
}

// withScreen adds the screen read, not the checked paste.
type withScreen struct{ claudeOnly }

func (withScreen) ClaudeScreen(context.Context, string) (string, error) { return idleClaudeScreen, nil }

// withChecked adds the checked paste, not the screen read.
type withChecked struct{ claudeOnly }

func (w withChecked) PasteChecked(ctx context.Context, p ClaudePane, text string, before, beforeEnter func(context.Context) error) error {
	return runCheckedPaste(ctx, checkedPasteSteps{
		paste: func(ctx context.Context) error { return w.b.PastePane(ctx, p.Pane, text) },
		enter: func(context.Context) error { return nil },
		sleep: func(context.Context, time.Duration) error { return nil },
	}, before, beforeEnter)
}

// both has both: the control.
type both struct{ withChecked }

func (both) ClaudeScreen(context.Context, string) (string, error) { return idleClaudeScreen, nil }

func TestAClaudePasteIsRefusedOnABackendThatCannotRunTheWholeGuard(t *testing.T) {
	for _, c := range []struct {
		name   string
		mk     func(*windowsBackend) Backend
		refuse bool
	}{
		{"no screen read, no checked paste", func(b *windowsBackend) Backend { return claudeOnly{b} }, true},
		{"screen read, no checked paste", func(b *windowsBackend) Backend { return withScreen{claudeOnly{b}} }, true},
		{"checked paste, no screen read", func(b *windowsBackend) Backend { return withChecked{claudeOnly{b}} }, true},
		{"both (control)", func(b *windowsBackend) Backend { return both{withChecked{claudeOnly{b}}} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDrivenMuster(t)
			b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
			d.run(t, c.mk(b), nil, true)
			a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "hello"})
			_, _, _, typed, _ := b.snapshot()
			if !c.refuse {
				if a.Error != "" || len(typed) != 1 {
					t.Fatalf("answered %+v, typed %q; want the paste typed", a, typed)
				}
				return
			}
			if !strings.Contains(a.Error, "cannot check Claude Code's screen") || len(typed) != 0 {
				t.Fatalf("answered %+v, typed %q; want refused as uncheckable, nothing typed", a, typed)
			}
		})
	}
}
