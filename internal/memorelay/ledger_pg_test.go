package memorelay

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func ledgerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t)) // skips, or fails, per MUSTER_TEST_REQUIRE_DB
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM audit_log WHERE action = $1`, AuditAction)
	})
	return pool
}

// TestThePGLedgerRemembersWhatEachAgentWasSent runs the real SQL under the real row-level
// security: what the operator's relay records, a NEW ledger over the same database reads back,
// per agent and per (scope, id).
func TestThePGLedgerRemembersWhatEachAgentWasSent(t *testing.T) {
	pool := ledgerPool(t)
	operator := authz.Operator(context.Background())
	ada, bob := agents.Agent{ID: 9001, Name: "ada"}, agents.Agent{ID: 9002, Name: "bob"}

	l := NewPGLedger(pool)
	if got, err := l.Delivered(operator, ada.ID); err != nil || len(got) != 0 {
		t.Fatalf("before anything is recorded: %v, %v; want empty", got, err)
	}
	for _, r := range []struct {
		a       agents.Agent
		scope   string
		ids     []int64
		outcome string
	}{
		{ada, "widgets", []int64{7, 9}, OutcomeDelivered},
		{ada, "gadgets", []int64{7}, OutcomeUnconfirmed},
		{bob, "widgets", []int64{11}, OutcomeDelivered},
	} {
		if err := l.Record(operator, r.a, r.scope, r.ids, r.outcome); err != nil {
			t.Fatal(err)
		}
	}

	fresh := NewPGLedger(pool) // a restarted process
	got, err := fresh.Delivered(operator, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Key]bool{{Scope: "widgets", ID: 7}: true, {Scope: "widgets", ID: 9}: true, {Scope: "gadgets", ID: 7}: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ada's record = %v, want %v (bob's memo 11 must not be in it)", got, want)
	}
	if got, _ := fresh.Delivered(operator, bob.ID); !reflect.DeepEqual(got, map[Key]bool{{Scope: "widgets", ID: 11}: true}) {
		t.Fatalf("bob's record = %v, want memo 11 alone", got)
	}

	// The row is the operator's own audit entry, attributed and readable as one.
	var userID int64
	var via, object string
	if err := pool.QueryRow(authz.System(context.Background()),
		`SELECT user_id, via, object FROM audit_log WHERE action = $1 AND detail->>'agent' = 'bob'`, AuditAction).
		Scan(&userID, &via, &object); err != nil {
		t.Fatal(err)
	}
	if userID != authz.OperatorUserID || via != string(authz.ViaOperator) || object != "agent:9002" {
		t.Fatalf("bob's row is user %d via %q object %q; want the operator's, via operator, object agent:9002", userID, via, object)
	}
}

// TestAnotherUserCannotReadTheOperatorsDeliveryRecord: row-level security, not a WHERE clause,
// keeps the record — which names the operator's scopes — from every other user of the deployment.
func TestAnotherUserCannotReadTheOperatorsDeliveryRecord(t *testing.T) {
	pool := ledgerPool(t)
	sys := authz.System(context.Background())
	var member int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', 'memo') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(sys, `DELETE FROM users WHERE id = $1`, member) })

	l := NewPGLedger(pool)
	ada := agents.Agent{ID: 9101, Name: "ada"}
	if err := l.Record(authz.Operator(context.Background()), ada, "widgets", []int64{7}, OutcomeDelivered); err != nil {
		t.Fatal(err)
	}
	// Positive control: the operator reads it.
	if got, err := l.Delivered(authz.Operator(context.Background()), ada.ID); err != nil || len(got) != 1 {
		t.Fatalf("control: the operator reads %v, %v; want the one memo", got, err)
	}
	asMember := authz.WithPrincipal(context.Background(), authz.Principal{UserID: member, Role: authz.RoleMember, Via: authz.ViaSession})
	if got, err := l.Delivered(asMember, ada.ID); err != nil || len(got) != 0 {
		t.Fatalf("a member reads %v, %v of the operator's record; want nothing", got, err)
	}
	// And nobody at all — a context with no principal — can neither read nor record.
	if got, err := l.Delivered(context.Background(), ada.ID); err != nil || len(got) != 0 {
		t.Fatalf("no principal reads %v, %v; want nothing", got, err)
	}
	if err := l.Record(context.Background(), ada, "widgets", []int64{8}, OutcomeDelivered); err == nil {
		t.Fatal("a delivery was recorded with no acting user")
	}
}

// TestARowTheLedgerCannotParseIsSkippedNotFatal: one unreadable row must not stop every delivery
// to its agent — and must not hide the readable rows beside it.
func TestARowTheLedgerCannotParseIsSkippedNotFatal(t *testing.T) {
	pool := ledgerPool(t)
	operator := authz.Operator(context.Background())
	ada := agents.Agent{ID: 9201, Name: "ada"}
	l := NewPGLedger(pool)
	if err := l.Record(operator, ada, "widgets", []int64{7}, OutcomeDelivered); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(operator, `INSERT INTO audit_log (user_id, via, action, object, detail)
		VALUES ($1, 'operator', $2, $3, '{"scope":"widgets","memos":"seven"}'::jsonb)`,
		authz.OperatorUserID, AuditAction, auditObject(ada.ID)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Delivered(operator, ada.ID)
	if err != nil || !reflect.DeepEqual(got, map[Key]bool{{Scope: "widgets", ID: 7}: true}) {
		t.Fatalf("got %v, %v; want memo 7 and no error", got, err)
	}
}
