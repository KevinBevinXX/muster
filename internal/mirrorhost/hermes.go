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

	"github.com/ZacxDev/muster/internal/mirror"
)

// Hermes process identity, and the host paste guard built on it
// (docs/design/hermes.md, "Every paste into a Hermes pane is gated on the
// host", and S10).
//
// 🔴 THIS IS THE GUARD'S BROAD MATCH, NOT THE MIRRORING RULE, AND IT FAILS
// CLOSED. It answers "could anything in this pane's process tree be Hermes",
// so a paste is refused on any doubt. A false refusal is safe; a false admit
// is not: paste+Enter on Hermes's approval modal APPROVES the command, whatever
// the pasted text says (S11). The stricter rule that decides which pane is a
// Hermes WINDOW (argv[2] == chat, no gateway, fd 0 on the pane's tty) is
// hermesCLIShape and HermesIdentity.CLI below, beside this one, never instead
// of it.
//
// It refuses a pane whose tree holds:
//   - a Hermes-shaped process (hermesShape);
//   - a terminal multiplexer CLIENT (tmux, screen, dtach, abduco, zellij):
//     `TMUX= tmux -L other attach` in a pane shows a session of ANOTHER server,
//     whose processes are not in this pane's tree, so a Hermes there would be
//     invisible to the walk. Every nested multiplexer is refused — a false
//     refusal for one that runs no Hermes, accepted as safe;
//   - a process it could not read (its stat or cmdline), which is not
//     presumed safe.
//
// Known gaps, as the design names them: a Hermes run over ssh or `docker exec`
// inside the pane (invisible to a local process walk); a Hermes launched under
// a renamed interpreter or from a copied tree the argv patterns do not match;
// and a host with no /proc (macOS), where this guard sees no processes and
// admits — a ratified gap: failing closed there would take the Terminal
// view's message box away from every macOS host, and no deployment runs
// Hermes on one.

// The guard's refusals. Their text is the person's (mirror.PasteRefused*Text):
// muster shows a host's refusal as is. Never with internals appended — those
// are logged on the host.
var (
	// ErrHermesPane: the pane's process tree holds a Hermes-shaped process.
	ErrHermesPane = errors.New(mirror.PasteRefusedHermesText)
	// ErrMultiplexerPane: the pane's process tree holds a multiplexer client.
	ErrMultiplexerPane = errors.New(mirror.PasteRefusedMultiplexerText)
	// ErrPaneUnchecked: the guard could not read the pane's processes.
	ErrPaneUnchecked = errors.New(mirror.PasteRefusedUncheckedText)
)

// pasteRefusalOf is the Msg.Refusal kind of a guard refusal, "" for any other
// error.
func pasteRefusalOf(err error) string {
	switch {
	case errors.Is(err, ErrHermesPane):
		return mirror.PasteRefusalHermes
	case errors.Is(err, ErrMultiplexerPane):
		return mirror.PasteRefusalMultiplexer
	case errors.Is(err, ErrPaneUnchecked):
		return mirror.PasteRefusalUnchecked
	}
	return ""
}

// HermesProc is the process in a pane's tree that made the guard refuse.
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
	// hermesRulePython: a Python interpreter with any argument that names
	// hermes_cli (`-m hermes_cli.main`, a script under hermes_cli/, `-c` code
	// importing it) or whose basename is `hermes` (the launcher run as
	// `python …/bin/hermes`, where comm is "python"). The gateway's own argv
	// (`-m hermes_cli.main … gateway run`) is one.
	hermesRulePython = "python running hermes"
	// hermesRuleCheckout: a Python interpreter with a cli.py argument whose
	// directory holds hermes_cli/ (a Hermes source checkout).
	hermesRuleCheckout = "python cli.py of a hermes checkout"
	// hermesRuleMultiplexer: a terminal multiplexer client, which can show a
	// Hermes this walk cannot see.
	hermesRuleMultiplexer = "multiplexer client"
)

// multiplexers are the terminal multiplexers whose client, in a pane, shows
// sessions whose processes are outside that pane's tree.
var multiplexers = map[string]bool{"tmux": true, "screen": true, "dtach": true, "abduco": true, "zellij": true}

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

// procWalkBound is how many processes InTree looks at before giving up — and
// refusing — so a pathological tree cannot stall a paste.
var procWalkBound = 4096

