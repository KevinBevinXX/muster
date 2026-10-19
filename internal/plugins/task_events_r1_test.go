package plugins

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Audit round 1: task events have their OWN allowance, separate from the
// plugin's chief-mail caps, and the dispatch ledger and the "Task to route"
// read are filtered to the caller's own rows in SQL, not only by row-level
// security.

// fillMessages inserts n rows for plugin p of owner 2 directly (task events
// when task > 0), created `age` ago ('0' for now).
func fillMessages(t *testing.T, dsn string, p Plugin, task int64, n int, prefix, age string) {
	t.Helper()
	var taskArg any
	if task > 0 {
		taskArg = task
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, created_at, task_id)
		SELECT 2, $1, $2 || g, 'b', now() - $4::interval, $5 FROM generate_series(1, $3) g`, p.ID, prefix, n, age, taskArg)
}

func taskEventIn(t *testing.T, task int64, d string) MessageIn {
	t.Helper()
	in, err := NewTaskEventIn(TaskEvent{Event: "assigned", TaskID: task, Title: "t", Change: d}, d)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// TestTaskEventsHaveTheirOwnHourlyAllowance, both directions: a full hour of
// chief mail does not refuse a task event, and a full hour of task events does
// not refuse chief mail — each bucket refuses only its own next one. The fills
// overshoot the OTHER bucket's cap, so a bucket that counted every row would
// refuse.
func TestTaskEventsHaveTheirOwnHourlyAllowance(t *testing.T) {
	t.Run("chief mail does not spend the task-event allowance", func(t *testing.T) {
		s, pool, dsn := pgStore(t)
		_, p := mustCreate(t, s, 2, "example-tracker")
		task := pgTask(t, pool, 2)
		fillMessages(t, dsn, p, 0, MaxTaskEventsPerHour, "mail-", "0")
		if _, _, err := s.Submit(plugin(2), p.ID, msg("mail-over")); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("chief mail past its hour: %v; want ErrRateLimited (control)", err)
		}
		if _, created, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e1")); err != nil || !created {
			t.Fatalf("a task event after a full hour of chief mail: created=%v %v; want stored", created, err)
		}
	})
	t.Run("task events do not spend the chief-mail allowance", func(t *testing.T) {
		s, pool, dsn := pgStore(t)
		_, p := mustCreate(t, s, 2, "example-tracker")
		task := pgTask(t, pool, 2)
		fillMessages(t, dsn, p, task, MaxTaskEventsPerHour-1, "ev-", "0")
		if _, created, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "last")); err != nil || !created {
			t.Fatalf("task event %d of %d: created=%v %v", MaxTaskEventsPerHour, MaxTaskEventsPerHour, created, err)
		}
		if _, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "over")); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("task event %d in an hour: %v; want ErrRateLimited", MaxTaskEventsPerHour+1, err)
		}
		if _, created, err := s.Submit(plugin(2), p.ID, msg("mail-1")); err != nil || !created {
			t.Fatalf("chief mail after a full hour of task events: created=%v %v; want stored", created, err)
		}
	})
}

// TestTaskEventsHaveTheirOwnQueue: the same, for the undelivered caps, with
// every row outside the hour.
func TestTaskEventsHaveTheirOwnQueue(t *testing.T) {
	t.Run("queued chief mail does not fill the task-event queue", func(t *testing.T) {
		s, pool, dsn := pgStore(t)
		_, p := mustCreate(t, s, 2, "example-tracker")
		task := pgTask(t, pool, 2)
		fillMessages(t, dsn, p, 0, MaxTaskEventsQueued, "mail-", "2 hours")
		if _, _, err := s.Submit(plugin(2), p.ID, msg("mail-over")); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("chief mail past its queue: %v; want ErrQueueFull (control)", err)
		}
		if _, created, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e1")); err != nil || !created {
			t.Fatalf("a task event behind a full chief-mail queue: created=%v %v; want stored", created, err)
		}
	})
	t.Run("queued task events do not fill the chief-mail queue", func(t *testing.T) {
		s, pool, dsn := pgStore(t)
		_, p := mustCreate(t, s, 2, "example-tracker")
		task := pgTask(t, pool, 2)
		fillMessages(t, dsn, p, task, MaxTaskEventsQueued-1, "ev-", "2 hours")
		if _, created, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "last")); err != nil || !created {
			t.Fatalf("undelivered task event %d of %d: created=%v %v", MaxTaskEventsQueued, MaxTaskEventsQueued, created, err)
		}
		if _, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "over")); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("task event past %d undelivered: %v; want ErrQueueFull", MaxTaskEventsQueued, err)
		}
		if _, created, err := s.Submit(plugin(2), p.ID, msg("mail-1")); err != nil || !created {
			t.Fatalf("chief mail behind a full task-event queue: created=%v %v; want stored", created, err)
		}
	})
}

// rlsOffForTest rolls row-level security back (db.RLSOff) for the rest of the
// test and restores it after. audit_log is not one of the tables RLSOff turns
// off (it stays under 0009's policies), so with alsoAudit it is turned off by
// hand too: the queries must keep to the caller's own rows on their own.
func rlsOffForTest(t *testing.T, pool *pgxpool.Pool, dsn string, alsoAudit bool) {
	t.Helper()
	if err := db.RLSOff(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if alsoAudit {
		dbtest.OwnerExec(t, dsn, `ALTER TABLE audit_log NO FORCE ROW LEVEL SECURITY; ALTER TABLE audit_log DISABLE ROW LEVEL SECURITY`)
	}
	t.Cleanup(func() {
		if alsoAudit {
			dbtest.OwnerExec(t, dsn, `ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY; ALTER TABLE audit_log FORCE ROW LEVEL SECURITY`)
		}
		if err := db.RLSOn(context.Background(), pool); err != nil {
			t.Errorf("RLSOn: %v", err)
		}
	})
}

// TestUnroutedTaskEventsAreTheCallersWithRLSOff: with row-level security
// rolled back (audit_log too), another owner's NEWER task event does not take
// the caller's place under the LIMIT, and another owner's routing row naming
// the caller's task does not take the caller's event off the list.
func TestUnroutedTaskEventsAreTheCallersWithRLSOff(t *testing.T) {
	s, pool, dsn := pgStore(t)
	_, mine := mustCreate(t, s, 2, "example-tracker")
	_, theirs := mustCreate(t, s, 3, "example-tracker")
	myTask, theirTask := pgTask(t, pool, 2), pgTask(t, pool, 3)
	if _, _, err := s.SubmitTaskEvent(plugin(2), mine.ID, myTask, taskEventIn(t, myTask, "mine")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTaskEvent(plugin(3), theirs.ID, theirTask, taskEventIn(t, theirTask, "theirs")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskRouted(session(3), myTask, "task_forward"); err != nil {
		t.Fatal(err)
	}
	rlsOffForTest(t, pool, dsn, true)
	got, err := s.UnroutedTaskEvents(session(2), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OwnerID != 2 || *got[0].TaskID != myTask {
		t.Fatalf("owner 2's unrouted events with RLS off: %+v; want only its own task %d", got, myTask)
	}
}

// TestTheChiefDispatchLedgerIsTheCallersWithoutRLS: with row-level security
// off (audit_log included), another owner's start for the SAME task id, its
// live agents and its day's starts count for nothing: the caller reserves.
func TestTheChiefDispatchLedgerIsTheCallersWithoutRLS(t *testing.T) {
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const shared = 610
	// Owner 3: a start for task 610 with a live agent, another live start, and
	// a full day of starts.
	for i, task := range []int64{shared, 611} {
		if _, err := s.ReserveChiefDispatch(session(3), task, nil, nil); err != nil {
			t.Fatal(err)
		}
		a := mustAgent(t, dsn, 3, "plg-led-"+strconv.Itoa(i))
		if err := s.RecordChiefDispatchAgent(session(3), task, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO audit_log (user_id, action, object)
		SELECT 3, $1, 'note:' || (700 + g) FROM generate_series(1, $2) g`, ActionChiefDispatch, MaxChiefDispatchesPerDay)
	rlsOffForTest(t, pool, dsn, true)
	r, err := s.ReserveChiefDispatch(session(2), shared, nil, nil)
	if err != nil || r.Prior {
		t.Fatalf("owner 2's first start for task %d with RLS off: %+v %v; want a fresh reservation", shared, r, err)
	}
}

