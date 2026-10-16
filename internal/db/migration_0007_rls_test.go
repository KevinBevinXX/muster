package db_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/notes"
)

// Migration 0007 — row-level security — against real Postgres, in SCRATCH
// databases (a second user must not leak into any other test). Every claim is
// about the DATABASE plus internal/db's per-acquire scoping (scope.go): the
// pool every store uses.
//
// The principals, built here directly (test code is outside the authz
// ledgers):
//
//	none      a bare context — the fail-closed case
//	user 1    the operator, as every request principal is today
//	user 2    a second user, created by the test
//	System    authz.System — the background loops (scope all)

func asUser(ctx context.Context, uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// rlsFixture is a migrated scratch database plus a second user.
type rlsFixture struct {
	mig  context.Context // the migrator (login role, scope all, uid 1): ground truth
	base context.Context // a bare context: no principal
	pool *pgxpool.Pool
	two  int64
}

func newRLSFixture(t *testing.T, name string) *rlsFixture {
	t.Helper()
	mig := ctx0006(t)
	pool := freshMigrated(mig, t, name)
	base, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	f := &rlsFixture{mig: mig, base: base, pool: pool}
	f.two = scanID(mig, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	return f
}

// writeEveryOwnedTable inserts ONE row into every owned table as ctx's
// principal, naming NO owner_id anywhere — so where each row lands is decided
// by 0007's default (the principal's muster.uid) and, for children, by the
// composite foreign keys. tag keeps names unique.
func writeEveryOwnedTable(ctx context.Context, pool *pgxpool.Pool, tag string) error {
	var note, agent, session, plugin, message int64
	steps := []struct {
		sql  string
		args func() []any
		dest *int64
	}{
		{`INSERT INTO notes (title) VALUES ($1) RETURNING id`, func() []any { return []any{"task-" + tag} }, &note},
		{`INSERT INTO note_comments (note_id, author, body) VALUES ($1, 'op', 'hi')`, func() []any { return []any{note} }, nil},
		{`INSERT INTO note_attachments (note_id, filename, size_bytes, data) VALUES ($1, 'a.txt', 1, '\x61')`, func() []any { return []any{note} }, nil},
		{`INSERT INTO task_sessions (note_id, session_id, role) VALUES ($1, $2, 'created')`, func() []any { return []any{note, "sess-" + tag} }, nil},
		{`INSERT INTO agents (name, namespace, note_id) VALUES ($1, $2, $3) RETURNING id`, func() []any { return []any{"agent-" + tag, "ns-" + tag, note} }, &agent},
		{`INSERT INTO chat_sessions (agent_id, session_key) VALUES ($1, $2) RETURNING id`, func() []any { return []any{agent, "key-" + tag} }, &session},
		{`INSERT INTO chat_messages (agent_id, session_id, role, content) VALUES ($1, $2, 'user', 'hello')`, func() []any { return []any{agent, session} }, nil},
		{`INSERT INTO connectors (name, hosts, auth_scheme, secret_ciphertext) VALUES ($1, ARRAY['api.example.test'], 'bearer', '\x00')`, func() []any { return []any{"conn-" + tag} }, nil},
		{`INSERT INTO connector_grants (agent_id, connector_name) VALUES ($1, $2)`, func() []any { return []any{agent, "conn-" + tag} }, nil},
		{`INSERT INTO connector_audit (kind, agent_id, agent_name, connector_name, method, host, path, status) VALUES ('proxy', $1, 'a', 'c', 'GET', 'h', '/', 200)`, func() []any { return []any{agent} }, nil},
		{`INSERT INTO github_connection (login, scopes, token_ciphertext) VALUES ($1, 'repo', '\xde')`, func() []any { return []any{"octo-" + tag} }, nil},
		{`INSERT INTO mirror_tokens (host, token_hash) VALUES ($1, $2)`, func() []any { return []any{"host-" + tag, "hash-" + tag} }, nil},
		{`INSERT INTO flow_runner_tokens (host, token_hash) VALUES ($1, $2)`, func() []any { return []any{"host-" + tag, "flowhash-" + tag} }, nil},
		{`INSERT INTO flow_requests (agent_id, agent_name, flow, host, instance, expires_at) VALUES ($1, 'a', 'muse.poll', 'laptop', 'personal', now() + interval '1 hour')`, func() []any { return []any{agent} }, nil},
		{`INSERT INTO flow_jobs (flow, class, host, instance, requested_via, expires_at) VALUES ('muse.poll', 'read', $1, 'personal', 'session', now() + interval '1 hour')`, func() []any { return []any{"host-" + tag} }, nil},
		{`INSERT INTO user_credentials (kind, name, secret_ciphertext, fingerprint) VALUES ('claude', $1, '\x00', 'fp')`, func() []any { return []any{"acct-" + tag} }, nil},
		{`INSERT INTO cc_account_marks (account, rate_limited_detail) VALUES ($1, 'x')`, func() []any { return []any{"acct-" + tag} }, nil},
		{`INSERT INTO push_subscriptions (endpoint, p256dh, auth) VALUES ($1, 'k', 'a')`, func() []any { return []any{"https://fcm.googleapis.com/fcm/send/" + tag} }, nil},
		{`INSERT INTO push_preferences (muted) VALUES ('{}')`, func() []any { return nil }, nil},
		{`INSERT INTO plugins (name, label, token_hash, data_scope) VALUES ($1, 'Example inbox', $2, 'example') RETURNING id`, func() []any { return []any{"plugin-" + strings.ToLower(tag), "pluginhash-" + tag} }, &plugin},
		{`INSERT INTO plugin_messages (plugin_id, external_id, body, status, agent_id) VALUES ($1, $2, 'hello', 'delivering', $3) RETURNING id`, func() []any { return []any{plugin, "ext-" + tag, agent} }, &message},
		{`INSERT INTO plugin_reply_drafts (message_id, agent_id, agent_name, body, expires_at) VALUES ($1, $2, 'a', 'reply', now() + interval '1 day')`, func() []any { return []any{message, agent} }, nil},
		{`INSERT INTO pins (kind, agent_id) VALUES ('agent', $1)`, func() []any { return []any{agent} }, nil},
		{`INSERT INTO send_queue (kind, agent_id, body) VALUES ('agent', $1, 'hello')`, func() []any { return []any{agent} }, nil},
	}
	for _, s := range steps {
		var err error
		if s.dest != nil {
			err = pool.QueryRow(ctx, s.sql, s.args()...).Scan(s.dest)
		} else {
			_, err = pool.Exec(ctx, s.sql, s.args()...)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(s.sql)[2], err)
		}
	}
	return nil
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count %s %s: %v", table, where, err)
	}
	return n
}

