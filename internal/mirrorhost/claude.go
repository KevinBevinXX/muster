package mirrorhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
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

// PaneBackend is a Backend that can type into a session's ACTIVE pane whatever
// runs in it — a shell, another TUI: what the Terminal view's composer needs
// for a session with no rendered view. The tmux backend implements it.
type PaneBackend interface {
	// ActivePane is session's active pane (of its active window) as an input
	// target, and false when the session has none this backend can resolve.
	ActivePane(ctx context.Context, session string) (string, bool)
	// PastePane types text into pane as ONE bracketed paste and presses Enter.
	PastePane(ctx context.Context, pane, text string) error
}

// PaneOf is b's PaneBackend, looking through wrappers like ClaudeOf.
func PaneOf(b Backend) (PaneBackend, bool) { return capabilityOf[PaneBackend](b) }

// ClaudeOf is b's ClaudeBackend, looking THROUGH any wrapper around it (a
// wrapper exposes what it wraps with `Unwrap() Backend`, as NoShrink's does).
// It is the ONE way to ask a backend for this capability: a bare type
// assertion on a wrapped backend answers "no" for a backend that can.
func ClaudeOf(b Backend) (ClaudeBackend, bool) { return capabilityOf[ClaudeBackend](b) }

// capabilityOf is b's optional capability T, looking THROUGH any wrapper around
// it (`Unwrap() Backend`). The one walk every capability lookup shares.
func capabilityOf[T any](b Backend) (T, bool) {
	for i := 0; b != nil && i < 8; i++ {
		if c, ok := b.(T); ok {
			return c, true
		}
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			break
		}
		b = u.Unwrap()
	}
	var zero T
	return zero, false
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
	// Status is Claude Code's own session status, as its sessions file states
	// it, read through mirror.NormalizeStatus (StatusBusy, StatusIdle,
	// StatusWaiting; "shell" — idle with a background shell running — as
	// StatusIdle); "" when it states none this side recognises, and for a
	// GUESSED transcript, which has no sessions file to read one from. Busy is
	// Status == busy.
	Status string
	// StatusSince is when Claude Code wrote Status — its sessions file's
	// statusUpdatedAt, Unix milliseconds — 0 when it states none (and whenever
	// Status is ""). The send queue reads idle-since from it.
	StatusSince int64
	// Pending is what the session waits on while Status is waiting
	// (pending.go: read from the transcript's tail by the host's listing, not
	// by the locator); nil otherwise.
	Pending *mirror.Pending
	// Source says how Transcript was found: SourcePaneMap, SourceSessions or
	// SourceGuessed (see ClaudeLocator).
	Source string
}

