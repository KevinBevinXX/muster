package mirrorhost

import (
	"testing"
	"time"
)

// 🔴 BUSY COUNTS AS ACTIVE: a Claude window whose last message is 30 days old
// is sent with the host's NOW (floored to the minute) while its session works
// — a turn running, waiting on the person, a background shell of its own —
// and with its last message again once it is idle. So a window running a long
// tool keeps its place at the top instead of reading "active 30d ago".
func TestAWorkingClaudeWindowIsActiveNow(t *testing.T) {
	now := mustTime(t, "2026-10-21T16:20:47Z")
	old := now.Add(-30*24*time.Hour - 7*time.Minute).Unix()
	pane := func(status, raw string) *ClaudePane {
		return &ClaudePane{Pane: "%1", Transcript: "/x", Status: status, RawStatus: raw, LastMessage: old}
	}
	windows := []WindowInfo{
		{Session: "busy", ID: "@1", Claude: pane("busy", "busy")},
		{Session: "waiting", ID: "@2", Claude: pane("waiting", "waiting")},
		{Session: "shell", ID: "@3", Claude: pane("idle", "shell")},
		{Session: "idle", ID: "@4", Claude: pane("idle", "idle")},
	}
	floor := func(v int64) int64 { return v - v%60 }
	got := map[string]int64{}
	for _, w := range WireWindows(windows, nil, true, now) {
		got[w.Session] = w.CardActivity()
	}
	for s, want := range map[string]int64{"busy": floor(now.Unix()), "waiting": floor(now.Unix()), "shell": floor(now.Unix()), "idle": floor(old)} {
		if got[s] != want {
			t.Errorf("%s: card activity %d, want %d", s, got[s], want)
		}
	}
	// The same window going idle: its last message rules again.
	windows[0].Claude = pane("idle", "idle")
	if w := WireWindows(windows, nil, true, now)[0]; w.Session != "busy" || w.CardActivity() != floor(old) {
		t.Errorf("gone idle: %s card activity %d, want the last message %d", w.Session, w.CardActivity(), floor(old))
	}
	// Floored: a busy window's time moves once a minute, not every listing.
	a := WireWindows(windows[1:2], nil, true, now)[0].LastMessage
	b := WireWindows(windows[1:2], nil, true, now.Add(9*time.Second))[0].LastMessage
	if a != b {
		t.Errorf("two listings 9s apart in one minute sent %d and %d", a, b)
	}
}
