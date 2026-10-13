package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// TestMirrorLabelsReachEverySurfaceAndNeverTheAddress: a host's labels (the
// plugin's HostView.Labels) title the Agents card, the terminal page and the
// rendered view, through the real mux — while every href and socket path still
// names the SESSION. Its control is the same host with no labels: raw names.
func TestMirrorLabelsReachEverySurfaceAndNeverTheAddress(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, labels: map[string]string{"main": "mango:1"}}
	s, ts := viewServer(t, fm)

	_, list := mirrorGet(t, s, ts, "/ui/agents")
	for _, want := range []string{`<span data-mirror-label="">mango:1</span>`, `title="tmux session main"`,
		`href="` + amp(ui.MirrorViewPath("ws", "main")) + `"`, `data-mirror-name="">chief</span>`} {
		if !strings.Contains(list, want) {
			t.Errorf("the Agents list lacks %q", want)
		}
	}
	if strings.Contains(list, "session=mango") {
		t.Error("a link addresses the session by its label")
	}

	_, term := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	if !strings.Contains(term, "<title>Terminal · mango:1 · muster</title>") || !strings.Contains(term, `title="tmux session main"`) {
		t.Error("the terminal page is not titled by the label with the raw name")
	}
	if !strings.Contains(term, amp(ui.MirrorTerminalWSPath("ws", "main"))) {
		t.Error("the terminal's socket does not address the session by name")
	}
	_, chiefTerm := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "chief"))
	if !strings.Contains(chiefTerm, "<title>Terminal · chief · muster</title>") || strings.Contains(chiefTerm, "data-mirror-session") {
		t.Error("an unlabelled session's terminal is not titled by its raw name alone")
	}

	_, view := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main"))
	if !strings.Contains(view, "<title>mango:1 · Mirrored · muster</title>") {
		t.Error("the rendered view is not titled by the label")
	}

	// Control: no labels — every surface is the raw name.
	s2, ts2 := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}})
	_, plain := mirrorGet(t, s2, ts2, "/ui/agents")
	if strings.Contains(plain, "data-mirror-label") || !strings.Contains(plain, `data-mirror-name="">main</span>`) {
		t.Error("an unlabelled host's card is not its raw name")
	}
}
