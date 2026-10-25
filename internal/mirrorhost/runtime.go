package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME INTERFACE, HOST HALF (docs/design/hermes.md, "A runtime seam
// instead of Claude-only code"): one implementation per agent CLI this host
// knows — Claude Code, Hermes, and the opencode stub — and every per-runtime
// decision the host makes goes through it, never through a comparison with a
// runtime's name:
//
//   - DETECTION, SESSION RESOLUTION AND STATUS: LocatePanes (every runtime);
//   - the RENDERED VIEW SOURCE: viewSource (Claude Code's transcript tail,
//     Hermes's state.db tail), chosen by the window a watch names;
//   - the SEND CAPABILITY: gatedSender, for a runtime whose send is its own
//     (Hermes's), routed by Msg.Runtime;
//   - the PROMPT CAPABILITY: answerer, for a runtime whose answers are its
//     own (Hermes's approvals, by Msg.Runtime);
//   - the PASTE GUARD: pasteGuard, a runtime whose panes the host's one
//     typing path (typeInto) refuses (Hermes; opencode).
//
// The server's half is mirror.RuntimeSpec; what muster may do with a
// runtime's windows is its Caps there (mirror.LookupRuntime), and tests pin
// that a host view source or gated sender exists exactly where the server's
// table says one does.

// Runtime is one agent CLI this host knows.
type Runtime interface {
	// Name is the runtime's wire name: mirror.RuntimeClaude,
	// mirror.RuntimeHermes or mirror.RuntimeOpenCode.
	Name() string
	// LocatePanes maps each of panes that this runtime CLAIMS — by the
	// process running in it, never by #{pane_current_command} — to the agent
	// session in it, keyed by pane id, reading the proc table procs/children
	// (readProcTable), which the caller reads ONCE for every runtime. The
	// tmux backend asks each runtime in turn, Claude Code first, and the first
	// to claim a pane has it.
	LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane
}

// AgentPane is one tmux pane running an agent CLI, of any runtime: ClaudePane
// under the name the seam uses. Its Runtime field says which; "" is Claude
// Code (every ClaudePane written before the seam). For a Hermes pane, Gen is
// the conversation's logical session id (its lineage root) when the pane has
// a rendered view, "" when it has none (Hermes says why); Transcript and
// Pending are always empty.
type AgentPane = ClaudePane

// viewSource is a Runtime whose windows this host serves a rendered view of.
type viewSource interface {
	// viewer is the runtime's view source on h's connection: claims, read
	// NOW, whether the window a watch names is one of its windows with a
	// view; serve streams it until ctx ends; gone is why a view ends that
	// no runtime claims. ok is false when this host or connection has none.
	viewer(h *hostSide) (claims func(context.Context, mirror.Msg) bool, serve func(context.Context, mirror.Msg) error, gone error, ok bool)
}

// gatedSender is a Runtime whose send is its own (Msg.Runtime names it): the
// one deliberate exception to typeInto, with its own checks.
type gatedSender interface {
	gatedSend(h *hostSide, m mirror.Msg)
}

// answerer is a Runtime whose asks are answered its own way (Msg.Runtime
// names it), never by keys into Claude Code's picker.
type answerer interface {
	answer(h *hostSide, m mirror.Msg)
}

// pasteGuard is a Runtime whose panes the host paste guard refuses
// (hostSide.guardPane): shape is the broad match on one process — which rule
// matched, and true — and refusal what the person reads, of kind
// refusalKind (Msg.Refusal). owns reports whether a rule is one of its.
type pasteGuard interface {
	shape(p procInfo, cwd func() string) (rule string, ok bool)
	owns(rule string) bool
	refusal() error
	refusalKind() string
	// label is how the host's log names the runtime.
	label() string
}

// knownRuntimes is every runtime this host knows, as decisions not tied to a
// listing (routing a message, the paste guard, capabilities) ask them; the
// guard asks them in THIS order, opencode before Hermes. The runtimes that
// claim panes, with their locators, are Tmux.runtimes.
var knownRuntimes = []Runtime{claudeRuntime{}, openCodeRuntime{}, hermesRuntime{}}

// runtimeNamed is the known runtime called name.
func runtimeNamed(name string) (Runtime, bool) {
	for _, r := range knownRuntimes {
		if r.Name() == name {
			return r, true
		}
	}
	return nil, false
}

// claudeRuntime is Claude Code behind the seam: exactly the code that ran
// before it, moved, not changed.
type claudeRuntime struct{ loc ClaudeLocator }

func (claudeRuntime) Name() string { return mirror.RuntimeClaude }

// viewer is Claude Code's transcript tail, for a window whose pane runs
// Claude Code NOW (claudePaneOf, re-resolved by the tail itself).
func (claudeRuntime) viewer(h *hostSide) (func(context.Context, mirror.Msg) bool, func(context.Context, mirror.Msg) error, error, bool) {
	cb, ok := h.claudeBackend()
	if !ok {
		return nil, nil, nil, false
	}
	return func(context.Context, mirror.Msg) bool { return true },
		func(ctx context.Context, m mirror.Msg) error { return h.tail(ctx, h.claudePaneOf(cb, m), m) }, nil, true
}

func (r claudeRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	return r.loc.locatePanesIn(panes, procs, children)
}

// hermesRuntime is Hermes behind the seam: it DETECTS a Hermes CLI in a pane
// (the strict mirroring rule, HermesIdentity.CLI) and, with a HermesSource,
// resolves the CLI's session and status from its terminal's breadcrumb
// (hermeshost.Locator). No send: that is Phase 3.
type hermesRuntime struct {
	id  HermesIdentity
	src *HermesSource
}

func (hermesRuntime) Name() string { return mirror.RuntimeHermes }

// viewer is the Hermes tail (hermes_stream.go), for a Hermes window whose
// host names its session, on a backend with a HermesSource and a v2
// connection.
func (hermesRuntime) viewer(h *hostSide) (func(context.Context, mirror.Msg) bool, func(context.Context, mirror.Msg) error, error, bool) {
	src, ok := h.hermesSource()
	if !ok {
		return nil, nil, nil, false
	}
	claims := func(ctx context.Context, m mirror.Msg) bool { _, is := h.hermesWindow(ctx, m); return is }
	return claims, func(ctx context.Context, m mirror.Msg) error { return h.hermesTail(ctx, src, m) }, errNoHermesViewNow, true
}

// gatedSend is the gated send into a Hermes pane (hermes_send.go).
func (hermesRuntime) gatedSend(h *hostSide, m mirror.Msg) { h.hermesPaste(m) }

// answer answers a Hermes approval through its gateway (hermes_asks.go).
func (hermesRuntime) answer(h *hostSide, m mirror.Msg) { h.answerHermes(m) }

func (hermesRuntime) shape(p procInfo, cwd func() string) (string, bool) { return hermesShape(p, cwd) }

func (hermesRuntime) owns(rule string) bool {
	switch rule {
	case hermesRuleLauncher, hermesRulePython, hermesRuleCheckout, hermesRuleChrome:
		return true
	}
	return false
}

func (hermesRuntime) refusal() error      { return ErrHermesPane }
func (hermesRuntime) refusalKind() string { return mirror.PasteRefusalHermes }
func (hermesRuntime) label() string       { return "Hermes" }

// openCodeRuntime is opencode behind the interface: a STUB. It claims no
// pane (no view, no status: the window is the plain one its command labels),
// muster sends nothing to it, and its one behaviour is the paste guard's
// refusal (openCodeShape). The read-only view is the next phase.
type openCodeRuntime struct{}

func (openCodeRuntime) Name() string { return mirror.RuntimeOpenCode }

// LocatePanes claims nothing: the stub has no locator yet.
func (openCodeRuntime) LocatePanes([]pane, map[int]procInfo, map[int][]int) map[string]AgentPane {
	return map[string]AgentPane{}
}

func (openCodeRuntime) shape(p procInfo, _ func() string) (string, bool) {
	if openCodeShape(p) {
		return hermesRuleOpenCode, true
	}
	return "", false
}

func (openCodeRuntime) owns(rule string) bool { return rule == hermesRuleOpenCode }
func (openCodeRuntime) refusal() error        { return ErrOpenCodePane }
func (openCodeRuntime) refusalKind() string   { return mirror.PasteRefusalOpenCode }
func (openCodeRuntime) label() string         { return "opencode" }

// LocatePanes claims each pane whose process tree holds a Hermes CLI by the
// strict rule on the pane's terminal — #{pane_tty} as the listing read it,
// else (a pane from a lookup that did not read it) the root process's stdin —
// and resolves its session when the host has a HermesSource.
//
// It walks nothing when the table holds no process named "hermes": the strict
// rule requires that comm, and this runs on every listing.
//
// ⚠ ON THE FALLBACK, A ROOT WHOSE STDIN IS NOT THE PANE'S TERMINAL (`exec
// </dev/null`) passes that file as "the pane's terminal", so a Hermes child
// with the same stdin is claimed — and then has no breadcrumb for that file,
// so no rendered view and an unknown status: it only takes things away.
func (r hermesRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	out := map[string]AgentPane{}
	if len(panes) == 0 || !anyComm(procs, "hermes") {
		return out
	}
	for _, p := range panes {
		tty := p.TTY
		if tty == "" {
			var err error
			if tty, err = os.Readlink(filepath.Join(r.id.proc(), strconv.Itoa(p.PID), "fd", "0")); err != nil {
				continue
			}
		}
		pid, ok := r.id.cli(p.PID, tty, procs, children)
		if !ok {
			continue
		}
		ap := AgentPane{Runtime: mirror.RuntimeHermes, Pane: p.ID, PID: pid, Source: SourceProcess}
		if r.src != nil {
			r.src.resolve(&ap, pid, tty, r.id.proc(), procs)
		}
		out[p.ID] = ap
	}
	return out
}

// anyComm reports whether any process in procs is named comm.
func anyComm(procs map[int]procInfo, comm string) bool {
	for _, p := range procs {
		if p.comm == comm {
			return true
		}
	}
	return false
}

// SourceProcess: an AgentPane found by its process alone (the strict Hermes
// rule), with no session resolved yet.
const SourceProcess = "process"

// runtimes is t's runtimes that LOCATE panes, in the order they claim one:
// Claude Code first, so a pane that runs both is the Claude window it always
// was. The opencode stub locates nothing, so it is not asked.
func (t Tmux) runtimes() []Runtime {
	return []Runtime{t.claudeRuntime(), t.hermesRuntime()}
}

func (t Tmux) claudeRuntime() claudeRuntime { return claudeRuntime{loc: t.Claude} }

func (t Tmux) hermesRuntime() hermesRuntime {
	return hermesRuntime{id: HermesIdentity{Proc: t.Claude.Proc}, src: t.Hermes}
}

// attachAgents sets each window's Agent: the active pane's agent session of a
// runtime other than Claude Code, for a window whose pane Claude Code did not
// claim (attachClaude ran first). Each runtime is asked in turn for the panes
// still unclaimed, over the one proc table read; the first claim wins.
func attachAgents(windows []WindowInfo, procs map[int]procInfo, children map[int][]int, others []Runtime) {
	for _, rt := range others {
		var free []pane
		for _, w := range windows {
			if w.Claude == nil && w.Agent == nil {
				free = append(free, pane{Session: w.Session, ID: w.Pane, PID: w.PanePID, TTY: w.TTY})
			}
		}
		if len(free) == 0 {
			return
		}
		claimed := rt.LocatePanes(free, procs, children)
		for i := range windows {
			if windows[i].Claude != nil || windows[i].Agent != nil {
				continue
			}
			if ap, ok := claimed[windows[i].Pane]; ok {
				windows[i].Agent = &ap
			}
		}
	}
}
