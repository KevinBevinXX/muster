package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WHICH CLI FIRED A HOOK: the primary's own process, or a `claude` running
// INSIDE it.
//
// A hook with no ?lane= is the primary's (lanes.go) — but that is a claim about
// the environment the hook ran in, and a nested `claude` that the chief runs in
// its own shell (a Bash tool call in /data/workspace) inherits that environment:
// CLAUDE_CONFIG_DIR, the settings that register `ccd hook`, and no CCD_LANE. Its
// SessionStart arrived as the primary's, and its session id was RECORDED as the
// primary's (primaryRecordFile), so the next primary restart resumed the nested
// conversation instead of the chief's own.
//
// An environment marker cannot tell the two apart — whatever ccd sets in the
// primary's environment the nested CLI inherits. The PROCESS TREE can: ccd
// started the primary in its tmux pane, so the pane's process IS the primary CLI
// (tmuxTerminal.Start never lets a shell sit in between), and the CLI that fired
// a hook is the hook's nearest ancestor running the same program. For the
// primary's own hook that is the pane's process; for a nested CLI's it is the
// nested CLI, which is met first on the way up.
//
// ⚠ IT GATES THE RECORD ONLY, AND THE FAILURE DIRECTION IS DELIBERATE. A hook
// this cannot place (no pid, an unreadable /proc, no pane yet) is not recorded,
// and the primary then starts as it did before any record existed
// (supervisor.argv's fallback) — never resuming a conversation it cannot prove
// is its own. Every other effect of the hook on the primary's state is
// unchanged.

// procRoot is /proc; tests point it at a fixture tree.
var procRoot = "/proc"

// maxProcWalk bounds the ancestor walk (a pid cycle in a broken /proc, or a
// very deep tree, ends it as "not the primary's").
const maxProcWalk = 64

// procParent is pid's parent pid, read from /proc/<pid>/stat. The comm field is
// parenthesised and may itself hold spaces and parentheses, so the fields are
// read after its LAST ')'.
func procParent(root string, pid int) (int, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("/proc/%d/stat has no comm field", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat is truncated", pid)
	}
	return strconv.Atoi(f[1])
}

// procExe is the program pid runs (/proc/<pid>/exe).
func procExe(root string, pid int) (string, error) {
	return os.Readlink(filepath.Join(root, strconv.Itoa(pid), "exe"))
}

// hookFiredBy reports whether cliPID is the CLI that fired the hook running as
// hookPID: walking up from the hook's PARENT (the hook is `ccd hook`, a
// different program — and in the test suite the same binary as the fake CLI),
// cliPID is reached before any other process running cliPID's program.
func hookFiredBy(root string, cliPID, hookPID int) bool {
	if cliPID <= 1 || hookPID <= 1 {
		return false
	}
	exe, err := procExe(root, cliPID)
	if err != nil || exe == "" {
		return false
	}
	pid, err := procParent(root, hookPID)
	for n := 0; err == nil && pid > 1 && n < maxProcWalk; n++ {
		if pid == cliPID {
			return true
		}
		if e, err := procExe(root, pid); err == nil && e == exe {
			return false // another instance of the CLI is nearer: a nested one
		}
		pid, err = procParent(root, pid)
	}
	return false
}
