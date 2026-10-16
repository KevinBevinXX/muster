// Command mkmember binds an existing GoTrue user to a NEW muster user row with
// the member role — the account a link invite (accounts PR 5) would create —
// so a browser spec can sign in as a member against the real GoTrue before the
// invite flow exists on this branch.
//
//	DATABASE_URL=…_e2e MUSTER_E2E_SUB=<gotrue uuid> MUSTER_E2E_EMAIL=… go run ./e2e/mkmember
//
// It prints the new user's id.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes the users table as the login role (users is scope-all-only under
// row-level security), which nothing outside a fixture may do.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkmember:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	sub, email := os.Getenv("MUSTER_E2E_SUB"), os.Getenv("MUSTER_E2E_EMAIL")
	if sub == "" || email == "" {
		return fmt.Errorf("MUSTER_E2E_SUB and MUSTER_E2E_EMAIL are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO users (gotrue_sub, email, display_name, role)
		VALUES ($1, $2, $3, 'member') RETURNING id`, sub, email, "e2e member").Scan(&id); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}
