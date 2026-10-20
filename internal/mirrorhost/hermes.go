package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Hermes process identity, and the host paste guard built on it
// (docs/design/hermes.md, "Every paste into a Hermes pane is gated on the
// host", and S10).
//
// 🔴 THIS IS THE GUARD'S BROAD MATCH, NOT THE MIRRORING RULE, AND IT FAILS
// CLOSED. It answers "is anything in this pane's process tree Hermes-shaped",
// so a paste is refused on any doubt. A false refusal is safe; a false admit
// is not: paste+Enter on Hermes's approval modal APPROVES the command, whatever
// the pasted text says (S11). The stricter rule that decides which pane a
// rendered view follows (argv[2] == chat, no gateway, fd 0 on the pane's tty)
// is Phase 2's, and is built on hermesShape, never instead of it.
//
// Known gaps, as the design names them: a Hermes run over ssh or `docker exec`
// inside the pane (invisible to a local process walk); a Hermes launched under
// a renamed interpreter or from a copied tree the argv patterns do not match;
// and a host with no /proc (macOS), where this guard sees no processes at all.

// ErrHermesPane is the refusal of a paste into a pane whose process tree holds
// a Hermes-shaped process. Its text is written for the person at the Terminal
// view: the server passes a host's refusal to the page as is.
var ErrHermesPane = errors.New("Not sent: Hermes runs in this pane. muster never pastes into Hermes from the " +
	"Terminal view, because Enter there can answer a prompt Hermes is showing, such as a command approval. " +
	"Type it in the terminal; sending from a Hermes rendered view comes in a later release")

// errHermesUnchecked is the refusal when the guard could not look. Failing
// closed: a pane whose processes cannot be read is not presumed Hermes-free.
var errHermesUnchecked = errors.New("Not sent: muster-mirror could not check whether Hermes runs in this pane. Type it in the terminal")

// HermesProc is one Hermes-shaped process found in a pane's process tree.
type HermesProc struct {
	// PID is the process.
	PID int
	// Rule names which pattern matched (one of the hermesRule* constants):
	// what a log line or a diagnostic prints. Never argv, which can hold
	// anything the person typed.
	Rule string
}

// The patterns of the broad match.
const (
	// hermesRuleLauncher: comm "hermes" and basename(argv[1]) "hermes" — the
	// Nix wrapper's exec, and a pip/uv console script (S10), whatever follows
	// (`chat`, `-p <profile> chat`, `gateway run`).
	hermesRuleLauncher = "comm=hermes"
	// hermesRuleModule: a Python interpreter running `-m hermes_cli…`. The
	// gateway's own argv (`-m hermes_cli.main … gateway run`) is one.
	hermesRuleModule = "python -m hermes_cli"
	// hermesRuleScript: a Python interpreter whose script is Hermes's code: a
	// path containing /hermes_cli/, a script named `hermes` (the launcher run
	// by an explicit interpreter, so comm is "python"), or a cli.py whose
	// directory holds hermes_cli/ (a Hermes source checkout).
	hermesRuleScript = "python hermes script"
	// hermesRuleInline: `python -c <code>` whose code names hermes_cli.
	hermesRuleInline = "python -c hermes_cli"
)

// HermesIdentity recognises Hermes processes from the proc filesystem.
type HermesIdentity struct {
	// Proc is the proc filesystem root ("" = /proc). A test points it at a
	// fixture tree, as ClaudeLocator's do.
	Proc string
}

func (h HermesIdentity) proc() string {
	if h.Proc != "" {
		return h.Proc
	}
	return "/proc"
}

