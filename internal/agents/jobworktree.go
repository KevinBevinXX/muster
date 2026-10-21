package agents

import (
	"path"
	"regexp"
	"strconv"
)

// WHERE A JOB'S WORKTREE LIVES, AS BOTH SIDES OF THE WIRE DERIVE IT.
//
// A machine job with a commit (internal/api jobs.go) runs in a git worktree of
// its own, never in the agent's checkout: muster creates it through the
// driver's exec (internal/agentprovision jobworkspace.go), then names it on the
// turn (HeaderCCWorkdir), and ccd starts that session's CLI in it
// (cmd/ccd lanes.go). Both sides compute the place from the agent's working
// directory with the functions below, so ccd can refuse any other directory
// without being told what the agent's layout is.
//
// 🔴 OUTSIDE THE CHECKOUT, ON THE SAME VOLUME. The worktrees are a SIBLING of
// the working directory (<parent>/.muster-jobs/<job id>), so a job never
// appears inside the checkout the agent's own sessions work in, and the
// worktree is on the agent's persistent volume, where git can create it.

// JobWorktreesDirName is the directory, beside the agent's working directory,
// that holds its jobs' worktrees.
const JobWorktreesDirName = ".muster-jobs"

// JobWorktreesDir is the directory holding the worktrees of the jobs of an
// agent whose CLI works in workingDir.
func JobWorktreesDir(workingDir string) string {
	return path.Join(path.Dir(path.Clean(workingDir)), JobWorktreesDirName)
}

// JobWorktreeDir is job jobID's worktree for an agent whose CLI works in
// workingDir.
func JobWorktreeDir(workingDir string, jobID int64) string {
	return path.Join(JobWorktreesDir(workingDir), strconv.FormatInt(jobID, 10))
}

// jobIDElement is the last element of a job worktree's path: a positive job id.
var jobIDElement = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// IsJobWorktreeDir reports whether dir is, as a string, a job worktree of an
// agent whose CLI works in workingDir: exactly JobWorktreeDir(workingDir, n)
// for some positive n. It checks the string only; whether the directory exists
// is the caller's to check.
func IsJobWorktreeDir(workingDir, dir string) bool {
	if dir == "" || path.Clean(dir) != dir || !path.IsAbs(dir) {
		return false
	}
	return path.Dir(dir) == JobWorktreesDir(workingDir) && jobIDElement.MatchString(path.Base(dir))
}
