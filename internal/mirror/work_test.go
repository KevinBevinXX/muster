package mirror_test

import (
	"context"
	"encoding/json"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestNormalizeWorkClampsAndRestrictsToListedWindows: a host cannot report
// work for a window it does not list, every count is clamped to
// [0, MaxWorkCount], a lower bound is at least 1, and an entry with nothing to
// show is dropped (absent means "nothing to show").
func TestNormalizeWorkClampsAndRestrictsToListedWindows(t *testing.T) {
	windows := []mirror.Window{{Session: "s", ID: "@1"}, {Session: "s", ID: "@2"}, {Session: "s", ID: "@3"}, {Session: "s", ID: "@4"}, {Session: "s", ID: "@5"}}
	got := mirror.NormalizeWork(map[string]mirror.WindowWork{
		"@1":  {Subagents: 3, Shells: 2},
		"@2":  {Subagents: 150, Shells: -4},
		"@3":  {ShellsAtLeast: true},
		"@4":  {},
		"@5":  {Shells: 7, ShellsAtLeast: true},
		"@99": {Subagents: 5},
	}, windows)
	want := map[string]mirror.WindowWork{
		"@1": {Subagents: 3, Shells: 2},
		"@2": {Subagents: mirror.MaxWorkCount},
		"@3": {Shells: 1, ShellsAtLeast: true},
		"@5": {Shells: 7, ShellsAtLeast: true},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("NormalizeWork = %v, want %v", got, want)
	}
	if got := mirror.NormalizeWork(map[string]mirror.WindowWork{"@4": {}}, windows); got != nil {
		t.Fatalf("all-zero work normalised to %v, want nil", got)
	}
}

// TestNormalizeColorsKeepsOnlyLowercaseHex: only `#rrggbb`, lowercase, for a
// listed window reaches muster's page; anything else — a name, uppercase, a
// short or long form, markup — is dropped whole.
func TestNormalizeColorsKeepsOnlyLowercaseHex(t *testing.T) {
	windows := []mirror.Window{{Session: "s", ID: "@1"}, {Session: "s", ID: "@2"}, {Session: "s", ID: "@3"},
		{Session: "s", ID: "@4"}, {Session: "s", ID: "@5"}, {Session: "s", ID: "@6"}, {Session: "s", ID: "@7"}}
	got := mirror.NormalizeColors(map[string]string{
		"@1":  "#3a7bd5",
		"@2":  "#3A7BD5",
		"@3":  "#abc",
		"@4":  "red",
		"@5":  "#3a7bd5;background:url(x)",
		"@6":  "#3a7bd5ff",
		"@7":  "",
		"@99": "#123456",
	}, windows)
	if want := map[string]string{"@1": "#3a7bd5"}; !maps.Equal(got, want) {
		t.Fatalf("NormalizeColors = %v, want %v", got, want)
	}
}

// TestWorkAndColorsMessagesReachEachWindowsEntry: a per-window v2 host's work
// and colors messages land on its entries — only for windows it lists — nudge
// the owner's views when they change (not when they repeat); an empty work map
// clears the counts; a host that went offline keeps its colours but NOT its
// counts (a count is a claim about now); and a host that never sends either
// (an older muster-mirror) has entries with no work and no colour. A v1 host's
// messages are ignored.
func TestWorkAndColorsMessagesReachEachWindowsEntry(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(time.Millisecond)
	var nudges atomic.Int32
	r.svc.SetNotify(func(int64) { nudges.Add(1) })
	tok, _ := r.store.mint(t, 7, "laptop")
	h := hello("laptop", false)
	h.Windows = []mirror.Window{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Claude: true, ClaudeSession: "sess-a"},
		{Session: "scratch2", ID: "@4", Index: 1},
	}
	host, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(window string) (mirror.SessionView, bool) {
		for _, hv := range r.svc.Hub().Hosts(7) {
			for _, e := range hv.Entries() {
				if e.Window == window {
					return e, true
				}
			}
		}
		return mirror.SessionView{}, false
	}
	eventually(t, "the host listed", func() bool { _, ok := entry("@7"); return ok })
	// Control: before either message — what an older host looks like.
	if e, _ := entry("@7"); !e.Work.IsZero() || e.Color != "" {
		t.Fatalf("a host that sent neither message has work %+v, colour %q", e.Work, e.Color)
	}
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	before := nudges.Load()
	send(mirror.Msg{Type: mirror.TypeWork, Work: map[string]mirror.WindowWork{"@7": {Subagents: 3, Shells: 2}, "@99": {Subagents: 1}}})
	eventually(t, "the work counts", func() bool { e, _ := entry("@7"); return e.Work == mirror.WindowWork{Subagents: 3, Shells: 2} })
	if got := r.svc.Hub().Hosts(7)[0].Work; len(got) != 1 {
		t.Fatalf("Work = %v, want only the listed window's", got)
	}
	eventually(t, "a nudge for the changed counts", func() bool { return nudges.Load() > before })
	send(mirror.Msg{Type: mirror.TypeColors, Colors: map[string]string{"@7": "#3a7bd5", "@4": "#D79921", "@99": "#123456"}})
	eventually(t, "the colour", func() bool { e, _ := entry("@7"); return e.Color == "#3a7bd5" })
	if e, _ := entry("@4"); e.Color != "" {
		t.Fatalf("an uppercase colour reached the entry: %q", e.Color)
	}
	time.Sleep(20 * time.Millisecond)
	settled := nudges.Load()
	send(mirror.Msg{Type: mirror.TypeWork, Work: map[string]mirror.WindowWork{"@7": {Subagents: 3, Shells: 2}}})
	send(mirror.Msg{Type: mirror.TypeColors, Colors: map[string]string{"@7": "#3a7bd5"}})
	time.Sleep(50 * time.Millisecond)
	if n := nudges.Load(); n != settled {
		t.Fatalf("the same work and colours again nudged %d times, want none", n-settled)
	}
	// An empty map clears the counts.
	send(mirror.Msg{Type: mirror.TypeWork, Work: map[string]mirror.WindowWork{}})
	eventually(t, "the counts cleared", func() bool { e, _ := entry("@7"); return e.Work.IsZero() })
	send(mirror.Msg{Type: mirror.TypeWork, Work: map[string]mirror.WindowWork{"@7": {Shells: 1, ShellsAtLeast: true}}})
	eventually(t, "the counts again", func() bool { e, _ := entry("@7"); return e.Work.ShellsAtLeast })

	// Offline: the colour is kept with the last list, the counts are not.
	host.ws.CloseNow()
	eventually(t, "the host offline", func() bool { hs := r.svc.Hub().Hosts(7); return len(hs) == 1 && !hs[0].Online })
	e, _ := entry("@7")
	if e.Color != "#3a7bd5" {
		t.Fatalf("an offline host's entry has colour %q, want its last one", e.Color)
	}
	if !e.Work.IsZero() {
		t.Fatalf("an offline host's entry still claims work %+v", e.Work)
	}

	// A v1 host has no rendered view, and both its messages are dropped.
	tok1, _ := r.store.mint(t, 8, "oldbox")
	h1 := hello("oldbox", false)
	h1.Version = 1
	h1.Windows = []mirror.Window{{Session: "s", ID: "@1", Index: 0, Active: true}}
	old, err := r.rawConnect(t, tok1, h1)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []mirror.Msg{
		{Type: mirror.TypeWork, Work: map[string]mirror.WindowWork{"@1": {Subagents: 2}}},
		{Type: mirror.TypeColors, Colors: map[string]string{"@1": "#3a7bd5"}},
	} {
		b, _ := json.Marshal(m)
		if err := old.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	for _, hv := range r.svc.Hub().Hosts(8) {
		if len(hv.Work) != 0 || len(hv.Colors) != 0 {
			t.Fatalf("a v1 host's work/colours were kept: %v %v", hv.Work, hv.Colors)
		}
	}
}
