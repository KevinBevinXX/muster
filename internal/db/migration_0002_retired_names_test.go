package db_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Migration 0002's guards — the DATABASE half of the agent-name tombstone.
//
// 🔴 WHAT MAKES THESE DIFFERENT FROM THE PGStore GUARDS IN internal/agents.
// Those prove the GENERATOR refuses a retired name when the application deletes
// an agent through PGStore.Delete. The property THIS file pins is the one the
// trigger was chosen for over an application-level insert: the tombstone appears
// for a DELETE that never touches Go at all. An insert in Provisioner.Destroy
// would be a rule every future call site has to remember, and would cover only
// the binary that has it; a trigger has nothing to remember. A test that deletes
// through the store cannot tell the two designs apart — so these delete with raw
// SQL.
//
// ⚠ They run against this package's private, fully-migrated database (dbtest
// copies an already-migrated template), i.e. the schema the whole 0001..0002
// chain actually produced — not a scratch schema built from 0002's body alone. A
// scratch-schema fixture could not see a trigger that failed to attach to the
// real `agents`.

// pinnedConn takes ONE connection out of the pool and holds it for the test.
//
// 🔴 A POOL IS NOT A SESSION, AND THE search_path GUARD BELOW IS A CLAIM ABOUT A
// SESSION. `SET search_path` applies to the connection it runs on; issued
// through a pool, the next statement can land on a different connection and the
// hijack the test believes it installed is simply not in effect. The guard has
// its own control for that, but pinning the connection is what makes the control
// pass for the right reason.
func pinnedConn(t *testing.T) (context.Context, *pgxpool.Conn) {
	t.Helper()
	ctx, pool := migrated(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a pinned connection: %v", err)
	}
	t.Cleanup(conn.Release)
	return ctx, conn
}

