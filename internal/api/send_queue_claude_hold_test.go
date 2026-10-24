package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestAWindowTheClaudeGuardHoldsIsNotSentToAndSaysWhy: while the hub holds a
// window after the host's prompt guard refused a paste into it
// (AgentState.GuardHeld) — though its status says idle, long enough — the
// queue's probe is not idle, and its reason is the hold's words; the decision
// sends nothing. The control: the same state without the hold sends.
func TestAWindowTheClaudeGuardHoldsIsNotSentToAndSaysWhy(t *testing.T) {
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	h := sendqueue.Head{Item: sendqueue.Item{Owner: 1, Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}}}
	user1 := asQueueUser(1, time.Now())
	idle := mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: mirror.StatusIdle,
		Since: time.Now().Add(-time.Hour).UnixMilli(), Observed: time.Now().Add(-time.Hour)}
	held := idle
	held.GuardHeld = "Held: muster-mirror refused it at the moment of typing — Claude's input box already has text — clear or send it first."
	for _, c := range []struct {
		st   mirror.AgentState
		send bool
	}{{held, false}, {idle, true}} {
		fm.mu.Lock()
		st := c.st
		fm.agentState = &st
		fm.mu.Unlock()
		p := s.sendQueueProbe(user1, h)
		d := s.sendQueueDecide(h, p)
		if d.Send != c.send {
			t.Fatalf("held=%t: decided %+v; want send=%t", c.st.GuardHeld != "", d, c.send)
		}
		if !c.send && !strings.Contains(d.Why, "input box already has text") {
			t.Fatalf("held: the queue says %q; want the hold's reason", d.Why)
		}
	}
}
