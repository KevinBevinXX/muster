// Command pluginfix is the plugins browser spec's fixture (e2e/tests/plugins.spec.ts):
// the few rows that spec needs and the e2e harness cannot make through a real
// route, because the e2e harness runs no agent runtime.
//
//	DATABASE_URL=…_e2e go run ./e2e/pluginfix chief <owner-id> <name> <token>   → prints the agent id
//	DATABASE_URL=…_e2e go run ./e2e/pluginfix deliver <message-id> <agent-id>
//	DATABASE_URL=…_e2e go run ./e2e/pluginfix lapse <draft-id>
//	DATABASE_URL=…_e2e go run ./e2e/pluginfix cleanup <owner-id> <name>
//
//   - chief: an agent labelled "chief", running, holding token — the owner's
//     chief, whose own token the spec then calls /mcp with. It REFUSES when the
//     owner already has an agent labelled chief: the lowest id is the chief, so
//     a leftover one would silently take the role and the spec would test it.
//   - deliver: settles a queued plugin message as delivered to that agent, which
//     is what a completed delivery turn records (internal/plugins.RecordDelivery).
//     The delivery itself — the fenced turn through the agent gateway — is the Go
//     seam tests' (plugins_seam_pg_test.go, plugin_drafts_pg_test.go), which run a
//     fake runtime the browser suite has no place for.
//   - lapse: moves a claimed draft's claim and lease ten minutes back, so its
//     two-minute lease has lapsed without waiting for it.
//   - cleanup: removes the chief and the owner's plugins (their messages and
//     drafts cascade), so later specs on the shared database see neither.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e", like e2e/seed:
// it writes as the login role with row-level security's scope set to all, which
// nothing outside a fixture may do.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pluginfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_e2e") {
		return fmt.Errorf("refusing: DATABASE_URL must name a database ending in _e2e")
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: pluginfix chief|deliver|lapse|cleanup …")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT set_config('muster.scope', 'all', false)`); err != nil {
		return err
	}
	num := func(i int) (int64, error) {
		if len(args) <= i {
			return 0, fmt.Errorf("%s: missing argument %d", args[0], i)
		}
		return strconv.ParseInt(args[i], 10, 64)
	}
	switch args[0] {
	case "chief":
		owner, err := num(1)
		if err != nil || len(args) != 4 {
			return fmt.Errorf("usage: chief <owner-id> <name> <token>")
		}
		var other string
		err = conn.QueryRow(ctx, `SELECT name FROM agents WHERE owner_id = $1 AND lower(trim(display_name)) = 'chief' LIMIT 1`,
			owner).Scan(&other)
		if err == nil {
			return fmt.Errorf("owner %d already has an agent labelled chief (%s); it would be the chief instead", owner, other)
		}
		if err != pgx.ErrNoRows {
			return err
		}
		var id int64
		if err := conn.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, hooks_token, kicked_off)
			VALUES ($1, $2, $3, 'chief', 'running', $4, true) RETURNING id`, owner, args[2], "devpod-"+args[2], args[3]).Scan(&id); err != nil {
			return err
		}
		fmt.Println(id)
	case "deliver":
		msg, err := num(1)
		if err != nil {
			return err
		}
		agent, err := num(2)
		if err != nil {
			return err
		}
		tag, err := conn.Exec(ctx, `UPDATE plugin_messages SET status = 'delivered', delivered_at = now(), agent_id = $2,
			attempts = attempts + 1, last_attempt_at = now() WHERE id = $1 AND status = 'queued'`, msg, agent)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("message %d is not queued", msg)
		}
	case "lapse":
		draft, err := num(1)
		if err != nil {
			return err
		}
		tag, err := conn.Exec(ctx, `UPDATE plugin_reply_drafts SET claimed_at = claimed_at - interval '10 minutes',
			lease_until = lease_until - interval '10 minutes' WHERE id = $1 AND claimed_at IS NOT NULL`, draft)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("draft %d is not claimed", draft)
		}
	case "cleanup":
		owner, err := num(1)
		if err != nil || len(args) != 3 {
			return fmt.Errorf("usage: cleanup <owner-id> <name>")
		}
		if _, err := conn.Exec(ctx, `DELETE FROM plugins WHERE owner_id = $1`, owner); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `DELETE FROM agents WHERE owner_id = $1 AND name = $2`, owner, args[2]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
