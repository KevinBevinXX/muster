// Package jobs is the machine job API's record (migration 0037): one-shot jobs
// an external orchestrator submits — "run this prompt on this agent, optionally
// at this commit, and give me the result" — and reads back by id.
//
// The package holds the row, its store and the one rule every caller must agree
// on: how the outcome of a turn becomes a job's terminal status ([Classify]).
// Running the turn is internal/api's (jobs.go there), because a turn needs the
// agent gateway and the chat store, which this package deliberately does not
// import.
//
// 🔴 EVERY METHOD EXCEPT ReapOverdue RUNS UNDER THE CALLER'S PRINCIPAL.
// jobs is under FORCE row-level security, so a caller reads and writes only its
// own user's rows; the owner of a row is the principal's user (owner_id's
// default), never a parameter. ReapOverdue is the reaper across every owner and
// runs under the system principal itself (sys).
//
// 🔴 A JOB IS FAILED BY THE REAPER ONLY WHEN IT IS PAST ITS OWN DEADLINE, NEVER
// BECAUSE A PROCESS STARTED. During a rolling update the new process boots while
// the old one is still running jobs, so "fail whatever is unfinished at boot"
// would fail a live job and drop its real result. The reaper instead fails a
// running job only once started_at + timeout_s + ReapSlack has passed, and a
// queued one only once created_at + MaxQueueWait has — points no live runner can
// still be before (see each constant). The same pass covers every other way a
// job can be left unfinished: a process that exited mid-job, a recovered panic,
// an outcome write that kept failing.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// Statuses. A job moves queued → running → one of the three terminal ones, or
// queued → failed when it never started.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
)

// Reasons a job did not succeed that this package and its runner produce
// themselves. A failure the agent runtime reported carries the runtime's own
// typed failure instead (cmd/ccd's failure.go vocabulary: rate_limited,
// auth_failed, busy, capacity, …), so a caller can branch on exactly the token
// ccd wrote — above all on rate_limited and auth_failed, which mean "re-queue on
// another account".
const (
	// ReasonTimeout: the job's own timeout_s elapsed.
	ReasonTimeout = "timeout"
	// ReasonInterrupted: nothing finished the job by its deadline, so the reaper
	// (ReapOverdue) failed it — the process running it stopped, panicked or could
	// not record the outcome. The token predates the reaper and is kept so a
	// caller branching on it sees no change.
	ReasonInterrupted = "interrupted"
	// ReasonAgentUnavailable: the agent the job names could not be loaded when it
	// was due to start (deleted since, or the store failed).
	ReasonAgentUnavailable = "agent_unavailable"
	// ReasonCheckoutFailed: the job's worktree at its sha could not be made (the
	// commit could not be fetched, the default branch could not be resolved or
	// carries a symlink among the overlaid paths, or git failed). The token
	// predates worktrees, when a sha job moved the agent's own checkout, and is
	// kept so a caller branching on it sees no change.
	ReasonCheckoutFailed = "checkout_failed"
	// ReasonWorkdirUnconfirmed: a sha job's turn succeeded but the agent's
	// runtime did not confirm it ran in the job's worktree — an agent image too
	// old for sha jobs — so its output was discarded.
	ReasonWorkdirUnconfirmed = "workdir_unconfirmed"
	// ReasonResultReadFailed: the turn succeeded but result_path could not be read.
	ReasonResultReadFailed = "result_read_failed"
	// ReasonEmptyReply: the turn succeeded with no reply text and no result_path
	// was asked for, so there is no result to return.
	ReasonEmptyReply = "empty_reply"
	// ReasonTurnFailed: the turn failed with an error the runtime did not type.
	ReasonTurnFailed = "turn_failed"
	// ReasonInternal: muster itself failed around the turn (a store write).
	ReasonInternal = "internal"
)