// insertAgentRow inserts a minimal `agents` row with raw SQL and returns its id.
// It deliberately does NOT go through internal/agents: this file's claim is about
// the database, and importing the store would make internal/db depend on it.
func insertAgentRow(ctx context.Context, t *testing.T, conn *pgxpool.Conn, name string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO agents (name, namespace, status) VALUES ($1, $2, 'provisioning') RETURNING id`,
		name, "devpod-"+name).Scan(&id); err != nil {
		t.Fatalf("insert agents row %q: %v", name, err)
	}
	return id
}

// clearName removes every trace of a name so an assertion cannot be answered by
// a leftover from an earlier run.
//
// 🔴 THE ORDER MATTERS AND THE SECOND TOMBSTONE DELETE IS NOT A TYPO. Clearing
// the `agents` row FIRES THE TRIGGER, which writes the very tombstone the caller
// is trying to clear. Tombstone, agent, tombstone again.
func clearName(ctx context.Context, t *testing.T, conn *pgxpool.Conn, name string) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
		`DELETE FROM public.agents WHERE name=$1`,
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
	} {
		if _, err := conn.Exec(ctx, stmt, name); err != nil {
			t.Fatalf("clear %q with %q: %v", name, stmt, err)
		}
	}
}

// TestRetiredNameTriggerIsRegisteredOnAgents is the STRUCTURAL half of the
// closing condition: pg_trigger holds exactly one non-internal trigger on
// `agents`, it is AFTER DELETE FOR EACH ROW, and the function it runs writes to
// the retired-names table.
//
// 🔴 IT IS A LEDGER OF THE WHOLE SET, NOT A SPOT CHECK, so it reddens when the
// set GROWS as well as when it shrinks. A second trigger on `agents` is exactly
// the kind of thing that changes what a DELETE means without any test noticing.
//
// ⚠ AND IT IS HALF A GUARD ON ITS OWN, WHICH IS WHY IT IS NAMED AS THE
// STRUCTURAL HALF. A trigger can be registered with all the right bits and still
// not tombstone anything — the function body is checked here only for the table
// NAME, which is a check on spelling. TestRetiredNameIsTombstonedByARawSQLDelete
// is the behavioural half; neither is sufficient alone.
func TestRetiredNameTriggerIsRegisteredOnAgents(t *testing.T) {
	ctx, conn := pinnedConn(t)

	rows, err := conn.Query(ctx, `
		SELECT t.tgname, t.tgtype, p.proname, pg_get_functiondef(p.oid)
		  FROM pg_trigger t
		  JOIN pg_class c     ON c.oid = t.tgrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_proc p      ON p.oid = t.tgfoid
		 WHERE NOT t.tgisinternal
		   AND n.nspname = 'public'
		   AND c.relname = 'agents'
		   AND (t.tgtype & 8) <> 0
		 ORDER BY t.tgname`)
	if err != nil {
		t.Fatalf("read pg_trigger: %v", err)
	}
	defer rows.Close()

	type trig struct {
		name   string
		tgtype int16
		fn     string
		fnBody string
	}
	var got []trig
	for rows.Next() {
		var tr trig
		if err := rows.Scan(&tr.name, &tr.tgtype, &tr.fn, &tr.fnBody); err != nil {
			t.Fatalf("scan pg_trigger row: %v", err)
		}
		got = append(got, tr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_trigger rows: %v", err)
	}

	if len(got) != 1 {
		var names []string
		for _, tr := range got {
			names = append(names, tr.name)
		}
		// ⚠ DELETE TRIGGERS ONLY (tgtype bit 3): migration 0003 adds a BEFORE UPDATE
		// trigger (agents_kind_account_immutable) that changes nothing a DELETE does,
		// and has its own guard (TestKindAndAccountAreImmutableAfterInsert).
		t.Fatalf("`agents` carries %d non-internal DELETE trigger(s) %v, want exactly 1 "+
			"(agents_retire_name). A missing one means a destroyed agent's name goes "+
			"straight back into the generator pool; an extra one changes what a DELETE "+
			"does with nothing else in the suite watching.", len(got), names)
	}
	tr := got[0]

	if tr.name != "agents_retire_name" {
		t.Errorf("the trigger on `agents` is named %q, want %q", tr.name, "agents_retire_name")
	}

	// pg_trigger.tgtype bit layout (src/include/catalog/pg_trigger.h):
	//   1<<0 ROW   1<<1 BEFORE   1<<2 INSERT   1<<3 DELETE
	//   1<<4 UPDATE   1<<5 TRUNCATE   1<<6 INSTEAD
	// AFTER is the absence of BEFORE and of INSTEAD.
	const (
		bitRow     = int16(1 << 0)
		bitBefore  = int16(1 << 1)
		bitDelete  = int16(1 << 3)
		bitInstead = int16(1 << 6)
	)
	if tr.tgtype&bitRow == 0 {
		t.Errorf("%s is a STATEMENT-level trigger (tgtype=%d), want FOR EACH ROW. A statement "+
			"trigger has no OLD row, so it cannot know which name was released — a DELETE "+
			"removing three agents would tombstone none of them.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitDelete == 0 {
		t.Errorf("%s does not fire on DELETE (tgtype=%d); DELETE is the only event that "+
			"releases a name back into the pool.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitBefore != 0 {
		t.Errorf("%s is a BEFORE trigger (tgtype=%d), want AFTER. BEFORE DELETE runs while the "+
			"row may still be skipped by another BEFORE trigger returning NULL, so it can "+
			"tombstone a name that was never actually released.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitInstead != 0 {
		t.Errorf("%s is an INSTEAD OF trigger (tgtype=%d), want AFTER — INSTEAD OF replaces the "+
			"delete entirely.", tr.name, tr.tgtype)
	}

	if tr.fn != "muster_retire_agent_name" {
		t.Errorf("%s executes %q, want %q", tr.name, tr.fn, "muster_retire_agent_name")
	}
	// A spelling check, and labelled as one: it says the function TALKS ABOUT the
	// ledger, not that it writes to it correctly. The behavioural guard below is
	// what pins the write.
	if !strings.Contains(tr.fnBody, "agent_retired_names") {
		t.Errorf("%s's function body never mentions agent_retired_names:\n%s", tr.fn, tr.fnBody)
	}
}

// TestRetiredNameIsTombstonedByARawSQLDelete is the call-site-independence
// claim: a DELETE issued by something that has never heard of muster's Go code —
// a psql prompt, a future binary, a cascade from another table — still retires
// the name. This is the BEHAVIOURAL half of the closing condition's first limb.
func TestRetiredNameIsTombstonedByARawSQLDelete(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzraw-sqldelete"
	clearName(ctx, t, conn, name)

	var before int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&before); err != nil {
		t.Fatalf("count tombstones before: %v", err)
	}
	if before != 0 {
		t.Fatalf("%q is already tombstoned before the test acts (%d rows); the assertion below "+
			"would pass whether or not the trigger fires", name, before)
	}

	id := insertAgentRow(ctx, t, conn, name)

	// The DELETE under test. No Go store, no application purge — just SQL.
	tag, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id)
	if err != nil {
		t.Fatalf("raw DELETE of agent %d: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("raw DELETE removed %d rows, want 1; nothing was deleted so the trigger had no "+
			"reason to fire", tag.RowsAffected())
	}

	var after int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&after); err != nil {
		t.Fatalf("count tombstones after: %v", err)
	}
	if after != 1 {
		t.Fatalf("agent_retired_names holds %d rows for %q after a raw SQL DELETE, want 1. The "+
			"name is back in the generator pool (internal/agents/names.go), so the next "+
			"generated agent can be its namesake and inherit the ServiceAccount and "+
			"cluster-scoped ClusterRoleBinding named after the dead one. An application-level "+
			"insert in Provisioner.Destroy would NOT have covered this delete — that is why "+
			"0002 is a trigger.", after, name)
	}

	// Re-deleting the same name must not error: the trigger's ON CONFLICT DO
	// NOTHING is what makes the ledger idempotent, and a name genuinely can be
	// inserted and deleted more than once (see the next test).
	id2 := insertAgentRow(ctx, t, conn, name)
	if _, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id2); err != nil {
		t.Fatalf("second DELETE of the same name failed: %v — the trigger's INSERT is not "+
			"conflict-tolerant, so destroying a re-provisioned agent would fail outright", err)
	}
}

// TestRetiredNameTombstoneIsSearchPathProof replays the hijack an unqualified
// INSERT in the trigger function is vulnerable to.
//
// 🔴 WHAT IT PINS, AND WHY A STRUCTURAL CHECK WOULD NOT DO. An UNQUALIFIED
// `INSERT INTO agent_retired_names` in the function body does not reach
// `public.agent_retired_names`, which is the only table PGStore.NameExists
// reads. With a decoy `<schema>.agent_retired_names` first on the calling
// session's path, public gets 0 rows, so NameExists never sees the tombstone and
// the name goes back into the pool. That defeats exactly the property 0002 is
// sold on ("any call site, any binary, including a hand-run psql DELETE") — and
// a hand-run psql session is where a non-default search_path comes from.
//
// ⚠ AN EARLIER REVISION OF THIS PARAGRAPH GAVE THE WRONG MECHANISM, AND THE
// CORRECTION MATTERS TO ANYONE READING THE FAILURE. It said the unqualified form
// "SUCCEEDS and writes the tombstone into the decoy — silent". That is true only
// with the function's `SET search_path = pg_catalog` ALSO removed. Measured with
// the `SET` in place, the unqualified form instead raises
// `relation "agent_retired_names" does not exist` and the DELETE ABORTS — loud,
// and fails closed. Either way this test reddens on `inPublic != 1`, so what it
// pins is unchanged; what changes is what the reader should expect to see. 0002's
// header carries all four measured combinations.
//
// Asserting `proconfig`, or grepping the function body for "public.", would be a
// check on the SPELLING of the fix. This asserts the OUTCOME, so it holds for
// either spelling and for any third one someone substitutes later.
//
// ⚠ WHAT IT THEREFORE DOES NOT COVER: removing only the `SET search_path` line
// while keeping the `public.` qualification leaves this test green. With nothing
// unqualified in the function body there is no behaviour to observe, so the
// `SET` is unpinned defence-in-depth. 0002's header says why that is accepted
// rather than closed with a proconfig assertion.
//
// The two assertions are a pair on purpose: `shadow == 0` alone is satisfied by
// a trigger that never fires at all, so `public == 1` is the positive control
// that makes the zero mean something.
func TestRetiredNameTombstoneIsSearchPathProof(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzsearchpath-hijack"
	const shadowSchema = "zz_shadow_retired_names"

	clearName(ctx, t, conn, name)
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + shadowSchema + ` CASCADE`,
		`CREATE SCHEMA ` + shadowSchema,
		`CREATE TABLE ` + shadowSchema + `.agent_retired_names (
			name TEXT PRIMARY KEY, retired_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		bg := db.AsMigratorForTest(context.Background())
		if _, err := conn.Exec(bg, `RESET search_path`); err != nil {
			t.Errorf("cleanup RESET search_path: %v", err)
		}
		if _, err := conn.Exec(bg, `DROP SCHEMA IF EXISTS `+shadowSchema+` CASCADE`); err != nil {
			t.Errorf("cleanup DROP SCHEMA: %v", err)
		}
	})

	// The decoy is FIRST on the path — the shape a psql session, a restore script
	// or a multi-schema layout produces.
	if _, err := conn.Exec(ctx, `SET search_path TO `+shadowSchema+`, public`); err != nil {
		t.Fatalf("set the hijacked search_path: %v", err)
	}
	// Control for the fixture: prove the hijack is actually in effect, or the whole
	// test is a re-run of TestRetiredNameIsTombstonedByARawSQLDelete.
	var resolved string
	if err := conn.QueryRow(ctx,
		`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE c.oid = 'agent_retired_names'::regclass`).Scan(&resolved); err != nil {
		t.Fatalf("resolve the unqualified name under the hijacked path: %v", err)
	}
	if resolved != shadowSchema {
		t.Fatalf("unqualified `agent_retired_names` resolves to %q, want %q — the search_path "+
			"hijack is not in effect and this test proves nothing", resolved, shadowSchema)
	}

	id := insertAgentRow(ctx, t, conn, name)
	if _, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id); err != nil {
		t.Fatalf("delete the agent under the hijacked search_path: %v", err)
	}

	var inPublic, inShadow int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&inPublic); err != nil {
		t.Fatalf("count public tombstones: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM `+shadowSchema+`.agent_retired_names WHERE name=$1`, name).Scan(&inShadow); err != nil {
		t.Fatalf("count shadow tombstones: %v", err)
	}

	if inPublic != 1 {
		t.Errorf("public.agent_retired_names holds %d rows for %q after the DELETE, want 1. The "+
			"trigger resolved its target through the CALLER's search_path, so the tombstone did "+
			"not land where PGStore.NameExists reads it — the name is back in the generator pool "+
			"and nothing reported a problem.", inPublic, name)
	}
	if inShadow != 0 {
		t.Errorf("%s.agent_retired_names holds %d rows for %q, want 0. The tombstone was written "+
			"into a schema the caller controls: any session that can create a schema can redirect "+
			"the ledger and silently free a retired name.", shadowSchema, inShadow, name)
	}
}