// How a ClaudePane's transcript was resolved, most to least authoritative.
const (
	// SourcePaneMap: the pane map's entry for this pane, validated against the
	// process running in the pane (ClaudeLocator.PaneMapDir).
	SourcePaneMap = "pane-map"
	// SourceSessions: Claude Code's own sessions/<pid>.json (following a
	// parked background job to the job's session).
	SourceSessions = "sessions"
	// SourceGuessed: the newest transcript in the process's cwd — a guess.
	SourceGuessed = "guessed"
)

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
//  0. THE PANE MAP, when PaneMapDir is set: a file per pane written by a Claude
//     Code hook on every event that carries the conversation's id — so it names
//     the conversation the pane SHOWS NOW. It is believed only after it is
//     proved to still describe this pane (paneMapEntry.valid); anything else
//     falls through to 2. Why it outranks 2: Claude Code does NOT rewrite
//     sessions/<pid>.json when the conversation in a process changes id — a
//     conversation moved to a background job (/bg) keeps the CLI's file naming
//     the session it was forked FROM, for hours.
//  1. Walk the pane's process tree from its root process (#{pane_pid}),
//     breadth-first, to the nearest process that IS Claude Code (isClaude). A
//     shell, `nix develop`, a wrapper script in between is fine.
//  2. Claude Code records itself in <home>/sessions/<pid>.json — {sessionId,
//     cwd, status}, written by the CLI for its own peers (status flips busy/idle
//     per turn; a /clear moves sessionId). That names the transcript exactly:
//     <home>/projects/<slug(cwd)>/<sessionId>.jsonl, and it is unambiguous when
//     several sessions share one cwd. When the file says the CLI has a
//     background job PARKED (parkedJobId), the pane shows that job: the live
//     job's own sessions file (kind "bg", the same jobId) names the session.
//  3. Fallback, for a CLI that writes no sessions file: the NEWEST *.jsonl in
//     <home>/projects/<slug(the process's cwd)>/. ⚠ With two CLIs in one cwd and
//     no sessions files, that is whichever wrote last — the one ambiguity this
//     cannot resolve, and it is only reached on CLIs older than the sessions
//     file. So a GUESSED transcript that another pane resolves to as well is
//     dropped (LocatePanes): two panes are never shown one conversation, and a
//     pane it cannot tell apart has no rendered view rather than a wrong one.
//
// It reads only files the user muster-mirror runs as can read: its own
// processes' /proc entries and its own ~/.claude.
type ClaudeLocator struct {
	// Proc is the proc filesystem root ("" = /proc). A test points it at a
	// fixture tree.
	Proc string
	// Home is Claude Code's config dir ("" = $CLAUDE_CONFIG_DIR, else ~/.claude).
	Home string
	// PaneMapDir is the pane map's directory ("" = not consulted). One file per
	// tmux pane, claude-p<N>.json for pane %N, whose first line is a JSON
	// object {runtime: "claude", pane_id, tmux_pid, session_id,
	// transcript_path, claude_pid, claude_start} — the shape a Claude Code hook
	// writes (the format is documented in docs/mirroring.md).
	PaneMapDir string
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
	// start is /proc/<pid>/stat field 22 (start time, clock ticks since boot),
	// as written: what tells a live process from a reused pid.
	start string
}

// table reads every process's parent, name and argv once.
func (l ClaudeLocator) table() (map[int]procInfo, map[int][]int) {
	procs, children, _ := readProcTable(l.proc())
	return procs, children
}

