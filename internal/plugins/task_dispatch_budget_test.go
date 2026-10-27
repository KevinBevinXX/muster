package plugins

import (
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// The target's dispatch budget (dispatchBudget / TaskDispatchBudget), on a real
// Postgres. Its fixture values are pairwise distinct AND distinct from the cap
// constants (live 2, today 4 — never 5 or 10), so a budget that reported a cap
// as a count, or one count as the other, cannot pass.

// budgetFixture writes, for owner 2, starts on tasks 810..814:
//
//	810  agent recorded, running   -> live (listed with its id and name), today
//	811  agent recorded, stopped   -> today only
//	812  no agent, reserved now    -> live (pending: id and name null), today
//	813  no agent, 20 minutes old  -> today only (past pendingAgentWindow)
//	814  no agent, two days old    -> neither
//
// and, for owner 3, a running recorded agent and two fresh starts — on 810, the
// same task id — none of which may count for owner 2, plus an agent row of
// owner 3's on 812 NEWER than owner 2's pending start there, which must not
// make that start read as recorded. It returns the agent of task 810.
func budgetFixture(t *testing.T, s *PGStore, dsn string) (int64, string) {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	for _, task := range []int64{810, 811, 812} {
		if r, err := s.ReserveTaskDispatch(session(2), task, nil, nil); err != nil || r.Prior {
			t.Fatalf("reserve %d: %+v %v", task, r, err)
		}
	}
	running := mustAgent(t, dsn, 2, "plg-budget-running")
	stopped := mustAgent(t, dsn, 2, "plg-budget-stopped")
	dbtest.OwnerExec(t, dsn, `UPDATE agents SET status = 'stopped' WHERE id = $1`, stopped.ID)
	if err := s.RecordTaskDispatchAgent(session(2), 810, running.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskDispatchAgent(session(2), 811, stopped.ID); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO audit_log (user_id, action, object, at) VALUES
		(2, $1, 'note:813', now() - interval '20 minutes'),
		(2, $1, 'note:814', now() - interval '2 days')`, ActionTaskDispatch)
	// Owner 3's: must count for nothing of owner 2's.
	for _, task := range []int64{810, 820} {
		if _, err := s.ReserveTaskDispatch(session(3), task, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	theirs := mustAgent(t, dsn, 3, "plg-budget-theirs")
	if err := s.RecordTaskDispatchAgent(session(3), 810, theirs.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskDispatchAgent(session(3), 812, theirs.ID); err != nil {
		t.Fatal(err)
	}
	return running.ID, running.Name
}

// TestTheChiefsBudgetCountsItsOwnLedger: the budget is exactly the owner's
// live target-started agents (a recorded live agent by id and name, a start
// whose agent is not recorded yet with id and name null) and its starts in the
// last day, with the caps — with row-level security on, and with it rolled back
// (every read names the owner).
func TestTheChiefsBudgetCountsItsOwnLedger(t *testing.T) {
	for _, rlsOff := range []bool{false, true} {
		name := "rls-on"
		if rlsOff {
			name = "rls-off"
		}
		t.Run(name, func(t *testing.T) {
			s, pool, dsn := pgStore(t)
			agentID, agentName := budgetFixture(t, s, dsn)
			if rlsOff {
				rlsOffForTest(t, pool, dsn, true)
			}
			got, err := s.TaskDispatchBudget(session(2))
			if err != nil {
				t.Fatal(err)
			}
			want := DispatchBudget{Live: 2, LiveCap: MaxLiveDispatchedAgents, Today: 4, DailyCap: MaxDispatchesPerDay,
				LiveAgents: []LiveDispatchedAgent{{ID: &agentID, Name: &agentName, Task: 810}, {Task: 812}}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("budget\n got %s\nwant %s", budgetString(got), budgetString(want))
			}
			if got.Full() || got.LiveFull() || got.DailyFull() {
				t.Errorf("a budget of live 2 / today 4 reads as full: %s", budgetString(got))
			}
		})
	}
}

// TestACapRefusalCarriesTheBudgetItWasDecidedOn: a live-cap refusal and a
// daily-cap refusal each return, with the error, the budget the reservation
// compared — equal to the read the target is shown, and full on THAT cap.
func TestACapRefusalCarriesTheBudgetItWasDecidedOn(t *testing.T) {
	s, _, dsn := pgStore(t)

	// The live cap: a full cap of starts in flight.
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	for task := int64(830); task < 830+MaxLiveDispatchedAgents; task++ {
		if _, err := s.ReserveTaskDispatch(session(2), task, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.ReserveTaskDispatch(session(2), 830+MaxLiveDispatchedAgents, nil, nil)
	if !errors.Is(err, ErrDispatchLiveCap) || r.Budget == nil {
		t.Fatalf("one start over the live cap: %+v %v; want ErrDispatchLiveCap with its budget", r, err)
	}
	read, err := s.TaskDispatchBudget(session(2))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*r.Budget, read) || !r.Budget.LiveFull() || r.Budget.Live != MaxLiveDispatchedAgents || len(r.Budget.LiveAgents) != MaxLiveDispatchedAgents {
		t.Fatalf("live-cap refusal's budget %s; the read %s", budgetString(*r.Budget), budgetString(read))
	}

	// The daily cap: a full day of starts, none live any more (older than the
	// pending window, no agent).
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO audit_log (user_id, action, object, at)
		SELECT 2, $1, 'note:' || (840 + g), now() - interval '1 hour' FROM generate_series(1, $2) g`,
		ActionTaskDispatch, MaxDispatchesPerDay)
	r, err = s.ReserveTaskDispatch(session(2), 860, nil, nil)
	if !errors.Is(err, ErrDispatchDailyCap) || r.Budget == nil {
		t.Fatalf("one start over the daily cap: %+v %v; want ErrDispatchDailyCap with its budget", r, err)
	}
	if read, err = s.TaskDispatchBudget(session(2)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*r.Budget, read) || !r.Budget.DailyFull() || r.Budget.LiveFull() || r.Budget.Today != MaxDispatchesPerDay {
		t.Fatalf("daily-cap refusal's budget %s; the read %s", budgetString(*r.Budget), budgetString(read))
	}

	// A start that passes carries no budget of its own (the caller reads one).
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	if r, err := s.ReserveTaskDispatch(session(2), 870, nil, nil); err != nil || r.Budget != nil {
		t.Fatalf("a fresh start: %+v %v; want no error and no budget", r, err)
	}
}

func budgetString(b DispatchBudget) string {
	s := ""
	for _, a := range b.LiveAgents {
		id, name := "null", "null"
		if a.ID != nil {
			id = strconv.FormatInt(*a.ID, 10)
		}
		if a.Name != nil {
			name = *a.Name
		}
		s += " {" + id + " " + name + " task " + strconv.FormatInt(a.Task, 10) + "}"
	}
	return "live " + strconv.FormatInt(int64(b.Live), 10) + "/" + strconv.FormatInt(int64(b.LiveCap), 10) + " today " + strconv.FormatInt(int64(b.Today), 10) + "/" + strconv.FormatInt(int64(b.DailyCap), 10) + " agents:" + s
}
