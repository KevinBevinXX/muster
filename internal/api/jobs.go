package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
)

// THE MACHINE JOB API (internal/jobs, migration 0037): an external orchestrator
// submits a one-shot job — "run this prompt on this agent, optionally at this
// commit, and give me the result" — and polls for its outcome.
//
//	POST /api/jobs       {agent | worker_pool, prompt, repo?, sha?, pr?, result_path?, timeout_s?, idempotency_key?}
//	                     → 202 {job_id, status: "queued", …}  (200 + deduplicated:true on a repeated key)
//	GET  /api/jobs/{id}  → {job_id, status, result?, reason?, error?, started_at, finished_at, agent, cc_account, …}
//
// 🔴 ASYNCHRONOUS OVER THE EXISTING SYNCHRONOUS TURN, NOT NEW ccd PLUMBING. A
// claude-code agent's turn is already one blocking request to ccd's
// /v1/responses that returns when the CLI's turn ends — with the final reply
// text, or a typed failure (rate_limited, auth_failed, turn_timeout, …). That
// request IS "the job is done when the agent's turn completes", so the runner
// below is that call (Gateway.Chat, the call `chief ask` makes) in a goroutine
// whose outcome is written to the row. Nothing in ccd changes.
//
// 🔴 THE SERVICE TIER, requireServiceToken: MUSTER_SERVICE_TOKEN plus an
// X-Muster-Actor naming the orchestrator, recorded on the row as requested_by.
// It is FAIL-CLOSED (503 while the token is unset) — the same reasoning
// requireArmedHookToken gives for POST /api/agents/{name}/messages applies with
// more force here, since a job is arbitrary work inside an agent instance. The
// tier acts as user 1, so a job can name only user 1's agents (row-level
// security hides every other owner's).
//
// 🔴 A sha JOB RUNS IN A WORKTREE OF ITS OWN, NOT IN THE AGENT'S CHECKOUT. Its
// turn is a fresh Claude Code session (jobSession) whose CLI ccd starts in a
// git worktree of the commit that this runner creates first, with the agent
// configuration the commit carries (`.claude/` at any depth, the root
// CLAUDE.md and AGENTS.md, and .mcp.json) replaced by the default
// branch's (agentprovision.PrepareJobWorktree) — so the commit under review
// cannot configure, hook or add MCP servers to its own reviewer — and removes
// it after the job, whatever the outcome (removeJobWorktree). The agent's own
// checkout is never moved.
//
// 🔴 ON A POOL WORKER IT RUNS WITHOUT A WRITE CREDENTIAL. The commit is fetched
// anonymously as pull request `pr`'s head from the repository's public URL,
// around any proxy routing the pod has, and the job runs only at that head or
// an ancestor; a POOL worker holds no approved grant reaching github.com
// (jobs_grants.go). A NAMED agent is not checked: one with an approved grant
// runs the job with that grant in its pod.
//
// ⚠ WHAT A JOB DOES NOT HAVE, STATED SO IT IS NOT INFERRED:
//   - isolation from the commit's code. The turn runs in the worker's pod, with
//     the worker's Claude account, network and volume; any other file in the
//     commit (a build script, a test) is the commit's, and the turn may run it.
//     So it is for commits by trusted members only (docs/jobs.md).
//   - survival across a restart. The turn runs in this process and nothing waits
//     for it on shutdown. A graceful shutdown fails the jobs THIS process holds
//     (FailOwnJobs), never another's. After a crash, the reaper (RunJobReaper
//     → jobs.Store.ReapOverdue, at boot and every JobReapInterval) fails a job
//     only once it is past its own deadline, so a job a crashed process left
//     behind is failed `interrupted`
//     up to timeout_s + jobs.ReapSlack after it started (or jobs.MaxQueueWait
//     after it was queued) — and a job the OTHER pod of a rolling update is
//     still running is left alone.
//   - more than jobs.MaxActivePerAgent jobs queued or running on one agent: a
//     submission past that is refused 429.
//   - cancellation of the agent's turn on timeout. The job's deadline aborts
//     muster's request to ccd and the job ends timed_out, but ccd's turn may run
//     on; a later job on the same agent can then be refused `busy` by ccd.
//   - more than one job per agent at a time. Jobs on one agent run one after
//     another in this process (jobLanes). Each has its own worktree and its own
//     CLI, so nothing on disk is shared any more; but one worker's turns share
//     its Claude account and its pod's memory (ccd runs at most
//     CCD_MAX_SESSIONS CLIs), and parallel job turns on one worker have not
//     been exercised.

// registerJobRoutes wires the job API.
//
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Each handler answers its own "nothing to serve from" case.
func (s *Server) registerJobRoutes(mux Mux) {
	// The gateway check sits inside the service gate, as on the agent message
	// write: an unauthenticated caller learns nothing about what is wired.
	mux.HandleFunc("POST /api/jobs", s.requireServiceToken(s.requireGatewayProvisioner(s.handleJobCreate)))
	mux.HandleFunc("GET /api/jobs/{id}", s.requireServiceToken(s.handleJobGet))
}

// jobRequest is POST /api/jobs's body. Unknown fields are refused: a misspelt
// `result_path` silently ignored would return the reply instead of the file.
type jobRequest struct {
	Agent          string `json:"agent"`
	WorkerPool     string `json:"worker_pool"`
	Prompt         string `json:"prompt"`
	Repo           string `json:"repo"`
	SHA            string `json:"sha"`
	PR             *int   `json:"pr"`
	ResultPath     string `json:"result_path"`
	TimeoutS       *int   `json:"timeout_s"`
	IdempotencyKey string `json:"idempotency_key"`
}

