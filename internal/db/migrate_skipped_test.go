package db_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestMigrateRefusesToSkipAMigrationBelowTheAppliedVersion (round-2 audit on
// #69): the runner applies only versions above max(version), so a migration
// that reaches a database AFTER a higher one was applied — #67's 0010 behind
// this PR's 0011, the case that prompted it — would be skipped forever. It must
// refuse instead, naming exactly the skipped file and how to apply it.
//
// 🔴 THE GAP IS BUILT FROM WHATEVER FILES THIS TREE EMBEDS (round-3 audit): a
// fully migrated database loses the row of its SECOND-NEWEST embedded
// migration. Hard-coding "11 applied, 10 absent" broke the moment #67's 0010
// joined the tree, and any 0012 would break it again.
//
//   - The control: an ordinary, fully migrated database boots exactly as before.
//   - The gap: Migrate refuses (ErrSkippedMigration) naming that one file and
//     its single-transaction command, names no other file, and records nothing.
//   - Recovery: what the command does (the file and its row, one transaction)
//     makes the next boot succeed.
func TestMigrateRefusesToSkipAMigrationBelowTheAppliedVersion(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_skipped_migration")
	embedded, err := db.EmbeddedMigrationsForTest()
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) < 2 {
		t.Fatalf("fixture: %d embedded migrations; the gap needs two", len(embedded))
	}
	newest, gap := embedded[len(embedded)-1], embedded[len(embedded)-2]

	var logs bytes.Buffer
	if err := db.Migrate(ctx, pool, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("control: a normal database refused to migrate: %v", err)
	}
	if !strings.Contains(logs.String(), fmt.Sprintf("schema is at version %d; nothing to apply", newest.Version)) {
		t.Fatalf("control: a re-run of a fully migrated database did something: %s", logs.String())
	}

	mustExec(ctx, t, pool, `DELETE FROM schema_migrations WHERE version = $1`, gap.Version)
	before := countRows(ctx, t, pool, "schema_migrations")
	err = db.Migrate(ctx, pool, log.New(io.Discard, "", 0))
	if !errors.Is(err, db.ErrSkippedMigration) {
		t.Fatalf("Migrate over a database missing %s below %s: %v; want ErrSkippedMigration", gap.Name, newest.Name, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf(": %s (the database is at version %d)", gap.Name, newest.Version)) {
		t.Errorf("the refusal does not name exactly %s at version %d: %s", gap.Name, newest.Version, msg)
	}
	if !strings.Contains(msg, db.ApplyByHandCommand(gap.Version, gap.Name)) {
		t.Errorf("the refusal does not give %s's command: %s", gap.Name, msg)
	}
	if !strings.Contains(msg, "From a checkout at the commit the running image was built from") {
		t.Errorf("the refusal does not say which checkout to run it from: %s", msg)
	}
	for _, m := range embedded {
		if m != gap && strings.Contains(msg, m.Name) {
			t.Errorf("the refusal names %s, which is not missing: %s", m.Name, msg)
		}
	}
	if after := countRows(ctx, t, pool, "schema_migrations"); after != before {
		t.Fatalf("a refused Migrate recorded %d migration(s)", after-before)
	}

	// Recovery: the file and its row in ONE transaction (what the command does;
	// every migration is idempotent), then the server boots.
	body := readMigration(t, gap.Name)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-applying %s: %v", gap.Name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, gap.Version, gap.Name); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("after applying %s by hand the server still refuses: %v", gap.Name, err)
	}
}

// TestTheSkippedMigrationRefusalGivesTheSingleTransactionCommand pins the
// operator's command WHOLE (round-3 audit on #69; the in-cluster stdin form
// since round 4): -1 makes the file and its
// schema_migrations row one transaction (without it the files' SET LOCAL
// lock_timeout is lost and a mid-file failure half-applies the file), and
// ON_ERROR_STOP aborts on the first error. This exact form was run against
// PostgreSQL 16 — see ApplyByHandCommand.
func TestTheSkippedMigrationRefusalGivesTheSingleTransactionCommand(t *testing.T) {
	const want = `kubectl -n muster exec -i deploy/muster-postgres -- psql -U muster -d muster -v ON_ERROR_STOP=1 -1 -f - -c "INSERT INTO schema_migrations (version, name) VALUES (10, '0010_sessions_not_before.sql')" < internal/db/migrations/0010_sessions_not_before.sql`
	if got := db.ApplyByHandCommand(10, "0010_sessions_not_before.sql"); got != want {
		t.Fatalf("ApplyByHandCommand =\n  %s\nwant\n  %s", got, want)
	}
}