// Bounds. Each is also a CHECK in migration 0037.
const (
	// MaxPromptBytes bounds a job's prompt.
	MaxPromptBytes = 64 << 10
	// MaxResultBytes bounds a stored result: the reply, or the file read back.
	MaxResultBytes = 1 << 20
	// MaxErrorBytes bounds a stored error message.
	MaxErrorBytes = 4096
	// DefaultTimeoutSeconds is a job's timeout when the request names none.
	DefaultTimeoutSeconds = 30 * 60
	// MaxTimeoutSeconds is the longest timeout a job may ask for: the claude-code
	// kind's per-request budget (agents.ClaudeCodeTurnTimeout), so the job's own
	// deadline, not the HTTP client's, is always what ends an overlong turn.
	MaxTimeoutSeconds = int(agents.ClaudeCodeTurnTimeout / time.Second)
	// MaxIdempotencyKeyBytes bounds an idempotency key.
	MaxIdempotencyKeyBytes = 128
	// MaxActivePerAgent is how many of one agent's jobs may be queued or running
	// at once; a submission past it is refused (ErrAgentAtCapacity). Jobs on one
	// agent run one at a time, so this is also what bounds a queued job's wait
	// (MaxQueueWait).
	MaxActivePerAgent = 4
)

// The reaper's deadlines (ReapOverdue).
const (
	// ReapSlack is how long past a running job's own deadline (started_at +
	// timeout_s) the reaper waits before failing it. A live runner is done by
	// then: the job's context ends the checkout, the turn and the read at the
	// deadline, and recording the outcome is a few bounded write attempts
	// (internal/api's jobFinishAttempts × jobFinishTimeout, well under a minute).
	// The rest is margin for clock skew between the processes and the database.
	ReapSlack = 5 * time.Minute
	// MaxQueueWait is how long a job may stay queued (from created_at) before the
	// reaper fails it. A queued job waits for the jobs ahead of it on the same
	// agent, at most MaxActivePerAgent−1 of them, each running for at most
	// MaxTimeoutSeconds + ReapSlack; one job's worth more is margin. With the
	// values above it is 2h40m.
	MaxQueueWait = MaxActivePerAgent * (time.Duration(MaxTimeoutSeconds)*time.Second + ReapSlack)
)

// shaPattern is a commit id a job may name: a full 40-character lowercase hex
// object id. An abbreviated one is refused because `git fetch origin <sha>`
// fetches only a full id (an abbreviation is "couldn't find remote ref").
// Migration 0037's jobs_sha_check spells the same expression.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidSHA reports whether s is a commit id a job may name.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }

// repoPattern is a GitHub repository owner/name a sha job may name: it becomes
// the anonymous fetch URL's path (agentprovision.AnonymousFetchURL), so each
// half is one plain path element. The same shape as agentspec's checkout name.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*/[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// ValidRepo reports whether s is a repository owner/name a sha job may name.
func ValidRepo(s string) bool { return repoPattern.MatchString(s) && !strings.Contains(s, "..") }

// MaxPR bounds a pull request number.
const MaxPR = 1<<31 - 1

// ValidResultPath reports whether p may be a job's result_path: a non-empty,
// relative, already-clean path with no ".", ".." or ".git" element, at most 512
// bytes, and no NUL. It is resolved against the agent's working directory.
//
// ⚠ IT CHECKS THE PATH STRING ONLY. It keeps the lexical path inside the
// working directory; it does not stop a symlink at or under that path from
// pointing outside it, and the read follows symlinks.
func ValidResultPath(p string) bool {
	if p == "" || len(p) > 512 || strings.ContainsRune(p, 0) || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, el := range strings.Split(p, "/") {
		// .git anywhere: the result file is deleted after it is read, and a
		// repository's own metadata is never a job's output.
		if el == ".." || el == "." || strings.EqualFold(el, ".git") {
			return false
		}
	}
	return true
}

// Job is one row.
type Job struct {
	ID      int64
	AgentID int64 // 0 once the agent is deleted
	Agent   string
	// WorkerPool is the pool the job was submitted to ("" when it named its
	// agent); Agent is the member the pool chose.
	WorkerPool string
	Prompt     string
	Repo       string
	SHA        string
	// PR is the pull request a sha job's commit is on (0 without a sha).
	PR             int
	ResultPath     string
	TimeoutSeconds int
	IdempotencyKey string
	RequestedBy    string
	Status         string
	// Result is set only on a succeeded job (HasResult), verbatim.
	Result    string
	HasResult bool
	Reason    string
	Error     string
	CCAccount string
	SessionID int64
	CreatedAt time.Time
	// StartedAt and FinishedAt are zero until set.
	StartedAt  time.Time
	FinishedAt time.Time
}

// Terminal reports whether the job has finished.
func (j Job) Terminal() bool {
	return j.Status == StatusSucceeded || j.Status == StatusFailed || j.Status == StatusTimedOut
}

