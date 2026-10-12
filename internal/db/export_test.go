package db

import "context"

// AsMigratorForTest exposes the migration runner's context to this package's
// external tests (package db_test). The migration and schema tests operate as
// the DATABASE OWNER — DDL, CREATE DATABASE/ROLE/SCHEMA, raw fixture rows for
// any owner — which is exactly what the migrator context is: the login role,
// muster.scope = 'all' — PLUS muster.uid = 1, so a raw fixture INSERT that names
// no owner_id still lands as the operator, as it did before 0007 made the
// default the principal's id. Application code cannot reach it (unexported, and
// this file compiles only into this package's test binary).
func AsMigratorForTest(ctx context.Context) context.Context {
	return context.WithValue(ctx, migratorKey{}, "1")
}

// AfterReleaseForTest is the pool's AfterRelease hook, for the test that a
// released connection carries nobody's id.
var AfterReleaseForTest = afterRelease
