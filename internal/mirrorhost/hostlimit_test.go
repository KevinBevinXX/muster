package mirrorhost

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTwentyLimitedWindowsAreAllHeldThroughTheHostLimit: a usage limit is the
// account's. With 20 idle Claude windows whose latest turn each hit it, the
// window list carries a TurnError on MaxTurnErrorWindows of them only — and
// the host reports the limit once, for all of them (Msg.HostLimit: the newest
// record and its reset), so the queue holds the other four too. A real answer
// in ANY Claude window after the limit lifts it; one before it does not.
func TestTwentyLimitedWindowsAreAllHeldThroughTheHostLimit(t *testing.T) {
	dir := t.TempDir()
	var raw []WindowInfo
	for i := 0; i < 20; i++ {
		tr := filepath.Join(dir, "w"+strconv.Itoa(i)+".jsonl")
		off := time.Duration(i) * time.Second
		if err := os.WriteFile(tr, []byte(tePrompt(off, "q")+"\n"+teLimit(off+300*time.Millisecond)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		raw = append(raw, WindowInfo{Session: "s" + strconv.Itoa(i), ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1,
			Claude: &ClaudePane{Transcript: tr, Status: mirror.StatusIdle, StatusSince: 1}})
	}
	h := &hostSide{c: &Client{cfg: Config{Backend: &windowsBackend{windows: raw}}, log: log.New(io.Discard, "", 0)},
		ctx: context.Background(), version: mirror.ProtocolVersion}
	l, ok := h.read()
	if !ok {
		t.Fatal("the read failed")
	}
	with := 0
	for _, w := range l.windows {
		if w.TurnError != nil {
			with++
		}
	}
	newest := t0.Add(19*time.Second + 300*time.Millisecond).UnixMilli()
	reset := time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC).UnixMilli()
	if with != mirror.MaxTurnErrorWindows || l.hostLimit == nil || *l.hostLimit != (mirror.HostLimit{At: newest, ResetsAt: reset}) {
		t.Fatalf("%d windows carry a turn error (want the bound %d); host limit %+v (want at %d, resets %d)", with, mirror.MaxTurnErrorWindows,
			l.hostLimit, newest, reset)
	}
	if m := l.msg(mirror.TypeSessions); m.HostLimit == nil || *m.HostLimit != *l.hostLimit {
		t.Fatalf("the sessions message carries %+v; want the host limit", m.HostLimit)
	}

	// A real answer in a 21st Claude window, written BEFORE the newest limit:
	// still limited. Written after it: lifted.
	answered := func(at time.Duration) WindowInfo {
		tr := filepath.Join(dir, "a"+strconv.Itoa(int(at))+".jsonl")
		if err := os.WriteFile(tr, []byte(tePrompt(at-time.Second, "q")+"\n"+teReply(at, "fine")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return WindowInfo{Session: "answered", ID: "@1", Active: true, Pane: "%1", PanePID: 1,
			Claude: &ClaudePane{Transcript: tr, Status: mirror.StatusIdle, StatusSince: 1}}
	}
	for _, c := range []struct {
		at     time.Duration
		lifted bool
	}{{10 * time.Second, false}, {20 * time.Second, true}} {
		ws := append(append([]WindowInfo{}, raw...), answered(c.at))
		var r turnEndReader
		r.attach(ws)
		if got := hostLimitOf(ws); (got == nil) != c.lifted {
			t.Fatalf("a real answer at +%s: host limit %+v; want lifted %t", c.at, got, c.lifted)
		}
	}
}

// TestAListingWhoseHostLimitMovedIsNotEqual: the host re-sends its listing
// when only its limit changed (it compares listings to decide), so a limit
// that arrives, moves or lifts reaches muster with nothing else changing.
func TestAListingWhoseHostLimitMovedIsNotEqual(t *testing.T) {
	a := listing{windows: []mirror.Window{}}
	b := a
	b.hostLimit = &mirror.HostLimit{At: 5}
	c := a
	c.hostLimit = &mirror.HostLimit{At: 5, ResetsAt: 9}
	if a.equal(b) || b.equal(c) || !b.equal(listing{windows: []mirror.Window{}, hostLimit: &mirror.HostLimit{At: 5}}) {
		t.Fatal("listing.equal does not see the host limit")
	}
}
