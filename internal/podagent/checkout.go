package podagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// The agent's repository, checked out BEFORE the agent starts.
//
// muster gives an agent dispatched with a repository its owner/name, an
// optional branch and the directory to check it out into — the agent's working
// directory. So the checkout IS the workspace, by construction.
//
// 🔴 THE AGENT DOES NOT START UNTIL THE CHECKOUT EXISTS. A session started in an
// empty directory would take its first turn without the repository and nothing
// would say so. Until the clone succeeds:
//   - every turn is `503 not_ready` whose message says WHY (the clone's error,
//     and what the github connector needs);
//   - muster's kickoff deliverer reads `repo.state` BEFORE it sends the first
//     turn, and while it is `cloning` or `awaiting_grant` it holds the turn,
//     unsent and unmarked, for up to a day (agents.DecideFirstTurn); on
//     `failed` it sends, re-sends the not_ready for a while, and records the
//     message as the agent's kickoff error, which the card shows;
//   - /healthz stays 200 (the pod must not restart-loop while it waits for an
//     operator) and reports the state under `repo`;
//   - the supervisor retries the clone every CheckoutRetry.
//
// 🔴 CREDENTIALS COME ONLY FROM THE `github` CONNECTOR. git reaches GitHub
// through muster's proxy once this agent holds an approved grant on it
// (gitroute.go keeps the routing in step); no token is ever placed in the pod.
// A private repository therefore cannot be cloned until that grant exists — and
// because the session that could ask for it has not started, the supervisor
// FILES THE REQUEST ITSELF, once per process, when a clone fails and the agent
// holds no grant at all (status `none`). It is the same request the session's
// connector_request tool files: the operator still approves it. It never
// re-asks after a denial or a revocation.
//
// An existing checkout (<dir>/.git, on the agent's volume after a pod restart)
// is used as it is: never re-cloned, fetched or reset.

const (
	// GithubConnector is the connector git reaches GitHub through.
	GithubConnector = "github"

	// CheckoutRetry is how often a failed clone is retried. The grants are
	// re-read every GitSyncInterval, so a clone retried after an approval goes
	// through the proxy within CheckoutRetry + GitSyncInterval.
	CheckoutRetry = 30 * time.Second
	// CheckoutTimeout bounds one clone.
	CheckoutTimeout = 10 * time.Minute

	// GithubCloneBase is where a repository is cloned from. A test points git at
	// a local repository with url.<local>.insteadOf this prefix, exactly as the
	// connector routing does with muster's proxy.
	GithubCloneBase = "https://github.com/"
)

// The states /healthz reports under `repo`. They ARE internal/agents' constants:
// muster's kickoff deliverer reads this field (agents.DecideFirstTurn) and holds
// an agent's first turn, unsent and unstamped, while it reads cloning or
// awaiting_grant — so a spelling that drifted on either side would send the turn
// into a checkout that does not exist yet.
const (
	CheckoutCloning = agents.CheckoutCloning
	CheckoutFailed  = agents.CheckoutFailed
	CheckoutReady   = agents.CheckoutReady
	// CheckoutAwaitingGrant: a clone failed and this agent's github connector
	// request is pending (the supervisor just filed it, or it already was) —
	// the one state the owner's approval alone resolves.
	CheckoutAwaitingGrant = agents.CheckoutAwaitingGrant
)

// Voice is how a supervisor names itself and the agent it starts in the
// sentences a person reads.
type Voice struct {
	// Who is the supervisor ("ccd", "hermesd"): log prefixes, "<who> retries".
	Who string
	// Agent is the thing that starts once the checkout exists, with its article
	// ("the Claude Code session").
	Agent string
	// AgentOf is the same with a possessive ("its Claude Code session"), for the
	// reason filed with the connector request.
	AgentOf string
}

