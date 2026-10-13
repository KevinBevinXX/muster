package mirrorhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ClaudeBackend is a Backend that can also see Claude Code in a session's pane:
// what the RENDERED VIEW needs (tunnel v2). The tmux backend implements it; a
// backend that does not simply has no rendered view.
type ClaudeBackend interface {
	// ClaudePanes is, for each of sessions whose ACTIVE pane runs Claude Code
	// with a transcript this user can read, that pane. A session missing from
	// the map is not a Claude pane (or could not be resolved).
	ClaudePanes(ctx context.Context, sessions []string) map[string]ClaudePane
	// Paste types text into pane as ONE bracketed paste and presses Enter.
	Paste(ctx context.Context, pane ClaudePane, text string) error
}

// ClaudeOf is b's ClaudeBackend, looking THROUGH any wrapper around it (a
// wrapper exposes what it wraps with `Unwrap() Backend`, as NoShrink's does).
// It is the ONE way to ask a backend for this capability: a bare type
// assertion on a wrapped backend answers "no" for a backend that can.
func ClaudeOf(b Backend) (ClaudeBackend, bool) {
	for i := 0; b != nil && i < 8; i++ {
		if cb, ok := b.(ClaudeBackend); ok {
			return cb, true
		}
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			break
		}
		b = u.Unwrap()
	}
	return nil, false
}

// ClaudePane is one tmux pane running Claude Code.
type ClaudePane struct {
	// Pane is the tmux pane id (%12): the only input target — never the session
	// name, which resolves to whatever pane was focused last.
	Pane string
	// PID is the Claude Code process.
	PID int
	// Transcript is the absolute path of the session's JSONL transcript.
	Transcript string
	// Gen names that transcript: the Claude Code session id. It changes when
	// the pane's CLI starts another session (/clear, a restart), which is how a
	// rendered view knows to start over.
	Gen string
	// Busy: Claude Code reports a turn running.
	Busy bool
}

// pane is one tmux pane as list-panes reports it.
type pane struct {
	Session string
	ID      string
	PID     int
}

// ClaudeLocator maps panes to the Claude Code sessions running in them.
//
// HOW (decided here, see the PR for the alternatives):
//
//  1. Walk the pane's process tree from its root process (#{pane_pid}),
//     breadth-first, to the nearest process that IS Claude Code (isClaude). A
//     shell, `nix develop`, a wrapper script in between is fine.
//  2. Claude Code records itself in <home>/sessions/<pid>.json — {sessionId,
//     cwd, status}, written by the CLI for its own peers and kept current
//     (status flips busy/idle per turn; a /clear moves sessionId). That names the
//     transcript exactly: <home>/projects/<slug(cwd)>/<sessionId>.jsonl, and it
//     is unambiguous when several sessions share one cwd.
//  3. Fallback, for a CLI that writes no sessions file: the NEWEST *.jsonl in
//     <home>/projects/<slug(the process's cwd)>/. ⚠ With two CLIs in one cwd and
//     no sessions files, that is whichever wrote last — the one ambiguity this
//     cannot resolve, and it is only reached on CLIs older than the sessions file.
//
// It reads only files the user muster-mirror runs as can read: its own
// processes' /proc entries and its own ~/.claude.
type ClaudeLocator struct {
	// Proc is the proc filesystem root ("" = /proc). A test points it at a
	// fixture tree.
	Proc string
	// Home is Claude Code's config dir ("" = $CLAUDE_CONFIG_DIR, else ~/.claude).
	Home string
}

func (l ClaudeLocator) proc() string {
	if l.Proc != "" {
		return l.Proc
	}
	return "/proc"
}

