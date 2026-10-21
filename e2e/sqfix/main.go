// Command sqfix is the send-queue browser spec's fixture (e2e/tests/send-queue.spec.ts):
// the two states of a queued message the spec cannot reach through a real
// route in a few seconds — a message queued days ago, and a message whose
// sender stopped mid-send.
//
//		DATABASE_URL=…_e2e go run ./e2e/sqfix age <host> <body> <seconds>
//		DATABASE_URL=…_e2e go run ./e2e/sqfix strand <host> <body>
//		DATABASE_URL=…_e2e go run ./e2e/sqfix route <host> <body>
//		DATABASE_URL=…_e2e go run ./e2e/sqfix lapse <host> <body>
//
//	  - age: moves the QUEUED message <body> for <host> back by <seconds> — its
//	    created_at and the sign-in time it was queued under, together, so it was
//	    authorized when it was queued. The dispatcher's own delivery then decides
//	    what that age means (internal/api sendQueueAuthorization).
//	  - strand: marks the QUEUED message <body> for <host> as being sent by a pod
//	    that has stopped: claimed, its lease already run out. The dispatcher's
//	    own Recover then fails it as "may have been delivered" on its next pass.
//	  - route: marks the QUEUED message <body> for <host> as ROUTED, to a route
//	    that does not exist (routeFixture). The dispatcher's own route gate then
//	    fails it as "its route could not be read" — a failed routed row.
//	  - lapse: runs the edit hold on the HELD message <body> for <host> out now
//	    (held_until a second ago, on the database's clock) — what ten minutes of
//	    an abandoned edit does.
//
// Each refuses unless exactly one row matched: a fixture that silently moved
// nothing would let the spec test a state it never built.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes as the login role with row-level security's scope set to all.
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

// routeFixture is the route a `route` row is marked with: well-formed
// (session_routes' rt_ + 24 hex), and no route's.
const routeFixture = "rt_000000000000000000000e2e"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sqfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) < 3 {
		return fmt.Errorf("usage: sqfix age <host> <body> <seconds> | strand <host> <body> | route <host> <body> | lapse <host> <body>")
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
	host, body := args[1], args[2]
	var q string
	var qargs []any
	switch args[0] {
	case "age":
		if len(args) != 4 {
			return fmt.Errorf("usage: age <host> <body> <seconds>")
		}
		secs, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("age: %q is not a positive number of seconds", args[3])
		}
		q = `UPDATE send_queue SET created_at = created_at - $3 * interval '1 second', stepup_at = stepup_at - $3 * interval '1 second'
			WHERE mirror_host = $1 AND body = $2 AND state = 'queued'`
		qargs = []any{host, body, secs}
	case "strand":
		q = `UPDATE send_queue SET state = 'sending', claimed_at = now(), claimed_by = 'e2e-stopped-pod',
			lease_until = now() - interval '1 second' WHERE mirror_host = $1 AND body = $2 AND state = 'queued'`
		qargs = []any{host, body}
	case "route":
		q = `UPDATE send_queue SET route_id = $3 WHERE mirror_host = $1 AND body = $2 AND state = 'queued'`
		qargs = []any{host, body, routeFixture}
	case "lapse":
		q = `UPDATE send_queue SET held_until = now() - interval '1 second'
			WHERE mirror_host = $1 AND body = $2 AND state = 'queued' AND held_until > now()`
		qargs = []any{host, body}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	tag, err := conn.Exec(ctx, q, qargs...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%s: %d queued rows of %q for %s matched; want exactly 1", args[0], tag.RowsAffected(), body, host)
	}
	return nil
}
