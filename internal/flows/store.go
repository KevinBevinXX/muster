package flows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/metrics"
)

// Status is a job's state. The set is closed (the 0012 CHECK).
type Status string

const (
	StatusQueued        Status = "queued"
	StatusRunning       Status = "running"
	StatusDone          Status = "done"
	StatusFailed        Status = "failed"
	StatusNeedsOperator Status = "needs_operator"
	StatusExpired       Status = "expired"
	StatusCancelled     Status = "cancelled"
)

// Terminal reports whether no further transition can happen.
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// Job is one row of flow_jobs.
type Job struct {
	ID           int64             `json:"id"`
	OwnerID      int64             `json:"ownerId"`
	Flow         string            `json:"flow"`
	Class        Class             `json:"class"`
	Host         string            `json:"host"`
	Instance     string            `json:"instance"`
	Params       map[string]string `json:"params"`
	Status       Status            `json:"status"`
	RequestedVia string            `json:"requestedVia"`
	RequestedBy  string            `json:"requestedBy"`
	Attempts     int               `json:"attempts"`
	CreatedAt    time.Time         `json:"createdAt"`
	ExpiresAt    time.Time         `json:"expiresAt"`
	ClaimedAt    *time.Time        `json:"claimedAt,omitempty"`
	LeaseUntil   *time.Time        `json:"leaseUntil,omitempty"`
	FinishedAt   *time.Time        `json:"finishedAt,omitempty"`
	ExitCode     *int              `json:"exitCode,omitempty"`
	Output       string            `json:"output"`
	Detail       string            `json:"detail"`
}

// Errors the API maps to status codes.
var (
	// ErrNotFound: no such job FOR THIS PRINCIPAL. Another owner's job is
	// indistinguishable from one that does not exist (row-level security).
	ErrNotFound = errors.New("flows: job not found")
	// ErrState: the job exists but is not in a state that allows this.
	ErrState = errors.New("flows: the job is not in a state that allows this")
	// ErrPendingCap: the owner already has MaxPendingPerOwner unfinished jobs.
	ErrPendingCap = errors.New("flows: too many unfinished jobs")
	// ErrBadRunnerToken: unknown, revoked, or its owner is disabled.
	ErrBadRunnerToken = errors.New("flows: unknown or revoked runner token")
)

// MaxPendingPerOwner bounds an owner's queued + running jobs. One browser runs
// one flow at a time, so a deep queue is only ever a stuck runner or a loop.
const MaxPendingPerOwner = 20

// LeaseDuration is how long a claim (or a heartbeat) holds a job. A runner
// heartbeats well inside it; a runner that goes silent loses the job when it
// lapses — a READ job is queued again, a WRITE job goes to needs_operator.
const LeaseDuration = 2 * time.Minute

// maxReadAttempts bounds how many times a read job is re-queued after a lost
// lease before it is failed instead.
const maxReadAttempts = 3

// MaxOutputBytes caps a job's stored output (the 0012 CHECK); a longer report
// is truncated at a UTF-8 boundary, never refused.
const MaxOutputBytes = 65536

// maxDetailBytes caps the stored detail line (the 0012 CHECK).
const maxDetailBytes = 4096

// Outcome is what a runner may report. A job is never reported back to
// queued: re-running is the server's decision (a lost READ lease), never the
// runner's.
type Outcome string

const (
	OutcomeDone          Outcome = "done"
	OutcomeFailed        Outcome = "failed"
	OutcomeNeedsOperator Outcome = "needs_operator"
)

// Result is a runner's report.
type Result struct {
	Outcome  Outcome `json:"outcome"`
	ExitCode *int    `json:"exitCode"`
	Output   string  `json:"output"`
	Detail   string  `json:"detail"`
}