// NewJob is what a submission records.
type NewJob struct {
	AgentID        int64
	Agent          string
	WorkerPool     string
	Prompt         string
	Repo           string
	SHA            string
	PR             int
	ResultPath     string
	TimeoutSeconds int
	IdempotencyKey string
	RequestedBy    string
}

// Outcome is how a job ended: a terminal status, and either a result
// (succeeded) or a reason and an error message (failed, timed_out). A
// succeeded outcome may also carry an error: a note that does not change the
// status (its result file could not be deleted).
type Outcome struct {
	Status string
	Result string
	Reason string
	Error  string
}

// ErrNotFound is Get's answer for an id the caller cannot see.
var ErrNotFound = errors.New("jobs: no such job")

// ErrAgentAtCapacity is Create's answer when the agent already has
// MaxActivePerAgent jobs queued or running.
var ErrAgentAtCapacity = errors.New("jobs: the agent has too many jobs queued or running")

// ChooseFunc is CreateChosen's choice: given each candidate's queued and running
// jobs, the job to record, or an error that refuses the submission.
type ChooseFunc func(active map[int64]ActiveCount) (NewJob, error)

// Store is the jobs persistence.
type Store interface {
	// Create records a queued job. With an idempotency key the caller already
	// used, it records nothing and returns that job with created == false. It
	// refuses with ErrAgentAtCapacity when the agent already has
	// MaxActivePerAgent jobs queued or running (a repeated key is answered
	// first, so a retry of an accepted job is never refused).
	Create(ctx context.Context, n NewJob) (j Job, created bool, err error)
	// CreateChosen is Create for a job whose agent is chosen from candidates by
	// what they hold: it reads each candidate's queued and running jobs and
	// calls choose with them (an agent with none is absent), and records the
	// job choose returns, whose AgentID must be one of candidates and whose
	// IdempotencyKey must be n's. Nothing else can record a job on any of
	// candidates between the read and the insert, so concurrent submissions
	// each see the ones before them. With n's idempotency key already used it
	// answers that job (created == false) without calling choose. An error from
	// choose is returned unwrapped, and nothing is recorded.
	CreateChosen(ctx context.Context, n NewJob, candidates []int64, choose ChooseFunc) (j Job, created bool, err error)
	// Get is one of the caller's jobs, or ErrNotFound.
	Get(ctx context.Context, id int64) (Job, error)
	// ByIdempotencyKey is the caller's job recorded with key, or ErrNotFound.
	ByIdempotencyKey(ctx context.Context, key string) (Job, error)
	// Start moves a queued job to running, recording the Claude account and the
	// chat session the turn runs on. It reports false when the job was not
	// queued any more (the reaper failed it), and then the caller must not run
	// it.
	Start(ctx context.Context, id int64, ccAccount string, sessionID int64) (bool, error)
	// Finish moves a queued or running job to o's terminal status. It reports
	// false when the job had already finished, and changes nothing then.
	Finish(ctx context.Context, id int64, o Outcome) (bool, error)
	// ReapOverdue fails, with ReasonInterrupted and across every owner, each
	// job that is past its own deadline at now: running with started_at +
	// timeout_s + ReapSlack before now, or queued with created_at +
	// MaxQueueWait before now. It returns how many. A job inside its deadline
	// is never touched, so it is safe while another process is still running
	// jobs (a rolling update). It runs under the system principal whatever ctx
	// carries.
	ReapOverdue(ctx context.Context, now time.Time) (int64, error)
}

// Classify turns a finished turn into a job outcome. reply and err are the
// turn's; deadline reports whether the job's own deadline had passed when the
// turn returned (ctx.Err() == context.DeadlineExceeded). On success the reply
// is the result; a caller that returns a file instead replaces it afterwards.
//
// 🔴 THE ORDER IS THE CONTRACT. A deadline wins over the error it caused (an
// aborted request surfaces as some transport error, and "timed_out" is the fact
// a caller can act on). A runtime's TYPED failure is reported by its own token,
// unchanged — rate_limited and auth_failed are what a caller re-queues on, so
// they must never be folded into a generic failure.
func Classify(reply string, err error, deadline bool) Outcome {
	if err == nil {
		return Outcome{Status: StatusSucceeded, Result: reply}
	}
	if deadline {
		return Outcome{Status: StatusTimedOut, Reason: ReasonTimeout, Error: "the job's timeout elapsed before the agent's turn finished: " + err.Error()}
	}
	var rt *agents.RuntimeError
	if errors.As(err, &rt) && rt.Type != "" {
		msg := rt.Message
		if msg == "" {
			msg = err.Error()
		}
		status := StatusFailed
		// ccd's own turn budget ran out: the same fact as our deadline, measured
		// by the runtime.
		if rt.Type == "turn_timeout" {
			status = StatusTimedOut
		}
		return Outcome{Status: status, Reason: rt.Type, Error: msg}
	}
	return Outcome{Status: StatusFailed, Reason: ReasonTurnFailed, Error: err.Error()}
}

