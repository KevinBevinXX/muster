package api

import (
	"context"
	"io"
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/runbooks"
)

// Extensions are the server's optional dependencies. A Server built without
// them still boots, serves the shell, answers /health and /readyz, and refuses
// every data route with a reason — which is the behaviour a first run against
// no database should have.
//
// 🔴 THIS STRUCT IS THE muster HALF OF A STRUCT THAT USED TO HOLD EIGHTEEN
// FIELDS, AND THE NINE THAT ARE MISSING ARE MISSING ON PURPOSE. Upstream it
// also carried Suggest, Attention, Tmux, Transcripts, TranscriptArchive,
// TermWrite, Layout, AutoApprove and GenSuggestions — every one of which backs
// a surface on the permission router's side of the route partition. Carrying
// them across would give this service nine dependencies it has no route for,
// and would make the extraction's central claim — that the two halves are
// separable — false in the type system while looking true in the diff.
//
// 🔴 `Suggest` IS THE ONE WHOSE ABSENCE IS LOAD-BEARING RATHER THAN TIDY, AND
// IT MUST NOT COME BACK. It is the store that owns the session table, and the
// whole of the upstream work that preceded this carve existed to remove that
// table from the domain layer so the domain layer could move without it. The
// bit it used to supply — SessionLink.DetailAvailable — is now composed from a
// SEAM instead; see UseExtensions and session_liveness.go. A reviewer who sees
// a `Suggest` field reappear here should read that as the extraction being
// undone, not as a convenience.
type Extensions struct {
	Notes     notes.Store
	Agents    agents.Store
	GitHub    github.Store
	Privilege privilege.Store
	Runbooks  runbooks.Store

	// AgentNamespacePrefix is the prefix [Extensions.AgentNamespace] builds a new
	// agent's stored namespace from. It is the DEPLOYMENT's prefix — the same
	// MUSTER_AGENT_NAMESPACE_PREFIX the provisioning driver is configured with —
	// and NOT a default this package chooses.
	//
	// 🔴 IT IS HERE BECAUSE A HARDCODED CONSTANT WAS SHIPPED IN ITS PLACE AND
	// DISAGREED WITH THE DRIVER, SILENTLY. The row-writing handler used
	// agents.NamespaceFor(name) over agents.NamespacePrefix ("devpod-") while the
	// driver created `muster-agent-<name>`. Nothing fails when those differ: the
	// value is recorded, served on GET /api/agents and labelled onto the
	// instance's objects, and NOTHING places anything with it — so a card names a
	// namespace that holds nothing and reads as "never provisioned" while the
	// instance runs one namespace over. Measured live.
	//
	// 🔴 EMPTY IS NOT A SUPPORTED PRODUCTION STATE WHEN AGENTS CAN BE CREATED, and
	// defects() says so rather than this comment: an unset prefix beside a wired
	// Agents store and Provisioner means the wiring was FORGOTTEN, and the
	// consequence is the defect above rather than a missing feature. It resolves
	// to agents.NamespacePrefix so a test fixture need not set it, which is
	// exactly why the omission has to be caught somewhere that is not a reader's
	// attention.
	AgentNamespacePrefix string

	// SessionLiveness answers "does a transcript record for this session still
	// exist" — the one bit SessionLink.DetailAvailable is made of.
	//
	// 🔴 IT IS A PORT, NOT A STORE, BECAUSE THE DATA IS NOT THIS SERVICE'S. The
	// session table stays with the permission router. Nil is TOLERATED and is not
	// a supported production state: the wrapper still applies, but every link
	// resolves to false and the three-state transcript row collapses to two. See
	// withSessionLiveness for why that is tolerated rather than fatal, and
	// TestExtensionsWiringPairsNotesWithALivenessProbe for what stops it shipping.
	SessionLiveness SessionLivenessProbe

	// PrivilegeApply applies/removes a granted profile's Kubernetes RBAC live.
	// Nil means grants are recorded but not applied — the handlers log and carry
	// on, mirroring a nil Provisioner.
	//
	// 🔴 "MIRRORING A NIL Provisioner" DESCRIBES THE HANDLERS AND NOT THE
	// CONSEQUENCE, AND THE DIFFERENCE IS THE WHOLE OF defects()' SECOND ENTRY. A
	// nil Provisioner dims a tab. A nil applier BESIDE a wired Provisioner and a
	// wired Privilege store is a READINESS DEFECT: /readyz refuses, because the
	// grant chip then claims a permission a real ServiceAccount does not have. The
	// two nils look alike at the call site and do not behave alike at the pod.
	//
	// ⚠ ITS PARENTHETICAL USED TO READ "(no in-cluster client)", WHICH NAMED THE
	// ONLY REASON THIS COULD BE NIL WHEN THERE WAS NO IMPLEMENTATION TO WIRE.
	// There is one now — cmd/muster-server builds internal/agentprivilege over the
	// same driver the lifecycle tier holds, behind MUSTER_AGENT_PRIVILEGE_APPLY —
	// so nil today usually means the operator has not armed that variable, which
	// is a different thing to go and check.
	PrivilegeApply PrivilegeApplier

	// TagAutoDispatch arms the `auto:dispatch` routing tag
	// (MUSTER_TAG_AUTODISPATCH). It ships FALSE and implements PLUMBING ONLY
	// (recognition + eligibility + a log line + the metric) — no hands-off
	// dispatch behaviour. Rationale: the MANUAL dispatch loop has never closed
	// end-to-end, and automating a path that has never once worked by hand
	// converts an unproven feature into an unattended one. While false,
	// `auto:dispatch` behaves as a descriptive tag.
	TagAutoDispatch bool

	// Provisioner drives agent instance LIFECYCLE (provision/start/stop/delete,
	// state, logs). Nil disables the seven lifecycle routes even if Agents is set,
	// through requireLifecycleProvisioner.
	Provisioner Provisioner

	// Gateway drives agent CHAT. Nil disables the two chat routes even if
	// Provisioner is set, through requireGatewayProvisioner.
	//
	// 🔴 IT IS A SEPARATE FIELD FROM Provisioner ON PURPOSE — see [Gateway]. The
	// two nils are independent, so a deployment that can provision but not chat
	// renders truthfully instead of having to choose which lie to tell.
	Gateway Gateway

	// Kinds answers which agent kinds a dispatch may ask for and, for the
	// claude-code kind, which Claude account it runs on. Nil means the gateway
	// kind alone — every deployment before kinds existed — and the dispatch form
	// shows no picker.
	Kinds AgentKinds

	// GitHubOAuth configures the OAuth web flow for connecting an account.
	GitHubOAuth GitHubOAuthConfig
}

