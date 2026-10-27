package agents

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ZacxDev/muster/internal/provision"
)

func TestGenerateNameFormat(t *testing.T) {
	adj := map[string]bool{}
	for _, a := range adjectives {
		adj[a] = true
	}
	noun := map[string]bool{}
	for _, n := range nouns {
		noun[n] = true
	}
	// Generate many to exercise the random picker and assert the shape holds.
	for i := 0; i < 200; i++ {
		name := generateName()
		parts := strings.Split(name, "-")
		if len(parts) != 2 {
			t.Fatalf("generateName()=%q, want adjective-noun (two parts)", name)
		}
		if !adj[parts[0]] {
			t.Errorf("generateName()=%q: %q not in adjectives", name, parts[0])
		}
		if !noun[parts[1]] {
			t.Errorf("generateName()=%q: %q not in nouns", name, parts[1])
		}
	}
}

func TestBuildUniqueAgentNameFirstTry(t *testing.T) {
	f := newFakeStore()
	// No name is taken → first candidate is returned.
	name, err := BuildUniqueAgentName(context.Background(), f)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName: %v", err)
	}
	if name == "" {
		t.Fatal("BuildUniqueAgentName returned an empty name")
	}
}

func TestBuildUniqueAgentNameRetriesOnCollision(t *testing.T) {
	var calls int
	f := newFakeStore()
	// First two candidates are "taken", the third is free — must return the third.
	f.nameExistsFn = func(name string) (bool, error) {
		calls++
		return calls <= 2, nil
	}
	name, err := BuildUniqueAgentName(context.Background(), f)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName: %v", err)
	}
	if name == "" {
		t.Fatal("expected a non-empty name after collisions")
	}
	if calls != 3 {
		t.Errorf("NameExists called %d times, want 3 (two collisions then a free name)", calls)
	}
}

// TestBuildUniqueAgentNameDiscriminatesWhenEveryDrawCollides is the regression
// guard for the exhaustion clock. Before the fallback existed this function
// returned `agents: could not generate a unique name` once every draw collided —
// i.e. agent creation FAILED, with a probability that only ever rises because a
// tombstoned name is never freed.
//
// 🔴 THE POOL IS NARROWED TO ONE NAME SO THE ASSERTION IS EXACT. With the real
// 16x16 pool the ten draws would land on ten different bases and the returned
// discriminated name would be unpredictable, so the only available assertion
// would be a shape check — which passes for the wrong base. Narrowed, the whole
// search is deterministic: `brave-heron` and `brave-heron-2` are taken, so the
// answer can only be `brave-heron-3`.
func TestBuildUniqueAgentNameDiscriminatesWhenEveryDrawCollides(t *testing.T) {
	base := narrowNamePool(t, "brave", "heron")
	if got := generateName(); got != base {
		t.Fatalf("narrowNamePool did not take effect: generateName()=%q, want %q — the "+
			"assertions below would be about the wrong name", got, base)
	}

	var asked []string
	f := newFakeStore()
	f.nameExistsFn = func(name string) (bool, error) {
		asked = append(asked, name)
		return name == base || name == base+"-2", nil
	}

	got, err := BuildUniqueAgentName(context.Background(), f)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName with the whole pool taken = %v; agent creation must not "+
			"fail for want of a name — the pool is finite and only ever shrinks, so an error "+
			"here is a clock, not a boundary condition", err)
	}
	if want := base + "-3"; got != want {
		t.Fatalf("BuildUniqueAgentName = %q, want %q (the base and -2 are taken, -3 is free)", got, want)
	}

	// The SAME predicate, not a second one. A discriminated candidate that
	// skipped store.NameExists would be the one name the tombstone ledger does
	// not cover, which is the whole hazard this file's other guards are about.
	wantAsked := map[string]bool{base + "-2": false, base + "-3": false}
	for _, n := range asked {
		if _, ok := wantAsked[n]; ok {
			wantAsked[n] = true
		}
	}
	for n, seen := range wantAsked {
		if !seen {
			t.Errorf("store.NameExists was never asked about %q (asked: %v); a discriminated "+
				"candidate that is not put through the store's predicate is not checked against "+
				"the retired-name ledger at all", n, asked)
		}
	}
	// The fast path must still have been TEN draws, not fewer: shortening it
	// would change how often an ordinary name gets a suffix.
	//
	// ⚠ 12 IS A LITERAL ON PURPOSE. Writing `nameDrawAttempts+2` would make this
	// assertion read the very constant it is checking, so a mutant that changes
	// the constant moves both sides and survives a green suite.
	if len(asked) != 12 {
		t.Errorf("store.NameExists called %d time(s), want 12 (%d draws, then -2 and -3); "+
			"nameDrawAttempts is %d", len(asked), 10, nameDrawAttempts)
	}
}