// Store is the flow queue and the runner-token table.
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL, AND BOTH TABLES ARE UNDER
// FORCED ROW-LEVEL SECURITY (0012). An enqueue lands owned by the caller; the
// runner door acts as the runner token's OWNER, so Claim/Heartbeat/Report can
// only ever see that owner's jobs. AuthenticateRunner is the one exception:
// it must find a token by its hash before anyone is known, so the API calls
// it under authz.System (a System-ledger entry), and only the token's owner
// and host are handed on.
type Store interface {
	Enqueue(ctx context.Context, r Request, via, by string) (Job, error)
	List(ctx context.Context, limit int) ([]Job, error)
	Get(ctx context.Context, id int64) (Job, error)
	Cancel(ctx context.Context, id int64) (Job, error)
	// Claim takes the oldest queued, unexpired job for host on one of
	// instances, unless that (owner, host, instance) already has a running job.
	// ok is false when there is nothing to take.
	Claim(ctx context.Context, host string, instances []string, runnerTokenID int64) (Job, bool, error)
	Heartbeat(ctx context.Context, id int64, host string) (Job, error)
	Report(ctx context.Context, id int64, host string, res Result) (Job, error)

	// Agent requests (0014; requests.go): filed by an agent, decided by its
	// owner in a browser session.
	FileRequest(ctx context.Context, agentID, ownerID int64, agentName string, r Request, reason string) (FlowRequest, error)
	RequestForAgent(ctx context.Context, agentID, id int64) (FlowRequest, error)
	ListRequests(ctx context.Context, limit int) ([]FlowRequest, error)
	DecideRequest(ctx context.Context, id int64, approve bool, by string) (FlowRequest, *Job, error)

	AuthenticateRunner(ctx context.Context, token string) (RunnerToken, error)
	CreateRunnerToken(ctx context.Context, host string) (string, RunnerToken, error)
	ListRunnerTokens(ctx context.Context) ([]RunnerToken, error)
	RevokeRunnerToken(ctx context.Context, id int64) (bool, error)
}

// PGStore is Store over Postgres (migration 0012).
type PGStore struct {
	pool *pgxpool.Pool
	// denyWords are the deployment's infrastructure names an AGENT's free
	// text may not carry (WithAgentDenyWords; requests.go).
	denyWords []string
}

// WithAgentDenyWords sets the deployment's own infrastructure names that an
// agent-composed message may not contain (MUSTER_FLOW_AGENT_DENY_WORDS).
func (s *PGStore) WithAgentDenyWords(words []string) *PGStore {
	s.denyWords = append([]string(nil), words...)
	return s
}