// TestRetiredNameStillAllowsInsertingARetiredName pins the decision NOT to
// refuse the insert.
//
// 🔴 IT IS A DECISION, NOT AN OMISSION, and it is the reason 0002 guards the
// GENERATOR rather than the table. The reserved `agent` agent is provisioned
// with a hand-set name (internal/api/chief_provision.go), so a BEFORE INSERT
// refusal on a retired name would make destroying and re-provisioning agent
// impossible with no override — an outage in exchange for a property the
// generator already has. If someone later "hardens" 0002 into a refusal, this
// test is what tells them what they broke.
//
// ⚠ IT IS AN INVARIANT GUARD, NOT REGRESSION COVERAGE: no bug ever made this
// insert fail. It is here to make the scope of 0002 machine-readable.
func TestRetiredNameStillAllowsInsertingARetiredName(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzreissued-byhand"
	clearName(ctx, t, conn, name)
	if _, err := conn.Exec(ctx,
		`INSERT INTO public.agent_retired_names (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		name); err != nil {
		t.Fatalf("seed the tombstone: %v", err)
	}

	// The insert the refusal would have blocked.
	id := insertAgentRow(ctx, t, conn, name)
	t.Cleanup(func() {
		if _, err := conn.Exec(db.AsMigratorForTest(context.Background()), `DELETE FROM public.agents WHERE id=$1`, id); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	var live string
	if err := conn.QueryRow(ctx, `SELECT name FROM public.agents WHERE id=$1`, id).Scan(&live); err != nil {
		t.Fatalf("read back the re-inserted agent: %v", err)
	}
	if live != name {
		t.Errorf("re-inserted agent name = %q, want %q", live, name)
	}
}

// ---------------------------------------------------------------------------
// The PRIVILEGE half of 0002: who can actually make the trigger write.
// ---------------------------------------------------------------------------

// The non-owner fixture. A LOGIN role holding ordinary table grants on `agents`
// and owning nothing — the shape of any deployment where the migrating role and
// the connecting role are not the same one, and the shape of a human at a psql
// prompt who was given write access to `agents` and nothing else.
const (
	nonOwnerRole     = "zz_muster_nonowner"
	nonOwnerPassword = "zz_nonowner"
	// A schema the non-owner OWNS, so it can create its own tables in it. That
	// is the capability the borrow test below needs, and it is deliberately not
	// `public` — PostgreSQL 15 already took CREATE on `public` away from PUBLIC,
	// so testing the borrow there would pass for a reason that has nothing to do
	// with this migration.
	nonOwnerSchema = "zz_nonowner_own"
)

// nonOwnerPool creates (once per server) the role above, re-grants it, and
// returns a pool connected AS that role.
//
// 🔴 EVERY CONTROL HERE EXISTS BECAUSE ITS ABSENCE MAKES THE TESTS BELOW PASS FOR
// THE WRONG REASON. A superuser bypasses privilege checks entirely; a role that
// happens to own the tables needs no grant; a role that already holds INSERT on
// the ledger would tombstone under SECURITY INVOKER too. Each is asserted false
// before anything is measured.
func nonOwnerPool(ctx context.Context, t *testing.T, owner *pgxpool.Conn) *pgxpool.Pool {
	t.Helper()

	// CREATE ROLE is CLUSTER-global, so it is created once and reused across runs;
	// the grants are per-database and idempotent. The role is never dropped —
	// dropping it would fail while any database still records a grant to it, and
	// the server these tests run against is a throwaway.
	for _, stmt := range []string{
		`DO $$ BEGIN
		     IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + nonOwnerRole + `') THEN
		         CREATE ROLE ` + nonOwnerRole + ` LOGIN PASSWORD '` + nonOwnerPassword + `';
		     END IF;
		 END $$`,
		`DO $$ BEGIN
		     EXECUTE format('GRANT CONNECT ON DATABASE %I TO ` + nonOwnerRole + `', current_database());
		 END $$`,
		`GRANT USAGE ON SCHEMA public TO ` + nonOwnerRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON public.agents TO ` + nonOwnerRole,
		`DROP SCHEMA IF EXISTS ` + nonOwnerSchema + ` CASCADE`,
		`CREATE SCHEMA ` + nonOwnerSchema + ` AUTHORIZATION ` + nonOwnerRole,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("non-owner fixture %q: %v\n\nThis fixture needs a role that may CREATE ROLE "+
				"(the throwaway Postgres in docker-compose.test.yml and the CI service container "+
				"both connect as the superuser POSTGRES_USER). It is NOT skipped when that is "+
				"missing: a skip here is invisible in a verdict, and the property below is the one "+
				"the migration is sold on.", stmt, err)
		}
	}
	t.Cleanup(func() {
		if _, err := owner.Exec(db.AsMigratorForTest(context.Background()),
			`DROP SCHEMA IF EXISTS `+nonOwnerSchema+` CASCADE`); err != nil {
			t.Errorf("cleanup DROP SCHEMA %s: %v", nonOwnerSchema, err)
		}
	})

	// Controls, in the order a wrong answer would be cheapest to misread.
	var super, owns, mayInsert bool
	if err := owner.QueryRow(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname=$1`, nonOwnerRole).Scan(&super); err != nil {
		t.Fatalf("read rolsuper: %v", err)
	}
	if super {
		t.Fatalf("%s is a SUPERUSER; it bypasses every privilege check, so the assertions below "+
			"would hold whatever the migration does", nonOwnerRole)
	}
	if err := owner.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_tables
		                WHERE schemaname='public'
		                  AND tablename IN ('agents','agent_retired_names')
		                  AND tableowner=$1)`, nonOwnerRole).Scan(&owns); err != nil {
		t.Fatalf("read table ownership: %v", err)
	}
	if owns {
		t.Fatalf("%s OWNS one of the tables under test; ownership carries every privilege, so "+
			"this fixture cannot tell a granted role from an owning one", nonOwnerRole)
	}
	if err := owner.QueryRow(ctx,
		`SELECT has_table_privilege($1, 'public.agent_retired_names', 'INSERT')`,
		nonOwnerRole).Scan(&mayInsert); err != nil {
		t.Fatalf("read ledger INSERT privilege: %v", err)
	}
	if mayInsert {
		t.Fatalf("%s already holds INSERT on public.agent_retired_names, so the tombstone would "+
			"be written even by a SECURITY INVOKER function — this fixture is measuring nothing",
			nonOwnerRole)
	}

	u, err := url.Parse(dbtest.DSN(t))
	if err != nil {
		t.Fatalf("parse the package DSN: %v", err)
	}
	u.User = url.UserPassword(nonOwnerRole, nonOwnerPassword)
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", nonOwnerRole, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRetiredNameIsTombstonedByANonOwnerRole is marginal-coverage path 1 taken
// literally: the hand-run `psql` DELETE, issued by a role that is not the one
// that ran the migration.
//
// 🔴 IT IS THE PATH THE FILE IS SOLD ON, AND IT DID NOT WORK. While
// muster_retire_agent_name() was SECURITY INVOKER, the tombstone INSERT ran with
// the DELETING role's privileges — so a role holding
// `GRANT SELECT, INSERT, UPDATE, DELETE ON agents` and nothing else got
// `permission denied for table agent_retired_names` and could not delete an agent
// at all. The `agents` row survived, which is why this was not an outage (the
// name stayed out of the pool, and muster's own server migrates and connects as
// one role) — but "a trigger fires for a DELETE issued by any call site, in any
// binary" was false for every role but one.
//
// The assertion is the OUTCOME, not the function's modifier: it holds for
// SECURITY DEFINER and equally for an explicit `GRANT INSERT` to the deleting
// role, which is the other way to satisfy it.
func TestRetiredNameIsTombstonedByANonOwnerRole(t *testing.T) {
	ctx, conn := pinnedConn(t)
	probe := nonOwnerPool(ctx, t, conn)

	const name = "zznonowner-delete"
	clearName(ctx, t, conn, name)

	var before int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&before); err != nil {
		t.Fatalf("count tombstones before: %v", err)
	}
	if before != 0 {
		t.Fatalf("%q is already tombstoned before the test acts (%d rows); the assertion below "+
			"would pass whether or not the delete retires it", name, before)
	}

	// Inserted BY THE OWNER, so the only thing under test is the DELETE.
	id := insertAgentRow(ctx, t, conn, name)

	tag, err := probe.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id)
	if err != nil {
		t.Fatalf("DELETE of agent %d as %s: %v\n\nThe trigger's INSERT ran with the DELETING "+
			"role's privileges, so the whole DELETE aborted and the agent still exists. This is "+
			"the hand-run `psql` DELETE that 0002's header calls its first marginal-coverage "+
			"path: for any role but the one that applied the migration it errors instead of "+
			"tombstoning.", id, nonOwnerRole, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("DELETE removed %d rows, want 1; nothing was deleted so the trigger had no "+
			"reason to fire", tag.RowsAffected())
	}

	var live, after int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agents WHERE id=$1`, id).Scan(&live); err != nil {
		t.Fatalf("count agents after: %v", err)
	}
	if live != 0 {
		t.Fatalf("agent %d survived a DELETE that reported %d row(s) affected", id, tag.RowsAffected())
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&after); err != nil {
		t.Fatalf("count tombstones after: %v", err)
	}
	if after != 1 {
		t.Fatalf("agent_retired_names holds %d rows for %q after a DELETE by %s, want 1. The name "+
			"is back in the generator pool, so the next generated agent can be its namesake and "+
			"inherit the ServiceAccount and cluster-scoped ClusterRoleBinding named after the "+
			"dead one.", after, name, nonOwnerRole)
	}
}

