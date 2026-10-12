// Command muster-migrate applies muster's migrations to the database named by
// DATABASE_URL and exits.
//
// 🔴 IT IS A CI AND DEVELOPMENT FIXTURE, NOT A DEPLOY STEP. The server migrates
// itself at boot.
//
// It also carries the ROLLBACK for migration 0007 (row-level security), and it
// ships in the image as /muster-migrate for exactly that:
//
//	/muster-migrate --rls-off   disable RLS + owner_id DEFAULT 1, so an image
//	                            built before accounts PR 3a can run against
//	                            this schema (internal/db/rollback/rls_off.sql)
//	/muster-migrate --rls-on    re-apply 0007 (undo --rls-off)
//
// Either flag does ONLY that — no migrations are applied in the same run. This exists so the TEMPLATE database that internal/dbtest
// copies can be migrated ONCE, before the test binaries start — otherwise the
// first Postgres-backed package to run pays the whole bootstrap under an
// advisory lock while every other package queues behind it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ZacxDev/muster/internal/db"
)

const budget = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "muster-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("muster-migrate", flag.ContinueOnError)
	rlsOff := fs.Bool("rls-off", false, "disable row-level security and restore owner_id DEFAULT 1 (rollback below accounts PR 3a), then exit")
	rlsOn := fs.Bool("rls-on", false, "re-apply migration 0007 (undo --rls-off), then exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *rlsOff && *rlsOn {
		return fmt.Errorf("--rls-off and --rls-on are mutually exclusive")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// 🔴 AN EMPTY DSN IS A REFUSAL, NOT A DEFAULT. Falling back to a local
		// server would let a CI step that forgot to set the variable migrate
		// something other than the database the tests will use, and report
		// success for it.
		return fmt.Errorf("DATABASE_URL is unset; refusing to guess a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch {
	case *rlsOff:
		if err := db.RLSOff(ctx, pool); err != nil {
			return err
		}
		fmt.Println("muster-migrate: row-level security is OFF and owner_id defaults to 1. " +
			"Re-enable with --rls-on once an image carrying migration 0007 is back.")
		return nil
	case *rlsOn:
		if err := db.RLSOn(ctx, pool); err != nil {
			return err
		}
		fmt.Println("muster-migrate: row-level security is ON (migration 0007 re-applied).")
		return nil
	}
	return db.Migrate(ctx, pool, log.New(os.Stdout, "", 0))
}
