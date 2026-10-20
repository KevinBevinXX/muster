package main

import (
	"fmt"
	"log"
	"slices"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/agentstatus"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// ---------------------------------------------------------------------------
// THE PROVISIONER SEAM'S CLOSING HALF.
//
// 🔴 THIS FILE IS WHAT MAKES internal/provision/k8s REACHABLE FROM A PROCESS,
// AND THAT WAS THE WHOLE MECHANICAL POINT. Before it, the Kubernetes driver —
// 2,565 non-test lines, fully tested against a fake clientset — was on the
// not-linked ledger in internal/modulegate/linkage_test.go: it compiled, its
// tests passed, and `go list -deps ./cmd/...` did not resolve it, so no process
// could execute a line of it. That ledger is ASSERTED, not an allowlist: it fails
// when the set shrinks as well as when it grows, so the two entries this change
// removes are the signal, not paperwork.
//
// 🔴 AND "IMPORTED" IS NOT "USED" — the ledger's own header says so, because an
// import can be a blank `_` that executes nothing. The driver is CONSTRUCTED
// here, from a real in-cluster client, behind a configuration value that names
// it; TestBuildingTheKubernetesProvisionerUsesTheRealDriver pins that the
// wired-up adapter reports the kubernetes driver's own name rather than any
// stand-in.
//
// ✅ CHAT WAS THE OPEN HALF AND IT IS WIRED NOW, BY A SECOND TYPE RATHER THAN BY
// WIDENING THE FIRST. The paragraph here used to read "WHAT IS STILL OPEN AFTER
// THIS FILE: chat. The adapter satisfies api.Provisioner (lifecycle) and NOT
// api.Gateway". The adapter still does not, and must not: internal/agentgateway
// is a separate type over the SAME driver, assigned to api.Extensions.Gateway
// below. The split's purpose was that each half can be wired independently, and
// the way to use that is two implementations — not one type growing two methods
// and losing the ability to refuse honestly.
//
// 🔴 BOTH HALVES SHARE ONE DRIVER INSTANCE, AND THAT IS LOAD-BEARING RATHER THAN
// THRIFTY. The gateway asks the driver where an agent is reachable; the lifecycle
// adapter asks the same driver to create it there. Two driver instances built from
// the same configuration would agree today and diverge the moment anything about a
// driver is stateful or per-instance — and the failure would be a chat turn that
// resolves no endpoint for a pod that demonstrably exists.
//
// 🔴 TWO PREREQUISITES FOR `kubernetes` THAT LIVE OUTSIDE THIS REPOSITORY, NAMED
// HERE BECAUSE NOTHING ELSE IN IT CAN CHECK THEM:
//
//  1. RBAC. This module ships no Kubernetes manifest of any kind, so nothing here
//     grants muster's own ServiceAccount the Deployment / Service / Secret /
//     ConfigMap / ServiceAccount / Namespace permissions internal/provision/k8s
//     needs. Every dispatch then 403s — and a 403 from the apiserver reads as a
//     driver defect rather than a missing Role. CLOSING CONDITION: a merged change
//     in the deployment's own repository adding that Role/RoleBinding, verified by
//     one real dispatch.
//     ⚠ THE PRIVILEGE TIER (MUSTER_AGENT_PRIVILEGE_APPLY) USED TO NEED A FURTHER
//     SET — write verbs plus `escalate`/`bind` on all four rbac resource types.
//     It was removed with the privilege subsystem (superseded by connectors), so
//     no code path here writes or deletes an rbac object any more and a
//     deployment's Role can drop that set.
//  2. ROLLBACK IS NOT SYMMETRIC.
//     Once MUSTER_AGENT_PROVISIONER has been enabled and instances exist, rolling
//     the image back to a build WITHOUT internal/agentprovision makes every
//     lifecycle route answer 503 again while the rows AND the instances remain: the
//     instances become unmanageable from muster (no stop, no destroy, no logs) and
//     have to be torn down with cluster tooling. Destroy every instance BEFORE
//     rolling back, or accept a manual teardown.
//
// ⚠ EVERY KUBERNETES ASSERTION IN THIS PACKAGE'S TESTS IS AGAINST
// k8s.io/client-go/kubernetes/fake. That is what makes them runnable, and it means
// no test here — or anywhere in this module — has ever driven the driver against a
// real apiserver. Plan step 22d is that test.
// ---------------------------------------------------------------------------

// buildAgentPlane builds both tiers of the agent seam over ONE driver: the
// lifecycle provisioner named by cfg and the chat gateway named by cfg. Either
// may be nil. (A third tier, the privilege applier, was removed with the
// privilege subsystem.)
//
// ⚠ A nil RESULT IS A SUPPORTED DEPLOYMENT AND NOT AN ERROR CASE. api.Extensions
// tolerates a nil Provisioner and a nil Gateway by design — those routes refuse at
// the door with api.ProvisionerUnwiredField — so "no provisioner" and "lifecycle
// without chat" are configurations, not failures. The caller must not treat either
// nil as something to fall back from.
//
// 🔴 IT RETURNS CONCRETE POINTER TYPES, NOT INTERFACES, AND main.go's nil-check
// DEPENDS ON THAT. Assigning a typed nil pointer to an interface field yields a
// non-nil interface holding a nil pointer, which sails past api's wrappers into a
// nil-pointer method call in a goroutine. The concrete return is what makes the
// check at the assignment site possible.
func buildAgentPlane(cfg config, store agents.Store, logger *log.Logger) (*agentprovision.Adapter, *agentgateway.Gateway, error) {
	return buildAgentPlaneWith(cfg, store, nil, nil, logger)
}

// buildClaudePool builds the claude-code kind's account pool, or (nil, nil) when
// the kind is not enabled.
//
// 🔴 IT NEEDS THE DATABASE: selection reads each account's marks and live-agent
// count, and a mark that could not be persisted would be forgotten at the next
// restart — the account would be picked again straight into its rate limit. So a
// claude-code deployment with no marks store is refused here rather than
// degraded to an in-memory pool.
//
// src is the per-owner account source — the credential plane in production, so
// the environment pool is the admin's and a member's accounts are their own.
func buildClaudePool(cfg config, src ccpool.Source, marks ccpool.Store) (*ccpool.Pool, error) {
	if !cfg.claudeCodeEnabled() {
		return nil, nil
	}
	if marks == nil {
		return nil, fmt.Errorf("%s includes %s, whose account pool persists its rate-limit and auth marks "+
			"in Postgres, and %s is unset", envAgentKinds, agents.KindClaudeCode, envDatabase)
	}
	return ccpool.New(src, marks)
}

// buildAgentPlaneWith is buildAgentPlane with the claude-code account pool,
// which the gateway marks typed ccd failures against. A claude-code deployment
// MUST pass one; buildAgentPlane (pool nil) is the pre-kinds wiring every
// gateway-only test uses.
//
// plane decides whose credentials each agent's spec is built with. nil builds
// the environment-only plane (no personal store, user 1 the only admin): every
// other owner then has NO credential and is refused — so the adapter ALWAYS
// resolves per owner, and no wiring can fall back to building a member's pod
// from the environment (TestTheAgentPlaneResolvesCredentialsPerOwner).
func buildAgentPlaneWith(cfg config, store agents.Store, pool *ccpool.Pool, plane *credentials.Plane, logger *log.Logger) (*agentprovision.Adapter, *agentgateway.Gateway, error) {
	named := cfg.agentProvisioner()
	if cfg.claudeCodeEnabled() && named != provisionerNone && pool == nil {
		return nil, nil, fmt.Errorf("%s includes %s but no account pool was built (it needs %s)",
			envAgentKinds, agents.KindClaudeCode, envDatabase)
	}
	if named == provisionerNone {
		// The gateway needs a driver to resolve an address, so there is nothing
		// to build here either. config.validateProvisioner refuses "gateway named,
		// provisioner none" at boot rather than letting it arrive here as a
		// silent nil.
		return nil, nil, nil
	}
	if store == nil {
		// Reachable only with no database: the stores are built inside the
		// `cfg.Database != ""` branch. An adapter with no store would resolve no
		// agent id, so every lifecycle call would fail at its first line — and it
		// would do so from a background goroutine, where the only trace is a log
		// line nobody is reading.
		return nil, nil, fmt.Errorf("%s=%s needs an agents store, and there is none because %s is "+
			"unset: an agent provisioner resolves every request through the database",
			envAgentProvisioner, named, envDatabase)
	}

	driver, err := buildDriver(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	// 🔴 THE GATEWAY IS BUILT *BEFORE* THE LIFECYCLE ADAPTER, AND THE ORDER IS NOW
	// LOAD-BEARING RATHER THAN INCIDENTAL. The adapter has to be told whether this
	// process has anything that could deliver a kickoff, because a dispatch asking
	// for one it cannot deliver is REFUSED instead of creating a pod that will never
	// be told what to do (agentprovision.KickoffRefusalReason). It cannot answer that
	// by looking at itself — it is the lifecycle half by construction and must never
	// hold a gateway — so the answer is READ OFF THE CONSTRUCTED OBJECT here.
	//
	// 🔴 OFF THE OBJECT, NOT OFF THE CONFIG, FOR THE SAME REASON THE BANNER READS
	// gw.Runtime() RATHER THAN cfg.agentGateway(): `gw != nil` is what the api
	// wrappers and api.Extensions.Gateway will branch on, so it is the only claim
	// that cannot disagree with the deployment's actual behaviour. A buildGateway
	// that mapped a named runtime to nil would otherwise leave the adapter believing
	// a kickoff was deliverable and restore the silent-strand behaviour exactly.
	//
	// 🔴 AND `gw != nil` IS NOT THE WHOLE PREDICATE — IT WAS, AND THAT WAS A LIVE
	// DEFECT RATHER THAN AN IMPRECISION. It is the NECESSARY half: a kickoff cannot
	// be delivered without a gateway. It is not the SUFFICIENT half: something has
	// to CALL the gateway when a dispatch asks for a first turn, and nothing in this
	// module does. Measured on the deployment this binary runs on, with
	// MUSTER_AGENT_GATEWAY set, the adapter was told a kickoff was deliverable, its
	// pre-create refusal was skipped, and a Dispatch click created a Deployment, a
	// ServiceAccount, a namespace, a Secret holding a minted token and a cloned
	// repository — then recorded that the first turn never happened, behind a card
	// agents.ComputeStatus refines to `running`. The second conjunct below is the
	// missing half; agentprovision.KickoffDeliveryWired carries the argument and
	// names the ledger that forces it to flip when a call site lands.
	//
	// ⚠ nil IS A SUPPORTED OUTCOME, so this is a capability report and not an error
	// check — see this function's own header on why neither nil is a failure.
	gw, err := buildGatewayWith(cfg, driver, pool)
	if err != nil {
		return nil, nil, err
	}
	specCfg, err := agentSpecConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	if plane == nil {
		if plane, err = buildCredentialPlane(cfg, nil, nil, logger); err != nil {
			return nil, nil, err
		}
	}
	prov, err := agentprovision.New(agentprovision.Config{
		Driver:             driver,
		Store:              store,
		Spec:               specCfg,
		Credentials:        credResolver{pool: pool, plane: plane},
		Logger:             logger,
		KickoffDeliverable: kickoffDeliverable(gw, agentprovision.KickoffDeliveryWired),
	})
	if err != nil {
		return nil, nil, err
	}
	return prov, gw, nil
}

// kickoffDeliverable reports whether this process can deliver an agent's FIRST
// TURN: it needs a gateway to deliver it THROUGH and a call site to deliver it
// FROM, and it is the conjunction of exactly those two.
//
// 🔴 IT IS A FUNCTION, OVER A PARAMETER, FOR ONE REASON: THE TRUTH TABLE IS
// TESTABLE IN ALL FOUR COMBINATIONS AND THE EXPRESSION IT REPLACES WAS NOT. Written
// inline as `gw != nil && agentprovision.KickoffDeliveryWired`, three of the four
// rows are unreachable from any test — the constant is a constant — so the only
// guard possible would have been a test reading this file's source for the words.
// Taking the second conjunct as an argument means a test can assert that BOTH are
// required, today and after the constant flips, without knowing its current value.
// See TestTheDeliverabilityPredicateNeedsBOTHConjuncts.
//
// ⚠ THE CALL SITE PASSES THE REAL CONSTANT AND NOTHING ELSE MAY. A caller that
// passed a literal `true` here would restore, in one word, the defect the second
// conjunct exists to remove — and internal/modulegate's kickoff-delivery ledger
// would not see it, because that ledger measures the CONSTANT against the module's
// call graph and knows nothing about this argument.
func kickoffDeliverable(gw *agentgateway.Gateway, deliveryWired bool) bool {
	return gw != nil && deliveryWired
}

// buildKickoffDeliverer builds the background loop that DELIVERS a dispatched
// agent's first turn, or nil when this process has nothing to deliver it through.
//
// 🔴 IT IS BUILT EXACTLY WHEN THE ADAPTER IS TOLD A KICKOFF IS DELIVERABLE, AND
// THAT EQUALITY IS THE WHOLE CONTRACT. kickoffDeliverable(gw, …) is what lifts
// agentprovision's pre-create refusal; if it were true with no deliverer running,
// a Dispatch click would create a pod, a namespace and a minted secret and nothing
// would ever tell the agent what to do — the defect KickoffDeliveryWired's history
// records. So the deliverer is keyed on the same two values, and
// TestADeliverableAdapterAlwaysComesWithARunningDeliverer holds the two together
// in both configurations.
//
// ⚠ IT TAKES THE ADAPTER AS ITS INSTANCE LISTER rather than the raw driver, so the
// deliverer sees exactly the instances the lifecycle routes see.
//
// onChange is called with an agent's name whenever the deliverer changes what that
// agent's card shows; buildApp passes api.Server.BroadcastAgentChanged so an open
// Agents list re-renders (agentkickoff.Config.OnChange).
func buildKickoffDeliverer(cfg config, store agents.Store, prov *agentprovision.Adapter, gw *agentgateway.Gateway, onChange func(string), logger *log.Logger) (*agentkickoff.Deliverer, error) {
	if prov == nil || !kickoffDeliverable(gw, agentprovision.KickoffDeliveryWired) {
		return nil, nil
	}
	return agentkickoff.New(agentkickoff.Config{
		Store:           store,
		Instances:       prov,
		Gateway:         gw,
		NamespacePrefix: cfg.AgentNamespacePrefix,
		Logger:          logger,
		OnChange:        onChange,
	})
}

// buildStatusPromoter builds the loop that moves a stored `provisioning` status
// to `running` once the agent's instance is ready (internal/agentstatus), or
// (nil, nil) when no provisioner is configured — with no instances there is
// nothing to be ready.
//
// 🔴 IT IS NOT GATED ON A GATEWAY, UNLIKE THE KICKOFF DELIVERER. Whether an
// agent is running is true or false regardless of whether this process can
// deliver it a first turn; gating it there would leave a gateway-less
// deployment's statuses stuck exactly as they were.
//
// It takes the ADAPTER as its instance lister, as the deliverer does, so both see
// exactly the instances the lifecycle routes see.
func buildStatusPromoter(cfg config, store agents.Store, prov *agentprovision.Adapter, onChange func(string), logger *log.Logger) (*agentstatus.Promoter, error) {
	if prov == nil {
		return nil, nil
	}
	return agentstatus.New(agentstatus.Config{
		Store:           store,
		Instances:       prov,
		NamespacePrefix: cfg.AgentNamespacePrefix,
		Logger:          logger,
		OnChange:        onChange,
	})
}

// buildGateway builds the agent chat gateway named by cfg over an
// already-constructed driver, or (nil, nil) when the configuration names none.
//
// 🔴 THE RUNTIME IS NAMED BY CONFIGURATION AND HAS NO DEFAULT, WHICH IS THE WHOLE
// REASON THIS IS NOT FOLDED INTO THE PROVISIONER'S SWITCH. Two of the three facts
// a chat turn needs — the bearer derivation and the model sentinel — belong to the
// agent IMAGE, not to the provisioning backend, and they are independent of it: the
// kubernetes driver can run any image, and the noop driver can record a spec for
// one. A deployment that names a driver is saying where instances live; naming a
// runtime is saying what protocol the thing inside speaks.
func buildGateway(cfg config, driver provision.Provisioner) (*agentgateway.Gateway, error) {
	return buildGatewayWith(cfg, driver, nil)
}

// buildGatewayWith is buildGateway with the claude-code account pool as the
// gateway's failure marker. A nil pool leaves Accounts nil — NOT a typed nil
// inside the interface, which would make the gateway's nil check pass and call
// a method on a nil *Pool.
func buildGatewayWith(cfg config, driver provision.Provisioner, pool *ccpool.Pool) (*agentgateway.Gateway, error) {
	rt, err := agentRuntime(cfg)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, nil
	}
	gc := agentgateway.Config{
		Driver:  driver,
		Runtime: rt,
		Model:   cfg.AgentGatewayModel,
	}
	if pool != nil {
		gc.Accounts = pool
	}
	return agentgateway.New(gc)
}

// agentRuntime resolves MUSTER_AGENT_GATEWAY to the credential scheme it names,
// or (nil, nil) for none.
//
// 🔴 IT IS ONE FUNCTION BECAUSE THERE ARE NOW TWO CONSUMERS AND THEY MUST NOT
// DISAGREE — the same argument oneOf and agents.ResolveNamespacePrefix already
// won in this repository. buildGateway needs the scheme to talk to an instance;
// agentSpecConfig needs the SAME scheme's Bearer to tell the instance what
// credential to accept. Two switches holding the same mapping is how one of them
// comes to hold a different case, and the observable of that drift is the
// precise failure the derivation exists to prevent: muster sending a bearer the
// instance was configured to reject, which is a 401 on every turn reading as a
// bad credential rather than as two mappings.
//
// ⚠ nil IS "none" AND IS NOT AN ERROR, so a caller must branch on it rather than
// treat it as something to fall back from. Returning a no-op Runtime instead was
// ruled out: it would make a deployment with no scheme ship a derived credential
// anyway, which is a claim about the image that config.AgentGateway's doc says
// this project must not make on an operator's behalf.
func agentRuntime(cfg config) (agentgateway.Runtime, error) {
	switch cfg.agentGateway() {
	case gatewayNone:
		return nil, nil
	case gatewayHooksSHA256:
		return agentgateway.HooksSHA256(), nil
	default:
		// Unreachable: config.validateProvisioner refuses anything else at boot. A
		// silent nil here would present as a working server whose chat routes all
		// answered 503 with no reason an operator could find.
		return nil, fmt.Errorf("unhandled %s %q (config.validateProvisioner should have refused "+
			"it at boot; this is a wiring bug, not a configuration one)",
			envAgentGateway, cfg.agentGateway())
	}
}

// e2eFakeTerminal, when non-nil, wraps the noop driver with a scripted
// terminal so the browser suite can drive the terminal page against a real
// server without a cluster. It is set ONLY by e2eterm.go, which carries the
// `e2eterm` build tag: the Dockerfile and every CI build but the e2e harness's
// build without it, so a deployed binary cannot hold one.
// TestTheDefaultBuildCarriesNoFakeTerminal pins the nil.
var e2eFakeTerminal func(provision.Provisioner) provision.Provisioner

// buildDriver builds the provisioning backend itself.
func buildDriver(cfg config, logger *log.Logger) (provision.Provisioner, error) {
	switch cfg.agentProvisioner() {
	case provisionerNoop:
		// The recording driver. It is a real driver with declared capabilities
		// that its own CheckSpec enforces and a Destroy that removes state — see
		// its type doc on why it is not the "declared but inert" fake its
		// predecessor was — so it is a legitimate deployment for developing
		// everything above the provisioner without a cluster.
		//
		// 🔴 IT IS NOT WHAT CLEARS THE LEDGER, and that distinction is the one
		// this whole step was told not to fudge. internal/provision already
		// linked (internal/api imports it); the entry that had to move is
		// internal/provision/k8s, and only the branch below reaches it. A noop
		// wired to satisfy a ledger would be exactly the "fake consumer" the
		// ledger's own entry forbids.
		noop, err := provision.NewNoop()
		if err != nil {
			return nil, err
		}
		if e2eFakeTerminal != nil {
			// Only a binary built with -tags e2eterm gets here; see e2eterm.go.
			logger.Printf("🔴 E2E BUILD (-tags e2eterm): the noop driver carries a FAKE terminal. " +
				"This binary is a browser-test fixture and must never be deployed.")
			return e2eFakeTerminal(noop), nil
		}
		return noop, nil
	case provisionerK8s:
		return buildK8sDriver(cfg, logger)
	default:
		// Unreachable: config.validateProvisioner refuses anything else at boot.
		// A silent nil here would present as a working server whose every
		// dispatch panicked in a goroutine.
		return nil, fmt.Errorf("unhandled %s %q (config.validateProvisioner should have refused "+
			"it at boot; this is a wiring bug, not a configuration one)",
			envAgentProvisioner, cfg.agentProvisioner())
	}
}

// buildK8sDriver builds the Kubernetes driver from the pod's own service
// account.
//
// 🔴 IT IS IN-CLUSTER ONLY, AND SAYING SO IS BETTER THAN A KUBECONFIG FALLBACK.
// A fallback to a local kubeconfig would make this binary provision against
// whatever cluster the developer's current context names — which is the failure
// mode where a test dispatch creates pods in production. In-cluster config fails
// with a message naming what is missing, and that message is the correct outcome
// outside a pod.
func buildK8sDriver(cfg config, logger *log.Logger) (provision.Provisioner, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("%s=%s needs in-cluster credentials and there are none: %w "+
			"(this binary does not fall back to a local kubeconfig, deliberately — a fallback "+
			"would provision into whichever cluster the ambient context names)",
			envAgentProvisioner, provisionerK8s, err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dc := k8sDriverConfig(cfg, logger)
	dc.Client = client
	dc.RESTConfig = rc
	return k8sdriver.New(dc)
}

// k8sDriverConfig maps this binary's configuration onto the driver's, WITHOUT
// touching a cluster.
//
// 🔴 THE SPLIT FROM buildK8sDriver IS WHAT MAKES THE MAPPING TESTABLE, and the
// mapping is where the mistakes are. Two of the fields below invert or default
// their source, and neither error would announce itself: a wrong
// NamespacePerInstance puts instances somewhere the stored row does not name, and
// a nil WorkspaceStorageClass makes the driver refuse persistence at the first
// dispatch rather than at boot. Acquiring the client is the part that needs a
// pod; deciding what to tell the driver is not, so only the first is left
// unreachable from a test.
func k8sDriverConfig(cfg config, logger *log.Logger) k8sdriver.Config {
	dc := k8sdriver.Config{
		// 🔴 THE FLAG IS INVERTED HERE, ON PURPOSE, AND THIS IS THE ONLY PLACE IT
		// HAPPENS. The driver's knob is NamespacePerInstance (default false =
		// shared); the deployment's is MUSTER_AGENT_NAMESPACE_SHARED (default
		// false = per-instance). See config.AgentNamespaceShared for why the
		// deployment default is the opposite of the driver's.
		NamespacePerInstance: !cfg.AgentNamespaceShared,
		NamespacePrefix:      cfg.AgentNamespacePrefix,
		Namespace:            cfg.AgentNamespace,
		EndpointTemplate:     cfg.AgentEndpointTemplate,
		Logger:               logger,
	}
	if cfg.AgentWorkspacePersist || cfg.claudeCodeEnabled() {
		// ⚠ ...OR THE CLAUDE-CODE KIND IS ENABLED: its /data volume is not optional
		// (the conversation `claude --resume <id>` resumes lives on it), so the driver
		// must accept a persistent workspace even when gateway-kind agents keep
		// theirs ephemeral. This raises the CAPABILITY only; a gateway-kind spec
		// still asks for persistence exactly when AgentWorkspacePersist says so.
		//
		// The pointer's PRESENCE is what raises Capabilities.Persistence; its
		// value being empty means "the cluster's default StorageClass". See
		// config.AgentWorkspacePersist on why an env var cannot express the third
		// state on its own.
		class := cfg.AgentStorageClass
		dc.WorkspaceStorageClass = &class
	}
	if cfg.claudeCodeNetworkPolicy() {
		// 🔴 THE POINTER'S PRESENCE RAISES Capabilities.NetworkIsolation, WHICH THE
		// CLAUDE-CODE SPEC NEEDS TO BE ACCEPTED AT ALL. Like the storage class
		// above it raises a CAPABILITY only: a gateway-kind spec declares no
		// isolation, so the driver renders no policy for one and makes no
		// networking call to create, update or scale it.
		//
		// 🔴 IT DOES CHANGE ONE THING FOR EVERY AGENT: with this set, the driver's
		// Destroy reads networkpolicies for every instance, gateway-kind included,
		// so the RBAC rule (k8s.NetworkPolicyRBACPrerequisite) has to exist before
		// this deployment destroys anything. See doc_seams.go entry 5.
		//
		// The parse error is dropped because config.validateKinds has already
		// refused a value this rejects; a config that skipped validate gets an
		// empty selector here, which k8s.New refuses by name rather than rendering.
		labels, _ := parsePodLabels(cfg.AgentNetpolFromLabels)
		dc.NetworkPolicy = &k8sdriver.NetworkPolicyConfig{
			ControllerNamespace: cfg.AgentNetpolFromNS,
			ControllerPodLabels: labels,
		}
	}
	return dc
}

// networkPolicyVerbs is the driver's own list of the verbs it needs on
// networkpolicies, for the banner — which must not carry a second spelling of it.
func networkPolicyVerbs() []string { return k8sdriver.NetworkPolicyRBACPrerequisite }

// agentSpecConfig is the deployment-wide half of every agent spec.
//
// ⚠ THE RESOURCE FIELDS agentspec.Config CARRIES ARE NOT EXPOSED HERE YET, and
// that is a gap rather than a decision: left empty, each one falls through to the
// driver's own default. It is stated because the alternative — a reader assuming
// the whole of agentspec.Config is reachable from the environment — is how a
// documented knob turns out to be unread, which is the finding config.go's const
// block exists to answer.
// 🔴 IT RETURNS AN ERROR NOW, AND THE ERROR IS WHAT MAKES THE RUNTIME-CONFIG
// BUNDLE UNABLE TO SHIP HALF-WIRED. The bundle's three parts are the operator's
// template, the operator's script and MUSTER'S OWN derivation — and the third is
// a property of the named scheme, which only agentRuntime resolves. Taking the
// scheme as a PARAMETER instead was the first draft: it lets a caller pass nil
// beside a configuration that names a runtime, and the result is a bundle
// installed with no credential, which is the 401 this whole seam exists to close.
// Deriving it from the same cfg the rest of this function reads makes that
// mismatch unexpressible.
func agentSpecConfig(cfg config) (agentspec.Config, error) {
	rt, err := agentRuntime(cfg)
	if err != nil {
		return agentspec.Config{}, err
	}
	var rc agentspec.RuntimeConfig
	if rt != nil {
		// 🔴 ALL THREE FROM ONE BRANCH. config.validateProvisioner refuses a named
		// scheme with an incomplete bundle and an incomplete bundle with no scheme,
		// so this branch is the resolved form of a configuration that already
		// passed both — and agentspec.RuntimeConfig.Configured is the third check,
		// which fails toward installing NOTHING rather than something partial.
		rc = agentspec.RuntimeConfig{
			Template:     []byte(cfg.AgentRuntimeConfig),
			Install:      []byte(cfg.AgentRuntimeInstall),
			DeriveBearer: rt.Bearer,
		}
	}
	return agentspec.Config{
		ImageRepo:  cfg.AgentImageRepo,
		ImageTag:   cfg.AgentImageTag,
		APIBaseURL: cfg.AgentAPIURL,
		Model:      cfg.AgentModel,
		// 🔴 THIS IS THE FIELD THAT MAKES A PROVISIONED AGENT ADDRESSABLE, AND IT
		// IS MAPPED EVEN THOUGH ZERO IS THE NORMAL VALUE. Zero is the UNSET
		// sentinel agentspec resolves to its own measured constant, so a reader
		// could mistake this line for dead weight — it is not: dropping it makes
		// MUSTER_AGENT_GATEWAY_PORT a declared, validated, documented variable
		// nothing reads, which is the exact gap this function's own header records
		// for the resource fields. cmd/muster-server's
		// TestAMalformedGatewayPortIsRefusedAtBootRatherThanInsideADispatch
		// asserts the mapping with a value the constant cannot equal.
		GatewayPort:      cfg.AgentGatewayPort,
		OpenRouterAPIKey: cfg.AgentOpenRouterKey,
		WorkspacePersist: cfg.AgentWorkspacePersist,
		CairnURL:         cfg.AgentCairnURL,
		CairnToken:       cfg.AgentCairnToken,
		Plugins:          cfg.pluginsEnabled(),
		RuntimeConfig:    rc,
		ClaudeCode:       claudeCodeSpecConfig(cfg),
		// A claude-code chief runs the gateway runtime beside ccd when its
		// sessions may be started on that runtime (validateSessionRuntimes
		// refuses this without the runtime bundle the sidecar is built from).
		ChiefGatewaySidecar: cfg.claudeCodeEnabled() && slices.Contains(cfg.ChiefSessionRuntimes, agents.SessionRuntimeGateway),
		// Both or neither: config.validateConnectors refuses a URL without the
		// port, so a URL here always comes with the pod port its policy allows.
		ConnectURL:  cfg.AgentConnectURL,
		ConnectPort: cfg.ConnectPort,
	}, nil
}

// claudeCodeSpecConfig is the claude-code kind's deployment half, or nil when the
// kind is not enabled. The token map is COPIED, so nothing downstream can mutate
// the configuration it came from.
func claudeCodeSpecConfig(cfg config) *agentspec.ClaudeCodeConfig {
	if !cfg.claudeCodeEnabled() {
		return nil
	}
	accounts := make(map[string]string, len(cfg.AgentCCTokens))
	for k, v := range cfg.AgentCCTokens {
		accounts[k] = v
	}
	return &agentspec.ClaudeCodeConfig{Image: cfg.AgentCCImage, Accounts: accounts, StorageSize: cfg.AgentCCStorage}
}