// jobBodyLimit bounds the request body: the prompt's cap plus room for the rest.
const jobBodyLimit = jobs.MaxPromptBytes + 16<<10

// jobsUnwired is the 503 both routes answer with no job store.
func (s *Server) jobsUnwired(w http.ResponseWriter) bool {
	if s.ext.Jobs != nil {
		return false
	}
	s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "this deployment has no job store (the job API needs the database)",
	})
	return true
}

func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	if s.jobsUnwired(w) {
		return
	}
	ctx := r.Context()
	var req jobRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, jobBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid job body: " + err.Error()})
		return
	}
	n, status, msg := validateJobRequest(req)
	if status != 0 {
		s.writeJSON(w, status, map[string]any{"error": msg})
		return
	}
	// A key already used answers its job before anything is chosen: a retry of
	// an accepted pool job must not be refused because its worker is now busy,
	// or because the agent it named has gone since. Create answers it too
	// (atomically, for a race between two first submissions); this read is
	// what lets the answer come before the choice.
	if n.IdempotencyKey != "" {
		j, err := s.ext.Jobs.ByIdempotencyKey(ctx, n.IdempotencyKey)
		if err == nil {
			v := jobView(j)
			v["deduplicated"] = true
			s.writeJSON(w, http.StatusOK, v)
			return
		}
		if !errors.Is(err, jobs.ErrNotFound) {
			s.logger.Printf("jobs: read idempotency key: %v", err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the job store"})
			return
		}
	}
	if (n.SHA != "" || n.ResultPath != "") && s.ext.JobWorkspace == nil {
		s.writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this deployment cannot reach inside an agent's instance (no provisioner wired), " +
				"so it cannot honour sha or result_path",
		})
		return
	}
	n.WorkerPool = strings.TrimSpace(req.WorkerPool)
	n.RequestedBy, _ = serviceActorFromContext(ctx)
	var j jobs.Job
	var created bool
	var err error
	// target names the agent, or the pool, in a log line.
	target := "agent " + strconv.Quote(strings.TrimSpace(req.Agent))
	if n.WorkerPool != "" {
		target = "pool " + strconv.Quote(n.WorkerPool)
		j, created, err = s.createPoolJob(ctx, n)
	} else {
		a, status, msg := s.namedJobAgent(ctx, strings.TrimSpace(req.Agent))
		if status != 0 {
			s.writeJSON(w, status, map[string]any{"error": msg})
			return
		}
		if n, status, msg = jobOnAgent(n, a); status != 0 {
			s.writeJSON(w, status, map[string]any{"error": msg})
			return
		}
		j, created, err = s.ext.Jobs.Create(ctx, n)
	}
	var refused *jobRefusal
	if errors.As(err, &refused) {
		s.writeJSON(w, refused.status, map[string]any{"error": refused.msg})
		return
	}
	if errors.Is(err, jobs.ErrAgentAtCapacity) {
		// A pool job's choice already skips a worker at its cap, so this is the
		// named agent's refusal; a pool's would be the store's backstop, and the
		// chosen worker's name stayed inside the store.
		msg := fmt.Sprintf("agent %q already has %d jobs queued or running (the limit); submit again once one finishes",
			n.Agent, jobs.MaxActivePerAgent)
		if n.WorkerPool != "" {
			msg = fmt.Sprintf("the worker chosen in pool %q already has %d jobs queued or running (the limit); submit again once one finishes",
				n.WorkerPool, jobs.MaxActivePerAgent)
		}
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": msg})
		return
	}
	if err != nil {
		s.logger.Printf("jobs: create for %s: %v", target, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not record the job"})
		return
	}
	if !created {
		v := jobView(j)
		v["deduplicated"] = true
		s.writeJSON(w, http.StatusOK, v)
		return
	}
	// The run outlives the request: detached from its cancellation, keeping its
	// principal (every store call below acts as the caller under row-level
	// security).
	runCtx := context.WithoutCancel(ctx)
	// Registered BEFORE the goroutine starts, so a shutdown that begins right
	// after this response cannot miss it (FailOwnJobs).
	s.ownJobs.add(j.ID, runCtx)
	safeGo(s.logger, "job "+strconv.FormatInt(j.ID, 10), func() { s.runJob(runCtx, j) })
	s.writeJSON(w, http.StatusAccepted, jobView(j))
}