// TestANonOwnerRoleCanBothRetireANameAndReadTheLedger is the OTHER HALF of the
// test above, and it is one test rather than two on purpose: the property 0002
// is sold on is a ROUND TRIP, and each half passes on its own while the pair is
// broken.
//
// 🔴 SECURITY DEFINER FIXED THE WRITE AND LEFT THE READ BROKEN, WHICH READ AS
// "the file no longer has to know a role name it cannot know". It did not.
// PGStore.NameExists is a plain SELECT over `agent_retired_names` issued AS THE
// CONNECTING ROLE — there is no SECURITY DEFINER anywhere in that path, and the
// ledger is created with a grant to nobody but its owner. Measured on Postgres
// 16 with exactly the grants nonOwnerPool installs (`USAGE ON SCHEMA public` +
// `SELECT, INSERT, UPDATE, DELETE ON public.agents`, nothing else):
//
//	ERROR:  permission denied for table agent_retired_names
//
// So a deployment whose migrating role is not its connecting role — the shape
// nonOwnerPool's own doc describes — destroyed agents fine and then failed EVERY
// auto-named agent creation, because createAndDispatchAgent goes through
// BuildUniqueAgentName and BuildUniqueAgentName asks nothing but this predicate.
// 0002 answers it with `GRANT SELECT ON public.agent_retired_names TO PUBLIC`.
//
// ⚠ IT CALLS THE REAL PREDICATE RATHER THAN RESTATING ITS SQL, which is the one
// place this file departs from its own "do not import internal/agents" rule (see
// insertAgentRow). A copy of the query here would be a guard on a SPELLING: it
// would stay green if NameExists grew a third clause over another ungranted
// table, which is the identical defect one layer along. The external test
// package makes the import legal — internal/agents imports internal/db, not the
// reverse.
func TestANonOwnerRoleCanBothRetireANameAndReadTheLedger(t *testing.T) {
	ctx, conn := pinnedConn(t)
	probe := nonOwnerPool(ctx, t, conn)
	store := agents.NewPG(probe)

	const name = "zznonowner-readback"
	clearName(ctx, t, conn, name)

	// Control: the role must not already read the name as taken, or the
	// assertion after the delete would hold whether or not the delete retired it.
	switch exists, err := store.NameExists(ctx, name); {
	case err != nil:
		t.Fatalf("NameExists(%q) as %s, BEFORE anything is deleted: %v\n\n"+
			"The generator's only predicate cannot run at all for this role. Every agent "+
			"created without a hand-set name fails here, so a deployment whose migrating role "+
			"is not its connecting role cannot provision an agent.", name, nonOwnerRole, err)
	case exists:
		t.Fatalf("NameExists(%q)=true before the test acts", name)
	}

	// The WRITE half, as a precondition rather than as the claim: the owner
	// inserts, the non-owner deletes, and the trigger must tombstone.
	id := insertAgentRow(ctx, t, conn, name)
	if _, err := probe.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id); err != nil {
		t.Fatalf("DELETE of agent %d as %s: %v", id, nonOwnerRole, err)
	}

	// The READ half, which is the claim. The agents row is gone, so a `true` here
	// can only have come from the ledger.
	exists, err := store.NameExists(ctx, name)
	if err != nil {
		t.Fatalf("NameExists(%q) as %s, after it retired the name: %v\n\n"+
			"The delete tombstoned the name and the same role then cannot see it. Nothing in "+
			"the read path is SECURITY DEFINER, so this needs a grant on the ledger itself.",
			name, nonOwnerRole, err)
	}
	if !exists {
		t.Fatalf("NameExists(%q)=false for a name this role has just retired; the generator "+
			"would hand it straight back and the namesake inherits the ServiceAccount and "+
			"cluster-scoped ClusterRoleBinding named after the dead agent", name)
	}

	// End to end: the path the operator actually takes. BuildUniqueAgentName is
	// the only caller of NameExists, and createAndDispatchAgent is the only caller
	// of it.
	got, err := agents.BuildUniqueAgentName(ctx, store)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName as %s: %v\n\nThis is what every auto-named agent "+
			"creation runs; a 500 here is the operator-visible symptom.", nonOwnerRole, err)
	}
	if got == "" || got == name {
		t.Fatalf("BuildUniqueAgentName = %q; want a free name that is not the retired %q",
			got, name)
	}
}

