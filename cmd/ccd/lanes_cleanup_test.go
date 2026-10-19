package main

import (
	"context"
	"testing"
	"time"
)

// losablePane is a stillPane whose CLI the supervisor stops seeing (terminal
// lost) once lost is closed — the lane's run then ends on its own.
type losablePane struct {
	stillPane
	lost chan struct{}
}

func (p *losablePane) Exited(context.Context, paneRef) (paneExit, error) {
	select {
	case <-p.lost:
		return paneExit{}, errTerminalLost
	default:
		return paneExit{}, nil
	}
}

func waitDone(t *testing.T, ln *lane) {
	t.Helper()
	select {
	case <-ln.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lane's run never ended")
	}
}

// 🔴 A LANE'S CLEANUP KILLS ITS tmux SESSION ONLY WHILE THE LANE IS STILL THE
// SET'S. The session is killed BY NAME (cc-<laneID(key)>), and a lane started
// again for the same key has the same name: an old lane whose run ends after it
// was replaced must not kill the new one's CLI. The positive control: a lane
// that is still the set's is removed and its session killed.
func TestALaneCleanupNeverKillsItsReplacementsSession(t *testing.T) {
	l := testLanes(t, 2)
	var panes []*losablePane
	l.newTerm = func(string) laneTerminal {
		p := &losablePane{lost: make(chan struct{})}
		panes = append(panes, p)
		return p
	}
	ctx := context.Background()

	// Positive control: the set's own lane loses its CLI.
	c, f := l.acquire(ctx, "sess-c", "")
	if f != nil {
		t.Fatal(f)
	}
	l.release(c)
	close(panes[0].lost)
	waitDone(t, c)
	if l.get(c.id) != nil || panes[0].kills() != 1 {
		t.Fatalf("the set's own lane ended: still in set=%v, session kills=%d; want removed and killed once",
			l.get(c.id) != nil, panes[0].kills())
	}

	// The old lane for sess-a is replaced (as an eviction's delete and a new
	// turn's start do) before its own run ends.
	a, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	l.release(a)
	l.mu.Lock()
	delete(l.set, a.id)
	l.mu.Unlock()
	a2, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(a2.stop)
	close(panes[1].lost)
	waitDone(t, a)
	if n := panes[1].kills(); n != 0 {
		t.Fatalf("the replaced lane's cleanup killed session cc-%s %d time(s) — the NEW lane's", a.id, n)
	}
	if l.get(a.id) != a2 {
		t.Fatal("the replaced lane's cleanup removed its replacement from the set")
	}
}