// readProcTable reads every process under the proc root once: parent, name,
// argv and start time, and each process's children (sorted). The error is the
// proc root's own (unreadable, absent); a process that vanishes mid-read is
// skipped. Shared by ClaudeLocator and HermesIdentity.
func readProcTable(root string) (map[int]procInfo, map[int][]int, error) {
	procs := map[int]procInfo{}
	children := map[int][]int{}
	ents, err := os.ReadDir(root)
	if err != nil {
		return procs, children, err
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
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
		if len(rest) > 19 {
			info.start = rest[19]
		}
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
	return procs, children, nil
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
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"`
	// StatusUpdatedAt is when the CLI last wrote Status, Unix milliseconds:
	// every write that carries a status stamps it (measured on Claude Code
	// 2.1.289; see docs/mirroring.md).
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	ProcStart       string `json:"procStart"`
	Kind            string `json:"kind"`
	JobID           string `json:"jobId"`
	// ParkedJobID: this CLI is showing a background job (/bg), not its own
	// session.
	ParkedJobID string `json:"parkedJobId"`
}

// liveSessions indexes every sessions file whose process is ALIVE — its pid is
// in procs and, when the file records procStart, has that start time (a sessions
// file outlives its process, and the pid can be reused). byID is sessionId ->
// file; byJob is a background job's jobId -> file, and a jobId two live files
// claim is left out: ambiguous is not resolved.
type liveSessions struct {
	byID  map[string]sessionFile
	byJob map[string]sessionFile
}

func (l ClaudeLocator) liveSessions(procs map[int]procInfo) liveSessions {
	out := liveSessions{byID: map[string]sessionFile{}, byJob: map[string]sessionFile{}}
	m, _ := filepath.Glob(filepath.Join(l.configDir(), "sessions", "*.json"))
	dup := map[string]bool{}
	for _, p := range m {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var sf sessionFile
		if json.Unmarshal(b, &sf) != nil || !sessionIDPattern.MatchString(sf.SessionID) {
			continue
		}
		pi, alive := procs[sf.PID]
		if !alive || (sf.ProcStart != "" && sf.ProcStart != pi.start) {
			continue
		}
		out.byID[sf.SessionID] = sf
		if sf.Kind == "bg" && sf.JobID != "" {
			if _, seen := out.byJob[sf.JobID]; seen {
				dup[sf.JobID] = true
			}
			out.byJob[sf.JobID] = sf
		}
	}
	for j := range dup {
		delete(out.byJob, j)
	}
	return out
}

// sessionIDPattern bounds what a sessions file may name: it is joined into a path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// resolve is the transcript of Claude Code process pid, or ok=false. guessed
// says the answer is the FALLBACK's (the newest transcript in the process's
// cwd), not one the CLI named for itself.
func (l ClaudeLocator) resolve(pid int, live func() liveSessions) (path, gen string, st paneStatus, guessed, ok bool) {
	cfg := l.configDir()
	if b, err := os.ReadFile(filepath.Join(cfg, "sessions", strconv.Itoa(pid)+".json")); err == nil {
		var sf sessionFile
		if json.Unmarshal(b, &sf) == nil && sessionIDPattern.MatchString(sf.SessionID) {
			// A parked background job is what the pane shows; its own file is
			// kept current, the CLI's is not.
			if sf.ParkedJobID != "" {
				if job, ok := live().byJob[sf.ParkedJobID]; ok {
					sf.SessionID, sf.Cwd, sf.Status, sf.StatusUpdatedAt = job.SessionID, job.Cwd, job.Status, job.StatusUpdatedAt
				}
			}
			st = statusOf(sf)
			name := sf.SessionID + ".jsonl"
			if sf.Cwd != "" {
				p := filepath.Join(cfg, "projects", ProjectSlug(sf.Cwd), name)
				if fileExists(p) {
					return p, sf.SessionID, st, false, true
				}
			}
			// The cwd moved, or the slug rule differs: the id is unique, so find it.
			if m, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", name)); len(m) > 0 {
				return m[0], sf.SessionID, st, false, true
			}
			// A brand-new session has a sessions file before its first record:
			// nothing to show YET. Not the fallback — that would show another
			// session's transcript under this pane.
			return "", "", paneStatus{}, false, false
		}
	}
	cwd, err := os.Readlink(filepath.Join(l.proc(), strconv.Itoa(pid), "cwd"))
	if err != nil {
		return "", "", paneStatus{}, false, false
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
		return "", "", paneStatus{}, false, false
	}
	return newest, strings.TrimSuffix(filepath.Base(newest), ".jsonl"), paneStatus{}, true, true
}

// paneStatus is a sessions file's status and when the CLI wrote it.
type paneStatus struct {
	status string
	since  int64
}

// statusOf is sf's status, normalised (mirror.NormalizeStatus: Claude Code's
// "shell" is idle), and its time — none without a status.
//
// 🔴 THE MAPPING IS APPLIED HERE, ON THE HOST, BEFORE ANYTHING IS SENT: muster
// only ever sees what this returns. So a muster-mirror older than the mapping
// sends "" for "shell" (and for any status it did not know), and the send
// queue holds that window as unknown whatever muster version runs.
func statusOf(sf sessionFile) paneStatus {
	st := paneStatus{status: mirror.NormalizeStatus(sf.Status)}
	if st.status != "" && sf.StatusUpdatedAt > 0 {
		st.since = sf.StatusUpdatedAt
	}
	return st
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// paneMapEntry is one pane map file's record (ClaudeLocator.PaneMapDir).
type paneMapEntry struct {
	Runtime     string `json:"runtime"`
	PaneID      string `json:"pane_id"`
	TmuxPID     string `json:"tmux_pid"`
	SessionID   string `json:"session_id"`
	Transcript  string `json:"transcript_path"`
	ClaudePID   string `json:"claude_pid"`
	ClaudeStart string `json:"claude_start"`
}

// paneIDPattern is a tmux pane id; only its digits name a pane map file.
var paneIDPattern = regexp.MustCompile(`^%[0-9]{1,9}$`)

// fromPaneMap is pane p's transcript from the pane map, or ok=false — when the
// map is off, has no entry for p, or its entry fails valid. claude is the
// Claude Code process findClaude found in p.
func (l ClaudeLocator) fromPaneMap(p pane, claude int, procs map[int]procInfo) (path, gen string, ok bool) {
	if l.PaneMapDir == "" || !paneIDPattern.MatchString(p.ID) {
		return "", "", false
	}
	f, err := os.Open(filepath.Join(l.PaneMapDir, "claude-p"+p.ID[1:]+".json"))
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	var e paneMapEntry
	if json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&e) != nil {
		return "", "", false
	}
	if !e.valid(p, claude, procs, filepath.Join(l.configDir(), "projects")) {
		return "", "", false
	}
	return e.Transcript, e.SessionID, true
}

