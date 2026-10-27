package plugins

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestTheDatabaseKeepsNoticeRoutesToTheirShape pins
// plugin_messages_routed_to_check (migration 0028) at the database: a notice
// route is "agent" or "attention", on a task, delivered; routed_done_at and
// routed_claimed_at exist only on one. Each invalid row is rejected by THAT
// constraint; each valid one is accepted.
func TestTheDatabaseKeepsNoticeRoutesToTheirShape(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-tracker")
	task := pgTask(t, pool, 2)
	n := 0
	insert := func(routedTo any, taskID any, delivered bool, done, claimed bool) error {
		n++
		status, at := "queued", "NULL"
		if delivered {
			status, at = "delivered", "now()"
		}
		doneAt, claimedAt := "NULL", "NULL"
		if done {
			doneAt = "now()"
		}
		if claimed {
			claimedAt = "now()"
		}
		_, err := pool.Exec(authz.System(context.Background()), `INSERT INTO plugin_messages
			(owner_id, plugin_id, external_id, body, task_id, routed_to, status, delivered_at, routed_done_at, routed_claimed_at)
			VALUES (2, $1, $2, 'b', $3, $4, $5, `+at+`, `+doneAt+`, `+claimedAt+`)`, p.ID, "chk-"+strconv.Itoa(n), taskID, routedTo, status)
		return err
	}
	for _, c := range []struct {
		name           string
		routedTo, task any
		delivered      bool
		done, claimed  bool
	}{
		{"an unknown destination", "target", task, true, false, false},
		{"a route without a task", "agent", nil, true, false, false},
		{"a route not delivered", "attention", task, false, false, false},
		{"done without a route", nil, task, true, true, false},
		{"claimed without a route", nil, task, true, false, true},
	} {
		err := insert(c.routedTo, c.task, c.delivered, c.done, c.claimed)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != "plugin_messages_routed_to_check" {
			t.Errorf("%s: %v; want rejected by plugin_messages_routed_to_check", c.name, err)
		}
	}
	for _, c := range []struct {
		name           string
		routedTo, task any
		delivered      bool
		done, claimed  bool
	}{
		{"an agent route, reserved", "agent", task, true, false, false},
		{"an attention route, done", "attention", task, true, true, false},
		{"a reclaimed route", "agent", task, true, false, true},
		{"a task event", nil, task, false, false, false},
		{"an ordinary message", nil, nil, false, false, false},
	} {
		if err := insert(c.routedTo, c.task, c.delivered, c.done, c.claimed); err != nil {
			t.Errorf("%s: %v; want accepted", c.name, err)
		}
	}
}