// Failed is a failed outcome.
func Failed(reason, msg string) Outcome {
	return Outcome{Status: StatusFailed, Reason: reason, Error: msg}
}

// TimedOut is a timed-out outcome.
func TimedOut(msg string) Outcome {
	return Outcome{Status: StatusTimedOut, Reason: ReasonTimeout, Error: msg}
}

// normalize enforces Outcome's shape against migration 0037's CHECKs, so a
// caller's mistake becomes a failed job rather than a row the database refuses
// (which would leave the job running for ever).
func normalize(o Outcome) Outcome {
	switch o.Status {
	case StatusSucceeded:
		if len(o.Result) > MaxResultBytes {
			return Failed(ReasonInternal, fmt.Sprintf("the result is %d bytes, over the %d-byte limit", len(o.Result), MaxResultBytes))
		}
		if strings.ContainsRune(o.Result, 0) {
			return Failed(ReasonInternal, "the result contains a NUL byte, which cannot be stored as text")
		}
		// A succeeded job carries no reason, but may carry an error: a note
		// about something that went wrong after the result was in hand (its
		// result file could not be deleted), which does not unmake the result.
		return Outcome{Status: StatusSucceeded, Result: o.Result, Error: truncateUTF8(strings.ReplaceAll(o.Error, "\x00", ""), MaxErrorBytes)}
	case StatusFailed, StatusTimedOut:
		if o.Reason == "" {
			o.Reason = ReasonInternal
		}
		if len(o.Reason) > 64 {
			o.Reason = o.Reason[:64]
		}
		o.Error = truncateUTF8(strings.ReplaceAll(o.Error, "\x00", ""), MaxErrorBytes)
		o.Result = ""
		return o
	}
	return Failed(ReasonInternal, fmt.Sprintf("a job cannot finish with status %q", o.Status))
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const jobColumns = `id, coalesce(agent_id, 0), agent_name, worker_pool, prompt, repo, sha, pr, result_path, timeout_s,
	coalesce(idempotency_key, ''), requested_by, status, result, reason, error, cc_account,
	coalesce(session_id, 0), created_at, started_at, finished_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var result *string
	var started, finished *time.Time
	if err := row.Scan(&j.ID, &j.AgentID, &j.Agent, &j.WorkerPool, &j.Prompt, &j.Repo, &j.SHA, &j.PR, &j.ResultPath, &j.TimeoutSeconds,
		&j.IdempotencyKey, &j.RequestedBy, &j.Status, &result, &j.Reason, &j.Error, &j.CCAccount,
		&j.SessionID, &j.CreatedAt, &started, &finished); err != nil {
		return Job{}, err
	}
	if result != nil {
		j.Result, j.HasResult = *result, true
	}
	if started != nil {
		j.StartedAt = *started
	}
	if finished != nil {
		j.FinishedAt = *finished
	}
	return j, nil
}

// Create records a queued job, or answers the caller's existing job for the
// same idempotency key.
//
// 🔴 THE CAPACITY COUNT AND THE INSERT ARE ONE TRANSACTION UNDER A PER-AGENT
// ADVISORY LOCK, so two concurrent submissions cannot both see room for one.
// MaxQueueWait is derived from the cap, so a cap that could be overshot would
// let the reaper fail a job that was legitimately still waiting.
func (s *PGStore) Create(ctx context.Context, n NewJob) (Job, bool, error) {
	return s.create(ctx, n, []int64{n.AgentID}, nil)
}

// CreateChosen records a job on the agent choose picks from candidates.
//
// 🔴 THE READ CHOOSE DECIDES ON AND THE INSERT ARE ONE TRANSACTION UNDER EVERY
// CANDIDATE'S PER-AGENT LOCK — the same lock Create takes for one agent. A pool
// choice read outside it let concurrent submissions all read the same queues
// and all pick the same worker: six submitted at once to two idle workers with
// four places each sent four to one, whose cap refused the other two with a
// 429 while the second worker sat idle. Under the locks each submission reads
// the queues the ones before it left, so choose sees every job it competes
// with, a named submission to a candidate included. The locks are taken in one
// global order (ascending key), so two choices over overlapping candidates
// cannot deadlock, and a named submission takes only one.
func (s *PGStore) CreateChosen(ctx context.Context, n NewJob, candidates []int64, choose ChooseFunc) (Job, bool, error) {
	if choose == nil || len(candidates) == 0 {
		return Job{}, false, errors.New("jobs: create chosen: no candidates or no choice")
	}
	return s.create(ctx, n, candidates, choose)
}

// lockKeys is the advisory lock keys of agentIDs (the per-agent key Create has
// always used), deduplicated, in ascending order.
func lockKeys(agentIDs []int64) []int32 {
	seen := map[int32]bool{}
	var keys []int32
	for _, id := range agentIDs {
		k := int32(id)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// create is Create and CreateChosen: under the per-agent locks of lockIDs, the
// idempotency answer, then (with choose) the choice, then the cap and the
// insert.
func (s *PGStore) create(ctx context.Context, n NewJob, lockIDs []int64, choose ChooseFunc) (Job, bool, error) {
	var key *string
	if n.IdempotencyKey != "" {
		key = &n.IdempotencyKey
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, false, fmt.Errorf("jobs: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, k := range lockKeys(lockIDs) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('jobs.create'), $1::int)`, k); err != nil {
			return Job{}, false, fmt.Errorf("jobs: create: lock: %w", err)
		}
	}
	// A key already used answers its job first: a retry of an accepted
	// submission is never refused for capacity. Under RLS this reads the
	// caller's own rows only.
	if key != nil {
		j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`, *key))
		if err == nil {
			return j, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, fmt.Errorf("jobs: read job for idempotency key: %w", err)
		}
	}
	if choose != nil {
		active, err := activeCounts(ctx, tx, lockIDs)
		if err != nil {
			return Job{}, false, err
		}
		chosen, err := choose(active)
		if err != nil {
			return Job{}, false, err
		}
		if !slices.Contains(lockIDs, chosen.AgentID) || chosen.IdempotencyKey != n.IdempotencyKey {
			return Job{}, false, fmt.Errorf("jobs: create chosen: the choice (agent %d, key %q) is not one of the candidates "+
				"locked for it, or changed the idempotency key", chosen.AgentID, chosen.IdempotencyKey)
		}
		n = chosen
	}
	var agentID *int64
	if n.AgentID != 0 {
		agentID = &n.AgentID
	}
	if agentID != nil {
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE agent_id = $1 AND status IN ('queued', 'running')`,
			*agentID).Scan(&active); err != nil {
			return Job{}, false, fmt.Errorf("jobs: create: count active: %w", err)
		}
		if active >= MaxActivePerAgent {
			return Job{}, false, fmt.Errorf("%w: %d queued or running on agent %q (the limit is %d)",
				ErrAgentAtCapacity, active, n.Agent, MaxActivePerAgent)
		}
	}
	j, err := scanJob(tx.QueryRow(ctx, `INSERT INTO jobs (agent_id, agent_name, worker_pool, prompt, repo, sha, pr, result_path,
		timeout_s, idempotency_key, requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (owner_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+jobColumns,
		agentID, n.Agent, n.WorkerPool, n.Prompt, n.Repo, n.SHA, n.PR, n.ResultPath, n.TimeoutSeconds, key, n.RequestedBy))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return Job{}, false, fmt.Errorf("jobs: create: commit: %w", err)
		}
		return j, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || key == nil {
		return Job{}, false, fmt.Errorf("jobs: create: %w", err)
	}
	// The key was taken since the read above, by a submission on another agent
	// (the lock is per agent): answer that job.
	j, err = scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`, *key))
	if err != nil {
		return Job{}, false, fmt.Errorf("jobs: read job for idempotency key: %w", err)
	}
	return j, false, nil
}

