package mirrorhost

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME SEAM (docs/design/hermes.md, "A runtime seam instead of
// Claude-only code"): one implementation per agent CLI this host mirrors.
//
// 🔴 SIZED FOR EXACTLY TWO, Claude Code and Hermes, and for what both have
// TODAY: which panes run the CLI. The send (CheckSend, Submit) and an
// answer's keys stay Claude Code's own code, and join this interface in the
// phases that give Hermes an implementation of them (Phase 3 and Phase 4); a
// method whose second implementation could only refuse would be a seam drawn
// on a guess. The rendered view's stream has two implementations since
// Phase 2 (transcript.go's tail, hermes_stream.go's hermesTail), chosen by the
// window a watch names; it joins the interface with the send in Phase 3.

// Runtime is one agent CLI muster mirrors.
type Runtime interface {
	// Name is the runtime's wire name: mirror.RuntimeClaude or
	// mirror.RuntimeHermes.
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

// claudeRuntime is Claude Code behind the seam: exactly the code that ran
// before it, moved, not changed.
type claudeRuntime struct{ loc ClaudeLocator }

func (claudeRuntime) Name() string { return mirror.RuntimeClaude }

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

// runtimes is t's runtimes, in the order they claim a pane: Claude Code first,
// so a pane that runs both is the Claude window it always was.
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
