// Package agentprovision adapts a [provision.Provisioner] DRIVER to the
// LIFECYCLE interface the agent HTTP handlers depend on (api.Provisioner):
// seven methods over an agent's int64 database id, rather than over a
// [provision.Ref] and a [provision.Spec].
//
// 🔴 IT IS THE LIFECYCLE HALF ONLY, AND THAT IS WHY IT CAN EXIST AT ALL. The
// consumer interface used to carry Chat and ChatWithTools as well, behind one
// nil and one wrapper, so nothing could wire provisioning without also claiming
// a model gateway this module has no implementation of. api.Provisioner and
// api.Gateway are now separate interfaces with separate nils and separate
// wrappers; this type satisfies the first and deliberately not the second, so a
// deployment gets working lifecycle routes and an honest 503 on the two chat
// ones. See internal/api/ext.go on api.Gateway.
//
// 🔴 WHAT THIS PACKAGE MUST NEVER DO, STATED BEFORE ANYTHING ELSE: it must
// never call agents.Store.SetKickedOff. That column answers "was the kickoff
// message HANDED TO A READY GATEWAY" — see agents.Agent.KickedOff and
// agents.Store.SetKickedOff — and this package has no gateway, so setting it
// would be a claim about a delivery that did not happen. That is the same class
// of lie the provisioner wrappers exist to prevent (a 200 over a pod that does
// not exist), one field down. It is not left to a reader's care:
// TestTheAdapterNeverClaimsAKickoffItCannotDeliver asserts it over the whole
// method set.
//
// WHAT HAPPENS TO A PENDING KICKOFF INSTEAD: the instance is created, the note
// stays in agents.pending_note where it was written, and the non-delivery is
// RECORDED on the row via agents.Store.SetKickoffError — the field whose own doc
// calls it "evidence, not a verdict".
//
// 🔴 THAT IS NOW THE *DELIVERABLE* PATH ONLY, AND THE PARAGRAPH ABOVE IS KEPT
// RATHER THAN REWRITTEN BECAUSE EVERYTHING BELOW IT ARGUES FROM IT. When nothing in
// this process could deliver a kickoff, Dispatch(kickoff=true) no longer creates
// anything: it REFUSES. See [Config.KickoffDeliverable], [ErrKickoffUndeliverable]
// and [KickoffRefusalReason].
//
// ✅ AND THE DELIVERABLE ARM IS NOW CREATE-THEN-DELIVER, NOT CREATE-THEN-RECORD.
// internal/agentkickoff is the deliverer [KickoffDeliveryWired] waited for:
// cmd/muster-server runs it whenever it builds a gateway, so on a deployment that
// names one, Dispatch(kickoff=true) creates the instance, stores `provisioning`, and
// the deliverer hands the pending note to the instance once it is ready. Nothing is
// written to kickoff_error on that path unless a delivery actually fails. With no
// gateway named the dispatch is still REFUSED, as below.
//
// WHY THE CREATE-THEN-RECORD BEHAVIOUR WAS THE WRONG ANSWER TO "I CANNOT DELIVER
// THIS": it answered a request this configuration cannot satisfy by performing the
// expensive, credential-holding, healthy-looking HALF of it. The surface an operator
// actually drives is a Dispatch button that always appears to work; the cost per
// click is a pod with a cloned repository and a model credential that will never be
// told what to do; and the only evidence was this pod's stdout plus a machine-tier
// field, both of which the paragraphs below correctly call unread. A request that
// cannot do the thing it was asked to do should say so, not do something else.
//
// 🔴 AND THE CONDITION IS CONFIGURATION, NOT A MISSING IMPLEMENTATION.
// internal/agentgateway is shipped and live-verified; MUSTER_AGENT_GATEWAY being
// unset is the whole of why nothing can deliver. Both the refusal text and the
// constant below say so, because the earlier wording ("this build cannot deliver")
// reads as an unimplemented feature and sends its reader to write code that exists.
//
// 🔴 AND NOTHING RENDERS THAT FIELD TO A HUMAN. THIS PARAGRAPH PREVIOUSLY CLAIMED
// IT DID, AND THE CLAIM WAS MEASURED FALSE — it is recorded here rather than
// quietly corrected, because it is the most load-bearing sentence in this doc.
// It read: "rendered into the agent's error line by agents.KickoffErrorSuffix. So
// the operator sees WHY the first turn never ran rather than a card that sits in
// `provisioning` with nothing written anywhere." Three things are wrong with that:
//
//   - agents.KickoffErrorSuffix has NO caller anywhere in this module. Its only
//     references are agents/reconcile_test.go and prose; the reconcile loop that
//     would call it is explicitly OWED in reconcile.go's own header.
//   - Agent.KickoffError reaches exactly ONE surface — GET /api/agents, which is
//     hook-token-gated MACHINE tier (internal/api/machine_agents.go). No HTML view
//     carries an agent error field at all.
//   - the card does NOT "sit in provisioning". agents.ComputeStatus refines a live
//     ready instance to `running`, so the card reads HEALTHY while the first turn
//     never happened. That is worse than the state the sentence claimed to fix.
//
// So: the field is where a MACHINE or an operator with a token can read it, and
// the boot banner names it — which is honest, because the banner names a field and
// not a screen. Nothing on any page says it. Do not restate the rendering claim
// without a caller to cite.
//
// ✅ THE FIRST AND THIRD BULLETS ARE CLOSED NOW, THE SECOND IS NOT — read which is
// which before quoting any of it. agents.KickoffErrorSuffix has a caller:
// internal/agentkickoff appends it to the error_message it writes when a first turn
// never arrives, so an undelivered kickoff DOES escalate to `error` once it has
// dwelt past agents.ProvisioningStuckTimeout — a red status DOT on the card, with
// the last send failure in error_message (machine tier only). And the card no
// longer reads healthy over an owed turn: agents.KickoffOwed renders a "kickoff owed"
// badge. What is unchanged is the second bullet: the REASON in kickoff_error reaches
// one hook-token-gated JSON route and no page.
package agentprovision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/scrubexec"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultOpTimeout bounds one lifecycle operation.
//
// 🔴 IT EXISTS BECAUSE FOUR OF THE SEVEN METHODS TAKE NO context.Context. The
// consumer interface's Dispatch/Start/Stop/Destroy are called from handler
// goroutines that hand over nothing, so without a budget here a wedged backend
// call holds a goroutine for the life of the process — and the handler has
// already returned, so nothing is left to cancel it. The driver operations under
// it APPLY objects rather than wait for readiness (see the kubernetes driver's
// Create), so this is generous rather than tight on purpose: it is a leak guard,
// not a latency policy.
const DefaultOpTimeout = 5 * time.Minute

// bookkeepingTimeout bounds the store write that RECORDS what an operation did.
// See bookkeepingCtx for why it is a separate budget rather than the operation's.
const bookkeepingTimeout = 15 * time.Second

// hooksTokenBytes is the entropy of a minted per-agent token, in bytes. 32 bytes
// renders as the 64 hex characters the deployment's other tokens use.
const hooksTokenBytes = 32

// UndeliveredKickoffReason is written to agents.kickoff_error when a START brings
// up an instance that owes a first turn on a deployment with no gateway. (A
// DISPATCH asking for one there is refused instead and creates nothing.)
//
// ⚠ "Start this agent again" IN THE REMEDY IS LOAD-BEARING: Start rewrites the
// row's status, which restarts the dwell clock agents.DecideReconcile bounds the
// first-turn retry by. A row last started more than agents.ProvisioningStuckTimeout
// before a gateway was named is otherwise ERRORED by the deliverer's first tick
// rather than delivered — that is the table's verdict on a ready instance that
// never got its turn, and the remedy routes around it rather than hiding it.
//
// 🔴 IT NAMES THE DEPLOYMENT, NOT THE AGENT, BECAUSE THE CONDITION IS A PROPERTY
// OF THE DEPLOYMENT. An operator reading it on a card must not go looking at the agent's
// pod, its model or its credentials — none of them is why the turn did not run.
// Same reasoning as api.ProvisionerUnwiredField being a field rather than a
// sentence: the reader has to be able to tell a permanent property of the
// deployment from a transient failure of this agent.
const UndeliveredKickoffReason = "kickoff NOT delivered: this deployment names no agent gateway " +
	"(MUSTER_AGENT_GATEWAY), so nothing can hand the pending note to the instance's model gateway. " +
	"The instance WAS created and the note is still in agents.pending_note. Name a gateway, restart " +
	"muster and Start this agent again: the kickoff deliverer hands the note over once the instance " +
	"is ready. This is a property of the deployment, not a failure of this agent: see " +
	"cmd/muster-server/doc_seams.go entry 1."

// ErrKickoffUndeliverable is returned by [Adapter.Dispatch] when it is asked to
// begin work — kickoff=true — in a process that has no gateway to begin it with.
//
// 🔴 IT IS A REFUSAL, NOT A FAILURE, AND THE DIFFERENCE IS WHAT IT PROTECTS. A
// failure means the request was reasonable and something broke. This means the
// request can NEVER succeed on this configuration, so doing half of it is the
// wrong answer: the half that works (create the pod, clone the repository into
// it, hand it a model credential) is exactly the half that costs money and holds
// a secret, while the half that matters — telling the agent what to do — cannot
// happen at all.
//
// ⚠ THIS SENTENCE SAID "and reads HEALTHY on every page", AND THAT CLAUSE IS NO
// LONGER TRUE — the clause is removed rather than kept, because unlike the
// retracted paragraphs in this file's header it was not load-bearing for anything
// else. Both tiers now show agents.KickoffOwed, so such a pod reads `running` AND
// `kickoff owed`. It does NOT weaken the refusal: the cost, the secret and the
// impossibility of the asked-for turn are each sufficient on their own, and a
// badge naming the gap is not a reason to create the gap.
//
// ⚠ IT IS A SENTINEL SO A CALLER CAN TELL IT FROM A DRIVER ERROR. Nothing in this
// module matches on it yet: the only consumer of Dispatch's error is a background
// goroutine in internal/api that logs it (see [KickoffRefusalReason]'s own note on
// what that costs and who owes the rest). It is exported now because the caller
// that WILL match on it must not have to reach for a string.
var ErrKickoffUndeliverable = errors.New("agentprovision: kickoff undeliverable")

