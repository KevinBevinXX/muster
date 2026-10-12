package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// AsOperator returns ctx acting as the operator (user 1, admin, NOT scope-all):
// the principal every Postgres-backed test acts as by default.
//
// 🔴 ROW-LEVEL SECURITY (migration 0007) MAKES A PRINCIPAL MANDATORY. Every
// pool internal/db opens switches to the application role and scopes each
// query to the context's principal; a test context with NONE sees no rows and
// cannot insert one. Every row these suites create belongs to user 1, exactly
// as it does in production today, so acting as the operator keeps each test
// asserting what it asserted before. A test that wants SOMEBODY ELSE (another
// user, or authz.System) builds that principal itself.
//
// ⚠ IT IS authz.Operator, and this function is therefore a site in
// internal/authz's Operator ledger. That is safe only because dbtest is a
// TEST-ONLY package: TestOnlyTestFilesImportDBTest in internal/authz fails if
// any non-test file in the module imports it.
func AsOperator(ctx context.Context) context.Context { return authz.Operator(ctx) }

// OwnerExec runs sql on a FRESH connection as the database's LOGIN role — not
// through internal/db's pool, which switches every connection to the
// application role. It is for fixture RESETS the application role is
// deliberately not allowed to do: TRUNCATE (which ignores row-level security,
// so the application role is not granted it) and RESTART IDENTITY (which needs
// the sequences' owner).
func OwnerExec(t testing.TB, dsn, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dbtest.OwnerExec: connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("dbtest.OwnerExec: %v", err)
	}
}