// GitHubOAuthConfig holds the OAuth App credentials + the public base URL used
// to build the redirect URI.
type GitHubOAuthConfig struct {
	ClientID     string
	ClientSecret string
	// BaseURL is the externally reachable origin used to construct the callback
	// URL. If empty the callback is derived from the inbound request host.
	BaseURL string
}

// Configured reports whether the OAuth App credentials are present.
func (c GitHubOAuthConfig) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// UseExtensions attaches the dependencies. Call before Handler.
//
// 🔴 IT WRAPS Notes, AND THAT WRAP IS THE WHOLE REASON THIS FUNCTION EXISTS
// RATHER THAN A PLAIN FIELD ASSIGNMENT. internal/notes does not join the
// session table — it cannot, the table is another service's — so every
// SessionLink it returns has DetailAvailable=false until something composes it.
// This is the ONE place that happens. Doing it in handlers instead would be
// twenty-odd places to forget, and forgetting is SILENT: false renders the
// words "no transcript recorded" over a live transcript, with nothing logged
// and nothing 404ing. Upstream that exact conflation disguised a months-long
// outage as ordinary retention.
//
// ⚠ It is applied here rather than in the binary's wiring on purpose: this is
// the only door into s.ext, so a fixture and the binary get the same store.
func (s *Server) UseExtensions(ext Extensions) {
	s.extDefects = ext.defects()
	ext.Notes = withSessionLiveness(ext.Notes, ext.SessionLiveness)
	s.ext = ext
}

