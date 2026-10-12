package agents

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// The generator half of the agent-name tombstone: a destroyed agent's name must
// never be handed to a new agent.
//
// 🔴 WHY THESE ARE POSTGRES-BACKED AND NOT FAKE-STORE TESTS. The whole change
// lives in the SQL — PGStore.NameExists's second EXISTS and migration 0002's
// AFTER DELETE trigger. A fake store decides for itself what NameExists returns,
// so a fake-store version of the assertion below is GREEN against the broken
// code: it would be testing the test's own stub. The only fixture that can tell
// the fix from its absence is a real database that really deletes a row.
//
// 🔴 AND THE POOL IS NARROWED TO A SINGLE NAME SO THE ASSERTION IS REACHED. With
// the real 16x16 pool, "BuildUniqueAgentName did not return the retired name" is
// a 255/256 coin flip per draw and 10 draws make it a near-certainty — the test
// would pass against the bug ~96% of the time and record a KILL for the wrong
// reason under mutation. The word lists are package-level vars, so narrowing
// them to one adjective and one noun makes the generator's output the retired
// name or nothing — and the control assertion on generateName() proves the
// narrowing took effect rather than assuming it.

// narrowNamePool pins generateName() to exactly one output for the duration of a
// test and returns it. Restored by t.Cleanup.
//
// ⚠ IT MUTATES PACKAGE STATE, so a test using it must not call t.Parallel().
// None here does.
func narrowNamePool(t *testing.T, adj, noun string) string {
	t.Helper()
	prevAdj, prevNouns := adjectives, nouns
	adjectives = []string{adj}
	nouns = []string{noun}
	t.Cleanup(func() { adjectives, nouns = prevAdj, prevNouns })
	return adj + "-" + noun
}

// retiredNameTestStore opens this package's private Postgres, applies the
// migrations and returns a PGStore over it.
func retiredNameTestStore(t *testing.T) (context.Context, *PGStore) {
	t.Helper()
	dsn := dbtest.DSN(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB

	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, NewPG(pool)
}

// TestBuildUniqueAgentNameNeverReissuesADestroyedAgentsName is the closing
// condition's second limb: create an agent, record its name, delete it, and
// assert the generator cannot hand that name to anyone else.
//
// The four assertions are deliberately ordered cheapest-discriminator-first: the
// narrowing control, then that the row really is gone (so the live-rows half of
// the predicate cannot be what is answering), then the predicate itself, then
// the behaviour callers depend on.
func TestBuildUniqueAgentNameNeverReissuesADestroyedAgentsName(t *testing.T) {
	ctx, store := retiredNameTestStore(t)

	want := narrowNamePool(t, "zzretired", "tombstone")
	if got := generateName(); got != want {
		t.Fatalf("narrowNamePool did not take effect: generateName()=%q, want %q — every "+
			"assertion below would be about the wrong name", got, want)
	}

	// This package's private database is REUSED across runs, so a tombstone left
	// by an earlier run of THIS test would make every assertion below pass
	// without the delete under test having done anything.
	//
	// 🔴 ORDER: the agents row FIRST, the tombstone SECOND. Clearing the row fires
	// the trigger, which writes the very tombstone this is trying to remove.
	for _, stmt := range []string{
		`DELETE FROM public.agents WHERE name=$1`,
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
	} {
		if _, err := store.pool.Exec(ctx, stmt, want); err != nil {
			t.Fatalf("clear leftovers for %q with %q: %v", want, stmt, err)
		}
	}
	if free, err := store.NameExists(ctx, want); err != nil {
		t.Fatalf("NameExists(%q) during setup: %v", want, err)
	} else if free {
		t.Fatalf("%q is already taken before the test creates anything; the assertions below "+
			"would pass whether or not the delete retires it", want)
	}

	created, err := store.Create(ctx, Agent{
		Name:      want,
		Namespace: NamespaceFor(NamespacePrefix, want),
		Status:    StatusProvisioning,
	})
	if err != nil {
		t.Fatalf("create the agent whose name will be retired: %v", err)
	}
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete agent %d: %v", created.ID, err)
	}

	// The row is really gone — so nothing below can be answered by the live-rows
	// half of NameExists. This is the negative control for the fixture: if Delete
	// were a soft delete the rest of the test would prove nothing about tombstones.
	if _, err := store.GetByName(ctx, want); err == nil {
		t.Fatalf("GetByName(%q) still returns a row after Delete; this fixture cannot "+
			"distinguish a tombstone from a surviving row", want)
	}

	// The mechanism: the name is TAKEN even though no agent holds it.
	exists, err := store.NameExists(ctx, want)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", want, err)
	}
	// t.Errorf, NOT t.Fatalf, and deliberately: a Fatal here would stop the
	// BEHAVIOURAL assertion below from ever executing against broken code, so that
	// assertion could never be watched to fail and would survive mutation testing
	// while proving nothing.
	if !exists {
		t.Errorf("NameExists(%q)=false after the agent holding it was DELETED; the name is back "+
			"in the pool, so a namesake gets the same ServiceAccount and inherits the "+
			"cluster-scoped ClusterRoleBinding named after the dead agent "+
			"(internal/provision/k8s/driver.go)", want)
	}

	// The behaviour: with the pool narrowed to this one name, BuildUniqueAgentName
	// must not hand that name out.
	//
	// ⚠ THIS ASSERTION USED TO BE `err != nil`, AND THAT WAS A DIFFERENT CLAIM.
	// Erroring was the old fallback, and it made agent creation fail outright once
	// the pool ran down — a clock, because a tombstoned name is never freed. The
	// property that actually matters has never been "it errors"; it is "it does
	// not return THIS name", and that is what is asserted now. See
	// BuildUniqueAgentName's doc for the discriminator that replaced the error.
	got, err := BuildUniqueAgentName(ctx, store)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName = %v with the (narrowed) pool fully retired; it must fall "+
			"back to a discriminated name rather than fail — provisioning that cannot get a name "+
			"is an outage with no operator-visible cause", err)
	}
	if got == want {
		t.Fatalf("BuildUniqueAgentName returned %q — the name of an agent that was DELETED. A "+
			"namesake gets the same ServiceAccount and inherits the cluster-scoped "+
			"ClusterRoleBinding named after the dead agent", got)
	}
	if got != want+"-2" {
		t.Errorf("BuildUniqueAgentName = %q, want %q: the pool holds exactly one name and it is "+
			"retired, so the first free discriminated form is the only correct answer", got, want+"-2")
	}
}