// TestAnotherOwnersStartedAgentDoesNotFreeTheCallersPendingSlot: with RLS off,
// another owner's recorded agent for the same task id does not make the
// caller's in-flight start for it stop counting as live.
func TestAnotherOwnersStartedAgentDoesNotFreeTheCallersPendingSlot(t *testing.T) {
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	if err := s.RecordChiefDispatchAgent(session(3), 620, mustAgent(t, dsn, 3, "plg-led-x").ID); err != nil {
		t.Fatal(err)
	}
	rlsOffForTest(t, pool, dsn, true)
	for _, task := range []int64{620, 621} {
		if r, err := s.ReserveChiefDispatch(session(2), task, nil, nil); err != nil || r.Prior {
			t.Fatalf("reserve %d: %+v %v", task, r, err)
		}
	}
	if _, err := s.ReserveChiefDispatch(session(2), 622, nil, nil); !errors.Is(err, ErrChiefLiveCap) {
		t.Fatalf("a third start with two of the caller's in flight: %v; want ErrChiefLiveCap", err)
	}
}

// TestAPriorStartNamesOnlyTheCallersAgent: with RLS off, the caller's prior
// start for a task answers with the caller's agent (none recorded here), never
// another owner's agent recorded under the same task id.
func TestAPriorStartNamesOnlyTheCallersAgent(t *testing.T) {
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	if _, err := s.ReserveChiefDispatch(session(2), 630, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(3), 630, mustAgent(t, dsn, 3, "plg-led-y").ID); err != nil {
		t.Fatal(err)
	}
	rlsOffForTest(t, pool, dsn, true)
	r, err := s.ReserveChiefDispatch(session(2), 630, nil, nil)
	if err != nil || !r.Prior || r.AgentID != nil {
		t.Fatalf("the caller's repeat: %+v %v; want Prior with no agent", r, err)
	}
}
