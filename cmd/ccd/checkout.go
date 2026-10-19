package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The agent's repository, checked out BEFORE the CLI starts.
//
// muster gives a claude-code agent dispatched with a repository three variables
// (internal/agentspec.claudeCodeEnv): CCD_REPO (owner/name), CCD_REPO_BRANCH
// (optional) and CCD_WORKSPACE — the CLI's working directory, set to the
// checkout. So the checkout IS the workspace, by construction: ccd clones into
// CCD_WORKSPACE, and the CLI it then starts has the repository as its project —
// the repository's CLAUDE.md and .claude/ (skills, project settings) are the
// session's, and the workspace trust `ccd seed` wrote is for that directory.
//
// 🔴 THE CLI DOES NOT START UNTIL THE CHECKOUT EXISTS. A session started in an
// empty directory would take its first turn without the repository and nothing
// would say so. Until the clone succeeds:
//   - every turn is `503 not_ready` whose message says WHY (the clone's error,
//     and what the github connector needs) — muster's kickoff deliverer re-sends
//     not_ready for a while and then records that message as the agent's
//     kickoff error, which is what the operator sees on the card;
//   - /healthz stays 200 (the pod must not restart-loop while it waits for an
//     operator) and reports the state under `repo`;
//   - ccd retries the clone every checkoutRetry.
//
// 🔴 CREDENTIALS COME ONLY FROM THE `github` CONNECTOR. git reaches GitHub
// through muster's proxy once this agent holds an approved grant on it
// (connect.go keeps the routing in step); no token is ever placed in the pod.
// A private repository therefore cannot be cloned until that grant exists — and
// because the session that could ask for it has not started, ccd FILES THE
// REQUEST ITSELF, once per process, when a clone fails and the agent holds no
// grant at all (status `none`). It is the same request the session's
// connector_request tool files: the operator still approves it. ccd never
// re-asks after a denial or a revocation.
//
// An existing checkout (CCD_WORKSPACE/.git, on the agent's volume after a pod
// restart) is used as it is: never re-cloned, fetched or reset.

const (
	envRepo       = "CCD_REPO"
	envRepoBranch = "CCD_REPO_BRANCH"

	// githubConnector is the connector git reaches GitHub through.
	githubConnector = "github"

	// checkoutRetry is how often a failed clone is retried. ccd re-reads the
	// grants every gitSyncInterval, so a clone retried after an approval goes
	// through the proxy within checkoutRetry + gitSyncInterval.
	checkoutRetry = 30 * time.Second
	// checkoutTimeout bounds one clone.
	checkoutTimeout = 10 * time.Minute
)

// The states /healthz reports under `repo`.
const (
	checkoutCloning = "cloning"
	checkoutFailed  = "failed"
	checkoutReady   = "ready"
)

// githubCloneBase is where a repository is cloned from. A test points git at a
// local repository with url.<local>.insteadOf this prefix, exactly as ccd's own
// connector routing does with muster's proxy.
const githubCloneBase = "https://github.com/"

