package agents

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strconv"

	"github.com/ZacxDev/muster/internal/provision"
)

// adjective-noun name parts, DNS-label-safe (lowercase alphanumeric) because
// provision.Ref.Name is used directly as an object, container or directory name
// by several drivers and is validated against that shape.
//
// ⚠ NO TENANT PREFIX. The generator this was modelled on prefixed each name
// with a tenant and a template id; muster is single-tenant, so the pair alone
// is the whole name. A multi-tenant deployment would have to widen this.
//
// ⚠ AND NOTHING WOULD SAY SO OUT LOUD ANY MORE. This note used to add that "the
// collision loop in BuildUniqueAgentName is what would notice first", which was
// true while that loop ERRORED on exhaustion. It now falls back to a
// discriminator, deliberately — see BuildUniqueAgentName — so a pool that is too
// small for its deployment presents as names quietly growing a numeric suffix,
// not as a failure. That is the right trade for provisioning and the wrong one
// for a warning: whoever makes muster multi-tenant gets no signal from here.
var (
	adjectives = []string{
		"swift", "brave", "clever", "calm", "bold", "bright", "keen", "lively",
		"nimble", "quiet", "sharp", "sturdy", "witty", "zesty", "mellow", "spry",
	}
	nouns = []string{
		"claw", "fox", "owl", "lynx", "otter", "hawk", "wren", "raven", "moth",
		"newt", "vole", "finch", "shrew", "stoat", "heron", "crane",
	}
)

// ChiefName is the one RESERVED agent name: the standing agent the operator
// talks to, as opposed to a worker dispatched against a task.
//
// 🔴 IT IS A CONSTANT BECAUSE THE INSTRUCTIONS BRANCH ON IT, AND A NAME THE
// GENERATOR PRODUCED WOULD SILENTLY FAIL THAT BRANCH. Ordinary agent names come
// from the adjective-noun pool above, so nothing a human can type reaches this
// value — POST /chief/provision is the only producer. Upstream, a release that
// reserved this name before anything could create one shipped an agent whose
// whole instruction set was unreachable: it was handed the WORKER instructions,
// dutifully asked for its assigned task, was told it had none, and reported to
// the operator that there was nothing to do. It was not malfunctioning; it was
// executing the only instructions it had.
//
// ⚠ IT IS NOT IN adjectives/nouns AND MUST NOT BE. The collision loop in
// BuildUniqueAgentName guards against an accidental duplicate of an EXISTING
// agent, not against the pool minting a reserved word — so a reserved name that
// the pool could also produce would be a coin flip, not a guard.
const ChiefName = "chief"

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

// generateName returns a random adjective-noun slug.
func generateName() string { return pick(adjectives) + "-" + pick(nouns) }

// nameDrawAttempts is how many times BuildUniqueAgentName DRAWS from the pool
// before it stops drawing and starts counting. It is not a cap on how hard it
// looks — see the discriminator loop, which has no fixed cap at all — it is only
// the point at which random sampling has stopped being the cheaper strategy.
const nameDrawAttempts = 10

// firstDiscriminator is the suffix the fallback starts at, so the first
// discriminated form of `brave-heron` is `brave-heron-2`. It starts at 2 because
// the undiscriminated name is conceptually the first one, and `-1` next to a
// bare name reads as though a `-0` were missing.
const firstDiscriminator = 2