// KickoffDeliveryWired reports whether THIS MODULE contains production code that
// hands an agent's pending note to its model gateway — a kickoff DELIVERER.
//
// ✅ IT IS TRUE: internal/agentkickoff is that deliverer. cmd/muster-server builds
// it whenever a gateway is built and runs it as a background loop, which drives
// agents.DecideReconcile over owned, never-kicked-off rows and delivers through the
// same agentgateway.Gateway that POST /api/agents/{name}/messages calls, using
// Chat's two halves (Resolve before the delivered stamp, Send after it).
//
// 🔴 IT IS STILL ONLY THE SECOND HALF OF THE PREDICATE. Delivering a kickoff needs
// a gateway this process can reach AND code that calls it; cmd/muster-server passes
// `gw != nil && KickoffDeliveryWired` into [Config.KickoffDeliverable]. With
// MUSTER_AGENT_GATEWAY unset there is no gateway, so a dispatch asking for a first
// turn is still REFUSED — see [KickoffRefusalReason]. When this was false, the
// first conjunct alone had been passed and a deployment WITH a gateway created pods
// nobody ever told what to do; the two-conjunct form is what closed that.
//
// ⚠ THIS PACKAGE DOES NOT READ IT AND MUST NOT. The adapter answers the question
// from what it is TOLD ([Config.KickoffDeliverable]); its only reader is
// cmd/muster-server/provisioner.go.
//
// 🔴 IT IS BOUND TO A MEASUREMENT, IN BOTH DIRECTIONS. internal/modulegate's
// TestKickoffDeliveryLedgerAgreesWithTheModule walks every non-test file for CALLS
// to the two store writes only a delivery makes (agents.Store.SetKickedOff,
// agents.Store.RecordKickoffDelivery) and fails when the measured set disagrees with
// this constant: deleting the deliverer without setting this false reddens it, and
// so does the reverse. A deliverer that exists but is not STARTED is the other half
// of that hazard, and cmd/muster-server's TestADeliverableAdapterAlwaysComesWithARunningDeliverer
// is what pins it.
const KickoffDeliveryWired = true

// KickoffRefusalSummary is the SHORT operator-facing form of the refusal: what
// happened, and what to do instead. [KickoffRefusalReason] is this string plus the
// mechanism, and nothing else.
//
// 🔴 IT EXISTS BECAUSE THE FULL REASON IS ~900 CHARACTERS AND THE SURFACE THAT NOW
// SHOWS IT IS A TOAST. internal/api answers POST /agents with action=dispatch 409 +
// this text, which internal/ui's htmx:responseError handler prints verbatim; the
// full mechanism paragraph in a toast is a wall of text an operator scrolls past.
// It is a PREFIX of [KickoffRefusalReason] rather than a second sentence of its own,
// so the two cannot drift into saying different things — pinned by
// TestTheShortRefusalIsAPrefixOfTheLongOne.
//
// ⚠ ONE CLAUSE OF THE TEXT BELOW IS NOW WEAKER THAN WHEN IT WAS WRITTEN, AND IT IS
// FLAGGED HERE RATHER THAN SILENTLY REWORDED. "a pod that reads healthy" was
// literally true while nothing on any page reported an undelivered first turn;
// agents.KickoffOwed now renders a "kickoff owed" badge beside the status dot, so
// such a pod would read `running` AND owed. The REMEDY and the cause are
// untouched and still exactly true, which is why the operator-facing bytes are
// left alone: this text is pinned by
// TestTheRemedyDoesNotPromiseADeliveryTheDispatchPathCannotMake, whose own doc
// records three regressions caused by editing it, and rewording it is a product
// decision about copy rather than part of surfacing the signal. If it is reworded,
// re-read that test's four properties first.
const KickoffRefusalSummary = "dispatch REFUSED and NOTHING was provisioned: this deployment " +
	"cannot deliver an agent's first turn, so beginning the work is impossible and creating the " +
	"instance would only produce a pod that reads healthy and was never told what to do. " +
	"Use \"Save for later\", then Start the agent — that brings the instance up without " +
	"claiming a first turn nothing can deliver."

// KickoffRefusalReason is the operator-facing text of that refusal. It is logged,
// written to the agent row's error message, and carried in the returned error.
//
// 🔴 IT NAMES THE CAUSE *AND* THE REMEDY, WHICH IS THE PROPERTY THAT SEPARATES IT
// FROM [UndeliveredKickoffReason]. That constant is evidence about a thing that
// already happened and its reader can do nothing with it; this one is read by
// someone whose click was just refused, so it has to answer "why" and "what do I
// do". An error with no remedy is how an operator concludes the feature is broken
// and files a bug against the agent.
//
// 🔴 THE REMEDY NAMES THE VARIABLE AGAIN, AND THAT IS NOW TRUE RATHER THAN A
// REGRESSION. An earlier revision led with "set MUSTER_AGENT_GATEWAY", which was
// measured FALSE while [KickoffDeliveryWired] was false: the variable was set on the
// deployment reading it and the refusal fired anyway, because nothing called the
// gateway. The text then said in as many words that setting the variable was NOT
// sufficient. With the deliverer wired the refusal fires ONLY when no gateway was
// built, so the variable is exactly the remedy — and "Save for later, then Start"
// stays in the summary as the move that works without it.
//
// ⚠ THE SPEC AND CREDENTIAL CLAUSE SURVIVES AND IS STILL LOAD-BEARING. The text
// tells the operator NOT to go looking at the rendered port or the gateway token,
// because both were real blockers once and both are closed — and
// TestEveryCapabilityClaimTheRefusalMakesIsTRUE pairs each of those two sentences
// with a predicate that executes, so an unchanged sentence becoming false reddens.
//
// ⚠ THE ENVIRONMENT VARIABLE IS SPELLED AS A LITERAL HERE AND THE BINARY HAS ITS
// OWN CONSTANT FOR IT. This package cannot import package main, so the two
// spellings are genuinely duplicated, and a duplicated name is a name that goes
// stale in one place. That is not left to care:
// TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable in cmd/muster-server
// asserts this string contains envAgentGateway, so renaming the variable there
// reddens here.
//
// ✅ WHERE AN OPERATOR READS IT, MEASURED RATHER THAN ASSUMED: the boot banner, this
// pod's log, the agent row's status becoming `error` (which agents.ComputeStatus
// treats as terminal, so the card can no longer be refined to `running` by a live
// instance that does not exist), agents.error_message on the hook-token-gated GET
// /api/agents — and, since the change that added [Adapter.KickoffUndeliverableReason],
// THE HTTP RESPONSE OF THE POST THAT ASKED. That last one was the open half and this
// paragraph used to say so: "IT DOES *NOT* REACH THE HTTP RESPONSE OF THE POST THAT
// ASKED … CLOSING CONDITION: a pull request in which POST /agents with
// action=dispatch answers 4xx carrying this text." internal/api's handleAgentCreate
// now asks [Adapter.KickoffUndeliverableReason] BEFORE the row is created and answers
// 409 with [KickoffRefusalSummary], which is this string's own prefix.
// ⚠ ONE CALLER IS STILL ASYNCHRONOUS AND THAT IS DELIBERATE: handleChiefProvision
// reaches Dispatch through createAndDispatchAgent inside safeGo, so its refusal is
// recorded on the row rather than returned. It has no "Save for later" affordance
// to be sent to; the row it creates can be brought up with Start, which is the
// same escape by a different door.
const KickoffRefusalReason = KickoffRefusalSummary + " CAUSE: this deployment names no agent " +
	"gateway, and a DELIVERED kickoff is a model turn sent through one. REMEDY: set " +
	"MUSTER_AGENT_GATEWAY=hooks-sha256 together with MUSTER_AGENT_GATEWAY_MODEL (and the agent " +
	"runtime-config bundle boot requires with them) and restart muster; the kickoff deliverer " +
	"(internal/agentkickoff) then hands each dispatched agent's pending note to its instance once " +
	"the instance is ready. See cmd/muster-server/doc_seams.go entry 1. Do not go looking at the " +
	"spec or the credential on the way: the rendered spec declares the gateway port and the " +
	"instance receives the token the bearer is derived from."