// validateJobRequest checks the request's own fields — everything that needs no
// lookup — and returns the job to record, or an HTTP status and a reason.
func validateJobRequest(req jobRequest) (jobs.NewJob, int, string) {
	n := jobs.NewJob{
		Prompt:         req.Prompt,
		Repo:           strings.TrimSpace(req.Repo),
		SHA:            strings.TrimSpace(req.SHA),
		ResultPath:     strings.TrimSpace(req.ResultPath),
		TimeoutSeconds: jobs.DefaultTimeoutSeconds,
		IdempotencyKey: req.IdempotencyKey,
	}
	agent, pool := strings.TrimSpace(req.Agent), strings.TrimSpace(req.WorkerPool)
	switch {
	case agent != "" && pool != "":
		return n, http.StatusBadRequest, "agent and worker_pool are mutually exclusive: name an agent, or a pool to choose one from"
	case agent == "" && pool == "":
		return n, http.StatusBadRequest, "agent or worker_pool is required"
	case pool != "" && !agents.ValidWorkerPool(pool):
		return n, http.StatusBadRequest, "worker_pool must be 1-63 of [a-z0-9-], not starting with '-'"
	case pool != "" && n.SHA != "" && n.Repo == "":
		return n, http.StatusBadRequest, "a worker_pool job with a sha must name its repo: the pool's workers may work on different repositories"
	case strings.TrimSpace(req.Prompt) == "":
		return n, http.StatusBadRequest, "prompt is required"
	case len(req.Prompt) > jobs.MaxPromptBytes:
		return n, http.StatusBadRequest, fmt.Sprintf("prompt is over %d bytes", jobs.MaxPromptBytes)
	case strings.ContainsRune(req.Prompt, 0):
		return n, http.StatusBadRequest, "prompt contains a NUL byte"
	case len(n.Repo) > 256:
		return n, http.StatusBadRequest, "repo is over 256 bytes"
	case n.SHA != "" && !jobs.ValidSHA(n.SHA):
		return n, http.StatusBadRequest, "sha must be a full commit id: 40 lowercase hex characters (an abbreviated sha cannot be fetched)"
	case n.SHA != "" && req.PR == nil:
		return n, http.StatusBadRequest, "a job with a sha must name its pull request (pr): the commit is fetched as that pull request's head"
	case n.SHA == "" && req.PR != nil:
		return n, http.StatusBadRequest, "pr is only for a job with a sha"
	case req.PR != nil && (*req.PR < 1 || *req.PR > jobs.MaxPR):
		return n, http.StatusBadRequest, "pr must be a positive pull request number"
	case n.Repo != "" && !jobs.ValidRepo(n.Repo):
		return n, http.StatusBadRequest, "repo must be owner/name"
	case n.ResultPath != "" && !jobs.ValidResultPath(n.ResultPath):
		return n, http.StatusBadRequest, "result_path must be a clean relative path inside the agent's working directory " +
			"(no leading /, no ., .. or .git elements, at most 512 bytes)"
	case len(req.IdempotencyKey) > jobs.MaxIdempotencyKeyBytes:
		return n, http.StatusBadRequest, fmt.Sprintf("idempotency_key is over %d bytes", jobs.MaxIdempotencyKeyBytes)
	case req.IdempotencyKey != "" && strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey:
		return n, http.StatusBadRequest, "idempotency_key has leading or trailing whitespace"
	}
	if req.PR != nil {
		n.PR = *req.PR
	}
	if req.TimeoutS != nil {
		if *req.TimeoutS < 1 || *req.TimeoutS > jobs.MaxTimeoutSeconds {
			return n, http.StatusBadRequest, fmt.Sprintf("timeout_s must be between 1 and %d", jobs.MaxTimeoutSeconds)
		}
		n.TimeoutSeconds = *req.TimeoutS
	}
	return n, 0, ""
}

// jobRefusal is a submission refused with an HTTP status and a reason, from
// inside the job store's transaction (createPoolJob's choice).
type jobRefusal struct {
	status int
	msg    string
}

func (e *jobRefusal) Error() string { return fmt.Sprintf("refused %d: %s", e.status, e.msg) }

// jobOnAgent is n placed on agent a, or the status and reason a cannot take it:
// a sha job needs a repository to check out, and a job naming a repository
// must name a's.
func jobOnAgent(n jobs.NewJob, a agents.Agent) (jobs.NewJob, int, string) {
	if n.SHA != "" && strings.TrimSpace(a.Repo) == "" {
		return n, http.StatusConflict, fmt.Sprintf("agent %q has no repository, so there is no checkout to move to %s", a.Name, n.SHA)
	}
	if n.Repo != "" && !strings.EqualFold(n.Repo, strings.TrimSpace(a.Repo)) {
		return n, http.StatusConflict, fmt.Sprintf("agent %q works on repository %q, not %q", a.Name, a.Repo, n.Repo)
	}
	n.AgentID, n.Agent = a.ID, a.Name
	// A sha job's commit is fetched from its repository's public URL, so the
	// job records the repository it ran on, the agent's when it named none.
	if n.SHA != "" && n.Repo == "" {
		n.Repo = strings.TrimSpace(a.Repo)
	}
	return n, 0, ""
}

// namedJobAgent loads the agent a job names; jobs run on claude-code agents only.
func (s *Server) namedJobAgent(ctx context.Context, name string) (agents.Agent, int, string) {
	a, err := s.ext.Agents.GetByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return agents.Agent{}, http.StatusNotFound, "no agent by that name"
	}
	if err != nil {
		s.logger.Printf("jobs: load agent %q: %v", name, err)
		return agents.Agent{}, http.StatusInternalServerError, "could not load that agent"
	}
	// Jobs run on claude-code agents only: the result, its typed failures
	// (rate_limited, auth_failed) and the workspace a result_path is read from
	// are all ccd's.
	if agents.ResolveKind(a.Kind) != agents.KindClaudeCode {
		return agents.Agent{}, http.StatusConflict, fmt.Sprintf("agent %q is kind %s; jobs run on %s agents only",
			a.Name, agents.ResolveKind(a.Kind), agents.KindClaudeCode)
	}
	return a, 0, ""
}