// userKeyedTables are the account tables migration 0009 puts under row-level
// security: keyed by a USER column (users.id, api_tokens.user_id,
// audit_log.user_id) or admin-only (invites), not by owner_id. Their policies
// are pinned whole by TestTheUserKeyedTablesCarryExactlyTheirPolicies.
var userKeyedTables = []string{"api_tokens", "audit_log", "invites", "users"}

// TestRLSCoversExactlyTheOwnedTables is the LEDGER: the set of public tables
// with an owner_id column and the set carrying the owner_isolation policy must
// be the same eleven, and the sets with RLS ENABLED and FORCED must be those
// eleven plus the user-keyed account tables (0009) — and each owner_isolation
// policy must have BOTH arms (the user and the scope-all) in USING and in WITH
// CHECK, and each owner_id must default to the principal. It fails when a new
// owned table arrives without RLS, and when RLS appears on a table that is in
// neither ledger.
func TestRLSCoversExactlyTheOwnedTables(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_ledger")
	set := func(sql string) []string {
		t.Helper()
		rows, err := f.pool.Query(f.mig, sql)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	want := rlsTables()
	sort.Strings(want)
	all := append(rlsTables(), userKeyedTables...)
	sort.Strings(all)
	for name, c := range map[string]struct {
		got, want []string
	}{
		"tables with owner_id": {set(`SELECT table_name::text FROM information_schema.columns
			WHERE table_schema='public' AND column_name='owner_id'`), want},
		"RLS enabled": {set(`SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND c.relkind='r' AND c.relrowsecurity`), all},
		"RLS forced": {set(`SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND c.relkind='r' AND c.relforcerowsecurity`), all},
		"tables with an owner_isolation policy": {set(`SELECT DISTINCT tablename::text FROM pg_policies
			WHERE schemaname='public' AND policyname='owner_isolation'`), want},
		"tables with a policy": {set(`SELECT DISTINCT tablename::text FROM pg_policies WHERE schemaname='public'`), all},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v\nwant          %v", name, c.got, c.want)
		}
	}
	rows, err := f.pool.Query(f.mig, `SELECT tablename::text, policyname::text, cmd, permissive, coalesce(qual,''), coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1)`, rlsTables())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, name, cmd, permissive, qual, check string
		if err := rows.Scan(&tbl, &name, &cmd, &permissive, &qual, &check); err != nil {
			t.Fatal(err)
		}
		if name != "owner_isolation" || cmd != "ALL" || permissive != "PERMISSIVE" {
			t.Errorf("%s: policy %q cmd=%s %s; want owner_isolation, ALL, PERMISSIVE", tbl, name, cmd, permissive)
		}
		for which, expr := range map[string]string{"USING": qual, "WITH CHECK": check} {
			if !strings.Contains(expr, "owner_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint") ||
				!strings.Contains(expr, "current_setting('muster.scope'::text, true) = 'all'::text") {
				t.Errorf("%s %s = %s; want the user arm AND the scope-all arm", tbl, which, expr)
			}
		}
	}
	for _, tbl := range rlsTables() {
		var def string
		if err := f.pool.QueryRow(f.mig, `SELECT coalesce(column_default,'') FROM information_schema.columns
			WHERE table_schema='public' AND table_name=$1 AND column_name='owner_id'`, tbl).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if def != "(NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint" {
			t.Errorf("%s.owner_id DEFAULT %q; want the principal's muster.uid", tbl, def)
		}
	}
}

