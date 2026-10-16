package mirror_test

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE ACTIVITY-ONLY THROTTLE: a card's "active … ago" moving with nothing else
// re-renders its owner's open views at most once per throttle window PER OWNER
// (however many of its hosts are busy), one inside the window becomes ONE
// trailing nudge, the trailing nudge opens a window of its own, and a raw time
// that moved inside its bucket is no change on a per-window host either.
//
// 🔴 Every silent step is proven processed before its count is read (the hub's
// stored activity shows the newer value, or a later message on the same
// connection has landed), and each test's connect is its positive control.
// ---------------------------------------------------------------------------

// nudgeCounter counts owner 1's nudges.
func nudgeCounter(r *rig) *atomic.Int64 {
	var n atomic.Int64
	r.svc.SetNotify(func(owner int64) {
		if owner == 1 {
			n.Add(1)
		}
	})
	return &n
}

// settled is n once every immediate nudge owed has fired (well past the 5 ms
// debounce these tests set).
func settled(n *atomic.Int64) int64 {
	time.Sleep(120 * time.Millisecond)
	return n.Load()
}

// TestAnOwnersBusyHostsShareOneActivityThrottle: several hosts of ONE owner
// each move a card's time inside one throttle window. The owner's open views
// are re-rendered once at once and once trailing — not once per host.
func TestAnOwnersBusyHostsShareOneActivityThrottle(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	const throttle = 800 * time.Millisecond
	r.svc.SetActivityNudgeEvery(throttle)
	nudges := nudgeCounter(r)
	const hosts = 4
	now := bucket(time.Now().Unix()) - 100*mirror.ActivityBucket
	// Host i's session is i*10 buckets behind host 0's: moving every one of
	// them a bucket never changes the order.
	at := func(i, step int) int64 { return now - int64(i*10-step)*mirror.ActivityBucket }
	raws := make([]*rawHost, hosts)
	for i := range hosts {
		name := fmt.Sprintf("host%d", i)
		tok, _ := r.store.mint(t, 1, name)
		h := hello(name, false, "s")
		h.Activity = map[string]int64{"s": at(i, 0)}
		raw, err := r.rawConnect(t, tok, h)
		if err != nil {
			t.Fatal(err)
		}
		raws[i] = raw
		r.online(t, 1, name, "s")
	}
	order := orderOf(r.svc.Hub().Hosts(1))
	base := settled(nudges)
	if base == 0 {
		t.Fatal("instrument check FAILED: connecting the hosts nudged nobody, so this counter cannot see a nudge")
	}
	began := time.Now()
	for i, raw := range raws {
		raw.sendActivity(t, []string{"s"}, map[string]int64{"s": at(i, 1)})
		name := fmt.Sprintf("host%d", i)
		eventually(t, "the hub to take "+name+"'s newer time", func() bool {
			return hostView(t, r, 1, name).Activity["s"] == at(i, 1)
		})
	}
	if got := orderOf(r.svc.Hub().Hosts(1)); !slices.Equal(got, order) {
		t.Fatalf("the order moved (%v -> %v), so this is not the activity-only case", order, got)
	}
	if time.Since(began) >= throttle {
		t.Fatalf("the sends took %v, longer than the %v throttle window: the measurement below would be meaningless", time.Since(began), throttle)
	}
	if got := settled(nudges); got != base+1 {
		t.Fatalf("%d hosts of one owner moving a card's time in one window nudged %d time(s) at once, want 1", hosts, got-base)
	}
	eventually(t, "the trailing nudge", func() bool { return nudges.Load() == base+2 })
	time.Sleep(throttle / 2)
	if got := nudges.Load(); got != base+2 {
		t.Fatalf("the window's changes nudged %d time(s) in total, want 2 (one at once, one trailing)", got-base)
	}
}