// InTree is the nearest process at or below root (a pane's #{pane_pid}),
// breadth-first, that makes the guard refuse — a Hermes-shaped process or a
// multiplexer client — and true; or false when the tree holds none.
//
// The error is the guard's fail-closed case, which the caller refuses on: the
// proc root could not be read; root is not in it; a process in the tree could
// not be read (its stat, or its cmdline); or the tree is larger than the
// walk's bound. A process's children are taken from the parent links AND from
// the kernel's own lists (/proc/<pid>/task/<tid>/children), so a child whose
// stat cannot be read is still found, and refused on — unless it has exited
// meanwhile. A zombie is skipped: it reads no input. The one exception is a
// host with no proc filesystem at all (not Linux, default root): there is
// nothing to walk, so it answers false — a ratified gap, not a check that
// passed.
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
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		if len(seen) >= procWalkBound {
			return HermesProc{}, false, fmt.Errorf("the process tree of %d is larger than %d processes", root, procWalkBound)
		}
		seen[pid] = true
		p, ok := procs[pid]
		if !ok {
			// Listed by the kernel as a child, absent from the table: its stat
			// could not be read. Gone since is fine; anything else is not.
			if _, err := os.Lstat(filepath.Join(h.proc(), strconv.Itoa(pid))); errors.Is(err, os.ErrNotExist) {
				continue
			}
			return HermesProc{}, false, fmt.Errorf("process %d in the tree of %d could not be read", pid, root)
		}
		if p.state == "Z" {
			continue
		}
		if p.argvUnread || len(strings.Join(p.argv, "")) == 0 {
			return HermesProc{}, false, fmt.Errorf("the command line of process %d in the tree of %d could not be read", pid, root)
		}
		if rule, ok := hermesShape(p, h.cwdOf(pid)); ok {
			return HermesProc{PID: pid, Rule: rule}, true, nil
		}
		queue = append(queue, children[pid]...)
		queue = append(queue, h.kernelChildren(pid)...)
	}
	return HermesProc{}, false, nil
}

// kernelChildren is pid's children as the kernel lists them, per thread
// (/proc/<pid>/task/<tid>/children); none when the lists cannot be read (a
// kernel without them, a fixture that writes none).
func (h HermesIdentity) kernelChildren(pid int) []int {
	files, _ := filepath.Glob(filepath.Join(h.proc(), strconv.Itoa(pid), "task", "*", "children"))
	var out []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, s := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(s); err == nil && c > 0 {
				out = append(out, c)
			}
		}
	}
	return out
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

// multiplexerName is a comm or argv[0] reduced to the program's name: its
// basename, before any ':' (tmux's client sets its comm to "tmux: client"),
// lowercased (GNU screen's server is "SCREEN").
func multiplexerName(s string) string {
	s = filepath.Base(s)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// hermesShape reports whether p makes the guard refuse, and which rule
// matched. cwd reads the process's working directory, and is called only for
// a relative cli.py.
func hermesShape(p procInfo, cwd func() string) (string, bool) {
	if multiplexers[multiplexerName(p.comm)] || (len(p.argv) > 0 && multiplexers[multiplexerName(p.argv[0])]) {
		return hermesRuleMultiplexer, true
	}
	if p.comm == "hermes" && len(p.argv) > 1 && filepath.Base(p.argv[1]) == "hermes" {
		return hermesRuleLauncher, true
	}
	if !isPython(p.argv) {
		return "", false
	}
	// 🔴 A FLAT SCAN, NOT A PARSE OF PYTHON'S OPTIONS: any argument counts, so
	// an option's value or a script's own argument naming hermes_cli refuses
	// too. That can only refuse more, which is this guard's bias.
	for _, a := range p.argv[1:] {
		if strings.Contains(a, "hermes_cli") || filepath.Base(a) == "hermes" {
			return hermesRulePython, true
		}
	}
	for _, a := range p.argv[1:] {
		if filepath.Base(a) == "cli.py" && hermesCheckout(a, cwd) {
			return hermesRuleCheckout, true
		}
	}
	return "", false
}

// hermesCheckout reports whether the directory of a cli.py path holds
// hermes_cli/ — failing closed when that cannot be found out.
func hermesCheckout(script string, cwd func() string) bool {
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
		// "Not a Hermes checkout" only when hermes_cli provably does not
		// exist; any other error (a loop, a permission) is not proof.
		return !errors.Is(err, os.ErrNotExist)
	}
	return fi.IsDir()
}

// THE STRICT MIRRORING RULE (docs/design/hermes.md, "Pane to session
// resolution", S10): which process in a pane IS the Hermes CLI a rendered view
// would follow. It sits beside the broad match above and never replaces it:
//
//   - hermesShape (broad) decides a REFUSAL, so it matches anything that could
//     be Hermes and fails closed;
//   - hermesCLIShape (strict) decides a CLAIM — the window is a Hermes window
//     — so it matches only the interactive CLI's exact shape and, on any
//     doubt, claims nothing: a window it misses is a plain terminal window,
//     which the broad guard still refuses to paste into.
//
// A strict match is always a broad one (TestEveryStrictHermesCLIIsRefusedByTheBroadGuard),
// so no pane can be a Hermes window and pasteable.

