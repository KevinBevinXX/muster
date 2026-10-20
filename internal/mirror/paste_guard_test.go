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
// runs Hermes, as the host's paste guard sees it (mirrorhost.HermesGuardBackend).
type guardedPaneBackend struct{ paneBackend }

func (b guardedPaneBackend) PaneHermes(_ context.Context, pane string) (mirrorhost.HermesProc, bool, error) {
	if pane == "%a-shell" {
		return mirrorhost.HermesProc{PID: 4242, Rule: "comm=hermes"}, true, nil
	}
	return mirrorhost.HermesProc{}, false, nil
}

// TestTheHostsHermesRefusalReachesMusterAsItsKind: a REAL host (muster-mirror's
// client) and a REAL hub. A Terminal-view paste into a session whose pane runs
// Hermes is refused by the host; muster receives it as ErrPasteRefused whose
// kind is PasteRefusalHermes and whose words are the host's sentence, and
// nothing is pasted. The Claude session on the same host is pasted into (the
// control).
func TestTheHostsHermesRefusalReachesMusterAsItsKind(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "rw")
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
	r.connect(t, "rw", tok, guardedPaneBackend{paneBackend{b}}, true)
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
	if !errors.Is(err, mirror.ErrPasteRefused) || !errors.As(err, &pr) || mirror.PasteGuardRefusal(err) != mirror.PasteRefusalHermes ||
		pr.Why != mirror.PasteRefusedHermesText {
		t.Fatalf("a paste into the Hermes pane = %v (%+v); want ErrPasteRefused of kind %q saying %q", err, pr, mirror.PasteRefusalHermes, mirror.PasteRefusedHermesText)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"pane %7:hi"}) {
		t.Fatalf("pasted %q, want only the control", got)
	}
}
