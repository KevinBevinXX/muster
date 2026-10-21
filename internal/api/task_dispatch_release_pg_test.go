package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// flippingProvisioner is recordingProvisioner whose deployment stops taking
// kickoffs once undeliverableNow says so — here, once a reservation exists:
// exactly the window between task_dispatch's reservation and its call.
type flippingProvisioner struct {
	*recordingProvisioner
	undeliverableNow func() bool
}

func (p *flippingProvisioner) KickoffUndeliverableReason() string {
	if p.undeliverableNow != nil && p.undeliverableNow() {
		return "the fixture deployment stopped taking kickoffs"
	}
	return ""
}

// TestADeploymentRefusalAfterTheReservationReleasesTheStart (#152 r2): the
// deployment's re-check AFTER the reservation refuses (it stopped taking
// kickoffs while the reservation ran). Nothing was created, so the task's one
// start is released: the next call, once the deployment takes kickoffs again,
// STARTS an agent instead of answering already_dispatched with none.
func TestADeploymentRefusalAfterTheReservationReleasesTheStart(t *testing.T) {
	var fp *flippingProvisioner
	f := newTaskRouteFixture(t, true, true, func(ext *Extensions) {
		fp = &flippingProvisioner{recordingProvisioner: &recordingProvisioner{}}
		ext.Provisioner = fp
	})
	f.prov = fp.recordingProvisioner
	n := f.task(t, "flip after reserve")
	fp.undeliverableNow = func() bool { return f.reservations(t, n.ID) > 0 }

	text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if !strings.Contains(text, `"error"`) || !strings.Contains(text, "stopped taking kickoffs") {
		t.Fatalf("the refused start: %s", text)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 0 {
		t.Fatalf("a refused start created %d agent(s)", len(got))
	}

	// The deployment takes kickoffs again.
	fp.undeliverableNow = nil
	text = f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if strings.Contains(text, `"error"`) || strings.Contains(text, "already_dispatched") || !strings.Contains(text, `"agent_id"`) {
		t.Fatalf("the call after the refusal: %s; want a started agent", text)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 1 {
		t.Fatalf("agents on the task after the retry: %d, want 1", len(got))
	}
	// The released start does not free a SECOND one: the next call is the
	// already_dispatched answer for the agent just started.
	if again := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}); !strings.Contains(again, `"already_dispatched":true`) {
		t.Fatalf("a third call: %s", again)
	}
}

// TestAForwardToARunningAgentWhoseFirstTurnIsOwedRidesThatTurn (quiet-stoat):
// a claude-code agent is RUNNING while its first turn is held (its repository
// is still being cloned; the turn is unstamped). A forward sent then as a turn
// of its own reached a runtime that answered not_ready, and nothing re-sent
// it. It is appended to the owed first turn instead — no turn of its own —
// and once that turn is stamped, a forward is a turn again.
func TestAForwardToARunningAgentWhoseFirstTurnIsOwedRidesThatTurn(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	n := f.task(t, "held first turn")
	store := agents.NewPG(f.pool)
	const first = "Work the held task."
	a, err := store.Create(f.asOwner, agents.Agent{Name: "trheld-" + strconv.FormatInt(f.owner, 10),
		Namespace: "ns-trheld", Status: agents.StatusRunning, NoteID: &n.ID, PendingNote: first})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var turns []string
	f.srv.taskForwardTurn = func(_ context.Context, x agents.Agent, _ agents.ChatSession, text string) {
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, text)
		f.srv.turns.leave(x.ID)
	}
	turnCount := func() int { mu.Lock(); defer mu.Unlock(); return len(turns) }

	text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "Scope changed upstream"})
	if strings.Contains(text, `"error"`) || !strings.Contains(text, `"turn_started":false`) {
		t.Fatalf("the forward: %s", text)
	}
	cur, err := store.Get(f.asOwner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cur.PendingNote, first) || !strings.Contains(cur.PendingNote, "Scope changed upstream") || turnCount() != 0 {
		t.Fatalf("first turn %q, %d turn(s); want the forward appended to the held first turn and no turn", cur.PendingNote, turnCount())
	}

	// Stamped: the next forward is a turn.
	if _, err := f.pool.Exec(f.asOwner, `UPDATE agents SET kicked_off = true WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "And again"}); !strings.Contains(text, `"turn_started":true`) {
		t.Fatalf("a forward after the stamp: %s", text)
	}
	if turnCount() != 1 {
		t.Fatalf("turns after the stamp: %d, want 1", turnCount())
	}
}
