package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE MACHINE JOB API'S TWO WORKSPACE OPERATIONS (internal/api jobs.go): move a
// claude-code agent's checkout to a commit before a job's turn, and read a file
// back out of its workspace after it. Both run a command inside the agent's
// live instance through the driver's exec, exactly as the terminal does, so a
// driver that cannot exec refuses them with provision.ErrUnsupported.
//
// 🔴 NEITHER COMMAND INTERPOLATES A VALUE INTO SHELL TEXT. The script is a
// constant and every caller-supplied value travels as a positional argument
// ($1, $2), so a sha or a path cannot become shell syntax. Both values are also
// validated here (jobs.ValidSHA, jobs.ValidResultPath — the same predicates
// the HTTP handler applies), before any exec.

// checkoutScript moves the checkout in $1 to commit $2, detached: fetched from
// origin first only when the commit is not already present.
//
// 🔴 A DIRTY TREE IS REFUSED BEFORE ANYTHING MOVES. `git checkout --detach`
// carries uncommitted edits and untracked files that do not conflict onto the
// new commit, silently, so a job would run on "sha + whatever was lying
// around". Any `git status --porcelain` output (modified, staged or untracked,
// not ignored) fails the job with DirtyTreeMessage; nothing is reset, cleaned or
// discarded. A refusal by `git checkout` itself still fails the job the same way.
const checkoutScript = `cd "$1" || exit 1
dirty=$(git status --porcelain) || exit 1
if [ -n "$dirty" ]; then
	printf '%s\n' "` + DirtyTreeMessage + `" >&2
	printf '%s\n' "$dirty" | head -n 20 >&2
	exit 3
fi
{ git cat-file -e "$2^{commit}" 2>/dev/null || git fetch --quiet origin "$2"; } && git checkout --quiet --detach "$2"`

// DirtyTreeMessage opens the error of a checkout refused for a dirty tree; the
// first lines of `git status --porcelain` follow it.
const DirtyTreeMessage = "the agent's working tree is dirty (uncommitted changes or untracked files), so it was not moved to the job's commit; nothing was discarded"

// readScript prints at most $2 bytes of the file $1. It follows symlinks: the
// path is checked lexically (jobs.ValidResultPath), not resolved, so a symlink
// in the workspace can point the read elsewhere in the instance.
const readScript = `head -c "$2" -- "$1"`

// CheckoutCommit moves ag's repository checkout to sha (detached HEAD). It
// refuses an agent with no repository, since there is no checkout to move.
func (a *Adapter) CheckoutCommit(ctx context.Context, ag agents.Agent, sha string) error {
	if !jobs.ValidSHA(sha) {
		return fmt.Errorf("agentprovision: %q is not a full commit id (40 lowercase hex)", sha)
	}
	if strings.TrimSpace(ag.Repo) == "" {
		return fmt.Errorf("agentprovision: agent %q has no repository, so there is no checkout to move to %s", ag.Name, sha)
	}
	dir, err := agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := []string{"sh", "-c", checkoutScript, "muster-job-checkout", dir, sha}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return fmt.Errorf("check out %s in %s: %w: %s", sha, dir, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return nil
}

// ErrFileTooLarge is ReadWorkspaceFile's answer for a file over its limit.
var ErrFileTooLarge = errors.New("the file is larger than the limit")

// ReadWorkspaceFile returns the contents of rel — a path relative to the
// directory ag's CLI works in (agentspec.ClaudeCodeWorkingDir) — byte for byte,
// refusing a file over max bytes with ErrFileTooLarge rather than truncating
// it.
func (a *Adapter) ReadWorkspaceFile(ctx context.Context, ag agents.Agent, rel string, max int) ([]byte, error) {
	if !jobs.ValidResultPath(rel) {
		return nil, fmt.Errorf("agentprovision: %q is not a relative path inside the workspace", rel)
	}
	if max <= 0 {
		return nil, fmt.Errorf("agentprovision: a read limit of %d bytes", max)
	}
	dir, err := agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return nil, err
	}
	full := path.Join(dir, rel)
	var out, errb bytes.Buffer
	// One byte past the limit: present means the file is over it.
	cmd := []string{"sh", "-c", readScript, "muster-job-read", full, strconv.Itoa(max + 1)}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &errb); err != nil {
		return nil, fmt.Errorf("read %s: %w: %s", full, err, strings.TrimSpace(capString(errb.String(), 1024)))
	}
	if out.Len() > max {
		return nil, fmt.Errorf("read %s: %w (%d bytes)", full, ErrFileTooLarge, max)
	}
	return out.Bytes(), nil
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
