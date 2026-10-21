// Package agentgateway is the CHAT half of the agent seam: it turns an agent row
// plus a live provisioner into one turn against that agent's model gateway.
//
// 🔴 IT IS A SEPARATE PACKAGE FROM internal/agentprovision, AND THE SPLIT IS THE
// SAME ONE internal/api MADE BETWEEN Provisioner AND Gateway. A driver contract
// (provision.Provisioner) has no notion of a chat turn at all: it creates, scales,
// destroys and reports. Chat is a conversation with the thing the driver created,
// over a protocol the driver knows nothing about. Behind one type the two would
// have to be wired together or not at all — and "lifecycle works, chat honestly
// refuses" is a real deployment state that api.Gateway's own doc names as such.
//
// WHAT THIS PACKAGE IS, PRECISELY: a bridge with no protocol of its own. The two
// wire formats live in internal/agents (responses.go, chatcompletions.go); the
// credential derivation lives in [Runtime]; the sentinel is configuration
// ([Config.Model]); the address comes from the driver. This package supplies the
// two inputs internal/agents deliberately would not invent — the model sentinel
// and the endpoint — and nothing else.
//
// ⚠ WHAT IT DOES NOT DO, STATED SO IT IS NOT INFERRED: it does not fall back from
// tools to plain chat. api.Gateway's contract puts that decision at the CALLER,
// because the caller is the only place that knows whether a toolless answer is
// acceptable for the turn it is running — a kickoff that silently lost its tools
// produces an agent that cannot read the task it was dispatched for.
//
// ⚠ IT DOES FALL BACK BETWEEN THE TWO *TRANSPORTS*, AND THE TWO ARE DIFFERENT
// DECISIONS. [Gateway.Chat] runs over /v1/responses and drops to
// /v1/chat/completions only when the runtime answers 404 there — a choice about
// which wire format an image speaks, which no caller has information about and
// which changes nothing the caller asked for. Losing TOOLS changes what the agent
// can do; changing transport does not. The reasoning-model measurement that forced
// this is on Chat.
package agentgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultTurnTimeout bounds ONE REQUEST to the agent runtime — it is the default
// http.Client.Timeout, nothing more.
//
// 🔴 IT DOES *NOT* BOUND A TURN, AND THIS COMMENT SAID IT DID. The retracted
// sentence read "bounds one chat turn, including every tool round inside a
// tool-enabled one … stops a wedged turn holding a request goroutine for ever, and
// this is that". Measured false by audit: a tool-enabled turn issues up to
// agents.MaxToolLoopIterations requests, EACH getting its own budget — so the
// reachable ceiling is that many multiples of this value, plus whatever an in-process
// tool dispatch blocks for, which is under no HTTP timeout at all because
// agents.ToolDispatch takes no context.
//
// ⚠ AN EARLIER RETRACTION OF THIS PARAGRAPH ADDED ITS OWN FALSE CLAUSE — "and the loop
// only checks ctx between them" — AND THEN A FIX ROUND CLAIMED TO HAVE REMOVED IT
// WITHOUT WRITING THE FILE. Both transports build with http.NewRequestWithContext
// (responses.go, chatcompletions.go), so a caller deadline aborts an IN-FLIGHT request
// too; that is exactly why the next paragraph can say the caller's context is what
// bounds a turn. Naming the loop instead of the tool dispatch sends a reader adding a
// ceiling to the wrong layer — and a record asserting a retraction that did not happen
// is worse than the clause, because it stops the next reader checking.
//
// ⚠ SO THE THING THAT ACTUALLY BOUNDS A TURN IS THE CALLER'S CONTEXT, and this
// package deliberately does not invent one: a request-scoped ctx already carries the
// caller's own deadline, and replacing it here would silently shorten a turn the
// caller was willing to wait for. If a turn-level ceiling is wanted it belongs at the
// call site, where the deadline has an owner.
const DefaultTurnTimeout = 10 * time.Minute