// createPoolJob records n on the member of its worker pool chosen for it
// (chooseWorker), reading the members, their Claude accounts' marks and their
// grants first, and their queues inside the store's transaction. Refusals are
// a *jobRefusal: 404, no agent is a member of the pool; 429, every member that
// could otherwise take the job is at its cap; 503, none could take it for any
// other reason, or this deployment cannot read the account marks the choice
// needs.
//
// 🔴 THE QUEUES ARE READ, AND THE CHOICE MADE, UNDER THE MEMBERS' LOCKS
// (jobs.Store.CreateChosen), NOT BEFORE. Read before, every submission arriving
// at once sees the same queues and chooses the same worker; its cap then
// refuses the overflow with a 429 although another worker is idle. Measured: six
// submitted at once to two idle workers with four places each — four went to
// one, and two were refused. chooseWorker stays the one place that decides; it
// is only called where its input cannot go stale.
func (s *Server) createPoolJob(ctx context.Context, n jobs.NewJob) (jobs.Job, bool, error) {
	pool, repo := n.WorkerPool, n.Repo
	refuse := func(status int, msg string) (jobs.Job, bool, error) {
		return jobs.Job{}, false, &jobRefusal{status: status, msg: msg}
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("jobs: list agents for pool %q: %v", pool, err)
		return refuse(http.StatusInternalServerError, "could not read the agents")
	}
	var members []agents.Agent
	var ids []int64
	for _, a := range list {
		if a.WorkerPool == pool {
			members = append(members, a)
			ids = append(ids, a.ID)
		}
	}
	if len(members) == 0 {
		return refuse(http.StatusNotFound, fmt.Sprintf("no worker pool %q: no agent is a member of it", pool))
	}
	// No kinds configured: no Claude account, so every member is skipped as
	// "not configured" below.
	states := map[string]ccpool.AccountState{}
	if s.ext.Kinds != nil {
		states, err = s.ext.Kinds.ClaudeAccountStates(ctx, callerID(ctx))
		if err != nil {
			s.logger.Printf("jobs: read Claude account marks for pool %q: %v", pool, err)
			return refuse(http.StatusServiceUnavailable, "could not read the Claude accounts' marks, so no worker was chosen")
		}
	}
	github, err := s.gitHubGrants(ctx, ids)
	if err != nil {
		s.logger.Printf("jobs: read connector grants for pool %q: %v", pool, err)
		return refuse(http.StatusInternalServerError, "could not read the workers' connector grants")
	}
	return s.ext.Jobs.CreateChosen(ctx, n, ids, func(active map[int64]jobs.ActiveCount) (jobs.NewJob, error) {
		a, why, full := chooseWorker(pool, members, repo, states, active, github)
		if why != "" {
			status := http.StatusServiceUnavailable
			if full {
				status = http.StatusTooManyRequests
			}
			return jobs.NewJob{}, &jobRefusal{status: status, msg: why}
		}
		placed, status, msg := jobOnAgent(n, a)
		if status != 0 {
			return jobs.NewJob{}, &jobRefusal{status: status, msg: msg}
		}
		return placed, nil
	})
}

// maxWorkerSkipsNamed bounds how many skipped workers a refusal names.
const maxWorkerSkipsNamed = 10

// chooseWorker is the worker-pool selection, pure. Of pool's members, a worker
// is ELIGIBLE when it is a claude-code agent whose stored status is running,
// works on repo (when the job names one), holds no approved grant reaching GitHub
// (github: worker id → that grant's connector; jobs_grants.go), runs on a
// Claude account that is configured and whose current token is not marked
// auth_failed, and has fewer than jobs.MaxActivePerAgent jobs queued or
// running. Of the eligible, it
// prefers, in order:
//
//  1. a worker with no job queued or running;
//  2. then the fewest jobs queued;
//  3. then the account least recently rate_limited (never before any time,
//     then the oldest);
//  4. then the agent's name, so the answer is deterministic.
//
// With none eligible it returns a non-empty reason naming why each member was
// skipped (at most maxWorkerSkipsNamed of them), and full: whether a member
// was skipped for its cap alone — every check before the cap is passed — so
// that every worker eligible but for capacity is at capacity (a 429, where a
// pool with no such worker is a 503).
func chooseWorker(pool string, members []agents.Agent, repo string, accounts map[string]ccpool.AccountState,
	active map[int64]jobs.ActiveCount, github map[int64]string) (chosen agents.Agent, why string, full bool) {
	type cand struct {
		a  agents.Agent
		c  jobs.ActiveCount
		rl time.Time
	}
	sorted := append([]agents.Agent(nil), members...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var cands []cand
	var skipped []string
	for _, m := range sorted {
		st, configured := accounts[m.CCAccount]
		c := active[m.ID]
		var skip string
		switch {
		case agents.ResolveKind(m.Kind) != agents.KindClaudeCode:
			skip = "kind " + agents.ResolveKind(m.Kind)
		case m.Status != agents.StatusRunning:
			skip = "status " + m.Status
		case repo != "" && !strings.EqualFold(repo, strings.TrimSpace(m.Repo)):
			skip = fmt.Sprintf("works on repository %q", m.Repo)
		case github[m.ID] != "":
			skip = fmt.Sprintf("holds a grant on connector %q, which reaches github.com: a pool worker holds no GitHub credential", github[m.ID])
		case !configured:
			skip = fmt.Sprintf("its Claude account %q is not configured", m.CCAccount)
		case st.AuthFailed:
			skip = fmt.Sprintf("its Claude account %q failed authentication", m.CCAccount)
		case c.Active() >= jobs.MaxActivePerAgent:
			skip = fmt.Sprintf("already has %d jobs queued or running (the limit)", c.Active())
			full = true
		}
		if skip != "" {
			skipped = append(skipped, m.Name+" ("+skip+")")
			continue
		}
		cands = append(cands, cand{a: m, c: c, rl: st.RateLimitedAt})
	}
	if len(cands) == 0 {
		named := skipped
		more := ""
		if len(named) > maxWorkerSkipsNamed {
			named, more = named[:maxWorkerSkipsNamed], fmt.Sprintf("; and %d more", len(skipped)-maxWorkerSkipsNamed)
		}
		return agents.Agent{}, fmt.Sprintf("no worker in pool %q can take the job: %s%s", pool, strings.Join(named, "; "), more), full
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.c.Active() == 0) != (b.c.Active() == 0) {
			return a.c.Active() == 0
		}
		if a.c.Queued != b.c.Queued {
			return a.c.Queued < b.c.Queued
		}
		if !a.rl.Equal(b.rl) {
			if a.rl.IsZero() || b.rl.IsZero() {
				return a.rl.IsZero()
			}
			return a.rl.Before(b.rl)
		}
		return a.a.Name < b.a.Name
	})
	return cands[0].a, "", false
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	if s.jobsUnwired(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a job id is a positive integer"})
		return
	}
	j, err := s.ext.Jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no job by that id"})
		return
	}
	if err != nil {
		s.logger.Printf("jobs: get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load the job"})
		return
	}
	s.writeJSON(w, http.StatusOK, jobView(j))
}

