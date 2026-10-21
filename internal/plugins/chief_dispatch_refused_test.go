package plugins

import (
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestARefusedReleaseFreesOnlyTheReservationItNames (#209 audit 🟡3/🟢7): a
// release after a refused create names its reservation (ReservationID). A
// stale id — an older reservation, since released and followed by a new one —
// writes nothing, so the NEW start stays spent; the new start's own id
// releases it, once; a start that recorded an agent is never released by it.
func TestARefusedReleaseFreesOnlyTheReservationItNames(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, agent = 690, 990021

	first, err := s.ReserveChiefDispatch(session(2), task, nil, nil)
	if err != nil || first.Prior || first.ReservationID <= 0 {
		t.Fatalf("first reservation: %+v %v", first, err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), task, agent); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), task, first.ReservationID); err != nil || wrote {
		t.Fatalf("a start that recorded an agent: %v %v; want nothing written", wrote, err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, agent); err != nil || !wrote {
		t.Fatalf("the owner's destroy: %v %v", wrote, err)
	}
	second, err := s.ReserveChiefDispatch(session(2), task, nil, nil)
	if err != nil || second.Prior || second.ReservationID <= first.ReservationID {
		t.Fatalf("second reservation: %+v %v", second, err)
	}
	if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), task, first.ReservationID); err != nil || wrote {
		t.Fatalf("a release naming the OLD reservation: %v %v; want nothing written", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || !r.Prior {
		t.Fatalf("after a stale release: %+v %v; want the second start still spent", r, err)
	}
	if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), task, second.ReservationID); err != nil || !wrote {
		t.Fatalf("the second reservation's own release: %v %v; want one written", wrote, err)
	}
	if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), task, second.ReservationID); err != nil || wrote {
		t.Fatalf("a repeat: %v %v; want nothing written", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || r.Prior {
		t.Fatalf("after the release: %+v %v; want a fresh reservation", r, err)
	}
}

// TestRefusedAndReleasedStartsDoNotHoldLiveSlots (#209 round-2 🟡1): a start
// released after its create was refused made no agent, so it must stop
// counting toward MaxChiefLiveAgents at once — not hold a slot for the
// pending-agent window. The audit's probe: MaxChiefLiveAgents refused cycles
// on as many tasks, then another task still reserves.
func TestRefusedAndReleasedStartsDoNotHoldLiveSlots(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task = 700
	for i := int64(0); i < MaxChiefLiveAgents; i++ {
		r, err := s.ReserveChiefDispatch(session(2), task+i, nil, nil)
		if err != nil || r.Prior {
			t.Fatalf("cycle %d: reserve: %+v %v", i, r, err)
		}
		if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), task+i, r.ReservationID); err != nil || !wrote {
			t.Fatalf("cycle %d: release: %v %v", i, wrote, err)
		}
	}
	if r, err := s.ReserveChiefDispatch(session(2), task+MaxChiefLiveAgents, nil, nil); err != nil || r.Prior {
		t.Fatalf("a start after %d refused-and-released ones (no live agent): %+v %v", MaxChiefLiveAgents, r, err)
	}
}