// Config is everything the adapter needs. Every field without a stated default
// is required, and New says which one is missing rather than producing an
// adapter that fails at the first dispatch.
type Config struct {
	// Driver is the provisioning backend. REQUIRED.
	Driver provision.Provisioner
	// Store resolves an agent id to its row and is where every status write
	// lands. REQUIRED.
	Store agents.Store
	// Spec is the deployment-wide half of every built spec — image repository,
	// the base URL instances phone home to, model, resources. REQUIRED, and
	// agentspec.Build refuses the two fields that have no defensible default.
	Spec agentspec.Config
	// KickoffDeliverable declares whether THIS PROCESS holds something that could
	// hand an agent's pending note to its model gateway.
	//
	// 🔴 "SOMETHING THAT COULD" IS A CONJUNCTION OF TWO FACTS AND THIS DOC USED TO
	// NAME ONLY ONE OF THEM. It read "— in cmd/muster-server, whether buildGateway
	// returned a gateway rather than nil", and that is what the binary passed: a
	// reachable gateway, with nothing asserted about whether any code CALLS one on
	// the dispatch path. On the deployment that runs this, with
	// MUSTER_AGENT_GATEWAY set, the first fact was true and the second false, so
	// this field was true, the refusal below was skipped, and a Dispatch click
	// produced the pod-that-was-never-told-what-to-do this whole file argues
	// against. The binary now passes `gw != nil && KickoffDeliveryWired` — see
	// [KickoffDeliveryWired] for the second conjunct and the ledger that keeps it
	// honest.
	//
	// 🔴 IT IS EXTERNAL KNOWLEDGE AND IT HAS TO BE, WHICH IS WHY IT IS A FIELD
	// RATHER THAN A METHOD ON THIS TYPE. This adapter is the LIFECYCLE half by
	// construction and must never grow a gateway (see the package header), so it
	// cannot answer the question by looking at itself. The answer lives in the
	// binary's configuration, and passing it in is what makes the refusal below
	// conditional on the ACTUAL resolved configuration instead of on a build tag or
	// an assumption about what this build contains.
	//
	// 🔴 THE ZERO VALUE IS "NO GATEWAY", SO IT FAILS CLOSED. A wiring that forgets
	// this field refuses kickoffs it could have delivered — visible, annoying and
	// fixable. The other polarity, defaulting to deliverable, silently restores the
	// create-a-pod-and-strand-the-note behaviour this field exists to remove, and
	// would do so with no error anywhere. Prefer the loud direction.
	KickoffDeliverable bool
	// Logger is optional.
	Logger *log.Logger
	// OpTimeout defaults to [DefaultOpTimeout].
	//
	// ⚠ NOTHING SETS IT TODAY — not buildProvisioner, not any test — and it was
	// kept over a round-0 deletion candidate for one reason: the alternative is a
	// package whose only timeout is a constant, which cannot be shortened by a
	// test that needs to observe the expiry. If no such test exists by the time the
	// reconcile loop lands, collapse this to the constant.
	OpTimeout time.Duration
	// Credentials resolves the model-provider credentials of an agent's OWNER
	// for every spec this adapter builds (internal/credentials.Plane). nil
	// keeps Spec's deployment-wide credentials for every agent — the shape
	// every pre-accounts test builds; cmd/muster-server always sets it
	// (TestTheAgentPlaneResolvesCredentialsPerOwner).
	Credentials CredentialResolver
	// AnonymousGitBase is where a commit job's pull request is fetched from,
	// anonymously: the repository is AnonymousGitBase + "owner/name".
	// Empty is [DefaultAnonymousGitBase], the only production value; tests
	// point it at a local directory (file://…/).
	AnonymousGitBase string
}

// CredentialResolver answers which credentials an OWNER's agent is built with.
//
// 🔴 EVERY SPEC BUILD ASKS, FOR THE AGENT ROW'S OWN owner_id. A member's agent
// is built with that member's setup-token and OpenRouter key and nothing else;
// the deployment-wide values in Config.Spec (the admin's environment pool and
// key) are REPLACED, not merged, so a member's pod can never be handed the
// admin's credential by falling through to them.
type CredentialResolver interface {
	ClaudeToken(ctx context.Context, owner int64, account string) (string, error)
	OpenRouterKey(ctx context.Context, owner int64) (string, error)
}

// Adapter implements the consumer-side lifecycle interface over a driver.
//
// ⚠ IT DOES NOT NAME THAT INTERFACE IN A COMPILE-TIME ASSERTION, and the
// omission is deliberate: internal/api declares it consumer-side precisely so
// this direction of the dependency does not exist. The binding is checked where
// it is used — cmd/muster-server assigns an *Adapter to
// api.Extensions.Provisioner, which is a compile error if the method set drifts
// — and internal/api's own TestTheLifecycleAdapterSatisfiesTheConsumerInterface
// pins it from the side that owns the interface.
type Adapter struct {
	// driver is the ONLY handle on the backend: Config.Driver wrapped once, so
	// every exec strips the turn credentials (agentexec.go, scrubexec).
	driver  scrubexec.Driver
	store   agents.Store
	spec    agentspec.Config
	log     *log.Logger
	timeout time.Duration
	// kickoffDeliverable mirrors [Config.KickoffDeliverable]. See there for why the
	// zero value refuses.
	kickoffDeliverable bool
	creds              CredentialResolver
	anonGitBase        string
	// githubGrant answers whether an agent holds an approved grant reaching
	// GitHub (connectors.GitHubGrantLookup); set by UseGitHubGrants. nil is
	// UNKNOWN, and vetoes job-lane bypass (jobBypassVeto).
	githubGrant func(ctx context.Context, agentID int64) (string, error)
}

// UseGitHubGrants wires the lookup jobBypassVeto asks. Call it before the
// adapter serves; without it every agent in a bypass-listed pool is started
// WITHOUT bypass (fail closed).
func (a *Adapter) UseGitHubGrants(f func(ctx context.Context, agentID int64) (string, error)) {
	a.githubGrant = f
}

// jobBypassVeto is why ag's job lanes must not run with permissions bypassed
// although its pool is listed (MUSTER_AGENT_CC_BYPASS_POOLS), or "".
//
// 🔴 AN AGENT HOLDING AN APPROVED GRANT THAT REACHES GITHUB IS NEVER BYPASSED.
// With such a grant ccd routes the pod's git through muster's proxy, which adds a
// GitHub App token with contents:write and pull_requests:write
// (internal/githubapp), so a prompt-injected lane running unapproved could push
// and merge. The approval doors already refuse such a grant to every pool worker
// (internal/api refusePoolWorkerGitHubGrant), so this is the third door: it is
// asked every time the spec is built — Dispatch, Start and ReapplyProfiles — and
// it FAILS CLOSED: an unreadable grant state, or no lookup wired, is a veto too.
// Each veto is logged naming the agent.
func (a *Adapter) jobBypassVeto(ctx context.Context, ag agents.Agent, cfg agentspec.Config) string {
	if !agentspec.JobBypassListed(ag, cfg) {
		return ""
	}
	veto := ""
	if a.githubGrant == nil {
		veto = "no GitHub grant lookup is wired, so whether it holds one is unknown"
	} else if conn, err := a.githubGrant(ctx, ag.ID); err != nil {
		veto = fmt.Sprintf("its GitHub grants could not be read (%v)", err)
	} else if conn != "" {
		veto = fmt.Sprintf("it holds an approved grant on connector %q, which reaches GitHub", conn)
	}
	if veto != "" {
		a.log.Printf("agentprovision: agent %q (pool %q) is listed for job-lane permission bypass "+
			"(MUSTER_AGENT_CC_BYPASS_POOLS) but is started WITHOUT it: %s", ag.Name, ag.WorkerPool, veto)
	}
	return veto
}

// JobBypassDecision is what this adapter decides for ag's job lanes the next
// time its spec is applied: bypass, or not and the veto ("" when its pool is
// simply not listed). It asks the same two questions buildSpec does
// (agentspec.JobBypassListed and jobBypassVeto), over the deployment's spec
// configuration, so the wiring that feeds them is observable from outside.
func (a *Adapter) JobBypassDecision(ctx context.Context, ag agents.Agent) (bypass bool, veto string) {
	if !agentspec.JobBypassListed(ag, a.spec) {
		return false, ""
	}
	veto = a.jobBypassVeto(ctx, ag, a.spec)
	return veto == "", veto
}

// New validates the configuration and builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.Driver == nil {
		return nil, errors.New("agentprovision: Config.Driver is required (an adapter over no " +
			"driver would answer every lifecycle call successfully and create nothing, which is " +
			"the exact observable the provisioner wrappers exist to prevent)")
	}
	if cfg.Store == nil {
		return nil, errors.New("agentprovision: Config.Store is required (every method resolves " +
			"an agent id through it, and every status this adapter reports is written to it)")
	}
	a := &Adapter{
		driver:             scrubexec.Wrap(cfg.Driver),
		store:              cfg.Store,
		spec:               cfg.Spec,
		log:                cfg.Logger,
		timeout:            cfg.OpTimeout,
		kickoffDeliverable: cfg.KickoffDeliverable,
		creds:              cfg.Credentials,
		anonGitBase:        cfg.AnonymousGitBase,
	}
	if a.anonGitBase == "" {
		a.anonGitBase = DefaultAnonymousGitBase
	}
	if a.log == nil {
		a.log = log.New(io.Discard, "", 0)
	}
	if a.timeout <= 0 {
		a.timeout = DefaultOpTimeout
	}
	return a, nil
}

// Driver returns the backend's short name, for the boot banner and for error
// messages that have to say which set of capability losses applies.
func (a *Adapter) Driver() string { return a.driver.Driver() }

