package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
)

// SESSION ROUTES (session_routes, migration 0021): the agent hands a plugin
// message delivered to it to the session best placed to answer it
// (route_to_session), the session answers (route_reply), and the answer
// becomes the plugin reply — a reply draft that is AUTO-APPROVED when the
// sender is on the owner's allowlist, and an ordinary pending draft the owner
// approves otherwise. The plugin claims and sends it through the existing
// claim/result path, unchanged.
//
//	routed ──answer──▶ answered ──relay──▶ relayed  (approved draft)
//	   │                      └──────────▶ fallback (pending draft)
//	   ├─ deadline ─▶ timed_out
//	   └─ injection or relay impossible ─▶ failed
//
// 🔴 EXACTLY ONCE, AND THE SCHEMA HOLDS IT. route_id is unique, one route per
// message is open at a time, and a draft names at most one route (0021's
// plugin_reply_drafts_one_per_route). RelayRoute takes the route row FOR UPDATE
// and returns the draft it already became when it has one, so a relay retried,
// raced or replayed files no second reply.
//
// 🔴 EVERY TRANSITION IS AUDITED (session_route.<state>), in the transaction
// that makes it, as the principal that made it — never as the system. The
// timeout sweep reads only ids and owners under System (ExpiredRoutes) and
// times each route out as its owner.

// RouteStore is the session_routes half of the plugin store (PGStore). Every
// method acts as ctx's principal except ExpiredRoutes (see there).
type RouteStore interface {
	CreateRoute(ctx context.Context, agentID, ownerID, messageID int64, target RouteTarget, timeout time.Duration) (Route, error)
	Route(ctx context.Context, routeID string) (Route, error)
	SetRouteQueued(ctx context.Context, routeID string, sendQueueID int64) error
	// FailRoute ends an open route as failed; handBack, when not "", is the
	// reason it is owed back to the agent (TakeHandBack delivers it).
	FailRoute(ctx context.Context, routeID, why, handBack string) (Route, error)
	// TakeHandBack clears and returns the hand-back routeID owes its agent,
	// exactly once: ok is false when none is owed (or another taker won).
	TakeHandBack(ctx context.Context, routeID string) (r Route, reason string, ok bool, err error)
	AnswerRoute(ctx context.Context, routeID, body string) (Route, bool, error)
	RelayRoute(ctx context.Context, routeID, agentName string, approve bool) (Route, Draft, error)
	TimeOutRoute(ctx context.Context, routeID string) (Route, bool, error)
	ExpiredRoutes(ctx context.Context) ([]DueRoute, error)
}

var _ RouteStore = (*PGStore)(nil)

// RouteState is where a route is.
type RouteState string

const (
	RouteRouted   RouteState = "routed"
	RouteAnswered RouteState = "answered"
	RouteRelayed  RouteState = "relayed"
	RouteTimedOut RouteState = "timed_out"
	RouteFallback RouteState = "fallback"
	RouteFailed   RouteState = "failed"
)

// Route is one row of session_routes.
type Route struct {
	ID          int64
	RouteID     string
	OwnerID     int64
	MessageID   int64
	AgentID     *int64
	TargetKind  string
	TargetKey   string
	TargetLabel string
	SendQueueID *int64
	State       RouteState
	CreatedAt   time.Time
	TimeoutAt   time.Time
	AnsweredAt  *time.Time
	FinishedAt  *time.Time
	ReplyBody   string
	DraftID     *int64
	Error       string
}

// RouteTarget names the session a route goes to: a kind ("agent", "mirror"),
// its key (the agent id, the Claude session id) and what it is called.
type RouteTarget struct {
	Kind, Key, Label string
}

