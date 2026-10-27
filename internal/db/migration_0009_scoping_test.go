package db_test

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
)

// Migration 0009 — per-user scoping (accounts PR 3b): one reserved agent per
// owner, and row-level security on the user-keyed account tables.

// wantUserKeyedPolicies is every policy on the user-keyed tables, in full:
// "<table> <policy> <cmd> USING=<qual> CHECK=<with_check>", as pg_policies
// renders them. 🔴 THE WHOLE NORMALISED STRING, NOT A SUBSTRING: a policy
// that gained a third arm (an `OR true`) or lost its user arm must fail, and a
// substring check passes both.
var wantUserKeyedPolicies = []string{
	"api_tokens scope_all ALL USING=(current_setting('muster.scope'::text, true) = 'all'::text) CHECK=(current_setting('muster.scope'::text, true) = 'all'::text)",
	"api_tokens user_isolation ALL USING=((user_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint) OR (current_setting('muster.scope'::text, true) = 'all'::text)) CHECK=((user_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint) OR (current_setting('muster.scope'::text, true) = 'all'::text))",
	"audit_log scope_all ALL USING=(current_setting('muster.scope'::text, true) = 'all'::text) CHECK=(current_setting('muster.scope'::text, true) = 'all'::text)",
	"audit_log self_append INSERT USING= CHECK=((user_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint) OR (current_setting('muster.scope'::text, true) = 'all'::text))",
	"audit_log self_read SELECT USING=((user_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint) OR (current_setting('muster.scope'::text, true) = 'all'::text)) CHECK=",
	"invites scope_all ALL USING=(current_setting('muster.scope'::text, true) = 'all'::text) CHECK=(current_setting('muster.scope'::text, true) = 'all'::text)",
	"users scope_all ALL USING=(current_setting('muster.scope'::text, true) = 'all'::text) CHECK=(current_setting('muster.scope'::text, true) = 'all'::text)",
	"users self_read SELECT USING=((id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint) OR (current_setting('muster.scope'::text, true) = 'all'::text)) CHECK=",
}

