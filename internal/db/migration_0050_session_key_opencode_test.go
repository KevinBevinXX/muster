package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0050AdmitsARuntimeQualifiedOpenCodeKey: at version 49 an
// opencode session's key — "opencode:ses_<id>" — is refused by each of the
// four session-key CHECKs (the control: 0036's pattern has no opencode
// branch, in every table); after 0050 it is stored in all four, and so are a
// Claude Code id and a Hermes key exactly as before. What it must STILL refuse
// is checked in every table too: an empty or over-long opencode id, a bad
// first character, a prefix spelled differently, two prefixes, and junk.
func TestMigration0050AdmitsARuntimeQualifiedOpenCodeKey(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0050_session_key", 49)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'key-agent', 'ns-key') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-key', 'Example inbox', 'pluginhash-key', 'example') RETURNING id`)
	message := scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id)
		VALUES (1, $1, 'ext-key', 'hello', 'delivering', $2) RETURNING id`, plugin, agent)

	type table struct {
		check string
		write func(key string, n int) error
	}
	tables := map[string]table{
		"agent_focus": {"agent_focus_subject_check", func(key string, _ int) error {
			_, err := pool.Exec(ctx, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', $1, 't')`, key)
			return err
		}},
		"agent_focus_history": {"agent_focus_history_subject_check", func(key string, _ int) error {
			_, err := pool.Exec(ctx, `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', $1, 't')`, key)
			return err
		}},
		"session_routes": {"session_routes_key_check", func(key string, n int) error {
			_, err := pool.Exec(ctx, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id, target_kind, target_key,
				timeout_at, state, finished_at) VALUES (1, 'rt_' || substr(md5($2), 1, 24), $3, $4, 'mirror', $1, now() + interval '1 hour', 'failed', now())`,
				key, "route-"+strconv.Itoa(n), message, agent)
			return err
		}},
		"attention_asks": {"attention_asks_mirror_session_check", func(key string, _ int) error {
			_, err := pool.Exec(ctx, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at)
				VALUES (1, 'ws', $1, 'which way?', now() + interval '1 hour')`, key)
			return err
		}},
	}
	opencode := "opencode:ses_0fixture1a2b3c4d5e6f7a8b9c"
	claude := "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	hermes := "hermes:local-" + strings.Repeat("0123456789abcdef", 4)

	n := 0
	stored := func(name, key string) error { n++; return tables[name].write(key, n) }
	refusedBy := func(err error, name string) bool {
		return err != nil && strings.Contains(err.Error(), tables[name].check)
	}
	for name := range tables {
		if err := stored(name, opencode); !refusedBy(err, name) {
			t.Fatalf("control: at version 49 %s stored an opencode key (or refused it for another reason): %v", name, err)
		}
		if err := stored(name, claude); err != nil {
			t.Fatalf("control: at version 49 %s refused a Claude Code id: %v", name, err)
		}
	}

	applyThroughFrom(ctx, t, pool, 50)

	admitted := []string{
		opencode,
		"1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c", // a Claude Code id, as before
		hermes,                                 // a Hermes key, as before
		"opencode:" + strings.Repeat("s", 128), // the opencode id's bound
		"opencode:" + "ses_x-" + strings.Repeat("9", 9),
	}
	refused := []string{
		"opencode:",                            // no id
		"opencode:" + strings.Repeat("e", 129), // one past the bound
		"opencode:_leading-underscore",         // the id's first character
		"OPENCODE:ses_fixture",                 // the prefix is exact
		"open-code:ses_fixture",                // another spelling
		"opencode:opencode:ses_fixture",        // one prefix only
		"opencode:ses fixture",                 // a space
		strings.Repeat("d", 65),                // a bare id one past 64, still refused
		"",
	}
	for name := range tables {
		for _, key := range admitted {
			if err := stored(name, key); err != nil {
				t.Errorf("after 0050, %s refused %q (%d chars): %v", name, key, len(key), err)
			}
		}
		for _, key := range refused {
			if err := stored(name, key); !refusedBy(err, name) {
				t.Errorf("after 0050, %s did not refuse %q (%d chars) by its key CHECK: %v", name, key, len(key), err)
			}
		}
	}
}
