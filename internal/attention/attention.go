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
// (owner_id's default), never a parameter. ExpireDue alone runs under the
// system scope (sys): the sweep moves every owner's lapsed asks and hands each
// back with its owner, so the caller can notify that owner and no other.
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

// Asker is who raised an ask: a dispatched agent (AgentID) or a mirrored
// Claude Code session (MirrorHost + MirrorSession, the Claude session id).
// Exactly one of the two is set.
type Asker struct {
	AgentID       int64
	MirrorHost    string
	MirrorSession string
	// Label is the name the view shows beside the question.
	Label string
}

// Valid reports whether a is exactly one kind of asker with valid fields.
func (a Asker) Valid() error {
	mirrored := a.MirrorHost != "" || a.MirrorSession != ""
	switch {
	case a.AgentID > 0 && mirrored:
		return fmt.Errorf("%w: an ask has one asker", ErrInvalid)
	case a.AgentID > 0:
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
}

// New is what an asker supplies.
type New struct {
	Asker    Asker
	Question string
	Options  []string
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
	// ExpireDue moves every owner's lapsed open asks to Expired and returns
	// them (system scope).
	ExpireDue(ctx context.Context) ([]Ask, error)
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG builds a PGStore.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// sys is the system scope, ExpireDue's alone (ledgered in internal/authz).
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// cols reads an ask; status is computed so an open ask past its expiry reads
// as expired before the sweep has written it.
const cols = `id, owner_id, coalesce(agent_id, 0), coalesce(mirror_host, ''), coalesce(mirror_session, ''), asker_label,
	question, options,
	CASE WHEN status = 'open' AND expires_at <= now() THEN 'expired' ELSE status END,
	coalesce(answer, ''), coalesce(answered_by, ''), created_at, expires_at, resolved_at`

func scan(row pgx.Row) (Ask, error) {
	var a Ask
	var opts []byte
	var resolved *time.Time
	var status string
	if err := row.Scan(&a.ID, &a.Owner, &a.Asker.AgentID, &a.Asker.MirrorHost, &a.Asker.MirrorSession, &a.Asker.Label,
		&a.Question, &opts, &status, &a.Answer, &a.AnsweredBy, &a.CreatedAt, &a.ExpiresAt, &resolved); err != nil {
		return Ask{}, err
	}
	a.Status = Status(status)
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
	a, err := scan(tx.QueryRow(ctx, `INSERT INTO attention_asks (agent_id, mirror_host, mirror_session, asker_label, question, options, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		RETURNING `+cols, agent, nullable(n.Asker.MirrorHost), nullable(n.Asker.MirrorSession), n.Asker.Label, n.Question, opts, n.Timeout.Seconds()))
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
	return s.settle(ctx, id, `UPDATE attention_asks SET status = 'answered', answer = $2, answered_by = $3, resolved_at = now()
		WHERE id = $1 AND status = 'open' AND expires_at > now() RETURNING `+cols, id, answer, ByOperator)
}

func (s *PGStore) Cancel(ctx context.Context, id int64) (Ask, error) {
	return s.settle(ctx, id, `UPDATE attention_asks SET status = 'cancelled', resolved_at = now()
		WHERE id = $1 AND status = 'open' AND expires_at > now() RETURNING `+cols, id)
}

func (s *PGStore) ExpireDue(ctx context.Context) ([]Ask, error) {
	return collect(s.pool.Query(sys(ctx), `UPDATE attention_asks SET status = 'expired', resolved_at = now()
		WHERE status = 'open' AND expires_at <= now() RETURNING `+cols))
}