// CheckoutState is the `repo` object /healthz reports.
type CheckoutState struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Dir    string `json:"dir"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// CheckoutConfig is what a checkout is made from.
type CheckoutConfig struct {
	Repo, Branch, Dir string
	// ConnectBase and Token reach muster's connector routes; empty when the pod
	// is not offered the connector store.
	ConnectBase, Token string
	Voice              Voice
}

// Checkout is one agent's repository checkout. Retry, Timeout and Logf may be
// changed before Run.
type Checkout struct {
	repo, branch, dir  string
	connectBase, token string
	voice              Voice
	// Retry is how often a failed clone is retried.
	Retry time.Duration
	// Timeout bounds one clone; 0 is CheckoutTimeout.
	Timeout time.Duration
	Logf    func(string, ...any)

	mu        sync.Mutex
	st        CheckoutState
	requested bool // the github request was filed by this process
	done      chan struct{}
}

// ParseRepo reports whether repo is owner/name in the shape a checkout accepts.
func ParseRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(name, "/\\") && !strings.HasPrefix(owner, "-") && !strings.HasPrefix(name, ".")
}

// NewCheckout makes the checkout cfg describes, in state cloning. The caller has
// validated cfg.Repo with ParseRepo and made cfg.Dir absolute.
func NewCheckout(cfg CheckoutConfig) *Checkout {
	c := &Checkout{
		repo: cfg.Repo, branch: cfg.Branch, dir: filepath.Clean(cfg.Dir),
		connectBase: cfg.ConnectBase, token: cfg.Token, voice: cfg.Voice,
		Retry: CheckoutRetry, Logf: func(string, ...any) {}, done: make(chan struct{}),
	}
	c.st = CheckoutState{Repo: c.repo, Branch: c.branch, Dir: c.dir, State: CheckoutCloning}
	return c
}

// State is what /healthz reports.
func (c *Checkout) State() CheckoutState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// Ready reports whether the checkout exists.
func (c *Checkout) Ready() bool { return c.State().State == CheckoutReady }

// Wait blocks until the checkout exists (true) or ctx ends (false).
func (c *Checkout) Wait(ctx context.Context) bool {
	select {
	case <-c.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// SetStateForTest overwrites the reported state without running a clone. It
// exists for a supervisor's own tests (a package outside this one cannot reach
// the unexported state), never for production code.
func (c *Checkout) SetStateForTest(state, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.State, c.st.Detail = state, detail
}

// NotReady is the not_ready message a turn gets while there is no checkout.
func (c *Checkout) NotReady() string {
	st := c.State()
	if st.State == CheckoutFailed || st.State == CheckoutAwaitingGrant {
		return c.voice.Agent + " has not started: it starts only once the repository is checked out, and " + st.Detail
	}
	return fmt.Sprintf("%s has not started: the repository %s is being cloned into %s", c.voice.Agent, st.Repo, st.Dir)
}

// Run clones until it succeeds or ctx ends.
func (c *Checkout) Run(ctx context.Context) {
	for {
		err := c.Attempt(ctx)
		if err == nil {
			c.mu.Lock()
			c.st.State, c.st.Detail = CheckoutReady, ""
			c.mu.Unlock()
			close(c.done)
			return
		}
		if ctx.Err() != nil {
			return
		}
		detail, awaiting := c.Explain(ctx, err)
		state := CheckoutFailed
		if awaiting {
			state = CheckoutAwaitingGrant
		}
		c.mu.Lock()
		c.st.State, c.st.Detail = state, detail
		c.mu.Unlock()
		c.Logf("%s: checkout: %s; retrying in %s", c.voice.Who, detail, c.Retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.Retry):
		}
	}
}

// Attempt makes the checkout exist: an existing one is used as it is; an empty
// (or absent) directory is cloned into; a directory holding anything else is
// refused rather than cloned over.
//
// 🔴 A CHECKOUT IS A .git WITH A COMMIT CHECKED OUT, NOT A .git. A clone that is
// killed (the timeout, the supervisor shutting down, the pod evicted) leaves a
// .git whose HEAD names no commit; treating that as ready would start the agent
// in an empty tree. So: the clone goes into a sibling scratch directory and is
// moved into place (moveCheckout, .git last) only once `git rev-parse --verify
// HEAD` succeeds there — a kill leaves the workspace untouched (the scratch
// directory is replaced on the next attempt) — and an existing .git counts only
// when HEAD verifies too. One with no HEAD (an interrupted clone by an older
// supervisor) is refused, not deleted: it never removes what it did not just
// create.
func (c *Checkout) Attempt(ctx context.Context) error {
	who := c.voice.Who
	if _, err := os.Lstat(filepath.Join(c.dir, ".git")); err == nil {
		if err := verifyHEAD(ctx, c.dir); err != nil {
			return fmt.Errorf("%s holds a .git with no commit checked out (an interrupted clone?); %s does not "+
				"clone over it — remove %s to let it clone again", c.dir, who, filepath.Join(c.dir, ".git"))
		}
		c.Logf("%s: checkout: using the existing checkout of %s in %s", who, c.repo, c.dir)
		return nil
	}
	entries, err := os.ReadDir(c.dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("%s holds files but is not a git checkout; %s does not clone over them", c.dir, who)
	}
	scratch := filepath.Join(filepath.Dir(c.dir), "."+filepath.Base(c.dir)+"."+who+"-clone")
	if err := os.RemoveAll(scratch); err != nil {
		return err
	}
	args := []string{"clone", "--quiet"}
	if c.branch != "" {
		args = append(args, "--branch="+c.branch)
	}
	args = append(args, "--", GithubCloneBase+c.repo+".git", scratch)
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = CheckoutTimeout
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
		if msg := LastLines(stderr.String(), 2, 300); msg != "" {
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
	c.Logf("%s: checkout: cloned %s into %s", who, c.repo, c.dir)
	return nil
}

// moveCheckout moves a verified clone's entries from scratch into dir, its
// .git LAST, then removes scratch.
//
// ⚠ dir itself is NOT replaced (no rename of the directory): it can be the
// supervisor's own working directory, and replacing it would leave it — and
// every git it runs — in a deleted directory. Each entry's rename is atomic,
// and because .git moves last, an interruption leaves files with no .git:
// refused as "not a git checkout", never mistaken for one.
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

// Explain is the operator-facing sentence for a failed clone: git's error, and
// what the `github` connector needs — filing this agent's request for it when
// it holds no grant (see the file comment). awaiting reports that the agent's
// request for the connector is pending — the one case the owner's approval
// alone resolves.
func (c *Checkout) Explain(ctx context.Context, cloneErr error) (detail string, awaiting bool) {
	who := c.voice.Who
	what := c.repo
	if c.branch != "" {
		what += " (branch " + c.branch + ")"
	}
	msg := fmt.Sprintf("the repository %s could not be cloned: %v", what, cloneErr)
	if c.connectBase == "" {
		return msg + ". This pod is not offered muster's connector store, so only a public repository can be cloned", false
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var conn AgentConnector
	if err := MusterGET(rctx, c.connectBase, c.token, "/agent/connectors/"+GithubConnector, &conn); err != nil {
		return msg + fmt.Sprintf(". The %s connector's status could not be read (%v); %s retries", GithubConnector, err, who), false
	}
	switch conn.Grant {
	case "approved":
		return msg + fmt.Sprintf(". This agent's %s connector grant is approved, so git goes through muster's proxy "+
			"(within %s of the approval): check the repository and branch exist and that the connector's token can read them",
			GithubConnector, GitSyncInterval), false
	case "pending":
		return msg + fmt.Sprintf(". If it is private, approve this agent's request for the %s connector (Connectors); "+
			"%s retries every %s", GithubConnector, who, c.Retry), true
	case "denied", "revoked":
		return msg + fmt.Sprintf(". This agent's %s connector grant was %s, and %s does not ask again: "+
			"dispatch a new agent and approve its request, or use a public repository", GithubConnector, conn.Grant, who), false
	}
	c.mu.Lock()
	already := c.requested
	c.mu.Unlock()
	if already {
		return msg + fmt.Sprintf(". A request for the %s connector was filed and is no longer pending (%q)", GithubConnector, conn.Grant), false
	}
	reason := "clone " + c.repo + ": this agent was dispatched to work in it, and " + c.voice.AgentOf + " starts only once it is checked out"
	if err := MusterPOST(rctx, c.connectBase, c.token, "/agent/connectors/"+GithubConnector+"/request",
		map[string]string{"reason": reason}); err != nil {
		return msg + fmt.Sprintf(". Filing this agent's request for the %s connector failed (%v); %s retries", GithubConnector, err, who), false
	}
	c.mu.Lock()
	c.requested = true
	c.mu.Unlock()
	exists := ""
	if !conn.Exists {
		exists = fmt.Sprintf(" (no %s connector exists yet: create one whose token can read the repository first)", GithubConnector)
	}
	return msg + fmt.Sprintf(". If it is private, approve the request %s filed for this agent for the %s connector "+
		"(Connectors)%s; %s retries every %s", who, GithubConnector, exists, who, c.Retry), true
}
