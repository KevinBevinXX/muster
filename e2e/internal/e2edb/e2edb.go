// Package e2edb is the ONE guard every e2e fixture command runs before it
// touches a database: the database must be an e2e one, named "…_e2e".
//
// 🔴 IT ASKS PGX WHICH DATABASE THE DSN NAMES, NEVER THE URL'S PATH. pgx
// honours `?dbname=` and `?database=` over the path, and a key=value DSN has no
// path at all, so a path check passes `postgres://…/x_e2e?dbname=postgres` and
// then connects to postgres. pgconn.ParseConfig is the parser pgx's own
// connect runs (db.Connect and pgx.Connect both go through it), so the name
// checked here is the name connected to.
package e2edb

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Suffix is what an e2e database's name ends in.
const Suffix = "_e2e"

// Check is nil only when dsn names a database whose name ends in Suffix, as
// pgx resolves the name.
func Check(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if !strings.HasSuffix(cfg.Database, Suffix) {
		return fmt.Errorf("refusing database %q: an e2e fixture only touches a database whose name ends in %s", cfg.Database, Suffix)
	}
	return nil
}
