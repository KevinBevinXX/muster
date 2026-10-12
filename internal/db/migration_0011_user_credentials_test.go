package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// freshAt is a scratch database migrated to exactly version v.
func freshAt(ctx context.Context, t *testing.T, name string, v int) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, name, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applyThrough(ctx, t, pool, v)
	return pool
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMigration0011MakesTheMarksPerOwnerAndKeepsTheEnvironmentPoolsMarks: a
// database at version 9 holding marks on the environment pool's accounts
// arrives at 0011 with those marks owned by user 1 (the admin, whose pool it
// is) — none lost — and from then on an account is (owner, name): a member may
// mark their own "work" beside the admin's, while a second mark on the same
// (owner, name) is still refused. Re-running the body changes nothing.
func TestMigration0011MakesTheMarksPerOwnerAndKeepsTheEnvironmentPoolsMarks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0011_marks", 9)
	mustExec(ctx, t, pool, `INSERT INTO cc_account_marks (account, rate_limited_detail) VALUES ('work', 'env pool'), ('personal-2', 'env pool')`)
	member := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)

	applyThroughFrom(ctx, t, pool, 11)

	var owner1 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cc_account_marks WHERE owner_id = 1 AND rate_limited_detail = 'env pool'`).Scan(&owner1); err != nil {
		t.Fatal(err)
	}
	if owner1 != 2 || countRows(ctx, t, pool, "cc_account_marks") != 2 {
		t.Fatalf("after 0011: %d of the environment pool's marks are user 1's (of %d rows); want 2 of 2",
			owner1, countRows(ctx, t, pool, "cc_account_marks"))
	}
	mustExec(ctx, t, pool, `INSERT INTO cc_account_marks (owner_id, account, rate_limited_detail) VALUES ($1, 'work', 'member')`, member)
	if _, err := pool.Exec(ctx, `INSERT INTO cc_account_marks (owner_id, account) VALUES (1, 'work')`); sqlState(err) != "23505" {
		t.Fatalf("a second mark row for (1, work): %v; want a unique violation (23505)", err)
	}

	// The body is idempotent (--rls-on re-applies it).
	body := readMigration(t, "0011_user_credentials.sql")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-running 0011: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "cc_account_marks"); n != 3 {
		t.Fatalf("after a re-run: %d mark rows, want 3", n)
	}
	var pk string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'public.cc_account_marks'::regclass AND contype = 'p'`).Scan(&pk); err != nil {
		t.Fatal(err)
	}
	if pk != "PRIMARY KEY (owner_id, account)" {
		t.Fatalf("cc_account_marks key = %q, want PRIMARY KEY (owner_id, account)", pk)
	}
}

// TestUserCredentialsRowsAreShapedByKind: the table's own checks — an
// OpenRouter row is named 'default' (one per user); a Claude row's name follows
// the account-name rule; one row per (owner, kind, name).
func TestUserCredentialsRowsAreShapedByKind(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0011_shape")
	ok := []string{
		`INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'work', '\x00', 'f')`,
		`INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'openrouter', 'default', '\x00', 'f')`,
	}
	for _, q := range ok {
		mustExec(ctx, t, pool, q)
	}
	for name, q := range map[string]string{
		"openrouter not named default":   `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'openrouter', 'two', '\x00', 'f')`,
		"a second openrouter key":        `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'openrouter', 'default', '\x00', 'f')`,
		"a bad account name":             `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'Work', '\x00', 'f')`,
		"an unknown kind":                `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'gemini', 'x', '\x00', 'f')`,
		"a second claude 'work' for one": `INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint) VALUES (1, 'claude', 'work', '\x00', 'f')`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: accepted", name)
		} else if s := sqlState(err); s != "23514" && s != "23505" {
			t.Errorf("%s: %v; want a check (23514) or unique (23505) violation", name, err)
		}
	}
}

// TestTheRollbackQueryFindsExactlyTheMembersLiveAgents runs the query 0011's
// rollback note tells the operator to run (read OUT of the migration file, so
// the note cannot drift from what is tested): it lists every enabled member —
// with each live agent, or alone when they have none, so they still get
// disabled — and nothing of the admin's, nothing stopped, nothing of a member
// already disabled. Each agent row carries the namespace and the Deployment
// name step 3 scales (one Deployment, never --all: in shared-namespace mode
// --all would stop the admin's agents too — round-2 audit on #69).
func TestTheRollbackQueryFindsExactlyTheMembersLiveAgents(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0011_rollback_query")
	body := readMigration(t, "0011_user_credentials.sql")
	i, j := strings.Index(body, "-- BEGIN rollback-member-agents\n"), strings.Index(body, "-- END rollback-member-agents")
	if i < 0 || j < i {
		t.Fatal("0011 has no rollback-member-agents block")
	}
	var q strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(body[i+len("-- BEGIN rollback-member-agents\n"):j]), "\n") {
		q.WriteString(strings.TrimPrefix(line, "--   ") + "\n")
	}
	a := scanID(ctx, t, pool, `INSERT INTO users (role, email) VALUES ('member', 'a@example.test') RETURNING id`)
	b := scanID(ctx, t, pool, `INSERT INTO users (role, email) VALUES ('member', 'b@example.test') RETURNING id`)
	c := scanID(ctx, t, pool, `INSERT INTO users (role, email, disabled_at) VALUES ('member', 'c@example.test', now()) RETURNING id`)
	mk := func(owner int64, name, status string) {
		mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status) VALUES ($1, $2, $3, $4)`, owner, name, "ns-"+name, status)
	}
	mk(1, "rq-admin-live", "running")
	mk(a, "rq-a-live", "running")
	mk(a, "rq-a-prov", "provisioning")
	mk(a, "rq-a-stopped", "stopped")
	mk(c, "rq-c-live", "running")

	rows, err := pool.Query(ctx, q.String())
	if err != nil {
		t.Fatalf("the rollback query does not run: %v\n%s", err, q.String())
	}
	// Closed on every path: a Fatalf with the rows open would hold the pool's
	// connection and hang the cleanup.
	defer rows.Close()
	var got []string
	for rows.Next() {
		var uid int64
		var email string
		var agentID *int64
		var kind, status, ns, deployment *string
		if err := rows.Scan(&uid, &email, &agentID, &kind, &status, &ns, &deployment); err != nil {
			t.Fatal(err)
		}
		cols := rows.FieldDescriptions()
		if string(cols[5].Name) != "namespace" || string(cols[6].Name) != "deployment" {
			t.Fatalf("the rollback query's last columns are %q, %q; the note's step 3 reads namespace and deployment",
				cols[5].Name, cols[6].Name)
		}
		n := "-"
		if deployment != nil {
			n = *deployment + "@" + *ns
		}
		got = append(got, fmt.Sprintf("%d:%s", uid, n))
	}
	rows.Close()
	want := []string{
		fmt.Sprintf("%d:rq-a-live@ns-rq-a-live", a),
		fmt.Sprintf("%d:rq-a-prov@ns-rq-a-prov", a),
		fmt.Sprintf("%d:-", b),
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback query rows = %v\nwant %v", got, want)
	}
}
