package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/jobs"
)

// THE MACHINE JOB API (internal/jobs, migration 0037): an external orchestrator
// submits a one-shot job — "run this prompt on this agent, optionally at this
// commit, and give me the result" — and polls for its outcome.
//
//	POST /api/jobs       {agent, prompt, repo?, sha?, result_path?, timeout_s?, idempotency_key?}
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
// ⚠ WHAT A JOB DOES NOT HAVE IN THIS FIRST CUT, STATED SO IT IS NOT INFERRED:
//   - worker-pool selection. A job names one existing, running claude-code agent;
//     `worker_pool` is refused 501. pickJobAgent is the seam a pool goes behind.
//   - survival across a restart. The turn runs in this process and nothing waits
//     for it on shutdown. The reaper (RunJobReaper → jobs.Store.ReapOverdue, at
//     boot and every JobReapInterval) fails a job only once it is past its own
//     deadline, so a job a dying process left behind is failed `interrupted`
//     up to timeout_s + jobs.ReapSlack after it started (or jobs.MaxQueueWait
//     after it was queued) — and a job the OTHER pod of a rolling update is
//     still running is left alone.
//   - more than jobs.MaxActivePerAgent jobs queued or running on one agent: a
//     submission past that is refused 429.
//   - cancellation of the agent's turn on timeout. The job's deadline aborts
//     muster's request to ccd and the job ends timed_out, but ccd's turn may run
//     on; a later job on the same agent can then be refused `busy` by ccd.
//   - more than one job per agent at a time. Jobs on one agent run one after
//     another in this process (jobLanes): a sha checkout moves the one working
//     tree the agent has, so two jobs at once would race each other's commit.

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
	if (n.SHA != "" || n.ResultPath != "") && s.ext.JobWorkspace == nil {
		s.writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this deployment cannot reach inside an agent's instance (no provisioner wired), " +
				"so it cannot honour sha or result_path",
		})
		return
	}
	a, status, msg := s.pickJobAgent(ctx, req)
	if status != 0 {
		s.writeJSON(w, status, map[string]any{"error": msg})
		return
	}
	if n.SHA != "" && strings.TrimSpace(a.Repo) == "" {
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("agent %q has no repository, so there is no checkout to move to %s", a.Name, n.SHA),
		})
		return
	}
	if n.Repo != "" && !strings.EqualFold(n.Repo, strings.TrimSpace(a.Repo)) {
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("agent %q works on repository %q, not %q", a.Name, a.Repo, n.Repo),
		})
		return
	}
	n.AgentID, n.Agent = a.ID, a.Name
	n.RequestedBy, _ = serviceActorFromContext(ctx)

	j, created, err := s.ext.Jobs.Create(ctx, n)
	if errors.Is(err, jobs.ErrAgentAtCapacity) {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": fmt.Sprintf("agent %q already has %d jobs queued or running (the limit); submit again once one finishes",
				a.Name, jobs.MaxActivePerAgent),
		})
		return
	}
	if err != nil {
		s.logger.Printf("jobs: create for agent %q: %v", a.Name, err)
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
	switch {
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
	case n.ResultPath != "" && !jobs.ValidResultPath(n.ResultPath):
		return n, http.StatusBadRequest, "result_path must be a clean relative path inside the agent's working directory " +
			"(no leading /, no . or .. elements, at most 512 bytes)"
	case len(req.IdempotencyKey) > jobs.MaxIdempotencyKeyBytes:
		return n, http.StatusBadRequest, fmt.Sprintf("idempotency_key is over %d bytes", jobs.MaxIdempotencyKeyBytes)
	case req.IdempotencyKey != "" && strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey:
		return n, http.StatusBadRequest, "idempotency_key has leading or trailing whitespace"
	}
	if req.TimeoutS != nil {
		if *req.TimeoutS < 1 || *req.TimeoutS > jobs.MaxTimeoutSeconds {
			return n, http.StatusBadRequest, fmt.Sprintf("timeout_s must be between 1 and %d", jobs.MaxTimeoutSeconds)
		}
		n.TimeoutSeconds = *req.TimeoutS
	}
	return n, 0, ""
}

// pickJobAgent resolves the agent a job runs on.
//
// 🔴 THIS IS THE SEAM WORKER POOLS GO BEHIND. Today a job names its agent and
// this is a lookup; a pool would choose one here (by repository, by which
// Claude account is rested) and everything after it is unchanged.
func (s *Server) pickJobAgent(ctx context.Context, req jobRequest) (agents.Agent, int, string) {
	name := strings.TrimSpace(req.Agent)
	if strings.TrimSpace(req.WorkerPool) != "" {
		return agents.Agent{}, http.StatusNotImplemented, "worker_pool selection is not implemented yet; name an agent"
	}
	if name == "" {
		return agents.Agent{}, http.StatusBadRequest, "agent is required"
	}
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
// succeeded job and `reason`/`error` only on an unsuccessful one, so a caller
// can branch on presence; the timestamps are RFC 3339 or null.
func jobView(j jobs.Job) map[string]any {
	v := map[string]any{
		"job_id":      j.ID,
		"status":      j.Status,
		"agent":       j.Agent,
		"cc_account":  j.CCAccount,
		"repo":        j.Repo,
		"sha":         j.SHA,
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

// runJobTurn is the job's work under its deadline: the checkout, the turn, the
// result read-back.
func (s *Server) runJobTurn(ctx context.Context, a agents.Agent, sess agents.ChatSession, j jobs.Job) jobs.Outcome {
	expired := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) }
	if j.SHA != "" {
		if err := s.ext.JobWorkspace.CheckoutCommit(ctx, a, j.SHA); err != nil {
			if expired() {
				return jobs.TimedOut("the job's timeout elapsed while checking out " + j.SHA + ": " + err.Error())
			}
			return jobs.Failed(jobs.ReasonCheckoutFailed, err.Error())
		}
	}
	// The transcript writes outlive the deadline: a timed-out job's prompt
	// still belongs in the session it was sent to.
	store := context.WithoutCancel(ctx)
	_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: j.Prompt})
	reply, err := s.ext.Gateway.Chat(withSessionRoute(ctx, a, sess), a, sess.SessionKey, j.Prompt, nil)
	if reply != "" {
		_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
	}
	o := jobs.Classify(reply, err, expired())
	if o.Status != jobs.StatusSucceeded {
		return o
	}
	if j.ResultPath != "" {
		data, err := s.ext.JobWorkspace.ReadWorkspaceFile(ctx, a, j.ResultPath, jobs.MaxResultBytes)
		if err != nil {
			if expired() {
				return jobs.TimedOut("the job's timeout elapsed while reading " + j.ResultPath + ": " + err.Error())
			}
			return jobs.Failed(jobs.ReasonResultReadFailed, err.Error())
		}
		if !utf8.Valid(data) {
			return jobs.Failed(jobs.ReasonResultReadFailed, j.ResultPath+" is not UTF-8 text, so it cannot be returned verbatim")
		}
		return jobs.Outcome{Status: jobs.StatusSucceeded, Result: string(data)}
	}
	if strings.TrimSpace(reply) == "" {
		return jobs.Failed(jobs.ReasonEmptyReply, "the agent's turn ended with no reply text and the job named no result_path")
	}
	return o
}
