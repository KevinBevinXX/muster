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
// TODAY: which panes run the CLI. The send (CheckSend, Submit), the rendered
// view's stream and an answer's keys stay Claude Code's own code, and join
// this interface in the phases that give Hermes an implementation of them
// (Phase 3, Phase 2 and Phase 4); a method whose second implementation could
// only refuse would be a seam drawn on a guess.

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
// the Hermes session id once the locator can read it (Phase 2), "" until then,
// and Transcript and Pending are always empty: it has no rendered view yet.
type AgentPane = ClaudePane

// claudeRuntime is Claude Code behind the seam: exactly the code that ran
// before it, moved, not changed.
type claudeRuntime struct{ loc ClaudeLocator }

func (claudeRuntime) Name() string { return mirror.RuntimeClaude }

func (r claudeRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	return r.loc.locatePanesIn(panes, procs, children)
}

// hermesRuntime is Hermes behind the seam. In this release it DETECTS a Hermes
// CLI in a pane (the strict mirroring rule, HermesIdentity.CLI) and nothing
// more: no session id, no status (unknown), no rendered view, no send.
type hermesRuntime struct{ id HermesIdentity }

func (hermesRuntime) Name() string { return mirror.RuntimeHermes }

// LocatePanes claims each pane whose process tree holds a Hermes CLI by the
// strict rule, reading the pane's terminal from its root process's stdin.
//
// It walks nothing when the table holds no process named "hermes": the strict
// rule requires that comm, and this runs on every listing.
//
// ⚠ THE PANE'S TERMINAL IS ITS ROOT PROCESS'S fd 0, NOT #{pane_tty}: the one
// window listing (windowsFormat) does not read the tty, and this release only
// needs to tell a Hermes window from another. A root whose stdin is NOT the
// pane's terminal (`exec </dev/null`) passes that file as "the pane's
// terminal", so a Hermes child with the SAME stdin is claimed — a window
// listed as Hermes that is not quite one, which only takes things away from it
// (no rendered view, every paste refused). A root whose stdin cannot be read
// has no Hermes window. Phase 2's breadcrumb locator, which names the
// breadcrumb FILE by #{pane_tty}, reads it from tmux.
func (r hermesRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	out := map[string]AgentPane{}
	if len(panes) == 0 || !anyComm(procs, "hermes") {
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
	return hermesRuntime{id: HermesIdentity{Proc: t.Claude.Proc}}
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
				free = append(free, pane{Session: w.Session, ID: w.Pane, PID: w.PanePID})
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