// KickoffUndeliverableReason answers, for THIS process, why an agent's first turn
// cannot be delivered — and "" when it can. It is the question
// [Adapter.Dispatch] answers by refusing, asked WITHOUT performing the refusal, so
// a caller can answer its own request before creating anything.
//
// ⚠ IT RETURNS THE SHORT FORM, [KickoffRefusalSummary], AND THE DIFFERENCE IS THE
// AUDIENCE RATHER THAN A LOSS. Its one consumer puts the string straight into an
// HTTP response body that internal/ui shows a human as a toast; the ~900-character
// mechanism paragraph in [KickoffRefusalReason] is what a toast gets scrolled past
// for. The full text still reaches the log, the agent row's error_message and the
// returned error on the Dispatch path, and the short form is a literal PREFIX of it,
// so neither surface can come to say something the other contradicts.
//
// 🔴 IT EXISTS SO THE REFUSAL REACHES THE HUMAN WHO ASKED, WHICH Dispatch CANNOT DO.
// internal/api calls Dispatch inside a background goroutine after the row exists, so
// the returned [ErrKickoffUndeliverable] is logged into this pod's stdout and the
// operator's POST has already answered 200 — a card that turns red with the reason
// in a machine-tier field. This method is what lets the create handler refuse
// SYNCHRONOUSLY, before the row, and hand back [KickoffRefusalSummary] with a 409.
//
// 🔴 IT IS A SECOND READ OF ONE FACT, NOT A SECOND PREDICATE, AND THAT IS THE WHOLE
// POINT OF ITS SHAPE. It returns the same field [Adapter.Dispatch] branches on, so
// the pre-flight answer cannot disagree with what the dispatch would actually do. A
// caller computing deliverability from its own copy of the configuration is the
// hazard: two expressions over one condition, disagreeing for every input the two
// were written against at different times. Guarded by internal/api's
// TestTheCreateHandlersPreflightAgreesWithWhatDispatchWouldDo.
//
// ⚠ "" MEANS DELIVERABLE, AND THE ZERO VALUE OF THIS RETURN IS THEREFORE THE
// PERMISSIVE ONE — the opposite polarity from [Config.KickoffDeliverable], whose zero
// value refuses. That is not an oversight and it does not reopen the hole that field's
// doc closes: a struct field can be FORGOTTEN by a wiring, which is why its zero value
// has to fail closed; a method cannot be forgotten, because omitting it is a compile
// error against the interface that names it. What an implementer can still do is
// return the wrong answer, and no polarity protects against that — the guard above is
// what does.
func (a *Adapter) KickoffUndeliverableReason() string {
	if a.kickoffDeliverable {
		return ""
	}
	return KickoffRefusalSummary
}

// ⚠ THERE IS DELIBERATELY NO Capabilities() PASSTHROUGH. One existed and was
// deleted: round 0 of this PR's audit measured ZERO callers anywhere in the module,
// and its own doc argued it was "so a caller that has only the adapter can still
// branch" — a caller that does not exist. Anything holding the driver can call
// driver.Capabilities() directly. Driver() stays because
// TestBuildingTheKubernetesProvisionerUsesTheRealDriver reads it to tell the real
// driver from a stand-in, and the boot banner prints it.

// ---------------------------------------------------------------------------
// LIFECYCLE
// ---------------------------------------------------------------------------

// Dispatch provisions an instance for the agent when kickoff is true, and
// creates NOTHING when it is false.
//
// 🔴 kickoff=false CREATES NO INSTANCE, AND AN EARLIER REVISION OF THIS METHOD
// CREATED ONE. That revision mirrored the upstream implementation, which installs
// its release either way and then stores `stopped` — and carrying that across
// turned a consumer-side comment into a falsehood and bypassed a guard. The caller
// is internal/api's createAndDispatchAgent, where `kickoff` is
// `action == "dispatch"`: the OTHER action is the UI's "Save for later" button,
// and the gate check four lines above it refuses a dispatch of a
// `gate:<reason>`-tagged task while allowing a save, giving as its reason that a
// save "provisions nothing". That was TRUE for as long as this module had no
// provisioner at all. Wiring one made it false in the worst direction: a save on a
// task explicitly gated from being worked created a pod, cloned the repository into
// it, and handed it a model credential — one per click, with the gate's own check
// passing.
//
// So the two arguments now differ in WHAT IS BUILT, not only in what is stored:
//
//	kickoff=true  -> WITH something in the process that could deliver it
//	                 (Config.KickoffDeliverable): create the instance and store
//	                 `provisioning` (a first turn is owed); internal/agentkickoff
//	                 delivers it once the instance is ready. WITHOUT: REFUSE —
//	                 create nothing, store `error` carrying KickoffRefusalReason,
//	                 and return ErrKickoffUndeliverable.
//	kickoff=false -> create nothing, store `stopped`. The status is now TRUE
//	                 rather than a statement about what this service is waiting
//	                 for, which is also what makes the model-change roll in
//	                 internal/api correct for such an agent: it branches on the
//	                 STORED status, so a `stopped` row over a running instance
//	                 made it skip a roll the instance needed.
//
// ⚠ Start IS WHAT PROVISIONS A SAVED AGENT, and it already does: its
// provision.ErrNotFound branch creates the instance the save deliberately did not.
// So "save, then start later" works without this method creating anything.
func (a *Adapter) Dispatch(ctx context.Context, agentID int64, kickoff bool) error {
	ctx, cancel := a.opCtx(ctx)
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: dispatch agent %d: load row: %w", agentID, err)
	}
	if !kickoff {
		// 🔴 NO DRIVER CALL AT ALL ON THIS PATH. It is not a route answering 200
		// and doing nothing — the caller asked for a saved agent and gets exactly
		// that: a row, a status that is true, and no cluster objects. The token is
		// deliberately NOT minted either: an agent that was never provisioned has
		// no instance holding a credential, and minting one now would be a secret
		// written for nothing.
		//
		// 🔴 AND THE REFUSAL BELOW IS DELIBERATELY *AFTER* THIS RETURN, NOT BEFORE
		// IT. A save asks for nothing this configuration cannot do, so refusing one
		// would remove a working operation to punish a missing gateway — and it is
		// the operation an operator is steered to by the refusal's own remedy text.
		// Guarded by TestASaveIsNeverRefusedWhenNoKickoffCanBeDelivered.
		return a.setStatus(ctx, ag.ID, agents.StatusStopped)
	}
	if !a.kickoffDeliverable {
		// 🔴 REFUSE BEFORE create, SO NOTHING IS BUILT AND NO SECRET IS MINTED.
		// This is the whole point of the branch: past this line the adapter would
		// create a Deployment, a ServiceAccount, a namespace and a Secret holding a
		// freshly-minted token, clone the repository into the pod, hand it a model
		// credential — and then record that the one thing the caller asked for did
		// not happen. Every object in that list is a cost, and the card that results
		// reads `running` (agents.ComputeStatus refines a live ready instance).
		//
		// ⚠ "SO THE OPERATOR'S ONLY EVIDENCE WAS A LOG LINE AND A MACHINE-TIER
		// FIELD" USED TO END THAT SENTENCE AND IS NO LONGER TRUE: the card carries
		// a "kickoff owed" badge (agents.KickoffOwed) beside the `running` dot. The
		// refusal stands on the COST — a pod, a namespace, a ServiceAccount and a
		// minted secret for work that cannot begin — which was always the stronger
		// of the two reasons.
		return a.refuseKickoff(ctx, ag)
	}
	if ag, err = a.create(ctx, ag); err != nil {
		return err
	}
	// 🔴 NOTHING IS RECORDED IN kickoff_error HERE, AND UNTIL THE DELIVERER LANDED
	// THIS LINE WROTE UndeliveredKickoffReason. On this arm something in the process
	// DOES deliver — internal/agentkickoff, driven off this row's `provisioning`
	// status once the instance is ready — so a "NOT delivered" written now would be a
	// false statement the delivery then has to overwrite. The row stays owed
	// (pending_note set, kicked_off false) and the deliverer pays it.
	return a.setStatus(ctx, ag.ID, agents.StatusProvisioning)
}

// Start scales a stopped instance back up, creating it first when the backend
// does not have it.
//
// 🔴 THE MISSING-INSTANCE BRANCH KEYS ON provision.ErrNotFound, NOT ON "Scale
// FAILED". Upstream fell back to a full re-provision on ANY scale error, which
// treats an unreachable backend (provision.ErrBlind — the driver knows nothing)
// as evidence that nothing is there, and answers it by creating. The sentinels
// exist to keep those apart: "the backend says there is nothing" is a reason to
// create, "I cannot see the backend" is a reason to stop and say so.
//
// 🔴 START IS *NOT* REFUSED WHEN NO KICKOFF CAN BE DELIVERED, AND THAT ASYMMETRY
// WITH Dispatch IS THE DECISION, NOT AN OVERSIGHT. The refusal belongs where a NEW
// PROMISE IS MADE. Dispatch(kickoff=true) is the one call that means "begin this
// work now" — it is what the UI's Dispatch button sends — and on a deployment with
// no gateway that promise cannot be kept at all, so making it is the defect. Start
// promises something narrower and entirely deliverable: bring this instance up.
// This build genuinely can, and does — the pod runs, its logs stream, and a human
// with a reachable instance can talk to it.
//
// So refusing here would DELETE WORKING CAPABILITY to signal a missing one. Three
// concrete operations would stop: restarting a stopped agent, recovering an agent
// whose instance was evicted, and provisioning an agent that was deliberately
// SAVED for later — which is the very escape route [KickoffRefusalReason] tells the
// operator to take, so refusing it would make the remedy text a lie. Guarded by
// TestStartStillWorksWhenNoKickoffCanBeDelivered.
//
// ⚠ NOTHING IS HIDDEN BY THAT CHOICE. The !ag.KickedOff branch below still records
// the non-delivery through recordUndeliveredKickoff, so a first turn that is owed
// and cannot be delivered is written to agents.kickoff_error and logged exactly as
// before — and here that field is honest, because an instance really does exist for
// a retry to act on. The difference between the two methods is whether an instance
// SHOULD come into being, not whether the missing gateway is reported.
func (a *Adapter) Start(ctx context.Context, agentID int64) error {
	ctx, cancel := a.opCtx(ctx)
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: start agent %d: load row: %w", agentID, err)
	}
	// 🔴 A START RE-APPLIES THE SPEC instead of only scaling WHENEVER THE SPEC
	// CARRIES A CREDENTIAL THAT CAN HAVE CHANGED SINCE CREATE: always for
	// claude-code (the account's setup-token), and for every kind once credentials
	// resolve per owner ([Config.Credentials] — a gateway agent's OpenRouter key).
	// Both live in the agent's own Secret, written at create; after a token or key
	// is rotated (an operator replacing an env token after an auth_failed mark, a
	// member saving a new key in Account → Credentials), Stop then Start is how an
	// existing agent gets the new value — a bare Scale brings the pod back on the
	// old one. And when the owner has DELETED the credential, building the spec
	// fails, so Start refuses instead of reviving a pod on a credential its owner
	// withdrew. (Round-1 audit on #69: gateway Start only scaled, so a member's
	// rotated key never reached the pod and a deleted key kept working —
	// TestAGatewayStartPicksUpARotatedKeyAndRefusesADeletedOne.)
	// Update creates an absent instance, so the saved-agent case is covered too.
	// It keeps the agent's stored account: buildSpec reads the row.
	//
	// ⚠ WITH NO RESOLVER (the pre-accounts wiring every older test builds) a
	// gateway Start still only scales: its spec then carries nothing per-owner
	// that could have changed, and the scale-first contract those tests pin
	// (create only when the backend SAYS the instance is absent) stands.
	if agents.RunsPodSupervisor(ag.Kind) || a.creds != nil {
		if ag, err = a.ensureHooksToken(ctx, ag); err != nil {
			return err
		}
		spec, err := a.buildSpec(ctx, ag)
		if err != nil {
			return a.fail(ctx, ag, "build spec", err)
		}
		if err := a.driver.Update(ctx, spec); err != nil {
			return a.fail(ctx, ag, "apply instance", err)
		}
	} else if err := a.driver.Scale(ctx, agents.RefOf(ag), 1); err != nil {
		if !errors.Is(err, provision.ErrNotFound) {
			return a.fail(ctx, ag, "scale up", err)
		}
		if ag, err = a.create(ctx, ag); err != nil {
			return err
		}
	}
	if err := a.setStatus(ctx, ag.ID, agents.StatusProvisioning); err != nil {
		return err
	}
	if !ag.KickedOff {
		// A first turn is still owed, whether this is a first start or a restart
		// of something that never got its note. With a deliverer in the process
		// the `provisioning` just written is what it acts on, so there is nothing
		// to record; without one, the non-delivery is recorded where it is honest.
		if a.kickoffDeliverable {
			return nil
		}
		return a.recordUndeliveredKickoff(ctx, ag)
	}
	// Already kicked off: nothing is owed, so the row stops saying it is waiting.
	// A live read refines this through agents.ComputeStatus.
	return a.setStatus(ctx, ag.ID, agents.StatusRunning)
}

