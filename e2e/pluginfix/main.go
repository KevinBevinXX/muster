// Command pluginfix is the plugins browser spec's fixture (e2e/tests/plugins.spec.ts):
// the few rows that spec needs and the e2e harness cannot make through a real
// route, because the e2e harness runs no agent runtime.
//
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix chief <owner-id> <name> <token>   → prints the agent id
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix delivering <message-id> <agent-id>
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix unconfirmed <message-id>
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix lapse <draft-id>
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix label <agent-id> <display-name>
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix rmagent <owner-id> <name>
//		DATABASE_URL=…_e2e go run ./e2e/pluginfix cleanup <owner-id> <name>
//
//	  - chief: an agent labelled "chief", running, holding token — the owner's
//	    chief, whose own token the spec then calls /mcp with. It REFUSES when the
//	    owner already has an agent labelled chief: the lowest id is the chief, so
//	    a leftover one would silently take the role and the spec would test it.
//	  - delivering: marks a queued plugin message as being delivered to that agent,
//	    which is what the delivery turn holds WHILE the chief acts on it
//	    (internal/plugins.BeginDelivery). 🔴 NOT `delivered`: the chief drafts its
//	    reply on the turn that delivers the message, and a fixture that pre-settled
//	    it as delivered would let the spec pass while that turn's state refused
//	    every draft (audit round 1). The turn itself — fenced, through the agent
//	    gateway — is the Go seam tests' (plugins_seam_pg_test.go,
//	    reply_draft_seam_pg_test.go), which run a fake runtime the browser suite
//	    has no place for.
//	  - unconfirmed: settles a message being delivered as unconfirmed — what a
//	    delivery turn cut off mid-way leaves, which the page's Retry is for.
//	  - lapse: moves a claimed draft's claim and lease ten minutes back, so its
//	    two-minute lease has lapsed without waiting for it.
//	  - label: sets an agent's display name — "retired" takes the chief role away
//	    (the chief IS the agent displayed "chief"), "chief" gives it back.
//	  - cleanup: removes the chief and the owner's plugins (their messages and
//	    drafts cascade), so later specs on the shared database see neither.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes as the login role with row-level security's scope set to all, which
// nothing outside a fixture may do.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pluginfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: pluginfix chief|agent|delivering|unconfirmed|lapse|label|rmagent|cleanup …")
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
	case "agent":
		// A running, NON-chief agent with its own token: the attention spec's asker.
		owner, err := num(1)
		if err != nil || len(args) != 4 {
			return fmt.Errorf("usage: agent <owner-id> <name> <token>")
		}
		var id int64
		if err := conn.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, hooks_token, kicked_off)
			VALUES ($1, $2, $3, $2, 'running', $4, true) RETURNING id`, owner, args[2], "devpod-"+args[2], args[3]).Scan(&id); err != nil {
			return err
		}
		fmt.Println(id)
	case "delivering":
		msg, err := num(1)
		if err != nil {
			return err
		}
		agent, err := num(2)
		if err != nil {
			return err
		}
		tag, err := conn.Exec(ctx, `UPDATE plugin_messages SET status = 'delivering', agent_id = $2,
			attempts = attempts + 1, last_attempt_at = now() WHERE id = $1 AND status = 'queued'`, msg, agent)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("message %d is not queued", msg)
		}
	case "unconfirmed":
		// Settles a message being delivered as `unconfirmed`, as a delivery turn
		// cut off mid-way does (internal/plugins RecordDelivery).
		msg, err := num(1)
		if err != nil {
			return err
		}
		tag, err := conn.Exec(ctx, `UPDATE plugin_messages SET status = 'unconfirmed', delivered_at = now()
			WHERE id = $1 AND status = 'delivering'`, msg)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("message %d is not being delivered", msg)
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
	case "label":
		agent, err := num(1)
		if err != nil || len(args) != 3 {
			return fmt.Errorf("usage: label <agent-id> <display-name>")
		}
		tag, err := conn.Exec(ctx, `UPDATE agents SET display_name = $2 WHERE id = $1`, agent, args[2])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("no agent %d", agent)
		}
	case "rmagent":
		// Deletes ONE agent and nothing else (cleanup also clears the owner's
		// plugins, which a spec on the shared server must not do).
		owner, err := num(1)
		if err != nil || len(args) != 3 {
			return fmt.Errorf("usage: rmagent <owner-id> <name>")
		}
		if _, err := conn.Exec(ctx, `DELETE FROM agents WHERE owner_id = $1 AND name = $2`, owner, args[2]); err != nil {
			return err
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