// defects lists wiring mistakes that must not be SERVED with, as opposed to
// dependencies that are merely absent.
//
// 🔴 THE DISTINCTION IS "DEGRADES VISIBLY" VERSUS "LIES SILENTLY", AND IT IS
// THE ONLY test applied here. A nil Runbooks store is not a defect: the
// Runbooks tab renders an empty list, which is TRUE — this server has no
// runbooks. A notes store with no liveness probe IS a defect: every transcript
// row renders "no transcript recorded" over transcripts that may be perfectly
// alive, and there is no request that fails, nothing logged, and nothing for an
// operator to notice. The first is a configuration; the second is a page that
// states a falsehood.
//
// 🔴 THE CONSEQUENCE IS /readyz, NOT A PANIC, AND THAT IS DELIBERATE. A panic
// in a wiring call is a crash loop that takes the whole service down over a
// feature; a readiness failure keeps the process up, keeps /health green for
// liveness, and keeps the pod OUT OF SERVICE until someone reads the reason —
// which handleReady prints. It is the loudest signal that is not also an
// outage.
//
// ⚠ IT IS A METHOD ON Extensions, NOT ON Server, SO IT CAN BE TESTED WITHOUT
// ONE — and so a future second caller (a config validator, a boot log) reaches
// the same list rather than writing a second one.
func (e Extensions) defects() []string {
	var out []string
	if e.Notes != nil && e.SessionLiveness == nil {
		out = append(out, "Notes is wired but SessionLiveness is not: every task's transcript "+
			"links would render \"no transcript recorded\" whether or not a transcript exists, "+
			"silently and on every surface. Wire a liveness probe (internal/router satisfies it) "+
			"or leave Notes unset.")
	}
	// 🔴 THIS ENTRY IS cmd/muster-server/doc_seams.go ENTRY 2 FALLING DUE, AND
	// ENTRY 2 NAMED THE EXACT MOMENT. A privilege store that RECORDS grants
	// nobody applies was defensible only while no agent pod could exist to hold
	// one: there was no surface on which a user was told a privilege was live when
	// it was not, because there was no live agent. A wired Provisioner creates
	// real instances, so the grant chip becomes a page stating a falsehood — the
	// chip says granted over a ServiceAccount with none of the permissions — and
	// that is the "lies silently" side of this function's own line.
	//
	// ⚠ IT IS FAIL-CLOSED AND IT WILL REFUSE A DEPLOYMENT THAT USED TO COME UP.
	// It is listed here rather than wrapped at a route — unlike the nil
	// Provisioner, which IS wrapped — because there is no single route to refuse:
	// the falsehood is rendered by every surface that shows a grant, and a grant
	// recorded through one route is read back through several.
	//
	// ✅ THE REFUSAL IS NOW ESCAPABLE WITHOUT AN IMAGE CHANGE, AND THE PARAGRAPH
	// THAT SAID OTHERWISE IS REPLACED RATHER THAN LEFT TO READ AS OPEN. It said:
	// "That is the point and it is the choice entry 2 offered: wire a
	// PrivilegeApplier in the same change, or make the combination unready." The
	// second option was taken in 2026-09, when the operator's standing decision was
	// that privilege stays WHOLE in the permission router — route, store AND
	// applier — which left nothing in this module able to satisfy the interface, so
	// the only exits were unsetting the provisioner or unsetting the database. THAT
	// DECISION WAS REVERSED (2026-09-28): internal/agentprivilege implements the
	// interface over the SAME provisioning driver the lifecycle tier holds, and
	// cmd/muster-server wires it behind MUSTER_AGENT_PRIVILEGE_APPLY (off by
	// default, because applying a grant needs RBAC-writing permissions muster's own
	// ServiceAccount may well not have — internal/provision/k8s.PolicyRBACPrerequisite
	// enumerates them, and it is MORE than the `escalate` and `bind` verbs this
	// paragraph used to name on their own).
	//
	// ⚠ "A DEPLOYMENT CHOICE" IS WHAT THIS HEADING USED TO CLAIM, AND IT
	// OVERSTATED BY ONE ESCAPE. Setting the variable is a deployment choice;
	// unsetting it again is NOT, because on the only kind of deployment that can
	// arm it — one with a database and a provisioner — taking it away re-enters
	// this very defect and pulls the pod from its Service. The tier arms forward
	// cheaply and rolls back expensively; cmd/muster-server/provisioner.go
	// prerequisite 2 states the asymmetry in full.
	//
	// 🔴 NONE OF WHICH CHANGES THIS FUNCTION — THE PREDICATE AND ITS THREE CONJUNCTS
	// ARE UNTOUCHED, AND THAT IS WORTH STATING BECAUSE THE OBVIOUS READING OF
	// "AN APPLIER EXISTS NOW" IS THAT THE CHECK CAN RELAX. It cannot: the check is
	// about whether THIS SERVER can apply what it records, and an unarmed applier
	// is as unable as an absent one. What the reversal changes is the refusal TEXT
	// below, which now names a variable an operator can set.
	//
	// ⚠ IT OVER-TRIGGERS FOR A PROVISIONER THAT CREATES NOTHING, and that is still
	// stated rather than fixed. The noop driver records instead of provisioning, so
	// a grant over one of its instances lies about a pod that was never real either
	// — harmless. Distinguishing the two would mean this function reading the
	// driver's capabilities, which makes a readiness check depend on a backend's
	// self-report; the fail-closed direction is cheaper and wrong only in the
	// direction of refusing to serve. ⚠ AND THE ESCAPE FROM THE OVER-TRIGGER IS NOW
	// CHEAP RATHER THAN ABSENT, which is the one thing the reversal does change
	// here: arming the applier over the noop driver satisfies this check and makes
	// every grant FAIL LOUDLY (provision.Grant refuses a driver that implements no
	// PolicyGranter, naming it) instead of being recorded silently. So the
	// over-trigger no longer forces a choice between an unready pod and a lie.
	if e.Provisioner != nil && e.Privilege != nil && e.PrivilegeApply == nil {
		out = append(out, "Provisioner is wired but PrivilegeApply is not, while a privilege "+
			"store IS wired: grants would be RECORDED and never applied, over agent instances "+
			"that now really exist. Every surface showing a grant would claim a permission the "+
			"instance's ServiceAccount does not have. TWO WAYS OUT. (1) Set "+
			"MUSTER_AGENT_PRIVILEGE_APPLY=1 to apply grants through the provisioning driver "+
			"(internal/agentprivilege); this server's own ServiceAccount then needs the rbac "+
			"permissions enumerated in internal/provision/k8s.PolicyRBACPrerequisite, which is "+
			"MORE than the `escalate` and `bind` verbs this text used to name — those two are "+
			"additional checks on top of ordinary create/get/update/delete/list writes, not "+
			"substitutes for them. (2) Unset MUSTER_AGENT_PROVISIONER, which leaves the "+
			"provisioner unwired. \"Leave the privilege store unset\" was listed here as a third "+
			"way and is NOT one: nothing gates that store on a variable of its own, so it means "+
			"running with no database at all, which drops notes, agents, runbooks and GitHub too. "+
			"See cmd/muster-server/doc_seams.go entry 2, which named this exact combination as "+
			"the moment its own argument dies, and provisioner.go prerequisite 2 for why "+
			"unsetting the apply variable again is an outage rather than a rollback.")
	}

	// 🔴 THIRD ENTRY: A FORGOTTEN NAMESPACE PREFIX IS THE "LIES SILENTLY" SHAPE IN
	// ITS PUREST FORM, AND IT IS THE DEFECT THIS ENTRY WAS ADDED FOR. Every agent
	// row this server writes records AgentNamespacePrefix+name. Nothing places
	// anything with that value — it is recorded, served on GET /api/agents and
	// labelled onto the instance's objects — so a prefix that does not match the
	// driver's produces rows naming namespaces that hold nothing, with no failing
	// request, nothing logged, and nothing for an operator to notice. That is
	// exactly what shipped: `devpod-lively-newt` on the row, `muster-agent-lively-newt`
	// in the cluster.
	//
	// 🔴 IT IS A DEFECT RATHER THAN A DEFAULT *BECAUSE* THE DEFAULT EXISTS.
	// AgentNamespace resolves an empty prefix to agents.NamespacePrefix, which is
	// what keeps ~20 struct-literal test fixtures from having to set it — and is
	// therefore precisely what would let a production wiring omit it and look
	// fine. The binary always resolves a non-empty prefix
	// (agents.ResolveNamespacePrefix, from cmd/muster-server's config load), so
	// empty HERE, with agents creatable, can only mean the wiring was dropped.
	//
	// ⚠ IT IS GATED ON Provisioner AS WELL AS Agents ON PURPOSE. With no
	// provisioner there is no driver to disagree with and no route that can create
	// an agent (all three callers of createAndDispatchAgent sit behind
	// requireLifecycleProvisioner), so the value is inert and refusing to serve
	// over it would be the "configuration, not a page that states a falsehood"
	// case this function's header rules out.
	if e.Agents != nil && e.Provisioner != nil && e.AgentNamespacePrefix == "" {
		out = append(out, "Agents and Provisioner are wired but AgentNamespacePrefix is empty: "+
			"every agent row created here would record agents.NamespacePrefix+name while the "+
			"provisioning driver places the instance under the prefix IT was configured with "+
			"(MUSTER_AGENT_NAMESPACE_PREFIX). Nothing fails when those disagree — the value is "+
			"recorded, served on GET /api/agents and labelled onto the instance's objects, and "+
			"nothing places anything with it — so `kubectl -n <what the row says>` returns "+
			"nothing and reads as \"never provisioned\" while the instance runs one namespace "+
			"over. That is a shipped defect, not a hypothetical. Set it from the same config "+
			"value the driver gets: cmd/muster-server passes cfg.AgentNamespacePrefix, which "+
			"agents.ResolveNamespacePrefix has already defaulted.")
	}
	return out
}

