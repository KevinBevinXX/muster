package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/ZacxDev/muster/internal/agents"
)

// A MACHINE JOB'S LANE WORKS IN THE JOB'S OWN WORKTREE (X-Muster-CC-Workdir).
//
// muster runs a job with a commit in a git worktree of that commit, outside the
// agent's checkout, whose agent configuration (`.claude/`, the root CLAUDE.md /
// AGENTS.md, `.mcp.json`) it has replaced with the default branch's
// (internal/agentprovision jobworkspace.go). The job's turn names a fresh
// session (X-Muster-CC-Session) and that worktree; ccd starts the session's
// lane CLI there — a new CLI in a new directory, so a fresh conversation whose
// project is the worktree.
//
// 🔴 ccd TAKES ONLY A JOB WORKTREE OF ITS OWN WORKSPACE. The directory must be
// exactly agents.JobWorktreeDir(<workspace>, <id>), exist, and be a directory
// rather than a symlink. Anything else is refused before a CLI starts.

// checkJobDir validates a lane's requested working directory. A nil failure
// means the lane may start there.
//
// ⚠ ccd WRITES NO TRUST FOR IT, AND NEEDS NONE: the CLI treats a git worktree
// of a trusted checkout as trusted. MEASURED on the pinned CLI (2.1.296, in the
// image, with ccd writing nothing to .claude.json): a lane started in a
// worktree of the checkout `ccd seed` trusted reached its prompt and ran the
// worktree's project SessionStart hook, while a plain directory at the same
// kind of path stopped at the trust dialog. A CLI that stopped honouring that
// fails closed — its dialog runs no hook and the turn is not_ready — and the
// image's real-CLI test (internal/agentprovision realcli_test.go) fails.
func (l *lanes) checkJobDir(dir string) *failure {
	if !agents.IsJobWorktreeDir(l.workspace, dir) {
		return &failure{Status: http.StatusBadRequest, Type: failInvalidInput,
			Message: fmt.Sprintf("%q is not a job worktree of this workspace (%s/<job id>)", dir, agents.JobWorktreesDir(l.workspace))}
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return paneNotReady(fmt.Sprintf("the job worktree %s does not exist", dir))
	case err != nil:
		return paneNotReady(fmt.Sprintf("the job worktree %s: %v", dir, err))
	case !fi.IsDir():
		return &failure{Status: http.StatusBadRequest, Type: failInvalidInput,
			Message: fmt.Sprintf("the job worktree %s is not a directory (%s)", dir, fi.Mode().Type())}
	}
	return nil
}