// NewPG builds the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const jobCols = `id, owner_id, flow, class, host, instance, params, status, requested_via, requested_by,
	attempts, created_at, expires_at, claimed_at, lease_until, finished_at, exit_code, output, detail`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var params []byte
	err := row.Scan(&j.ID, &j.OwnerID, &j.Flow, &j.Class, &j.Host, &j.Instance, &params, &j.Status,
		&j.RequestedVia, &j.RequestedBy, &j.Attempts, &j.CreatedAt, &j.ExpiresAt, &j.ClaimedAt,
		&j.LeaseUntil, &j.FinishedAt, &j.ExitCode, &j.Output, &j.Detail)
	if err != nil {
		return Job{}, err
	}
	j.Params = map[string]string{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &j.Params); err != nil {
			return Job{}, fmt.Errorf("flows: job %d params: %w", j.ID, err)
		}
	}
	return j, nil
}

func count(flow, outcome string) { metrics.FlowJobs.WithLabelValues(flow, outcome).Inc() }

// Enqueue validates r against the catalogue and queues it, owned by ctx's
// principal. via and by are the audit facts (the door, and a label for who).
func (s *PGStore) Enqueue(ctx context.Context, r Request, via, by string) (Job, error) {
	f, err := Validate(&r)
	if err != nil {
		return Job{}, err
	}
	if err := s.sweep(ctx); err != nil {
		return Job{}, err
	}
	params, err := json.Marshal(r.Params)
	if err != nil {
		return Job{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	j, err := enqueueTx(ctx, tx, f, r, params, via, by)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, err
	}
	count(j.Flow, "enqueued")
	return j, nil
}

// enqueueTx inserts one validated job inside tx, under the pending cap. It is
// the ONE insert into flow_jobs: Enqueue (an owner's request) and an approved
// agent request (DecideRequest) both go through it.
func enqueueTx(ctx context.Context, tx pgx.Tx, f Flow, r Request, params []byte, via, by string) (Job, error) {
	// The cap is per owner; serialise this owner's enqueues so two concurrent
	// ones cannot both pass the count. The lock key is the owner id the row
	// will carry (the column default), read back from the session setting.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('flow_jobs.enqueue'),
		nullif(current_setting('muster.uid', true), '')::int)`); err != nil {
		return Job{}, err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM flow_jobs WHERE status IN ('queued', 'running')`).Scan(&pending); err != nil {
		return Job{}, err
	}
	if pending >= MaxPendingPerOwner {
		return Job{}, fmt.Errorf("%w: %d queued or running (the limit is %d)", ErrPendingCap, pending, MaxPendingPerOwner)
	}
	return scanJob(tx.QueryRow(ctx, `
		INSERT INTO flow_jobs (flow, class, host, instance, params, requested_via, requested_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8))
		RETURNING `+jobCols,
		f.Name, string(f.Class), r.Host, r.Instance, params, via, truncateRunes(by, 100), r.TTL.Seconds()))
}

// 🔴 THE TTL DOES NOT RUN WHILE A JOB IS BLOCKED (migration 0044, PR #228
// round-1 F1). A claim refuses a job while a job of the same owner, host and
// instance is running, and a muse dispatch may hold that instance for over an
// hour; a job queued behind it must not expire merely for waiting. So a queued
// job's deadline is expires_at + ttl_paused, the sweep never expires it while
// it is blocked (blockedBy), and when the blocker leaves `running` the same
// statement adds the blocked time to each job still queued behind it
// (resumeBehind). Doing it in the transition's own statement is what makes it
// race-safe: no other statement can observe the blocker gone and the pause not
// yet applied.

// MaxTTLPause is the ABSOLUTE ceiling on the ttl credit a queued job earns
// while blocked (round-2 F-A). Without it a runner that keeps heartbeating a
// wedged flow — nothing caps how long a job may run — would hold every job
// behind it forever, and the owner's MaxPendingPerOwner would never clear.
// Sized from the longest LEGITIMATE hold of one instance, a muse dispatch:
// the pacing gap waited inside the job (10 min, the muse wrapper's default
// MUSE_MIN_SEND_GAP_MIN) + MaxWaitSeconds (60 min) + the runner's 240 s slack
// = 74 min, plus 16 min of headroom = 90 min. A queued job therefore runs no
// later than expires_at + 90 min, however long it is blocked: the sweep
// expires it at that point even while the blocker is still running, and the
// credit added when a blocker finishes is capped at the same 90 min.
const MaxTTLPause = 90 * time.Minute

// maxTTLPauseSQL is MaxTTLPause as an SQL interval literal (a constant, not
// input, so it is spliced rather than bound).
var maxTTLPauseSQL = fmt.Sprintf("interval '%d seconds'", int64(MaxTTLPause.Seconds()))

// blockedBy is the NOT EXISTS body: a running job holds q's browser instance.
const blockedBy = `SELECT 1 FROM flow_jobs r
		                        WHERE r.status = 'running' AND r.owner_id = q.owner_id
		                          AND r.host = q.host AND r.instance = q.instance`

// resumeBehind, inside a statement with a CTE `fin` (the rows that just left
// running, with owner_id, host, instance and claimed_at), adds to each job
// still queued on the same instance the time that job blocked it.
var resumeBehind = `UPDATE flow_jobs q SET ttl_paused = least(q.ttl_paused + greatest(now() - greatest(q.created_at, fin.claimed_at), interval '0'), ` + maxTTLPauseSQL + `)
		  FROM fin
		 WHERE q.status = 'queued' AND q.owner_id = fin.owner_id
		   AND q.host = fin.host AND q.instance = fin.instance`

// sweep applies the two time-driven transitions to the CALLER's rows (row-level
// security scopes the UPDATE): a queued job past its TTL becomes expired, and a
// running job whose lease lapsed is re-queued (read, attempts left, TTL left),
// failed (read, attempts spent), expired (read, TTL spent) or handed to a human
// (write — it may already have been delivered, so it is never re-run).
//
// 🔴 IT IS LAZY, AND THAT IS THE DESIGN, NOT A SHORTCUT. Every read and every
// claim sweeps first, under the caller's principal, so no background loop needs
// authz.System to act across users, and a job's displayed state is never stale
// by more than the request that shows it. The claim's own WHERE clause refuses
// an expired job regardless, so "never runs late" does not depend on a sweep
// having happened.
func (s *PGStore) sweep(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		UPDATE flow_jobs q SET status = 'expired', finished_at = now(),
		       detail = CASE WHEN EXISTS (`+blockedBy+`)
		           THEN 'not claimed within its ttl plus `+strconv.Itoa(int(MaxTTLPause.Minutes()))+` minutes of credit for waiting blocked behind a running job on the same browser; it will not run later than that'
		           ELSE 'not claimed before its ttl (plus any credit for time spent blocked); it will not run later than that' END
		 WHERE q.status = 'queued'
		   AND ((q.expires_at + q.ttl_paused <= now() AND NOT EXISTS (`+blockedBy+`))
		        OR q.expires_at + `+maxTTLPauseSQL+` <= now())
		RETURNING q.flow`)
	if err != nil {
		return fmt.Errorf("flows: sweep ttl: %w", err)
	}
	if err := countRows(rows, "expired"); err != nil {
		return err
	}
	rows, err = s.pool.Query(ctx, `
		WITH fin AS (
		UPDATE flow_jobs SET
		       status = CASE
		           WHEN class = 'write' THEN 'needs_operator'
		           WHEN expires_at + ttl_paused <= now() THEN 'expired'
		           WHEN attempts >= $1 THEN 'failed'
		           ELSE 'queued' END,
		       lease_until = NULL,
		       finished_at = CASE
		           WHEN class = 'read' AND expires_at + ttl_paused > now() AND attempts < $1 THEN NULL
		           ELSE now() END,
		       detail = CASE
		           WHEN class = 'write' THEN 'the runner stopped reporting mid-run; this write may have been delivered, so it is not re-run — check by hand'
		           WHEN expires_at + ttl_paused <= now() THEN 'the runner stopped reporting mid-run, and the ttl has passed'
		           WHEN attempts >= $1 THEN 'the runner stopped reporting mid-run on every attempt'
		           ELSE 'the runner stopped reporting mid-run; re-queued' END
		 WHERE status = 'running' AND lease_until < now()
		RETURNING flow, status, owner_id, host, instance, claimed_at),
		paused AS (`+resumeBehind+`)
		SELECT flow, status FROM fin`, maxReadAttempts)
	if err != nil {
		return fmt.Errorf("flows: sweep leases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var flow, st string
		if err := rows.Scan(&flow, &st); err != nil {
			return err
		}
		if st == string(StatusQueued) {
			st = "requeued"
		}
		count(flow, st)
	}
	return rows.Err()
}