// buildNameWithin runs BuildUniqueAgentName under a watchdog.
//
// 🔴 IT EXISTS BECAUSE THE SEARCH IS DELIBERATELY UNBOUNDED, so a test whose
// fixture removes the thing that stops it does not fail — it HANGS, and `go
// test` reports a package-level timeout panic instead of the assertion that was
// supposed to catch it. That is the difference between a mutant scored KILLED
// and one scored ERROR, and between a readable failure and a stack dump.
func buildNameWithin(t *testing.T, ctx context.Context, store Store, d time.Duration) (string, error) {
	t.Helper()
	type result struct {
		name string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := BuildUniqueAgentName(ctx, store)
		ch <- result{n, err}
	}()
	select {
	case r := <-ch:
		return r.name, r.err
	case <-time.After(d):
		t.Fatalf("BuildUniqueAgentName did not return within %s. The discriminator search has "+
			"exactly two stopping conditions — a free name, and a candidate provision.Ref "+
			"refuses — and this fixture leaves neither, so one of them is gone", d)
		return "", nil
	}
}

// TestBuildUniqueAgentNameRefusesANameTheProvisionerWouldReject reaches the one
// branch of the fallback that no ordinary deployment can.
//
// 🔴 A GUARD THAT CANNOT BE REACHED IS NOT A GUARD, AND THIS ONE IS UNREACHABLE
// BY ARITHMETIC: the longest name the real pool can produce is 12 characters and
// provision.Ref allows 48, so the discriminator would have to pass 10^35 before
// the length check could fire. Narrowing the pool to a base that is already AT
// the cap is what makes the branch executable, so it can be watched to fail with
// its OWN message rather than being certified by reading it.
func TestBuildUniqueAgentNameRefusesANameTheProvisionerWouldReject(t *testing.T) {
	// A base exactly at provision.Ref's cap: any suffix overflows it. Sized by
	// asking provision.Ref, not by restating its constant.
	adj, noun := "a", "b"
	for {
		cand := adj + "a-" + noun
		if err := (provision.Ref{Name: cand}).Validate(); err != nil {
			break
		}
		adj += "a"
	}
	base := narrowNamePool(t, adj, noun)
	if err := (provision.Ref{Name: base}).Validate(); err != nil {
		t.Fatalf("the fixture base %q (%d chars) is already invalid: %v", base, len(base), err)
	}
	if err := (provision.Ref{Name: base + "-2"}).Validate(); err == nil {
		t.Fatalf("the fixture base %q (%d chars) still admits a discriminator; this test cannot "+
			"reach the branch it exists for", base, len(base))
	}

	f := newFakeStore()
	f.nameExistsFn = func(string) (bool, error) { return true, nil }

	got, err := buildNameWithin(t, context.Background(), f, 15*time.Second)
	if err == nil {
		t.Fatalf("BuildUniqueAgentName = %q with no valid candidate left; a name the "+
			"provisioner refuses would fail at Ref.Validate three packages later, in the "+
			"middle of a create", got)
	}
	if got != "" {
		t.Errorf("BuildUniqueAgentName returned both a name (%q) and an error (%v)", got, err)
	}
	// THIS guard's own message, not a neighbour's: the exhaustion path and the
	// length path must be distinguishable in a log.
	if !strings.Contains(err.Error(), "the provisioner would refuse") {
		t.Errorf("BuildUniqueAgentName error = %v; want the message from the Ref.Validate "+
			"branch, so an operator can tell it from a store failure", err)
	}
	if !strings.Contains(err.Error(), base+"-2") {
		t.Errorf("BuildUniqueAgentName error = %v; want it to name the candidate %q it refused",
			err, base+"-2")
	}
}