// TestTheTombstoneFunctionCannotBeBorrowedByAnotherTable pins the BOUND on what
// SECURITY DEFINER widens.
//
// 🔴 IT IS A GUARD ON A HAZARD THIS CHANGE INTRODUCED, NOT ON A PRE-EXISTING
// DEFECT — labelled so, because the red/green matrix alone would not say it.
// `CREATE FUNCTION` grants EXECUTE to PUBLIC by default. While the function was
// SECURITY INVOKER that was harmless: a borrowed copy wrote with the borrower's
// own privileges and simply failed. Under SECURITY DEFINER it is an arbitrary
// INSERT into public.agent_retired_names AS THE OWNER, reachable by any role with
// CREATE on any schema — and since a tombstone is never freed, that is a durable
// poisoning of the name pool rather than a transient one. 0002's
// `REVOKE EXECUTE … FROM PUBLIC` is what refuses it.
//
// ⚠ THE POSITIVE CONTROL IS NOT OPTIONAL. "CREATE TRIGGER was refused" is also
// what a role that cannot create triggers at all looks like, and the refusal
// would then say nothing about the REVOKE. So the same role first attaches a
// function it IS allowed to execute, on the same table, and that must succeed.
func TestTheTombstoneFunctionCannotBeBorrowedByAnotherTable(t *testing.T) {
	ctx, conn := pinnedConn(t)
	probe := nonOwnerPool(ctx, t, conn)

	for _, stmt := range []string{
		`CREATE TABLE ` + nonOwnerSchema + `.borrowed (name TEXT)`,
		`CREATE FUNCTION ` + nonOwnerSchema + `.own_noop() RETURNS trigger
		     LANGUAGE plpgsql AS $$ BEGIN RETURN OLD; END; $$`,
	} {
		if _, err := probe.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q as %s: %v", stmt, nonOwnerRole, err)
		}
	}

	// Positive control: this role CAN create a trigger on this table.
	if _, err := probe.Exec(ctx,
		`CREATE TRIGGER own_trigger AFTER DELETE ON `+nonOwnerSchema+`.borrowed
		     FOR EACH ROW EXECUTE FUNCTION `+nonOwnerSchema+`.own_noop()`); err != nil {
		t.Fatalf("the positive control failed: %s cannot create a trigger on its OWN table with "+
			"its OWN function (%v), so a refusal below would say nothing about EXECUTE on "+
			"muster_retire_agent_name", nonOwnerRole, err)
	}

	_, err := probe.Exec(ctx,
		`CREATE TRIGGER borrow_trigger AFTER DELETE ON `+nonOwnerSchema+`.borrowed
		     FOR EACH ROW EXECUTE FUNCTION public.muster_retire_agent_name()`)
	if err == nil {
		// Demonstrate the harm rather than assert it, so the failure names what is
		// actually reachable rather than what is feared.
		const poison = "zzpoisoned-by-borrow"
		if _, e := conn.Exec(ctx,
			`DELETE FROM public.agent_retired_names WHERE name=$1`, poison); e != nil {
			t.Fatalf("clear the poison name: %v", e)
		}
		_, driveErr := probe.Exec(ctx,
			`INSERT INTO `+nonOwnerSchema+`.borrowed (name) VALUES ($1)`, poison)
		if driveErr == nil {
			_, driveErr = probe.Exec(ctx, `DELETE FROM `+nonOwnerSchema+`.borrowed WHERE name=$1`, poison)
		}
		var landed int
		if e := conn.QueryRow(ctx,
			`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, poison).Scan(&landed); e != nil {
			t.Fatalf("count the poisoned rows: %v", e)
		}
		t.Fatalf("%s attached public.muster_retire_agent_name() to a table it owns. EXECUTE is "+
			"granted to PUBLIC, so 0002's REVOKE is missing. Driving that trigger wrote %d "+
			"arbitrary row(s) into public.agent_retired_names (drive error: %v) — and a tombstone "+
			"is never freed, so every name written that way is removed from the generator pool "+
			"for good.", nonOwnerRole, landed, driveErr)
	}
	// The refusal must be THIS one — a privilege refusal naming the function —
	// and not, say, a missing-relation error that would also produce a non-nil err.
	if !strings.Contains(err.Error(), "muster_retire_agent_name") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("CREATE TRIGGER was refused, but not by the EXECUTE privilege this test is "+
			"about: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The SCHEMA precondition: 0002 hardcodes `public`, so it must refuse anywhere
// else rather than apply cleanly and break every later destroy.
// ---------------------------------------------------------------------------

// scratchDatabase creates an EMPTY database (no template) and returns a DSN for
// it, optionally pinning the connections' search_path.
//
// ⚠ IT CREATES AND DROPS A DATABASE, WHICH internal/dbtest DELIBERATELY AVOIDS —
// `DROP DATABASE` forces an immediate checkpoint across every database on the
// server. That cost is paid here because no cheaper fixture can observe the
// property: the package's own database was copied from an already-migrated
// template, so `public.agents` exists in it and the guard under test is
// unreachable there by construction.
func scratchDatabase(ctx context.Context, t *testing.T, name, searchPath string) string {
	t.Helper()
	base := dbtest.Base(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.EnvVar, err)
	}
	admin := *u
	// `template1` when the base database IS `postgres`, for the same reason
	// internal/dbtest steps aside: the maintenance connection must not sit on a
	// database anything else needs exclusive access to.
	if strings.TrimPrefix(u.Path, "/") == "postgres" {
		admin.Path = "/template1"
	} else {
		admin.Path = "/postgres"
	}

	adminPool, err := db.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v", err)
	}
	defer adminPool.Close()

	drop := `DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`
	if _, err := adminPool.Exec(ctx, drop); err != nil {
		t.Fatalf("%s: %v", drop, err)
	}
	if _, err := adminPool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create the scratch database %q: %v", name, err)
	}
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 60*time.Second)
		defer cancel()
		p, err := db.Connect(bg, admin.String())
		if err != nil {
			t.Errorf("cleanup: connect to drop %q: %v", name, err)
			return
		}
		defer p.Close()
		if _, err := p.Exec(bg, drop); err != nil {
			t.Errorf("cleanup: %s: %v", drop, err)
		}
	})

	scratch := *u
	scratch.Path = "/" + name
	if searchPath != "" {
		q := scratch.Query()
		q.Set("search_path", searchPath)
		scratch.RawQuery = q.Encode()
	}
	return scratch.String()
}

// TestMigrationRefusesASchemaWhoseAgentsTableIsNotInPublic is 0002's step-0
// guard, exercised end to end.
//
// 🔴 THE FAILURE IT MOVES. 0002 hardcodes `public` — the trigger body writes to
// `public.agent_retired_names` because that is where PGStore.NameExists reads.
// Applied through a connection carrying `?search_path=<other>`, the unqualified
// spelling of this file created its table and attached its trigger in THAT
// schema while the body still named `public`, and BOTH migrations applied
// cleanly with no warning. The deployment then discovered it at the first
// destroy: every `DELETE FROM agents` failing with
// `relation "public.agent_retired_names" does not exist`, on a database that
// worked before 0002. The guard turns that into a refusal at migration time.
//
// The two subtests are a pair. The refusal alone is indistinguishable from a
// migration that cannot apply to a fresh database at all, so the public case is
// the positive control that makes the refusal mean something — and it is a fresh
// EMPTY database, not the package's migrated one, so 0001 and 0002 both really
// run in it.
func TestMigrationRefusesASchemaWhoseAgentsTableIsNotInPublic(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)

	t.Run("public", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
		t.Cleanup(cancel)
		dsn := scratchDatabase(ctx, t, "ms_0002_guard_public", "")

		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect to the scratch database: %v", err)
		}
		t.Cleanup(pool.Close)

		if err := db.Migrate(ctx, pool, quiet); err != nil {
			t.Fatalf("migrating a FRESH database whose schema lands in `public` failed: %v — the "+
				"refusal asserted by the sibling subtest would then prove nothing, because this "+
				"migration would be refusing everything", err)
		}
		var ledger *string
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('public.agent_retired_names')::text`).Scan(&ledger); err != nil {
			t.Fatalf("look up the ledger: %v", err)
		}
		if ledger == nil {
			t.Fatal("0002 reported success but public.agent_retired_names does not exist")
		}
	})

	t.Run("non-public", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
		t.Cleanup(cancel)
		const appSchema = "musterapp"
		dsn := scratchDatabase(ctx, t, "ms_0002_guard_nonpublic", appSchema)

		// The schema has to exist before anything can be created in it, and it is
		// created through a connection that is NOT search_path-pinned so this step
		// cannot itself be what the guard is reacting to.
		bare, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse the scratch DSN: %v", err)
		}
		bare.RawQuery = ""
		setup, err := db.Connect(ctx, bare.String())
		if err != nil {
			t.Fatalf("connect to create the schema: %v", err)
		}
		if _, err := setup.Exec(ctx, `CREATE SCHEMA `+appSchema); err != nil {
			setup.Close()
			t.Fatalf("create schema %s: %v", appSchema, err)
		}
		setup.Close()

		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect with search_path=%s: %v", appSchema, err)
		}
		t.Cleanup(pool.Close)

		// Fixture controls. Without the first, the DSN parameter might simply have
		// been ignored and this would be a duplicate of the `public` subtest.
		var current string
		if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
			t.Fatalf("read current_schema(): %v", err)
		}
		if current != appSchema {
			t.Fatalf("current_schema()=%q, want %q — the search_path in the DSN is not in effect "+
				"and this subtest is not exercising the guard", current, appSchema)
		}
		var agents *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.agents')::text`).Scan(&agents); err != nil {
			t.Fatalf("look up public.agents: %v", err)
		}
		if agents != nil {
			t.Fatalf("public.agents already exists (%q) in a database this test just created; the "+
				"precondition the guard checks is not violated here", *agents)
		}

		err = db.Migrate(ctx, pool, quiet)
		if err == nil {
			t.Fatalf("migrating into schema %s SUCCEEDED. 0002 hardcodes `public`: its trigger "+
				"writes to public.agent_retired_names, which is the only table "+
				"PGStore.NameExists reads, while its table and trigger just landed in %s. "+
				"Nothing warned, and every later `DELETE FROM agents` on this database now "+
				"fails with `relation \"public.agent_retired_names\" does not exist` — a "+
				"deployment that could destroy an agent before 0002 and cannot after it.",
				appSchema, appSchema)
		}
		// 🔴 THIS GUARD'S OWN REFUSAL, BY SQLSTATE AND NOT BY WORDING — and the
		// SQLSTATE half is here because a mutation sweep proved the wording half
		// insufficient on its own. With step 0 weakened to an UNQUALIFIED
		// `to_regclass('agents')` the migration STILL fails, because the qualified
		// `DROP TRIGGER … ON public.agents` further down then raises Postgres's own
		// `relation "public.agents" does not exist`. That message also contains
		// "public.agents", so a substring check matched it and the mutant SURVIVED.
		// Postgres's code there is 42P01; a bare RAISE EXCEPTION is P0001, so the
		// code is what separates "the migration refused ON PURPOSE, naming the
		// cause" from "the migration fell over on the way past it".
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("0002 refused with a non-Postgres error, so it is not the step-0 "+
				"guard: %v", err)
		} else if pgErr.Code != "P0001" {
			t.Errorf("0002 refused with SQLSTATE %s (%q), want P0001 — the step-0 guard's own "+
				"RAISE. %s is what Postgres itself raises when a later statement trips over the "+
				"missing table, which is the failure this guard exists to pre-empt, not the "+
				"guard firing.", pgErr.Code, pgErr.Message, pgErr.Code)
		}
		if !strings.Contains(err.Error(), "public.agents") {
			t.Errorf("0002's refusal does not name public.agents, so it does not tell the "+
				"operator what to fix: %v", err)
		}

		// State, not just the message: nothing half-built.
		var ledger *string
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass($1)::text`, appSchema+".agent_retired_names").Scan(&ledger); err != nil {
			t.Fatalf("look up the ledger in %s: %v", appSchema, err)
		}
		if ledger != nil {
			t.Errorf("0002 refused but left %q behind; the migration runs as one transaction, so "+
				"a refusal must leave the schema untouched", *ledger)
		}
	})

	// 🔴 THE CASE STEP 0 CANNOT SEE, WHICH IS WHY THE `public.` QUALIFICATION IS
	// NOT REDUNDANT WITH IT. `public.agents` exists — an ordinary deployment,
	// already migrated — and someone adds `?search_path=<other>,public` to
	// DATABASE_URL, or sets it with ALTER ROLE/ALTER DATABASE. Step 0's check
	// passes, because it asks about `public.agents` and that really is there. But
	// `CREATE TABLE IF NOT EXISTS agent_retired_names` resolves its CREATION
	// namespace to the FIRST entry on the path and checks only that one, so
	// measured on Postgres 16 it creates a second, empty ledger in `<other>` even
	// though `public.agent_retired_names` already exists — while the trigger body
	// keeps writing to `public`. Qualified, it is a no-op. This subtest is what
	// pins that; without it the qualification would be unpinned defence.
	t.Run("search_path_preferring_another_schema", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
		t.Cleanup(cancel)
		const otherSchema = "musterpref"
		dsn := scratchDatabase(ctx, t, "ms_0002_guard_pref", "")

		// Phase 1: an ordinary, fully-migrated deployment, all of it in `public`.
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect to the scratch database: %v", err)
		}
		if err := db.Migrate(ctx, pool, quiet); err != nil {
			pool.Close()
			t.Fatalf("migrate the scratch database into public: %v", err)
		}
		if _, err := pool.Exec(ctx, `CREATE SCHEMA `+otherSchema); err != nil {
			pool.Close()
			t.Fatalf("create schema %s: %v", otherSchema, err)
		}
		pool.Close()

		// Phase 2: the same database, reached through a path that PREFERS the other
		// schema, and 0002's body re-applied by hand. `db.Migrate` would skip it —
		// the version is already recorded — so the file is read and executed
		// directly. That is also the restore / schema-rebuild case 0002's lock-budget
		// note calls out as the one where it really is re-run.
		body, err := os.ReadFile(filepath.Join("migrations", "0002_agent_retired_names.sql"))
		if err != nil {
			t.Fatalf("read 0002: %v", err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse the scratch DSN: %v", err)
		}
		q := u.Query()
		q.Set("search_path", otherSchema+",public")
		u.RawQuery = q.Encode()
		shifted, err := db.Connect(ctx, u.String())
		if err != nil {
			t.Fatalf("connect with the shifted search_path: %v", err)
		}
		t.Cleanup(shifted.Close)

		// Fixture controls: the path really is shifted, AND step 0's precondition
		// really is satisfied — so a pass below cannot be step 0 doing the work.
		var current string
		if err := shifted.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
			t.Fatalf("read current_schema(): %v", err)
		}
		if current != otherSchema {
			t.Fatalf("current_schema()=%q, want %q — the shifted search_path is not in effect",
				current, otherSchema)
		}
		var agents *string
		if err := shifted.QueryRow(ctx, `SELECT to_regclass('public.agents')::text`).Scan(&agents); err != nil {
			t.Fatalf("look up public.agents: %v", err)
		}
		if agents == nil {
			t.Fatal("public.agents does not exist, so step 0 would refuse and this subtest would " +
				"be a duplicate of the one above")
		}

		tx, err := shifted.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("re-apply 0002 through the shifted path: %v — it must stay idempotent; the "+
				"restore and schema-rebuild case re-runs this file by hand", err)
		}

		var stray *string
		if err := tx.QueryRow(ctx,
			`SELECT to_regclass($1)::text`, otherSchema+".agent_retired_names").Scan(&stray); err != nil {
			t.Fatalf("look up the ledger in %s: %v", otherSchema, err)
		}
		if stray != nil {
			t.Fatalf("re-applying 0002 created %q. The trigger body writes to "+
				"public.agent_retired_names — the only table PGStore.NameExists reads — so this "+
				"second ledger is written by nothing and read by nothing, while the tombstones "+
				"that matter keep landing in public. Step 0 cannot catch this: public.agents "+
				"exists, so its precondition is satisfied. Only the `public.` qualification on "+
				"the CREATE TABLE does.", *stray)
		}
	})
}