// EndpointResolver is the one thing this package needs from a provisioner: where
// an instance is reachable.
//
// 🔴 IT IS A NARROW INTERFACE RATHER THAN provision.Provisioner, AND THAT IS A
// CLAIM ABOUT BLAST RADIUS, NOT A STYLE CHOICE. A gateway holding the full driver
// contract could Create, Scale and Destroy — a chat path with the authority to
// delete the instance it is talking to. It is satisfied by provision.Provisioner
// as written, so the wiring passes the same driver the lifecycle adapter holds and
// the two cannot disagree about where an agent lives.
type EndpointResolver interface {
	Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error)
}

// Config is everything the gateway needs. Every field without a stated default is
// required, and New says which one is missing rather than producing a gateway that
// fails at the first turn.
type Config struct {
	// Driver resolves an agent's reachable address. REQUIRED.
	Driver EndpointResolver
	// Runtime is the credential scheme the attached agent image uses. REQUIRED —
	// there is no default, because guessing one means deriving a credential by the
	// wrong formula and reporting 401s.
	Runtime Runtime
	// Model is the runtime's passthrough sentinel: REQUIRED on the wire by both
	// endpoints, and REQUIRED here for the same reason.
	//
	// 🔴 IT IS NOT THE MODEL AN AGENT RUNS, AND IT HAS NO DEFENSIBLE DEFAULT. The
	// runtime resolves the actual model from its own configuration; this field only
	// satisfies the wire's required `model` field, and the value is whatever the
	// attached image recognises. Substituting something more descriptive — a real
	// provider/model slug, which is the obvious guess — produces a request the
	// runtime rejects. It is configuration rather than a constant because the value
	// belongs to the image, not to this project.
	Model string
	// Client is the HTTP client for gateway calls. Optional; a client with
	// [DefaultTurnTimeout] is built when nil.
	//
	// ⚠ A KIND WITH ITS OWN TURN BUDGET (agents.KindTurnTimeout — today
	// claude-code and hermes) gets a COPY of this client with that Timeout; the transport is
	// shared. The gateway kind uses this client exactly as given.
	Client *http.Client
	// Accounts records a claude-code agent's typed `rate_limited` / `auth_failed`
	// failures against the Claude account it runs on. Optional: nil means no
	// claude-code kind is enabled, and nothing is marked.
	Accounts AccountMarker
}

// AccountMarker is the one thing this package needs from the Claude account pool
// (internal/ccpool.Pool satisfies it). It is narrow for the reason
// EndpointResolver is: a chat path has no business selecting accounts or reading
// tokens.
type AccountMarker interface {
	//
	// owner is the agent's owner: an account is (owner, name) since members
	// bring their own (internal/ccpool's Store doc).
	MarkFailure(ctx context.Context, owner int64, account, failure, detail string) error
}

// ⚠ THERE IS DELIBERATELY NO Logger FIELD, AND THERE WAS ONE. It was accepted,
// defaulted to io.Discard and never read — a documented knob that did nothing, which
// is the exact pattern cmd/muster-server/config.go's const block exists to answer,
// shipped in the change that cited it. It is DELETED rather than wired to a log line,
// because every failure on this path is RETURNED: the caller renders it to the user
// and the HTTP layer records it. A log line would duplicate what the error already
// carries. If a future diagnostic genuinely has no caller to return to, add the field
// back WITH its first reader in the same change.

// Gateway implements the consumer-side chat interface over a driver and a runtime.
//
// ⚠ IT DOES NOT NAME THAT INTERFACE IN A COMPILE-TIME ASSERTION, for the reason
// agentprovision.Adapter's doc gives: internal/api declares it consumer-side so
// this direction of the dependency does not exist. The binding is checked where it
// is used — cmd/muster-server assigns a *Gateway to api.Extensions.Gateway — and
// internal/api's own seam test pins it from the side that owns the interface,
// including what a *Gateway must NOT satisfy.
type Gateway struct {
	driver   EndpointResolver
	runtime  Runtime
	model    string
	client   *http.Client
	accounts AccountMarker
}