type checkoutState struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Dir    string `json:"dir"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type checkout struct {
	repo, branch, dir string
	// connectBase and token reach muster's connector routes; empty when the pod
	// is not offered the connector store.
	connectBase, token string
	retry              time.Duration
	timeout            time.Duration // one clone's bound; 0 is checkoutTimeout
	logf               func(string, ...any)

	mu        sync.Mutex
	st        checkoutState
	requested bool // the github request was filed by this process
	done      chan struct{}
}

// checkoutFromEnv is the checkout this pod was given, or nil when it was given
// no repository (the agent works in its plain workspace, as before).
func checkoutFromEnv(workspace string) (*checkout, error) {
	repo := strings.TrimSpace(os.Getenv(envRepo))
	if repo == "" {
		return nil, nil
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(owner, "-") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("ccd: %s=%q is not owner/name", envRepo, repo)
	}
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("ccd: %s is set, so CCD_WORKSPACE (the checkout) must be an absolute path, not %q", envRepo, workspace)
	}
	c := &checkout{
		repo: repo, branch: strings.TrimSpace(os.Getenv(envRepoBranch)), dir: filepath.Clean(workspace),
		retry: checkoutRetry, logf: log.Printf, done: make(chan struct{}),
	}
	if base, tok := strings.TrimRight(os.Getenv(envConnectURL), "/"), os.Getenv(envAgentToken); base != "" && tok != "" {
		c.connectBase, c.token = base, tok
	}
	c.st = checkoutState{Repo: c.repo, Branch: c.branch, Dir: c.dir, State: checkoutCloning}
	return c, nil
}

func (c *checkout) state() checkoutState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// ready reports whether the checkout exists.
func (c *checkout) ready() bool { return c.state().State == checkoutReady }

// wait blocks until the checkout exists (true) or ctx ends (false).
func (c *checkout) wait(ctx context.Context) bool {
	select {
	case <-c.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// notReady is the not_ready message a turn gets while there is no checkout.
func (c *checkout) notReady() string {
	st := c.state()
	if st.State == checkoutFailed {
		return "the Claude Code session has not started: it starts only once the repository is checked out, and " + st.Detail
	}
	return fmt.Sprintf("the Claude Code session has not started: the repository %s is being cloned into %s", st.Repo, st.Dir)
}

// run clones until it succeeds or ctx ends.
func (c *checkout) run(ctx context.Context) {
	for {
		err := c.attempt(ctx)
		if err == nil {
			c.mu.Lock()
			c.st.State, c.st.Detail = checkoutReady, ""
			c.mu.Unlock()
			close(c.done)
			return
		}
		if ctx.Err() != nil {
			return
		}
		detail := c.explain(ctx, err)
		c.mu.Lock()
		c.st.State, c.st.Detail = checkoutFailed, detail
		c.mu.Unlock()
		c.logf("ccd: checkout: %s; retrying in %s", detail, c.retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.retry):
		}
	}
}

// attempt makes the checkout exist: an existing one is used as it is; an empty
// (or absent) directory is cloned into; a directory holding anything else is
// refused rather than cloned over.
//
// 🔴 A CHECKOUT IS A .git WITH A COMMIT CHECKED OUT, NOT A .git. A clone that is
// killed (the timeout, ccd shutting down, the pod evicted) leaves a .git whose
// HEAD names no commit; treating that as ready would start the CLI in an empty
// tree. So: the clone goes into a sibling scratch directory and is moved into
// place (moveCheckout, .git last) only once `git rev-parse --verify HEAD`
// succeeds there — a kill leaves the workspace untouched (the scratch directory
// is replaced on the next attempt) — and an existing .git counts only when HEAD
// verifies too. One with
// no HEAD (an interrupted clone by an older ccd) is refused, not deleted: ccd
// never removes what it did not just create.
func (c *checkout) attempt(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(c.dir, ".git")); err == nil {
		if err := verifyHEAD(ctx, c.dir); err != nil {
			return fmt.Errorf("%s holds a .git with no commit checked out (an interrupted clone?); ccd does not "+
				"clone over it — remove %s to let it clone again", c.dir, filepath.Join(c.dir, ".git"))
		}
		c.logf("ccd: checkout: using the existing checkout of %s in %s", c.repo, c.dir)
		return nil
	}
	entries, err := os.ReadDir(c.dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("%s holds files but is not a git checkout; ccd does not clone over them", c.dir)
	}
	scratch := filepath.Join(filepath.Dir(c.dir), "."+filepath.Base(c.dir)+".ccd-clone")
	if err := os.RemoveAll(scratch); err != nil {
		return err
	}
	args := []string{"clone", "--quiet"}
	if c.branch != "" {
		args = append(args, "--branch="+c.branch)
	}
	args = append(args, "--", githubCloneBase+c.repo+".git", scratch)
	timeout := c.timeout
	if timeout <= 0 {
		timeout = checkoutTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	// The hang guard muster also sets on the pod: with nobody to answer, a
	// credential prompt must fail the clone, not block it.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := lastLines(stderr.String(), 2, 300); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	if err := verifyHEAD(ctx, scratch); err != nil {
		return fmt.Errorf("the clone of %s finished with no commit checked out: %v", c.repo, err)
	}
	if err := moveCheckout(scratch, c.dir); err != nil {
		return fmt.Errorf("moving the clone into %s: %w", c.dir, err)
	}
	c.logf("ccd: checkout: cloned %s into %s", c.repo, c.dir)
	return nil
}

// moveCheckout moves a verified clone's entries from scratch into dir, its
// .git LAST, then removes scratch.
//
// ⚠ dir itself is NOT replaced (no rename of the directory): it is ccd's own
// working directory (the entrypoint cd's into it), and replacing it would leave
// ccd — and every git it runs — in a deleted directory. Each entry's rename is
// atomic, and because .git moves last, an interruption leaves files with no
// .git: refused as "not a git checkout", never mistaken for one.
func moveCheckout(scratch, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.Rename(filepath.Join(scratch, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(scratch, ".git"), filepath.Join(dir, ".git")); err != nil {
		return err
	}
	return os.Remove(scratch)
}

// verifyHEAD reports whether dir's HEAD names a commit.
func verifyHEAD(ctx context.Context, dir string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git rev-parse --verify HEAD: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// explain is the operator-facing sentence for a failed clone: git's error, and
// what the `github` connector needs — filing this agent's request for it when
// it holds no grant (see the file comment).
func (c *checkout) explain(ctx context.Context, cloneErr error) string {
	what := c.repo
	if c.branch != "" {
		what += " (branch " + c.branch + ")"
	}
	msg := fmt.Sprintf("the repository %s could not be cloned: %v", what, cloneErr)
	if c.connectBase == "" {
		return msg + ". This pod is not offered muster's connector store, so only a public repository can be cloned"
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var conn agentConnector
	if err := musterGET(rctx, c.connectBase, c.token, "/agent/connectors/"+githubConnector, &conn); err != nil {
		return msg + fmt.Sprintf(". The %s connector's status could not be read (%v); ccd retries", githubConnector, err)
	}
	switch conn.Grant {
	case "approved":
		return msg + fmt.Sprintf(". This agent's %s connector grant is approved, so git goes through muster's proxy "+
			"(within %s of the approval): check the repository and branch exist and that the connector's token can read them",
			githubConnector, gitSyncInterval)
	case "pending":
		return msg + fmt.Sprintf(". If it is private, approve this agent's request for the %s connector (Connectors); "+
			"ccd retries every %s", githubConnector, c.retry)
	case "denied", "revoked":
		return msg + fmt.Sprintf(". This agent's %s connector grant was %s, and ccd does not ask again: "+
			"dispatch a new agent and approve its request, or use a public repository", githubConnector, conn.Grant)
	}
	c.mu.Lock()
	already := c.requested
	c.mu.Unlock()
	if already {
		return msg + fmt.Sprintf(". A request for the %s connector was filed and is no longer pending (%q)", githubConnector, conn.Grant)
	}
	reason := "clone " + c.repo + ": this agent was dispatched to work in it, and its Claude Code session starts only once it is checked out"
	if err := musterPOST(rctx, c.connectBase, c.token, "/agent/connectors/"+githubConnector+"/request",
		map[string]string{"reason": reason}); err != nil {
		return msg + fmt.Sprintf(". Filing this agent's request for the %s connector failed (%v); ccd retries", githubConnector, err)
	}
	c.mu.Lock()
	c.requested = true
	c.mu.Unlock()
	exists := ""
	if !conn.Exists {
		exists = fmt.Sprintf(" (no %s connector exists yet: create one whose token can read the repository first)", githubConnector)
	}
	return msg + fmt.Sprintf(". If it is private, approve the request ccd filed for this agent for the %s connector "+
		"(Connectors)%s; ccd retries every %s", githubConnector, exists, c.retry)
}

// musterPOST sends a JSON body to one of muster's agent routes.
func musterPOST(ctx context.Context, base, token, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Muster-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirects.Do(req)
	if err != nil {
		return fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("muster answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// lastLines is the last n non-empty lines of s, joined with "; ", control
// characters removed and capped at max bytes.
func lastLines(s string, n, max int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, l)
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	out := strings.Join(keep, "; ")
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