// TestTheApplicationRoleIsBoundByRowLevelSecurity: the role every pooled
// connection acts as is NOT a superuser and holds no bypass — the property
// that makes the policies apply at all, given that the login role in
// production IS a superuser. And the pool really does switch to it.
func TestTheApplicationRoleIsBoundByRowLevelSecurity(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_role")
	var super, bypass bool
	if err := f.pool.QueryRow(f.mig, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=$1`, db.AppRole).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("%s: rolsuper=%v rolbypassrls=%v; either one makes every policy inert", db.AppRole, super, bypass)
	}
	for name, ctx := range map[string]context.Context{"user 1": asUser(f.base, 1), "System": authz.System(f.base), "no principal": f.base} {
		var who string
		if err := f.pool.QueryRow(ctx, `SELECT current_user::text`).Scan(&who); err != nil {
			t.Fatal(err)
		}
		if who != db.AppRole {
			t.Errorf("%s: a pooled query ran as %q, want %q", name, who, db.AppRole)
		}
	}
	// Control: the migrator keeps the login role (it needs DDL).
	var who string
	if err := f.pool.QueryRow(f.mig, `SELECT current_user::text`).Scan(&who); err != nil || who == db.AppRole {
		t.Fatalf("the migrator ran as %q (%v); want the login role", who, err)
	}
}

// TestRowLevelSecurityScopesEveryOwnedTable is the behaviour, per principal,
// on every owned table: no principal sees nothing and cannot insert; user 1
// sees every row there is today; user 2 sees none of user 1's rows, cannot
// touch them, and its own writes land as user 2; System sees everyone's.
func TestRowLevelSecurityScopesEveryOwnedTable(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_scope")
	one, two := asUser(f.base, 1), asUser(f.base, f.two)

	// User 1 writes a row into every owned table with no owner named.
	if err := writeEveryOwnedTable(one, f.pool, "one"); err != nil {
		t.Fatalf("user 1's writes: %v", err)
	}
	// "User 1 sees all of today's data": before a second user has written
	// anything, user 1's view of every table IS the whole table.
	for _, tbl := range rlsTables() {
		all := count(t, f.mig, f.pool, tbl, "")
		if all == 0 {
			t.Fatalf("fixture: %s is empty", tbl)
		}
		if got := count(t, one, f.pool, tbl, ""); got != all {
			t.Errorf("user 1 sees %d of %d rows in %s", got, all, tbl)
		}
		if got := count(t, f.mig, f.pool, tbl, "WHERE owner_id <> 1"); got != 0 {
			t.Errorf("%s: user 1's writes landed with %d rows not owned by 1", tbl, got)
		}
	}

	// User 2 writes the same, again naming no owner.
	if err := writeEveryOwnedTable(two, f.pool, "two"); err != nil {
		t.Fatalf("user 2's writes: %v", err)
	}
	for _, tbl := range rlsTables() {
		ones := count(t, f.mig, f.pool, tbl, "WHERE owner_id = 1")
		twos := count(t, f.mig, f.pool, tbl, "WHERE owner_id = $1", f.two)
		if twos == 0 {
			t.Errorf("%s: user 2's write did not land as owner %d", tbl, f.two)
		}
		if got := count(t, two, f.pool, tbl, ""); got != twos {
			t.Errorf("user 2 sees %d rows in %s; owns %d", got, tbl, twos)
		}
		if got := count(t, two, f.pool, tbl, "WHERE owner_id = 1"); got != 0 {
			t.Errorf("user 2 sees %d of user 1's rows in %s", got, tbl)
		}
		if got := count(t, one, f.pool, tbl, ""); got != ones {
			t.Errorf("user 1 sees %d rows in %s; owns %d (user 2's leaked in?)", got, tbl, ones)
		}
		if got := count(t, authz.System(f.base), f.pool, tbl, ""); got != ones+twos {
			t.Errorf("System sees %d rows in %s; there are %d", got, tbl, ones+twos)
		}
		if got := count(t, f.base, f.pool, tbl, ""); got != 0 {
			t.Errorf("NO principal sees %d rows in %s; want 0 (fail closed)", got, tbl)
		}
		// User 2 can neither change nor delete user 1's rows: zero rows matched.
		tag, err := f.pool.Exec(two, `DELETE FROM `+tbl+` WHERE owner_id = 1`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("user 2 DELETE of user 1's %s rows: %v rows, %v; want 0", tbl, tag.RowsAffected(), err)
		}
		tag, err = f.pool.Exec(two, `UPDATE `+tbl+` SET owner_id = owner_id WHERE owner_id = 1`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("user 2 UPDATE of user 1's %s rows: %v rows, %v; want 0", tbl, tag.RowsAffected(), err)
		}
		if got := count(t, f.mig, f.pool, tbl, "WHERE owner_id = 1"); got != ones {
			t.Errorf("%s: user 1 owns %d rows after user 2's attempts, %d before", tbl, got, ones)
		}
	}

	// User 2 cannot WRITE a row as user 1 (WITH CHECK), nor hand its own away.
	_, err := f.pool.Exec(two, `INSERT INTO notes (title, owner_id) VALUES ('forged', 1)`)
	assertSQLState(t, "user 2 inserting a row owned by 1", err, "42501")
	_, err = f.pool.Exec(two, `UPDATE notes SET owner_id = 1 WHERE owner_id = $1`, f.two)
	assertSQLState(t, "user 2 re-owning its row to 1", err, "42501")

	// No principal: no insert lands — owner_id defaults to NULL, which no
	// policy's WITH CHECK admits (42501, raised before NOT NULL) — and it is
	// reported as unscoped.
	var unscoped atomic.Int32
	restore := db.SetUnscopedHook(func(string) { unscoped.Add(1) })
	_, err = f.pool.Exec(f.base, `INSERT INTO notes (title) VALUES ('nobody')`)
	restore()
	assertSQLState(t, "an insert with no principal", err, "42501")
	if unscoped.Load() == 0 {
		t.Errorf("the unscoped insert was not reported to the hook")
	}
	// Control: a scoped query is not reported.
	unscoped.Store(0)
	restore = db.SetUnscopedHook(func(string) { unscoped.Add(1) })
	_ = count(t, one, f.pool, "notes", "")
	restore()
	if unscoped.Load() != 0 {
		t.Errorf("a query under user 1 was reported unscoped")
	}
}

func assertSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: err = %v; want SQLSTATE %s", what, err, code)
	}
}

// TestForceAppliesRLSToTheTablesOwner: FORCE is what binds a connection acting
// as the tables' OWNER (a deployment whose login role is an ordinary owner, not
// a superuser, connecting without the role switch — e.g. an image older than
// 0007). Ownership of the owned tables is handed to a NOSUPERUSER role and a
// raw connection SETs ROLE to it: with no muster.uid it sees nothing.
func TestForceAppliesRLSToTheTablesOwner(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_force")
	if err := writeEveryOwnedTable(asUser(f.base, 1), f.pool, "one"); err != nil {
		t.Fatal(err)
	}
	mustExec(f.mig, t, f.pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zz_muster_rls_owner') THEN
			CREATE ROLE zz_muster_rls_owner NOLOGIN NOSUPERUSER NOBYPASSRLS;
		END IF; END $$`)
	for _, tbl := range rlsTables() {
		mustExec(f.mig, t, f.pool, `ALTER TABLE public.`+tbl+` OWNER TO zz_muster_rls_owner`)
	}
	conn, err := f.pool.Acquire(f.mig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.mig, `SELECT set_config('muster.uid', '', false), set_config('muster.scope', '', false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(f.mig, `SET ROLE zz_muster_rls_owner`); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range rlsTables() {
		var n int
		if err := conn.QueryRow(f.mig, `SELECT count(*) FROM public.`+tbl).Scan(&n); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("the tables' OWNER, with no principal, sees %d rows of %s; want 0 (FORCE)", n, tbl)
		}
	}
	// Control: the same owner connection WITH user 1's id sees the rows.
	if _, err := conn.Exec(f.mig, `SELECT set_config('muster.uid', '1', false)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(f.mig, `SELECT count(*) FROM public.notes`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("control: the owner as user 1 sees %d notes (%v); the instrument cannot see rows", n, err)
	}
	if _, err := conn.Exec(f.mig, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
}

// TestTheNameTakenCheckSeesEveryUsersAgents: agents.name is ONE namespace, so
// the check must see other users' agents — which a plain SELECT under RLS does
// not. The SECURITY DEFINER function returns only the boolean.
func TestTheNameTakenCheckSeesEveryUsersAgents(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_names")
	if err := writeEveryOwnedTable(asUser(f.base, 1), f.pool, "one"); err != nil {
		t.Fatal(err)
	}
	two := asUser(f.base, f.two)
	var plain bool
	if err := f.pool.QueryRow(two, `SELECT EXISTS (SELECT 1 FROM agents WHERE name='agent-one')`).Scan(&plain); err != nil || plain {
		t.Fatalf("control: user 2's plain SELECT sees user 1's agent name = %v (%v); then the function proves nothing", plain, err)
	}
	store := agents.NewPG(f.pool)
	for name, want := range map[string]bool{"agent-one": true, "agent-nobody-has": false} {
		got, err := store.NameExists(two, name)
		if err != nil || got != want {
			t.Errorf("NameExists(%q) as user 2 = %v, %v; want %v", name, got, err, want)
		}
	}
	// The function's definition: SECURITY DEFINER, search_path pinned, and
	// scope 'all' for its own duration.
	var definer bool
	var config []string
	if err := f.pool.QueryRow(f.mig, `SELECT prosecdef, proconfig FROM pg_proc WHERE proname='muster_agent_name_taken'`).Scan(&definer, &config); err != nil {
		t.Fatal(err)
	}
	slices.Sort(config)
	if !definer || !slices.Equal(config, []string{"muster.scope=all", "search_path=pg_catalog"}) {
		t.Errorf("muster_agent_name_taken: prosecdef=%v proconfig=%v", definer, config)
	}
	// The complete SECURITY DEFINER inventory of the schema.
	rows, err := f.pool.Query(f.mig, `SELECT p.proname::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public' AND p.prosecdef ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"muster_agent_name_taken", "muster_retire_agent_name"}; !slices.Equal(got, want) {
		t.Errorf("SECURITY DEFINER functions in public = %v; want exactly %v", got, want)
	}
}

// TestSystemWritesChildRowsAsTheParentsOwner: the kickoff deliverer runs as
// System (no user) and writes chat rows for whichever user's agent it serves.
// A child row takes its PARENT's owner, so they land as that user — not NULL
// (refused) and not user 1.
func TestSystemWritesChildRowsAsTheParentsOwner(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_children")
	two := asUser(f.base, f.two)
	as := agents.NewPG(f.pool)
	a, err := as.Create(two, agents.Agent{Name: "child-owner-agent", Namespace: "ns-child-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnerID != f.two {
		t.Fatalf("an agent created by user %d has OwnerID %d", f.two, a.OwnerID)
	}
	sys := authz.System(f.base)
	sess, err := as.LatestOrCreateSession(sys, a.ID, a.Name)
	if err != nil {
		t.Fatalf("System LatestOrCreateSession: %v", err)
	}
	if _, err := as.AddChatMessage(sys, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: "hello"}); err != nil {
		t.Fatalf("System AddChatMessage: %v", err)
	}
	for _, tbl := range []string{"chat_sessions", "chat_messages"} {
		if got := count(t, f.mig, f.pool, tbl, "WHERE agent_id=$1 AND owner_id=$2", a.ID, f.two); got != 1 {
			t.Errorf("%s: %d rows of user %d's agent owned by %d; want 1", tbl, got, f.two, f.two)
		}
	}
	// And a note's children under System (the reaper's comment) take the note's owner.
	ns := notes.NewPG(f.pool)
	n, err := ns.Create(two, notes.Note{Title: "user two's task", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ns.AddComment(sys, notes.Comment{NoteID: n.ID, Author: "system", Body: "x"}); err != nil {
		t.Fatalf("System AddComment: %v", err)
	}
	if got := count(t, f.mig, f.pool, "note_comments", "WHERE note_id=$1 AND owner_id=$2", n.ID, f.two); got != 1 {
		t.Errorf("a System comment on user %d's task: %d rows owned by them; want 1", f.two, got)
	}
	if _, _, err := ns.LinkSession(sys, n.ID, notes.SessionLink{SessionID: "sys-session", Role: "read"}, false); err != nil {
		t.Fatalf("System LinkSession: %v", err)
	}
	if got := count(t, f.mig, f.pool, "task_sessions", "WHERE note_id=$1 AND owner_id=$2", n.ID, f.two); got != 1 {
		t.Errorf("a System session link on user %d's task: %d rows owned by them; want 1", f.two, got)
	}
	if _, err := ns.AddAttachment(sys, n.ID, notes.Attachment{Filename: "s.txt", ContentType: "text/plain", SizeBytes: 1, Data: []byte("s")}); err != nil {
		t.Fatalf("System AddAttachment: %v", err)
	}
	if got := count(t, f.mig, f.pool, "note_attachments", "WHERE note_id=$1 AND owner_id=$2", n.ID, f.two); got != 1 {
		t.Errorf("a System attachment on user %d's task: %d rows owned by them; want 1", f.two, got)
	}
	// Control: user 1 cannot attach a comment to user 2's task (it cannot see it).
	if _, err := ns.AddComment(asUser(f.base, 1), notes.Comment{NoteID: n.ID, Author: "op", Body: "y"}); err == nil {
		t.Errorf("user 1 commented on user %d's task", f.two)
	}
}

// TestAReleasedConnectionCarriesNobody: AfterRelease clears the scope.
func TestAReleasedConnectionCarriesNobody(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_release")
	conn, err := f.pool.Acquire(asUser(f.base, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	read := func() (string, string) {
		var uid, scope string
		if err := conn.QueryRow(f.base, `SELECT current_setting('muster.uid', true), current_setting('muster.scope', true)`).Scan(&uid, &scope); err != nil {
			t.Fatal(err)
		}
		return uid, scope
	}
	if uid, _ := read(); uid != "1" {
		t.Fatalf("control: an acquire as user 1 set muster.uid=%q", uid)
	}
	if !db.AfterReleaseForTest(conn.Conn()) {
		t.Fatal("afterRelease refused a healthy connection")
	}
	if uid, scope := read(); uid != "" || scope != "" {
		t.Errorf("after release: muster.uid=%q muster.scope=%q; want both empty", uid, scope)
	}
}

// TestRLSOffAndOnRoundTrip is the rollback: --rls-off makes a no-principal
// (old-image) connection see rows and insert as user 1 again; --rls-on undoes it.
func TestRLSOffAndOnRoundTrip(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_rollback")
	if err := writeEveryOwnedTable(asUser(f.base, 1), f.pool, "one"); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOff(f.base, f.pool); err != nil {
		t.Fatal(err)
	}
	assertRLSState(t, f, false)
	for _, tbl := range ownedTables {
		if got, all := count(t, f.base, f.pool, tbl, ""), count(t, f.mig, f.pool, tbl, ""); got != all {
			t.Errorf("RLS off: no principal sees %d of %d rows in %s", got, all, tbl)
		}
	}
	var owner int64
	if err := f.pool.QueryRow(f.base, `INSERT INTO notes (title) VALUES ('old image') RETURNING owner_id`).Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("RLS off: an insert with no principal = owner %d, %v; want 1", owner, err)
	}
	if err := db.RLSOn(f.base, f.pool); err != nil {
		t.Fatal(err)
	}
	assertRLSState(t, f, true)
	if got := count(t, f.base, f.pool, "notes", ""); got != 0 {
		t.Errorf("RLS back on: no principal sees %d notes; want 0", got)
	}
	if got := count(t, asUser(f.base, 1), f.pool, "notes", ""); got != 2 {
		t.Errorf("RLS back on: user 1 sees %d notes; want 2", got)
	}
}

// TestMigrateRunsFromABareContext: muster-server and muster-migrate call
// db.Migrate with a context that carries NO principal, against a database
// 0007 may not have reached (the application role may not even exist). Migrate
// must act as the migrator on its own — the login role, every row — or a
// fresh database could never be migrated.
func TestMigrateRunsFromABareContext(t *testing.T) {
	mig := ctx0006(t)
	pool, err := db.Connect(mig, scratchDatabase(mig, t, "ms_0007_bare_migrate", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applyThrough(mig, t, pool, 6)
	// Populated, so 0007 has rows to act on and a data step would see them.
	scanID(mig, t, pool, `INSERT INTO notes (title) VALUES ('pre-0007') RETURNING id`)
	bare, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(bare, pool, nil); err != nil {
		t.Fatalf("db.Migrate from a bare context: %v", err)
	}
	if got := count(t, asUser(bare, 1), pool, "notes", ""); got != 1 {
		t.Errorf("after migrating, user 1 sees %d notes; want 1", got)
	}
}

// TestMigrateRecreatesTheAppRolesGrants is the restore case: a database at the
// latest version whose `GRANT … TO muster_app` statements never landed (the
// backup's plain dump restored into a fresh Postgres, where the role did not
// exist) must come back to life on the next boot. The cluster role itself
// cannot be dropped here — other test databases on this server depend on it —
// so the in-suite half removes the GRANTS; the missing-role half was measured
// against a fresh PG16 (see appRoleSQL).
func TestMigrateRecreatesTheAppRolesGrants(t *testing.T) {
	f := newRLSFixture(t, "ms_0007_restore")
	if err := writeEveryOwnedTable(asUser(f.base, 1), f.pool, "one"); err != nil {
		t.Fatal(err)
	}
	mustExec(f.mig, t, f.pool, `REVOKE ALL ON ALL TABLES IN SCHEMA public FROM muster_app`)
	mustExec(f.mig, t, f.pool, `REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM muster_app`)
	mustExec(f.mig, t, f.pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM muster_app`)
	mustExec(f.mig, t, f.pool, `REVOKE USAGE ON SCHEMA public FROM muster_app`)
	one := asUser(f.base, 1)
	var n int
	err := f.pool.QueryRow(one, `SELECT count(*) FROM notes`).Scan(&n)
	assertSQLState(t, "control: user 1 reading notes with the grants gone", err, "42501")

	if err := db.Migrate(f.base, f.pool, nil); err != nil {
		t.Fatalf("db.Migrate on a restored (latest-version, grant-less) database: %v", err)
	}
	if got := count(t, one, f.pool, "notes", ""); got != 1 {
		t.Errorf("after a boot's Migrate, user 1 sees %d notes; want 1", got)
	}
	if _, err := f.pool.Exec(one, `INSERT INTO notes (title) VALUES ('after restore')`); err != nil {
		t.Errorf("after a boot's Migrate, user 1 cannot insert: %v", err)
	}
}

// assertRLSState checks db.RLSStatus — what muster-migrate prints after
// --rls-off / --rls-on — against the eleven owned tables.
func assertRLSState(t *testing.T, f *rlsFixture, on bool) {
	t.Helper()
	states, err := db.RLSStatus(f.base, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for _, st := range states {
		tables = append(tables, st.Table)
		if st.Enabled != on || st.Forced != on || (st.OwnerDefault == "1") == on {
			t.Errorf("%s after rls on=%v: %+v", st.Table, on, st)
		}
	}
	want := rlsTables()
	sort.Strings(want)
	if !slices.Equal(tables, want) {
		t.Errorf("RLSStatus tables = %v; want %v", tables, want)
	}
}
