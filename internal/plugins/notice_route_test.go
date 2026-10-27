package plugins

import (
	"errors"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestANoticeRouteAndATaskEventNeverTakeEachOthersRow (task routing, audit
// round 2): a notice route is a delivered row routed around the target, never
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

// TestAReclaimedNoticeRouteIsHeldByItsReclaimer: a stale reservation never
// completed is reclaimed by ONE send; while that send's action runs (not yet
// completed), the next send is told not to act — the claim, not the row's age,
// is what it is measured from — and once completed nothing reclaims it.
func TestAReclaimedNoticeRouteIsHeldByItsReclaimer(t *testing.T) {
	s, pool, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-tracker")
	task := pgTask(t, pool, 2)
	in := taskEventIn(t, task, "m-stale")
	m, act, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, in, "agent")
	if err != nil || !act {
		t.Fatalf("reserve: %v %v", act, err)
	}
	if _, act, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, in, "agent"); err != nil || act {
		t.Fatalf("a send while the reservation is fresh: act=%v %v; want not to act", act, err)
	}
	dbtest.OwnerExec(t, dsn, `DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE plugin_messages SET created_at = now() - interval '1 hour' WHERE id = `+strconv.FormatInt(m.ID, 10)+`;
	END $$`)
	got, act, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, in, "attention")
	if err != nil || !act || got.ID != m.ID || *got.RoutedTo != "attention" {
		t.Fatalf("a send after it went stale: %+v act=%v %v; want to reclaim it, for today's destination", got, act, err)
	}
	if _, act, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, in, "attention"); err != nil || act {
		t.Fatalf("a send while the reclaimer acts: act=%v %v; want not to act", act, err)
	}
	if err := s.CompleteNoticeRoute(plugin(2), m.ID); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE plugin_messages SET created_at = now() - interval '2 hours', routed_claimed_at = now() - interval '2 hours'
		 WHERE id = `+strconv.FormatInt(m.ID, 10)+`;
	END $$`)
	if _, act, err := s.ReserveNoticeRoute(plugin(2), p.ID, task, in, "agent"); err != nil || act {
		t.Fatalf("a send long after it completed: act=%v %v; want not to act", act, err)
	}
}