// New validates the configuration and builds the gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Driver == nil {
		return nil, errors.New("agentgateway: Config.Driver is required (a gateway with no way to " +
			"resolve an instance's address cannot reach a single agent, and it would fail inside a " +
			"request handler rather than at boot)")
	}
	if cfg.Runtime == nil {
		return nil, errors.New("agentgateway: Config.Runtime is required (the bearer derivation is " +
			"the attached runtime's and there is no neutral default: a guessed derivation answers " +
			"401 on every turn, which reads as a bad credential)")
	}
	if cfg.Model == "" {
		return nil, errors.New("agentgateway: Config.Model is required (the runtime's passthrough " +
			"sentinel is REQUIRED on the wire by both endpoints, and an empty one is a 400 from " +
			"inside a turn rather than at boot)")
	}
	g := &Gateway{
		driver:   cfg.Driver,
		runtime:  cfg.Runtime,
		model:    cfg.Model,
		client:   cfg.Client,
		accounts: cfg.Accounts,
	}
	if g.client == nil {
		g.client = &http.Client{Timeout: DefaultTurnTimeout}
	}
	return g, nil
}

// Runtime returns the attached runtime's name, for the boot banner and for error
// messages that have to say which wire contract applies.
func (g *Gateway) Runtime() string { return g.runtime.Name() }

// Chat sends one user message to the agent's gateway under sessionKey, streaming
// assistant text deltas to emit (nil-safe), and returns the full reply. It carries
// NO tools — see the package doc on why the TOOLS fallback decision is the
// caller's. The TRANSPORT choice below is not that decision and is made here.
//
// 🔴 IT RUNS OVER /v1/responses AND ONLY FALLS BACK TO /v1/chat/completions ON A
// 404, BECAUSE THE STREAMING CHAT-COMPLETIONS PATH SILENTLY LOSES A REASONING
// MODEL'S ENTIRE ANSWER. The measurement is recorded on agents.RunToollessTurn: the
// same prompt to the same live agent produced zero content deltas over streaming
// chat-completions — an HTTP 200 and `{"reply":""}` out of
// POST /api/agents/{name}/messages, which is `chief ask`'s door — and the full text
// over /v1/responses. Nothing in this package could have detected it: a 200 with no
// deltas is indistinguishable here from a model that genuinely said nothing.
//
// 🔴 THE 404 FALLBACK IS THE *ONLY* ONE, AND THAT IS DELIBERATE RATHER THAN
// INCOMPLETE. ErrResponsesUnsupported means the endpoint is absent (responses.go
// maps a 404 and nothing else), so chat-completions is then the only transport that
// runtime has. Every OTHER failure propagates: falling back on a 400 or a 500 would
// route the turn straight back into the transport this change exists to leave, and
// the observable would be the empty reply returning under a different cause.
//
// ⚠ WHAT THE FALLBACK COSTS, SINCE THE CALLER CANNOT SEE IT: on a runtime that
// lacks /v1/responses, one turn is two requests — the 404 probe and the real call.
// That is paid only by images old enough to lack the endpoint, and it buys a caller
// that does not have to know which transport an agent's image speaks.
//
// ⚠ IT SENDS NO `instructions`, AND THAT IS PARITY RATHER THAN AN OVERSIGHT. The
// transport this replaces posted one user message and no system prompt at all, so
// supplying one here would change what the agent is told on a path whose only
// defect was the wire format. Chat's signature has nowhere to carry a prompt; a
// caller that needs one has ChatWithTools, which takes it.
//
// It is exactly [Gateway.Resolve] followed by [Gateway.Send]; the split exists for a
// caller that must know whether a failure happened BEFORE anything was sent.
func (g *Gateway) Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return "", err
	}
	return g.Send(ctx, t, sessionKey, message, emit)
}

