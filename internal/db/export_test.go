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

// EmbeddedMigrationForTest is one embedded migration's version and file name.
type EmbeddedMigrationForTest struct {
	Version int
	Name    string
}

// EmbeddedMigrationsForTest lists the embedded migrations, ascending — so a
// test can pick a version from whatever files this tree holds rather than
// hard-coding one that a later migration (or another branch's) shifts.
func EmbeddedMigrationsForTest() ([]EmbeddedMigrationForTest, error) {
	ms, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	out := make([]EmbeddedMigrationForTest, len(ms))
	for i, m := range ms {
		out[i] = EmbeddedMigrationForTest{Version: m.version, Name: m.name}
	}
	return out, nil
}