// Stop scales the instance to zero replicas, keeping its declaration.
func (a *Adapter) Stop(ctx context.Context, agentID int64) error {
	ctx, cancel := a.opCtx(ctx)
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: stop agent %d: load row: %w", agentID, err)
	}
	// 🔴 provision.ErrNotFound IS ALREADY-STOPPED, NOT A FAILURE, AND TREATING IT
	// AS ONE LEFT THE ROW PERMANENTLY WRONG. An instance that was evicted or
	// deleted out of band is gone, which is what a stop is FOR; refusing meant the
	// stored status stayed `running` with no way to correct it from the UI, and the
	// model-change roll kept trying to reconcile an instance that does not exist.
	// Destroy already draws this distinction — see its own doc.
	if err := a.driver.Scale(ctx, agents.RefOf(ag), 0); err != nil && !errors.Is(err, provision.ErrNotFound) {
		return fmt.Errorf("agentprovision: stop agent %d (%s): %w", ag.ID, ag.Name, err)
	}
	// 🔴 THE DELIVERY PROVENANCE IS DROPPED, AND agents.Store's own doc on this
	// method is the argument: a deliberate stop means the next start brings up an
	// instance with a new id, which is indistinguishable from an eviction — so
	// leaving the old recipient stamped would make agents.kickoffLost read this
	// agent as having lost its message and re-send the original task. Best-effort
	// on purpose: failing the stop over a provenance write would be a worse
	// answer than a stop that succeeded with stale provenance.
	if err := a.clearKickoffDelivery(ctx, ag); err != nil {
		a.log.Printf("agentprovision: stop %s: clear kickoff delivery: %v", ag.Name, err)
	}
	return a.setStatus(ctx, ag.ID, agents.StatusStopped)
}

// Destroy removes the instance and then the stored row.
//
// 🔴 THE ROW IS DELETED ONLY WHEN THE BACKEND HOLDS NOTHING, AND THIS IS WHERE
// THIS ADAPTER IS STRICTER THAN THE IMPLEMENTATION IT REPLACES. Upstream tore
// down best-effort and deleted the row unconditionally, which was safe there
// because its teardown could only fail transiently. The driver contract has two
// failures that are NOT transient-and-harmless, and both of them mean nothing
// was removed:
//
//   - provision.ErrNotManaged — something under this instance's name is not
//     muster's, so Destroy refused BEFORE deleting anything and the contract
//     calls the refusal terminal. Deleting the row here would leave whatever is
//     running with no record in this service at all: unlistable, unstoppable,
//     and invisible to the very refusal that was trying to protect it.
//   - provision.ErrBlind — the backend could not be reached, so the driver knows
//     nothing. Deleting the row on no information is the same loss with a
//     different cause.
//
// A nil error and provision.ErrNotFound both mean the backend holds nothing, and
// only those two reach the delete.
func (a *Adapter) Destroy(ctx context.Context, agentID int64) error {
	ctx, cancel := a.opCtx(ctx)
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: destroy agent %d: load row: %w", agentID, err)
	}
	err = a.driver.Destroy(ctx, agents.RefOf(ag))
	if err != nil && !errors.Is(err, provision.ErrNotFound) {
		// 🔴 THE REASON GOES ON THE ROW, NOT ONLY INTO A RETURNED ERROR, AND AN
		// EARLIER REVISION RETURNED IT AND WROTE NOTHING. Nobody reads the return:
		// handleAgentDelete runs this in a background goroutine that logs, and it
		// has ALREADY answered an empty 200, so htmx removed the card before this
		// line ran. With no row write the card simply REAPPEARS on the next render
		// with no explanation anywhere a user looks — and the row we correctly kept
		// is the only evidence the instance is still out there.
		//
		// 🔴 YOU ARE THE FIFTH WRITER OF THIS PARAGRAPH. Three successive drafts
		// asserted something about what a HUMAN can see, and each was measured false
		// by the next round. Do not reach for a fourth formulation — the two claims
		// below are the ones that survived measurement, and nothing wider is known.
		//
		// WHAT IS TRUE:
		//   - agents.ComputeStatus returns StatusError FIRST, so a stored `error` is
		//     not refined away by a live read.
		//   - internal/ui renders that status as a coloured DOT with a title
		//     attribute (statusIcon / cardStatusIcon / liveStatusIcon). The colour
		//     changes; no text does.
		//
		// WHAT THE DEAD DRAFTS CLAIMED, so nobody re-derives them:
		//   1. "a red card NAMING an ownership refusal" — Agent.ErrorMessage reaches
		//      no HTML surface at all.
		//   2. "a red PILL" — ui.statusDot IS a labelled pill and has ZERO callers
		//      tree-wide. It is not what renders. The CARD's border is unconditional.
		//   3. "reaches exactly ONE surface, the hook-token-gated GET /api/agents" —
		//      newAgentJSON is also called from the task API (internal/api/notes.go),
		//      and requireHookToken is ENFORCE-WHEN-SET, so on a server with no
		//      MUSTER_HOOK_TOKEN it is gated by nothing.
		//
		// So the operator gets a dot that turns red instead of a card that silently
		// reappears, and the REASON is machine-readable only. That is worth having and
		// it is all that is claimed.
		return a.fail(ctx, ag, "destroy instance (the stored row was KEPT because the "+
			"instance was not removed)", err)
	}
	// 🔴 THE DELETE IS BOOKKEEPING, NOT THE OPERATION, AND IT WAS ON THE WRONG
	// BUDGET FOR ONE ROUND. bookkeepingCtx's own doc predicted this site in as many
	// words — "a rule asking every caller to remember which context to hand a status
	// write is a rule that gets obeyed at four sites and missed at the fifth" — and
	// this was the fifth. driver.Destroy is the slow call here, so if it consumes the
	// operation budget and THEN succeeds, the instance is gone and the row survives
	// with its pre-delete status; the returned error is only logged, after an empty
	// 200 htmx has already used to remove the card. The card comes back pointing at
	// nothing. Caught by a mutant that handed this call a cancelled context and
	// SURVIVED the whole suite.
	if err := a.deleteRow(ctx, ag); err != nil {
		return fmt.Errorf("agentprovision: destroy agent %d (%s): instance removed but the row "+
			"was not deleted: %w", ag.ID, ag.Name, err)
	}
	return nil
}

// deleteRow removes the stored agent on the bookkeeping budget. See bookkeepingCtx.
func (a *Adapter) deleteRow(ctx context.Context, ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	return a.store.Delete(ctx, ag.ID)
}

// Instances returns every instance the driver manages, for status
// reconciliation.
//
// ⚠ IT PASSES THE DRIVER'S ERROR THROUGH RATHER THAN DEGRADING TO AN EMPTY
// SLICE. An empty slice is a positive claim that nothing is running, and
// api.Server.instanceIndex distinguishes a nil index ("live state was not
// observed", keep the stored status) from an empty one ("observed, nothing is
// running", every agent is stopped). Swallowing the error here would collapse
// those two and report a whole board stopped on one unreachable backend.
func (a *Adapter) Instances(ctx context.Context) ([]provision.Instance, error) {
	return a.driver.List(ctx)
}

// StreamLogs follows the agent's instance output, calling emit per line.
func (a *Adapter) StreamLogs(ctx context.Context, ag agents.Agent, emit func(string)) error {
	return a.driver.StreamLogs(ctx, agents.RefOf(ag), emit)
}

