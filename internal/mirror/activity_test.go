package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// ---------------------------------------------------------------------------
// MOST RECENTLY ACTIVE FIRST: a host may say when each session was last active.
// It orders the list and is printed on each card ("active … ago"), optional
// on the wire in both directions, bounded and floored (mirror.ActivityBucket) —
// and a host's update re-renders open Agents views at once when the ORDER
// comes out different, and at most once per throttle window per OWNER when
// only a card's shown time moved.
// ---------------------------------------------------------------------------

// bucket is t floored the way the wire carries it.
func bucket(t int64) int64 { return t - t%mirror.ActivityBucket }

func TestNormalizeActivityKeepsListedSessionsFlooredAndCapsTheFuture(t *testing.T) {
	// 1_800_000_037 is deliberately NOT on a bucket boundary (37 s into one):
	// a fixture already on the boundary cannot tell flooring from no flooring.
	const now = int64(1_800_000_037)
	got := mirror.NormalizeActivity(map[string]int64{
		"busy":    now - 5,    // this bucket (floored to its start)
		"earlier": now - 400,  // six buckets back
		"ghost":   now - 1,    // not listed: dropped
		"zero":    0,          // not a time: dropped
		"neg":     -17,        // not a time: dropped
		"ahead":   now + 9000, // a clock running ahead: capped at now
		"ancient": 31,         // floors to 0: dropped
	}, []string{"ahead", "ancient", "busy", "earlier", "neg", "plain", "zero"}, now)
	want := map[string]int64{"busy": 1_800_000_000, "earlier": 1_799_999_580, "ahead": 1_800_000_000}
	if !maps.Equal(got, want) {
		t.Fatalf("NormalizeActivity = %v, want %v", got, want)
	}
	// now <= 0 skips the cap (and only the cap).
	if got := mirror.NormalizeActivity(map[string]int64{"a": now + 9000}, []string{"a"}, 0); got["a"] != bucket(now+9000) {
		t.Fatalf("with no clock the entry must be floored and kept, got %v", got)
	}
	if got := mirror.NormalizeActivity(map[string]int64{"ghost": now}, []string{"main"}, now); got != nil {
		t.Fatalf("nothing valid left must be nil (omitted on the wire), got %v", got)
	}
	if got := mirror.NormalizeActivity(nil, []string{"main"}, now); got != nil {
		t.Fatalf("no activity must be nil, got %v", got)
	}
}

