package agentkickoff

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// refusingGateway answers the first n sends with refusal, then delegates.
type refusingGateway struct {
	*fakeGateway
	mu      sync.Mutex
	n       int
	refusal *agents.RuntimeError
}

func (g *refusingGateway) Send(ctx context.Context, t agentgateway.Target, key, msg string, emit func(string)) (string, error) {
	g.mu.Lock()
	left := g.n
	if g.n > 0 {
		g.n--
	}
	g.mu.Unlock()
	reply, err := g.fakeGateway.Send(ctx, t, key, msg, emit)
	if left > 0 {
		r := *g.refusal
		return "", &r
	}
	return reply, err
}

func refusingHarness(t *testing.T, n int, refusal *agents.RuntimeError) *harness {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	h.d.gw = &refusingGateway{fakeGateway: h.gw, n: n, refusal: refusal}
	h.d.notReadyWait, h.d.notReadyPoll = 50*time.Millisecond, 5*time.Millisecond
	return h
}

// ccd's typed `409 busy` — code awaiting_approval (a dialog on its pane) or
// plain (another turn holds the session) — is given BEFORE it types anything,
// as `503 not_ready` is. So a kickoff refused that way:
//   - is re-sent within the same delivery, and lands once the refusal stops
//     (one stamp, one delivery);
//   - refused for the whole delivery, is recorded as NOT ACCEPTED, resend-safe,
//     never as "failed after it was handed to the gateway" (possibly sent).
//
// An untyped 409 proves nothing and is not re-sent (the control).
func TestABusyRefusedKickoffIsResentAndNeverRecordedAsPossiblySent(t *testing.T) {
	for _, refusal := range []*agents.RuntimeError{
		{Status: 409, Type: "busy", Code: "awaiting_approval", Message: "the CLI's pane shows a dialog"},
		{Status: 409, Type: "busy", Message: "another turn is already being driven"},
	} {
		name := refusal.Type + "/" + refusal.Code
		h := refusingHarness(t, 2, refusal)
		log := h.tick(t)
		if h.gw.callCount() != 3 {
			t.Fatalf("%s: sends = %d, want 3 (two refusals, then the turn).%s", name, h.gw.callCount(), transcript(log))
		}
		if n := countPrefix(log, "SetKickedOff(7301,true)"); n != 1 {
			t.Fatalf("%s: stamped %d time(s), want 1.%s", name, n, transcript(log))
		}
		if r := h.store.row(7301); !r.KickedOff || agents.KickoffFailed(r) {
			t.Fatalf("%s: kicked_off=%t failed=%t (%q), want delivered", name, r.KickedOff, agents.KickoffFailed(r), r.KickoffError)
		}

		h = refusingHarness(t, 1<<30, refusal)
		h.tick(t)
		if h.gw.callCount() < 2 {
			t.Fatalf("%s: the refusal was not re-sent (%d send)", name, h.gw.callCount())
		}
		r := h.store.row(7301)
		if !agents.KickoffFailed(r) || !agents.KickoffResendSafe(r) || !strings.HasPrefix(r.KickoffError, agents.KickoffBusyReason) {
			t.Fatalf("%s: a lasting refusal recorded %q (resend-safe=%t); want %q, resend-safe",
				name, r.KickoffError, agents.KickoffResendSafe(r), agents.KickoffBusyReason)
		}
	}

	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	h.d.notReadyWait, h.d.notReadyPoll = 50*time.Millisecond, 5*time.Millisecond
	h.gw.err = &agents.RuntimeError{Status: 409}
	h.tick(t)
	if r := h.store.row(7301); h.gw.callCount() != 1 || agents.KickoffResendSafe(r) {
		t.Fatalf("control: an untyped 409 was re-sent (%d sends) or recorded resend-safe (%q)", h.gw.callCount(), r.KickoffError)
	}
}