func TestTheUserKeyedTablesCarryExactlyTheirPolicies(t *testing.T) {
	f := newRLSFixture(t, "ms_0009_policies")
	rows, err := f.pool.Query(f.mig, `SELECT tablename::text, policyname::text, cmd, permissive,
			coalesce(qual,''), coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1)`, userKeyedTables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var tbl, name, cmd, permissive, qual, check string
		if err := rows.Scan(&tbl, &name, &cmd, &permissive, &qual, &check); err != nil {
			t.Fatal(err)
		}
		if permissive != "PERMISSIVE" {
			t.Errorf("%s.%s is %s; every policy here is PERMISSIVE", tbl, name, permissive)
		}
		got = append(got, tbl+" "+name+" "+cmd+" USING="+qual+" CHECK="+check)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(wantUserKeyedPolicies, "\n") {
		t.Errorf("user-keyed policies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantUserKeyedPolicies, "\n"))
	}
}

// TestTheUserKeyedTablesAreScopedPerUser: behaviour behind the policies above.
// A member sees its own users row and nobody else's, cannot write any users
// row (its own included — no self-promotion to admin), sees no invite, and
// sees and writes only its own tokens and audit entries. System sees all.
func TestTheUserKeyedTablesAreScopedPerUser(t *testing.T) {
	f := newRLSFixture(t, "ms_0009_behaviour")
	two := asUser(f.base, f.two)
	sys := authz.System(f.base)
	hash := func(b byte) []byte {
		h := make([]byte, 32)
		for i := range h {
			h[i] = b
		}
		return h
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO invites (token_hash, created_by, expires_at) VALUES ($1, 1, now() + interval '1 day')`, []any{hash(1)}},
		{`INSERT INTO api_tokens (user_id, token_hash) VALUES (1, $1)`, []any{hash(2)}},
		{`INSERT INTO api_tokens (user_id, token_hash) VALUES ($1, $2)`, []any{f.two, hash(3)}},
		{`INSERT INTO audit_log (user_id, action) VALUES (1, 'one')`, nil},
		{`INSERT INTO audit_log (user_id, action) VALUES ($1, 'two')`, []any{f.two}},
	} {
		if _, err := f.pool.Exec(f.mig, q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}

	if n := count(t, two, f.pool, "users", ""); n != 1 {
		t.Errorf("user 2 sees %d users rows; want only its own", n)
	}
	if n := count(t, two, f.pool, "users", "WHERE id=1"); n != 0 {
		t.Errorf("user 2 sees the operator's users row")
	}
	if tag, err := f.pool.Exec(two, `UPDATE users SET role='admin' WHERE id=$1`, f.two); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("user 2 promoting itself: %v, %d rows; want 0 rows", err, tag.RowsAffected())
	}
	if tag, err := f.pool.Exec(two, `DELETE FROM users WHERE id=$1`, f.two); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("user 2 deleting its own row: %v, %d rows; want 0 rows", err, tag.RowsAffected())
	}
	if n := count(t, two, f.pool, "invites", ""); n != 0 {
		t.Errorf("user 2 sees %d invites; want none", n)
	}
	if n := count(t, two, f.pool, "api_tokens", ""); n != 1 {
		t.Errorf("user 2 sees %d tokens; want its own one", n)
	}
	if n := count(t, two, f.pool, "audit_log", ""); n != 1 {
		t.Errorf("user 2 sees %d audit entries; want its own one", n)
	}
	var pgErr *pgconn.PgError
	if _, err := f.pool.Exec(two, `INSERT INTO api_tokens (user_id, token_hash) VALUES (1, $1)`, hash(4)); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("user 2 minting a token for user 1: %v; want 42501", err)
	}
	if _, err := f.pool.Exec(two, `INSERT INTO audit_log (user_id, action) VALUES (1, 'forged')`); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("user 2 appending to user 1's audit trail: %v; want 42501", err)
	}
	if _, err := f.pool.Exec(two, `INSERT INTO audit_log (user_id, action) VALUES ($1, 'mine')`, f.two); err != nil {
		t.Errorf("user 2 appending its own audit entry: %v", err)
	}
	if tag, err := f.pool.Exec(two, `DELETE FROM audit_log WHERE user_id=$1`, f.two); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("user 2 pruning its own audit trail: %v, %d rows; want 0 (append-only for users)", err, tag.RowsAffected())
	}
	// Control: the same reads as System see every row, so the zeros above are
	// the policies, not empty tables.
	for tbl, min := range map[string]int{"users": 2, "invites": 1, "api_tokens": 2, "audit_log": 3} {
		if n := count(t, sys, f.pool, tbl, ""); n < min {
			t.Errorf("System sees %d rows in %s; want >= %d", n, tbl, min)
		}
	}
	// And the operator reads its own row (the role lookup's self-read arm).
	if n := count(t, asUser(f.base, 1), f.pool, "users", "WHERE id=1 AND role='admin'"); n != 1 {
		t.Errorf("user 1 cannot read its own admin row (%d)", n)
	}
}

// TestAnOwnerHoldsOneReservedChief: the partial unique index refuses a second
// reserved agent slug for the same owner, and lets two owners each have one.
func TestAnOwnerHoldsOneReservedChief(t *testing.T) {
	f := newRLSFixture(t, "ms_0009_chief")
	ins := func(owner int64, name string) error {
		_, err := f.pool.Exec(f.mig, `INSERT INTO agents (owner_id, name, namespace) VALUES ($1, $2, $3)`,
			owner, name, "ns-"+name)
		return err
	}
	if err := ins(1, "agent"); err != nil {
		t.Fatalf("the operator's agent: %v", err)
	}
	if err := ins(f.two, "agent-"+strconv.FormatInt(f.two, 10)); err != nil {
		t.Fatalf("user 2's agent beside the operator's: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := ins(1, "agent-99"); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("a second reserved agent for user 1: %v; want 23505", err)
	}
	// Control: an ordinary agent for the same owner is not caught by the index.
	if err := ins(1, "brisk-otter"); err != nil {
		t.Errorf("an ordinary agent beside the agent: %v", err)
	}
}

// TestEachUserHasTheirOwnGitHubConnection (accounts PR 3b): the GitHub store
// acts for the PRINCIPAL's user (it was the constant 1). Two users each save
// and read their own connection; neither Clear touches the other; a scope-all
// caller has no connection to read; and a token sealed for one owner does not
// open on another owner's row.
func TestEachUserHasTheirOwnGitHubConnection(t *testing.T) {
	f := newRLSFixture(t, "ms_0009_github")
	st, err := github.NewPG(f.pool, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	one, two := asUser(f.base, 1), asUser(f.base, f.two)
	if err := st.Save(one, "octo-one", "repo", "gho_one"); err != nil {
		t.Fatalf("user 1 Save: %v", err)
	}
	if err := st.Save(two, "octo-two", "repo", "gho_two"); err != nil {
		t.Fatalf("user 2 Save: %v", err)
	}
	for _, c := range []struct {
		ctx        context.Context
		login, tok string
	}{{one, "octo-one", "gho_one"}, {two, "octo-two", "gho_two"}} {
		got, ok, err := st.Get(c.ctx)
		if err != nil || !ok || got.Login != c.login || got.Token != c.tok {
			t.Errorf("Get = %+v %v %v; want %s's own", got, ok, err, c.login)
		}
	}
	if _, _, err := st.Get(authz.System(f.base)); err == nil {
		t.Error("System read a GitHub connection; a scope-all caller has none")
	}
	// A ciphertext moved onto another owner's row does not open there.
	if _, err := f.pool.Exec(f.mig, `UPDATE github_connection SET token_ciphertext =
		(SELECT token_ciphertext FROM github_connection WHERE owner_id=1) WHERE owner_id=$1`, f.two); err != nil {
		t.Fatal(err)
	}
	if got, _, err := st.Get(two); err == nil {
		t.Errorf("user 1's sealed token opened on user 2's row: %+v", got)
	}
	if err := st.Clear(two); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Get(one); err != nil || !ok {
		t.Errorf("user 2's Clear removed user 1's connection: %v %v", ok, err)
	}
	if _, ok, _ := st.Get(two); ok {
		t.Error("user 2's Clear left its own connection")
	}
}
