package mirror_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// guardedPaneBackend is a current muster-mirror's tmux whose "shell" session
// runs what the host's paste guard sees as rule (mirrorhost.HermesGuardBackend:
// a Hermes rule, or opencode's).
type guardedPaneBackend struct {
	paneBackend
	rule string
}

func (b guardedPaneBackend) PaneHermes(_ context.Context, pane string) (mirrorhost.HermesProc, bool, error) {
	if pane == "%a-shell" {
		return mirrorhost.HermesProc{PID: 4242, Rule: b.rule}, true, nil
	}
	return mirrorhost.HermesProc{}, false, nil
}

// TestTheHostsGuardRefusalReachesMusterAsItsKind: a REAL host (muster-mirror's
// client) and a REAL hub, for each runtime the host's paste guard refuses. A
// Terminal-view paste into a session whose pane runs it is refused by the
// host; muster receives it as ErrPasteRefused whose kind is that runtime's
// (a paste-guard refusal) and whose words are the host's sentence, and
// nothing is pasted. The Claude session on the same host is pasted into (the
// control).
func TestTheHostsGuardRefusalReachesMusterAsItsKind(t *testing.T) {
	for _, c := range []struct{ rule, kind, text string }{
		{"comm=hermes", mirror.PasteRefusalHermes, mirror.PasteRefusedHermesText},
		{"opencode", mirror.PasteRefusalOpenCode, mirror.PasteRefusedOpenCodeText},
	} {
		t.Run(c.kind, func(t *testing.T) {
			r := newRig(t)
			tok, _ := r.store.mint(t, 1, "rw")
			tr := newTranscript(t, promptLine("x"))
			b := newClaudeBackend("main", "shell")
			b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
			r.connect(t, "rw", tok, guardedPaneBackend{paneBackend{b}, c.rule}, true)
			eventually(t, "rw lists its Claude pane", func() bool {
				for _, h := range r.svc.Hosts(asUser(1)) {
					if h.Host == "rw" {
						return h.Online && len(h.Claude) == 1
					}
				}
				return false
			})

			if err := r.svc.Paste(asUser(1), "rw", "main", "hi", mirror.PasteActivePane); err != nil {
				t.Fatalf("control: a paste into the Claude session: %v", err)
			}
			err := r.svc.Paste(asUser(1), "rw", "shell", "4", mirror.PasteActivePane)
			var pr *mirror.PasteRefusedError
			if !errors.Is(err, mirror.ErrPasteRefused) || !errors.As(err, &pr) || mirror.PasteGuardRefusal(err) != c.kind || pr.Why != c.text {
				t.Fatalf("a paste into the guarded pane = %v (%+v); want ErrPasteRefused of kind %q saying %q", err, pr, c.kind, c.text)
			}
			if got := b.pasted(); !slices.Equal(got, []string{"pane %7:hi"}) {
				t.Fatalf("pasted %q, want only the control", got)
			}
		})
	}
}