// TestAChangeJustAfterATrailingNudgeWaitsForTheNextWindow: the trailing nudge
// opens a throttle window of its own, so a card's time moving right after it is
// NOT re-rendered at once — it waits for that window's end.
func TestAChangeJustAfterATrailingNudgeWaitsForTheNextWindow(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	const throttle = 500 * time.Millisecond
	r.svc.SetActivityNudgeEvery(throttle)
	nudges := nudgeCounter(r)
	tok, _ := r.store.mint(t, 1, "laptop")
	now := bucket(time.Now().Unix()) - 100*mirror.ActivityBucket
	h := hello("laptop", false, "s")
	h.Activity = map[string]int64{"s": now}
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "s")
	base := settled(nudges)
	if base == 0 {
		t.Fatal("instrument check FAILED: the connect nudged nobody, so this counter cannot see a nudge")
	}
	step := 0
	move := func() {
		step++
		want := now + int64(step)*mirror.ActivityBucket
		raw.sendActivity(t, []string{"s"}, map[string]int64{"s": want})
		eventually(t, "the hub to take the newer time", func() bool { return hostView(t, r, 1, "laptop").Activity["s"] == want })
	}
	move() // at once: the window was open
	move() // inside the window: the trailing nudge
	eventually(t, "the trailing nudge", func() bool { return nudges.Load() == base+2 })
	trailingAt := time.Now()
	move() // just after the trailing nudge: inside the window IT opened
	if time.Since(trailingAt) >= throttle-150*time.Millisecond {
		t.Fatalf("the send took %v, most of the %v throttle window: the measurement below would be meaningless", time.Since(trailingAt), throttle)
	}
	if got := settled(nudges); got != base+2 {
		t.Fatalf("a change just after the trailing nudge nudged %d time(s) at once, want none (the trailing nudge opens a window)", got-base-2)
	}
	eventually(t, "the next window's trailing nudge", func() bool { return nudges.Load() == base+3 })
	if waited := time.Since(trailingAt); waited < throttle-50*time.Millisecond {
		t.Fatalf("the next trailing nudge came %v after the last, inside the %v throttle", waited, throttle)
	}
}

// TestAPerWindowRawTimeMovingInsideItsBucketIsNoChange: a per-window host's
// window reports a raw time that moved but stayed in its bucket. The card
// prints the floored time, so nothing it shows moved: no nudge, at once or
// trailing. A move into the next bucket (the positive control, and the proof
// the first message was processed) nudges exactly once.
func TestAPerWindowRawTimeMovingInsideItsBucketIsNoChange(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	const throttle = 300 * time.Millisecond
	r.svc.SetActivityNudgeEvery(throttle)
	nudges := nudgeCounter(r)
	tok, _ := r.store.mint(t, 1, "laptop")
	b := bucket(time.Now().Unix()) - 100*mirror.ActivityBucket
	wins := func(a int64) []mirror.Window {
		return []mirror.Window{
			{Session: "main", ID: "@1", Index: 0, Active: true, Activity: a},
			{Session: "main", ID: "@2", Index: 1, Activity: b - 50*mirror.ActivityBucket},
		}
	}
	raw, err := r.rawConnect(t, tok, winHello("laptop", false, wins(b+5)...))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "main")
	base := settled(nudges)
	if base == 0 {
		t.Fatal("instrument check FAILED: the connect nudged nobody, so this counter cannot see a nudge")
	}
	at := func() int64 { e, _ := hostView(t, r, 1, "laptop").Find("main", "@1"); return e.Activity }
	if at() != b {
		t.Fatalf("the window's time is %d, want it floored to %d", at(), b)
	}

	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: wins(b + 50)}) // same bucket
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: wins(b + mirror.ActivityBucket)})
	eventually(t, "the hub to take the next bucket", func() bool { return at() == b+mirror.ActivityBucket })
	// Past any trailing nudge either message could have scheduled.
	time.Sleep(throttle + 120*time.Millisecond)
	if got := nudges.Load() - base; got != 1 {
		t.Fatalf("a raw time moving inside its bucket, then into the next, nudged %d time(s), want 1 (only the bucket move)", got)
	}
}