func (l ClaudeLocator) configDir() string {
	if l.Home != "" {
		return l.Home
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

// procInfo is the little of a process the walk needs.
type procInfo struct {
	ppid int
	comm string
	argv []string
}

// table reads every process's parent, name and argv once.
func (l ClaudeLocator) table() (map[int]procInfo, map[int][]int) {
	procs := map[int]procInfo{}
	children := map[int][]int{}
	ents, err := os.ReadDir(l.proc())
	if err != nil {
		return procs, children
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(l.proc(), e.Name())
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		// stat is "pid (comm) state ppid …"; comm can hold spaces and ')', so
		// the fields after it are found from the LAST ')'.
		open, close := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if open < 0 || close < open {
			continue
		}
		rest := strings.Fields(string(stat[close+1:]))
		if len(rest) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(rest[1])
		info := procInfo{ppid: ppid, comm: string(stat[open+1 : close])}
		if cmd, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			for _, a := range bytes.Split(bytes.TrimRight(cmd, "\x00"), []byte{0}) {
				info.argv = append(info.argv, string(a))
			}
		}
		procs[pid] = info
		children[ppid] = append(children[ppid], pid)
	}
	for _, c := range children {
		sort.Ints(c)
	}
	return procs, children
}

// isClaude reports whether a process is the Claude Code CLI: its name (the CLI
// sets its process title to "claude"; the NixOS wrapper execs ".claude-wrapped"),
// its argv[0], or the npm package's entry script in its argv (a CLI started
// as `node …/@anthropic-ai/claude-code/cli.js`).
func isClaude(p procInfo) bool {
	if p.comm == "claude" || p.comm == ".claude-wrapped" {
		return true
	}
	if len(p.argv) > 0 {
		if b := filepath.Base(p.argv[0]); b == "claude" || b == ".claude-wrapped" {
			return true
		}
	}
	for _, a := range p.argv {
		if strings.Contains(a, "@anthropic-ai/claude-code/") {
			return true
		}
	}
	return false
}

// findClaude is the nearest Claude Code process at or below root, breadth-first,
// or 0. The walk is bounded so a pathological tree cannot stall a refresh.
func findClaude(root int, procs map[int]procInfo, children map[int][]int) int {
	queue, seen := []int{root}, map[int]bool{}
	for len(queue) > 0 && len(seen) < 4096 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := procs[pid]; ok && isClaude(p) {
			return pid
		}
		queue = append(queue, children[pid]...)
	}
	return 0
}

// slugPattern is what Claude Code replaces with '-' to name a cwd's project dir.
var slugPattern = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectSlug is the directory name Claude Code keeps a cwd's transcripts under:
// every character that is not an ASCII letter or digit becomes '-'
// (/srv/my.repo → -srv-my-repo).
func ProjectSlug(cwd string) string { return slugPattern.ReplaceAllString(cwd, "-") }

// sessionFile is <home>/sessions/<pid>.json, the fields read.
type sessionFile struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"`
}

// sessionIDPattern bounds what a sessions file may name: it is joined into a path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// resolve is the transcript of Claude Code process pid, or ok=false.
func (l ClaudeLocator) resolve(pid int) (path, gen string, busy, ok bool) {
	cfg := l.configDir()
	if b, err := os.ReadFile(filepath.Join(cfg, "sessions", strconv.Itoa(pid)+".json")); err == nil {
		var sf sessionFile
		if json.Unmarshal(b, &sf) == nil && sessionIDPattern.MatchString(sf.SessionID) {
			busy = sf.Status == "busy"
			name := sf.SessionID + ".jsonl"
			if sf.Cwd != "" {
				p := filepath.Join(cfg, "projects", ProjectSlug(sf.Cwd), name)
				if fileExists(p) {
					return p, sf.SessionID, busy, true
				}
			}
			// The cwd moved, or the slug rule differs: the id is unique, so find it.
			if m, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", name)); len(m) > 0 {
				return m[0], sf.SessionID, busy, true
			}
			// A brand-new session has a sessions file before its first record:
			// nothing to show YET. Not the fallback — that would show another
			// session's transcript under this pane.
			return "", "", false, false
		}
	}
	cwd, err := os.Readlink(filepath.Join(l.proc(), strconv.Itoa(pid), "cwd"))
	if err != nil {
		return "", "", false, false
	}
	m, _ := filepath.Glob(filepath.Join(cfg, "projects", ProjectSlug(cwd), "*.jsonl"))
	var newest string
	var newestT time.Time
	for _, p := range m {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && (newest == "" || st.ModTime().After(newestT)) {
			newest, newestT = p, st.ModTime()
		}
	}
	if newest == "" {
		return "", "", false, false
	}
	return newest, strings.TrimSuffix(filepath.Base(newest), ".jsonl"), false, true
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Locate maps each pane that runs Claude Code to its ClaudePane, keyed by the
// pane's session.
func (l ClaudeLocator) Locate(panes []pane) map[string]ClaudePane {
	out := map[string]ClaudePane{}
	if len(panes) == 0 {
		return out
	}
	procs, children := l.table()
	for _, p := range panes {
		pid := findClaude(p.PID, procs, children)
		if pid == 0 {
			continue
		}
		path, gen, busy, ok := l.resolve(pid)
		if !ok {
			continue
		}
		out[p.Session] = ClaudePane{Pane: p.ID, PID: pid, Transcript: path, Gen: gen, Busy: busy}
	}
	return out
}