// jobView is a job as the API answers it. `result` is present only on a
// succeeded job and `reason` only on an unsuccessful one, so a caller can
// branch on presence; `error` is on every unsuccessful job and on a succeeded
// one only as a note. The timestamps are RFC 3339 or null.
func jobView(j jobs.Job) map[string]any {
	v := map[string]any{
		"job_id":      j.ID,
		"status":      j.Status,
		"agent":       j.Agent,
		"worker_pool": j.WorkerPool,
		"cc_account":  j.CCAccount,
		"repo":        j.Repo,
		"sha":         j.SHA,
		"pr":          j.PR,
		"result_path": j.ResultPath,
		"timeout_s":   j.TimeoutSeconds,
		"created_at":  jobTime(j.CreatedAt),
		"started_at":  jobTime(j.StartedAt),
		"finished_at": jobTime(j.FinishedAt),
	}
	if j.IdempotencyKey != "" {
		v["idempotency_key"] = j.IdempotencyKey
	}
	if j.SessionID != 0 {
		v["session_id"] = j.SessionID
	}
	if j.HasResult {
		v["result"] = j.Result
	}
	if j.Reason != "" {
		v["reason"] = j.Reason
	}
	// error is on every unsuccessful job, and on a succeeded one only as a note
	// (its result file could not be deleted).
	if j.Reason != "" || j.Error != "" {
		v["error"] = j.Error
	}
	return v
}

func jobTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ownJobs is the set of jobs THIS process accepted and has not finished, so a
// graceful shutdown can fail them (FailOwnJobs): a job runs only in the process
// that accepted it, so nothing else ever will.
type ownJobs struct {
	mu sync.Mutex
	m  map[int64]*ownJob
}

type ownJob struct {
	// ctx is the submitter's principal, detached from the request: every store
	// call on a job runs under it (row-level security).
	ctx     context.Context
	started bool
}

func (o *ownJobs) add(id int64, ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = make(map[int64]*ownJob)
	}
	o.m[id] = &ownJob{ctx: ctx}
}

func (o *ownJobs) markStarted(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if j, ok := o.m[id]; ok {
		j.started = true
	}
}

func (o *ownJobs) drop(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, id)
}

// JobShutdownTimeout bounds FailOwnJobs at shutdown.
const JobShutdownTimeout = 5 * time.Second

// FailOwnJobs fails, with reason interrupted, every job this process accepted
// and has not finished — queued ones (never sent to the agent) and running
// ones (whose turn this process is about to abandon) — and returns how many it
// failed. Called by cmd/muster-server on a graceful shutdown, AFTER the HTTP
// drain, so no submission can arrive behind it; bounded by timeout overall.
//
// 🔴 ONLY THIS PROCESS'S OWN JOBS, NEVER A SWEEP. During a rolling update the
// other pod is running ITS jobs; this touches none of them. Each write is the
// store's conditional Finish, so a job whose runner records its real outcome
// first keeps it. A crash skips this entirely, and the deadline reaper
// (RunJobReaper) remains the backstop.
func (s *Server) FailOwnJobs(timeout time.Duration) int {
	if s.ext.Jobs == nil {
		return 0
	}
	type pending struct {
		id      int64
		ctx     context.Context
		started bool
	}
	s.ownJobs.mu.Lock()
	var list []pending
	for id, j := range s.ownJobs.m {
		list = append(list, pending{id, j.ctx, j.started})
	}
	s.ownJobs.mu.Unlock()
	deadline := time.Now().Add(timeout)
	failed := 0
	for _, p := range list {
		msg := jobs.ShutdownQueuedError
		if p.started {
			msg = jobs.ShutdownRunningError
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(p.ctx), deadline)
		ok, err := s.ext.Jobs.Finish(ctx, p.id, jobs.Failed(jobs.ReasonInterrupted, msg))
		cancel()
		switch {
		case err != nil:
			s.logger.Printf("jobs: %d: fail on shutdown: %v (the deadline reaper will fail it)", p.id, err)
		case ok:
			failed++
		}
	}
	if failed > 0 {
		s.logger.Printf("jobs: failed %d job(s) this process held at shutdown (reason %q)", failed, jobs.ReasonInterrupted)
	}
	return failed
}

