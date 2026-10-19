package plugins

import (
	"errors"
	"testing"
)

// TestANoticeRouteAndATaskEventNeverTakeEachOthersRow (task routing, audit
// round 2): a notice route is a delivered row routed around the chief, never
// a task event, and under one external_id the first kind stored wins: the
// other answers ErrTaskEventIDTaken. A repeat of the same route is the stored
// row, not a second one.
func TestANoticeRouteAndATaskEventNeverTakeEachOthersRow(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-tracker")
	task := pgTask(t, pool, 2)
	route := taskEventIn(t, task, "m1")
	m, created, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, route, "agent")
	if err != nil || !created || m.RoutedTo == nil || *m.RoutedTo != "agent" || m.Status != StatusDelivered {
		t.Fatalf("reserve: %+v created=%v %v; want a delivered agent route", m, created, err)
	}
	again, created, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, route, "attention")
	if err != nil || created || again.ID != m.ID || *again.RoutedTo != "agent" {
		t.Fatalf("a repeat: %+v created=%v %v; want the stored agent route", again, created, err)
	}
	if _, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, route); !errors.Is(err, ErrTaskEventIDTaken) {
		t.Fatalf("a task event under a route's id: %v; want ErrTaskEventIDTaken", err)
	}
	ev := taskEventIn(t, task, "e1")
	if _, _, err := s.SubmitTaskEvent(plugin(2), p.ID, task, ev); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, ev, "agent"); !errors.Is(err, ErrTaskEventIDTaken) {
		t.Fatalf("a route under a task event's id: %v; want ErrTaskEventIDTaken", err)
	}
	if err := s.ReleaseNoticeRoute(plugin(2), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, route, "attention"); err != nil || !created {
		t.Fatalf("after a release: created=%v %v; want a fresh reservation", created, err)
	}
}