// Target is an agent whose address and credential are resolved: everything a
// [Gateway.Chat] turn needs before it opens a connection. Obtain one with
// [Gateway.Resolve]. Its fields are unexported, so a caller outside this package
// cannot fill one in; only a zero value can be built there, and that has no address.
type Target struct {
	ep     provision.Endpoint
	bearer string
	// kind and account are the agent's, so Send can apply the kind's turn budget
	// and mark the account a typed failure came from.
	kind    string
	account string
	owner   int64
}

// Resolve does the part of [Gateway.Chat] that contacts no agent runtime: it
// refuses a token-less row and asks the driver where the agent is (for the k8s
// driver, a Deployment read from the Kubernetes API).
//
// 🔴 IT IS SEPARATE SO A CALLER THAT PAYS FOR A TURN CAN RUN IT BEFORE ITS POINT OF
// NO RETURN. internal/agentkickoff marks a first turn delivered before sending it, so
// it is never paid twice. A transient Kubernetes API error here costs nothing and
// sends nothing; inside Chat it used to land after that mark and was recorded as a
// failed, unretried kickoff.
func (g *Gateway) Resolve(ctx context.Context, a agents.Agent) (Target, error) {
	ep, bearer, err := g.reach(ctx, a)
	if err != nil {
		return Target{}, unresolved{err}
	}
	return Target{ep: ep, bearer: bearer, kind: agents.ResolveKind(a.Kind), account: a.CCAccount, owner: a.OwnerID}, nil
}

// Send runs the turn half of [Gateway.Chat] against a [Target] from
// [Gateway.Resolve]: /v1/responses, falling back to /v1/chat/completions on a 404
// (see Chat).
func (g *Gateway) Send(ctx context.Context, t Target, sessionKey, message string, emit func(string)) (string, error) {
	client := g.clientFor(t.kind)
	reply, err := agents.RunToollessTurn(ctx, client, agents.ResponsesURL(t.ep), t.bearer, sessionKey,
		g.model, "", message, textDeltasOnly(emit))
	if !errors.Is(err, agents.ErrResponsesUnsupported) {
		return reply, g.noteFailure(ctx, t, err)
	}
	return agents.ChatStream(ctx, client, agents.ChatCompletionsURL(t.ep), t.bearer, sessionKey,
		g.model, []agents.ChatMessageIn{{Role: "user", Content: message}}, emit)
}

// clientFor returns the HTTP client a kind's turns use: the configured client,
// or — for a kind with its own turn budget — a copy with that Timeout. See
// agents.ClaudeCodeTurnTimeout for why claude-code's budget outlasts ccd's own.
func (g *Gateway) clientFor(kind string) *http.Client {
	t := agents.KindTurnTimeout(kind)
	if t <= 0 {
		return g.client
	}
	c := *g.client
	c.Timeout = t
	return &c
}

// noteFailure marks a claude-code agent's Claude account when ccd answered a
// typed `rate_limited` or `auth_failed`, and returns err (annotated only if the
// mark itself could not be written).
//
// 🔴 ONLY THE TYPED FAILURE MARKS, NEVER A STATUS CODE. A 429 from a proxy, or a
// 502 from anything that is not ccd, carries no `error.type` and marks nothing:
// an account must not be pushed to the back of the pool by a fault that was not
// its own.
//
// ⚠ THE MARK IS WRITTEN ON A CONTEXT DETACHED FROM THE TURN'S, so a caller that
// hangs up the moment the error arrives does not also cancel the record of why.
func (g *Gateway) noteFailure(ctx context.Context, t Target, err error) error {
	// ⚠ t.account IS THE ONLY KIND TEST, AND IT IS SUFFICIENT: migration 0003's
	// agents_cc_account_matches_kind makes "has an account" and "is claude-code"
	// the same row property. A second `t.kind` conjunct was unreachable — the
	// mutation sweep showed it could be deleted with every test green — so it
	// is not here pretending to guard something.
	if err == nil || g.accounts == nil || t.account == "" {
		return err
	}
	// A turn routed to the GATEWAY runtime in a claude-code pod (the chief's
	// sidecar, agents.TurnRoute) did not run on the Claude account at all: its
	// failure is an OpenRouter one, and must not push that account to the back
	// of the pool.
	if r, ok := agents.TurnRouteFrom(ctx); ok && r.Runtime == agents.SessionRuntimeGateway {
		return err
	}
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) {
		return err
	}
	if rt.Type != "rate_limited" && rt.Type != "auth_failed" {
		return err
	}
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if merr := g.accounts.MarkFailure(mctx, t.owner, t.account, rt.Type, rt.Message); merr != nil {
		return fmt.Errorf("%w (recording %s against Claude account %q also failed: %v)", err, rt.Type, t.account, merr)
	}
	return err
}