// jobLanes serialises jobs per agent within this process.
type jobLanes struct {
	mu sync.Mutex
	m  map[int64]*sync.Mutex
}

// lock blocks until no other job of agentID holds its lane, and returns the
// release.
func (l *jobLanes) lock(agentID int64) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[int64]*sync.Mutex)
	}
	lane, ok := l.m[agentID]
	if !ok {
		lane = &sync.Mutex{}
		l.m[agentID] = lane
	}
	l.mu.Unlock()
	lane.Lock()
	return lane.Unlock
}

// Recording a job's outcome: each attempt is bounded by jobFinishTimeout, on a
// context detached from the job's own (which has usually just expired when a
// job times out), and the write is attempted up to jobFinishAttempts times,
// jobFinishRetryPause apart. The worst case (about 32s) is far inside
// jobs.ReapSlack, so the reaper never races a runner that is still retrying;
// when every attempt fails, the reaper is what ends the job.
const (
	jobFinishTimeout    = 10 * time.Second
	jobFinishAttempts   = 3
	jobFinishRetryPause = time.Second
)

// JobReapInterval is how often RunJobReaper runs after its first pass at boot.
// A job is reaped at most this long after its deadline (jobs.ReapOverdue).
const JobReapInterval = time.Minute

// RunJobReaper fails abandoned jobs — each past its own deadline
// (jobs.Store.ReapOverdue) — once immediately, then every interval until ctx ends.
// It does nothing with no job store. Started by cmd/muster-server.
//
// 🔴 IT REPLACES A BOOT SWEEP THAT FAILED EVERY UNFINISHED JOB, which was wrong
// under a rolling update: the new pod boots while the old one still runs jobs,
// so the sweep failed live jobs (whose real result the old pod's conditional
// Finish then dropped), and a job the old pod accepted after the sweep was never
// failed by anything. A deadline is a fact about the job, not about which
// process is alive, so this pass is right from any pod at any time, and running
// it on every replica is duplicated work, not duplicated effect.
func (s *Server) RunJobReaper(ctx context.Context, interval time.Duration) {
	if s.ext.Jobs == nil {
		return
	}
	if interval <= 0 {
		interval = JobReapInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.reapJobsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reapJobsOnce is one reaper pass at the reaper's clock.
func (s *Server) reapJobsOnce(ctx context.Context) {
	now := time.Now
	if s.jobNow != nil {
		now = s.jobNow
	}
	n, err := s.ext.Jobs.ReapOverdue(ctx, now())
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("jobs: reap overdue jobs: %v", err)
		}
		return
	}
	if n > 0 {
		s.logger.Printf("jobs: failed %d job(s) past their deadline with nothing running them (reason %q)", n, jobs.ReasonInterrupted)
	}
}

// jobTimeout is a job's timeout_s as a duration. jobSecond is a test seam (zero
// is a real second), so a timeout test does not have to sleep for one.
func (s *Server) jobTimeout(j jobs.Job) time.Duration {
	unit := s.jobSecond
	if unit <= 0 {
		unit = time.Second
	}
	return time.Duration(j.TimeoutSeconds) * unit
}

// runJob runs one job to its end and records the outcome. ctx carries the
// submitter's principal and no deadline.
func (s *Server) runJob(ctx context.Context, j jobs.Job) {
	// Deferred first, so it runs last: the job is this process's until its
	// outcome is recorded.
	defer s.ownJobs.drop(j.ID)
	release := s.jobLanes.lock(j.AgentID)
	defer release()
	finish := func(o jobs.Outcome) {
		pause := s.jobFinishBackoff
		if pause <= 0 {
			pause = jobFinishRetryPause
		}
		for attempt := 1; ; attempt++ {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobFinishTimeout)
			_, err := s.ext.Jobs.Finish(fctx, j.ID, o)
			cancel()
			if err == nil {
				return
			}
			if attempt >= jobFinishAttempts {
				s.logger.Printf("jobs: %d: could not record outcome %s/%s after %d attempts, the reaper will fail it "+
					"once it is past its deadline: %v", j.ID, o.Status, o.Reason, attempt, err)
				return
			}
			s.logger.Printf("jobs: %d: record outcome %s/%s (attempt %d of %d): %v", j.ID, o.Status, o.Reason,
				attempt, jobFinishAttempts, err)
			time.Sleep(pause)
		}
	}
	// A panic below would otherwise leave the row queued or running until the
	// reaper's deadline: record it as a failure now, then let it propagate to
	// safeGo, which logs it with the stack.
	defer func() {
		if rec := recover(); rec != nil {
			finish(jobs.Failed(jobs.ReasonInternal, "muster failed while running the job (a recovered panic)"))
			panic(rec)
		}
	}()
	// Re-read the agent: it may have been deleted, or moved to another Claude
	// account, while the job waited for its lane.
	a, err := s.ext.Agents.Get(ctx, j.AgentID)
	if err != nil {
		finish(jobs.Failed(jobs.ReasonAgentUnavailable, fmt.Sprintf("agent %q could not be loaded: %v", j.Agent, err)))
		return
	}
	sess, err := s.jobSession(ctx, a)
	if err != nil {
		s.logger.Printf("jobs: %d: open a chat session on %q: %v", j.ID, a.Name, err)
		finish(jobs.Failed(jobs.ReasonInternal, "could not open a chat session on the agent"))
		return
	}
	// Marked BEFORE the row moves, so a shutdown racing the move reports the
	// cautious "the agent may have done the work", never "nothing was sent".
	s.ownJobs.markStarted(j.ID)
	started, err := s.ext.Jobs.Start(ctx, j.ID, a.CCAccount, sess.ID)
	if err != nil {
		s.logger.Printf("jobs: %d: start: %v", j.ID, err)
		finish(jobs.Failed(jobs.ReasonInternal, "could not mark the job running"))
		return
	}
	if !started {
		// Not queued any more: something else already finished it.
		return
	}
	jctx, cancel := context.WithTimeout(ctx, s.jobTimeout(j))
	defer cancel()
	finish(s.runJobTurn(jctx, a, sess, j))
}