// Get is one of the caller's jobs.
func (s *PGStore) Get(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("jobs: get %d: %w", id, err)
	}
	return j, nil
}

// ByIdempotencyKey is the caller's job recorded with key.
func (s *PGStore) ByIdempotencyKey(ctx context.Context, key string) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("jobs: get by idempotency key: %w", err)
	}
	return j, nil
}

// ActiveCount is how many of one agent's jobs are queued and running.
type ActiveCount struct {
	Queued, Running int
}

// Active is Queued + Running: what MaxActivePerAgent bounds.
func (c ActiveCount) Active() int { return c.Queued + c.Running }

// activeCounts counts the caller's queued and running jobs per agent, inside
// CreateChosen's transaction and under its locks: what its choice is given. An
// agent with none is absent.
func activeCounts(ctx context.Context, tx pgx.Tx, agentIDs []int64) (map[int64]ActiveCount, error) {
	out := map[int64]ActiveCount{}
	if len(agentIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT agent_id, count(*) FILTER (WHERE status = 'queued'),
		count(*) FILTER (WHERE status = 'running')
		FROM jobs WHERE agent_id = ANY($1) AND status IN ('queued', 'running') GROUP BY agent_id`, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("jobs: count active: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c ActiveCount
		if err := rows.Scan(&id, &c.Queued, &c.Running); err != nil {
			return nil, fmt.Errorf("jobs: count active: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Start moves a queued job to running.
func (s *PGStore) Start(ctx context.Context, id int64, ccAccount string, sessionID int64) (bool, error) {
	var sess *int64
	if sessionID != 0 {
		sess = &sessionID
	}
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET status = 'running', started_at = now(), cc_account = $2, session_id = $3
		WHERE id = $1 AND status = 'queued'`, id, ccAccount, sess)
	if err != nil {
		return false, fmt.Errorf("jobs: start %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Finish moves a queued or running job to o's terminal status.
func (s *PGStore) Finish(ctx context.Context, id int64, o Outcome) (bool, error) {
	o = normalize(o)
	var result *string
	if o.Status == StatusSucceeded {
		result = &o.Result
	}
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET status = $2, result = $3, reason = $4, error = $5, finished_at = now()
		WHERE id = $1 AND status IN ('queued', 'running')`, id, o.Status, result, o.Reason, o.Error)
	if err != nil {
		return false, fmt.Errorf("jobs: finish %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// sys is ctx under the system principal: the reaper's one UPDATE, which spans
// every owner's jobs and returns only a count. It is internal/authz's System
// ledger entry for this package.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// The errors ReapOverdue records.
const (
	// AbandonedRunningError is a running job's.
	AbandonedRunningError = "nothing recorded this job's outcome by its deadline (timeout_s plus a grace period): " +
		"the server running it stopped, crashed or could not record the outcome. The agent may have done some " +
		"or all of the work, so re-submit it only if repeating that is safe"
	// AbandonedQueuedError is a queued job's.
	AbandonedQueuedError = "this job never started within the longest a queued job can wait: the server holding " +
		"it stopped or crashed before running it. Nothing was sent to the agent for it"
)

// The errors a graceful shutdown records on the jobs the stopping process held
// (internal/api FailOwnJobs).
const (
	// ShutdownQueuedError is a queued job's.
	ShutdownQueuedError = "the server holding this job shut down before starting it. Nothing was sent to the agent for it, so it is safe to re-submit"
	// ShutdownRunningError is a running job's.
	ShutdownRunningError = "the server running this job shut down before the agent's turn finished. The agent may have done some " +
		"or all of the work, so re-submit it only if repeating that is safe"
)

// ReapOverdue fails every job past its own deadline at now, across every
// owner, under the system principal (sys) whatever ctx carries.
func (s *PGStore) ReapOverdue(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(sys(ctx), `UPDATE jobs SET status = 'failed', reason = $1, finished_at = now(),
		error = CASE WHEN status = 'running' THEN $2 ELSE $3 END
		WHERE (status = 'running' AND started_at + make_interval(secs => timeout_s + $4::double precision) < $5)
		   OR (status = 'queued' AND created_at + make_interval(secs => $6::double precision) < $5)`,
		ReasonInterrupted, AbandonedRunningError, AbandonedQueuedError,
		ReapSlack.Seconds(), now, MaxQueueWait.Seconds())
	if err != nil {
		return 0, fmt.Errorf("jobs: reap overdue: %w", err)
	}
	return tag.RowsAffected(), nil
}
