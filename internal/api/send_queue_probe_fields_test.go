package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestTheProbeCarriesTheTurnErrorHostLimitAndSessionOfAnIdleWindow: the
// queue's probe of an idle window hands the dispatcher what the hold on a
// usage limit decides on — the window's TurnError, its host's HostLimit and
// the Claude session — exactly as the hub reported them. Without a database:
// a merge that drops any one of them from sendQueueProbe fails here, where
// only the Postgres tier would otherwise see it. Pointers are distinct
// values, so a field filled from the wrong source fails too.
func TestTheProbeCarriesTheTurnErrorHostLimitAndSessionOfAnIdleWindow(t *testing.T) {
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	h := sendqueue.Head{Item: sendqueue.Item{Owner: 1, Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}}}
	te := &mirror.TurnError{Kind: "rate_limit", At: 1792000001234}
	hl := &mirror.HostLimit{At: 1792000005678, ResetsAt: 1792003600000}
	for _, rt := range []string{mirror.RuntimeClaude, mirror.RuntimeHermes} {
		fm.mu.Lock()
		fm.agentState = &mirror.AgentState{Runtime: rt, Status: mirror.StatusIdle, Since: 7, TurnError: te, HostLimit: hl, Session: "c-probe"}
		fm.mu.Unlock()
		got := s.sendQueueProbe(asQueueUser(1, time.Now()), h)
		if !got.Idle || got.TurnError != te || got.HostLimit != hl || got.Session != "c-probe" {
			t.Errorf("%s: the probe of an idle window = %+v; want idle with TurnError %+v, HostLimit %+v, Session c-probe", rt, got, te, hl)
		}
	}
	// Control: a window that is not idle carries none, and says why.
	fm.mu.Lock()
	fm.agentState = &mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: mirror.StatusBusy, Since: 7, TurnError: te, HostLimit: hl, Session: "c-probe"}
	fm.mu.Unlock()
	if got := s.sendQueueProbe(asQueueUser(1, time.Now()), h); got.Idle || got.Why != "Claude Code is working." {
		t.Errorf("control, a busy window: %+v", got)
	}
}
