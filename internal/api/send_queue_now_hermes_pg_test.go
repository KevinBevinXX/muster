package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestSendNowIntoAHermesWindowIsRefusedUnlessItIsIdle: Send now into a HERMES
// window refuses busy (Enter would interrupt the turn), unknown (an approval
// modal takes a pasted Enter as "approve once") and waiting — each 409 with
// its own reason, the row left queued, nothing pasted. Idle passes to the
// delivery. The control is a Claude Code window, which keeps today's rule:
// busy and unknown are sent, only waiting is refused.
func TestSendNowIntoAHermesWindowIsRefusedUnlessItIsIdle(t *testing.T) {
	f := newSQFixture(t)
	cookie := sessionCookieFor(f.srv)
	since := time.Now().Add(-time.Minute).UnixMilli()
	post := func(id int64) (int, string) {
		resp, body := f.retry(t, sendNowPath(id), cookie, true, nil)
		return resp.StatusCode, body
	}
	state := func(st mirror.AgentState) {
		f.mirror.mu.Lock()
		f.mirror.agentState = &st
		f.mirror.mu.Unlock()
	}
	unknown := errors.New("mirror: the host cannot read whether this window's Hermes session is busy")
	it := f.enqueue(t, "for Hermes", time.Minute, 0)
	for _, c := range []struct {
		name string
		st   mirror.AgentState
		want string
	}{
		{"busy", mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusBusy, Since: since}, SendQueueHermesBusyNow},
		{"unknown", mirror.AgentState{Runtime: mirror.RuntimeHermes, Unknown: unknown}, SendQueueHermesUnknownNow},
		{"waiting", mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting, Since: since}, SendQueueHermesWaitingNow},
		// A status this side does not name is not idle either.
		{"an unnamed status", mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: "thinking", Since: since}, SendQueueHermesBusyNow},
	} {
		state(c.st)
		if code, body := post(it.ID); code != http.StatusConflict || body != c.want+"\n" {
			t.Fatalf("Hermes %s: %d %q; want 409 %q", c.name, code, body, c.want)
		}
		if got := f.row(t, it.ID); got.State != sendqueue.Queued || len(f.pasted()) != 0 {
			t.Fatalf("after Hermes %s: %s, pasted %q; want still queued, nothing pasted", c.name, got.State, f.pasted())
		}
	}
	// Idle: past the Send now gate, into the delivery.
	state(mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusIdle, Since: since})
	if code, body := post(it.ID); code != http.StatusNoContent || len(f.pasted()) != 1 {
		t.Fatalf("Hermes idle: %d %q, pasted %q; want it handed to the delivery", code, body, f.pasted())
	}

	// CONTROL: Claude Code keeps today's rule — busy and unknown are sent.
	for _, st := range []mirror.AgentState{
		{Runtime: mirror.RuntimeClaude, Status: mirror.StatusBusy, Since: since},
		{Runtime: mirror.RuntimeClaude, Unknown: mirror.ErrNoStatus},
	} {
		c := f.enqueue(t, "for Claude Code", time.Minute, 0)
		state(st)
		if code, body := post(c.ID); code != http.StatusNoContent {
			t.Fatalf("Claude Code %+v: %d %q; want sent", st, code, body)
		}
	}
	state(mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: mirror.StatusWaiting, Since: since})
	w := f.enqueue(t, "for a waiting Claude Code", time.Minute, 0)
	if code, body := post(w.ID); code != http.StatusConflict || body != SendQueueWaitingNow+"\n" {
		t.Fatalf("Claude Code waiting: %d %q", code, body)
	}
}