// The route errors.
var (
	// ErrAlreadyRouted: the message already has an open route.
	ErrAlreadyRouted = errors.New("plugins: this message is already routed to a session and not yet answered")
	// ErrRouteNotFound: no such route for this principal (another owner's is the same).
	ErrRouteNotFound = errors.New("plugins: no such route")
	// ErrRouteClosed: the route is no longer waiting for an answer (timed out,
	// failed, or answered with a different text).
	ErrRouteClosed = errors.New("plugins: this route is no longer waiting for an answer")
	// ErrRouteNotAnswered: a relay of a route with no answer.
	ErrRouteNotAnswered = errors.New("plugins: this route has no answer to relay")
)

// MaxRouteError bounds a route's recorded error (0021's cap).
const MaxRouteError = 1024

var routeIDPattern = regexp.MustCompile(`^rt_[0-9a-f]{24}$`)

// ValidRouteID reports whether id is a route id.
func ValidRouteID(id string) bool { return routeIDPattern.MatchString(id) }

// NewRouteID is a fresh, unguessable route id.
func NewRouteID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "rt_" + hex.EncodeToString(b[:])
}

// RouteObject is the audit_log `object` naming a route.
func RouteObject(routeID string) string { return "session_route:" + routeID }

const routeCols = `id, route_id, owner_id, plugin_message_id, agent_id, target_kind, target_key, target_label, send_queue_id,
	state, created_at, timeout_at, answered_at, finished_at, coalesce(reply_body, ''), draft_id, error`

func scanRoute(row pgx.Row) (Route, error) {
	var r Route
	var st string
	err := row.Scan(&r.ID, &r.RouteID, &r.OwnerID, &r.MessageID, &r.AgentID, &r.TargetKind, &r.TargetKey, &r.TargetLabel,
		&r.SendQueueID, &st, &r.CreatedAt, &r.TimeoutAt, &r.AnsweredAt, &r.FinishedAt, &r.ReplyBody, &r.DraftID, &r.Error)
	r.State = RouteState(st)
	return r, err
}

func clampRouteError(msg string) string {
	if len(msg) > MaxRouteError {
		return strings.ToValidUTF8(msg[:MaxRouteError], "")
	}
	return msg
}

// CreateRoute opens a route for message messageID, by agent agentID of
// owner ownerID, to target, answerable until timeout from now. The message
// must be one delivered (or being delivered) to that agent (DeliveredTo), not
// marked no_reply, with no reply already approved and in flight, and not
// already routed (ErrAlreadyRouted).
func (s *PGStore) CreateRoute(ctx context.Context, agentID, ownerID, messageID int64, target RouteTarget, timeout time.Duration) (Route, error) {
	if agentID <= 0 || ownerID <= 0 {
		return Route{}, invalid("the agent has no owner")
	}
	var r Route
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		m, inFlight := lockMessage(ctx, tx, messageID, 0)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		if !DeliveredTo(m, agentID, ownerID) {
			return ErrNotFound
		}
		if m.NoReply {
			return ErrNoReply
		}
		if inFlight != nil {
			return inFlight
		}
		var err error
		r, err = scanRoute(tx.QueryRow(ctx, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id,
				target_kind, target_key, target_label, timeout_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8)) RETURNING `+routeCols,
			ownerID, NewRouteID(), messageID, agentID, target.Kind, target.Key, cutRunes(target.Label, 200), timeout.Seconds()))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "session_routes_one_open" {
			return ErrAlreadyRouted
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, "session_route.routed", RouteObject(r.RouteID), map[string]any{
			"message_id": messageID, "agent_id": agentID, "target": target.Kind + ":" + target.Key,
			"timeout_at": r.TimeoutAt.UTC().Format(time.RFC3339)})
	})
	return r, err
}

// Route reads one of the principal's routes.
func (s *PGStore) Route(ctx context.Context, routeID string) (Route, error) {
	if !ValidRouteID(routeID) {
		return Route{}, ErrRouteNotFound
	}
	r, err := scanRoute(s.pool.QueryRow(ctx, `SELECT `+routeCols+` FROM session_routes WHERE route_id = $1`, routeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, ErrRouteNotFound
	}
	return r, err
}

// SetRouteQueued records the send-queue row that carries routeID's message into
// its session (a mirrored target).
func (s *PGStore) SetRouteQueued(ctx context.Context, routeID string, sendQueueID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE session_routes SET send_queue_id = $2 WHERE route_id = $1`, routeID, sendQueueID)
	return err
}

