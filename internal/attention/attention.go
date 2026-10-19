// Package attention is the ATTENTION QUEUE's explicit asks (migration 0025): a
// question an agent — a dispatched agent or a mirrored Claude Code session —
// puts to its owner, and the owner's answer.
//
// The attention VIEW (internal/api) is wider: it also lists mirrored sessions
// waiting on the person and reply drafts awaiting approval. Those are derived
// live from the state that owns them; only the explicit asks are stored, here.
//
// 🔴 THE STORE HAS TWO HALVES AND THEY RUN UNDER DIFFERENT PRINCIPALS. Ask,
// Get, Open, Answer and Cancel run under the CALLER's principal: attention_asks
// is under FORCE row-level security, so a request sees, asks into and answers
// only its own user's rows, and the owner of a new row is the principal's user
// (owner_id's default), never a parameter. ExpireDue runs under the
// system scope (sys): the sweep moves every owner's lapsed asks and hands each
// back with its owner, so the caller can notify that owner and no other. The
// delivery sweep's two (DueDeliveries, RecoverDeliveries) are the same shape:
// every owner's rows, each handed back with its owner, so the caller delivers
// under THAT owner's principal.
//
// 🔴 AN ANSWER TO AN AGENT IS DELIVERED (migration 0031). Answer queues the
// delivery in the same statement; the caller (internal/api) pushes it into the
// asker's session as a turn exactly once, unless the asker read it with
// attention_wait first (MarkWaited) — see the Delivery states.
//
// 🔴 AN ASK LEAVES "open" ONCE. Answer, Cancel and ExpireDue are each one
// UPDATE conditional on status = 'open' (and, for an answer or a cancel, on the
// ask not having lapsed), so a concurrent answer and cancel cannot both land:
// the loser reads ErrNotOpen and the row as the winner left it.
package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Status is where an ask is.
type Status string

const (
	Open      Status = "open"
	Answered  Status = "answered"
	Cancelled Status = "cancelled"
	Expired   Status = "expired"
	// AnsweredInSession: the operator wrote to the asker directly — a message
	// into the mirrored session's window, or to the dispatched agent's chat —
	// after the ask was raised (AnswerInSession, migration 0026). The message is
	// the answer.
	AnsweredInSession Status = "answered_in_session"
)

// ByOperator is who answers an ask: the operator, on the attention view. No
// agent answers one — not even the chief.
const ByOperator = "operator"

// The bounds, each also a CHECK in 0025.
const (
	MaxQuestionRunes = 4000
	MaxOptions       = 10
	MaxOptionRunes   = 200
	MaxAnswerRunes   = 8000
	MaxLabelRunes    = 200
	// MaxOpenPerOwner bounds one owner's open asks: an agent asking in a loop
	// would otherwise fill the view (and the owner's lock screen) without limit.
	MaxOpenPerOwner = 50
	// DefaultTimeout is how long an ask waits for an answer when the asker
	// names no timeout; MinTimeout and MaxTimeout clamp one it does name.
	DefaultTimeout = 24 * time.Hour
	MinTimeout     = time.Minute
	MaxTimeout     = 7 * 24 * time.Hour
)

var (
	// ErrInvalid: the ask or answer breaks a bound (its text says which).
	ErrInvalid = errors.New("attention: invalid")
	// ErrNotFound: no such ask among the caller's.
	ErrNotFound = errors.New("attention: no such ask")
	// ErrNotOpen: the ask was already answered, cancelled or expired.
	ErrNotOpen = errors.New("attention: the ask is no longer open")
	// ErrTooMany: the owner already has MaxOpenPerOwner open asks.
	ErrTooMany = fmt.Errorf("attention: there are already %d open asks; wait for some to be answered", MaxOpenPerOwner)
)

// SystemAskerLabel is the label of an ask muster itself raised.
const SystemAskerLabel = "muster"