// jobSession opens the chat session a job's turn runs in: a new one per job, on
// the claude-code runtime where the store supports explicit runtimes, so ccd
// runs it in a Claude Code session of its own rather than the agent's primary
// conversation. The prompt and reply are stored in it like any other turn, so
// the job is readable on the agent's page.
func (s *Server) jobSession(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	if st, ok := s.sessionRuntimeStore(); ok {
		return st.CreateSessionOn(ctx, a.ID, a.Name, agents.SessionRuntimeClaudeCode, "")
	}
	return s.ext.Agents.CreateSession(ctx, a.ID, a.Name)
}

// runJobTurn is the job's work under its deadline: the worktree, the turn, the
// result read-back — and, for a sha job, the worktree's removal after all of
// them, whatever the outcome (a panic included: the deferred removal runs as
// it unwinds).
func (s *Server) runJobTurn(ctx context.Context, a agents.Agent, sess agents.ChatSession, j jobs.Job) (o jobs.Outcome) {
	expired := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) }
	turnCtx := withSessionRoute(ctx, a, sess)
	// worktree is the job whose worktree the result is read from: 0 for a job
	// without a sha, which runs in the agent's working directory.
	var worktree int64
	// echo is the working directory ccd says the turn's CLI used
	// (agents.HeaderCCWorkdirUsed), for a sha job.
	var echo workdirEcho
	if j.SHA != "" {
		// 🔴 ONLY IN A SESSION OF ITS OWN. A turn with no explicit Claude Code
		// session goes to the agent's primary CLI, which works in the agent's
		// checkout — exactly where a commit's own configuration must not run.
		if sess.Runtime != agents.SessionRuntimeClaudeCode {
			return jobs.Failed(jobs.ReasonInternal, JobNoOwnSessionError)
		}
		// Registered BEFORE the worktree is made, so a half-made one goes too.
		defer func() { o = s.removeJobWorktree(ctx, a, j, o) }()
		dir, err := s.ext.JobWorkspace.PrepareJobWorktree(ctx, a, j.ID, j.Repo, j.PR, j.SHA)
		if err != nil {
			if expired() {
				return jobs.TimedOut("the job's timeout elapsed while creating its worktree at " + j.SHA + ": " + err.Error())
			}
			return jobs.Failed(jobs.ReasonCheckoutFailed, err.Error())
		}
		worktree = j.ID
		turnCtx = withJobWorkdir(turnCtx, dir, &echo)
	}
	// The transcript writes outlive the deadline: a timed-out job's prompt
	// still belongs in the session it was sent to.
	store := context.WithoutCancel(ctx)
	_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: j.Prompt})
	reply, err := s.ext.Gateway.Chat(turnCtx, a, sess.SessionKey, j.Prompt, nil)
	if reply != "" {
		_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
	}
	o = jobs.Classify(reply, err, expired())
	if o.Status != jobs.StatusSucceeded {
		return o
	}
	// 🔴 A RESULT FROM ANOTHER TREE IS NOT THE JOB'S RESULT. A ccd from before
	// job worktrees ignores the workdir header and runs the turn in the agent's
	// own checkout, answering as if nothing were wrong; only its echo (absent
	// there) tells the two apart.
	if worktree != 0 {
		if used, ok := echo.get(); !ok || used != echo.want {
			got := "no " + agents.HeaderCCWorkdirUsed
			if ok {
				got = fmt.Sprintf("%s %q", agents.HeaderCCWorkdirUsed, used)
			}
			return jobs.Failed(jobs.ReasonWorkdirUnconfirmed, fmt.Sprintf("the agent's runtime did not confirm it ran the "+
				"turn in the job's worktree %s (it answered with %s): its image is too old for sha jobs, so the turn's "+
				"output was discarded; update the agent's image", echo.want, got))
		}
	}
	if j.ResultPath != "" {
		o = s.readJobResult(ctx, a, worktree, j)
		if worktree != 0 {
			// The file goes with the worktree (removeJobWorktree, deferred).
			return o
		}
		return s.removeJobResultFile(ctx, a, j, o)
	}
	if strings.TrimSpace(reply) == "" {
		return jobs.Failed(jobs.ReasonEmptyReply, "the agent's turn ended with no reply text and the job named no result_path")
	}
	return o
}

// JobNoOwnSessionError is a sha job's error when the chat store cannot open a
// Claude Code session of the job's own (agents.SessionRuntimeStore).
const JobNoOwnSessionError = "this deployment's chat store cannot open a Claude Code session of the job's own, " +
	"so the turn could not be kept out of the agent's checkout; nothing was run"

