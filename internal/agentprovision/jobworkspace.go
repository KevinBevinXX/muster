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

// THE MACHINE JOB API'S WORKSPACE OPERATIONS (internal/api jobs.go): give a
// commit job a git worktree of its own, isolated from that commit's agent
// configuration; read a file back out of the job's directory after its turn;
// delete that file once read; remove the worktree after the job. Each runs a
// command inside the agent's live instance through the driver's exec, exactly
// as the terminal does, so a driver that cannot exec refuses them with
// provision.ErrUnsupported.
//
// 🔴 NO COMMAND INTERPOLATES A VALUE INTO SHELL TEXT. The script is a
// constant and every caller-supplied value travels as a positional argument
// ($1, $2, …), so a sha or a path cannot become shell syntax. Every value is
// also validated here (jobs.ValidSHA, jobs.ValidResultPath — the same
// predicates the HTTP handler applies — and a positive job id), before any
// exec.

// JobOverlayRootFiles are the files at a job worktree's root that are replaced
// by the repository's default branch's (or removed, when the default branch
// has none). Together with every `.claude` directory (jobOverlayScript) they
// are what the overlay covers, and nothing else is.
//
//   - CLAUDE.md, AGENTS.md: the instructions the CLI loads at launch from the
//     project root.
//   - .mcp.json: the project's MCP servers. The CLI STARTS each server's
//     command, so it is code the commit would otherwise run.
var JobOverlayRootFiles = []string{"CLAUDE.md", "AGENTS.md", ".mcp.json"}

// jobOverlayScript prepares job worktree $2 of the checkout $1 at commit $3,
// the remaining arguments being JobOverlayRootFiles:
//
//  1. resolve the default branch, refs/remotes/origin/HEAD (asking origin with
//     `git remote set-head --auto` only when the ref is missing), and fail
//     with NoDefaultBranchMessage when it cannot be resolved: there is then
//     no trusted configuration to give the job;
//  2. fetch the commit from origin only when it is not already present;
//  3. refuse (SymlinkOverlayMessage) when the default branch carries a symlink
//     among the overlaid paths: a symlink restored from it could point at a
//     file the commit controls;
//  4. `git worktree add --detach` the commit at $2 — the agent's own checkout,
//     its HEAD, branch and working tree, is not touched;
//  5. THE OVERLAY: remove every `.claude` directory (or file) anywhere in the
//     worktree, then restore every `.claude` the default branch has, at the
//     same paths; remove each root file in JobOverlayRootFiles, then restore
//     the default branch's copy where it has one.
//
// 🔴 WHY `.claude` AT EVERY DEPTH, NOT ONLY AT THE ROOT. The root `.claude/`
// holds the project settings (hooks: commands the CLI runs), agents and
// skills; and the CLI also discovers `.claude/skills` in SUBDIRECTORIES as a
// session works in them (measured in the CLI bundle: its own guidance names
// "a subproject directory (e.g. `ios/.claude/skills/…`)"), and a skill's or
// an agent's frontmatter can declare hooks. Replacing only the root would let
// a commit add `sub/.claude/…` and have it loaded.
//
// ⚠ NOT OVERLAID, AND SO THE COMMIT'S: every other file — including
// CLAUDE.md / AGENTS.md files below the root (instructions the CLI reads as it
// works in those directories; text, not commands) and anything the turn
// chooses to run from the tree (a build script, a test). The overlay keeps the
// COMMIT's configuration from configuring the reviewer; it does not make the
// commit's code safe to execute.
const jobOverlayScript = `main=$1 wt=$2 sha=$3
shift 3
cd "$main" || exit 1
def=$(git rev-parse --verify --quiet 'refs/remotes/origin/HEAD^{commit}') ||
	{ git remote set-head origin --auto >/dev/null 2>&1 &&
		def=$(git rev-parse --verify --quiet 'refs/remotes/origin/HEAD^{commit}'); } ||
	{ echo "` + NoDefaultBranchMessage + `" >&2; exit 4; }
git cat-file -e "$sha^{commit}" 2>/dev/null || git fetch --quiet origin "$sha" || exit 1
links=$(git ls-tree -r "$def" | awk -F '\t' '$1 ~ /^120000 / {print $2}' | grep -E '(^|/)\.claude(/|$)')
for p in "$@"; do
	case $(git ls-tree "$def" -- "$p") in
	120000\ *) links="$links
$p" ;;
	esac
done
if [ -n "$links" ]; then
	echo "` + SymlinkOverlayMessage + `" >&2
	printf '%s\n' "$links" | head -n 20 >&2
	exit 5
fi
mkdir -p "${wt%/*}" || exit 1
git worktree add --quiet --detach "$wt" "$sha" || exit 1
cd "$wt" || exit 1
find . -name .git -prune -o -name .claude -prune -exec rm -rf {} + || exit 1
for p in "$@"; do
	rm -rf ./"$p" || exit 1
done
git ls-tree -r --name-only "$def" | grep -E '(^|/)\.claude(/|$)' |
	sed -E 's#^((.*/)?\.claude)(/.*)?$#\1#' | sort -u |
	while IFS= read -r p; do
		git --literal-pathspecs checkout --quiet "$def" -- "$p" || exit 1
	done || exit 1
for p in "$@"; do
	if git cat-file -e "$def:$p" 2>/dev/null; then
		git --literal-pathspecs checkout --quiet "$def" -- "$p" || exit 1
	fi
done`