// AgentNamespace is the ONE expression that turns an agent's slug name into the
// namespace its row records.
//
// 🔴 IT IS A METHOD ON Extensions, NOT A PACKAGE FUNCTION, SO IT CANNOT BE CALLED
// WITHOUT THE DEPLOYMENT'S PREFIX IN HAND. The defect it replaces was a
// zero-argument call — agents.NamespaceFor(name) — that captured a package
// constant while the driver used the configured prefix; every line of the caller
// looked correct. Reaching this value now requires holding the wiring that
// carries the prefix, and defects() refuses to serve when that wiring is empty
// beside a creatable agent.
//
// ⚠ IT IS EXPORTED FOR THE GUARD AS WELL AS THE HANDLER.
// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith calls it from
// cmd/muster-server and compares the result against the namespace the real
// driver is OBSERVED to create — which is only a claim about the production path
// if the guard and the handler evaluate the same expression, rather than two
// copies of it.
func (e Extensions) AgentNamespace(name string) string {
	return agents.NamespaceFor(e.AgentNamespacePrefix, name)
}

// ProvisionerUnwiredField is the JSON key both provisioner wrappers set to `true` on
// its 503 body.
//
// 🔴 IT IS THE FIELD, NOT THE SENTENCE, THAT A CLIENT BRANCHES ON — the same
// shape HookUnarmedField gives the machine tier, and for a sharper reason. Every
// OTHER 503 a caller meets on these routes is a transient outage worth retrying;
// this one is a permanent property of the BUILD, and a client that cannot tell
// them apart retries for ever against a server whose answer can never change.
// A discriminator made of prose is one a reword silently breaks.
const ProvisionerUnwiredField = "provisionerUnwired"