// lockRoute is routeID's row, locked.
func lockRoute(ctx context.Context, tx pgx.Tx, routeID string) (Route, error) {
	if !ValidRouteID(routeID) {
		return Route{}, ErrRouteNotFound
	}
	r, err := scanRoute(tx.QueryRow(ctx, `SELECT `+routeCols+` FROM session_routes WHERE route_id = $1 FOR UPDATE`, routeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, ErrRouteNotFound
	}
	return r, err
}

// FailRoute ends an OPEN route as failed, recording why (the message could not
// be put into the session, or its window moved on) and, when handBack is not
// "", the hand-back it owes the agent. A route that is no longer open is
// returned unchanged, and owes nothing new.
func (s *PGStore) FailRoute(ctx context.Context, routeID, why, handBack string) (Route, error) {
	var r Route
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockRoute(ctx, tx, routeID)
		if err != nil {
			return err
		}
		r = cur
		if cur.State != RouteRouted && cur.State != RouteAnswered {
			return nil
		}
		r, err = failRouteTx(ctx, tx, cur, why, handBack)
		return err
	})
	return r, err
}

// failRouteTx ends cur — locked by the caller, and open — as failed, in tx,
// owing the agent handBack ("" for nothing).
func failRouteTx(ctx context.Context, tx pgx.Tx, cur Route, why, handBack string) (Route, error) {
	r, err := scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = 'failed', finished_at = now(), error = $2,
		handback = nullif($3, '') WHERE route_id = $1 RETURNING `+routeCols, cur.RouteID, clampRouteError(why), clampHandBack(handBack)))
	if err != nil {
		return Route{}, err
	}
	return r, audit(ctx, tx, "session_route.failed", RouteObject(cur.RouteID), map[string]any{"from": string(cur.State), "why": clampRouteError(why)})
}

// AnswerRoute records body as routeID's answer. fresh is false when the route
// was ALREADY answered with exactly this body (a retried reply: the stored
// route is the answer, and nothing changes); any other answer to a route that
// is no longer routed is ErrRouteClosed. body is screened as a reply draft is
// (ScreenDraft), since it becomes one.
func (s *PGStore) AnswerRoute(ctx context.Context, routeID, body string) (r Route, fresh bool, err error) {
	body, err = ScreenDraft(body, s.denyWords)
	if err != nil {
		return Route{}, false, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockRoute(ctx, tx, routeID)
		if err != nil {
			return err
		}
		r = cur
		switch cur.State {
		case RouteRouted:
		case RouteAnswered, RouteRelayed, RouteFallback:
			if cur.ReplyBody == body {
				return nil
			}
			return ErrRouteClosed
		default:
			return ErrRouteClosed
		}
		if r, err = scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = 'answered', answered_at = now(), reply_body = $2
			WHERE route_id = $1 RETURNING `+routeCols, routeID, body)); err != nil {
			return err
		}
		fresh = true
		return audit(ctx, tx, "session_route.answered", RouteObject(routeID), map[string]any{"bytes": len(body)})
	})
	return r, fresh, err
}