// InTree is the nearest Hermes-shaped process at or below root (a pane's
// #{pane_pid}), breadth-first, and true; or false when the tree holds none.
//
// The error is the guard's fail-closed case: the proc root could not be read,
// or root itself is not in it (the pane's process is gone, or is not visible
// to this user). A caller refuses on it. The one exception is a host with no
// proc filesystem at all (not Linux, default root): there is nothing to walk,
// so it answers false — a named gap, not a check that passed.
func (h HermesIdentity) InTree(root int) (HermesProc, bool, error) {
	procs, children, err := readProcTable(h.proc())
	if err != nil {
		if h.Proc == "" && runtime.GOOS != "linux" && errors.Is(err, os.ErrNotExist) {
			return HermesProc{}, false, nil
		}
		return HermesProc{}, false, fmt.Errorf("read %s: %w", h.proc(), err)
	}
	if _, ok := procs[root]; !ok {
		return HermesProc{}, false, fmt.Errorf("process %d is not in %s", root, h.proc())
	}
	queue, seen := []int{root}, map[int]bool{}
	for len(queue) > 0 && len(seen) < 4096 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := procs[pid]; ok {
			if rule, ok := hermesShape(p, h.cwdOf(pid)); ok {
				return HermesProc{PID: pid, Rule: rule}, true, nil
			}
		}
		queue = append(queue, children[pid]...)
	}
	if len(queue) > 0 {
		// The walk was cut off with processes left to look at: fail closed.
		return HermesProc{}, false, fmt.Errorf("the process tree of %d is larger than the walk's bound", root)
	}
	return HermesProc{}, false, nil
}

// cwdOf is a function reading pid's working directory (its /proc cwd link),
// for resolving a relative script path; "" when it cannot be read.
func (h HermesIdentity) cwdOf(pid int) func() string {
	return func() string {
		d, err := os.Readlink(filepath.Join(h.proc(), strconv.Itoa(pid), "cwd"))
		if err != nil {
			return ""
		}
		return d
	}
}

// isPython: argv[0] names a Python interpreter (python, python3, python3.14,
// a venv's python).
func isPython(argv []string) bool {
	return len(argv) > 0 && strings.HasPrefix(filepath.Base(argv[0]), "python")
}

// hermesShape reports whether p is Hermes-shaped by the guard's broad match,
// and which rule matched. cwd reads the process's working directory, and is
// called only for a relative cli.py.
func hermesShape(p procInfo, cwd func() string) (string, bool) {
	if p.comm == "hermes" && len(p.argv) > 1 && filepath.Base(p.argv[1]) == "hermes" {
		return hermesRuleLauncher, true
	}
	if !isPython(p.argv) {
		return "", false
	}
	module, script, code := pythonTarget(p.argv[1:])
	switch {
	case module != "":
		if module == "hermes_cli" || strings.HasPrefix(module, "hermes_cli.") {
			return hermesRuleModule, true
		}
	case code != "":
		if strings.Contains(code, "hermes_cli") {
			return hermesRuleInline, true
		}
	case script != "":
		if hermesScript(script, cwd) {
			return hermesRuleScript, true
		}
	}
	return "", false
}

// hermesScript reports whether a Python script path is Hermes's code.
func hermesScript(script string, cwd func() string) bool {
	if strings.Contains(script, "/hermes_cli/") || strings.HasPrefix(script, "hermes_cli/") {
		return true
	}
	base := filepath.Base(script)
	if base == "hermes" {
		return true
	}
	if base != "cli.py" {
		return false
	}
	path := script
	if !filepath.IsAbs(path) {
		dir := cwd()
		if dir == "" {
			// A relative cli.py whose directory cannot be found: fail closed.
			return true
		}
		path = filepath.Join(dir, path)
	}
	fi, err := os.Stat(filepath.Join(filepath.Dir(path), "hermes_cli"))
	if err != nil {
		// Gone or unreadable is "not a Hermes install" only when the
		// directory provably does not hold one.
		return !errors.Is(err, os.ErrNotExist)
	}
	return fi.IsDir()
}

