// Package sendqueue is the SERVER-SIDE SEND QUEUE (migration 0019): messages a
// user queued for a mirrored Claude Code window, which the server sends ONE AT
// A TIME, each only once that window's Claude Code session has been idle for a
// while (Dispatcher). The queue is per user and stored, so it survives
// the page that queued it closing, and every device of the owner sees (and can
// cancel) the same queue.
//
// 🔴 THE STORE HAS TWO HALVES AND THEY RUN UNDER DIFFERENT PRINCIPALS.
// Enqueue, List, Get and Cancel run under the CALLER's principal: send_queue is
// under FORCE row-level security, so a request sees, queues into and cancels
// only its own user's rows, and the owner of a new row is the principal's user
// (owner_id's default), never a parameter. Recover, Heads, Claim, Finish and
// Prune are the dispatcher's, and run under the system scope (sys): they read
// and move every owner's rows, and hand each row to the delivery, which acts
// as that row's owner.
//
// 🔴 A MESSAGE IS CLAIMED ONCE AND NEVER RETRIED. Claim moves a row from
// queued to sending in one conditional UPDATE, and nothing ever moves a row
// back: a send that fails is failed, and a process that dies mid-send leaves a
// row that Recover marks failed (may have been delivered) at the next start.
// A message is never pasted twice by the queue.
package sendqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// State is where a queued message is.
type State string