// The provisioner wrappers refuse a route that cannot do its work without
// Extensions.Provisioner, rather than letting the handler reach a nil interface.
//
// 🔴 THIS EXISTS BECAUSE "REGISTERED AND REFUSES" WAS NOT TRUE, AND THE BOOT
// BANNER SAID IT ON EVERY START. Measured against the image this tree builds,
// with Provisioner nil (the only state cmd/muster-server can be in today):
// POST /agents answered 200 and rendered a card while the row sat
// `provisioning` for ever; POST /agents/{id}/start answered 200 with 3,590
// bytes of card HTML; POST /agents/{id}/stop answered 200; DELETE /agents/{id}
// answered 200 and htmx removed the card while the row was still there on the
// next read; and GET /agents/{name}/logs/stream answered 200 with `: connected`
// and then EOF, which a reader sees as "this agent has no logs".
// muster_panics_total{source="goroutine"} stood at 4. Every one of those is a
// surface stating a falsehood with nothing logged where a user is looking.
//
// 🔴 IT IS THE SAME OBSERVABLE doc_seams.go ENTRY 1 ARGUES AGAINST. That entry
// refuses to wire a FAKE provisioner because a fake "would make the Agents tab
// LOOK operational: dispatch would report success, no pod would exist, and the
// agent would sit in `provisioning` for ever." Wiring nil produced exactly that,
// byte for byte. The nil was never the honest option on its own — this wrapper
// is what makes it one.
//
// 🔴 WHY A WRAPPER AND NOT A defects() ENTRY, STATED SO THE NEXT READER DOES NOT
// RE-OPEN IT. Adding `Provisioner == nil && Agents != nil` to defects() would
// fail /readyz, which is fail-closed and simpler — and would make this module
// UNDEPLOYABLE until the provisioner adapter lands, taking the task board, notes,
// repos, projects and runbooks down with a seam that is open on purpose. The
// refusal belongs at the routes that cannot work, not at the whole process. With
// this wrapper a nil Provisioner moves from the "lies silently" side of
// defects()'s own line to the "degrades visibly" side, which is precisely why it
// is NOT listed there.
//
// ⚠ IT IS THE INNER WRAPPER, WITH AUTH OUTSIDE IT, AND THE ORDER IS LOAD-BEARING.
// Registered as requireSession(requireLifecycleProvisioner(h)) the session check runs
// first, so an anonymous caller is refused before this reply can tell them which
// dependencies this deployment has wired. The reverse order turns every one of
// these routes into an unauthenticated probe of the server's build.
//
// ⚠ IT DOES NOT MOVE THE ROUTE GOLDEN, WHICH IS WHY THE ROUTES STAY REGISTERED.
// The golden records patterns only (see routes_golden_test.go's KNOWN LIMIT
// banner), and a wrapper changes the handler, not the pattern. doc_seams.go
// entry 1 says the golden moving for this nil would itself be a defect; it does
// not move.
// 🔴 THERE ARE TWO WRAPPERS NOW, NOT ONE, AND EVERY WORD ABOVE STILL APPLIES TO
// BOTH. The split is described on [Gateway]: one nil used to gate nine routes, so
// wiring the seven lifecycle ones meant also claiming the two chat ones. Each
// wrapper refuses at the same door, in the same vocabulary, naming the dependency
// the route actually needs — which is the reason the split was preferred over an
// adapter whose chat methods returned a rendered refusal from inside a handler.
// One condition, one refusal shape.
func (s *Server) requireLifecycleProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Provisioner == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent provisioner wired, so it cannot " +
					"provision, start, stop, destroy or stream logs from an agent. " +
					"This is a declared seam, not an outage and not your credential: " +
					"see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
}

