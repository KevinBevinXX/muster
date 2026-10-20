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
// 🔴 EVERY METHOD EXCEPT FailInterrupted RUNS UNDER THE CALLER'S PRINCIPAL.
// jobs is under FORCE row-level security, so a caller reads and writes only its
// own user's rows; the owner of a row is the principal's user (owner_id's
// default), never a parameter. FailInterrupted is the boot sweep and needs the
// system scope (authz.System).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
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
	// ReasonInterrupted: the server stopped while the job was queued or running.
	ReasonInterrupted = "interrupted"
	// ReasonAgentUnavailable: the agent the job names could not be loaded when it
	// was due to start (deleted since, or the store failed).
	ReasonAgentUnavailable = "agent_unavailable"
	// ReasonCheckoutFailed: moving the agent's checkout to the job's sha failed.
	ReasonCheckoutFailed = "checkout_failed"
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
)

// shaPattern is a commit id a job may name: 7–40 lowercase hex. Migration
// 0037's jobs_sha_check spells the same expression.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ValidSHA reports whether s is a commit id a job may name.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }

// ValidResultPath reports whether p may be a job's result_path: a non-empty,
// relative, already-clean path with no "." or ".." element, at most 512 bytes,
// and no NUL. It is resolved against the agent's working directory, and this
// is what keeps it there.
func ValidResultPath(p string) bool {
	if p == "" || len(p) > 512 || strings.ContainsRune(p, 0) || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." || el == "." {
			return false
		}
	}
	return true
}

// Job is one row.
type Job struct {
	ID             int64
	AgentID        int64 // 0 once the agent is deleted
	Agent          string
	Prompt         string
	Repo           string
	SHA            string
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
	Prompt         string
	Repo           string
	SHA            string
	ResultPath     string
	TimeoutSeconds int
	IdempotencyKey string
	RequestedBy    string
}

// Outcome is how a job ended: a terminal status, and either a result
// (succeeded) or a reason and an error message (failed, timed_out).
type Outcome struct {
	Status string
	Result string
	Reason string
	Error  string
}

// ErrNotFound is Get's answer for an id the caller cannot see.
var ErrNotFound = errors.New("jobs: no such job")

// Store is the jobs persistence.
type Store interface {
	// Create records a queued job. With an idempotency key the caller already
	// used, it records nothing and returns that job with created == false.
	Create(ctx context.Context, n NewJob) (j Job, created bool, err error)
	// Get is one of the caller's jobs, or ErrNotFound.
	Get(ctx context.Context, id int64) (Job, error)
	// Start moves a queued job to running, recording the Claude account and the
	// chat session the turn runs on. It reports false when the job was not
	// queued any more (the boot sweep failed it), and then the caller must not
	// run it.
	Start(ctx context.Context, id int64, ccAccount string, sessionID int64) (bool, error)
	// Finish moves a queued or running job to o's terminal status. It reports
	// false when the job had already finished, and changes nothing then.
	Finish(ctx context.Context, id int64, o Outcome) (bool, error)
	// FailInterrupted fails every queued or running job with
	// ReasonInterrupted, across every owner, returning how many. It is the boot
	// sweep: no job survives a restart of the process that was running it. It
	// needs the system scope.
	FailInterrupted(ctx context.Context) (int64, error)
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
		return Outcome{Status: StatusSucceeded, Result: o.Result}
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

const jobColumns = `id, coalesce(agent_id, 0), agent_name, prompt, repo, sha, result_path, timeout_s,
	coalesce(idempotency_key, ''), requested_by, status, result, reason, error, cc_account,
	coalesce(session_id, 0), created_at, started_at, finished_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var result *string
	var started, finished *time.Time
	if err := row.Scan(&j.ID, &j.AgentID, &j.Agent, &j.Prompt, &j.Repo, &j.SHA, &j.ResultPath, &j.TimeoutSeconds,
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
func (s *PGStore) Create(ctx context.Context, n NewJob) (Job, bool, error) {
	var key *string
	if n.IdempotencyKey != "" {
		key = &n.IdempotencyKey
	}
	var agentID *int64
	if n.AgentID != 0 {
		agentID = &n.AgentID
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `INSERT INTO jobs (agent_id, agent_name, prompt, repo, sha, result_path,
		timeout_s, idempotency_key, requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (owner_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+jobColumns,
		agentID, n.Agent, n.Prompt, n.Repo, n.SHA, n.ResultPath, n.TimeoutSeconds, key, n.RequestedBy))
	if err == nil {
		return j, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || key == nil {
		return Job{}, false, fmt.Errorf("jobs: create: %w", err)
	}
	// The key was taken: under RLS this reads the caller's own row only.
	j, err = scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`, *key))
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

// InterruptedError is the error FailInterrupted records.
const InterruptedError = "the server stopped before this job finished; the agent may have done some or all of the work, so re-submit it only if repeating that is safe"

// FailInterrupted fails every unfinished job, across every owner.
func (s *PGStore) FailInterrupted(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET status = 'failed', reason = $1, error = $2, finished_at = now()
		WHERE status IN ('queued', 'running')`, ReasonInterrupted, InterruptedError)
	if err != nil {
		return 0, fmt.Errorf("jobs: fail interrupted: %w", err)
	}
	return tag.RowsAffected(), nil
}