// TestNameExistsTreatsARetiredNameAsTaken isolates the PREDICATE from the
// trigger: a name present ONLY in agent_retired_names — no agents row now and
// none in this test's history — must read as taken, and a name in neither table
// must read as free.
//
// The second half is the positive control for the instrument: without it, a
// NameExists hardwired to `return true, nil` would satisfy the first half.
func TestNameExistsTreatsARetiredNameAsTaken(t *testing.T) {
	ctx, store := retiredNameTestStore(t)

	const retired = "zzonly-inthetombstone"
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO public.agent_retired_names (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		retired); err != nil {
		t.Fatalf("seed agent_retired_names: %v", err)
	}

	exists, err := store.NameExists(ctx, retired)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", retired, err)
	}
	if !exists {
		t.Errorf("NameExists(%q)=false for a name in agent_retired_names; the generator would "+
			"reissue it", retired)
	}

	const free = "zznever-existedatall"
	exists, err = store.NameExists(ctx, free)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", free, err)
	}
	if exists {
		t.Errorf("NameExists(%q)=true for a name in neither agents nor agent_retired_names; this "+
			"predicate says every name is taken, which would exhaust the generator outright", free)
	}
}

// TestProvisioningStillGetsANameWhenTheENTIREPoolIsTombstoned is the end state
// the tombstone makes inevitable, exercised rather than reasoned about: every one
// of the 256 adjective-noun combinations sits in agent_retired_names, so no draw
// can ever succeed again.
//
// 🔴 THIS IS THE GUARD FOR THE EXHAUSTION CLOCK, AND IT IS POSTGRES-BACKED FOR
// THE SAME REASON THE REST OF THIS FILE IS. The question is not "does the Go
// loop count up" — a fake store answers that, and answers it against its own stub
// — but "does a discriminated candidate get tested against THE LEDGER". Only a
// real database with a real tombstone table can tell the two apart. Against the
// pre-fallback code this fails with `agents: could not generate a unique name`,
// which is what agent creation returned to the operator.
//
// ⚠ IT SEEDS AND THEN UNSEEDS. This package's database is REUSED across runs, so
// leaving 256 tombstones behind would shrink the pool for every later test in
// this package — and for the next run of this one. Only rows this test actually
// inserted are removed, so a name genuinely retired by another test survives.
func TestProvisioningStillGetsANameWhenTheENTIREPoolIsTombstoned(t *testing.T) {
	ctx, store := retiredNameTestStore(t)

	// Ordered iteration, not a map: a Go map's order is randomised per run, and a
	// fixture whose contents depend on iteration order is how a gate becomes
	// intermittently red and a mutation verdict becomes a coin flip.
	var pool []string
	for _, a := range adjectives {
		for _, n := range nouns {
			pool = append(pool, a+"-"+n)
		}
	}
	if want := len(adjectives) * len(nouns); len(pool) != want {
		t.Fatalf("built %d pool names, want %d", len(pool), want)
	}

	var seeded []string
	t.Cleanup(func() {
		if len(seeded) == 0 {
			return
		}
		// dbtest.AsOperator(context.Background()): ctx is cancelled by its own t.Cleanup, and which
		// cleanup runs first is not something this should depend on.
		cctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
		defer cancel()
		if _, err := store.pool.Exec(cctx,
			`DELETE FROM public.agent_retired_names WHERE name = ANY($1)`, seeded); err != nil {
			t.Errorf("unseed %d tombstone(s): %v — this package's database is reused, so the "+
				"pool stays shrunk for every later run", len(seeded), err)
		}
	})

	for _, name := range pool {
		var inserted string
		err := store.pool.QueryRow(ctx,
			`INSERT INTO public.agent_retired_names (name) VALUES ($1)
			 ON CONFLICT (name) DO NOTHING RETURNING name`, name).Scan(&inserted)
		switch {
		case err == nil:
			seeded = append(seeded, inserted)
		case err.Error() == "no rows in result set":
			// Already tombstoned by something else. Leave it alone.
		default:
			t.Fatalf("seed tombstone %q: %v", name, err)
		}
	}

	// The fixture's own control: with every draw guaranteed to collide, a name
	// that IS free must still read free, or the assertion below would pass
	// against a predicate hardwired to "taken".
	if exists, err := store.NameExists(ctx, "zzcontrol-neverseeded"); err != nil {
		t.Fatalf("NameExists control: %v", err)
	} else if exists {
		t.Fatal("NameExists says an unseeded name is taken; this fixture cannot tell a found " +
			"name from a refused one")
	}
	for _, name := range pool {
		if exists, err := store.NameExists(ctx, name); err != nil {
			t.Fatalf("NameExists(%q): %v", name, err)
		} else if !exists {
			t.Fatalf("%q reads FREE after seeding the whole pool; the draws below could succeed "+
				"without the fallback ever running", name)
		}
	}

	got, err := BuildUniqueAgentName(ctx, store)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName with all %d pool names tombstoned = %v. Provisioning must "+
			"not fail for want of a name: a retired name is never freed, so this is where every "+
			"deployment ends up, and the failure lands on the operator as \"creating an agent "+
			"stopped working\" with no cause anywhere in the UI", len(pool), err)
	}

	// It is a name the store agrees is free...
	if exists, err := store.NameExists(ctx, got); err != nil {
		t.Fatalf("NameExists(%q): %v", got, err)
	} else if exists {
		t.Errorf("BuildUniqueAgentName returned %q, which its own predicate calls TAKEN", got)
	}
	// ...it is not one of the retired ones...
	for _, name := range pool {
		if got == name {
			t.Fatalf("BuildUniqueAgentName returned %q, a tombstoned name", got)
		}
	}
	// ...and it is a name the provisioner will accept, which is the constraint
	// that decides whether the discriminator is usable at all.
	if err := (provision.Ref{Name: got}).Validate(); err != nil {
		t.Errorf("BuildUniqueAgentName returned %q, which provision.Ref refuses: %v — the "+
			"namespace, ServiceAccount and every RBAC object are named from it", got, err)
	}
	// ...of the expected discriminated shape: one of the pool's bases plus a
	// numeric suffix of at least the first discriminator.
	i := strings.LastIndex(got, "-")
	if i < 0 {
		t.Fatalf("BuildUniqueAgentName = %q, want a discriminated `<adjective>-<noun>-<n>`", got)
	}
	base, suffix := got[:i], got[i+1:]
	if !slices.Contains(pool, base) {
		t.Errorf("BuildUniqueAgentName = %q; %q is not one of the pool's names, so this is not "+
			"a discriminated form of a drawn name", got, base)
	}
	if d, err := strconv.Atoi(suffix); err != nil || d < firstDiscriminator {
		t.Errorf("BuildUniqueAgentName = %q; %q is not a discriminator of at least %d",
			got, suffix, firstDiscriminator)
	}
	t.Logf("whole pool (%d names) tombstoned; provisioning still got %q", len(pool), got)
}
