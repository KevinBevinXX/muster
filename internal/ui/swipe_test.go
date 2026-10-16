package ui

import (
	"strings"
	"testing"
)

// touchListenerBodies returns the source of every document-level touch
// listener appScript registers, keyed by the event name.
//
// ⚠ A TEXT EXTRACT: from the `addEventListener('touch…'` to the `{ passive:
// true });` that closes it. Every touch listener in appScript is registered
// that way; one that is not is MISSED here, which the positive controls below
// turn into a failure rather than a silent pass.
func touchListenerBodies(t *testing.T, js string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rest := js
	for {
		i := strings.Index(rest, "document.addEventListener('touch")
		if i < 0 {
			return out
		}
		rest = rest[i:]
		nameEnd := strings.Index(rest[len("document.addEventListener('"):], "'")
		name := rest[len("document.addEventListener('") : len("document.addEventListener('")+nameEnd]
		end := strings.Index(rest, "{ passive: true });")
		if end < 0 {
			t.Fatalf("the %s listener has no `{ passive: true });` terminator — the extract cannot find its end", name)
		}
		out[name] += rest[:end]
		rest = rest[end:]
	}
}

// TestNoTabSwitchSwipe pins the removal of the tab-switch swipe AND the survival
// of the sidebar's edge-swipe, in the one script both lived in.
//
// 🔴 THE TWO HALVES ARE ONE TEST ON PURPOSE. Deleting the touch block wholesale
// would satisfy "no tab switch on swipe" while taking the sidebar gesture with
// it; keeping the block untouched would keep the sidebar and the bug. Each half
// alone is walkable by the other mistake.
//
// The behaviour is pinned in the browser too (recent-agents.spec.ts): a sideways
// swipe on the recents strip leaves the tab alone, and an edge-swipe still opens
// the sidebar.
func TestNoTabSwitchSwipe(t *testing.T) {
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		js := mustStripJSComments(t, jsSource(t, appScript(feat)))
		bodies := touchListenerBodies(t, js)

		// POSITIVE CONTROLS: both listeners were found and carry real code.
		for _, ev := range []string{"touchstart", "touchend"} {
			if len(bodies[ev]) < 40 {
				t.Fatalf("%s: no %s listener extracted (%q) — nothing below is a claim about the gesture code",
					shape, ev, bodies[ev])
			}
		}

		// THE REMOVAL. No touch listener reaches the tab router, and nothing arms on
		// a tabpanel.
		for ev, body := range bodies {
			for _, gone := range []string{"switchTab", "TABS", "tabFromPath", "PANEL_SELECTOR", "tabpanel", "#panel-"} {
				if strings.Contains(body, gone) {
					t.Errorf("%s: the %s listener still names %q — a horizontal swipe can switch the tab "+
						"again (swiping the recent-agents strip switched the parent tab)", shape, ev, gone)
				}
			}
		}
		if strings.Contains(js, "PANEL_SELECTOR") {
			t.Errorf("%s: appScript still emits PANEL_SELECTOR, whose only consumer was the tab-switch swipe", shape)
		}

		// THE SURVIVOR. The sidebar's edge-swipe arms at the edge and opens/closes.
		if !strings.Contains(bodies["touchstart"], "ownsSideways(t)") || !strings.Contains(js, "data-own-swipe") {
			t.Errorf("%s: the edge-swipe no longer stands down for a touch that began on something that "+
				"swipes sideways itself (the recent-agents strip starts inside the edge zone)", shape)
		}
		if !strings.Contains(bodies["touchstart"], "SB_EDGE") {
			t.Errorf("%s: touchstart no longer arms the sidebar's open gesture at the left edge (SB_EDGE)", shape)
		}
		for _, want := range []string{"setOpen(true)", "setOpen(false)"} {
			if !strings.Contains(bodies["touchend"], want) {
				t.Errorf("%s: touchend no longer calls %s — the mobile sidebar's swipe is gone", shape, want)
			}
		}
	})
}
