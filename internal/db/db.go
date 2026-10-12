package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StatementTimeout is injected into every connection this package opens.
//
// 🔴 IT IS SET HERE RATHER THAN LEFT TO THE SERVER'S DEFAULT (which is "no
// limit"), because a query with no bound is indistinguishable from a hung
// process at every layer above it. A caller that genuinely needs longer — the
// migration runner's own CREATE INDEX, say — must say so explicitly on its own
// connection rather than raise this for everyone.
const StatementTimeout = 10 * time.Second

// Connect opens a pool against dsn with muster's connection settings applied,
// and verifies it can actually reach the server before returning.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, set := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !set {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", StatementTimeout.Milliseconds())
	}
	if _, set := cfg.ConnConfig.RuntimeParams["application_name"]; !set {
		cfg.ConnConfig.RuntimeParams["application_name"] = "muster"
	}
	// Row-level security: every acquire acts as AppRole with the context's
	// principal; every release clears it. See scope.go.
	cfg.PrepareConn = prepareConn
	cfg.AfterRelease = afterRelease
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// 🔴 PING BEFORE RETURNING. pgxpool.New is LAZY — it returns a healthy-looking
	// pool against a server that does not exist, and the failure then surfaces at
	// the first query, attributed to whatever happened to run it.
	//
	// It pings as the MIGRATOR (the login role, no row scope): the pool may be
	// connecting to a database 0007 has not reached yet, where AppRole does not
	// exist, and a ping reads no rows.
	if err := pool.Ping(asMigrator(ctx)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