// markTimeout bounds the account-mark write.
const markTimeout = 5 * time.Second

// textDeltasOnly adapts Chat's text-delta callback to the responses transport's
// event stream, and returns nil for a nil callback so the transport's own nil-safety
// still applies rather than being replaced by a wrapper that calls into nothing.
//
// 🔴 THE KIND FILTER IS LOAD-BEARING, NOT A TIDY-UP. The responses stream carries
// "thinking" events as well as "text" ones, and a reasoning model emits far more of
// the former. Forwarding both would render a model's private reasoning to the user
// as if it were the answer — and it would not even match the returned reply, which
// is assembled from the completed response's message items only.
func textDeltasOnly(emit func(string)) agents.StreamEmit {
	if emit == nil {
		return nil
	}
	return func(ev agents.StreamEvent) {
		if ev.Kind == "text" {
			emit(ev.Text)
		}
	}
}

// ChatWithTools runs one tool-enabled turn through the runtime's /v1/responses
// endpoint: the model gets the native function tools and dispatch executes each
// call in this process. Returns agents.ErrResponsesUnsupported unwrapped when the
// runtime lacks the endpoint, so a caller's errors.Is fallback works.
func (g *Gateway) ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef, dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return "", err
	}
	reply, err := agents.RunToolLoop(ctx, g.clientFor(t.kind), agents.ResponsesURL(t.ep), t.bearer, sessionKey,
		g.model, instructions, message, tools, dispatch, emit)
	return reply, g.noteFailure(ctx, t, err)
}

// reach resolves where an agent is and what credential talks to it — the two
// per-agent inputs both turn shapes need, derived once so they cannot diverge.
//
// 🔴 AN EMPTY HOOKS TOKEN IS REFUSED HERE, AND THIS IS THE GUARD MOST WORTH
// KEEPING. sha256("gw-" + "") is a perfectly well-formed 64-hex credential, so a
// row whose token was never minted does not fail to produce a bearer — it produces
// a WRONG one, and every turn comes back 401 from the runtime. That reads as a
// rotated or mismatched secret and sends the reader to the deployment's secrets,
// while the real fault is a row the provisioner never finished. The refusal names
// the agent and the field so the reader lands in the right place.
func (g *Gateway) reach(ctx context.Context, a agents.Agent) (provision.Endpoint, string, error) {
	if a.HooksToken == "" {
		return provision.Endpoint{}, "", fmt.Errorf("agent %q (id %d) has no hooks token, so no "+
			"gateway credential can be derived: the token is minted when the instance is created, "+
			"and an empty one would derive a well-formed bearer the runtime rejects with 401",
			a.Name, a.ID)
	}
	ep, err := g.driver.Endpoint(ctx, agents.RefOf(a))
	if err != nil {
		return provision.Endpoint{}, "", fmt.Errorf("resolve gateway endpoint for agent %q (id %d): %w",
			a.Name, a.ID, err)
	}
	return ep, g.runtime.Bearer(a.HooksToken), nil
}
