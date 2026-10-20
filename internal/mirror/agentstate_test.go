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
// argument, written at since (Unix ms, the CLI's statusUpdatedAt), @2 a shell.
func statusWindows(status string, since int64) []mirror.Window {
	return []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, Status: status, StatusSince: since},
		{Session: "work", ID: "@2", Index: 1, Command: "zsh"},
	}
}

// t1, t2, t3 are pairwise-distinct CLI timestamps (Unix ms), none of them now.
const (
	t1 = int64(1792000001111)
	t2 = int64(1792000002222)
	t3 = int64(1792000003333)
)

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
		m := winHello(c.host, true, statusWindows(mirror.StatusIdle, t1)...)
		m.StatusReport = c.report
		if _, err := r.rawConnect(t, tok, m); err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, c.host, "work")
		st := r.svc.AgentState(asUser(1), c.host, "work", "@1")
		if c.want == nil {
			if st.Unknown != nil || st.Status != mirror.StatusIdle || st.Since != t1 {
				t.Fatalf("host %s (advertised): %+v, want idle since the CLI's %d", c.host, st, t1)
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

// TestTheAgentStateIsTheCLIsOwnStatusAndTime: the state is the host's latest
// report for that window, and Since is EXACTLY the time Claude Code wrote the
// status (Window.StatusSince) — not when muster saw it. So a turn that began
// and ended between two reports (idle, then idle again with a newer time)
// moves Since; a report that changes something else (activity) and repeats the
// time does not. A status with no time is unknown (ErrNoStatusSince), never
// idle since a guess. Plus each other unknown: a shell window, a Claude window
// whose status the host cannot read, another owner's host, no principal.
func TestTheAgentStateIsTheCLIsOwnStatusAndTime(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, statusWindows(mirror.StatusIdle, t1)...)
	m.StatusReport = true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	at := func(window string) mirror.AgentState { return r.svc.AgentState(asUser(1), "h", "work", window) }
	if st := at("@1"); st.Status != mirror.StatusIdle || st.Since != t1 {
		t.Fatalf("first = %+v, want idle since %d", st, t1)
	}
	// The session address resolves to its active window, @1.
	if st := at(""); st.Status != mirror.StatusIdle || st.Since != t1 {
		t.Fatalf("the session's address = %+v, want @1's state", st)
	}
	send := func(status string, since, activity int64) {
		t.Helper()
		ws := statusWindows(status, since)
		ws[0].Activity = activity
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: ws})
	}
	waitSince := func(what, status string, since int64) {
		t.Helper()
		eventually(t, what, func() bool {
			st := at("@1")
			return st.Unknown == nil && st.Status == status && st.Since == since
		})
	}
	send(mirror.StatusBusy, t2, 0)
	waitSince("busy since t2", mirror.StatusBusy, t2)
	// Idle again, a turn later: the CLI's time, not muster's clock.
	send(mirror.StatusIdle, t3, 0)
	waitSince("idle since t3", mirror.StatusIdle, t3)
	// A change that is not the status, with the same time: Since stays.
	send(mirror.StatusIdle, t3, time.Now().Unix())
	time.Sleep(50 * time.Millisecond)
	if st := at("@1"); st.Since != t3 {
		t.Fatalf("an unchanged status and time moved Since to %v", st.Since)
	}
	// A turn too short for any report to see busy: idle both times, the time
	// newer. Since follows it.
	send(mirror.StatusIdle, t3+1, 0)
	waitSince("idle again since t3+1", mirror.StatusIdle, t3+1)

	// 🔴 Claude Code's "shell" — idle, a background shell still running — is
	// IDLE, since the time it was written: the prompt is free, and the time is
	// the turn's end, which the queue's "newer than the paste" rule waits for.
	send("shell", t3+2, 0)
	waitSince("shell is idle since t3+2", mirror.StatusIdle, t3+2)
	// A status without its time.
	send(mirror.StatusIdle, 0, 0)
	eventually(t, "a status with no time is unknown", func() bool { return errors.Is(at("@1").Unknown, mirror.ErrNoStatusSince) })
	if st := at("@1"); st.Status != "" || st.Since != 0 {
		t.Fatalf("a status with no time = %+v, want unknown", st)
	}
	if st := at("@2"); !errors.Is(st.Unknown, mirror.ErrNotClaude) {
		t.Fatalf("a shell window = %+v, want ErrNotClaude", st)
	}
	send("", t1, 0)
	eventually(t, "the status went unreadable", func() bool { return errors.Is(at("@1").Unknown, mirror.ErrNoStatus) })
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
// advertise it and carry each Claude window's status AND its time, and a busy
// flip reaches AgentState on the next refresh, with the CLI's time. A backend that does not report status (the
// same windows, no StatusReporter) advertises nothing, and is unknown.
func TestARealHostReportsItsClaudeStatus(t *testing.T) {
	r := newRig(t)
	claude := func(status string, since int64) []mirrorhost.WindowInfo {
		return []mirrorhost.WindowInfo{{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1,
			Claude: &mirrorhost.ClaudePane{Pane: "%1", Gen: "g", Status: status, StatusSince: since}}}
	}
	b := &winBackend{}
	b.set(claude(mirror.StatusBusy, t1)...)
	tok, _ := r.store.mint(t, 1, "rep")
	r.connect(t, "rep", tok, statusBackend{b}, true)
	r.online(t, 1, "rep", "work")
	is := func(status string, since int64) func() bool {
		return func() bool {
			st := r.svc.AgentState(asUser(1), "rep", "work", "@1")
			return st.Status == status && st.Since == since
		}
	}
	eventually(t, "busy since t1", is(mirror.StatusBusy, t1))
	b.set(claude(mirror.StatusIdle, t2)...)
	eventually(t, "idle since t2", is(mirror.StatusIdle, t2))
	if h := r.svc.Hosts(asUser(1))[0]; !h.StatusReport {
		t.Fatalf("the host did not advertise StatusReport: %+v", h)
	}

	quiet := &winBackend{}
	quiet.set(claude(mirror.StatusIdle, t2)...)
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
	m := winHello("h", true, statusWindows(mirror.StatusIdle, t1)...)
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

// TestABusyFlipDoesNotNudgeTheAgentsView: Status and its time move with every
// turn and no card draws them, so a sessions message that changes ONLY those
// re-renders no open Agents view (sameWindows ignores them) — while it still reaches
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
	m := winHello("h", true, statusWindows(mirror.StatusIdle, t1)...)
	m.StatusReport = true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	time.Sleep(100 * time.Millisecond)
	base := nudges.Load()
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: statusWindows(mirror.StatusBusy, t2)})
	eventually(t, "busy", func() bool { return r.svc.AgentState(asUser(1), "h", "work", "@1").Status == mirror.StatusBusy })
	time.Sleep(100 * time.Millisecond)
	if got := nudges.Load(); got != base {
		t.Fatalf("a status-only change nudged the Agents view %d time(s)", got-base)
	}
	labelled := statusWindows(mirror.StatusBusy, t2)
	labelled[0].Label = "Gold:0"
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: labelled})
	eventually(t, "CONTROL: a visible change nudges", func() bool { return nudges.Load() > base })
}

