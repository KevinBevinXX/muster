package mirror_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE AGENT STATE a queued message waits on (agentstate.go): what a host says
// of each window's Claude Code session, and since when muster has seen it.

// statusWindows is one host's windows: @1 a Claude pane whose status is the
// argument, @2 a shell.
func statusWindows(status string) []mirror.Window {
	return []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, Status: status},
		{Session: "work", ID: "@2", Index: 1, Command: "zsh"},
	}
}

// TestAHostThatDoesNotAdvertiseStatusIsNeverIdle is the FAIL-CLOSED half: a
// host whose hello does not carry StatusReport — every muster-mirror before
// the send queue — is UNKNOWN for every window, even when its windows say
// "idle" (a field it is not believed about), and the reason names the remedy.
// A per-session host is unknown too, StatusReport or not. The positive control
// is the same window list from a host that DID advertise it.
func TestAHostThatDoesNotAdvertiseStatusIsNeverIdle(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		host   string
		report bool
		want   error
	}{
		{"old", false, mirror.ErrNoStatusReport},
		{"new", true, nil},
	} {
		tok, _ := r.store.mint(t, 1, c.host)
		m := winHello(c.host, true, statusWindows(mirror.StatusIdle)...)
		m.StatusReport = c.report
		if _, err := r.rawConnect(t, tok, m); err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, c.host, "work")
		st := r.svc.AgentState(asUser(1), c.host, "work", "@1")
		if c.want == nil {
			if st.Unknown != nil || st.Status != mirror.StatusIdle || st.Since.IsZero() {
				t.Fatalf("host %s (advertised): %+v, want idle since a time", c.host, st)
			}
			continue
		}
		if !errors.Is(st.Unknown, c.want) || st.Status != "" {
			t.Fatalf("host %s (not advertised): %+v, want unknown with %v", c.host, st, c.want)
		}
	}
	// A per-session host has no window to report a status of.
	tok, _ := r.store.mint(t, 1, "persession")
	m := hello("persession", true, "work")
	m.Claude, m.StatusReport = []string{"work"}, true
	if _, err := r.rawConnect(t, tok, m); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "persession", "work")
	if st := r.svc.AgentState(asUser(1), "persession", "work", ""); !errors.Is(st.Unknown, mirror.ErrNoStatusReport) {
		t.Fatalf("a per-session host: %+v, want ErrNoStatusReport", st)
	}
}

