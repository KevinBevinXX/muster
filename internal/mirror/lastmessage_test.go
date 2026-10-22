package mirror

import (
	"encoding/json"
	"testing"
)

// A Claude window's CARD activity — the list's order, its "active … ago" and
// its Idle section — is the last message its host reported, not tmux's window
// activity (which a tmux restart resets for every window). A window without
// one, and every non-Claude window, keeps tmux's.
func TestAClaudeWindowIsListedByItsLastMessage(t *testing.T) {
	const now = int64(1_792_600_000)
	in := []Window{
		// tmux: active a minute ago (the restore); last message 30 days ago.
		{Session: "old", ID: "@1", Claude: true, Activity: now - 61, LastMessage: now - 30*86400 - 17},
		// No last message from the host (an older muster-mirror): tmux's time.
		{Session: "olderhost", ID: "@2", Claude: true, Activity: now - 3*3600 - 5},
		// A shell claiming one: not Claude, so it has none.
		{Session: "shell", ID: "@3", Activity: now - 7*60 - 3, LastMessage: now - 40*86400},
		// From the future (clock skew): clamped to now.
		{Session: "skew", ID: "@4", Claude: true, Activity: now - 120, LastMessage: now + 999},
	}
	out := NormalizeWindows(in, now)
	byS := map[string]Window{}
	for _, w := range out {
		byS[w.Session] = w
	}
	floor := func(v int64) int64 { return v - v%ActivityBucket }
	for s, want := range map[string]int64{
		"old":       floor(now - 30*86400 - 17),
		"olderhost": floor(now - 3*3600 - 5),
		"shell":     floor(now - 7*60 - 3),
		"skew":      floor(now),
	} {
		if got := byS[s].CardActivity(); got != want {
			t.Errorf("%s: CardActivity = %d, want %d", s, got, want)
		}
	}
	if byS["shell"].LastMessage != 0 {
		t.Error("a non-Claude window kept a last message")
	}
	h := HostView{Host: "h", Online: true, Windows: out}
	order := []string{}
	act := map[string]int64{}
	for _, e := range Ordered([]HostView{h}) {
		order = append(order, e.Session)
		act[e.Session] = e.Activity
	}
	if want := "skew,shell,olderhost,old"; joined(order) != want {
		t.Errorf("order = %s, want %s (the 30-day-old conversation last, despite its fresh tmux time)", joined(order), want)
	}
	if act["old"] != floor(now-30*86400-17) {
		t.Errorf("the entry's Activity (what its card prints) = %d, want the last message", act["old"])
	}
	// A moved last message is a change to the window (Equal), but not a card
	// change outside activity's throttle (sameWindows), like Activity.
	a, b := byS["old"], byS["old"]
	b.LastMessage += ActivityBucket
	if a.Equal(b) {
		t.Error("Equal ignores LastMessage")
	}
	if !sameWindows([]Window{a}, []Window{b}) {
		t.Error("a last-message-only move counts as a list change, not an activity one")
	}
	// Wire compat: an older muster's Window has no such field and decodes the
	// rest; an older host's JSON has none and reads 0.
	raw, _ := json.Marshal(byS["old"])
	var old struct {
		Session  string `json:"session"`
		Activity int64  `json:"activity"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || old.Session != "old" || old.Activity != byS["old"].Activity {
		t.Errorf("an older muster cannot read a new window: %v %+v", err, old)
	}
	var fromOld Window
	if err := json.Unmarshal([]byte(`{"session":"s","id":"@1","claude":true,"activity":1792599900}`), &fromOld); err != nil || fromOld.CardActivity() != 1792599900 {
		t.Errorf("an older host's window: %v, CardActivity %d", err, fromOld.CardActivity())
	}
}

func joined(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
