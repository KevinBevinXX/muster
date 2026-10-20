package mirrorhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME SEAM (docs/design/hermes.md, "A runtime seam instead of
// Claude-only code"): one implementation per agent CLI this host mirrors.
//
// 🔴 SIZED FOR EXACTLY TWO, Claude Code and Hermes, and for what both have
// TODAY: which panes run the CLI, the at-the-moment-of-acting send gate, and
// the submit. The rendered view's stream and an answer's keys stay Claude
// Code's own code until Hermes has an implementation of them (Phase 2 and
// Phase 4); a method with one real implementation and one stub that can only
// refuse would be a seam drawn on a guess.

// Runtime is one agent CLI muster mirrors.
type Runtime interface {
	// Name is the runtime's wire name: mirror.RuntimeClaude or
	// mirror.RuntimeHermes.
	Name() string
	// LocatePanes maps each of panes that this runtime CLAIMS — by the
	// process running in it, never by #{pane_current_command} — to the agent
	// session in it, keyed by pane id. The tmux backend asks each runtime in
	// turn, Claude Code first, and the first to claim a pane has it.
	LocatePanes(panes []pane) map[string]AgentPane
	// CheckSend is the send gate at the moment of acting, on the host: nil to
	// type into p now, else why not. Claude Code's admits (it queues typed
	// input itself), which is how the seam leaves its send unchanged.
	CheckSend(ctx context.Context, p AgentPane) error
	// Submit types text into p's pane and presses Enter — through CheckSend.
	// It is reached only through hostSide.typeInto (the Hermes paste guard),
	// via the backend's Paste.
	Submit(ctx context.Context, pb PaneBackend, p AgentPane, text string) error
}

// AgentPane is one tmux pane running an agent CLI, of any runtime: ClaudePane
// under the name the seam uses. Its Runtime field says which; "" is Claude
// Code (every ClaudePane written before the seam). For a Hermes pane, Gen is
// the Hermes session id once the locator can read it (Phase 2), "" until then,
// and Transcript and Pending are always empty: it has no rendered view yet.
type AgentPane = ClaudePane

// claudeRuntime is Claude Code behind the seam: exactly the code that ran
// before it, moved, not changed.
type claudeRuntime struct{ loc ClaudeLocator }

func (claudeRuntime) Name() string { return mirror.RuntimeClaude }

func (r claudeRuntime) LocatePanes(panes []pane) map[string]AgentPane {
	return r.loc.LocatePanes(panes)
}

// CheckSend admits: Claude Code takes typed input while busy and queues it,
// and the server's own gates (idle, waiting) are where its send is decided.
func (claudeRuntime) CheckSend(context.Context, AgentPane) error { return nil }

func (r claudeRuntime) Submit(ctx context.Context, pb PaneBackend, p AgentPane, text string) error {
	if err := r.CheckSend(ctx, p); err != nil {
		return err
	}
	return pb.PastePane(ctx, p.Pane, text)
}

// The Hermes runtime's own refusals of a submit.
var (
	// ErrHermesSendOff: this host was not started with -hermes-send, so its
	// Hermes runtime refuses every submit whatever muster sent — the host
	// half of the hermesSend switch.
	ErrHermesSendOff = errors.New("this host does not send to Hermes (muster-mirror was not started with -hermes-send)")
	// ErrHermesSendUnavailable: the gated send into Hermes (the status, screen
	// and database checks of docs/design/hermes.md's "The send path") is not
	// in this release, so nothing is ever typed into a Hermes pane, flag or
	// no flag.
	ErrHermesSendUnavailable = errors.New("this muster-mirror cannot send to Hermes yet: the gated send comes in a later release")
)

// hermesRuntime is Hermes behind the seam. In this release it DETECTS a Hermes
// CLI in a pane (the strict mirroring rule, HermesIdentity.CLI) and nothing
// more: no session id, no status (unknown), no rendered view, and a submit it
// always refuses.
type hermesRuntime struct {
	id HermesIdentity
	// send is the host's -hermes-send flag.
	send bool
}

func (hermesRuntime) Name() string { return mirror.RuntimeHermes }

// LocatePanes claims each pane whose process tree holds a Hermes CLI by the
// strict rule, reading the pane's terminal from its root process's stdin.
//
// ⚠ THE PANE'S TERMINAL IS ITS ROOT PROCESS'S fd 0, NOT #{pane_tty}: the one
// window listing (windowsFormat) does not read the tty, and this release only
// needs to tell a Hermes window from another. A root whose stdin is not the
// pane's terminal (a shell started `exec </dev/null`) simply has no Hermes
// window; Phase 2's breadcrumb locator, which names the breadcrumb FILE by
// #{pane_tty}, reads it from tmux.
func (r hermesRuntime) LocatePanes(panes []pane) map[string]AgentPane {
	out := map[string]AgentPane{}
	if len(panes) == 0 {
		return out
	}
	procs, children, err := readProcTable(r.id.proc())
	if err != nil {
		return out
	}
	for _, p := range panes {
		tty, err := os.Readlink(filepath.Join(r.id.proc(), strconv.Itoa(p.PID), "fd", "0"))
		if err != nil {
			continue
		}
		if pid, ok := r.id.cli(p.PID, tty, procs, children); ok {
			out[p.ID] = AgentPane{Runtime: mirror.RuntimeHermes, Pane: p.ID, PID: pid, Source: SourceProcess}
		}
	}
	return out
}

// CheckSend refuses: the gated send is Phase 3's.
func (hermesRuntime) CheckSend(context.Context, AgentPane) error { return ErrHermesSendUnavailable }

// Submit refuses unless this host runs with -hermes-send, and then refuses
// at CheckSend. 🔴 It never types: there is no Hermes send path in this
// release, and the flag cannot open one.
func (r hermesRuntime) Submit(ctx context.Context, _ PaneBackend, p AgentPane, _ string) error {
	if !r.send {
		return ErrHermesSendOff
	}
	return r.CheckSend(ctx, p)
}

// SourceProcess: an AgentPane found by its process alone (the strict Hermes
// rule), with no session resolved yet.
const SourceProcess = "process"

// runtimes is t's runtimes, in the order they claim a pane: Claude Code first,
// so a pane that runs both is the Claude window it always was.
func (t Tmux) runtimes() []Runtime {
	return []Runtime{t.claudeRuntime(), t.hermesRuntime()}
}

func (t Tmux) claudeRuntime() claudeRuntime { return claudeRuntime{loc: t.Claude} }

func (t Tmux) hermesRuntime() hermesRuntime {
	return hermesRuntime{id: HermesIdentity{Proc: t.Claude.Proc}, send: t.HermesSend}
}

// runtimeOf is the runtime p belongs to.
func (t Tmux) runtimeOf(p AgentPane) Runtime {
	if p.Runtime == mirror.RuntimeHermes {
		return t.hermesRuntime()
	}
	return t.claudeRuntime()
}

// attachAgents sets each window's Agent: the active pane's agent session of a
// runtime other than Claude Code, for a window whose pane Claude Code did not
// claim (attachClaude ran first). Each runtime is asked in turn for the panes
// still unclaimed; the first claim wins.
func attachAgents(windows []WindowInfo, others []Runtime) {
	for _, rt := range others {
		var free []pane
		for _, w := range windows {
			if w.Claude == nil && w.Agent == nil {
				free = append(free, pane{Session: w.Session, ID: w.Pane, PID: w.PanePID})
			}
		}
		if len(free) == 0 {
			return
		}
		claimed := rt.LocatePanes(free)
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

// HermesSendBackend is a backend that says whether it runs with -hermes-send:
// what a hello's Msg.HermesSend advertises. Tmux is one.
type HermesSendBackend interface {
	HermesSendEnabled() bool
}

// HermesSendEnabled implements HermesSendBackend.
func (t Tmux) HermesSendEnabled() bool { return t.HermesSend }

// hermesSend is what this host advertises as Msg.HermesSend: only an
// -interactive v2 host whose backend runs with -hermes-send.
func (h *hostSide) hermesSend() bool {
	if h.version < 2 || !h.c.cfg.Interactive {
		return false
	}
	b, ok := capabilityOf[HermesSendBackend](h.c.cfg.Backend)
	return ok && b.HermesSendEnabled()
}