// NoDefaultBranchMessage opens the error of a job whose repository's default
// branch could not be resolved.
const NoDefaultBranchMessage = "the repository's default branch (origin/HEAD) could not be resolved, so there is no trusted agent configuration to give the job's worktree; nothing was run"

// SymlinkOverlayMessage opens the error of a job refused because the default
// branch carries a symlink among the overlaid paths; the paths follow it.
const SymlinkOverlayMessage = "the repository's default branch has a symlink among the agent-configuration paths a job's worktree takes from it, which could point at a file the job's commit controls; nothing was run"

// jobRemoveScript removes job worktree $2 of the checkout $1: `git worktree
// remove --force --force` (twice: a locked worktree too), then the directory
// itself if git left it (it was not a registered worktree, or git refused),
// then `git worktree prune`. It fails only when the directory is still there.
const jobRemoveScript = `cd "$1" 2>/dev/null && git worktree remove --force --force "$2" >/dev/null 2>&1
rm -rf "$2"
cd "$1" 2>/dev/null && git worktree prune >/dev/null 2>&1
if [ -e "$2" ] || [ -L "$2" ]; then
	echo "the job's worktree $2 could not be removed" >&2
	exit 1
fi`

// readScript prints at most $2 bytes of the file $1. It follows symlinks: the
// path is checked lexically (jobs.ValidResultPath), not resolved, so a symlink
// in the workspace can point the read elsewhere in the instance.
const readScript = `head -c "$2" -- "$1"`

// jobDirs is ag's working directory and, for a positive jobID, that job's
// worktree, refusing an agent with no repository when a worktree is asked for.
func jobDirs(ag agents.Agent, jobID int64) (main, wt string, err error) {
	if strings.TrimSpace(ag.Repo) == "" {
		return "", "", fmt.Errorf("agentprovision: agent %q has no repository, so a job cannot have a worktree of it", ag.Name)
	}
	if jobID <= 0 {
		return "", "", fmt.Errorf("agentprovision: job id %d is not positive", jobID)
	}
	main, err = agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return "", "", err
	}
	return main, agents.JobWorktreeDir(main, jobID), nil
}