func countRows(rows pgx.Rows, outcome string) error {
	defer rows.Close()
	for rows.Next() {
		var flow string
		if err := rows.Scan(&flow); err != nil {
			return err
		}
		count(flow, outcome)
	}
	return rows.Err()
}

// List returns the caller's newest jobs, at most limit (1..200).
func (s *PGStore) List(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if err := s.sweep(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM flow_jobs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get returns one of the caller's jobs.
func (s *PGStore) Get(ctx context.Context, id int64) (Job, error) {
	if err := s.sweep(ctx); err != nil {
		return Job{}, err
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM flow_jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Cancel withdraws a queued job, or dismisses one that needs the operator. A
// running job cannot be cancelled from here: the runner owns it until it
// reports or its lease lapses (stop the runner unit to stop it now).
func (s *PGStore) Cancel(ctx context.Context, id int64) (Job, error) {
	if err := s.sweep(ctx); err != nil {
		return Job{}, err
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `
		UPDATE flow_jobs SET status = 'cancelled', finished_at = now(),
		       detail = CASE WHEN status = 'needs_operator' THEN 'dismissed by the owner: ' || detail
		                     ELSE 'cancelled by the owner before it was claimed' END
		 WHERE id = $1 AND status IN ('queued', 'needs_operator')
		RETURNING `+jobCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, s.notFoundOrState(ctx, id)
	}
	if err != nil {
		return Job{}, err
	}
	count(j.Flow, "cancelled")
	return j, nil
}

// notFoundOrState tells "no such job (for you)" from "your job, wrong state".
func (s *PGStore) notFoundOrState(ctx context.Context, id int64) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM flow_jobs WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrState
	}
	return ErrNotFound
}

// Claim — see Store. 🔴 TWO GUARDS AGAINST A SECOND RUNNING JOB ON ONE
// BROWSER: the NOT EXISTS here (the common case, no error), and the 0012
// partial unique index, which makes the loser of a true race fail with a
// unique violation — reported as "nothing to take", never as a second job.
func (s *PGStore) Claim(ctx context.Context, host string, instances []string, runnerTokenID int64) (Job, bool, error) {
	if err := ValidHost(host); err != nil {
		return Job{}, false, err
	}
	if len(instances) == 0 {
		return Job{}, false, invalidf("a claim names at least one connected instance")
	}
	for _, in := range instances {
		if err := ValidInstance(in); err != nil {
			return Job{}, false, err
		}
	}
	if err := s.sweep(ctx); err != nil {
		return Job{}, false, err
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `
		WITH cand AS (
		    SELECT q.id FROM flow_jobs q
		     WHERE q.status = 'queued' AND q.host = $1 AND q.instance = ANY($2::text[])
		       AND q.expires_at + q.ttl_paused > now()
		       AND NOT EXISTS (`+blockedBy+`)
		     ORDER BY q.id
		     LIMIT 1
		     FOR UPDATE SKIP LOCKED)
		UPDATE flow_jobs j SET status = 'running', claimed_at = now(),
		       lease_until = now() + make_interval(secs => $3), attempts = j.attempts + 1,
		       runner_token_id = $4, detail = ''
		  FROM cand WHERE j.id = cand.id AND j.status = 'queued'
		RETURNING `+prefixed("j", jobCols), host, instances, LeaseDuration.Seconds(), runnerTokenID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "flow_jobs_one_running" {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	count(j.Flow, "claimed")
	return j, true, nil
}

// Heartbeat extends a running job's lease. host is the runner token's host: a
// runner can only touch jobs addressed to its own host.
//
// ⚠ A LAPSED LEASE THAT NO SWEEP HAS ACTED ON YET IS STILL EXTENDED (as a
// Report is still accepted): whichever arrives first — the runner, or a sweep
// run by some read — decides. Once a sweep has moved the job, both answer
// ErrState and the runner's late result is not recorded.
func (s *PGStore) Heartbeat(ctx context.Context, id int64, host string) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `
		UPDATE flow_jobs SET lease_until = now() + make_interval(secs => $3)
		 WHERE id = $1 AND host = $2 AND status = 'running'
		RETURNING `+jobCols, id, host, LeaseDuration.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, s.notFoundOrStateOnHost(ctx, id, host)
	}
	return j, err
}

// notFoundOrStateOnHost is notFoundOrState for the runner door: a job on
// another host of the same owner is "not found" to this runner, exactly like
// another owner's.
func (s *PGStore) notFoundOrStateOnHost(ctx context.Context, id int64, host string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM flow_jobs WHERE id = $1 AND host = $2)`, id, host).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrState
	}
	return ErrNotFound
}

// Report finishes a running job with the runner's result.
func (s *PGStore) Report(ctx context.Context, id int64, host string, res Result) (Job, error) {
	switch res.Outcome {
	case OutcomeDone, OutcomeFailed, OutcomeNeedsOperator:
	default:
		return Job{}, invalidf("outcome %q is not one of done, failed, needs_operator", res.Outcome)
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `
		WITH fin AS (
		UPDATE flow_jobs SET status = $3, finished_at = now(), lease_until = NULL,
		       exit_code = $4, output = $5, detail = $6
		 WHERE id = $1 AND host = $2 AND status = 'running'
		RETURNING *),
		paused AS (`+resumeBehind+`)
		SELECT `+jobCols+` FROM fin`, id, host, string(res.Outcome), res.ExitCode,
		truncateBytes(res.Output, MaxOutputBytes), truncateBytes(res.Detail, maxDetailBytes)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, s.notFoundOrStateOnHost(ctx, id, host)
	}
	if err != nil {
		return Job{}, err
	}
	count(j.Flow, string(j.Status))
	return j, nil
}

// --- runner tokens -----------------------------------------------------------

// RunnerTokenPrefix starts every runner token, so the runner door can refuse
// every other credential (the hook token, a personal token) before a database
// read.
const RunnerTokenPrefix = "mfr_"

// RunnerToken is one minted runner token's record; the token itself is never
// stored.
type RunnerToken struct {
	ID        int64      `json:"id"`
	OwnerID   int64      `json:"ownerId"`
	Host      string     `json:"host"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsedAt,omitempty"`
}

// HashRunnerToken is the stored form of a runner token.
func HashRunnerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewRunnerToken mints RunnerTokenPrefix + 43 base64url characters (256 bits).
func NewRunnerToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("flows: mint a runner token: %w", err)
	}
	return RunnerTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// RunnerTokenShapeOK reports whether token is shaped like one NewRunnerToken