// requireGatewayProvisioner is the chat half. See requireLifecycleProvisioner.
func (s *Server) requireGatewayProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Gateway == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent gateway wired, so it cannot chat " +
					"with an agent. Provisioning may still work — the two are separate " +
					"dependencies. This is a declared seam, not an outage and not your " +
					"credential: see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
}

// PrivilegeApplier applies a granted privilege profile's Kubernetes RBAC to an
// agent's ServiceAccount, live (no pod restart). Implemented by
// internal/provision (which holds the in-cluster client + knows the
// SA/namespace naming); defined here consumer-side like Provisioner.
type PrivilegeApplier interface {
	// ApplyGrant ensures the RBAC objects for profile (its ClusterRules and/or
	// NamespaceRules) exist, bound to the agent's ServiceAccount. Idempotent.
	ApplyGrant(ctx context.Context, agentName, namespace string, profile privilege.Profile) error
	// RemoveGrant deletes the RBAC objects this service created for
	// (agent, profile).
	RemoveGrant(ctx context.Context, agentName, namespace, profileName string) error
}

// ProfileReapplier reconciles a running instance to a freshly-rendered spec, which
// rolls it. Optional: the grant path and the model-change path type-assert for it, so a
// nil provisioner (and the RBAC-only path) is unaffected.
//
// ⚠ ITS PREVIOUS WORDING WAS "re-applies an agent's granted-profile env/kubeconfig to
// its running POD (a HELM UPGRADE; the pod rolls)" — the same helm-and-pod vocabulary
// removed from Destroy, TailLogs and StreamLogs below, and missed in the same sweep
// that removed them. It is false of the only implementation (which calls the driver's
// Update), false for a driver with no pods, and it names a mechanism the provisioner
// contract deliberately does not have.
type ProfileReapplier interface {
	ReapplyProfiles(ctx context.Context, agentID int64) error
}