// TestTheAgentStateFollowsTheHostAndKeepsItsSince: the state is the host's
// latest report for that window, and Since moves ONLY when the status changes —
// a sessions message that changes something else (activity) leaves it, which
// is what makes "idle for 30 s" mean thirty continuous seconds. Plus each
// unknown case: a shell window, a Claude window whose status the host cannot
// read, another owner's host.
func TestTheAgentStateFollowsTheHostAndKeepsItsSince(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle)...)
	m.StatusReport = true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	first := r.svc.AgentState(asUser(1), "h", "work", "@1")
	if first.Status != mirror.StatusIdle || first.Since.IsZero() {
		t.Fatalf("first = %+v, want idle", first)
	}
	// The session address resolves to its active window, @1.
	if st := r.svc.AgentState(asUser(1), "h", "work", ""); st.Status != mirror.StatusIdle || !st.Since.Equal(first.Since) {
		t.Fatalf("the session's address = %+v, want @1's state", st)
	}

	send := func(status string, activity int64) {
		t.Helper()
		ws := statusWindows(status)
		ws[0].Activity = activity
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: ws})
	}
	time.Sleep(5 * time.Millisecond)
	send(mirror.StatusIdle, time.Now().Unix()) // a change that is not the status
	time.Sleep(50 * time.Millisecond)
	if st := r.svc.AgentState(asUser(1), "h", "work", "@1"); !st.Since.Equal(first.Since) {
		t.Fatalf("an unchanged status moved Since: %v → %v", first.Since, st.Since)
	}
	send(mirror.StatusBusy, 0)
	eventually(t, "busy", func() bool { return r.svc.AgentState(asUser(1), "h", "work", "@1").Status == mirror.StatusBusy })
	busy := r.svc.AgentState(asUser(1), "h", "work", "@1")
	if !busy.Since.After(first.Since) {
		t.Fatalf("busy since %v, not after idle since %v", busy.Since, first.Since)
	}
	time.Sleep(5 * time.Millisecond)
	send(mirror.StatusWaiting, 0)
	eventually(t, "waiting", func() bool { return r.svc.AgentState(asUser(1), "h", "work", "@1").Status == mirror.StatusWaiting })
	time.Sleep(5 * time.Millisecond)
	send(mirror.StatusIdle, 0)
	eventually(t, "idle again", func() bool { return r.svc.AgentState(asUser(1), "h", "work", "@1").Status == mirror.StatusIdle })
	if again := r.svc.AgentState(asUser(1), "h", "work", "@1"); !again.Since.After(busy.Since) {
		t.Fatalf("idle again since %v, not after busy since %v", again.Since, busy.Since)
	}

	if st := r.svc.AgentState(asUser(1), "h", "work", "@2"); !errors.Is(st.Unknown, mirror.ErrNotClaude) {
		t.Fatalf("a shell window = %+v, want ErrNotClaude", st)
	}
	send("", 0)
	eventually(t, "the status went unreadable", func() bool {
		return errors.Is(r.svc.AgentState(asUser(1), "h", "work", "@1").Unknown, mirror.ErrNoStatus)
	})
	if st := r.svc.AgentState(asUser(2), "h", "work", "@1"); !errors.Is(st.Unknown, mirror.ErrHostOffline) {
		t.Fatalf("another owner = %+v, want ErrHostOffline", st)
	}
	if st := r.svc.AgentState(context.Background(), "h", "work", "@1"); st.Unknown == nil {
		t.Fatalf("no principal = %+v, want unknown", st)
	}
}

// statusBackend is winBackend that reports status (mirrorhost.StatusReporter).
type statusBackend struct{ *winBackend }

func (statusBackend) ReportsStatus() bool { return true }

// TestARealHostReportsItsClaudeStatus is the wire end to end with the REAL
// muster-mirror client: a backend that reports status makes the client
// advertise it and carry each Claude window's status, and a busy flip reaches
// AgentState on the next refresh. A backend that does not report status (the
// same windows, no StatusReporter) advertises nothing, and is unknown.
func TestARealHostReportsItsClaudeStatus(t *testing.T) {
	r := newRig(t)
	claude := func(status string) []mirrorhost.WindowInfo {
		return []mirrorhost.WindowInfo{{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1,
			Claude: &mirrorhost.ClaudePane{Pane: "%1", Gen: "g", Status: status}}}
	}
	b := &winBackend{}
	b.set(claude(mirror.StatusBusy)...)
	tok, _ := r.store.mint(t, 1, "rep")
	r.connect(t, "rep", tok, statusBackend{b}, true)
	r.online(t, 1, "rep", "work")
	eventually(t, "busy", func() bool { return r.svc.AgentState(asUser(1), "rep", "work", "@1").Status == mirror.StatusBusy })
	b.set(claude(mirror.StatusIdle)...)
	eventually(t, "idle", func() bool { return r.svc.AgentState(asUser(1), "rep", "work", "@1").Status == mirror.StatusIdle })
	if h := r.svc.Hosts(asUser(1))[0]; !h.StatusReport {
		t.Fatalf("the host did not advertise StatusReport: %+v", h)
	}

	quiet := &winBackend{}
	quiet.set(claude(mirror.StatusIdle)...)
	tok2, _ := r.store.mint(t, 1, "quiet")
	r.connect(t, "quiet", tok2, quiet, true)
	r.online(t, 1, "quiet", "work")
	if st := r.svc.AgentState(asUser(1), "quiet", "work", "@1"); !errors.Is(st.Unknown, mirror.ErrNoStatusReport) {
		t.Fatalf("a backend that does not report status: %+v, want ErrNoStatusReport", st)
	}
}

