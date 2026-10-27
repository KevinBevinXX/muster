package plugins

import (
	"errors"
	"strings"
	"testing"
)

// TestATaskEventTheChiefActedOnIsNotRetryable (audit round 4): an unconfirmed
// task event whose task the target already routed (ActionTaskRouted) or started
// an agent for (ActionTaskDispatchAgent) after the event is not retryable —
// a retry would forward or ask twice. The predicate is UnroutedTaskEvents'
// own (agentActedOn). An action recorded BEFORE the event does not count.
func TestATaskEventTheChiefActedOnIsNotRetryable(t *testing.T) {
	for _, act := range []string{ActionTaskRouted, ActionTaskDispatchAgent} {
		t.Run(act, func(t *testing.T) {
			s, pool, dsn := pgStore(t)
			_, p := mustCreate(t, s, 2, "example-tracker")
			target := mustAgent(t, dsn, 2, "plg-retry-target")
			task := pgTask(t, pool, 2)
			record := func() {
				t.Helper()
				var err error
				if act == ActionTaskRouted {
					err = s.RecordTaskRouted(session(2), task, "task_forward")
				} else {
					err = s.RecordTaskDispatchAgent(session(2), task, target.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			record() // before the event: does not count
			m, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e1"))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.BeginDelivery(plugin(2), m.ID, target.ID); err != nil || !ok {
				t.Fatalf("begin: %v %v", ok, err)
			}
			if ok, err := s.RecordDelivery(plugin(2), m.ID, target.ID, StatusUnconfirmed); err != nil || !ok {
				t.Fatalf("unconfirmed: %v %v", ok, err)
			}
			retryable := func() bool {
				t.Helper()
				ms, err := s.Retryable(session(2), 50)
				if err != nil {
					t.Fatal(err)
				}
				for _, x := range ms {
					if x.ID == m.ID {
						return true
					}
				}
				return false
			}
			if !retryable() {
				t.Fatal("CONTROL: an unconfirmed task event with only an older action is not retryable")
			}
			record() // after the event: the target acted on it
			if retryable() {
				t.Fatal("a task event the target acted on is listed as retryable")
			}
			if _, err := s.Requeue(session(2), m.ID); !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "already acted on this task") {
				t.Fatalf("requeue: %v; want ErrNotRetryable saying the target already acted on the task", err)
			}
		})
	}
}