// Provisioner is the agent-instance LIFECYCLE driver the agent handlers depend
// on: bring one into existence, scale it, destroy it, read its state and its
// logs. It is defined here (consumer side) so this package does not import the
// heavy cluster dependencies unless the binary wires a real provisioner in.
//
// ⚠ CHAT IS NOT HERE ANY MORE — see [Gateway] for why the two are separate.
type Provisioner interface {
	// Dispatch provisions an instance for the agent when kickoff is true, and
	// creates NOTHING when it is false — see the note below on both halves.
	//
	// 🔴 THIS DOC USED TO DESCRIBE BEHAVIOUR NO IMPLEMENTATION HAS. It read
	// "provisions a pod for the agent and, when kickoff is true, sends the note as
	// the first message once the gateway is reachable", which is wrong twice over
	// for internal/agentprovision — the only implementation in this module:
	//
	//   - it does NOT send the note. Delivery needs an api.Gateway and there is no
	//     implementation of one here, so the non-delivery is recorded on the row
	//     instead (agentprovision.UndeliveredKickoffReason). An interface doc
	//     promising a delivery is how a future implementer comes to call
	//     SetKickedOff, which is the one thing that package must never do.
	//   - kickoff=false creates NOTHING rather than provisioning-then-storing-
	//     stopped. The caller's OTHER action is the UI's "Save for later", whose
	//     gate check (agents.go, the `gate:<reason>` refusal) allows a save on the
	//     stated grounds that it "provisions nothing".
	//
	// An implementer that provisions on kickoff=false, or that reports a delivery
	// it did not make, is wrong against this interface — not merely different.
	//
	// 🔴 REFUSING A KICKOFF IT CANNOT DELIVER IS ALSO LEGAL, AND IS WHAT
	// internal/agentprovision NOW DOES when nothing in its process could deliver
	// one: it creates nothing and returns an error
	// (agentprovision.ErrKickoffUndeliverable). The paragraph above describes the
	// CREATE-THEN-RECORD path. Both are honest; what this interface forbids is the
	// third option, doing the expensive half and reporting success.
	//
	// ⚠ THIS USED TO SAY CREATE-THEN-RECORD WAS "the gateway-configured one only",
	// WHICH MADE A CONFIGURED GATEWAY SOUND SUFFICIENT. It was not while nothing
	// CALLED the gateway. internal/agentkickoff now does, and the first bullet
	// above is superseded for a deployment that names a gateway: the adapter
	// still never sends the note itself, but it records no non-delivery either —
	// it creates the instance and leaves the owed row to that deliverer, which is
	// the one place SetKickedOff is called. See agentprovision.KickoffDeliveryWired.
	//
	// ✅ AND THE REFUSAL NOW REACHES THE CALLER, through
	// KickoffUndeliverableReason below rather than through this method. This
	// paragraph read "THE CALLER OF THIS METHOD IN THIS PACKAGE CANNOT SURFACE THAT
	// REFUSAL … A refusal an operator can see needs a synchronous check in the
	// create handler, before the row", and that check exists: handleAgentCreate
	// answers 409 with the reason before Agents.Create. The asynchronous half is
	// unchanged and still true of THIS method — createAndDispatchAgent calls it
	// inside safeGo, after the row exists — which is why the pre-flight is a
	// separate question rather than a different return value here.
	Dispatch(agentID int64, kickoff bool) error
	// Start brings a stopped or never-provisioned agent up, creating its instance
	// when the driver reports the backend does not have one.
	//
	// ⚠ IT DOES NOT KICK OFF, AND THIS DOC SAID IT DID. It read "(kicking off if
	// pending)" — the same retracted claim Dispatch's doc above corrects, left
	// behind in the sibling method for one round. The only implementation records
	// the non-delivery instead; see agentprovision.UndeliveredKickoffReason.
	Start(agentID int64) error
	// Stop scales a running agent down to zero replicas, keeping its declaration.
	// An instance the backend does not have is already stopped, not an error.
	Stop(agentID int64) error
	// Destroy removes the instance and everything the driver created for it, then
	// deletes the stored row.
	//
	// ⚠ IT DELETES THE ROW ONLY WHEN THE BACKEND HOLDS NOTHING, and this doc did
	// not say so — which is the contract the lifecycle adapter added. A terminal
	// ownership refusal or an unreachable backend KEEPS the row, because the row is
	// then the only record of a live instance.
	//
	// ⚠ ITS PREVIOUS WORDING WAS "uninstalls the release and deletes the
	// namespace" — helm and Kubernetes vocabulary in an interface whose Instances
	// doc makes a 🔴 point of having removed exactly that, and false for any driver
	// without namespaces.
	Destroy(agentID int64) error
	// Instances returns the live state of every agent workload, for status
	// reconciliation.
	//
	// 🔴 IT IS provision.Instance, NOT A POD, AND THE RENAME IS THE CARVE. The
	// method this replaces was `Pods` returning a Kubernetes-shaped struct, which
	// made every status consumer in this package a Kubernetes consumer by
	// transitivity. The provisioner contract's own vocabulary is what lets
	// agents.ComputeStatus answer for an agent running under any driver.
	Instances(ctx context.Context) ([]provision.Instance, error)
	// TailLogs returns the last N log lines of the agent's running instance.
	TailLogs(ctx context.Context, a agents.Agent, lines int64) (string, error)
	// StreamLogs follows the agent's instance output, invoking emit per line.
	StreamLogs(ctx context.Context, a agents.Agent, emit func(string)) error
	// AttachTerminal attaches a terminal to the agent's live session until ctx
	// ends or the session's client exits: what is written to stdin is typed,
	// what the terminal shows arrives on stdout, and each size on resize is
	// applied. readOnly attaches a client that cannot type.
	//
	// It refuses — agents.ErrNoTerminal for a kind with no session,
	// provision.ErrUnsupported for a driver that cannot exec — rather than
	// returning without having attached anything.
	//
	// ⚠ ON THE INTERFACE RATHER THAN TYPE-ASSERTED, for the reason
	// KickoffUndeliverableReason below gives: an optional interface an adapter
	// forgets yields ok=false with no trace, and the terminal route would then
	// refuse every agent while every capability reported it could exec.
	AttachTerminal(ctx context.Context, a agents.Agent, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error
	// KickoffUndeliverableReason answers why this process cannot deliver an
	// agent's FIRST TURN, and "" when it can. It must not create, write or
	// provision anything: it is the question Dispatch answers by refusing, asked
	// before anything exists to refuse over.
	//
	// 🔴 IT IS ON THIS INTERFACE RATHER THAN BEING A TYPE ASSERTION, AND THE
	// POLARITY OF THE FAILURE IS WHY. An optional interface that an
	// implementation does not satisfy yields ok=false and leaves NO TRACE —
	// doc_seams.go entry 3 is about exactly that — and here the no-trace branch
	// is the permissive one: no pre-flight, so a dispatch the deployment cannot
	// satisfy answers 200 again and the operator is back to reading a `running`
	// card over an agent nobody told what to do. A method on the interface makes
	// omitting it a compile error instead.
	//
	// 🔴 A CALLER MUST NOT RE-DERIVE THIS FROM ITS OWN CONFIGURATION. The answer
	// has to be the SAME fact the implementation's own Dispatch branches on, or
	// the pre-flight and the dispatch disagree — which is worse than no
	// pre-flight, because it refuses requests that would have worked or admits
	// ones that will not. internal/agentprovision returns the field Dispatch
	// reads; see its own doc on why.
	//
	// ⚠ "" IS DELIVERABLE, so the zero return is the permissive one. The
	// fail-closed argument lives on agentprovision.Config.KickoffDeliverable,
	// where a wiring can FORGET a struct field; a method cannot be forgotten.
	KickoffUndeliverableReason() string
}

// Gateway is the agent CHAT surface: talking to a running instance's model
// gateway.
//
// 🔴 IT IS A SEPARATE INTERFACE FROM Provisioner, AND THE SPLIT IS THE WHOLE
// POINT. Both used to be one, behind one nil and one wrapper, so the seven
// LIFECYCLE methods could not be wired without also claiming the two CHAT ones.
// That is not a packaging preference: provision.Provisioner — the driver contract
// a lifecycle adapter is built on — has no notion of a chat turn at all, so an
// adapter over it can implement the seven honestly and the two not at all. Behind
// one interface the only ways to express that were to wire a stub (which
// doc_seams.go entry 1 rejects, because dispatch reporting success over a pod that
// does not exist is the defect the wrapper was built to remove) or to leave
// lifecycle unwired for as long as chat is. Two interfaces make "lifecycle works,
// chat honestly refuses" sayable.
//
// ⚠ A DEPLOYMENT MAY WIRE EITHER, BOTH, OR NEITHER, and each combination has a
// truthful rendering. Chat without lifecycle is unusual but not incoherent — an
// installation whose instances are provisioned by something else entirely.
type Gateway interface {
	// Chat sends a user message to the agent gateway under the given chat-session
	// key (each session = an independent gateway context), streaming assistant
	// deltas to emit, and returns the full assistant reply. It carries no tools.
	//
	// ⚠ IT DOES NOT RETURN agents.ErrResponsesUnsupported, AND THE ASYMMETRY WITH
	// ChatWithTools BELOW IS THE POINT. An implementation picks the transport
	// itself — the one documented on agentgateway.Gateway.Chat runs over the
	// responses endpoint and drops to chat-completions on a 404 — because no caller
	// has information about which wire format an agent's image speaks. There is
	// nothing for a caller to branch on here, so a caller that received that
	// sentinel from ChatWithTools has exactly one move: call this.
	Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error)
	// ChatWithTools runs a tool-enabled turn via the gateway's responses API
	// under the given chat-session key: the model gets the native function tools
	// and dispatch executes each call in-process. emit (nil-safe) streams live
	// text/thinking deltas plus tool_call/tool_result events as the loop runs.
	// Returns agents.ErrResponsesUnsupported when the agent's gateway lacks the
	// responses API, so the caller can fall back to Chat.
	ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef, dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error)
}

// AgentKinds is the dispatch side of agent kinds (cmd/muster-server implements it
// over internal/ccpool).
type AgentKinds interface {
	// Enabled is the kinds a dispatch may name, in picker order. It always
	// includes agents.KindGateway.
	Enabled() []string
	// ClaudeAccounts is the configured Claude account NAMES (never tokens), for
	// the pin picker. Empty when the claude-code kind is not enabled.
	ClaudeAccounts() []string
	// SelectClaudeAccount picks the account a new claude-code agent runs on, or
	// returns the pin when it names a configured account. An error is a dispatch
	// refusal.
	SelectClaudeAccount(ctx context.Context, pin string) (string, error)
}

// enabledKinds is Extensions.Kinds.Enabled with the nil case resolved.
func (e Extensions) enabledKinds() []string {
	if e.Kinds == nil {
		return []string{agents.KindGateway}
	}
	return e.Kinds.Enabled()
}

// kindEnabled reports whether a dispatch may ask for kind.
func (e Extensions) kindEnabled(kind string) bool {
	for _, k := range e.enabledKinds() {
		if k == agents.ResolveKind(kind) {
			return true
		}
	}
	return false
}
