// Command heldfix is the held-kickoff browser spec's fixture
// (e2e/tests/kickoff-wait.spec.ts): an agent whose first turn the kickoff
// deliverer is holding for its github connector grant — the row exactly as
// internal/agentkickoff leaves it (owed, not kicked off, kickoff_error opening
// with agents.KickoffAwaitingGrantReason). Reaching that state through a real
// dispatch needs a ccd answering /healthz at the agent's address, which the
// e2e noop driver does not have; the hold itself is pinned against the real
// store and deliverer in internal/agentkickoff (TestAHeldFirstTurnIsDeliveredOnceAfterTheGrant).
//
//	DATABASE_URL=…_e2e go run ./e2e/heldfix create <name>
//	DATABASE_URL=…_e2e go run ./e2e/heldfix delete <name>
//
// Each refuses unless exactly one row was written.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/agents"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "heldfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: heldfix create <name> | delete <name>")
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
	name := args[1]
	var q string
	var qargs []any
	switch args[0] {
	case "create":
		// Owner 1 is the seeded operator the spec signs in as.
		q = `INSERT INTO agents (name, namespace, display_name, repo, status, kind, cc_account, pending_note,
			hooks_token, kickoff_error, owner_id)
			VALUES ($1, 'agents', $1, 'example-org/shoal-map', 'running', $2, 'alpha', 'map the shoal-map build',
			'e2e-held-token-not-a-real-one', $3, 1)`
		qargs = []any{name, agents.KindClaudeCode, agents.KickoffAwaitingGrantReason +
			": the repository example-org/shoal-map could not be cloned"}
	case "delete":
		q = `DELETE FROM agents WHERE name = $1`
		qargs = []any{name}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	tag, err := conn.Exec(ctx, q, qargs...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%s %s: %d rows written; want exactly 1", args[0], name, tag.RowsAffected())
	}
	return nil
}
