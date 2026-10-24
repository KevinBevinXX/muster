package mirror_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// openCodePaneBackend is a current muster-mirror's tmux whose "shell" session
// runs opencode, as the host's paste guard sees it (mirrorhost.HermesGuardBackend;
// the rule string is the guard's, mirrorhost's hermesRuleOpenCode).
type openCodePaneBackend struct{ paneBackend }

func (b openCodePaneBackend) PaneHermes(_ context.Context, pane string) (mirrorhost.HermesProc, bool, error) {
	if pane == "%a-shell" {
		return mirrorhost.HermesProc{PID: 4343, Rule: "opencode"}, true, nil
	}
	return mirrorhost.HermesProc{}, false, nil
}

// TestTheHostsOpenCodeRefusalReachesMusterAsItsKind: a REAL host and a REAL
// hub. A Terminal-view paste into a session whose pane runs opencode is
// refused by the host; muster receives it as ErrPasteRefused whose kind is
// PasteRefusalOpenCode (a paste-guard refusal) and whose words are the host's
// sentence, and nothing is pasted. The Claude session on the same host is
// pasted into (the control).
func TestTheHostsOpenCodeRefusalReachesMusterAsItsKind(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "rw")
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
	r.connect(t, "rw", tok, openCodePaneBackend{paneBackend{b}}, true)
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
	err := r.svc.Paste(asUser(1), "rw", "shell", "y", mirror.PasteActivePane)
	var pr *mirror.PasteRefusedError
	if !errors.Is(err, mirror.ErrPasteRefused) || !errors.As(err, &pr) || mirror.PasteGuardRefusal(err) != mirror.PasteRefusalOpenCode ||
		pr.Why != mirror.PasteRefusedOpenCodeText {
		t.Fatalf("a paste into the opencode pane = %v (%+v); want ErrPasteRefused of kind %q saying %q", err, pr, mirror.PasteRefusalOpenCode, mirror.PasteRefusedOpenCodeText)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"pane %7:hi"}) {
		t.Fatalf("pasted %q, want only the control", got)
	}
}
