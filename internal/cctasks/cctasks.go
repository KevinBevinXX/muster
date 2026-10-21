// Package cctasks counts what a Claude Code CLI has running beside its turn:
// its background shells, from the CLI's own process tree, and — with a
// subagent count its caller reads from the transcript — the work rule that
// turns both, and the CLI's own session status, into what an agent card may
// claim (docs/design/agent-cards.md (a), "The work rule").
//
// 🔴 IT NEVER CLAIMS MORE THAN IT KNOWS. A count is exact only while no
// foreground command can be running; whenever one could be (any running turn)
// the shell count is a lower bound, "1+ shell" (operator decision 3), because
// a foreground Bash call — or a running subagent's — looks exactly like a
// background one in the process tree. And with no status at all (no live
// sessions file) there is no Work: the card shows nothing rather than a stale
// or guessed number.
//
// The layout relied on, measured on Claude Code 2.1.289 (one host, one sample
// each — the design's Phase 0 spike is what widens it):
//
//   - every Bash tool call, background or foreground, runs as a descendant of
//     the CLI with fd 1 and fd 2 on its task output file,
//     <tmp>/claude-<uid>/<slug>/<session>/tasks/b<8 base-36>.output;
//   - a Monitor task is a DIRECT child of the CLI whose argv names the CLI's
//     shell snapshot (<config>/shell-snapshots/…), with fd 0 /dev/null and
//     fds 1 and 2 sockets — no task output file;
//   - a subagent's own symlink in that directory (a<16 hex>.output) is not a
//     process and is never counted;
//   - a daemon a task left behind (`nohup … &`) is reparented away from the
//     CLI, so it is not a descendant and not counted — the task is done.
package cctasks

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Claude Code's session statuses, as its sessions file writes them RAW —
// before muster's own mapping folds "shell" into idle (mirror.NormalizeStatus).
const (
	StatusIdle    = "idle"
	StatusShell   = "shell"
	StatusBusy    = "busy"
	StatusWaiting = "waiting"
)

// Work is what a CLI has running now. ShellsAtLeast: Shells is a lower bound
// ("1+ shell"), never an exact count.
type Work struct {
	Subagents     int
	Shells        int
	ShellsAtLeast bool
}

// Shells is what the process tree says: N distinct background-shell task ids,
// and whether a Monitor was seen (which has no task id to count).
type Shells struct {
	N       int
	Monitor bool
}

// taskFilePattern is a Bash task's output file, as a descendant's fd 1 or 2
// links it: only the suffix is matched, never the root, so a
// CLAUDE_CODE_TMPDIR override still counts.
var taskFilePattern = regexp.MustCompile(`/tasks/(b[0-9a-z]{8})\.output$`)

// maxWalk bounds the descendant walk, so a pathological tree cannot stall a
// refresh.
const maxWalk = 4096

// CountShells is the shells CLI process cli has running, from the proc
// filesystem at procRoot ("" = /proc): the distinct Bash task ids among its
// LIVE descendants' fd 1 and fd 2, and whether one of its direct children is a
// Monitor. children is the caller's process table's child lists (sorted);
// argv is a process's argv from that table. configDir is Claude Code's config
// dir, whose shell-snapshots/ a Monitor's argv names.
func CountShells(procRoot string, cli int, children map[int][]int, argv func(int) []string, configDir string) Shells {
	if procRoot == "" {
		procRoot = "/proc"
	}
	link := func(pid, fd int) string {
		l, _ := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "fd", strconv.Itoa(fd)))
		return l
	}
	snapshots := filepath.Join(configDir, "shell-snapshots") + string(filepath.Separator)
	var out Shells
	ids := map[string]bool{}
	direct := map[int]bool{}
	for _, c := range children[cli] {
		direct[c] = true
	}
	queue, seen := append([]int(nil), children[cli]...), map[int]bool{cli: true}
	for len(queue) > 0 && len(seen) < maxWalk {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		queue = append(queue, children[pid]...)
		task := false
		for _, fd := range []int{1, 2} {
			if m := taskFilePattern.FindStringSubmatch(link(pid, fd)); m != nil {
				ids[m[1]], task = true, true
			}
		}
		if !task && direct[pid] && isMonitor(argv(pid), snapshots, link(pid, 0), link(pid, 1), link(pid, 2)) {
			out.Monitor = true
		}
	}
	out.N = len(ids)
	return out
}

// isMonitor is the Monitor signature (the package comment): its argv names
// the CLI's shell snapshot, stdin is /dev/null and stdout and stderr are
// sockets. A socket-stdout child without the snapshot argv (an MCP server's
// shape) is not one.
func isMonitor(argv []string, snapshots, fd0, fd1, fd2 string) bool {
	named := false
	for _, a := range argv {
		if strings.Contains(a, snapshots) {
			named = true
			break
		}
	}
	return named && fd0 == "/dev/null" && strings.HasPrefix(fd1, "socket:[") && strings.HasPrefix(fd2, "socket:[")
}

// Decide is THE WORK RULE: what a card may claim from the CLI's RAW session
// status, its process-tree shells, and its running subagents (asked only when
// the status needs them — a transcript read). ok is false when there is
// nothing it may claim at all: no status (no live sessions file) or one it
// does not know.
//
//   - idle: nothing runs — the CLI says so (no agent task, no Bash task).
//   - shell: no subagent runs, and at least one Bash task does. The count is
//     exact unless a Monitor was seen (its signature rests on one sample) or
//     the tree shows none (unreadable, or a changed layout): then "1+".
//   - busy, waiting: a turn may be running, so any shell seen is "1+", never a
//     number; the subagents are the transcript's running count.
func Decide(status string, sh Shells, subagents func() int) (Work, bool) {
	switch status {
	case StatusIdle:
		return Work{}, true
	case StatusShell:
		if sh.Monitor || sh.N == 0 {
			return Work{Shells: max(sh.N, 1), ShellsAtLeast: true}, true
		}
		return Work{Shells: sh.N}, true
	case StatusBusy, StatusWaiting:
		w := Work{Subagents: max(subagents(), 0)}
		if sh.N > 0 || sh.Monitor {
			w.Shells, w.ShellsAtLeast = 1, true
		}
		return w, true
	}
	return Work{}, false
}