// AttachTerminal attaches a terminal to the agent's live session: the kind's
// attach command (agents.TerminalCommand — ErrNoTerminal for a kind without
// one) run on a pty through provision.ExecTTY, which refuses a driver that
// cannot exec. readOnly is passed straight to the command; there is no default
// here for a caller to inherit.
//
// 🔴 IT RETURNS ONLY AFTER THE ATTACH IS RELEASED, ON EVERY EXIT PATH. Ending
// the exec stream does not end the process it started in the container — the
// runtime keeps it and its pty, and tells it nothing — so every attach that may
// have started is followed by releaseTerminal, a second exec that ends exactly
// that attach's tmux client (agents.TerminalReleaseCommand). One place, after
// ExecTTY returns, whatever made it return: the browser closing the socket, a
// dropped connection the keepalive noticed, a mode switch (which is a close
// and a new attach), a stream error, the client exiting on its own, or
// muster's shutdown cancelling every terminal (api.Server.EndTerminals).
// Only a refusal BEFORE any stream (terminalMayHaveStarted) skips it. A muster
// that dies without running this (a crash, SIGKILL) leaves its clients
// attached until the pod restarts; see agents.TerminalCommand on sizing.
func (a *Adapter) AttachTerminal(ctx context.Context, ag agents.Agent, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	if err := agents.CheckTerminal(ag.Kind); err != nil {
		return err
	}
	tag, err := agents.NewTerminalTag()
	if err != nil {
		return err
	}
	cmd, err := agents.TerminalCommand(ag.Kind, readOnly, tag)
	if err != nil {
		return err
	}
	ref := agents.RefOf(ag)
	// 🔴 NO ATTACH SHRINKS THE SESSION'S WINDOW (internal/termsize): ask how
	// big it is, and size the client so it is not narrowed — a phone gets the
	// TUI at its real width, scrolled, and the operator's later desktop attach
	// is not met by a phone-narrow TUI. Unknown size = the old behaviour.
	out, sized := noShrink(ctx, a.windowSize(ctx, ref), stdout, resize)
	err = provision.ExecTTY(ctx, a.driver, ref, cmd, stdin, out, sized)
	if terminalMayHaveStarted(err) {
		a.releaseTerminal(ctx, ref, tag)
	}
	return err
}

// TerminalReleaseTimeout bounds one terminal release. It is its own budget, on
// a context DETACHED from the attach's: by the time a release runs, the
// attach's context has usually just been cancelled — that is what ended it.
const TerminalReleaseTimeout = 10 * time.Second

// terminalMayHaveStarted is false only for a refusal that provably came before
// any process could start: the driver cannot exec at all, the cluster refused
// the exec (RBAC — ErrUnsupported), the command was invalid, or there is no
// pod. Anything else — success, cancellation, a stream error — may have left a
// client running.
func terminalMayHaveStarted(err error) bool {
	return !errors.Is(err, provision.ErrUnsupported) &&
		!errors.Is(err, provision.ErrInvalidSpec) &&
		!errors.Is(err, provision.ErrNotFound)
}

// releaseTerminal ends the attach tagged tag, logging what it found. Its
// failure is logged, not returned: the caller's error is the attach's, and a
// release that could not run (the pod is gone, the apiserver is unreachable)
// has nothing the browser could act on.
func (a *Adapter) releaseTerminal(ctx context.Context, ref provision.Ref, tag agents.TerminalTag) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TerminalReleaseTimeout)
	defer cancel()
	var out bytes.Buffer
	err := provision.Exec(rctx, a.driver, ref, agents.TerminalReleaseCommand(),
		strings.NewReader(agents.TerminalReleaseInput(tag)), &out, &out)
	if err != nil {
		a.log.Printf("agents: terminal %s: release FAILED, its tmux client may still be attached: %v: %s",
			ref.Name, err, strings.TrimSpace(out.String()))
		return
	}
	a.log.Printf("agents: terminal %s: %s", ref.Name, strings.TrimSpace(out.String()))
}

