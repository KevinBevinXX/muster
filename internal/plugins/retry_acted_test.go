package plugins

import (
	"errors"
	"strings"
	"testing"
)

// TestATaskEventTheChiefActedOnIsNotRetryable (audit round 4): an unconfirmed
// task event whose task the chief already routed (ActionTaskRouted) or started
// an agent for (ActionChiefDispatchAgent) after the event is not retryable —
// a retry would forward or ask twice. The predicate is UnroutedTaskEvents'
// own (chiefActedOn). An action recorded BEFORE the event does not count.
func TestATaskEventTheChiefActedOnIsNotRetryable(t *testing.T) {
	for _, act := range []string{ActionTaskRouted, ActionChiefDispatchAgent} {
		t.Run(act, func(t *testing.T) {
			s, pool, dsn := pgStore(t)
			_, p := mustCreate(t, s, 2, "example-tracker")
			chief := mustAgent(t, dsn, 2, "plg-retry-chief")
			task := pgTask(t, pool, 2)
			record := func() {
				t.Helper()
				var err error
				if act == ActionTaskRouted {
					err = s.RecordTaskRouted(session(2), task, "task_forward")
				} else {
					err = s.RecordChiefDispatchAgent(session(2), task, chief.ID)
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
			if ok, err := s.BeginDelivery(plugin(2), m.ID, chief.ID); err != nil || !ok {
				t.Fatalf("begin: %v %v", ok, err)
			}
			if ok, err := s.RecordDelivery(plugin(2), m.ID, chief.ID, StatusUnconfirmed); err != nil || !ok {
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
			record() // after the event: the chief acted on it
			if retryable() {
				t.Fatal("a task event the chief acted on is listed as retryable")
			}
			if _, err := s.Requeue(session(2), m.ID); !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "already acted on this task") {
				t.Fatalf("requeue: %v; want ErrNotRetryable saying the chief already acted on the task", err)
			}
		})
	}
}