// activePanes is each of sessions' active pane (of its active window), from one
// `tmux list-panes -a`.
func (t Tmux) activePanes(ctx context.Context, sessions []string) []pane {
	want := map[string]bool{}
	for _, s := range sessions {
		want[s] = true
	}
	cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), "list-panes", "-a", "-F",
		"#{window_active}#{pane_active}\t#{pane_id}\t#{pane_pid}\t#{session_name}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseActivePanes(string(out), want)
}

// parseActivePanes reads activePanes' list-panes output. The session name is the
// LAST field, so a name is taken whole whatever it holds (tabs cannot occur: a
// listed name has no control characters).
func parseActivePanes(out string, want map[string]bool) []pane {
	var panes []pane
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 4)
		if len(f) != 4 || f[0] != "11" || !want[f[3]] || !strings.HasPrefix(f[1], "%") {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil || pid <= 0 {
			continue
		}
		panes = append(panes, pane{Session: f[3], ID: f[1], PID: pid})
	}
	return panes
}

// ClaudePanes implements ClaudeBackend.
func (t Tmux) ClaudePanes(ctx context.Context, sessions []string) map[string]ClaudePane {
	return t.Claude.Locate(t.activePanes(ctx, sessions))
}

// enterDelay separates the paste from the Enter, so the TUI has taken the paste
// in before the submit key arrives (ccd's value, measured there).
const enterDelay = 150 * time.Millisecond

// Paste implements ClaudeBackend the way ccd hands a provisioned agent its
// prompt: `load-buffer` from STDIN into a uniquely named buffer (never argv —
// visible in the process table and parsed by tmux), `paste-buffer -p -d` into
// the PANE id (bracketed, buffer deleted after), then Enter.
//
// The text must already be mirror.SanitizePaste'd: a control character inside
// a bracketed paste can end it, and what follows is then TYPED.
func (t Tmux) Paste(ctx context.Context, p ClaudePane, text string) error {
	if !strings.HasPrefix(p.Pane, "%") {
		return fmt.Errorf("muster-mirror: %q is not a tmux pane id", p.Pane)
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := "muster-mirror-" + hex.EncodeToString(rnd[:])
	run := func(stdin []byte, args ...string) error {
		cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), args...)...)
		cmd.Env = tmuxEnv()
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run([]byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if err := run(nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", p.Pane); err != nil {
		_ = run(nil, "delete-buffer", "-b", buf)
		return err
	}
	select {
	case <-time.After(enterDelay):
	case <-ctx.Done():
		return errors.New("muster-mirror: the message was pasted but not submitted (cancelled)")
	}
	if err := run(nil, "send-keys", "-t", p.Pane, "Enter"); err != nil {
		return fmt.Errorf("muster-mirror: the message was pasted but not submitted: %w", err)
	}
	return nil
}
