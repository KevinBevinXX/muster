package api

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE BUG, through the real server against a real Postgres: a session hit its
// usage limit, and the queue sent every message it held into that limit.
//
// Claude Code under a usage limit refuses each message at once with a record
// it writes itself and goes back to its prompt — idle, with a NEWER status
// time, which is all the queue waited for. The fake host here plays exactly
// that: each paste ends its turn at once, and the host reports how (a
// mirror.TurnError: a usage limit, the prompt's only reply).
//
// 🔴 RED ON origin/main, where the host's report does not exist: with the
// TurnError assignment in onPaste dropped and the three sendqueue constants
// written out (what main cannot compile), the queue drains — all three pasted
// in as many passes.
func TestAUsageLimitHoldsTheQueueAndFailsOnlyTheBouncedMessage(t *testing.T) {
	f := newSQFixture(t)
	// ONE dispatcher for the whole run, as RunSendQueue keeps: it remembers
	// which Claude session each message was pasted into.
	d := f.srv.newSendQueueDispatcher()
	tick := func() {
		t.Helper()
		if _, err := d.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	// A fixed clock (the queue's: sendQueueClock), so the held line's time is
	// a literal: a reset on the same UTC day reads "14:00 UTC".
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	f.srv.sendQueueClock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	reset := now.Add(2 * time.Hour)
	limited := true
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(), Observed: now.Add(-time.Hour),
		Session: "sess-1"}
	first := f.enqueue(t, "the first, queued", time.Minute, 0)
	second := f.enqueue(t, "the second, queued", time.Minute, 0)
	third := f.enqueue(t, "the third, queued", time.Minute, 0)
	// onPaste runs under f.mirror.mu after the paste was recorded.
	f.mirror.onPaste = func() {
		prev := f.mirror.agentState.Since
		st := mirror.AgentState{Status: mirror.StatusIdle, Since: prev + 700, Observed: f.srv.sendQueueClock().Add(-time.Hour), Pasted: prev,
			Session: f.mirror.agentState.Session}
		if limited {
			// The host names the refused prompt (the paste just recorded: v2's
			// lock is held by this same paste) and the record before it, the
			// previous turn's end, no later than the status it was pasted on.
			ps := f.mirror.v2.pastes
			st.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit, At: time.Now().UnixMilli(), ResetsAt: reset.UnixMilli(), Rejected: true,
				Prompt: mirror.PromptDigest(ps[len(ps)-1].text), PrevAt: prev, Quick: 1}
		}
		f.mirror.agentState = &st
	}

	for i := 0; i < 6; i++ {
		tick()
	}
	if got := f.pasted(); len(got) != 1 || got[0] != first.Body {
		t.Fatalf("pasted %q into a session under a usage limit; want the first message only", got)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Failed || r.Detail != sendqueue.RejectedLimitDetail || r.MayHaveDelivered {
		t.Fatalf("the message the limit bounced: %s %q (may have been sent: %t); want failed, %q", r.State, r.Detail, r.MayHaveDelivered, sendqueue.RejectedLimitDetail)
	}
	for _, it := range []sendqueue.Item{second, third} {
		if r := f.row(t, it.ID); r.State != sendqueue.Queued {
			t.Fatalf("%q: %s; want still queued", it.Body, r.State)
		}
	}
	// The composer's list: the queue held, saying until when (in the reader's
	// zone, the server's own rendering in UTC); the bounced row with its
	// reason and a plain Retry.
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	for _, want := range []string{`data-queue-hold`, `title="` + sendqueue.LimitHeldText + ` 14:00 UTC"`, `data-clock="` + strconv.FormatInt(reset.UnixMilli(), 10) + `"`,
		`>14:00 UTC</time>`, sendqueue.RejectedLimitDetail, `action="` + ui.SendQueueRetryPath(first.ID) + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the queue's list does not contain %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `action="`+ui.SendQueueRetryPath(first.ID)+`" data-retry="" data-confirm`) {
		t.Fatalf("the bounced row's Retry asks whether it may already have been sent; it was not:\n%s", page)
	}

	// The reset passes (plus the margin), the limit lifted: the rest go, one
	// per turn — and the bounced message is not re-sent on its own.
	limited = false
	mu.Lock()
	now = reset.Add(sendqueue.LimitMargin)
	mu.Unlock()
	for i := 0; i < 6; i++ {
		tick()
	}
	want := []string{first.Body, second.Body, third.Body}
	if got := f.pasted(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after the reset, pasted %q; want %q (the bounced one only by a Retry)", got, want)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Failed {
		t.Fatalf("the bounced message after the reset: %s; want still failed, waiting for a Retry by hand", r.State)
	}
}

// TestTheHostsLimitHoldsAWindowThroughTheProbe: the window's own latest turn
// says nothing, but its host reports the account's limit (Msg.HostLimit): the
// queue holds it, saying until when, and sends once the host stops reporting it.
func TestTheHostsLimitHoldsAWindowThroughTheProbe(t *testing.T) {
	f := newSQFixture(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	f.srv.sendQueueClock = func() time.Time { return now }
	limited := &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(), Observed: now.Add(-time.Hour),
		HostLimit: &mirror.HostLimit{At: 1, ResetsAt: now.Add(time.Hour).UnixMilli()}}
	f.mirror.agentState = limited
	it := f.enqueue(t, "held by the host's limit", time.Minute, 0)
	for i := 0; i < 3; i++ {
		f.tick(t)
	}
	if got := f.pasted(); len(got) != 0 {
		t.Fatalf("pasted %q on a limited host; want nothing", got)
	}
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	if !strings.Contains(page, `title="`+sendqueue.LimitHeldText+` 13:00 UTC"`) {
		t.Fatalf("the list does not say it is held until 13:00 UTC:\n%s", page)
	}
	cleared := *limited
	cleared.HostLimit = nil
	f.mirror.agentState = &cleared
	f.tick(t)
	if got := f.pasted(); len(got) != 1 || got[0] != it.Body {
		t.Fatalf("CONTROL, the limit lifted: pasted %q; want the message", got)
	}
}
