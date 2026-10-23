package db_test

import (
	"testing"
)

// TestMigration0045StartsTheClockAtMigrationTime: at version 44, a stopped
// agent whose updated_at is 30 days old and a running one exist. After 0045
// the stopped one's stopped_at is the migration's moment — NOT its old
// updated_at, which would make it reapable on the first sweep — and the
// running one has none. Then the trigger: a row inserted stopped is stamped,
// stopped -> stopped keeps the stamp, and leaving and re-entering stopped
// stamps anew.
func TestMigration0045StartsTheClockAtMigrationTime(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0045_stopped_at", 44)
	stopped := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status) VALUES (1, 'old-stopped', 'ns-a', 'stopped') RETURNING id`)
	running := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status) VALUES (1, 'old-running', 'ns-b', 'running') RETURNING id`)
	if _, err := pool.Exec(ctx, `UPDATE agents SET updated_at = now() - interval '30 days' WHERE id = ANY($1)`, []int64{stopped, running}); err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, 45)

	// ageSecs is how old id's stopped_at is, in seconds; nil when it has none.
	ageSecs := func(id int64) *float64 {
		t.Helper()
		var secs *float64
		if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - stopped_at)::float8 FROM agents WHERE id = $1`, id).Scan(&secs); err != nil {
			t.Fatal(err)
		}
		return secs
	}
	if a := ageSecs(stopped); a == nil || *a > 600 {
		t.Fatalf("an agent stopped before 0045 has stopped_at %v seconds old; want the migration's moment", a)
	}
	if a := ageSecs(running); a != nil {
		t.Fatalf("a running agent was given stopped_at (%v seconds old)", *a)
	}

	fresh := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status) VALUES (1, 'new-stopped', 'ns-c', 'stopped') RETURNING id`)
	if a := ageSecs(fresh); a == nil || *a > 600 {
		t.Fatalf("a row inserted stopped has stopped_at %v", a)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE agents SET stopped_at = now() - interval '20 days', reap_claimed_at = now() WHERE id = $1`, fresh)
	exec(`UPDATE agents SET status = 'stopped' WHERE id = $1`, fresh)
	if a := ageSecs(fresh); a == nil || *a < 19*86400 {
		t.Fatalf("stopped -> stopped moved the stamp: %v", a)
	}
	exec(`UPDATE agents SET status = 'running' WHERE id = $1`, fresh)
	if a := ageSecs(fresh); a == nil || *a < 19*86400 {
		t.Fatalf("leaving stopped changed stopped_at: %v (it is kept, read with status)", a)
	}
	exec(`UPDATE agents SET status = 'stopped' WHERE id = $1`, fresh)
	var claimed *string
	if err := pool.QueryRow(ctx, `SELECT reap_claimed_at::text FROM agents WHERE id = $1`, fresh).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if a := ageSecs(fresh); a == nil || *a > 600 || claimed != nil {
		t.Fatalf("re-entering stopped: stopped_at %v seconds old, claim %v; want a fresh stamp and no claim", a, claimed)
	}
}
