package db_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// Migration 0003's guards, against the real migrated schema, with raw SQL — the
// claims are about the DATABASE, whatever binary writes to it.

// uniq is a per-call suffix so a leftover row from an earlier run cannot answer
// an assertion.
func uniq() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

func execErr(ctx context.Context, conn *pgxpool.Conn, sql string, args ...any) error {
	_, err := conn.Exec(ctx, sql, args...)
	return err
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// TestAnInsertNamingNoKindIsAGatewayAgentWithNoAccount: every INSERT a pre-kinds
// binary makes names no kind, and must land as exactly what it always was.
func TestAnInsertNamingNoKindIsAGatewayAgentWithNoAccount(t *testing.T) {
	ctx, conn := pinnedConn(t)
	name := "kindless-" + uniq()
	t.Cleanup(func() { clearName(db.AsMigratorForTest(context.Background()), t, conn, name) })
	id := insertAgentRow(ctx, t, conn, name)
	var kind, account string
	if err := conn.QueryRow(ctx, `SELECT kind, cc_account FROM agents WHERE id=$1`, id).Scan(&kind, &account); err != nil {
		t.Fatal(err)
	}
	if kind != "gateway" || account != "" {
		t.Fatalf("kind %q account %q, want gateway and none", kind, account)
	}
}

// TestTheKindAndAccountMustAgree: a claude-code row must name an account, and no
// other row may; an unknown kind is refused. Each is a CHECK violation (23514).
func TestTheKindAndAccountMustAgree(t *testing.T) {
	ctx, conn := pinnedConn(t)
	ins := `INSERT INTO agents (name, namespace, status, kind, cc_account) VALUES ($1, 'ns', 'provisioning', $2, $3)`
	for _, c := range []struct{ kind, account, constraint string }{
		{"claude-code", "", "agents_cc_account_matches_kind"},
		{"gateway", "work", "agents_cc_account_matches_kind"},
		{"teleport", "", "agents_kind_check"},
	} {
		name := "mismatch-" + uniq()
		err := execErr(ctx, conn, ins, name, c.kind, c.account)
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("kind %q account %q: err = %v, want a %s CHECK violation", c.kind, c.account, err, c.constraint)
			clearName(db.AsMigratorForTest(context.Background()), t, conn, name)
		}
	}
	// The control: the agreeing pair is accepted.
	name := "agree-" + uniq()
	t.Cleanup(func() { clearName(db.AsMigratorForTest(context.Background()), t, conn, name) })
	if err := execErr(ctx, conn, ins, name, "claude-code", "work"); err != nil {
		t.Fatalf("control: a claude-code row with an account was refused: %v", err)
	}
}

// TestKindAndAccountAreImmutableAfterInsert: an agent keeps its kind and its
// Claude account for life — an UPDATE of either raises (P0001, the trigger's own
// RAISE), while an UPDATE of anything else still works.
func TestKindAndAccountAreImmutableAfterInsert(t *testing.T) {
	ctx, conn := pinnedConn(t)
	name := "sticky-" + uniq()
	t.Cleanup(func() { clearName(db.AsMigratorForTest(context.Background()), t, conn, name) })
	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO agents (name, namespace, status, kind, cc_account)
		VALUES ($1, 'ns', 'provisioning', 'claude-code', 'work') RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, upd := range []string{
		`UPDATE agents SET cc_account='personal' WHERE id=$1`,
		`UPDATE agents SET kind='gateway', cc_account='' WHERE id=$1`,
	} {
		err := execErr(ctx, conn, upd, id)
		if sqlState(err) != "P0001" || !strings.Contains(err.Error(), "fixed at creation") {
			t.Errorf("%s: err = %v, want the trigger's P0001 refusal", upd, err)
		}
	}
	if err := execErr(ctx, conn, `UPDATE agents SET status='running', cc_account='work' WHERE id=$1`, id); err != nil {
		t.Fatalf("control: an update that leaves kind/account unchanged was refused: %v", err)
	}
	var account string
	if err := conn.QueryRow(ctx, `SELECT cc_account FROM agents WHERE id=$1`, id).Scan(&account); err != nil || account != "work" {
		t.Fatalf("account = %q, %v; want work", account, err)
	}
}