// BuildUniqueAgentName returns a name that is not TAKEN, where taken means "held
// by a live agent, OR ever held by one that was destroyed".
//
// 🔴 A DESTROYED AGENT'S NAME IS NEVER REISSUED, AND THE REFUSAL IS THE WHOLE
// POINT OF THE LOOP RATHER THAN A TIDINESS RULE. The name is the provisioning
// driver's Ref.Name, so it is what the per-instance namespace and the
// ServiceAccount are named after. A namesake reuses both: whatever a partial
// Destroy left in that namespace, and anything bound to that ServiceAccount
// name out of band (a hand-made RoleBinding, say), passes to it silently. (The
// strongest version of this argument — muster's own granted ClusterRoleBindings
// outliving the row — went away with the privilege subsystem; the namespace and
// ServiceAccount reuse did not.) The
// enforcement lives in the STORE — PGStore.NameExists consults the
// agent_retired_names ledger that migration 0002's AFTER DELETE trigger fills —
// because a check written here would only cover this one call path, while the
// trigger covers every DELETE in any binary.
//
// 🔴 A RETIRED NAME IS NEVER FREED, SO THE POOL ONLY SHRINKS — AND THAT IS WHY
// THIS FUNCTION MUST NOT BE ABLE TO FAIL FOR WANT OF A NAME. 16 adjectives x 16
// nouns is 256 combinations. An earlier revision took ten draws and then
// returned an error, which with k of 256 consumed is a provisioning failure with
// probability (k/256)^10 — and since k only ever goes UP, that probability is a
// clock. The failure would land on the operator as "creating an agent stopped
// working", at a moment nobody chose, for a reason nothing in the UI names.
//
// So the draws are a FAST PATH, not the whole search. When all of them collide
// the last drawn name is DISCRIMINATED — `brave-heron` becomes `brave-heron-2`,
// `-3`, … — counting up until the store says free. The namespace that search
// walks is unbounded in every sense that matters:
//
//   - The only ceiling is provision.Ref's own, asked HERE rather than restated:
//     a candidate the provisioner would refuse is refused here instead of being
//     handed on. That package caps a name at 48 characters today and the longest
//     name this pool can produce is 12 (`clever-otter`), which leaves 35 digits
//     of discriminator — on the order of 10^35 names per base, against a pool
//     whose entire supply of bases is 256. It is not a cliff that has been
//     moved; it is a number no deployment reaches.
//     TestTheDiscriminatorCeilingIsDerivedFromProvisionRef re-derives it from
//     provision.Ref.Validate rather than trusting this paragraph.
//   - The discriminated candidate is tested through THE SAME PREDICATE as a
//     drawn one — store.NameExists, which consults live rows AND the tombstone
//     ledger. There is deliberately no second existence check here: one rule,
//     one place, or a discriminated name becomes the one path the tombstone does
//     not cover.
//
// 🔴 THE ONLY BOUND ON THE SEARCH IS REQUEST CANCELLATION. NOT A DEADLINE —
// THERE IS NONE ON THIS PATH. An earlier revision of this paragraph said "the
// caller already owns a deadline (see agentprovision's opCtx)". That is false:
// opCtx is never in this call path. BuildUniqueAgentName has exactly one caller,
// internal/api's createAndDispatchAgent, reached from handleAgentCreate and
// handleChiefProvision — both hand it `r.Context()`.
// cmd/muster-server sets only ReadHeaderTimeout on its http.Server: no
// WriteTimeout, no http.TimeoutHandler, and no context.WithTimeout anywhere
// between the handler and here. So `ctx` is done when the CLIENT DISCONNECTS,
// and at no other moment.
//
// That justification is therefore withdrawn rather than replaced. The loop has
// no deadline bound; it has a cancellation bound, and ctx.Err() is checked on
// every iteration so a cancelled caller gets an answer even from a store that
// ignores its context. TestBuildUniqueAgentNameStopsWhenTheCallerCancels is what
// pins that, and it is the whole of it.
//
// ⚠ WHY THAT IS TOLERATED, AS SCALE RATHER THAN AS A BOUND. Reaching a
// hundred discriminators on one base takes on the order of 25,600 agent
// creations, because the discriminators fill in across all 256 bases. The
// consumption rate measured on the live deployment is ≈19.5 names/month — TWO
// ENDPOINTS IN ONE 53-DAY WINDOW, quoted at that scope only — which puts 25,600
// creations a century out. A run of the loop costs one NameExists round trip per
// occupied discriminator on the drawn base, so the work per call is set by how
// many names have actually been issued, not by anything unbounded.
//
// ⚠ AND THE DECISION, STATED RATHER THAN ASSUMED: THE LOOP GETS NO CAP OF ITS
// OWN. A cap is a new exhaustion cliff, and the cliff is the thing this fallback
// exists to remove — the previous revision's error return was exactly that, and
// it made agent creation fail at a moment nobody chose. The case a cap would
// catch is a store whose NameExists answers `true` for everything: that spins,
// with nothing logged, until the request is cancelled. It is a BROKEN STORE, not
// a full pool, and this function is not the right place to diagnose one — it has
// no logger, and giving it one would thread a dependency through a pure function
// for a failure mode with no production instance. What it does instead is make
// the spin legible when it ends: the cancellation error names how many
// discriminators were tried.
//
// ⚠ IT DOES NOT CHANGE THE SHAPE OF AN ORDINARY NAME. The fast path returns
// exactly what it always did, so every name already issued, every namespace and
// ServiceAccount named after one, and every guard that asserts the adjective-noun
// format of generateName's output are untouched. The discriminator is what a
// deployment sees only after 10 consecutive collisions.
func BuildUniqueAgentName(ctx context.Context, store Store) (string, error) {
	var base string
	for i := 0; i < nameDrawAttempts; i++ {
		base = generateName()
		exists, err := store.NameExists(ctx, base)
		if err != nil {
			return "", err
		}
		if !exists {
			return base, nil
		}
	}

	// Every draw collided. Stop drawing and start counting.
	for d := firstDiscriminator; ; d++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("agents: still looking for a free name after %d colliding draws "+
				"and %d discriminators: %w", nameDrawAttempts, d-firstDiscriminator, err)
		}
		cand := base + "-" + strconv.Itoa(d)
		ref := provision.Ref{Name: cand}
		if err := ref.Validate(); err != nil {
			// Unreachable at any size a deployment can reach — see the ceiling
			// note above — but returning a name the provisioner will refuse is
			// worse than saying so here, where the arithmetic is written down.
			return "", fmt.Errorf("agents: no free name found; the next candidate %q is one the "+
				"provisioner would refuse, so the search cannot continue: %w", cand, err)
		}
		exists, err := store.NameExists(ctx, cand)
		if err != nil {
			return "", err
		}
		if !exists {
			return cand, nil
		}
	}
}