// TestAPasteTheHostNeverConfirmedIsMarkedUnconfirmed: a paste handed to the
// host and never answered may have landed (ErrPasteUnconfirmed — the send
// queue then waits for a turn); a paste the host refused, and one refused
// before it reached the host, did not and are not so marked.
func TestAPasteTheHostNeverConfirmedIsMarkedUnconfirmed(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle)...)
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	// Unanswered: the caller gives up first.
	ctx, cancel := context.WithTimeout(asUser(1), 100*time.Millisecond)
	defer cancel()
	err = r.svc.PasteWindow(ctx, "h", "work", "@1", "hello", mirror.PasteClaude)
	if !errors.Is(err, mirror.ErrPasteUnconfirmed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unanswered paste = %v, want ErrPasteUnconfirmed wrapping the deadline", err)
	}
	// Refused by the host.
	go func() {
		eventually(t, "the second paste", func() bool { return len(raw.of(mirror.TypePaste)) == 2 })
		raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[1].ID, Error: "no"})
	}()
	err = r.svc.PasteWindow(asUser(1), "h", "work", "@1", "hello", mirror.PasteClaude)
	if !errors.Is(err, mirror.ErrPasteRefused) || errors.Is(err, mirror.ErrPasteUnconfirmed) {
		t.Fatalf("a refused paste = %v, want ErrPasteRefused and NOT unconfirmed", err)
	}
	// Refused before it reached the host.
	err = r.svc.PasteWindow(asUser(1), "h", "work", "@2", "hello", mirror.PasteClaude)
	if !errors.Is(err, mirror.ErrNotClaude) || errors.Is(err, mirror.ErrPasteUnconfirmed) {
		t.Fatalf("a paste refused by muster = %v, want ErrNotClaude and NOT unconfirmed", err)
	}
}

// TestABusyFlipDoesNotNudgeTheAgentsView: Status flips with every turn and no
// card draws it, so a sessions message that changes ONLY a status re-renders
// no open Agents view (sameWindows ignores it) — while it still reaches
// AgentState. The positive control is a visible change (a label) on the same
// connection, which does nudge.
func TestABusyFlipDoesNotNudgeTheAgentsView(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	var nudges atomic.Int64
	r.svc.SetNotify(func(owner int64) {
		if owner == 1 {
			nudges.Add(1)
		}
	})
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle)...)
	m.StatusReport = true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	time.Sleep(100 * time.Millisecond)
	base := nudges.Load()
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: statusWindows(mirror.StatusBusy)})
	eventually(t, "busy", func() bool { return r.svc.AgentState(asUser(1), "h", "work", "@1").Status == mirror.StatusBusy })
	time.Sleep(100 * time.Millisecond)
	if got := nudges.Load(); got != base {
		t.Fatalf("a status-only change nudged the Agents view %d time(s)", got-base)
	}
	labelled := statusWindows(mirror.StatusBusy)
	labelled[0].Label = "Gold:0"
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: labelled})
	eventually(t, "CONTROL: a visible change nudges", func() bool { return nudges.Load() > base })
}

// TestAStatusIsKeptOnlyForAClaudeWindowAndOnlyAsOneOfThree: the normalizer
// both halves apply keeps busy / idle / waiting on a Claude window, blanks any
// other value, and blanks a status on a window that is not Claude Code.
func TestAStatusIsKeptOnlyForAClaudeWindowAndOnlyAsOneOfThree(t *testing.T) {
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Claude: true, Status: mirror.StatusWaiting},
		{Session: "s", ID: "@2", Claude: true, Status: "compacting"},
		{Session: "s", ID: "@3", Status: mirror.StatusIdle},
	}, 0)
	want := map[string]string{"@1": mirror.StatusWaiting, "@2": "", "@3": ""}
	for _, w := range got {
		if w.Status != want[w.ID] {
			t.Errorf("window %s: Status %q, want %q", w.ID, w.Status, want[w.ID])
		}
	}
}