// valid: the entry still describes pane p as it is NOW. Every clause is a way a
// pane map entry goes stale or wrong while its file sits there unchanged:
//
//   - it is another pane's (pane ids are per server) or another runtime's;
//   - it was written under a DIFFERENT tmux server: p's root process is a child
//     of its server, so the recorded server pid must be that parent;
//   - the Claude Code process it names is not the one in the pane NOW — exited,
//     replaced, or the pane runs something else (claude == 0). Not merely "a
//     descendant of the pane": a `claude -p` run from inside a session is a
//     descendant too, with its own session id, and is not what the pane shows;
//   - that pid was reused: the recorded start time must be the live one's;
//   - the transcript is not a file under <home>/projects named <session>.jsonl.
//     The map is data, not a path to follow anywhere it points.
func (e paneMapEntry) valid(p pane, claude int, procs map[int]procInfo, projects string) bool {
	if e.Runtime != "claude" || e.PaneID != p.ID || claude <= 0 {
		return false
	}
	root, ok := procs[p.PID]
	if !ok || e.TmuxPID == "" || e.TmuxPID != strconv.Itoa(root.ppid) {
		return false
	}
	c, ok := procs[claude]
	if !ok || e.ClaudePID != strconv.Itoa(claude) || e.ClaudeStart == "" || e.ClaudeStart != c.start {
		return false
	}
	if !sessionIDPattern.MatchString(e.SessionID) || !filepath.IsAbs(e.Transcript) ||
		filepath.Base(e.Transcript) != e.SessionID+".jsonl" {
		return false
	}
	rel, err := filepath.Rel(projects, filepath.Clean(e.Transcript))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return fileExists(e.Transcript)
}

// Locate maps each pane that runs Claude Code to its ClaudePane, keyed by the
// pane's session (one pane per session: each session's active pane).
func (l ClaudeLocator) Locate(panes []pane) map[string]ClaudePane {
	byPane := l.LocatePanes(panes)
	out := map[string]ClaudePane{}
	for _, p := range panes {
		if cp, ok := byPane[p.ID]; ok {
			out[p.Session] = cp
		}
	}
	return out
}