// mints. A FILTER, not authentication: the hash lookup is the property.
func RunnerTokenShapeOK(token string) bool {
	rest, ok := strings.CutPrefix(token, RunnerTokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

const runnerCols = `id, owner_id, host, created_at, revoked_at, last_used_at`

func scanRunner(row pgx.Row) (RunnerToken, error) {
	var t RunnerToken
	err := row.Scan(&t.ID, &t.OwnerID, &t.Host, &t.CreatedAt, &t.RevokedAt, &t.LastUsed)
	return t, err
}

// AuthenticateRunner resolves a runner token to its record and stamps its use.
// A revoked token, an unknown one and one whose owner is disabled are all
// ErrBadRunnerToken.
func (s *PGStore) AuthenticateRunner(ctx context.Context, token string) (RunnerToken, error) {
	if !RunnerTokenShapeOK(token) {
		return RunnerToken{}, ErrBadRunnerToken
	}
	t, err := scanRunner(s.pool.QueryRow(ctx, `
		UPDATE flow_runner_tokens t SET last_used_at = now()
		  FROM users u
		 WHERE t.token_hash = $1 AND t.revoked_at IS NULL
		   AND u.id = t.owner_id AND u.disabled_at IS NULL
		RETURNING `+prefixed("t", runnerCols), HashRunnerToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunnerToken{}, ErrBadRunnerToken
	}
	return t, err
}

// CreateRunnerToken mints a token for host, owned by ctx's principal, and
// returns it ONCE.
func (s *PGStore) CreateRunnerToken(ctx context.Context, host string) (string, RunnerToken, error) {
	if err := ValidHost(host); err != nil {
		return "", RunnerToken{}, err
	}
	tok, err := NewRunnerToken()
	if err != nil {
		return "", RunnerToken{}, err
	}
	t, err := scanRunner(s.pool.QueryRow(ctx, `
		INSERT INTO flow_runner_tokens (host, token_hash) VALUES ($1, $2) RETURNING `+runnerCols,
		host, HashRunnerToken(tok)))
	if err != nil {
		return "", RunnerToken{}, err
	}
	return tok, t, nil
}

// ListRunnerTokens lists the caller's runner tokens, newest first.
func (s *PGStore) ListRunnerTokens(ctx context.Context) ([]RunnerToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runnerCols+` FROM flow_runner_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunnerToken{}
	for rows.Next() {
		t, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeRunnerToken revokes one of the caller's runner tokens, reporting
// whether the caller has such a token at all.
func (s *PGStore) RevokeRunnerToken(ctx context.Context, id int64) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `
		WITH upd AS (UPDATE flow_runner_tokens SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1 RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM upd)`, id).Scan(&found)
	return found, err
}

// --- helpers -----------------------------------------------------------------

// prefixed qualifies a column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
