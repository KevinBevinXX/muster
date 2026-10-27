package db_test

import (
	"strings"
	"testing"
)

// TestMigration0030OnTheLiveChiefShape: a database at version 28 holding the
// shape a running deployment has — a claude-code agent with its legacy session
// (the one its kickoff wrote) and two later sessions keyed by the
// timestamp scheme, one of them the most recently active, with messages in all
// three — arrives at 0030 with:
//
//   - every pre-existing session LEGACY (runtime ” and model ”), so not one of
//     them changes route: a claude-code agent's legacy sessions all stay on the
//     one CLI ccd has always run;
//   - the agent's default being its LEGACY-KEY session, not the most recently
//     active one and not the oldest by id alone;
//   - an agent whose sessions include no legacy key defaulting to its OLDEST;
//   - an agent with no session having no default (elected lazily later);
//   - the messages untouched.
//
// Then the new rules hold: at most one default per agent, a legacy session
// cannot name a model, the runtime set is closed, and runtime/model cannot be
// changed once written.
func TestMigration0030OnTheLiveChiefShape(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0030_runtime", 28)

	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, kind, cc_account)
		VALUES (1, 'agent', 'muster-agent-agent', 'claude-code', 'acct-one') RETURNING id`)
	worker := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'quiet-otter', 'ns-otter') RETURNING id`)
	empty := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'idle-wren', 'ns-wren') RETURNING id`)

	session := func(agent int64, key string, created, updated string) int64 {
		return scanID(ctx, t, pool, `INSERT INTO chat_sessions (owner_id, agent_id, session_key, title, created_at, updated_at)
			VALUES (1, $1, $2, 't', now() - $3::interval, now() - $4::interval) RETURNING id`, agent, key, created, updated)
	}
	// The plugin sessions are NEWER and more recently active than the legacy one:
	// a backfill by "latest" or "newest" would pick one of them.
	legacy := session(agent, "agent:agent:webchat", "3 days", "2 days")
	discord := session(agent, "agent:agent:webchat:1792372820708414638", "2 days", "1 hour")
	mail := session(agent, "agent:agent:webchat:1792389590335149096", "1 day", "5 minutes")
	wOld := session(worker, "agent:quiet-otter:webchat:100", "5 days", "5 days")
	wNew := session(worker, "agent:quiet-otter:webchat:200", "1 day", "1 minute")
	for _, s := range []int64{legacy, discord, mail, wOld} {
		if _, err := pool.Exec(ctx, `INSERT INTO chat_messages (owner_id, agent_id, session_id, role, content)
			SELECT 1, agent_id, id, 'user', 'hello ' || id FROM chat_sessions WHERE id = $1`, s); err != nil {
			t.Fatal(err)
		}
	}
	var msgsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages`).Scan(&msgsBefore); err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, 30)

	var legacyRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_sessions WHERE runtime = '' AND model = ''`).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 5 {
		t.Errorf("after 0030, %d of 5 pre-existing sessions are legacy (runtime '' and model '')", legacyRows)
	}
	defaults := map[int64]int64{}
	rows, err := pool.Query(ctx, `SELECT agent_id, id FROM chat_sessions WHERE is_default`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, s int64
		if err := rows.Scan(&a, &s); err != nil {
			t.Fatal(err)
		}
		defaults[a] = s
	}
	rows.Close()
	if defaults[agent] != legacy {
		t.Errorf("the agent's default is session %d, want its legacy-key session %d (discord %d, mail %d)", defaults[agent], legacy, discord, mail)
	}
	if defaults[worker] != wOld {
		t.Errorf("an agent with no legacy key defaults to %d, want its oldest %d (newest %d)", defaults[worker], wOld, wNew)
	}
	if s, ok := defaults[empty]; ok {
		t.Errorf("an agent with no session has default %d, want none", s)
	}
	var msgsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages`).Scan(&msgsAfter); err != nil {
		t.Fatal(err)
	}
	if msgsAfter != msgsBefore || msgsBefore != 4 {
		t.Errorf("messages: %d before, %d after, want 4 and 4", msgsBefore, msgsAfter)
	}

	// At most one default per agent.
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET is_default = true WHERE id = $1`, mail); err == nil ||
		!strings.Contains(err.Error(), "chat_sessions_one_default") {
		t.Errorf("a second default for one agent was not refused by the partial unique index: %v", err)
	}
	// Moving it is two statements in one transaction, and allowed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET is_default = false WHERE agent_id = $1 AND is_default`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET is_default = true WHERE id = $1`, mail); err != nil {
		t.Errorf("moving the default: %v", err)
	}
	_ = tx.Rollback(ctx)

	insert := func(runtime, model string) error {
		_, err := pool.Exec(ctx, `INSERT INTO chat_sessions (owner_id, agent_id, session_key, runtime, model)
			VALUES (1, $1, 'agent:agent:webchat:' || nextval(pg_get_serial_sequence('chat_sessions', 'id')), $2, $3)`, agent, runtime, model)
		return err
	}
	for _, c := range []struct{ runtime, model string }{
		{"claude-code", "opus"}, {"claude-code", ""}, {"gateway", "openrouter/vendor/model-x"},
	} {
		if err := insert(c.runtime, c.model); err != nil {
			t.Errorf("after 0030, a %q session on %q: %v", c.runtime, c.model, err)
		}
	}
	for _, c := range []struct{ runtime, model, why string }{
		{"", "opus", "a legacy session naming a model"},
		{"webchat", "", "a runtime outside the set"},
		{"gateway", strings.Repeat("m", 201), "a model over 200 characters"},
	} {
		if err := insert(c.runtime, c.model); err == nil || !strings.Contains(err.Error(), "chat_sessions_runtime_check") {
			t.Errorf("after 0030, %s was not refused by chat_sessions_runtime_check: %v", c.why, err)
		}
	}
	for _, set := range []string{`runtime = 'gateway', model = 'openrouter/vendor/model-x'`, `model = 'opus'`} {
		if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET `+set+` WHERE id = $1`, discord); err == nil ||
			!strings.Contains(err.Error(), "fixed when it is created") {
			t.Errorf("after 0030, UPDATE %s on an existing session was not refused by the immutability trigger: %v", set, err)
		}
	}
	// Other columns still move (the trigger is scoped to runtime and model).
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET title = 'renamed', updated_at = now() WHERE id = $1`, discord); err != nil {
		t.Errorf("after 0030, an ordinary UPDATE: %v", err)
	}
}
