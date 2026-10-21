package plugins

import (
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// The owner's destroy releasing the chief's one start on a task
// (ReleaseChiefDispatch), on a real Postgres. The agent ids here name no agents
// row, so no agent counts as live: only reservations do.

// TestAReleaseFreesOneStartAndOnlyForTheChiefsAgent: a release naming an agent
// the chief's start did not make writes nothing; one naming it writes one row
// and the next reservation is fresh; releasing again before that reservation
// writes nothing (one destroy, one new start), and the new start is spent until
// released in turn.
func TestAReleaseFreesOneStartAndOnlyForTheChiefsAgent(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, chiefs, other = 650, 990001, 990002
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, chiefs); err != nil || wrote {
		t.Fatalf("a release on a task with no start: %v %v; want nothing written", wrote, err)
	}
	if _, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), task, chiefs); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, other); err != nil || wrote {
		t.Fatalf("a release naming another agent: %v %v; want nothing written", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || !r.Prior {
		t.Fatalf("after a release naming another agent: %+v %v; want the start still spent", r, err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, chiefs); err != nil || !wrote {
		t.Fatalf("the release naming the chief's agent: %v %v; want one written", wrote, err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, chiefs); err != nil || wrote {
		t.Fatalf("a second release of the same start: %v %v; want nothing written", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || r.Prior {
		t.Fatalf("the start after the release: %+v %v; want a fresh reservation", r, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || !r.Prior {
		t.Fatalf("a second start after one release: %+v %v; want the new start spent", r, err)
	}
}

// TestAReleasedTasksNewStartCountsAsLiveUntilItsAgentIsRecorded: after a
// release, the task's NEW reservation (its agent not recorded yet) counts
// toward the live cap even though the task already has an older agent row —
// so a full cap of in-flight starts refuses one more.
func TestAReleasedTasksNewStartCountsAsLiveUntilItsAgentIsRecorded(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, chiefs = 660, 990003
	if _, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), task, chiefs); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, chiefs); err != nil || !wrote {
		t.Fatalf("release: %v %v", wrote, err)
	}
	// Fill the cap, the released task's new start first.
	for id := int64(task); id < task+MaxChiefLiveAgents; id++ {
		if r, err := s.ReserveChiefDispatch(session(2), id, nil, nil); err != nil || r.Prior {
			t.Fatalf("reserve %d: %+v %v", id, r, err)
		}
	}
	if _, err := s.ReserveChiefDispatch(session(2), task+MaxChiefLiveAgents, nil, nil); !errors.Is(err, ErrChiefLiveCap) {
		t.Fatalf("one more start with the cap in flight (one on the released task): %v; want ErrChiefLiveCap", err)
	}
}

// TestAPriorStartNamesOnlyItsOwnAgentAfterARelease: after a release, a new
// start that never recorded an agent answers Prior with NO agent — never the
// older, already-destroyed agent of the released start (audit round 2).
func TestAPriorStartNamesOnlyItsOwnAgentAfterARelease(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, destroyed = 670, 990009
	if _, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), task, destroyed); err != nil {
		t.Fatal(err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || !r.Prior || r.AgentID == nil || *r.AgentID != destroyed {
		t.Fatalf("control: before the release the repeat names its agent: %+v %v", r, err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, destroyed); err != nil || !wrote {
		t.Fatalf("release: %v %v", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || r.Prior {
		t.Fatalf("the new start: %+v %v", r, err)
	}
	// That start fails before any agent is recorded; the repeat:
	r, err := s.ReserveChiefDispatch(session(2), task, nil, nil)
	if err != nil || !r.Prior || r.AgentID != nil {
		t.Fatalf("the repeat after an agentless start: %+v (agent %v) %v; want Prior with no agent", r, deref(r.AgentID), err)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestChiefStartedAgentIsTheLatestUnreleasedStartsAgent: what a completed task
// stops (ChiefStartedAgent). None for a task with no start, none for a start
// that recorded no agent, the agent for a recorded start, none once that start
// is released — never the older, released start's agent — and the new start's
// agent once one is recorded. Another owner's start on the same task id is
// never this owner's.
func TestChiefStartedAgentIsTheLatestUnreleasedStartsAgent(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, first, second, theirs = 680, 990011, 990012, 990013
	want := func(label string, wantID int64, wantOK bool) {
		t.Helper()
		id, ok, err := s.ChiefStartedAgent(session(2), task)
		if err != nil || ok != wantOK || id != wantID {
			t.Fatalf("%s: (%d, %v, %v); want (%d, %v, nil)", label, id, ok, err, wantID, wantOK)
		}
	}
	want("no start", 0, false)
	// Another owner's start on the same task id.
	if _, err := s.ReserveChiefDispatch(session(3), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(3), task, theirs); err != nil {
		t.Fatal(err)
	}
	want("another owner's start", 0, false)
	if _, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	want("a start with no agent recorded", 0, false)
	if err := s.RecordChiefDispatchAgent(session(2), task, first); err != nil {
		t.Fatal(err)
	}
	want("the recorded start", first, true)
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, first); err != nil || !wrote {
		t.Fatalf("release: %v %v", wrote, err)
	}
	want("after the release", 0, false)
	if _, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil {
		t.Fatal(err)
	}
	want("the new start before its agent", 0, false)
	if err := s.RecordChiefDispatchAgent(session(2), task, second); err != nil {
		t.Fatal(err)
	}
	want("the new start's agent", second, true)
}
