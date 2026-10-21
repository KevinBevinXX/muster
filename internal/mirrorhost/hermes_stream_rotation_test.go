package mirrorhost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A rotation while the view is open (the session ends by compression and
// continues in a child, whose first rows re-insert the retained context)
// starts the view over on the new lineage — read as appends, the retained
// context would be drawn twice. A row written INACTIVE is never drawn.
func TestAHermesViewStartsOverOnARotationAndDrawsOnlyActiveRows(t *testing.T) {
	home := hermestest.New(t)
	root, child := hermestest.LocalID(), hermestest.ChildID()
	home.Session(root, hermestest.SessionOpts{})
	home.Message(root, hermestest.Msg{Role: "user", Content: "kept prompt", UID: "k1"})
	src := NewHermesSource(home.Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	wb := &windowsBackend{windows: []WindowInfo{hermesWindowInfo(root)}}
	d := newDrivenMuster(t)
	d.run(t, hermesBackend{wb, src}, nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 6, Session: "h", Window: "@5"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 6); return len(evs) == 1 })

	home.Message(root, hermestest.Msg{Role: "assistant", Content: "INACTIVE-ROW", Inactive: true})
	home.Message(root, hermestest.Msg{Role: "assistant", Content: "after the inactive one"})
	waitUntil(t, "the active row", func() bool {
		evs, _, _ := watchEvents(d, 6)
		return strings.Contains(evText(evs), "after the inactive one")
	})
	if evs, _, _ := watchEvents(d, 6); strings.Contains(evText(evs), "INACTIVE-ROW") {
		t.Fatalf("an inactive row was drawn: %s", evText(evs))
	}

	home.End(root, "compression")
	home.Session(child, hermestest.SessionOpts{Parent: root})
	home.Message(child, hermestest.Msg{Role: "user", Content: "kept prompt", UID: "k1"})
	home.Message(child, hermestest.Msg{Role: "assistant", Content: "in the child"})
	waitUntil(t, "the reset on the rotation", func() bool { _, resets, _ := watchEvents(d, 6); return resets == 2 })
	waitUntil(t, "the child's row", func() bool { evs, _, _ := watchEvents(d, 6); return strings.Contains(evText(evs), "in the child") })
	evs, _, last := watchEvents(d, 6)
	if n := strings.Count(evText(evs), "prompt:kept prompt"); n != 1 || last.Gen != root {
		t.Fatalf("after the rotation the retained prompt is drawn %d times (gen %q): %s", n, last.Gen, evText(evs))
	}
}