// TestAStatusIsKeptOnlyForAClaudeWindowAndOnlyAsOneOfThree: the normalizer
// both halves apply keeps busy / idle / waiting on a Claude window, reads
// Claude Code's "shell" (idle, a background shell running) as idle, blanks any
// other value, and blanks a status on a window that is not Claude Code — and a
// status's time goes with it, and is never negative.
func TestAStatusIsKeptOnlyForAClaudeWindowAndOnlyAsOneOfThree(t *testing.T) {
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Claude: true, Status: mirror.StatusWaiting, StatusSince: t1},
		{Session: "s", ID: "@2", Claude: true, Status: "compacting", StatusSince: t1},
		{Session: "s", ID: "@3", Status: mirror.StatusIdle, StatusSince: t1},
		{Session: "s", ID: "@4", Claude: true, Status: mirror.StatusIdle, StatusSince: -1},
		{Session: "s", ID: "@5", Claude: true, Status: "shell", StatusSince: t2},
	}, 0)
	type st struct {
		status string
		since  int64
	}
	want := map[string]st{"@1": {mirror.StatusWaiting, t1}, "@2": {}, "@3": {}, "@4": {mirror.StatusIdle, 0},
		"@5": {mirror.StatusIdle, t2}}
	for _, w := range got {
		if (st{w.Status, w.StatusSince}) != want[w.ID] {
			t.Errorf("window %s: Status %q since %d, want %+v", w.ID, w.Status, w.StatusSince, want[w.ID])
		}
	}
}
