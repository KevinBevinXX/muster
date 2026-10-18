package mirror_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE ASK HOOK'S HALF OF THE PLUGIN (service.go AuthenticateHost /
// HostClaudeSession, hub.go SetWaiting): the hook's token door, the "is this
// host's session" check, and the WAITING overlay that a question held by the
// hook puts on its window.

// askWindows: @1 a Claude pane in session sess-a, BUSY (what Claude Code's own
// file says while a PreToolUse hook runs), @2 a Claude pane in sess-b, idle.
func askWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, Status: mirror.StatusBusy, StatusSince: t1, ClaudeSession: "sess-a"},
		{Session: "work", ID: "@2", Index: 1, Claude: true, Status: mirror.StatusIdle, StatusSince: t2, ClaudeSession: "sess-b"},
	}
}

func windowStatus(t *testing.T, r *rig, owner int64, host, id string) string {
	t.Helper()
	for _, h := range r.svc.Hosts(asUser(owner)) {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if w.ID == id {
				return w.Status
			}
		}
	}
	t.Fatalf("no window %s on %s", id, host)
	return ""
}

// TestAHeldQuestionMarksItsWindowWaiting: with the overlay saying sess-a is
// waiting, window @1 reads WAITING in Hosts and in AgentState (the send
// queue's read), keeping the CLI's own status time; @2 and another owner's
// view are untouched. The control: the same windows with the overlay saying
// no read busy.
func TestAHeldQuestionMarksItsWindowWaiting(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "lap")
	m := winHello("lap", true, askWindows()...)
	m.StatusReport = true
	if _, err := r.rawConnect(t, tok, m); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "lap", "work")

	holding := false
	var asked []string
	r.svc.SetWaiting(func(owner int64, host, session string) bool {
		asked = append(asked, host+"/"+session)
		return holding && owner == 1 && host == "lap" && session == "sess-a"
	})
	if got := windowStatus(t, r, 1, "lap", "@1"); got != mirror.StatusBusy {
		t.Fatalf("no question held: @1 is %q, want busy (the CLI's own)", got)
	}
	if st := r.svc.AgentState(asUser(1), "lap", "work", "@1"); st.Status != mirror.StatusBusy {
		t.Fatalf("no question held: AgentState %+v", st)
	}
	holding = true
	if got := windowStatus(t, r, 1, "lap", "@1"); got != mirror.StatusWaiting {
		t.Fatalf("a held question: @1 is %q, want waiting", got)
	}
	if got := windowStatus(t, r, 1, "lap", "@2"); got != mirror.StatusIdle {
		t.Fatalf("a held question in sess-a moved @2 (sess-b) to %q", got)
	}
	st := r.svc.AgentState(asUser(1), "lap", "work", "@1")
	if st.Unknown != nil || st.Status != mirror.StatusWaiting || st.Since != t1 {
		t.Fatalf("AgentState with a held question: %+v, want waiting since the CLI's %d", st, t1)
	}
	if hs := r.svc.Hosts(asUser(2)); len(hs) != 0 {
		t.Fatalf("another owner sees %+v", hs)
	}
	if len(asked) == 0 {
		t.Fatal("the overlay was never consulted")
	}
}

// TestHostClaudeSessionIsTheConnectedHostsOwn: the registration check — the
// owner's host, connected, listing that Claude session — and -interactive.
func TestHostClaudeSessionIsTheConnectedHostsOwn(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		host        string
		interactive bool
	}{{"rw", true}, {"ro", false}} {
		tok, _ := r.store.mint(t, 1, c.host)
		if _, err := r.rawConnect(t, tok, winHello(c.host, c.interactive, askWindows()...)); err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, c.host, "work")
	}
	cases := []struct {
		owner             int64
		host, session     string
		listed, writeable bool
	}{
		{1, "rw", "sess-a", true, true},
		{1, "rw", "sess-b", true, true},
		{1, "ro", "sess-a", true, false},
		{1, "rw", "sess-zzz", false, true},
		{2, "rw", "sess-a", false, false},
		{1, "nohost", "sess-a", false, false},
		{1, "rw", "", false, false},
	}
	for _, c := range cases {
		listed, interactive := r.svc.HostClaudeSession(c.owner, c.host, c.session)
		if listed != c.listed || (listed && interactive != c.writeable) {
			t.Errorf("%d/%s/%s: listed %v interactive %v, want %v %v", c.owner, c.host, c.session, listed, interactive, c.listed, c.writeable)
		}
	}
}

// TestAuthenticateHostIsTheConnectDoor: a minted token yields its owner and
// host; a wrong one is refused with 401 and nothing yielded.
func TestAuthenticateHostIsTheConnectDoor(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 7, "lap")
	for _, c := range []struct {
		token string
		ok    bool
	}{{tok, true}, {"mmt_" + "x", false}, {"", false}, {"not-a-token", false}} {
		req := httptest.NewRequest(http.MethodPost, "/mirror/ask", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		owner, host, ok := r.svc.AuthenticateHost(rec, req)
		if ok != c.ok {
			t.Fatalf("token %q: ok %v", c.token, ok)
		}
		if ok && (owner != 7 || host != "lap") {
			t.Fatalf("yielded %d/%s, want 7/lap", owner, host)
		}
		if !ok && rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q refused with %d", c.token, rec.Code)
		}
	}
}
