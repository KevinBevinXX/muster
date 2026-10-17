package mirror_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE CLAUDE SESSION ID PER WINDOW (Window.ClaudeSession): what a mirrored
// session's focus is keyed by. A new host reports it; an older one does not,
// and must keep working with its windows simply un-focused.

const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"

func TestNormalizeWindowsKeepsOnlyAClaudePanesValidSessionID(t *testing.T) {
	in := []mirror.Window{
		{Session: "a", ID: "@1", Claude: true, ClaudeSession: sid},
		{Session: "a", ID: "@2", Claude: false, ClaudeSession: sid},               // not Claude: no conversation
		{Session: "a", ID: "@3", Claude: true, ClaudeSession: "../../etc/passwd"}, // not an id
		{Session: "a", ID: "@4", Claude: true, ClaudeSession: strings.Repeat("a", 65)},
		{Session: "a", ID: "@5", Claude: true},
	}
	got := mirror.NormalizeWindows(in, 0)
	want := []string{sid, "", "", "", ""}
	for i, w := range got {
		if w.ClaudeSession != want[i] {
			t.Errorf("window %s: ClaudeSession %q, want %q", w.ID, w.ClaudeSession, want[i])
		}
	}
}

// TestAnOlderHostWithNoSessionIDStillRegisters: a per-window hello written the
// way a muster-mirror that predates the field writes it — no claudeSession key
// at all — is accepted, listed, and its Claude window has no session id.
func TestAnOlderHostWithNoSessionIDStillRegisters(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "oldhost")
	h := mirror.Msg{Type: mirror.TypeHello, Version: mirror.ProtocolVersion, Host: "oldhost", Sessions: []string{"work"},
		Claude: []string{"work"}, Windows: []mirror.Window{{Session: "work", ID: "@7", Index: 1, Active: true, Claude: true}}}
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "claudeSession") {
		t.Fatalf("instrument check: this hello is not an older host's, it carries the field: %s", b)
	}
	if _, err := r.rawConnect(t, tok, h); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "oldhost", "work")
	entries := r.svc.Hub().Hosts(1)[0].Entries()
	if len(entries) != 1 || !entries[0].Claude || entries[0].Window != "@7" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].ClaudeSession != "" {
		t.Fatalf("an older host's window has session id %q", entries[0].ClaudeSession)
	}
}

// TestANewHostReportsTheSessionIDEndToEnd: a REAL muster-mirror client whose
// window runs Claude Code sends the conversation's id (ClaudePane.Gen), and the
// server's entry carries it — the positive control for the test above.
func TestANewHostReportsTheSessionIDEndToEnd(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "newhost")
	b := &winBackend{}
	b.set(mirrorhost.WindowInfo{Session: "work", ID: "@9", Index: 2, Active: true, Pane: "%3", PanePID: 10,
		Claude: &mirrorhost.ClaudePane{Pane: "%3", PID: 11, Transcript: "/x/" + sid + ".jsonl", Gen: sid}})
	r.connect(t, "newhost", tok, b, false)
	r.online(t, 1, "newhost", "work")
	var got string
	eventually(t, "the session id", func() bool {
		for _, h := range r.svc.Hub().Hosts(1) {
			for _, e := range h.Entries() {
				got = e.ClaudeSession
			}
		}
		return got != ""
	})
	if got != sid {
		t.Fatalf("session id = %q, want %q", got, sid)
	}
}