// LocatePanes maps each pane that runs Claude Code to its ClaudePane, keyed by
// the PANE id — so any number of panes of one session (one per window) each
// resolve to their own Claude Code process and so their own transcript.
//
// 🔴 TWO PANES NEVER SHARE A GUESSED TRANSCRIPT. A CLI that named its own
// session (its sessions file) is resolved exactly, however many share a cwd. A
// CLI that did not is GUESSED from its cwd (resolve's fallback), and two of
// those in one cwd guess the same file: whichever wrote last. Showing that one
// conversation under both windows would put the wrong transcript above a
// composer that sends to the right pane — so a guess that any other pane also
// resolves to is dropped, and that window opens in the Terminal instead.
func (l ClaudeLocator) LocatePanes(panes []pane) map[string]ClaudePane {
	out := map[string]ClaudePane{}
	if len(panes) == 0 {
		return out
	}
	procs, children := l.table()
	// Read every sessions file at most once per lookup, and only when a pane
	// needs it (a parked job, or a pane-map session's busy flag).
	var live *liveSessions
	lives := func() liveSessions {
		if live == nil {
			ls := l.liveSessions(procs)
			live = &ls
		}
		return *live
	}
	claims := map[string]int{}
	for _, p := range panes {
		pid := findClaude(p.PID, procs, children)
		if pid == 0 {
			continue
		}
		if path, gen, ok := l.fromPaneMap(p, pid, procs); ok {
			st := statusOf(lives().byID[gen])
			out[p.ID] = ClaudePane{Pane: p.ID, PID: pid, Transcript: path, Gen: gen, Busy: st.status == mirror.StatusBusy,
				Status: st.status, StatusSince: st.since, Source: SourcePaneMap}
			claims[path]++
			continue
		}
		path, gen, st, guess, ok := l.resolve(pid, lives)
		if !ok {
			continue
		}
		src := SourceSessions
		if guess {
			src = SourceGuessed
		}
		out[p.ID] = ClaudePane{Pane: p.ID, PID: pid, Transcript: path, Gen: gen, Busy: st.status == mirror.StatusBusy,
			Status: st.status, StatusSince: st.since, Source: src}
		claims[path]++
	}
	for id, cp := range out {
		if cp.Source == SourceGuessed && claims[cp.Transcript] > 1 {
			delete(out, id)
		}
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
	cmd := exec.CommandContext(ctx, t.bin(), t.query("list-panes", "-a", "-F",
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

// Paste implements ClaudeBackend: PastePane into the Claude pane's id.
func (t Tmux) Paste(ctx context.Context, p ClaudePane, text string) error {
	return t.PastePane(ctx, p.Pane, text)
}

// ActivePane implements PaneBackend: session's active pane id, from the same
// `list-panes` read ClaudePanes resolves a Claude pane with — so for a session
// whose pane runs Claude Code the two name the SAME pane.
func (t Tmux) ActivePane(ctx context.Context, session string) (string, bool) {
	// activePanes keeps only the panes of the sessions it is given, matched on
	// the whole name, and a session has one active pane.
	if panes := t.activePanes(ctx, []string{session}); len(panes) == 1 {
		return panes[0].ID, true
	}
	return "", false
}

// PastePane implements PaneBackend — and is the ONE paste this host has (Paste
// calls it) — the way ccd hands a provisioned agent its prompt: `load-buffer`
// from STDIN into a uniquely named buffer (never argv — visible in the process
// table and parsed by tmux), `paste-buffer -p -d` into the PANE id (buffer
// deleted after), then Enter — itself a pasted newline, see the end of the body.
//
// `-p` brackets the paste ONLY for a program that asked for bracketed paste
// (zsh and bash 5.1+ do by default, as does Claude Code): there the whole text,
// newlines included, arrives as one paste and the Enter submits it once. A
// program that did not ask gets the bytes as typed, so every newline in the
// text is an Enter of its own (measured: tmux_integration_test.go).
//
// The text must already be mirror.SanitizePaste'd: a control character inside
// a bracketed paste can end it, and what follows is then TYPED.
func (t Tmux) PastePane(ctx context.Context, paneID, text string) error {
	if !strings.HasPrefix(paneID, "%") {
		return fmt.Errorf("muster-mirror: %q is not a tmux pane id", paneID)
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
	if err := run(nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", paneID); err != nil {
		_ = run(nil, "delete-buffer", "-b", buf)
		return err
	}
	select {
	case <-time.After(enterDelay):
	case <-ctx.Done():
		return errors.New("muster-mirror: the message was pasted but not submitted (cancelled)")
	}
	// 🔴 THE ENTER IS A PASTED NEWLINE, NOT `send-keys`. tmux refuses
	// `send-keys` with "client is read-only" when the client it resolves for
	// the target is a read-only one — and muster's own Terminal view attaches
	// exactly that (`attach -r`), so with that page open the text landed and
	// the Enter never did. `paste-buffer` has no such check. Without `-p` the
	// buffer is written unbracketed and tmux turns its LF into CR: the same
	// byte the Enter key sends.
	if err := run([]byte("\n"), "load-buffer", "-b", buf, "-"); err != nil {
		return fmt.Errorf("muster-mirror: the message was pasted but not submitted: %w", err)
	}
	if err := run(nil, "paste-buffer", "-d", "-b", buf, "-t", paneID); err != nil {
		_ = run(nil, "delete-buffer", "-b", buf)
		return fmt.Errorf("muster-mirror: the message was pasted but not submitted: %w", err)
	}
	return nil
}