// Asker is who raised an ask: a dispatched agent (AgentID), a mirrored Claude
// Code session (MirrorHost + MirrorSession, the Claude session id), or muster
// itself (System: an ask nobody waits on, migration 0028). Exactly one is set.
type Asker struct {
	AgentID       int64
	MirrorHost    string
	MirrorSession string
	// System: muster itself raised the ask (no agent, no session).
	System bool
	// Label is the name the view shows beside the question.
	Label string
}

// Valid reports whether a is exactly one kind of asker with valid fields.
func (a Asker) Valid() error {
	mirrored := a.MirrorHost != "" || a.MirrorSession != ""
	switch {
	case a.AgentID > 0 && mirrored, a.System && (a.AgentID > 0 || mirrored):
		return fmt.Errorf("%w: an ask has one asker", ErrInvalid)
	case a.System, a.AgentID > 0:
	case mirrored:
		if err := mirror.ValidHost(a.MirrorHost); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if !mirror.ValidClaudeSession(a.MirrorSession) {
			return fmt.Errorf("%w: not a Claude session id", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: an ask needs an asker", ErrInvalid)
	}
	return nil
}

// Same reports whether b is the same asker as a (the label aside).
func (a Asker) Same(b Asker) bool {
	return a.AgentID == b.AgentID && a.MirrorHost == b.MirrorHost && a.MirrorSession == b.MirrorSession
}

// Ask is one explicit ask.
type Ask struct {
	ID         int64     `json:"id"`
	Owner      int64     `json:"-"`
	Asker      Asker     `json:"-"`
	Question   string    `json:"question"`
	Options    []string  `json:"options"`
	Status     Status    `json:"status"`
	Answer     string    `json:"answer,omitempty"`
	AnsweredBy string    `json:"answered_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
	// Delivery is the answer's delivery to the asker (migration 0031), ""
	// when there is nothing to deliver; DeliveryDetail why it is waiting or
	// failed; DeliveredSession the chat session it went to (0 = none).
	Delivery         Delivery `json:"-"`
	DeliveryDetail   string   `json:"-"`
	DeliveryAttempts int      `json:"-"`
	DeliveredSession int64    `json:"-"`
	// DeliveryQueuedAt is when the delivery was last queued (the answer, or a
	// Retry); zero when there is none.
	DeliveryQueuedAt time.Time `json:"-"`
	// TaskID is the task the ask is about (migration 0032), 0 for none. Only
	// muster's own task path sets it (New.TaskID); an agent's attention_ask
	// never does.
	TaskID int64 `json:"task_id,omitempty"`
}

// Delivery is where an answer stands on its way to the asker (migration 0031).
type Delivery string

const (
	// DeliveryNone: nothing to deliver (not answered by the operator in the
	// queue, a system asker's, or answered before delivery existed).
	DeliveryNone Delivery = ""
	// DeliveryQueued: answered; a delivery into the asking agent's session is owed.
	DeliveryQueued Delivery = "queued"
	// DeliveryDelivering: one process holds the delivery lease.
	DeliveryDelivering Delivery = "delivering"
	// DeliveryDelivered: the turn completed in DeliveredSession.
	DeliveryDelivered Delivery = "delivered"
	// DeliveryUnconfirmed: the turn failed after the text may have reached the
	// agent, or its lease lapsed mid-turn. Never resent unless the operator
	// retries it (RetryDelivery).
	DeliveryUnconfirmed Delivery = "unconfirmed"
	// DeliveryWaited: the asker read the answer through attention_wait.
	DeliveryWaited Delivery = "waited"
	// DeliveryPull: a mirrored session's answer, left for its attention_wait.
	DeliveryPull Delivery = "pull"
)

// MaxDeliveryDetailRunes bounds DeliveryDetail (0031's CHECK).
const MaxDeliveryDetailRunes = 300

// ErrNotRetryable: only an unconfirmed delivery can be retried.
var ErrNotRetryable = errors.New("attention: only an answer whose delivery failed can be delivered again")

// New is what an asker supplies.
type New struct {
	Asker    Asker
	Question string
	Options  []string
	// TaskID links the ask to one of the caller's tasks (0: none). Set ONLY by
	// muster's task path (internal/api askOwnerAboutTask), never from an
	// agent's arguments: task_dispatch trusts it as the provenance of the
	// owner's answer.
	TaskID int64
	// Timeout is how long the ask waits; 0 is DefaultTimeout, and anything
	// else is clamped to [MinTimeout, MaxTimeout].
	Timeout time.Duration
}

// Clean validates and normalises n: the question and options trimmed, blank
// options dropped, duplicates refused, the timeout clamped.
func Clean(n New) (New, error) {
	if err := n.Asker.Valid(); err != nil {
		return n, err
	}
	n.Asker.Label = truncateRunes(strings.TrimSpace(n.Asker.Label), MaxLabelRunes)
	n.Question = strings.TrimSpace(n.Question)
	if n.Question == "" {
		return n, fmt.Errorf("%w: the question is empty", ErrInvalid)
	}
	if utf8.RuneCountInString(n.Question) > MaxQuestionRunes {
		return n, fmt.Errorf("%w: the question is longer than %d characters", ErrInvalid, MaxQuestionRunes)
	}
	var opts []string
	seen := map[string]bool{}
	for _, o := range n.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if utf8.RuneCountInString(o) > MaxOptionRunes {
			return n, fmt.Errorf("%w: an option is longer than %d characters", ErrInvalid, MaxOptionRunes)
		}
		if seen[o] {
			return n, fmt.Errorf("%w: option %q is listed twice", ErrInvalid, o)
		}
		seen[o] = true
		opts = append(opts, o)
	}
	if len(opts) > MaxOptions {
		return n, fmt.Errorf("%w: at most %d options", ErrInvalid, MaxOptions)
	}
	n.Options = opts
	switch {
	case n.Timeout <= 0:
		n.Timeout = DefaultTimeout
	case n.Timeout < MinTimeout:
		n.Timeout = MinTimeout
	case n.Timeout > MaxTimeout:
		n.Timeout = MaxTimeout
	}
	return n, nil
}

// CleanAnswer validates an answer's text.
func CleanAnswer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: the answer is empty", ErrInvalid)
	}
	if utf8.RuneCountInString(s) > MaxAnswerRunes {
		return "", fmt.Errorf("%w: the answer is longer than %d characters", ErrInvalid, MaxAnswerRunes)
	}
	return s, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Store is the asks' persistence. See the package doc for which methods run
// under whose principal.
type Store interface {
	// Create files n for the caller's principal.
	Create(ctx context.Context, n New) (Ask, error)
	// Get is one of the caller's asks; an open ask past its expiry reads as
	// Expired even before the sweep has moved it.
	Get(ctx context.Context, id int64) (Ask, error)
	// Open is the caller's open, unexpired asks, oldest first.
	Open(ctx context.Context) ([]Ask, error)
	// Answer records the operator's answer to an open ask.
	Answer(ctx context.Context, id int64, answer string) (Ask, error)
	// Cancel withdraws an open ask.
	Cancel(ctx context.Context, id int64) (Ask, error)
	// AnswerInSession settles as AnsweredInSession every open ask asker raised
	// before this statement began (created_at < statement_timestamp()), with
	// message (capped at MaxAnswerRunes) as the answer and queueID (0 = none)
	// as the send-queue row that carried it, and returns them; none for a
	// blank message.
	AnswerInSession(ctx context.Context, asker Asker, message string, queueID int64) ([]Ask, error)
	// AnsweredByQueue is, for each of ids that answered asks in session, the
	// label of the asker it answered.
	AnsweredByQueue(ctx context.Context, ids []int64) (map[int64]string, error)
	// ExpireDue moves every owner's lapsed open asks to Expired and returns
	// them (system scope).
	ExpireDue(ctx context.Context) ([]Ask, error)

	// --- the answer's delivery (migration 0031) ---

	// MarkWaited records that the asker read ask id's answer through
	// attention_wait: a queued, pull or in-flight delivery becomes
	// DeliveryWaited (an in-flight one loses its lease), so no turn is pushed.
	// It reports whether it moved the row.
	MarkWaited(ctx context.Context, id int64) (bool, error)
	// ClaimDelivery takes ask id's delivery lease for by, for lease: one
	// conditional UPDATE on DeliveryQueued and on the answer being at least
	// grace old (an asker blocked in attention_wait reads it first). False,
	// with nothing changed, when another claimer or a reader won.
	ClaimDelivery(ctx context.Context, id int64, by string, lease, grace time.Duration) (Ask, bool, error)
	// ReleaseDelivery returns a claimed delivery that sent nothing to
	// DeliveryQueued, detail saying why (only by's own lease).
	ReleaseDelivery(ctx context.Context, id int64, by, detail string) error
	// FinishDelivery settles by's claimed delivery: DeliveryDelivered into
	// sessionID, or DeliveryUnconfirmed with detail. It reports whether by
	// still held the lease.
	FinishDelivery(ctx context.Context, id int64, by string, delivered bool, sessionID int64, detail string) (bool, error)
	// RetryDelivery moves the caller's unconfirmed delivery back to
	// DeliveryQueued (ErrNotRetryable otherwise).
	RetryDelivery(ctx context.Context, id int64) (Ask, error)
	// Answered is the caller's answers resolved since since that have a
	// delivery to show (or were a system asker's) — and every unconfirmed one,
	// however old, since it waits on the owner's Retry — newest first, at most
	// limit.
	Answered(ctx context.Context, since time.Time, limit int) ([]Ask, error)
	// DueDeliveries is every owner's queued deliveries at least grace old,
	// least-tried first (so answers stuck on an agent that will not take them
	// cannot starve newer ones), then oldest, at most limit (system scope).
	DueDeliveries(ctx context.Context, grace time.Duration, limit int) ([]Ask, error)
	// RecoverDeliveries settles as DeliveryUnconfirmed every owner's delivery
	// whose lease lapsed (its process died mid-turn) and returns them (system
	// scope).
	RecoverDeliveries(ctx context.Context) ([]Ask, error)

	// AnswersForTask is the caller's asks linked to taskID that were answered
	// THROUGH THE ASK ITSELF (Answered), newest first. AnsweredInSession is
	// left out on purpose: one message to the asker's session settles every
	// open ask that asker raised, on every task, so it is not an answer to any
	// one task's ask (task_dispatch's instructions provenance).
	AnswersForTask(ctx context.Context, taskID int64) ([]Ask, error)
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG builds a PGStore.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// sys is the system scope: ExpireDue's, DueDeliveries' and RecoverDeliveries'
// (one helper, ledgered in internal/authz).
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// cols reads an ask; status is computed so an open ask past its expiry reads
// as expired before the sweep has written it.
const cols = `id, owner_id, coalesce(agent_id, 0), coalesce(mirror_host, ''), coalesce(mirror_session, ''), system_asker, asker_label,
	question, options,
	CASE WHEN status = 'open' AND expires_at <= now() THEN 'expired' ELSE status END,
	coalesce(answer, ''), coalesce(answered_by, ''), created_at, expires_at, resolved_at,
	coalesce(delivery, ''), delivery_detail, delivery_attempts, coalesce(delivered_session_id, 0), delivery_queued_at,
	coalesce(task_id, 0)`

func scan(row pgx.Row) (Ask, error) {
	var a Ask
	var opts []byte
	var resolved, queued *time.Time
	var status, delivery string
	if err := row.Scan(&a.ID, &a.Owner, &a.Asker.AgentID, &a.Asker.MirrorHost, &a.Asker.MirrorSession, &a.Asker.System, &a.Asker.Label,
		&a.Question, &opts, &status, &a.Answer, &a.AnsweredBy, &a.CreatedAt, &a.ExpiresAt, &resolved,
		&delivery, &a.DeliveryDetail, &a.DeliveryAttempts, &a.DeliveredSession, &queued, &a.TaskID); err != nil {
		return Ask{}, err
	}
	if queued != nil {
		a.DeliveryQueuedAt = *queued
	}
	a.Status = Status(status)
	a.Delivery = Delivery(delivery)
	if err := json.Unmarshal(opts, &a.Options); err != nil {
		return Ask{}, fmt.Errorf("attention: options of ask %d: %w", a.ID, err)
	}
	if a.Options == nil {
		a.Options = []string{}
	}
	if resolved != nil {
		a.ResolvedAt = *resolved
	}
	return a, nil
}

func collect(rows pgx.Rows, err error) ([]Ask, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ask
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *PGStore) Create(ctx context.Context, n New) (Ask, error) {
	n, err := Clean(n)
	if err != nil {
		return Ask{}, err
	}
	opts, err := json.Marshal(n.Options)
	if err != nil {
		return Ask{}, err
	}
	if n.Options == nil {
		opts = []byte("[]")
	}
	var agent any
	if n.Asker.AgentID > 0 {
		agent = n.Asker.AgentID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ask{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The per-owner cap, counted under the caller's principal: row-level
	// security makes it this owner's count. Serialised per owner by an
	// advisory lock so two concurrent asks cannot both pass at the cap.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('attention_asks:' || current_setting('muster.uid', true), 0))`); err != nil {
		return Ask{}, err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM attention_asks WHERE status = 'open' AND expires_at > now()`).Scan(&open); err != nil {
		return Ask{}, err
	}
	if open >= MaxOpenPerOwner {
		return Ask{}, ErrTooMany
	}
	var task any
	if n.TaskID > 0 {
		task = n.TaskID
	}
	a, err := scan(tx.QueryRow(ctx, `INSERT INTO attention_asks (agent_id, mirror_host, mirror_session, system_asker, asker_label, question, options, expires_at, task_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8), $9)
		RETURNING `+cols, agent, nullable(n.Asker.MirrorHost), nullable(n.Asker.MirrorSession), n.Asker.System, n.Asker.Label, n.Question, opts, n.Timeout.Seconds(), task))
	if err != nil {
		return Ask{}, err
	}
	return a, tx.Commit(ctx)
}

func (s *PGStore) Get(ctx context.Context, id int64) (Ask, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM attention_asks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, ErrNotFound
	}
	return a, err
}

// AnswersForTask runs under the caller's principal (row-level security keeps it
// to their own asks), and names the owner as well.
func (s *PGStore) AnswersForTask(ctx context.Context, taskID int64) ([]Ask, error) {
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE task_id = $1 AND status = 'answered' AND answered_by = $2
		  AND owner_id = nullif(current_setting('muster.uid', true), '')::bigint
		ORDER BY id DESC`, taskID, ByOperator))
}