const (
	Queued    State = "queued"
	Sending   State = "sending"
	Sent      State = "sent"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

// Open reports whether a message in state s is still to be sent (or being sent).
func (s State) Open() bool { return s == Queued || s == Sending }

// Target addresses what messages are queued for: a mirrored window by (host,
// session, tmux window id) — "" being a per-session host's session. It is the
// queue's identity: messages for one target are sent in the order they were
// queued, one at a time. A queued message always lands in the window's Claude
// Code pane (mirror.PasteClaude), never whatever else is active there.
type Target struct {
	Host    string
	Session string
	Window  string
}

// Valid reports whether t is a target the table can hold.
func (t Target) Valid() error {
	if err := mirror.ValidHost(t.Host); err != nil {
		return err
	}
	if err := mirror.ValidSession(t.Session); err != nil {
		return err
	}
	if t.Window != "" {
		return mirror.ValidWindowID(t.Window)
	}
	return nil
}

// String is t for a log line: ids and names, never anything sent to it.
func (t Target) String() string {
	return fmt.Sprintf("mirror %s/%s%s", t.Host, t.Session, t.Window)
}

// Item is one queued message.
type Item struct {
	ID     int64
	Owner  int64
	Target Target
	Body   string
	// StepUpAt is the sign-in time of the session that queued the message (the
	// step-up decision, recorded): the delivery presents it to the same
	// freshness gate a manual send passes.
	StepUpAt         time.Time
	State            State
	Detail           string
	MayHaveDelivered bool
	CreatedAt        time.Time
	ClaimedAt        time.Time
	FinishedAt       time.Time
}

// MaxOpenPerTarget bounds the messages one target may have queued (or being
// sent) at once. A target that is never idle — a host that stopped reporting,
// a session that is always busy — would otherwise collect without limit.
const MaxOpenPerTarget = 20

// MaxDetail bounds an outcome's detail (0019's send_queue_detail_cap).
const MaxDetail = 1024

// HistoryShown is how many finished messages List returns beside the open
// ones: the recent outcomes a composer shows under its queue.
const HistoryShown = 5

var (
	// ErrFull refuses a message past MaxOpenPerTarget.
	ErrFull = fmt.Errorf("sendqueue: at most %d messages can be queued for one target; wait for some to send, or cancel one", MaxOpenPerTarget)
	// ErrNotFound: no such message the caller can see (absent, or another
	// user's — the two answer identically).
	ErrNotFound = errors.New("sendqueue: no such queued message")
	// ErrNotCancellable: the message is no longer queued (it is being sent, or
	// already finished).
	ErrNotCancellable = errors.New("sendqueue: the message is no longer queued, so it cannot be cancelled")
	// ErrEmpty refuses a message with no text.
	ErrEmpty = errors.New("sendqueue: nothing to queue: type a message first")
)

// PGStore is the queue on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// cols is every column an Item is read from, in scanItem's order.
const cols = `id, owner_id, mirror_host, mirror_session, mirror_window, body, stepup_at, state, detail,
	may_have_delivered, created_at, coalesce(claimed_at, 'epoch'::timestamptz), coalesce(finished_at, 'epoch'::timestamptz)`

// itemDests is where cols scan into it, with its state into state.
func itemDests(it *Item, state *string) []any {
	return []any{&it.ID, &it.Owner, &it.Target.Host, &it.Target.Session, &it.Target.Window,
		&it.Body, &it.StepUpAt, state, &it.Detail, &it.MayHaveDelivered, &it.CreatedAt, &it.ClaimedAt, &it.FinishedAt}
}

// settle finishes a scanned item: its state, and the epoch placeholders back to
// zero times.
func settle(it *Item, state string, more ...*time.Time) {
	it.State = State(state)
	for _, t := range append([]*time.Time{&it.ClaimedAt, &it.FinishedAt}, more...) {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
}

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	var state string
	err := row.Scan(itemDests(&it, &state)...)
	settle(&it, state)
	return it, err
}

// targetWhere is the SQL predicate "this row is target t's", with t's
// parameters numbered from n.
func targetWhere(t Target, n int) (string, []any) {
	return fmt.Sprintf(`mirror_host = $%d AND mirror_session = $%d AND mirror_window = $%d`, n, n+1, n+2),
		[]any{t.Host, t.Session, t.Window}
}

// Enqueue queues it for its target, as the caller, and returns the stored row.
// Only its Target, Body and StepUpAt are read.
//
// The per-target bound is checked AFTER the insert, in the same transaction,
// serialised per owner by an advisory lock, so two concurrent messages cannot
// both pass it (internal/pins' shape).
func (s *PGStore) Enqueue(ctx context.Context, it Item) (Item, error) {
	if err := it.Target.Valid(); err != nil {
		return Item{}, err
	}
	if strings.TrimSpace(it.Body) == "" {
		return Item{}, ErrEmpty
	}
	text, err := mirror.SanitizePaste(it.Body)
	if err != nil {
		return Item{}, err
	}
	if it.StepUpAt.IsZero() {
		// The step-up decision has to have been made: a message without the
		// sign-in time that admitted it could never be sent.
		return Item{}, errors.New("sendqueue: a queued message needs the sign-in time of the session that queued it")
	}
	q := `INSERT INTO send_queue (mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + cols
	args := []any{it.Target.Host, it.Target.Session, it.Target.Window, text, it.StepUpAt}
	var out Item
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sendqueue.enqueue'),
			coalesce(nullif(current_setting('muster.uid', true), '')::int, 0))`); err != nil {
			return fmt.Errorf("sendqueue: lock: %w", err)
		}
		var err error
		out, err = scanItem(tx.QueryRow(ctx, q, args...))
		if err != nil {
			return fmt.Errorf("sendqueue: enqueue: %w", err)
		}
		where, wargs := targetWhere(it.Target, 1)
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM send_queue WHERE state IN ('queued', 'sending') AND `+where, wargs...).Scan(&n); err != nil {
			return fmt.Errorf("sendqueue: count: %w", err)
		}
		if n > MaxOpenPerTarget {
			return ErrFull
		}
		return nil
	})
	if err != nil {
		return Item{}, err
	}
	return out, nil
}

// List is the caller's messages for t: every open one, oldest first, then
// the HistoryShown most recently finished, newest first.
func (s *PGStore) List(ctx context.Context, t Target) ([]Item, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	where, args := targetWhere(t, 1)
	rows, err := s.pool.Query(ctx, `(SELECT `+cols+`, 0 AS part FROM send_queue WHERE state IN ('queued', 'sending') AND `+where+`)
		UNION ALL
		(SELECT `+cols+`, 1 AS part FROM send_queue WHERE state NOT IN ('queued', 'sending') AND `+where+`
			ORDER BY finished_at DESC, id DESC LIMIT `+fmt.Sprint(HistoryShown)+`)
		ORDER BY part, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sendqueue: list: %w", err)
	}
	defer rows.Close()
	var open, done []Item
	for rows.Next() {
		var it Item
		var part int
		var state string
		if err := rows.Scan(append(itemDests(&it, &state), &part)...); err != nil {
			return nil, fmt.Errorf("sendqueue: list: %w", err)
		}
		settle(&it, state)
		if part == 0 {
			open = append(open, it)
		} else {
			done = append(done, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sendqueue: list: %w", err)
	}
	// The finished part is drawn newest first.
	for i, j := 0, len(done)-1; i < j; i, j = i+1, j-1 {
		done[i], done[j] = done[j], done[i]
	}
	return append(open, done...), nil
}

// Get is the caller's message id.
func (s *PGStore) Get(ctx context.Context, id int64) (Item, error) {
	it, err := scanItem(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM send_queue WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("sendqueue: get: %w", err)
	}
	return it, nil
}

// Cancel cancels the caller's message id, which must still be queued: one
// being sent is past cancelling (ErrNotCancellable), and another user's is not
// found.
func (s *PGStore) Cancel(ctx context.Context, id int64, now time.Time) (Item, error) {
	it, err := scanItem(s.pool.QueryRow(ctx, `UPDATE send_queue SET state = 'cancelled', finished_at = $2,
		detail = 'cancelled' WHERE id = $1 AND state = 'queued' RETURNING `+cols, id, now))
	if err == nil {
		return it, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Item{}, fmt.Errorf("sendqueue: cancel: %w", err)
	}
	if _, gerr := s.Get(ctx, id); gerr != nil {
		return Item{}, gerr
	}
	return Item{}, ErrNotCancellable
}

// ---------------------------------------------------------------------------
// The dispatcher's half: every owner's rows, under the system scope.
// ---------------------------------------------------------------------------

// sys is ctx under the system principal: the dispatcher's own reads and
// state transitions, which span every owner's queue. It is internal/authz's
// System ledger entry for this package, and nothing read through it is handed
// to a request.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// RecoveredDetail is the outcome of a message a process stopped in the middle
// of sending (Recover).
const RecoveredDetail = "muster restarted while sending this message, so it may or may not have been delivered; it was not sent again"

// Recover marks every message left 'sending' — its process stopped mid-send —
// as failed, may have been delivered. Run once at start, before the first
// claim. It never returns a message to queued.
//
// ⚠ WITH MORE THAN ONE REPLICA this would fail a peer's live send; muster runs
// one (the mirror hub is in-process state too).
func (s *PGStore) Recover(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(sys(ctx), `UPDATE send_queue SET state = 'failed', may_have_delivered = true,
		detail = $1, finished_at = $2 WHERE state = 'sending'`, RecoveredDetail, now)
	if err != nil {
		return 0, fmt.Errorf("sendqueue: recover: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Head is one target's next message (its oldest open row), with what the
// dispatcher decides on.
type Head struct {
	Item
	// LastAttempt is when the target was last handed a message that may have
	// reached it — one sent, or one that failed after it may have been
	// delivered. Zero when there is none. The next message waits for the target
	// to have been busy and gone idle again SINCE then (Decide).
	LastAttempt time.Time
}

// Heads is every target's next message, across every owner: a target's row
// being sent if it has one, else its oldest queued row.
func (s *PGStore) Heads(ctx context.Context) ([]Head, error) {
	return s.heads(sys(ctx), "")
}

// HeadOf is the caller's head for target t (false when t has nothing open):
// what a composer's queue shows the dispatcher deciding on. Under the CALLER's
// principal.
func (s *PGStore) HeadOf(ctx context.Context, t Target) (Head, bool, error) {
	if err := t.Valid(); err != nil {
		return Head{}, false, err
	}
	where, args := targetWhere(t, 1)
	hs, err := s.heads(ctx, " AND "+where, args...)
	if err != nil || len(hs) == 0 {
		return Head{}, false, err
	}
	return hs[0], true, nil
}

func (s *PGStore) heads(ctx context.Context, where string, args ...any) ([]Head, error) {
	rows, err := s.pool.Query(ctx, `WITH h AS (
			SELECT DISTINCT ON (owner_id, mirror_host, mirror_session, mirror_window)
				*
			FROM send_queue WHERE state IN ('queued', 'sending')`+where+`
			ORDER BY owner_id, mirror_host, mirror_session, mirror_window, (state = 'sending') DESC, id
		)
		SELECT `+cols+`, coalesce((SELECT max(p.claimed_at) FROM send_queue p
			WHERE p.owner_id = h.owner_id AND p.mirror_host = h.mirror_host
				AND p.mirror_session = h.mirror_session AND p.mirror_window = h.mirror_window
				AND (p.state = 'sent' OR (p.state = 'failed' AND p.may_have_delivered))), 'epoch'::timestamptz)
		FROM h ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sendqueue: heads: %w", err)
	}
	defer rows.Close()
	var out []Head
	for rows.Next() {
		var h Head
		var state string
		if err := rows.Scan(append(itemDests(&h.Item, &state), &h.LastAttempt)...); err != nil {
			return nil, fmt.Errorf("sendqueue: heads: %w", err)
		}
		settle(&h.Item, state, &h.LastAttempt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Claim moves message id from queued to sending at now and returns it, or
// false when it is no longer queued (cancelled, or claimed by another
// dispatcher) or its target already has a message being sent.
func (s *PGStore) Claim(ctx context.Context, id int64, now time.Time) (Item, bool, error) {
	it, err := scanItem(s.pool.QueryRow(sys(ctx), `UPDATE send_queue SET state = 'sending', claimed_at = $2
		WHERE id = $1 AND state = 'queued' RETURNING `+cols, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		// The target's one-sending index: another row of it is being sent.
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("sendqueue: claim: %w", err)
	}
	return it, true, nil
}

// Outcome is how a send ended.
type Outcome struct {
	// Sent: the target accepted the message.
	Sent bool
	// MayHaveDelivered: the message may have reached the target even though
	// the send did not succeed (the host never confirmed). Always true when
	// Sent.
	MayHaveDelivered bool
	// Detail is the outcome in words, shown to the owner: why it failed, or
	// how it was delivered. Never the message.
	Detail string
}

// Finish records how the send of message id ended, at now. Only a message
// being sent is finished.
func (s *PGStore) Finish(ctx context.Context, id int64, o Outcome, now time.Time) error {
	state := Failed
	if o.Sent {
		state, o.MayHaveDelivered = Sent, true
	}
	detail := o.Detail
	if len(detail) > MaxDetail {
		detail = strings.ToValidUTF8(detail[:MaxDetail], "")
	}
	if _, err := s.pool.Exec(sys(ctx), `UPDATE send_queue SET state = $2, detail = $3, may_have_delivered = $4,
		finished_at = $5 WHERE id = $1 AND state = 'sending'`, id, string(state), detail, o.MayHaveDelivered, now); err != nil {
		return fmt.Errorf("sendqueue: finish: %w", err)
	}
	return nil
}

// Retention is how long a finished message is kept (its outcome is shown
// under the composer) before Prune deletes it.
const Retention = 7 * 24 * time.Hour

// Prune deletes every message that finished before before.
func (s *PGStore) Prune(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(sys(ctx), `DELETE FROM send_queue WHERE state NOT IN ('queued', 'sending') AND finished_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("sendqueue: prune: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
