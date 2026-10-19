package attention

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestTheDatabaseAllowsExactlyOneAsker pins attention_asks_asker_check
// (migration 0028) at the database, bypassing Asker.Valid: each invalid
// combination of agent / mirror session / muster is rejected by THAT
// constraint, and each of the three single askers is accepted.
func TestTheDatabaseAllowsExactlyOneAsker(t *testing.T) {
	_, pool, _ := pgStore(t)
	var agent int64
	if err := pool.QueryRow(as(2), `INSERT INTO agents (name, namespace) VALUES ($1, $1) RETURNING id`,
		"attn-chk-"+time.Now().Format("150405.000000")).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	insert := func(agentID any, host, session any, system bool) error {
		_, err := pool.Exec(as(2), `INSERT INTO attention_asks (agent_id, mirror_host, mirror_session, system_asker, question, expires_at)
			VALUES ($1, $2, $3, $4, 'q', now() + interval '1 hour')`, agentID, host, session, system)
		return err
	}
	const host, sess = "laptop", "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	for _, c := range []struct {
		name          string
		agent         any
		host, session any
		system        bool
	}{
		{"agent and muster", agent, nil, nil, true},
		{"session and muster", nil, host, sess, true},
		{"agent and session", agent, host, sess, false},
		{"nobody", nil, nil, nil, false},
		{"a host without its session", nil, host, nil, false},
		{"a host without its session, and muster", nil, host, nil, true},
	} {
		err := insert(c.agent, c.host, c.session, c.system)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != "attention_asks_asker_check" {
			t.Errorf("%s: %v; want rejected by attention_asks_asker_check", c.name, err)
		}
	}
	for _, c := range []struct {
		name          string
		agent         any
		host, session any
		system        bool
	}{
		{"an agent", agent, nil, nil, false},
		{"a session", nil, host, sess, false},
		{"muster", nil, nil, nil, true},
	} {
		if err := insert(c.agent, c.host, c.session, c.system); err != nil {
			t.Errorf("%s: %v; want accepted", c.name, err)
		}
	}
}