// workdirEcho records the working directory ccd answered with for a turn.
type workdirEcho struct {
	want string
	mu   sync.Mutex
	used string
	seen bool
}

func (e *workdirEcho) record(h http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v := h.Values(agents.HeaderCCWorkdirUsed); len(v) == 1 {
		e.used, e.seen = v[0], true
	}
}

func (e *workdirEcho) get() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.used, e.seen
}

// withJobWorkdir is ctx with the turn's route also naming dir as the working
// directory of the session's CLI (agents.HeaderCCWorkdir), and recording into
// echo the directory ccd answers it used (agents.HeaderCCWorkdirUsed).
func withJobWorkdir(ctx context.Context, dir string, echo *workdirEcho) context.Context {
	r, _ := agents.TurnRouteFrom(ctx)
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(agents.HeaderCCWorkdir, dir)
	r.Header = h
	echo.want = dir
	r.OnResponse = echo.record
	return agents.WithTurnRoute(ctx, r)
}

// jobWorktreeRemoveTimeout bounds the removal of a job's worktree, on a context
// detached from the job's own (which has expired when the job timed out).
const jobWorktreeRemoveTimeout = 30 * time.Second

// removeJobWorktree removes job j's worktree and returns o, noting in its error
// a worktree that could not be removed, without changing its status.
func (s *Server) removeJobWorktree(ctx context.Context, a agents.Agent, j jobs.Job, o jobs.Outcome) jobs.Outcome {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobWorktreeRemoveTimeout)
	defer cancel()
	if err := s.ext.JobWorkspace.RemoveJobWorktree(rctx, a, j.ID); err != nil {
		s.logger.Printf("jobs: %d: remove the job's worktree: %v", j.ID, err)
		note := JobWorktreeRemoveFailedPrefix + err.Error()
		if o.Error != "" {
			o.Error += "; " + note
		} else {
			o.Error = note
		}
	}
	return o
}

// JobWorktreeRemoveFailedPrefix opens the note a job's error carries when its
// worktree could not be removed.
const JobWorktreeRemoveFailedPrefix = "could not remove the job's worktree: "

// readJobResult is the outcome of reading a job's result_path back, from job
// worktree's worktree (0: the agent's working directory).
func (s *Server) readJobResult(ctx context.Context, a agents.Agent, worktree int64, j jobs.Job) jobs.Outcome {
	data, err := s.ext.JobWorkspace.ReadWorkspaceFile(ctx, a, worktree, j.ResultPath, jobs.MaxResultBytes)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return jobs.TimedOut("the job's timeout elapsed while reading " + j.ResultPath + ": " + err.Error())
		}
		return jobs.Failed(jobs.ReasonResultReadFailed, err.Error())
	}
	if !utf8.Valid(data) {
		return jobs.Failed(jobs.ReasonResultReadFailed, j.ResultPath+" is not UTF-8 text, so it cannot be returned verbatim")
	}
	return jobs.Outcome{Status: jobs.StatusSucceeded, Result: string(data)}
}

// jobResultRemoveTimeout bounds the delete of a job's result file, on a context
// detached from the job's own, so the delete does not depend on how much of the
// job's deadline the read left.
const jobResultRemoveTimeout = 10 * time.Second

// removeJobResultFile deletes the result_path file of a job WITHOUT a sha (its
// turn ran in the agent's own working directory) when — and only when — it was
// read successfully (o succeeded) and the repository does not track it, and
// returns o, noting in its error a file left behind, without changing its
// status. A sha job's file goes with its worktree (removeJobWorktree).
//
// 🔴 WHY THE FILE IS DELETED: left in the agent's working directory, it would
// be there for the next job naming the same result_path, and a turn of that
// job that wrote none would return THIS job's output as its own. After a
// successful read its content is in the job's result, byte for byte, so
// removing it hides nothing.
//
// 🔴 AND WHY ONLY THEN: after a failed read (over the limit, not UTF-8, gone)
// the file is the only copy of the agent's output, so it stays. A TRACKED file
// (result_path naming e.g. CHANGELOG.md) is part of the repository: deleting it
// would itself dirty the tree, so it stays too, with a note.
func (s *Server) removeJobResultFile(ctx context.Context, a agents.Agent, j jobs.Job, o jobs.Outcome) jobs.Outcome {
	if o.Status != jobs.StatusSucceeded {
		return o
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobResultRemoveTimeout)
	defer cancel()
	removed, err := s.ext.JobWorkspace.RemoveUntrackedWorkspaceFile(rctx, a, j.ResultPath)
	var note string
	switch {
	case err != nil:
		s.logger.Printf("jobs: %d: remove result file %s: %v", j.ID, j.ResultPath, err)
		note = JobResultRemoveFailedPrefix + j.ResultPath + " (a later job on this agent naming the same result_path " +
			"could return it if its own turn writes none): " + err.Error()
	case !removed:
		note = JobResultTrackedNotePrefix + j.ResultPath + " is tracked by the repository, so it was left in place, " +
			"as the turn left it, in the agent's checkout"
	default:
		return o
	}
	if o.Error != "" {
		o.Error += "; " + note
	} else {
		o.Error = note
	}
	return o
}

// JobResultRemoveFailedPrefix opens the note a job's error carries when its
// result file could not be deleted.
const JobResultRemoveFailedPrefix = "could not delete the result file "

// JobResultTrackedNotePrefix opens the note a job's error carries when its
// result file is tracked by the repository and so was not deleted.
const JobResultTrackedNotePrefix = "the result file "