// TestBuildUniqueAgentNameStopsWhenTheCallerCancels pins the ONLY bound on the
// discriminator search. It is unbounded by design — the caller owns the deadline
// — so a store that keeps answering "taken" must still terminate for a caller
// that has given up. The fake store ignores ctx entirely, which is the case
// ctx.Err() inside the loop exists for.
func TestBuildUniqueAgentNameStopsWhenTheCallerCancels(t *testing.T) {
	f := newFakeStore()
	f.nameExistsFn = func(string) (bool, error) { return true, nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := buildNameWithin(t, ctx, f, 15*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("BuildUniqueAgentName error = %v, want the caller's context.Canceled", err)
	}
	if got != "" {
		t.Errorf("BuildUniqueAgentName returned both a name (%q) and an error (%v); a caller that "+
			"ignores the error would provision it", got, err)
	}
}

// TestTheDiscriminatorCeilingIsDerivedFromProvisionRef re-derives the fallback's
// only ceiling from the package that OWNS it, rather than restating a number.
//
// 🔴 THE POINT IS THAT THE CEILING IS NOT A MOVED CLIFF. The claim in
// BuildUniqueAgentName's doc is that the discriminator space per base is so large
// that no deployment reaches it; this measures the space instead of asserting it,
// and it fails if provision ever tightens Ref so far that the claim stops holding.
func TestTheDiscriminatorCeilingIsDerivedFromProvisionRef(t *testing.T) {
	longest := ""
	for _, a := range adjectives {
		for _, n := range nouns {
			if name := a + "-" + n; len(name) > len(longest) {
				longest = name
			}
		}
	}
	if longest == "" {
		t.Fatal("the name pool is empty; this test is not measuring anything")
	}
	if err := (provision.Ref{Name: longest}).Validate(); err != nil {
		t.Fatalf("the longest name the pool can produce (%q) is already not a valid "+
			"provision.Ref: %v", longest, err)
	}

	// The widest discriminator that still fits, found by asking provision.Ref
	// rather than by arithmetic over a constant this test would then own.
	digits := 0
	for {
		cand := longest + "-" + strings.Repeat("9", digits+1)
		if err := (provision.Ref{Name: cand}).Validate(); err != nil {
			break
		}
		digits++
		if digits > 200 {
			t.Fatalf("provision.Ref accepted a %d-digit discriminator; this loop is not "+
				"measuring a bound", digits)
		}
	}
	t.Logf("longest pool name %q (%d chars); provision.Ref admits %d discriminator digits, "+
		"i.e. ~1e%d names per base against a pool of %d bases",
		longest, len(longest), digits, digits, len(adjectives)*len(nouns))

	// A floor, not the measured value: the assertion is "astronomically more
	// than the pool", and pinning the exact digit count would just re-encode
	// provision's constant here.
	const wantAtLeast = 20
	if digits < wantAtLeast {
		t.Errorf("provision.Ref leaves room for only %d discriminator digits after the longest "+
			"pool name %q; at fewer than %d the fallback is a moved cliff rather than an "+
			"unbounded namespace, and BuildUniqueAgentName's doc claims otherwise",
			digits, longest, wantAtLeast)
	}
	// And the boundary itself: one digit more must be refused, so the loop above
	// found a real edge and not the end of its own patience.
	overflow := longest + "-" + strings.Repeat("9", digits+1)
	if err := (provision.Ref{Name: overflow}).Validate(); err == nil {
		t.Errorf("provision.Ref accepted %q (%d chars), so the ceiling measured above is not "+
			"where this test believes it is", overflow, len(overflow))
	}
}

// TestEveryNameThisGeneratorCanProduceSurvivesItsConsumers walks the WHOLE pool,
// undiscriminated and discriminated, against the two validators that decide
// whether a name can become cluster objects.
//
// 🔴 IT ASSERTS STATE, NOT SPELLING. `agent` is refused by identity — the name a
// reserved-agent branch compares against — rather than by a rule about dashes
// that a future pool entry could satisfy while still colliding.
func TestEveryNameThisGeneratorCanProduceSurvivesItsConsumers(t *testing.T) {
	// Discriminators at the bottom, at a decade boundary and well past one, so
	// the check is not a single length.
	discriminators := []int{firstDiscriminator, 9, 10, 99, 100, 999999}
	checked := 0
	for _, a := range adjectives {
		for _, n := range nouns {
			base := a + "-" + n
			cands := []string{base}
			for _, d := range discriminators {
				cands = append(cands, base+"-"+strconv.Itoa(d))
			}
			for _, cand := range cands {
				checked++
				if err := (provision.Ref{Name: cand}).Validate(); err != nil {
					t.Errorf("%q is not a valid provision.Ref: %v — the driver names the "+
						"namespace, the ServiceAccount and every RBAC object after it", cand, err)
				}
				// internal/provision/k8s/policy.go writes the instance name
				// verbatim into the muster.dev/policy-subject label, and
				// revokeAllPolicies SELECTS on that label. A name the apiserver
				// refuses as a label value makes the grant fail and the teardown
				// unable to find what it created.
				if msgs := validation.IsValidLabelValue(cand); len(msgs) > 0 {
					t.Errorf("%q is not a valid label value (%s); the policy labels carry the "+
						"agent name verbatim and the teardown selector reads one of them",
						cand, strings.Join(msgs, "; "))
				}
			}
		}
	}
	if want := len(adjectives) * len(nouns) * (1 + len(discriminators)); checked != want {
		t.Fatalf("checked %d candidate(s), want %d — this walk is not covering the pool it "+
			"believes it is", checked, want)
	}
}

func TestBuildUniqueAgentNameStoreError(t *testing.T) {
	sentinel := errors.New("db down")
	f := newFakeStore()
	f.nameExistsFn = func(name string) (bool, error) { return false, sentinel }
	_, err := BuildUniqueAgentName(context.Background(), f)
	if !errors.Is(err, sentinel) {
		t.Errorf("BuildUniqueAgentName error = %v, want the store error propagated", err)
	}
}