// ReapplyProfiles re-renders the agent's spec from its current row and
// reconciles the live instance to it, which rolls the instance.
//
// 🔴 IT SATISFIES api.ProfileReapplier, WHICH IS A TYPE ASSERTION AND THEREFORE
// THE ONE FORM OF OPTIONAL DEPENDENCY THAT LEAVES NO TRACE WHEN IT IS NOT
// SATISFIED. cmd/muster-server/doc_seams.go entry 3 exists for exactly that: an
// adapter without this method compiles, wires, and takes the else branch for
// ever with no nil anywhere to notice. So it is implemented rather than omitted,
// and internal/api's TestTheLifecycleAdapterSatisfiesTheConsumerInterface
// asserts the assertion succeeds — "it compiles" is not that test, because a
// method added to the wrong type compiles just as well.
//
// ⚠ ROLLING THE INSTANCE RE-DELIVERS A KICKOFF ONCE A GATEWAY EXISTS, and
// agents.kickoffLost's own doc says so and calls it intended: a roll has already
// killed whatever turn was running, so the choice is between re-delivering the
// task and silently doing nothing. It is capped by agents.MaxKickoffAttempts.
// ⚠ STILL LATENT: internal/agentkickoff delivers FIRST turns only and never acts on
// a kicked-off row, so agents.ActionResendKickoff has no driver and a roll re-sends
// nothing today.
func (a *Adapter) ReapplyProfiles(ctx context.Context, agentID int64) error {
	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: reapply agent %d: load row: %w", agentID, err)
	}
	ag, err = a.ensureHooksToken(ctx, ag)
	if err != nil {
		return err
	}
	spec, err := a.buildSpec(ctx, ag)
	if err != nil {
		return err
	}
	if err := a.driver.Update(ctx, spec); err != nil {
		return fmt.Errorf("agentprovision: reapply agent %d (%s): %w", ag.ID, ag.Name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// INTERNALS
// ---------------------------------------------------------------------------

// create mints the agent's token if it has none, builds its spec, and brings the
// instance into existence. It returns the row as updated, so a caller that goes
// on to read ag.HooksToken sees the minted value rather than the empty one.
func (a *Adapter) create(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	ag, err := a.ensureHooksToken(ctx, ag)
	if err != nil {
		return ag, err
	}
	spec, err := a.buildSpec(ctx, ag)
	if err != nil {
		return ag, a.fail(ctx, ag, "build spec", err)
	}
	if err := a.driver.Create(ctx, spec); err != nil {
		// provision.ErrDivergentSpec is named in the message because it is the
		// one Create failure a reader can act on WITHOUT touching the cluster:
		// the instance exists and does not match, and the way to change it is
		// Update (ReapplyProfiles), not another Create.
		if errors.Is(err, provision.ErrDivergentSpec) {
			return ag, a.fail(ctx, ag, "create instance (it already exists with a different "+
				"spec; reconciling an existing instance is ReapplyProfiles, not Dispatch)", err)
		}
		return ag, a.fail(ctx, ag, "create instance", err)
	}
	return ag, nil
}

// ensureHooksToken gives the agent its own per-agent token if it has none.
//
// ⚠ IT IS A SECRET AND IS NEVER LOGGED, not even truncated. agentspec.Build puts
// it in Spec.Secrets rather than Spec.Env for the same reason.
//
// 🔴 OWED, NAMED RATHER THAN FIXED: THE MINT IS NOT ATOMIC. The guard reads a row
// fetched at the top of the calling method, and agents.Store.SetHooksToken is an
// unconditional `UPDATE agents SET hooks_token=$2`. Two operations on the SAME
// agent that both see an empty token both mint, and the second write overwrites the
// credential the first-created instance was handed — after which that instance's
// Spec.Secrets no longer matches, so the next Create returns
// provision.ErrDivergentSpec and the agent cannot authenticate to muster.
//
// ⚠ THE WINDOW IS NARROWER THAN IT LOOKS, WHICH IS WHY IT IS DEFERRED RATHER THAN
// DISMISSED. Two dispatch clicks do NOT collide: internal/api creates a NEW row per
// click, so there is no shared row. The reachable path is a model change (which
// reaches ReapplyProfiles) landing while the dispatch goroutine for the same agent
// is still in flight.
// TestDispatchCreatesTheInstanceAndMintsTheAgentsOwnToken covers the
// single-threaded case only, and says so.
//
// CLOSING CONDITION: agents.Store gains a compare-and-set mint — the SQL clause is
// `WHERE id=$1 AND coalesce(hooks_token,”) = ”` plus a reported row count — and
// this function re-reads the row when it LOSES, so the spec is built from the
// winner's token. It is a one-caller change (this is the only non-test caller of
// SetHooksToken, measured), which is why it is small and why it was not bundled
// into an audit-fix round already carrying a behaviour change.
// WHO CHECKS IT: the reviewer of that PR, against a test that runs two mints
// concurrently and asserts one row value and one winner.
//
// It also gives a claude-code agent its turn credential (ensureGatewaySecret):
// every path that builds a spec comes through here first, so no claude-code spec
// is built without one.
func (a *Adapter) ensureHooksToken(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	if ag.HooksToken == "" {
		buf := make([]byte, hooksTokenBytes)
		if _, err := rand.Read(buf); err != nil {
			return ag, a.fail(ctx, ag, "mint hooks token", err)
		}
		token := hex.EncodeToString(buf)
		if err := a.store.SetHooksToken(ctx, ag.ID, token); err != nil {
			return ag, a.fail(ctx, ag, "store hooks token", err)
		}
		ag.HooksToken = token
	}
	return a.ensureGatewaySecret(ctx, ag)
}

// ensureGatewaySecret gives a claude-code agent its own turn credential
// (agents.Agent.GatewaySecret) if it has none: random, and NOT derived from its
// hooks token, which its session holds (see that field). Other kinds get none
// and keep the derived bearer (agents.TurnBearer).
//
// ⚠ THE MINT LANDS BEFORE THE APPLY. If the driver then fails, muster sends the
// new secret to a pod still built with the old bearer, and its turns are 401
// until an apply succeeds (the next Start) — a loud failure, not a silent one.
// The same non-atomic-mint note as ensureHooksToken's applies.
func (a *Adapter) ensureGatewaySecret(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	if agents.ResolveKind(ag.Kind) != agents.KindClaudeCode || ag.GatewaySecret != "" {
		return ag, nil
	}
	buf := make([]byte, hooksTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ag, a.fail(ctx, ag, "mint turn credential", err)
	}
	secret := hex.EncodeToString(buf)
	if err := a.store.SetGatewaySecret(ctx, ag.ID, secret); err != nil {
		return ag, a.fail(ctx, ag, "store turn credential", err)
	}
	ag.GatewaySecret = secret
	return ag, nil
}

// buildSpec renders one agent row into a driver spec.
//
// 🔴 agentspec.WorkerInstructions IS PASSED, NOT OMITTED. That prose is the only
// thing that tells a dispatched instance the autosave rescue log exists (see
// TestTheInstructionsAndTheDaemonReferToEachOther), so an adapter that built a
// spec without it would ship the daemon and tell nobody to look at its log —
// which is what that test's "move both or neither" is about, one layer up.
//
// 🔴 THE SUPERVISOR IS THE ONE AGENT THAT GETS DIFFERENT PROSE AND THE ONE AGENT
// ELIGIBLE FOR THE SUBSYSTEM-STORE CREDENTIAL, AND THAT POLICY LIVES HERE BY
// DESIGN. agentspec deliberately does not compare an agent name against a
// constant — its own Options.SeedFiles comment argues that a name-to-privilege
// rule is a caller's policy, not a spec-building rule — so this is the layer that
// knows which row is the supervisor. The credential is READ+WRITE across every
// scope of the operator's knowledge store; a dispatched worker running a model
// over somebody's repository contents has no business holding one, and widening
// the condition below is a one-line change here while un-polluting the store is
// not. See internal/agentspec/cairn.go.
//
// 🔴 BOTH CONSUMERS GO THROUGH THE SAME PREDICATE, WHICH IS THE WHOLE POINT. The
// PROSE claims the credential and the SPEC carries it, and those two must not be
// able to disagree — a supervisor told it holds a read+write key it does not have
// reports a working subsystem as broken. What makes that impossible is that both
// answers come from [agentspec.Config.CairnConfigured]: this function passes it to
// ChiefInstructions, and agentspec's own gate calls the same method when it decides
// whether to place the client and the credential. The hazard is a SECOND
// EXPRESSION — `cfg.CairnURL != ""` in one place and the method in the other, which
// disagree for every half-configured input. Two guards pin it, both behavioural:
// TestTheSupervisorsProseAndItsSpecAgreeAboutTheStore on the configured and
// unconfigured cases, and the half-configured one on the inputs that DISCRIMINATE
// between the candidate expressions.
//
// ⚠ THIS USED TO CLAIM THE GATE IS "READ ONCE INTO A VARIABLE AND USED TWICE", AND
// THAT INVARIANT DOES NOT EXIST. The variable below is read ONCE; the second
// consumer never sees it, because agentspec re-evaluates the predicate itself
// inside its own gate. So the single-read form buys nothing, the inlined form is
// the same expression evaluated twice, and that test's own comment says so. A 🔴
// note defending a form that carries no invariant is worse than no note: it sends
// the next reader to protect a formatting choice.
func (a *Adapter) buildSpec(ctx context.Context, ag agents.Agent) (provision.Spec, error) {
	cfg, err := a.specFor(ctx, ag)
	if err != nil {
		return provision.Spec{}, err
	}
	switch agents.ResolveKind(ag.Kind) {
	case agents.KindClaudeCode:
		return a.buildClaudeCodeSpec(ag, cfg, a.jobBypassVeto(ctx, ag, cfg))
	case agents.KindHermes:
		return buildHermesSpec(ag, cfg)
	}
	if !agents.IsChiefName(ag.Name) {
		return agentspec.Build(ag, cfg, agentspec.Options{
			Instructions: agentspec.WorkerInstructions,
		})
	}
	store := cfg.CairnConfigured()
	return agentspec.Build(ag, cfg, agentspec.Options{
		Instructions:  agentspec.ChiefInstructions(store, cfg.Plugins),
		CairnEligible: true,
	})
}

// buildClaudeCodeSpec is buildSpec for a claude-code row.
//
// 🔴 A WORKER GETS NO INSTRUCTIONS, AS BEFORE — NOT WorkerInstructions. That prose
// teaches the in-pod `muster` CLI and the autosave daemon's log, and the
// claude-code image carries neither; now that agentspec PLACES a claude-code
// agent's Instructions (the managed CLAUDE.md), passing it would ship a prompt
// describing tools the instance does not have. A worker's task reaches it as its
// kickoff turn. A claude-code worker prose is its own change.
//
// 🔴 THE CHIEF (the reserved slug) GETS ChiefInstructionsClaudeCode AND IS NEVER
// CairnEligible. The subsystem-store client is a Python program installed by an
// Init step, and a claude-code spec carries no Init (the image's apt list has no
// python3 either); the instance's network policy admits public 443 only, so the
// store would be out of reach regardless — the credential would
// be a secret placed for nothing, and agentspec refuses CairnEligible for this
// kind anyway. When the deployment HAS a store configured that is a capability the
// gateway chief would have had, so it is said in the log rather than dropped
// silently.
//
// 🔴 A CHIEF WITH NO MCP SERVER IS REFUSED. Every tool its prose names is an MCP
// tool, and without [agentspec.Config.ClaudeCodeMCPOffered] ccd registers no
// server: the chief would boot with no muster tools and a prompt promising them.
//
// jobBypassVeto is Adapter.jobBypassVeto's answer for a worker; a chief is never
// listed for bypass (agentspec.JobBypassListed), so it is unused there.
func (a *Adapter) buildClaudeCodeSpec(ag agents.Agent, cfg agentspec.Config, jobBypassVeto string) (provision.Spec, error) {
	if !agents.IsChiefName(ag.Name) {
		return agentspec.Build(ag, cfg, agentspec.Options{JobBypassVeto: jobBypassVeto})
	}
	if !cfg.ClaudeCodeMCPOffered() {
		return provision.Spec{}, fmt.Errorf("agentprovision: chief %q is kind %s, which reaches muster's tools only "+
			"over its MCP server, and this deployment offers none (MUSTER_AGENT_CONNECT_URL is unset)", ag.Name, agents.KindClaudeCode)
	}
	if cfg.CairnConfigured() {
		a.log.Printf("agentprovision: chief %q is kind %s: the subsystem store (cairn) is configured but is SKIPPED "+
			"for this kind — the client is not in its image and its network policy cannot reach the store", ag.Name, agents.KindClaudeCode)
	}
	opts := agentspec.Options{Instructions: agentspec.ChiefInstructionsClaudeCode(cfg.Plugins)}
	// The gateway runtime beside ccd, for the chief's sessions on it
	// (agentspec/chief_sidecar.go). Start re-applies the spec, so a deployment
	// turning this on reaches a running chief on its next Stop→Start.
	if cfg.ChiefGatewaySidecar {
		opts.GatewaySidecar = &agentspec.GatewaySidecar{}
	}
	return agentspec.Build(ag, cfg, opts)
}

// buildHermesSpec is buildSpec for a hermes row.
//
// 🔴 A WORKER GETS NO INSTRUCTIONS, as a claude-code worker gets none:
// WorkerInstructions teaches the in-pod `muster` CLI and the autosave daemon,
// and the hermes image carries neither. Its task reaches it as its kickoff turn.
//
// 🔴 A HERMES CHIEF IS REFUSED, for now. Every chief tool is reached over
// muster's MCP server, and Hermes's MCP client against muster's /mcp has not
// been exercised; a chief that boots with a prompt promising tools it cannot
// call reads as a working chief. The chief replacement refuses the kind first
// (internal/api/chief_replace.go); this is the backstop for any other path.
func buildHermesSpec(ag agents.Agent, cfg agentspec.Config) (provision.Spec, error) {
	if agents.IsChiefName(ag.Name) {
		return provision.Spec{}, fmt.Errorf("agentprovision: chief %q is kind %s, which cannot be the chief yet "+
			"(its MCP client against muster's tools is unverified)", ag.Name, agents.KindHermes)
	}
	return agentspec.Build(ag, cfg, agentspec.Options{})
}

// specFor is the deployment-wide spec config with the model-provider
// credentials REPLACED by those of ag's owner (see [CredentialResolver]).
//
// 🔴 BOTH CREDENTIALS ARE OVERWRITTEN FOR EVERY KIND, NOT ONLY THE ONE THE KIND
// USES: a claude-code spec gets an empty OpenRouter key and a gateway spec an
// empty account map. agentspec places each only for its own kind today; zeroing
// the other means a future kind that reads both still cannot carry the admin's.
func (a *Adapter) specFor(ctx context.Context, ag agents.Agent) (agentspec.Config, error) {
	cfg := a.spec
	if a.creds == nil {
		return cfg, nil
	}
	cfg.OpenRouterAPIKey = ""
	if cfg.ClaudeCode != nil {
		cc := *cfg.ClaudeCode
		cc.Accounts = map[string]string{}
		cfg.ClaudeCode = &cc
	}
	switch agents.ResolveKind(ag.Kind) {
	case agents.KindClaudeCode:
		if cfg.ClaudeCode == nil || ag.CCAccount == "" {
			return cfg, nil // agentspec refuses this row, naming why
		}
		tok, err := a.creds.ClaudeToken(ctx, ag.OwnerID, ag.CCAccount)
		if err != nil {
			// The account NAME and the error only — never a token.
			return cfg, fmt.Errorf("agentprovision: agent %q: Claude account %q of its owner: %w", ag.Name, ag.CCAccount, err)
		}
		cfg.ClaudeCode.Accounts[ag.CCAccount] = tok
		// A chief running the gateway runtime beside ccd needs its owner's
		// OpenRouter key for that runtime. Without one the chief still starts —
		// its Claude Code sessions do not need it — and its gateway sessions fail
		// at the model call, naming the provider; that is said here too.
		if cfg.ChiefGatewaySidecar && agents.IsChiefName(ag.Name) {
			key, err := a.creds.OpenRouterKey(ctx, ag.OwnerID)
			if err != nil {
				a.log.Printf("agentprovision: chief %q: no OpenRouter key of its owner for the gateway runtime beside it "+
					"(%v); its gateway sessions will fail at the model call", ag.Name, err)
			} else {
				cfg.OpenRouterAPIKey = key
			}
		}
	default:
		key, err := a.creds.OpenRouterKey(ctx, ag.OwnerID)
		if err != nil {
			return cfg, fmt.Errorf("agentprovision: agent %q: its owner's OpenRouter key: %w", ag.Name, err)
		}
		cfg.OpenRouterAPIKey = key
	}
	return cfg, nil
}

// clearKickoffDelivery drops the delivery provenance on its own budget. See
// bookkeepingCtx — this is a record of what the stop did, not part of doing it.
func (a *Adapter) clearKickoffDelivery(ctx context.Context, ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	return a.store.ClearKickoffDelivery(ctx, ag.ID)
}

// recordUndeliveredKickoff writes the non-delivery where a human reads it.
//
// 🔴 IT WRITES THE KICKOFF *ERROR* AND NEVER THE KICKOFF *FLAG*. The two fields
// are one letter apart in a call site and opposite in meaning: kickoff_error is
// evidence that a send did not happen, KickedOff is a claim that one did. See
// the package doc.
func (a *Adapter) recordUndeliveredKickoff(ctx context.Context, ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	a.log.Printf("agentprovision: agent %d (%s) owes a first turn this deployment cannot "+
		"deliver (no gateway) — instance up, note left pending. %s", ag.ID, ag.Name,
		UndeliveredKickoffReason)
	if err := a.store.SetKickoffError(ctx, ag.ID, UndeliveredKickoffReason); err != nil {
		return fmt.Errorf("agentprovision: agent %d (%s): record undelivered kickoff: %w",
			ag.ID, ag.Name, err)
	}
	return nil
}

// refuseKickoff records an undeliverable kickoff as a TERMINAL refusal and returns
// [ErrKickoffUndeliverable]. Nothing has been created when it runs.
//
// 🔴 IT WRITES agents.StatusError AND NOT agents.kickoff_error, WHICH IS THE
// OPPOSITE CHOICE FROM recordUndeliveredKickoff BELOW, FOR ONE REASON: there is no
// instance. kickoff_error means "the instance exists and its first message was not
// delivered" — evidence a retry could act on, which is exactly how
// agents.DecideReconcile reads it (ActionRetryKickoff). Writing it here would
// describe a delivery failure against a pod that was never created, and point a
// future reconcile loop at an instance it cannot find. The status write says the
// truthful thing instead: this agent did not start, and here is why.
//
// 🔴 agents.StatusError IS ALSO THE ONLY STATUS THE CARD CANNOT TALK ITSELF OUT OF.
// agents.ComputeStatus returns early on it, so the row stays visibly failed;
// `provisioning` — which is what the row carries when this is reached, written by
// the create handler — would be refined by any live instance and is the state the
// package header calls worse than the bug.
//
// ⚠ THE STATUS WRITE'S OWN FAILURE IS RETURNED, NOT SWALLOWED, unlike fail()'s.
// fail() already has a cause worth reporting and must not lose it to bookkeeping;
// here the refusal IS the outcome, so a refusal nobody could record is a real
// failure of this call and the caller's log line should say which one happened.
func (a *Adapter) refuseKickoff(ctx context.Context, ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	// The log keeps the whole information of the line it replaces — which agent,
	// that nothing was delivered, and why — and adds that nothing was built.
	a.log.Printf("agentprovision: agent %d (%s) asked for a kickoff this deployment cannot "+
		"deliver — REFUSED, nothing provisioned. %s", ag.ID, ag.Name, KickoffRefusalReason)
	if err := a.store.UpdateStatus(ctx, ag.ID, agents.StatusError, "", KickoffRefusalReason); err != nil {
		return fmt.Errorf("agentprovision: agent %d (%s): record refused kickoff: %w",
			ag.ID, ag.Name, err)
	}
	return fmt.Errorf("agentprovision: agent %d (%s): %w: %s",
		ag.ID, ag.Name, ErrKickoffUndeliverable, KickoffRefusalReason)
}

// setStatus persists a status with no output and no error message.
//
// 🔴 IT TAKES NO ctx AND DERIVES ITS OWN, WHICH IS THE WHOLE POINT — see
// bookkeepingCtx. It used to take the operation's context, and that is how the one
// failure most in need of a durable record got none.
func (a *Adapter) setStatus(ctx context.Context, id int64, status string) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	if err := a.store.UpdateStatus(ctx, id, status, "", ""); err != nil {
		return fmt.Errorf("agentprovision: agent %d: persist status %q: %w", id, status, err)
	}
	return nil
}

// fail records a terminal failure on the agent and returns the error.
//
// ⚠ THE STATUS WRITE'S OWN FAILURE IS LOGGED, NOT RETURNED. The error being
// reported is what the caller needs; replacing it with "could not write the
// status" would lose the cause and report the bookkeeping.
func (a *Adapter) fail(ctx context.Context, ag agents.Agent, what string, cause error) error {
	ctx, cancel := a.bookkeepingCtx(ctx)
	defer cancel()
	msg := what + ": " + cause.Error()
	if err := a.store.UpdateStatus(ctx, ag.ID, agents.StatusError, "", msg); err != nil {
		a.log.Printf("agentprovision: agent %d (%s): could not record failure %q: %v",
			ag.ID, ag.Name, msg, err)
	}
	return fmt.Errorf("agentprovision: agent %d (%s): %s: %w", ag.ID, ag.Name, what, cause)
}

// opCtx is the budget for a lifecycle method: a.timeout, DETACHED from the
// caller's cancellation (the work outlives the request that started it) but
// KEEPING its values.
//
// 🔴 THE VALUES ARE THE POINT (accounts PR 3a). parent carries the caller's
// authz.Principal, and row-level security scopes every store call on this
// budget to it. This was context.Background()-derived when the methods took no
// context, which under RLS reads no rows: Dispatch would fail "load row" for
// every agent. context.WithoutCancel keeps the principal and drops both the
// cancellation and the deadline, so the budget is still a.timeout, fresh.
func (a *Adapter) opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), a.timeout)
}