// RelayRoute turns routeID's answer into the reply draft for its message,
// ONCE: approve files it already APPROVED (relayed — the plugin's next claim
// takes it, the owner is not asked), otherwise PENDING for the owner
// (fallback). A route that already became a draft returns that draft and
// files nothing (exactly once). A relay that cannot be made — the agent is
// gone, the message is no_reply, another reply to it is already in flight, a
// pending draft would pass MaxPendingDraftsPerAgent — fails the route, saying
// why, in the same transaction (ErrRouteClosed: exactly one caller gets it).
func (s *PGStore) RelayRoute(ctx context.Context, routeID, agentName string, approve bool) (Route, Draft, error) {
	if err := s.sweep(ctx); err != nil {
		return Route{}, Draft{}, err
	}
	var r Route
	var d Draft
	var failWhy string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The MESSAGE lock first, as every draft write takes it (lockMessage),
		// then the route.
		var messageID int64
		if err := tx.QueryRow(ctx, `SELECT plugin_message_id FROM session_routes WHERE route_id = $1`, routeID).Scan(&messageID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRouteNotFound
			}
			return err
		}
		m, inFlight := lockMessage(ctx, tx, messageID, 0)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		cur, err := lockRoute(ctx, tx, routeID)
		if err != nil {
			return err
		}
		r = cur
		if cur.DraftID != nil {
			// 🔴 EXACTLY ONCE: it is already a draft.
			d, err = scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1`, *cur.DraftID))
			return err
		}
		if cur.State != RouteAnswered {
			return ErrRouteNotAnswered
		}
		switch {
		case cur.AgentID == nil:
			failWhy = "the agent that routed it no longer exists"
		case m.NoReply:
			failWhy = "the message may not be replied to (no_reply)"
		case inFlight != nil:
			failWhy = "another reply to the message is already approved and on its way"
		}
		if failWhy == "" && !approve {
			// FileDraft's cap: a PENDING draft awaits the owner, and one agent may
			// have at most MaxPendingDraftsPerAgent of them (this message's own
			// pending one, superseded below, not counted). Serialised as FileDraft
			// serialises it.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_reply_drafts.file'), $1::int)`, int32(*cur.AgentID)); err != nil {
				return err
			}
			var pending int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_reply_drafts
				WHERE agent_id = $1 AND status = 'pending' AND message_id <> $2`, *cur.AgentID, messageID).Scan(&pending); err != nil {
				return err
			}
			if pending >= MaxPendingDraftsPerAgent {
				failWhy = fmt.Sprintf("%d reply drafts already await the owner (the limit is %d)", pending, MaxPendingDraftsPerAgent)
			}
		}
		if failWhy != "" {
			// Failed IN THIS TRANSACTION, under the route's lock: a second relay
			// waiting on it then finds the route failed (ErrRouteNotAnswered),
			// so exactly one caller sees the failure happen.
			r, err = failRouteTx(ctx, tx, cur, failWhy, "its answer could not be sent ("+failWhy+")")
			return err
		}
		// FileDraft's supersede, with FileDraft's accounting: the count on the
		// filing's audit row.
		tag, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET status = 'superseded', decided_at = now()
			WHERE message_id = $1 AND status = 'pending'`, messageID)
		if err != nil {
			return err
		}
		superseded := int(tag.RowsAffected())
		status, q := DraftPending, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, route_id)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), $7) RETURNING `+draftCols
		if approve {
			status, q = DraftApproved, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, route_id,
				status, decided_at, approved_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), $7, 'approved', now(), now()) RETURNING `+draftCols
		}
		if d, err = scanDraft(tx.QueryRow(ctx, q, cur.OwnerID, messageID, *cur.AgentID, cutRunes(agentName, 100), cur.ReplyBody,
			DraftTTL.Seconds(), routeID)); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.draft.filed", DraftObject(d.ID), map[string]any{
			"message_id": messageID, "agent_id": *cur.AgentID, "route_id": routeID, "superseded": superseded}); err != nil {
			return err
		}
		next := RouteFallback
		if status == DraftApproved {
			next = RouteRelayed
			if err := audit(ctx, tx, "plugin.draft.approved", DraftObject(d.ID), map[string]any{
				"message_id": messageID, "route_id": routeID, "auto": true}); err != nil {
				return err
			}
		}
		if r, err = scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = $2, draft_id = $3, finished_at = now()
			WHERE route_id = $1 RETURNING `+routeCols, routeID, string(next), d.ID)); err != nil {
			return err
		}
		return audit(ctx, tx, "session_route."+string(next), RouteObject(routeID), map[string]any{"draft_id": d.ID})
	})
	if err != nil {
		return r, d, err
	}
	if failWhy != "" {
		return r, Draft{}, fmt.Errorf("%w: %s", ErrRouteClosed, failWhy)
	}
	return r, d, nil
}

// TimeOutRoute times routeID out when it is still routed and its deadline has
// passed (on the database's clock). timedOut is false — and nothing changes —
// otherwise.
func (s *PGStore) TimeOutRoute(ctx context.Context, routeID string) (r Route, timedOut bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockRoute(ctx, tx, routeID)
		if err != nil {
			return err
		}
		r = cur
		var due bool
		if err := tx.QueryRow(ctx, `SELECT timeout_at <= now() FROM session_routes WHERE route_id = $1`, routeID).Scan(&due); err != nil {
			return err
		}
		if cur.State != RouteRouted || !due {
			return nil
		}
		if r, err = scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = 'timed_out', finished_at = now(),
			error = 'no answer before the deadline', handback = $2 WHERE route_id = $1 RETURNING `+routeCols,
			routeID, "no answer came by "+cur.TimeoutAt.UTC().Format(time.RFC3339))); err != nil {
			return err
		}
		timedOut = true
		return audit(ctx, tx, "session_route.timed_out", RouteObject(routeID), map[string]any{
			"timeout_at": cur.TimeoutAt.UTC().Format(time.RFC3339)})
	})
	return r, timedOut, err
}

