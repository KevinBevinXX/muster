package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0035AdmitsARuntimeQualifiedHermesKey: at version 34 a Hermes
// conversation's key — "hermes:local-<64 hex>", 77 characters — is refused by
// each of the four session-key CHECKs (the control: the old 64-character
// bound is real, in every table); after 0035 it is stored in all four, and so
// is a Claude Code id exactly as before. What the wider bound must STILL
// refuse is checked in every table too: a bare Hermes id (only the qualified
// form is wider), a bare id one past 64, an empty or over-long Hermes id,
// another runtime's prefix, and plain junk.
func TestMigration0035AdmitsARuntimeQualifiedHermesKey(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0035_session_key", 34)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'key-agent', 'ns-key') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-key', 'Example inbox', 'pluginhash-key', 'example') RETURNING id`)
	message := scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id)
		VALUES (1, $1, 'ext-key', 'hello', 'delivering', $2) RETURNING id`, plugin, agent)

	// Each table's write of one key, and the CHECK that refuses a bad one.
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
			// One open route per message: each write is a FINISHED route, so
			// they never collide on session_routes_one_open.
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
	hex64 := strings.Repeat("0123456789abcdef", 4)
	hermes := "hermes:local-" + hex64 // 77 characters: the key of a 70-character Hermes id
	claude := "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	if len(hermes) != 77 || len("local-"+hex64) != 70 {
		t.Fatalf("instrument check: the Hermes fixture is %d characters (id %d), want 77 (70)", len(hermes), len("local-"+hex64))
	}

	n := 0
	stored := func(name, key string) error { n++; return tables[name].write(key, n) }
	refusedBy := func(err error, name string) bool {
		return err != nil && strings.Contains(err.Error(), tables[name].check)
	}

	for name := range tables {
		if err := stored(name, hermes); !refusedBy(err, name) {
			t.Fatalf("control: at version 34 %s stored a 77-character Hermes key (or refused it for another reason): %v", name, err)
		}
		if err := stored(name, claude); err != nil {
			t.Fatalf("control: at version 34 %s refused a Claude Code id: %v", name, err)
		}
	}

	applyThroughFrom(ctx, t, pool, 35)

	admitted := []string{
		hermes,
		// Another Claude Code id: agent_focus holds one row per key, and the
		// control stored this one's sibling already.
		"1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c",
		"hermes:" + strings.Repeat("a", 32),        // a rotated child's bare 32 hex
		"hermes:" + strings.Repeat("b", 128),       // the Hermes id's bound
		strings.Repeat("c", 64),                    // the Claude id's bound, unchanged
		"hermes:" + "x_-" + strings.Repeat("9", 9), // the id's own alphabet
	}
	refused := []string{
		"local-" + hex64,                     // a BARE Hermes id: only the qualified form is wider
		strings.Repeat("d", 65),              // a bare id one past 64
		"hermes:",                            // no id
		"hermes:" + strings.Repeat("e", 129), // one past the Hermes bound
		"hermes:-leading-dash",               // the id's first character
		"claude:" + claude,                   // another runtime's prefix
		"HERMES:" + "local-" + hex64,         // the prefix is exact
		"hermes:hermes:local-" + hex64[:8],   // one prefix only
		"hermes:local " + hex64[:8],          // a space
		"../etc",
		"",
	}
	for name := range tables {
		for _, key := range admitted {
			if err := stored(name, key); err != nil {
				t.Errorf("after 0035, %s refused %q (%d chars): %v", name, key, len(key), err)
			}
		}
		for _, key := range refused {
			if err := stored(name, key); !refusedBy(err, name) {
				t.Errorf("after 0035, %s did not refuse %q (%d chars) by its key CHECK: %v", name, key, len(key), err)
			}
		}
	}
}