// PrepareJobWorktree creates job jobID's worktree of ag's repository at sha,
// with the overlay (jobOverlayScript), and returns its absolute path in the
// instance. ag's own checkout is not moved. On an error a partly made
// worktree may be left; RemoveJobWorktree removes it either way.
func (a *Adapter) PrepareJobWorktree(ctx context.Context, ag agents.Agent, jobID int64, sha string) (string, error) {
	if !jobs.ValidSHA(sha) {
		return "", fmt.Errorf("agentprovision: %q is not a full commit id (40 lowercase hex)", sha)
	}
	main, wt, err := jobDirs(ag, jobID)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := append([]string{"sh", "-c", jobOverlayScript, "muster-job-worktree", main, wt, sha}, JobOverlayRootFiles...)
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return "", fmt.Errorf("create the worktree %s at %s: %w: %s", wt, sha, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return wt, nil
}

// RemoveJobWorktree removes job jobID's worktree of ag's repository
// (jobRemoveScript). A worktree that is not there is not an error.
func (a *Adapter) RemoveJobWorktree(ctx context.Context, ag agents.Agent, jobID int64) error {
	main, wt, err := jobDirs(ag, jobID)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := []string{"sh", "-c", jobRemoveScript, "muster-job-worktree-remove", main, wt}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return fmt.Errorf("remove the worktree %s: %w: %s", wt, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return nil
}

// ErrFileTooLarge is ReadWorkspaceFile's answer for a file over its limit.
var ErrFileTooLarge = errors.New("the file is larger than the limit")

// ReadWorkspaceFile returns the contents of rel byte for byte, refusing a file
// over max bytes with ErrFileTooLarge rather than truncating it. rel is
// relative to job jobID's worktree for a positive jobID (PrepareJobWorktree),
// and to the directory ag's CLI works in (agentspec.ClaudeCodeWorkingDir) for
// zero.
func (a *Adapter) ReadWorkspaceFile(ctx context.Context, ag agents.Agent, jobID int64, rel string, max int) ([]byte, error) {
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
	if jobID != 0 {
		if _, dir, err = jobDirs(ag, jobID); err != nil {
			return nil, err
		}
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

// removeScript deletes the file $1 — the same file as $3, relative to the
// checkout in $2 — only when it is certain the repository does not track it.
// A missing file is not an error; rm removes a symlink itself, never its target.
//
// 🔴 FAIL CLOSED: ONLY `ls-files --error-unmatch` EXITING EXACTLY 1 ("did not
// match") MEANS UNTRACKED. Exit 0 means tracked: it prints RemoveTrackedMarker
// and deletes nothing. ANY other status — git missing (127), a refusal such as
// "dubious ownership" or another fatal error (128) — leaves the file in place
// and fails with a message, which the runner records as a note: treating "git
// could not answer" as "untracked" would delete a tracked file. The one case
// that skips git is a working directory with no .git at all (an agent with no
// repository), where nothing can be tracked.
//
// 🔴 --literal-pathspecs: a result_path is a file name, and without it a name
// such as ":(top)…" or "*.md" would be read as pathspec magic or a glob and
// could match a DIFFERENT, tracked file — or none.
const removeScript = `if [ -e "$2/.git" ]; then
	git --literal-pathspecs -C "$2" ls-files --error-unmatch -- "$3" >/dev/null 2>&1
	rc=$?
	if [ "$rc" -eq 0 ]; then
		echo ` + RemoveTrackedMarker + `
		exit 0
	fi
	if [ "$rc" -ne 1 ]; then
		echo "could not tell whether $3 is tracked (git ls-files exited $rc), so it was left in place" >&2
		exit 3
	fi
fi
rm -f -- "$1"`

// RemoveTrackedMarker is what removeScript prints when it leaves a tracked file.
const RemoveTrackedMarker = "muster-job-remove:tracked"

// RemoveUntrackedWorkspaceFile deletes rel — a path relative to the directory
// ag's CLI works in, validated as ReadWorkspaceFile validates it — unless the
// repository checked out there tracks it. removed reports whether it deleted
// (or found nothing to delete); false with a nil error means the file is
// tracked and was left in place. It is for a job WITHOUT a commit, whose turn
// ran in the agent's own working directory; a commit job's whole worktree is
// removed instead (RemoveJobWorktree).
func (a *Adapter) RemoveUntrackedWorkspaceFile(ctx context.Context, ag agents.Agent, rel string) (removed bool, err error) {
	if !jobs.ValidResultPath(rel) {
		return false, fmt.Errorf("agentprovision: %q is not a relative path inside the workspace", rel)
	}
	dir, err := agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return false, err
	}
	full := path.Join(dir, rel)
	var out bytes.Buffer
	cmd := []string{"sh", "-c", removeScript, "muster-job-remove", full, dir, rel}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return false, fmt.Errorf("remove %s: %w: %s", full, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return strings.TrimSpace(out.String()) != RemoveTrackedMarker, nil
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