// hermesCLIShape reports whether p is the Hermes CLI by its own identity:
//
//   - comm "hermes";
//   - argv[0] a Python interpreter and basename(argv[1]) "hermes" (the Nix
//     wrapper execs, leaving no shell; a pip/uv console script has the same
//     shape);
//   - argv[2] "chat", or absent (the bare form is UNVERIFIED upstream);
//   - no argument "gateway": the gateway is comm=hermes too, long-lived, and
//     outlives the panes.
//
// ⚠ A profile-launched CLI (`hermes -p coder chat`) does NOT match — its
// argv[2] is "-p" — and so has no Hermes window. That is this rule failing
// toward "no claim", which the broad guard's flat scan covers for pasting.
func hermesCLIShape(p procInfo) bool {
	if p.comm != "hermes" || len(p.argv) < 2 || !isPython(p.argv) || filepath.Base(p.argv[1]) != "hermes" {
		return false
	}
	if len(p.argv) > 2 && p.argv[2] != "chat" {
		return false
	}
	for _, a := range p.argv[1:] {
		if a == "gateway" {
			return false
		}
	}
	return true
}

// CLI is the Hermes CLI in the tree at or below root (a pane's #{pane_pid}),
// breadth-first and bounded: the nearest process that matches hermesCLIShape
// AND whose stdin (fd 0) is tty, the pane's terminal. false when there is none
// — or when anything needed cannot be read: this is a claim, so doubt claims
// nothing.
func (h HermesIdentity) CLI(root int, tty string) (int, bool) {
	procs, children, err := readProcTable(h.proc())
	if err != nil {
		return 0, false
	}
	return h.cli(root, tty, procs, children)
}

// cli is CLI over a proc table already read.
func (h HermesIdentity) cli(root int, tty string, procs map[int]procInfo, children map[int][]int) (int, bool) {
	if tty == "" {
		return 0, false
	}
	queue, seen := []int{root}, map[int]bool{}
	for len(queue) > 0 && len(seen) < procWalkBound {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		p, ok := procs[pid]
		if !ok {
			continue
		}
		if p.state != "Z" && hermesCLIShape(p) {
			if fd0, err := os.Readlink(filepath.Join(h.proc(), strconv.Itoa(pid), "fd", "0")); err == nil && fd0 == tty {
				return pid, true
			}
		}
		queue = append(queue, children[pid]...)
	}
	return 0, false
}

// HermesGuardBackend is a backend that can tell what runs in a pane: the paste
// guard's view of it. The tmux backend implements it. A backend that does not
// (a test's fake, the browser suite's fake tmux) has no processes to look at,
// and what it types is not guarded.
type HermesGuardBackend interface {
	// PaneHermes is the process in pane's tree that makes the guard refuse,
	// read NOW (HermesIdentity.InTree); false when it holds none. An error
	// means it could not be checked.
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

// typeInto is the ONE way this host types into a pane: the Terminal view's
// paste, the rendered view's paste and an answer's keys all come through here
// (hermes_ledger_test.go fails when a call that types reaches tmux any other
// way). 🔴 It runs the Hermes paste guard on pane FIRST — by what runs in the
// pane NOW, whatever muster sent, so it holds whichever server version asked —
// then cancels this connection's copy-mode, then types. A refusal types
// nothing and touches nothing.
func (h *hostSide) typeInto(ctx context.Context, pane string, keys func() error) error {
	if err := h.guardPane(ctx, pane); err != nil {
		return err
	}
	h.leaveCopyModeFor(pane)
	return keys()
}

// guardPane is the Hermes paste guard for pane: nil to admit, else the
// refusal. Internals (tmux's stderr, which process could not be read) are
// logged here and never put in the refusal the person sees.
func (h *hostSide) guardPane(ctx context.Context, pane string) error {
	g, ok := HermesGuardOf(h.c.cfg.Backend)
	if !ok {
		return nil
	}
	hp, found, err := g.PaneHermes(ctx, pane)
	switch {
	case err != nil:
		h.c.log.Printf("muster-mirror: could not check what runs in pane %s, so nothing is typed into it: %v", pane, err)
		return ErrPaneUnchecked
	case !found:
		return nil
	case hp.Rule == hermesRuleMultiplexer:
		h.c.log.Printf("muster-mirror: pane %s runs a terminal multiplexer client (pid %d), which can hide Hermes; nothing is typed into it", pane, hp.PID)
		return ErrMultiplexerPane
	default:
		h.c.log.Printf("muster-mirror: pane %s runs Hermes (pid %d, %s); nothing is typed into it", pane, hp.PID, hp.Rule)
		return ErrHermesPane
	}
}
