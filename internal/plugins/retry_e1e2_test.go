package plugins

import (
	"testing"
)

// TestRetryReOffersAnEventTheChiefNeverActedOn (audit round 5): two events
// queued on one task. The chief's turn on E1 routes the task; E2's own turn is
// then cut off (unconfirmed). That action came BEFORE E2 was delivered, so it
// is not an action on E2: Retry lists E2 and requeues it. An event the chief
// acted on during or after its OWN delivery (E3) is still refused.
func TestRetryReOffersAnEventTheChiefNeverActedOn(t *testing.T) {
	s, pool, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-tracker")
	chief := mustAgent(t, dsn, 2, "plg-e1e2-chief")
	task := pgTask(t, pool, 2)
	e1, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e1"))
	if err != nil {
		t.Fatal(err)
	}
	e2, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e2"))
	if err != nil {
		t.Fatal(err)
	}
	settle := func(id int64, outcome Status, during func()) {
		t.Helper()
		if ok, err := s.BeginDelivery(plugin(2), id, chief.ID); err != nil || !ok {
			t.Fatalf("begin %d: %v %v", id, ok, err)
		}
		if during != nil {
			during()
		}
		if ok, err := s.RecordDelivery(plugin(2), id, chief.ID, outcome); err != nil || !ok {
			t.Fatalf("settle %d: %v %v", id, ok, err)
		}
	}
	route := func() {
		t.Helper()
		if err := s.RecordTaskRouted(session(2), task, "task_forward"); err != nil {
			t.Fatal(err)
		}
	}
	settle(e1.ID, StatusDelivered, route) // the chief forwards on E1's turn
	settle(e2.ID, StatusUnconfirmed, nil) // E2's turn is cut off
	listed := func(id int64) bool {
		t.Helper()
		ms, err := s.Retryable(session(2), 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range ms {
			if x.ID == id {
				return true
			}
		}
		return false
	}
	if !listed(e2.ID) {
		t.Fatal("E2, whose turn was cut off before the chief saw it, is not retryable")
	}
	if _, err := s.Requeue(session(2), e2.ID); err != nil {
		t.Fatalf("requeue E2: %v; want requeued", err)
	}
	// E3: the chief acts during E3's own turn, which is then cut off.
	e3, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e3"))
	if err != nil {
		t.Fatal(err)
	}
	settle(e3.ID, StatusUnconfirmed, route)
	if listed(e3.ID) {
		t.Fatal("E3, acted on during its own turn, is listed as retryable")
	}
}

// TestTaskToRouteListsAnEventTheChiefNeverActedOn: the same per-event rule on
// the "Task to route" side. E1 routed on its own turn; E2, handed over after
// that and not acted on, is listed — it was hidden for good when the rule
// counted any action on the task since E2's creation.
func TestTaskToRouteListsAnEventTheChiefNeverActedOn(t *testing.T) {
	s, pool, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-tracker")
	chief := mustAgent(t, dsn, 2, "plg-ttr-chief")
	task := pgTask(t, pool, 2)
	e1, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e1"))
	if err != nil {
		t.Fatal(err)
	}
	e2, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, taskEventIn(t, task, "e2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{e1.ID, e2.ID} {
		if ok, err := s.BeginDelivery(plugin(2), id, chief.ID); err != nil || !ok {
			t.Fatalf("begin %d: %v %v", id, ok, err)
		}
		if id == e1.ID {
			if err := s.RecordTaskRouted(session(2), task, "task_forward"); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := s.RecordDelivery(plugin(2), id, chief.ID, StatusDelivered); err != nil || !ok {
			t.Fatalf("settle %d: %v %v", id, ok, err)
		}
	}
	got, err := s.UnroutedTaskEvents(session(2), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != e2.ID {
		t.Fatalf("unrouted %+v; want exactly E2 (%d): E1 was routed on its turn, E2 never was", got, e2.ID)
	}
}
