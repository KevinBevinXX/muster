package mirror_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAWindowStartingToWaitCallsTheWaitingHookAndNudgesNoCard: a Claude
// window going idle → waiting → busy calls the waiting hook on each change of
// the waiting SET (twice), never for a change that leaves it as it was, and
// nudges no card (no card draws Status) — the attention queue's only way to
// hear a session start or stop waiting.
func TestAWindowStartingToWaitCallsTheWaitingHookAndNudgesNoCard(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	var nudges, waits atomic.Int64
	r.svc.SetNotify(func(int64) { nudges.Add(1) })
	r.svc.SetWaitingNotify(func(owner int64) {
		if owner == 1 {
			waits.Add(1)
		}
	})
	tok, _ := r.store.mint(t, 1, "laptop")
	win := func(status string, since int64) mirror.Window {
		return mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b",
			Status: status, StatusSince: since}
	}
	raw, err := r.rawConnect(t, tok, winHello("laptop", false, win(mirror.StatusIdle, 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "work")
	time.Sleep(120 * time.Millisecond)
	base := nudges.Load()
	send := func(w mirror.Window) {
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: []mirror.Window{w}})
		time.Sleep(120 * time.Millisecond)
	}
	send(win(mirror.StatusWaiting, 2))
	eventually(t, "the waiting hook", func() bool { return waits.Load() == 1 })
	send(win(mirror.StatusWaiting, 3)) // still waiting: the set did not change
	send(win(mirror.StatusBusy, 4))
	eventually(t, "the hook on leaving", func() bool { return waits.Load() == 2 })
	time.Sleep(120 * time.Millisecond)
	if got := waits.Load(); got != 2 {
		t.Fatalf("the waiting hook ran %d times, want 2", got)
	}
	if got := nudges.Load(); got != base {
		t.Fatalf("a status-only change nudged the cards %d time(s)", got-base)
	}
}
