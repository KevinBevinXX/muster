package db_test

import (
	"testing"
)

// TestMigration0032LinksOnlyTheChiefRoutedAsks: a database before 0032 holding
// the live shape — the agent's task_route_attention asks (each written beside
// its `task.chief_routed` audit row), plus asks that only LOOK like one — arrives
// at 0032 with task_id set on exactly the agent-routed ones:
//
//   - linked: question "Task #N needs you: …" AND the owner's chief_routed row for
//     note:N, tool task_route_attention, within 5s after the ask;
//   - NOT linked: the same question with no audit row (an agent's own
//     attention_ask can write any text); an audit row for ANOTHER task; one
//     another owner wrote; one written 10s later; one for task_forward;
//   - a task that is not the ask owner's is never linked.
//
// Then the new rules hold: an ask cannot name another owner's task (the
// trigger), and deleting the task un-links the ask without deleting it.
func TestMigration0032LinksOnlyTheChiefRoutedAsks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0032_task_link", 31)

	other := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, kind, cc_account)
		VALUES (1, 'agent', 'muster-agent-agent', 'claude-code', 'acct-one') RETURNING id`)
	task := func(owner int64) int64 {
		return scanID(ctx, t, pool, `INSERT INTO notes (owner_id, body) VALUES ($1, 'b') RETURNING id`, owner)
	}
	t1, t2, t3, t4, t5, t6, theirs := task(1), task(1), task(1), task(1), task(1), task(1), task(other)
	ask := func(owner int64, agent any, taskID int64, created string) int64 {
		system := agent == nil
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, agent_id, system_asker, question, expires_at, created_at)
			VALUES ($1, $2, $3, 'Task #' || $4::bigint::text || ' needs you: t', now() + interval '1 day', now() - $5::interval) RETURNING id`,
			owner, agent, system, taskID, created)
	}
	audit := func(user, taskID int64, tool string, at string) {
		mustExec(ctx, t, pool, `INSERT INTO audit_log (user_id, via, action, object, detail, at)
			VALUES ($1, 'connect', 'task.chief_routed', 'note:' || $2::bigint::text, jsonb_build_object('tool', $3::text), now() - $4::interval)`,
			user, taskID, tool, at)
	}
	// Linked: the ask at -60s, its audit row 3ms after (the live gap).
	routed := ask(1, agent, t1, "60 seconds")
	audit(1, t1, "task_route_attention", "59.997 seconds")
	// Not linked: text only.
	textOnly := ask(1, agent, t2, "50 seconds")
	// Not linked: the audit names another task.
	wrongTask := ask(1, agent, t3, "40 seconds")
	audit(1, t4, "task_route_attention", "39.99 seconds")
	// Not linked: another owner's audit row.
	wrongUser := ask(1, agent, t5, "30 seconds")
	audit(other, t5, "task_route_attention", "29.99 seconds")
	// Not linked: 10s after the ask.
	late := ask(1, agent, t6, "100 seconds")
	audit(1, t6, "task_route_attention", "90 seconds")
	// Not linked: task_forward, not task_route_attention.
	forward := ask(1, nil, t4, "20 seconds")
	audit(1, t4, "task_forward", "19.99 seconds")
	// Not linked: the task is another owner's (the text and audit both name it).
	notMine := ask(1, nil, theirs, "10 seconds")
	audit(1, theirs, "task_route_attention", "9.99 seconds")

	applyThroughFrom(ctx, t, pool, 32)

	linked := map[int64]int64{}
	rows, err := pool.Query(ctx, `SELECT id, task_id FROM attention_asks WHERE task_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, n int64
		if err := rows.Scan(&a, &n); err != nil {
			t.Fatal(err)
		}
		linked[a] = n
	}
	rows.Close()
	if len(linked) != 1 || linked[routed] != t1 {
		t.Fatalf("linked %v; want only ask %d → task %d (not text-only %d, wrong-task %d, wrong-user %d, late %d, forward %d, not-mine %d)",
			linked, routed, t1, textOnly, wrongTask, wrongUser, late, forward, notMine)
	}

	// An ask cannot name another owner's task.
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET task_id = $1 WHERE id = $2`, theirs, textOnly); err == nil {
		t.Fatal("an ask was linked to another owner's task")
	}
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET task_id = $1 WHERE id = $2`, t2, textOnly); err != nil {
		t.Fatalf("control: linking an ask to its own owner's task: %v", err)
	}
	// Deleting the task un-links, and keeps, the ask.
	mustExec(ctx, t, pool, `DELETE FROM notes WHERE id = $1`, t1)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attention_asks WHERE id = $1 AND task_id IS NULL`, routed).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after deleting the task: %d rows (%v)", n, err)
	}
	// Re-running the body changes nothing and does not fail.
	mustExec(ctx, t, pool, readMigration(t, "0032_attention_task_link.sql"))
}
