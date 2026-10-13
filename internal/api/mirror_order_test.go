package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheAgentsListShowsMirroredSessionsMostRecentlyActiveFirst: through the
// real mux, the mirrored cards come out in mirror.Ordered's order — the
// session active last on top — and a host that reports no activity keeps the
// name order it always had (the control, and the reason the first half is not
// vacuous: "main" before "chief" is the REVERSE of the name order).
func TestTheAgentsListShowsMirroredSessionsMostRecentlyActiveFirst(t *testing.T) {
	chief, main := `href="`+amp(ui.MirrorTerminalPath("ws", "chief"))+`"`, `href="`+amp(ui.MirrorTerminalPath("ws", "main"))+`"`
	positions := func(fm *fakeMirror) (int, int) {
		t.Helper()
		s, ts := viewServer(t, fm)
		_, list := mirrorGet(t, s, ts, "/ui/agents")
		c, m := strings.Index(list, chief), strings.Index(list, main)
		if c < 0 || m < 0 {
			t.Fatalf("instrument check FAILED: the list lacks a card (chief at %d, main at %d)", c, m)
		}
		return c, m
	}

	if c, m := positions(&fakeMirror{online: true, activity: map[string]int64{"chief": 1_800_000_000, "main": 1_800_000_660}}); m > c {
		t.Errorf("main was active last and is listed AFTER chief (main at %d, chief at %d)", m, c)
	}
	// The other way round, so the order follows the times and not the fixture.
	if c, m := positions(&fakeMirror{online: true, activity: map[string]int64{"chief": 1_800_000_660, "main": 1_800_000_000}}); c > m {
		t.Errorf("chief was active last and is listed AFTER main (chief at %d, main at %d)", c, m)
	}
	// Control: no activity — the name order, as before.
	if c, m := positions(&fakeMirror{online: true}); c > m {
		t.Errorf("with no activity the cards are not in name order (chief at %d, main at %d)", c, m)
	}
}