// TestActivityIsOmittedFromTheWireWhenAbsent: a host with no activity sends
// exactly what an older host sends, so an older muster sees no difference —
// and an older muster's decoder ignores the field when it is there.
func TestActivityIsOmittedFromTheWireWhenAbsent(t *testing.T) {
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: []string{"main"}})
	if strings.Contains(string(b), "activity") {
		t.Fatalf("a message without activity carries the key: %s", b)
	}
	b, _ = json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: []string{"main"}, Activity: map[string]int64{"main": 1_800_000_000}})
	if !strings.Contains(string(b), `"activity":{"main":1800000000}`) {
		t.Fatalf("a message with activity does not carry it: %s", b)
	}
	// The pre-activity Msg, as an older muster decodes it.
	var old struct {
		Type     string   `json:"type"`
		Sessions []string `json:"sessions"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.Type != mirror.TypeSessions || !slices.Equal(old.Sessions, []string{"main"}) {
		t.Fatalf("an older decoder cannot read a message carrying activity: %v %+v", err, old)
	}
}

func orderOf(hosts []mirror.HostView) []string {
	var out []string
	for _, s := range mirror.Ordered(hosts) {
		out = append(out, s.Host+"/"+s.Session)
	}
	return out
}

// TestOrderedIsMostRecentlyActiveFirstAcrossHostsAndStable pins the whole
// ordering rule on one fixture: newest first ACROSS hosts; equal times keep
// the (host, session-name) order they always had; sessions with no activity
// come last in that same order; and with no activity at all the list is the
// old one exactly.
func TestOrderedIsMostRecentlyActiveFirstAcrossHostsAndStable(t *testing.T) {
	hosts := []mirror.HostView{
		{Host: "laptop", Online: true, Interactive: true, Sessions: []string{"alpha", "beta", "delta", "gamma"}, Claude: []string{"beta"},
			Labels:   map[string]string{"beta": "mango:2"},
			Activity: map[string]int64{"alpha": 600, "beta": 1200, "gamma": 600}},
		{Host: "tower", Sessions: []string{"one", "three", "two"},
			Activity: map[string]int64{"one": 900, "two": 1200}},
	}
	want := []string{
		"laptop/beta",  // 1200, and laptop sorts before tower
		"tower/two",    // 1200
		"tower/one",    // 900
		"laptop/alpha", // 600 — tied with gamma, name order kept
		"laptop/gamma", // 600
		"laptop/delta", // no activity: last, in the old order
		"tower/three",  // no activity
	}
	if got := orderOf(hosts); !slices.Equal(got, want) {
		t.Fatalf("Ordered = %v\n         want %v", got, want)
	}
	// Each row carries its own host's facts, not its neighbour's.
	first := mirror.Ordered(hosts)[0]
	if first.Label != "mango:2" || !first.Claude || !first.Online || !first.Interactive || first.Activity != 1200 {
		t.Fatalf("laptop/beta's row = %+v", first)
	}
	if second := mirror.Ordered(hosts)[1]; second.Label != "" || second.Claude || second.Online || second.Interactive {
		t.Fatalf("tower/two's row took another host's facts: %+v", second)
	}

	// Control: no activity anywhere (every host an older muster-mirror) — the
	// list is exactly what it was before ordering existed.
	for i := range hosts {
		hosts[i].Activity = nil
	}
	old := []string{"laptop/alpha", "laptop/beta", "laptop/delta", "laptop/gamma", "tower/one", "tower/three", "tower/two"}
	if got := orderOf(hosts); !slices.Equal(got, old) {
		t.Fatalf("with no activity Ordered = %v, want the old order %v", got, old)
	}
}

func (h *rawHost) sendActivity(t *testing.T, sessions []string, activity map[string]int64) {
	t.Helper()
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: sessions, Activity: activity})
	if err := h.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// TestTheHubKeepsValidActivityAndDropsTheRestWithoutRefusingTheHello: a hello
// carrying good, bad, unlisted and future activity registers, keeping the good
// entries floored and the future one capped at muster's own clock.
func TestTheHubKeepsValidActivityAndDropsTheRestWithoutRefusingTheHello(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	now := time.Now().Unix()
	h := hello("laptop", false, "busy", "idle", "junk", "skewed")
	h.Activity = map[string]int64{"busy": now - 90, "idle": now - 7200, "junk": -4, "unlisted": now, "skewed": now + 86400}
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatalf("a hello with invalid activity was refused: %v", err)
	}
	r.online(t, 1, "laptop", "busy")
	got := hostView(t, r, 1, "laptop").Activity
	if got["busy"] != bucket(now-90) || got["idle"] != bucket(now-7200) || len(got) != 3 {
		t.Fatalf("activity = %v, want busy=%d idle=%d and skewed capped", got, bucket(now-90), bucket(now-7200))
	}
	// Capped at muster's clock: within a bucket of now, never a day ahead.
	if s := got["skewed"]; s > time.Now().Unix() || s < bucket(now) {
		t.Fatalf("a time a day in the future was kept as %d (now %d)", s, now)
	}

	// A sessions message REPLACES the activity, and one with none clears it
	// (the host stopped reporting): the list falls back to name order.
	raw.sendActivity(t, []string{"busy", "idle", "junk", "skewed"}, map[string]int64{"idle": now - 30})
	eventually(t, "the activity to follow the sessions message", func() bool {
		return maps.Equal(hostView(t, r, 1, "laptop").Activity, map[string]int64{"idle": bucket(now - 30)})
	})
	raw.sendSessions(t, "busy", "idle", "junk", "skewed")
	eventually(t, "a sessions message without activity to clear it", func() bool {
		return hostView(t, r, 1, "laptop").Activity == nil
	})
}

// TestAnOfflineHostKeepsItsLastActivity: a disconnected host is shown with its
// last list, and that list keeps its place in the order.
func TestAnOfflineHostKeepsItsLastActivity(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	now := time.Now().Unix()
	h := hello("laptop", false, "aaa", "zzz")
	h.Activity = map[string]int64{"zzz": now - 300}
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "zzz")
	raw.ws.CloseNow()
	eventually(t, "the host to go offline", func() bool {
		hv := r.svc.Hub().Hosts(1)
		return len(hv) == 1 && !hv[0].Online
	})
	if got := orderOf(r.svc.Hub().Hosts(1)); !slices.Equal(got, []string{"laptop/zzz", "laptop/aaa"}) {
		t.Fatalf("offline order = %v, want zzz (active) before aaa", got)
	}
}

// TestAnActivityUpdateNudgesOpenViewsThrottled is the hub's nudge
// policy for activity: a card prints its time ("active … ago"), so a time that
// moves a bucket re-renders the open views even when the order stays put — but
// an activity-only change nudges at most once per throttle window per owner,
// one inside the window becomes ONE trailing nudge at its end (not none, not
// one each), a raw time that moved within its bucket is no change at all, and
// an order change nudges at once, throttle or not.
//
// 🔴 EACH SILENT STEP IS PROVEN TO HAVE BEEN PROCESSED before its nudge count
// is read (the hub's stored activity shows the new value, or a later message
// on the same connection has landed), so "no nudge" is never "the message had
// not arrived yet"; and the connect is the positive control — the counter can
// see a nudge.
func TestAnActivityUpdateNudgesOpenViewsThrottled(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	// Long enough that a settle() (120 ms) fits inside it many times over,
	// short enough that the trailing nudge is waited for, not slept past.
	const throttle = 700 * time.Millisecond
	r.svc.SetActivityNudgeEvery(throttle)
	var nudges atomic.Int64
	r.svc.SetNotify(func(owner int64) {
		if owner == 1 {
			nudges.Add(1)
		}
	})
	tokA, _ := r.store.mint(t, 1, "laptop")
	tokB, _ := r.store.mint(t, 1, "tower")
	now := bucket(time.Now().Unix()) - 10*mirror.ActivityBucket
	sessA := []string{"a1", "a2"}
	ha := hello("laptop", false, sessA...)
	ha.Activity = map[string]int64{"a1": now, "a2": now - 5*mirror.ActivityBucket}
	rawA, err := r.rawConnect(t, tokA, ha)
	if err != nil {
		t.Fatal(err)
	}
	hb := hello("tower", false, "b1")
	hb.Activity = map[string]int64{"b1": now - 2*mirror.ActivityBucket}
	if _, err := r.rawConnect(t, tokB, hb); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "a1")
	r.online(t, 1, "tower", "b1")
	if got, want := orderOf(r.svc.Hub().Hosts(1)), []string{"laptop/a1", "tower/b1", "laptop/a2"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	settle := func() int64 {
		// Well past the 5 ms debounce: any immediate nudge the last message
		// owed has fired.
		time.Sleep(120 * time.Millisecond)
		return nudges.Load()
	}
	base := settle()
	if base == 0 {
		t.Fatal("instrument check FAILED: connecting two hosts nudged nobody, so this counter cannot see a nudge")
	}
	a1 := func() int64 { return hostView(t, r, 1, "laptop").Activity["a1"] }
	send := func(a1, a2 int64) { rawA.sendActivity(t, sessA, map[string]int64{"a1": a1, "a2": a2}) }

	// 1. The top session stays busy: its time moves a bucket, the order does
	// not. Its card's "active … ago" did move, so the views are told — at once,
	// the first activity-only change this host has made.
	firstAt := time.Now() // before the hub can have stamped its nudge
	send(now+mirror.ActivityBucket, now-5*mirror.ActivityBucket)
	eventually(t, "the hub to take a1's newer time", func() bool { return a1() == now+mirror.ActivityBucket })
	if got := settle(); got != base+1 {
		t.Fatalf("a shown time moving (order unchanged) nudged %d time(s), want exactly 1", got-base)
	}

	// 2. Another bucket, INSIDE the throttle window: no nudge now ...
	send(now+2*mirror.ActivityBucket, now-5*mirror.ActivityBucket)
	eventually(t, "the hub to take a1's next time", func() bool { return a1() == now+2*mirror.ActivityBucket })
	// ... and a third that joins the same trailing nudge rather than adding one.
	send(now+3*mirror.ActivityBucket, now-5*mirror.ActivityBucket)
	eventually(t, "the hub to take a1's third time", func() bool { return a1() == now+3*mirror.ActivityBucket })
	if got := settle(); got != base+1 {
		t.Fatalf("activity-only changes inside the throttle window nudged %d time(s) at once, want none", got-base-1)
	}
	// 3. The window's end delivers ONE trailing nudge, so the last change of the
	// burst still reaches the cards.
	eventually(t, "the trailing nudge", func() bool { return nudges.Load() == base+2 })
	if waited := time.Since(firstAt); waited < throttle {
		t.Fatalf("the trailing nudge came %v after the first, inside the %v throttle", waited, throttle)
	}
	if got := settle(); got != base+2 {
		t.Fatalf("the burst nudged %d time(s) in total, want 2 (one at once, one trailing)", got-base)
	}

	// 4. A raw time that moved WITHIN its bucket shows the same card: nothing.
	// The window is open again (sleep past it), so a nudge here would be
	// immediate and would land before step 5's message.
	time.Sleep(throttle)
	send(now+3*mirror.ActivityBucket+30, now-5*mirror.ActivityBucket)
	if got := settle(); got != base+2 {
		t.Fatalf("a raw time moving inside its bucket nudged %d time(s), want none", got-base-2)
	}

	// 5. a2 catches up with b1's bucket: a tie, which the stable order breaks
	// laptop-first — so a2 moves above b1. An ORDER change nudges at once.
	// (It is also the proof step 4 was processed: one connection, in order.)
	send(now+3*mirror.ActivityBucket, now-2*mirror.ActivityBucket)
	eventually(t, "a2 to move above b1", func() bool {
		return slices.Equal(orderOf(r.svc.Hub().Hosts(1)), []string{"laptop/a1", "laptop/a2", "tower/b1"})
	})
	if got := settle(); got != base+3 {
		t.Fatalf("an activity update that reordered the list nudged %d time(s), want exactly 1", got-base-2)
	}
}

// activityBackend is fakeBackend plus per-session last-active times.
type activityBackend struct {
	fakeBackend
	amu      sync.Mutex
	activity map[string]int64
}

func (b *activityBackend) setActivity(session string, at int64) {
	b.amu.Lock()
	defer b.amu.Unlock()
	if b.activity == nil {
		b.activity = map[string]int64{}
	}
	b.activity[session] = at
}

func (b *activityBackend) Activity(context.Context) (map[string]int64, error) {
	b.amu.Lock()
	defer b.amu.Unlock()
	return maps.Clone(b.activity), nil
}

// TestASessionThatBecomesActiveMovesToTheTopWithTheSameSessionSet runs the REAL
// muster-mirror client against the REAL hub, through NoShrink as the shipped
// binary wraps its backend: the session set never changes, only which session
// was active last, and muster's order follows. A refresh that compares only
// sessions, the Claude set and labels never re-sends, and the order stays put.
func TestASessionThatBecomesActiveMovesToTheTopWithTheSameSessionSet(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	b := &activityBackend{}
	b.set("apple", "pear", "zebra")
	now := time.Now().Unix()
	b.setActivity("zebra", now-3*mirror.ActivityBucket)
	b.setActivity("pear", now-30*mirror.ActivityBucket)
	// apple: no activity known for it at all — it sorts last.

	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "laptop", Backend: mirrorhost.NoShrink(b),
		AllowHTTP: true, Refresh: 30 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()

	r.online(t, 1, "laptop", "zebra")
	if got, want := orderOf(r.svc.Hub().Hosts(1)), []string{"laptop/zebra", "laptop/pear", "laptop/apple"}; !slices.Equal(got, want) {
		t.Fatalf("the hello's order = %v, want %v", got, want)
	}
	b.setActivity("pear", now-mirror.ActivityBucket)
	eventually(t, "pear (now the most recently active) to lead, same session set", func() bool {
		return slices.Equal(orderOf(r.svc.Hub().Hosts(1)), []string{"laptop/pear", "laptop/zebra", "laptop/apple"})
	})
}