func (s *PGStore) Open(ctx context.Context) ([]Ask, error) {
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks WHERE status = 'open' AND expires_at > now() ORDER BY id`))
}

// settle runs one conditional transition and reads ErrNotFound / ErrNotOpen
// apart when it moved nothing.
func (s *PGStore) settle(ctx context.Context, id int64, q string, args ...any) (Ask, error) {
	a, err := scan(s.pool.QueryRow(ctx, q, args...))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, err
	}
	cur, gerr := s.Get(ctx, id)
	if gerr != nil {
		return Ask{}, gerr
	}
	return cur, ErrNotOpen
}

func (s *PGStore) Answer(ctx context.Context, id int64, answer string) (Ask, error) {
	answer, err := CleanAnswer(answer)
	if err != nil {
		return Ask{}, err
	}
	// 🔴 THE DELIVERY IS OWED IN THE SAME STATEMENT AS THE ANSWER (migration
	// 0031), so no answer to an agent exists without its delivery state: an
	// agent's answer is queued for a turn in its session; a mirrored session's
	// is left for its attention_wait (pull); a system asker's has nobody to
	// reach (NULL).
	return s.settle(ctx, id, `UPDATE attention_asks SET status = 'answered', answer = $2, answered_by = $3, resolved_at = now(),
		  delivery = CASE WHEN agent_id IS NOT NULL THEN 'queued' WHEN mirror_session IS NOT NULL THEN 'pull' END,
		  delivery_queued_at = CASE WHEN agent_id IS NOT NULL THEN now() END
		WHERE id = $1 AND status = 'open' AND expires_at > now() RETURNING `+cols, id, answer, ByOperator)
}

// cutDetail bounds a delivery detail (0031's CHECK).
func cutDetail(s string) string { return truncateRunes(strings.TrimSpace(s), MaxDeliveryDetailRunes) }

// MarkWaited runs under the caller's principal (the asker's tool call).
//
// 🔴 IT ALSO TAKES A DELIVERY IN FLIGHT. A wait that reads the answer while an
// attempt holds the lease wins it: the lease is dropped, so the attempt's
// give-up (ReleaseDelivery) or outcome (FinishDelivery), both conditional on
// still holding it, change nothing — the give-up used to put a read answer
// back in the queue, and the sweep pushed it a second time.
func (s *PGStore) MarkWaited(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE attention_asks SET delivery = 'waited', delivery_detail = '',
		  delivery_claimed_by = NULL, delivery_lease_until = NULL
		WHERE id = $1 AND delivery IN ('queued', 'pull', 'delivering')`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimDelivery runs under the owner's principal.
func (s *PGStore) ClaimDelivery(ctx context.Context, id int64, by string, lease, grace time.Duration) (Ask, bool, error) {
	a, err := scan(s.pool.QueryRow(ctx, `UPDATE attention_asks SET delivery = 'delivering', delivery_claimed_by = $2,
		  delivery_lease_until = now() + make_interval(secs => $3), delivery_attempts = delivery_attempts + 1
		WHERE id = $1 AND delivery = 'queued' AND resolved_at <= now() - make_interval(secs => $4)
		RETURNING `+cols, id, by, lease.Seconds(), grace.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ask{}, false, nil
	}
	if err != nil {
		return Ask{}, false, err
	}
	return a, true, nil
}

func (s *PGStore) ReleaseDelivery(ctx context.Context, id int64, by, detail string) error {
	_, err := s.pool.Exec(ctx, `UPDATE attention_asks SET delivery = 'queued', delivery_claimed_by = NULL, delivery_lease_until = NULL,
		  delivery_detail = $3
		WHERE id = $1 AND delivery = 'delivering' AND delivery_claimed_by = $2`, id, by, cutDetail(detail))
	return err
}

func (s *PGStore) FinishDelivery(ctx context.Context, id int64, by string, delivered bool, sessionID int64, detail string) (bool, error) {
	state := DeliveryUnconfirmed
	if delivered {
		state, detail = DeliveryDelivered, ""
	}
	var session any
	if sessionID > 0 {
		session = sessionID
	}
	tag, err := s.pool.Exec(ctx, `UPDATE attention_asks SET delivery = $3, delivery_claimed_by = NULL, delivery_lease_until = NULL,
		  delivery_detail = $4, delivered_session_id = $5::bigint, delivered_at = CASE WHEN $3 = 'delivered' THEN now() END
		WHERE id = $1 AND delivery = 'delivering' AND delivery_claimed_by = $2`, id, by, string(state), cutDetail(detail), session)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RetryDelivery runs under the caller's principal: row-level security keeps it
// to their own asks.
func (s *PGStore) RetryDelivery(ctx context.Context, id int64) (Ask, error) {
	a, err := scan(s.pool.QueryRow(ctx, `UPDATE attention_asks SET delivery = 'queued', delivery_detail = '', delivery_queued_at = now()
		WHERE id = $1 AND delivery = 'unconfirmed' RETURNING `+cols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.Get(ctx, id)
		if gerr != nil {
			return Ask{}, gerr
		}
		return cur, ErrNotRetryable
	}
	return a, err
}

// Answered runs under the caller's principal.
func (s *PGStore) Answered(ctx context.Context, since time.Time, limit int) ([]Ask, error) {
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE status = 'answered' AND (delivery IS NOT NULL OR system_asker)
		  AND (resolved_at >= $1 OR delivery = 'unconfirmed')
		ORDER BY resolved_at DESC, id DESC LIMIT $2`, since, limit))
}

func (s *PGStore) DueDeliveries(ctx context.Context, grace time.Duration, limit int) ([]Ask, error) {
	return collect(s.pool.Query(sys(ctx), `SELECT `+cols+` FROM attention_asks
		WHERE delivery = 'queued' AND resolved_at <= now() - make_interval(secs => $1)
		ORDER BY delivery_attempts, resolved_at, id LIMIT $2`, grace.Seconds(), limit))
}

// DeliveryLapsedDetail is an unconfirmed delivery's detail when its process
// stopped mid-turn.
const DeliveryLapsedDetail = "muster stopped while delivering it, so the agent may or may not have received it"

func (s *PGStore) RecoverDeliveries(ctx context.Context) ([]Ask, error) {
	return collect(s.pool.Query(sys(ctx), `UPDATE attention_asks SET delivery = 'unconfirmed', delivery_claimed_by = NULL,
		  delivery_lease_until = NULL, delivery_detail = $1
		WHERE delivery = 'delivering' AND delivery_lease_until < now() RETURNING `+cols, DeliveryLapsedDetail))
}

func (s *PGStore) Cancel(ctx context.Context, id int64) (Ask, error) {
	return s.settle(ctx, id, `UPDATE attention_asks SET status = 'cancelled', resolved_at = now()
		WHERE id = $1 AND status = 'open' AND expires_at > now() RETURNING `+cols, id)
}

// AnswerInSession runs under the caller's principal, like Answer: row-level
// security keeps it to the caller's own asks.
//
// 🔴 ONLY ASKS RAISED BEFORE THE MESSAGE: the caller runs it as the message is
// sent (or queued), and an ask is answered only when its created_at is before
// this UPDATE's statement_timestamp() — the time the statement began.
func (s *PGStore) AnswerInSession(ctx context.Context, asker Asker, message string, queueID int64) ([]Ask, error) {
	message = truncateRunes(strings.TrimSpace(message), MaxAnswerRunes)
	if message == "" || asker.Valid() != nil {
		return nil, nil
	}
	var agent any
	if asker.AgentID > 0 {
		agent = asker.AgentID
	}
	var queue any
	if queueID > 0 {
		queue = queueID
	}
	return collect(s.pool.Query(ctx, `UPDATE attention_asks SET status = 'answered_in_session', answer = $1, answered_by = $2, resolved_at = now(),
		  answer_queue_id = $6::bigint
		WHERE status = 'open' AND expires_at > now() AND created_at < statement_timestamp()
		  AND agent_id IS NOT DISTINCT FROM $3::bigint AND mirror_host IS NOT DISTINCT FROM $4::text AND mirror_session IS NOT DISTINCT FROM $5::text
		RETURNING `+cols, message, ByOperator, agent, nullable(asker.MirrorHost), nullable(asker.MirrorSession), queue))
}

// AnsweredByQueue runs under the caller's principal (row-level security keeps
// it to their own asks).
func (s *PGStore) AnsweredByQueue(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (answer_queue_id) answer_queue_id, asker_label, coalesce(agent_id, 0), coalesce(mirror_session, '')
		FROM attention_asks WHERE answer_queue_id = ANY($1) AND status = 'answered_in_session' ORDER BY answer_queue_id, id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, agent int64
		var label, session string
		if err := rows.Scan(&id, &label, &agent, &session); err != nil {
			return nil, err
		}
		if label == "" {
			label = askerLabelOf(Asker{AgentID: agent, MirrorSession: session})
		}
		out[id] = label
	}
	return out, rows.Err()
}

// askerLabelOf names an asker that has no label.
func askerLabelOf(a Asker) string {
	if a.AgentID > 0 {
		return fmt.Sprintf("agent %d", a.AgentID)
	}
	return "session " + a.MirrorSession
}

func (s *PGStore) ExpireDue(ctx context.Context) ([]Ask, error) {
	return collect(s.pool.Query(sys(ctx), `UPDATE attention_asks SET status = 'expired', resolved_at = now()
		WHERE status = 'open' AND expires_at <= now() RETURNING `+cols))
}