// DueRoute is one route the sweep must act on, and whose: a ROUTED one past
// its deadline (time it out), an ANSWERED one whose relay never happened —
// a crash or an error between the answer's commit and the relay — which would
// otherwise hold the message's one open route forever (relay it again: the
// relay is idempotent), or an ended one that still OWES its agent a hand-back
// (HandBack; deliver it).
type DueRoute struct {
	RouteID  string
	OwnerID  int64
	State    RouteState
	HandBack bool
}

// RelayGrace is how long an answered route may wait for its relay before the
// sweep retries it: longer than any relay that is merely in progress.
const RelayGrace = time.Minute

// ExpiredRoutes is every route the sweep must act on (DueRoute), across every
// owner — ids, owners and states only. Under the system scope (internal/authz's ledger
// names this one method): each is then timed out AS ITS OWNER (TimeOutRoute).
func (s *PGStore) ExpiredRoutes(ctx context.Context) ([]DueRoute, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT route_id, owner_id, state, handback IS NOT NULL FROM session_routes
		WHERE (state = 'routed' AND timeout_at <= now())
		   OR (state = 'answered' AND answered_at <= now() - make_interval(secs => $1))
		   OR handback IS NOT NULL
		ORDER BY id`, RelayGrace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DueRoute, error) {
		var d DueRoute
		var st string
		err := row.Scan(&d.RouteID, &d.OwnerID, &st, &d.HandBack)
		d.State = RouteState(st)
		return d, err
	})
}

// MaxHandBack bounds a recorded hand-back reason (0021's cap).
const MaxHandBack = 2048

func clampHandBack(s string) string {
	if len(s) <= MaxHandBack {
		return s
	}
	return strings.ToValidUTF8(s[:MaxHandBack], "")
}

// TakeHandBack clears routeID's owed hand-back and returns it with the route,
// under the route's lock: of two takers, exactly one gets ok.
func (s *PGStore) TakeHandBack(ctx context.Context, routeID string) (Route, string, bool, error) {
	var r Route
	var reason *string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockRoute(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT handback FROM session_routes WHERE id = $1`, cur.ID).Scan(&reason); err != nil {
			return err
		}
		if reason == nil {
			return nil
		}
		r = cur
		_, err = tx.Exec(ctx, `UPDATE session_routes SET handback = NULL WHERE id = $1`, cur.ID)
		return err
	})
	if err != nil || reason == nil {
		return Route{}, "", false, err
	}
	return r, *reason, true, nil
}
