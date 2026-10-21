package plugins

import (
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAnUnstartedReservationIsReleasedAndFreesItsLiveSlot (#152 r2): a
// reservation the caller refused after (no agent recorded) is released — the
// task's next reservation is fresh — and it stops counting as a live start, so
// a full cap with one released frees a slot. One whose agent was recorded is
// not released by it (only the owner's destroy releases a started agent).
func TestAnUnstartedReservationIsReleasedAndFreesItsLiveSlot(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, started = 680, 990011

	if wrote, err := s.ReleaseUnstartedChiefDispatch(session(2), task); err != nil || wrote {
		t.Fatalf("a task with no start: %v %v; want nothing written", wrote, err)
	}
	// A started one is not released.
	if _, err := s.ReserveChiefDispatch(session(2), task+100, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), task+100, started); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseUnstartedChiefDispatch(session(2), task+100); err != nil || wrote {
		t.Fatalf("a start that made an agent: %v %v; want nothing written", wrote, err)
	}
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)

	// Fill the live cap with in-flight starts (no agent recorded yet).
	for id := int64(task); id < task+MaxChiefLiveAgents; id++ {
		if r, err := s.ReserveChiefDispatch(session(2), id, nil, nil); err != nil || r.Prior {
			t.Fatalf("reserve %d: %+v %v", id, r, err)
		}
	}
	if wrote, err := s.ReleaseUnstartedChiefDispatch(session(2), task); err != nil || !wrote {
		t.Fatalf("release the unstarted start: %v %v; want one written", wrote, err)
	}
	if wrote, err := s.ReleaseUnstartedChiefDispatch(session(2), task); err != nil || wrote {
		t.Fatalf("a second release: %v %v; want nothing written", wrote, err)
	}
	// Its slot is free: a start on another task fits under the cap.
	if r, err := s.ReserveChiefDispatch(session(2), task+MaxChiefLiveAgents, nil, nil); err != nil || r.Prior {
		t.Fatalf("a start after one in-flight start was released: %+v %v; want a fresh reservation", r, err)
	}
	// And the released task's own start is fresh again.
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action = 'task.chief_dispatch' AND object <> 'note:680'`)
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || r.Prior {
		t.Fatalf("the released task's next start: %+v %v; want a fresh reservation", r, err)
	}
}