// pythonTarget reads what a Python command line runs, from its arguments after
// argv[0]: the module of `-m <module>` (or `-m<module>`), the code of `-c`, or
// the script path — the first argument that is not an option. Interpreter
// options are skipped, the two that take a value (-W, -X) with it.
func pythonTarget(args []string) (module, script, code string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return "", args[i+1], ""
			}
			return "", "", ""
		case a == "-":
			return "", "", "" // the script is on stdin
		case a == "-m" || a == "-c":
			if i+1 >= len(args) {
				return "", "", ""
			}
			if a == "-m" {
				return args[i+1], "", ""
			}
			return "", "", args[i+1]
		case strings.HasPrefix(a, "--"):
			// A long option (--version, --help): it takes no separate value.
		case strings.HasPrefix(a, "-"):
			// A cluster of single-letter options (-u, -OO, -uB) that may end in
			// one taking the rest of the cluster or the next argument as its
			// value: m or c (`-um hermes_cli.main`, `-mhermes_cli`), or W or X,
			// whose value (`-Wonce`) is not read as options.
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'W', 'X':
					if j+1 == len(a) {
						i++
					}
					break cluster
				case 'm', 'c':
				default:
					continue
				}
				val := a[j+1:]
				if val == "" {
					if i+1 >= len(args) {
						return "", "", ""
					}
					val = args[i+1]
				}
				if a[j] == 'm' {
					return val, "", ""
				}
				return "", "", val
			}
		default:
			return "", a, ""
		}
	}
	return "", "", ""
}

// HermesGuardBackend is a PaneBackend that can tell what runs in a pane: the
// paste guard's view of it. The tmux backend implements it. A backend that does
// not (a test's fake, the browser suite's fake tmux) has no processes to look
// at, and its pastes are not guarded.
type HermesGuardBackend interface {
	// PaneHermes is the Hermes-shaped process in pane's process tree, read
	// NOW; false when it holds none. An error means it could not be checked.
	PaneHermes(ctx context.Context, pane string) (HermesProc, bool, error)
}

// HermesGuardOf is b's HermesGuardBackend, looking through wrappers.
func HermesGuardOf(b Backend) (HermesGuardBackend, bool) { return capabilityOf[HermesGuardBackend](b) }

// PaneHermes implements HermesGuardBackend: the pane's root process
// (#{pane_pid}, read now by the pane's id) and a walk of its tree.
func (t Tmux) PaneHermes(ctx context.Context, paneID string) (HermesProc, bool, error) {
	pid, err := t.panePID(ctx, paneID)
	if err != nil {
		return HermesProc{}, false, err
	}
	return HermesIdentity{Proc: t.Claude.Proc}.InTree(pid)
}

// panePID is pane's #{pane_pid}, by its id.
func (t Tmux) panePID(ctx context.Context, paneID string) (int, error) {
	if !strings.HasPrefix(paneID, "%") {
		return 0, fmt.Errorf("%q is not a tmux pane id", paneID)
	}
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "-t", paneID, "#{pane_pid}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("tmux display-message: %v: %s", err, strings.TrimSpace(string(out)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("tmux reported pane %s's process as %q", paneID, strings.TrimSpace(string(out)))
	}
	return pid, nil
}

// guardHermesPaste is the host's paste guard for a paste into pane: nil to
// admit, else the refusal. Decided HERE, at the host's paste entry, by process
// identity — never by anything the server sent — so it holds whatever muster
// version asked for the paste.
func guardHermesPaste(ctx context.Context, b Backend, pane string) error {
	g, ok := HermesGuardOf(b)
	if !ok {
		return nil
	}
	hp, found, err := g.PaneHermes(ctx, pane)
	if err != nil {
		return fmt.Errorf("%w (%v)", errHermesUnchecked, err)
	}
	if found {
		return &hermesRefusal{proc: hp}
	}
	return nil
}

// hermesRefusal is ErrHermesPane, carrying what matched for the host's log.
type hermesRefusal struct{ proc HermesProc }

func (e *hermesRefusal) Error() string { return ErrHermesPane.Error() }
func (e *hermesRefusal) Unwrap() error { return ErrHermesPane }