// bookkeepingCtx is a FRESH, SHORT budget for the store write that RECORDS what an
// operation did — never the operation's own context.
//
// ⚠ ONLY "FRESH" IS GUARDED. A mutant reverting this to the operation's context dies
// on TestATimedOutDestroyStillDELETESTheRow and
// TestATimedOutOperationStillRECORDSItsFailure. A mutant replacing the timeout with a
// bare context.WithCancel — fresh but UNBOUNDED — SURVIVES every package, so "SHORT"
// is a statement of intent rather than a pinned property. The cost of the gap is the
// leak class DefaultOpTimeout's own doc exists to prevent: an unbounded bookkeeping
// write against a wedged database holds its goroutine for the life of the process.
// Measured, not supposed; recorded because a reader would otherwise take the whole
// sentence as covered.
//
// 🔴 A RECORD WRITTEN ON THE EXPIRED BUDGET OF THE THING IT IS RECORDING CANNOT
// LAND IN THE CASE THAT NEEDS IT MOST. The failure is exact: opCtx's budget covers
// the driver call AND every store write after it, so when a driver call consumes the
// whole budget, `fail`'s UpdateStatus runs on a dead context, returns `context
// deadline exceeded`, and is only logged. Net result: the cluster holds partial
// objects, the row still says whatever the handler wrote, and the sole record of the
// timeout is a line in this pod's stdout — which is the observable this package's
// header says it exists to remove, reproduced one layer down.
//
// ⚠ THIS DRAWS NO CONTRAST WITH opCtx, AND AN EARLIER REVISION IMPLIED ONE. It said
// this budget "ALSO survives shutdown cancellation … the operation itself is NOT
// solved here", which reads as though opCtx did not. It does: opCtx is
// context.Background()-derived too, so NEITHER is cancelled at shutdown. What this
// budget actually provides is a FRESH deadline rather than a shared one. Shutdown
// cancellation of an in-flight lifecycle operation is genuinely unsolved — for both —
// and naming it here as a property of one of them obscured that.
//
// ⚠ THE HELPERS DERIVE IT THEMSELVES RATHER THAN TAKING IT, so no call site can
// pass the wrong one. That is the structural form of the fix; a rule asking every
// caller to remember which context to hand a status write is a rule that gets
// obeyed at four sites and missed at the fifth.
//
// 🔴 IT IS DERIVED FROM parent WITH context.WithoutCancel, NOT FROM
// context.Background() (accounts PR 3a): FRESH still holds — WithoutCancel drops
// parent's deadline and cancellation, so an operation that consumed its whole
// budget does not take this write down with it — but parent's VALUES survive,
// and the principal is one of them. A Background budget acts as nobody under
// row-level security, so the record would land nowhere: the same "the only
// record is a log line" outcome the paragraph above exists to prevent.
func (a *Adapter) bookkeepingCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
}
