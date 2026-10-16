package mirror_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TURNS (turns.go): the hub records the window's status time at every paste
// that may have landed, and when it first observed the current status, on its
// own clock.

// TestTheHubRecordsTheStatusTimeAtEveryPasteThatMayHaveLanded: a paste the
// host confirmed records the StatusSince the window held then (Pasted); so
// does one the host never answered (it may have landed). A paste the host
// refused, and one muster refused before the host, record nothing. Pasted only
// grows, and it is the window's own: another window's paste does not move it.
func TestTheHubRecordsTheStatusTimeAtEveryPasteThatMayHaveLanded(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle, t1)...)
	m.StatusReport = true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	at := func() mirror.AgentState { return r.svc.AgentState(asUser(1), "h", "work", "@1") }
	if p := at().Pasted; p != 0 {
		t.Fatalf("before any paste, Pasted = %d", p)
	}
	answer := func(n int, why string) {
		go func() {
			eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == n })
			raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[n-1].ID, Error: why})
		}()
	}
	sessions := func(since int64) {
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: statusWindows(mirror.StatusIdle, since)})
		eventually(t, "the new status time", func() bool { return at().Since == since })
	}

	// Refused by the host: nothing.
	answer(1, "no")
	_ = r.svc.PasteWindow(asUser(1), "h", "work", "@1", "refused", mirror.PasteClaude)
	if p := at().Pasted; p != 0 {
		t.Fatalf("a paste the host refused recorded Pasted = %d", p)
	}
	// Refused by muster (a shell window): nothing, on either window.
	_ = r.svc.PasteWindow(asUser(1), "h", "work", "@2", "refused", mirror.PasteClaude)
	if p := at().Pasted; p != 0 {
		t.Fatalf("a paste muster refused recorded Pasted = %d", p)
	}
	// Confirmed, while the window holds t1.
	answer(2, "")
	if err := r.svc.PasteWindow(asUser(1), "h", "work", "@1", "confirmed", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	if p := at().Pasted; p != t1 {
		t.Fatalf("a confirmed paste at t1 recorded Pasted = %d, want %d", p, t1)
	}
	// Unanswered, while it holds t2: it may have landed.
	sessions(t2)
	ctx, cancel := context.WithTimeout(asUser(1), 100*time.Millisecond)
	defer cancel()
	_ = r.svc.PasteWindow(ctx, "h", "work", "@1", "unanswered", mirror.PasteClaude)
	if p := at().Pasted; p != t2 {
		t.Fatalf("an unconfirmed paste at t2 recorded Pasted = %d, want %d", p, t2)
	}
	// A host that reports an OLDER time (it never should) does not lower it.
	sessions(t1)
	answer(4, "")
	if err := r.svc.PasteWindow(asUser(1), "h", "work", "@1", "older", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	if p := at().Pasted; p != t2 {
		t.Fatalf("a paste at an older time lowered Pasted to %d, want %d", p, t2)
	}
	// Another owner sees nothing of it.
	if p := r.svc.AgentState(asUser(2), "h", "work", "@1").Pasted; p != 0 {
		t.Fatalf("another owner's view of Pasted = %d", p)
	}
}

// TestObservedIsWhenTheHubFirstSawTheStatus: Observed is muster's own clock at
// the report that brought a new (status, time) pair; a report repeating the
// pair keeps it, a newer time (a turn nobody saw busy) moves it, and so does a
// status change with the same time. It is never the host's time.
func TestObservedIsWhenTheHubFirstSawTheStatus(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle, t1)...)
	m.StatusReport = true
	before := time.Now()
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	at := func() mirror.AgentState { return r.svc.AgentState(asUser(1), "h", "work", "@1") }
	first := at().Observed
	if first.Before(before) || first.After(time.Now()) {
		t.Fatalf("Observed = %v, want muster's clock at the hello (after %v); the host's time is %v", first, before, time.UnixMilli(t1))
	}
	send := func(status string, since int64, activity int64) {
		ws := statusWindows(status, since)
		ws[0].Activity = activity
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: ws})
	}
	time.Sleep(20 * time.Millisecond)
	send(mirror.StatusIdle, t1, time.Now().Unix())
	// A visible-only change that the hub has applied: wait for it, then check.
	eventually(t, "the repeat applied", func() bool { return at().Since == t1 })
	time.Sleep(50 * time.Millisecond)
	if got := at().Observed; !got.Equal(first) {
		t.Fatalf("a repeated (status, time) moved Observed from %v to %v", first, got)
	}
	send(mirror.StatusIdle, t2, 0)
	eventually(t, "a newer time", func() bool { return at().Since == t2 })
	second := at().Observed
	if !second.After(first) {
		t.Fatalf("a newer status time kept Observed at %v (was %v)", second, first)
	}
	time.Sleep(20 * time.Millisecond)
	send(mirror.StatusBusy, t2, 0)
	eventually(t, "busy at the same time", func() bool { return at().Status == mirror.StatusBusy })
	if got := at().Observed; !got.After(second) {
		t.Fatalf("a new status with the same time kept Observed at %v", got)
	}
}
