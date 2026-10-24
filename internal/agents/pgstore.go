package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// ownerOfAgent is the owner_id a CHILD row of agent $1 (a chat session or
// message) is inserted with: the agent's own (accounts PR 3a). 0006's composite
// foreign keys already demand child.owner_id = agent.owner_id, so it is the only
// legal value — and the kickoff deliverer, which runs as authz.System and names
// no user, writes chat rows for whichever user's agent it is delivering to. An
// agent the caller cannot see, or none, yields the caller's own id instead, so
// the foreign key refuses the row as it did before.
const ownerOfAgent = `COALESCE((SELECT owner_id FROM agents WHERE id = $1), nullif(current_setting('muster.uid', true), '')::bigint)`

// PGStore is the Postgres-backed agents Store.
type PGStore struct{ pool querier }

// querier is what every PGStore statement goes through: a pool, or one
// transaction (NewPGTx).
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	// Begin opens a transaction (on a pool) or a savepoint (inside NewPGTx's).
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewPG constructs a Postgres-backed agents Store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// NewPGTx is a Store whose every statement runs inside tx, so several calls
// commit or roll back together. The caller owns tx (Commit / Rollback).
func NewPGTx(tx pgx.Tx) *PGStore { return &PGStore{pool: tx} }

const agentCols = `id, name, namespace, display_name, repo, repo_branch, note_id, note_text,
	pending_note, model, status, hooks_token, kicked_off, last_output, error_message, created_at, updated_at,
	kickoff_pod, kickoff_restarts, kickoff_attempts, kickoff_error, kind, cc_account, owner_id, worker_pool, gateway_secret`

func scanAgent(row interface {
	Scan(dest ...any) error
}) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.Name, &a.Namespace, &a.DisplayName, &a.Repo, &a.RepoBranch,
		&a.NoteID, &a.NoteText, &a.PendingNote, &a.Model, &a.Status, &a.HooksToken, &a.KickedOff,
		&a.LastOutput, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt,
		&a.KickoffPod, &a.KickoffRestarts, &a.KickoffAttempts, &a.KickoffError, &a.Kind, &a.CCAccount, &a.OwnerID, &a.WorkerPool,
		&a.GatewaySecret)
	return a, err
}

// Create inserts an agent and returns it with generated id/timestamps.
func (s *PGStore) Create(ctx context.Context, a Agent) (Agent, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO agents (name, namespace, display_name, repo, repo_branch, note_id, note_text, pending_note, model, status, hooks_token, kind, cc_account, worker_pool)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+agentCols,
		a.Name, a.Namespace, a.DisplayName, a.Repo, a.RepoBranch, a.NoteID, a.NoteText,
		a.PendingNote, a.Model, nonEmptyStatus(a.Status), a.HooksToken, ResolveKind(a.Kind), a.CCAccount, a.WorkerPool)
	return scanAgent(row)
}

func (s *PGStore) Get(ctx context.Context, id int64) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id=$1`, id))
}

func (s *PGStore) GetByName(ctx context.Context, name string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE name=$1`, name))
}

// GetByHooksToken looks an agent up by its per-agent hooks token. This is the
// agent self-service auth path: the token is unique per agent and injected into
// the pod, so a valid bearer both authenticates and identifies the caller.
func (s *PGStore) GetByHooksToken(ctx context.Context, token string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE hooks_token=$1`, token))
}

func (s *PGStore) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentsByNoteIDs returns the latest agent per requested note id. It queries with
// `note_id = ANY($1)` ordered created_at ASC so, as rows are scanned, a later
// (more recent) agent overwrites an earlier one for the same note — leaving the
// most-recent linked agent in the map. Uses the STORED status column (no k8s).
func (s *PGStore) AgentsByNoteIDs(ctx context.Context, ids []int64) (map[int64]Agent, error) {
	out := make(map[int64]Agent, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+agentCols+` FROM agents WHERE note_id = ANY($1) ORDER BY created_at ASC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		if a.NoteID != nil {
			out[*a.NoteID] = a // ASC order → most-recent agent wins
		}
	}
	return out, rows.Err()
}

// LastMessageByAgentIDs returns the newest chat_messages.created_at per requested
// agent id, in ONE grouped query (no per-agent fan-out — the machine API resolves
// a whole page of agents at once). Agents with no persisted transcript row are
// absent from the map rather than mapped to the zero time, so the caller can tell
// "never produced a turn" from "produced one at the epoch".
func (s *PGStore) LastMessageByAgentIDs(ctx context.Context, ids []int64) (map[int64]time.Time, error) {
	out := make(map[int64]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT agent_id, max(created_at) FROM chat_messages WHERE agent_id = ANY($1) GROUP BY agent_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// NameExists reports whether a name is TAKEN — by a live agent, or forever by
// one that was destroyed.
//
// 🔴 THE SECOND EXISTS IS THE ENFORCEMENT POINT FOR THE NAME TOMBSTONE, AND IT
// IS HERE BECAUSE THIS IS THE ONLY THING THE GENERATOR ASKS. BuildUniqueAgentName
// accepts any candidate this returns false for. Delete is a HARD delete, so
// without the second clause a destroyed agent's name goes straight back into the
// bounded pool in names.go and the next generated agent can be its namesake —
// landing in the per-instance namespace and ServiceAccount NAMED after the dead
// one. Destroy can leave objects behind (a partial teardown that keeps going, a
// stranger's object it refuses to delete), and anything bound to that name out
// of band — a hand-made RoleBinding naming the ServiceAccount, say — would pass
// to the namesake silently. See migration 0002 for the trigger that fills the
// ledger this reads.
//
// ⚠ THE LEDGER IS ONLY EVER READ HERE. Nothing refuses an INSERT of a retired
// name, deliberately (migration 0002 says why: `chief` is provisioned with a
// hand-set name and a refusal would make re-provisioning it impossible). So this
// closes the AUTO-GENERATED path and no other.
//
// 🔴 THIS SELECT RUNS AS THE CONNECTING ROLE, WHICH IS WHY 0002 GRANTS SELECT ON
// THE LEDGER. The write side is a SECURITY DEFINER trigger and needs no grant;
// this side is ordinary application SQL and cannot be made to run as anyone
// else. `CREATE TABLE` grants nothing to anyone but the owner, so a deployment
// whose migrating role is not its connecting role got
// `permission denied for table agent_retired_names` (42501) here — on EVERY
// auto-named agent creation, since BuildUniqueAgentName asks nothing else.
// TestANonOwnerRoleCanBothRetireANameAndReadTheLedger pins both halves together;
// each passes alone while the pair is broken.
//
// 🔴 IT ASKS A SECURITY DEFINER FUNCTION, NOT THE TABLE (accounts PR 3a). An
// agent name is ONE GLOBAL NAMESPACE — it becomes a Kubernetes namespace — but
// under row-level security a plain SELECT on agents sees only the caller's own
// agents, so user B would be told user A's agent's name is free and fail later
// on the UNIQUE index (or, for a retired name, not at all). Migration 0007's
// muster_agent_name_taken reads every user's names and returns ONLY the
// boolean; it is the same two EXISTS clauses this method used to run.
func (s *PGStore) NameExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT public.muster_agent_name_taken($1)`, name).Scan(&exists)
	return exists, err
}

func (s *PGStore) UpdateStatus(ctx context.Context, id int64, status, lastOutput, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE agents SET status=$2, last_output=$3, error_message=$4, updated_at=now() WHERE id=$1`,
		id, status, lastOutput, errMsg)
	return err
}

func (s *PGStore) SetModel(ctx context.Context, id int64, model string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx,
		`UPDATE agents SET model=$2, updated_at=now() WHERE id=$1 RETURNING `+agentCols, id, model))
}

// SetDisplayName updates an agent's display name. A blank name falls back to the
// agent's immutable slug (the `name` column) so the card is never label-less.
func (s *PGStore) SetDisplayName(ctx context.Context, id int64, name string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx,
		`UPDATE agents SET display_name=COALESCE(NULLIF($2,''), name), updated_at=now()
		 WHERE id=$1 RETURNING `+agentCols, id, name))
}

func (s *PGStore) SetHooksToken(ctx context.Context, id int64, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET hooks_token=$2, updated_at=now() WHERE id=$1`, id, token)
	return err
}

// FoldResult is what FoldIntoPendingNote did.
type FoldResult int

const (
	// FoldNotOwed: the first message is no longer owed (stamped, or none);
	// nothing was written and the caller delivers another way.
	FoldNotOwed FoldResult = iota
	// FoldAdded: the text was folded in.
	FoldAdded
	// FoldOverLimit: the text was NOT added — the first message is at its
	// size bound; it says so (the over line, once). The text was not delivered.
	FoldOverLimit
)

// jsonLen is s's length once JSON-encoded as a string, by encoding/json (what
// the turn's request body uses): `<`, `>` and `&` become six bytes, a quote,
// a backslash and a newline two.
func jsonLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		return 6 * len(s) // worst case; Marshal of a string does not fail
	}
	return len(b) - 2
}

// FoldIntoPendingNote is AppendPendingNote for a message folded into an owed
// first turn by someone other than its author (a forward): text goes BEFORE
// last when the note ends with it (muster's rules stay the last word), else at
// the end. 🔴 THE BOUND IS ON THE ENCODED SIZE: maxBytes is compared with the
// note's JSON-encoded length (jsonLen), because the runtime's read limit
// applies to the encoded request, and an HTML-heavy text grows up to six-fold.
// Past it the text is not added and over is (once): FoldOverLimit.
func (s *PGStore) FoldIntoPendingNote(ctx context.Context, id int64, text, last string, maxBytes int, over string) (FoldResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FoldNotOwed, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var note string
	if err := tx.QueryRow(ctx, `SELECT pending_note FROM agents
		WHERE id = $1 AND NOT kicked_off AND pending_note <> '' FOR UPDATE`, id).Scan(&note); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FoldNotOwed, nil
		}
		return FoldNotOwed, err
	}
	add, res := text, FoldAdded
	if jsonLen(note)+jsonLen(text) > maxBytes {
		if strings.Contains(note, over) {
			return FoldOverLimit, tx.Commit(ctx) // already says so
		}
		add, res = over, FoldOverLimit
	}
	if last != "" && strings.HasSuffix(note, last) {
		note = strings.TrimSuffix(note, last) + add + last
	} else {
		note += add
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET pending_note = $2 WHERE id = $1`, id, note); err != nil {
		return FoldNotOwed, err
	}
	return res, tx.Commit(ctx)
}

func (s *PGStore) SetGatewaySecret(ctx context.Context, id int64, secret string) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET gateway_secret=$2, updated_at=now() WHERE id=$1`, id, secret)
	return err
}

// AppendPendingNote appends text to agent id's first message while that
// message is still owed — not kicked off, and non-empty — and reports whether
// it did. The kickoff re-reads the note AFTER stamping kicked_off
// (agentkickoff.deliver), so an append either lands before the stamp and rides
// the first turn, or is refused (false) and the caller delivers another way.
// updated_at is left alone: an append is not the agent's activity, and the
// kickoff's dwell bound reads it.
func (s *PGStore) AppendPendingNote(ctx context.Context, id int64, text string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agents SET pending_note = pending_note || $2
		WHERE id = $1 AND NOT kicked_off AND pending_note <> ''`, id, text)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGStore) SetKickedOff(ctx context.Context, id int64, v bool) error {
	// The wait markers are cleared by the stamp itself (see Store.SetKickedOff).
	_, err := s.pool.Exec(ctx, `UPDATE agents SET kicked_off=$2,
		kickoff_error = CASE WHEN $2 AND (starts_with(kickoff_error, $3) OR starts_with(kickoff_error, $4))
			THEN '' ELSE kickoff_error END,
		updated_at=now() WHERE id=$1`, id, v, KickoffAwaitingGrantReason, KickoffAwaitingCloneReason)
	return err
}

// RecordKickoffDelivery stamps the recipient pod + restart count, burns one unit
// of the re-send budget, and clears the previous send error. updated_at is
// deliberately bumped: this IS a state transition worth showing as activity.
func (s *PGStore) RecordKickoffDelivery(ctx context.Context, id int64, pod string, restarts int32) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET kickoff_pod=$2, kickoff_restarts=$3,
		       kickoff_attempts=kickoff_attempts+1, kickoff_error='', updated_at=now()
		 WHERE id=$1`, id, pod, restarts)
	return err
}

// SetKickoffError records a kickoff send failure. It touches neither status nor
// updated_at — the status verdict belongs to the reconciler.
//
// 🔴 updated_at IS DELIBERATELY NOT BUMPED, AND THIS WAS A LATENT DEFECT UNTIL A
// DELIVERER DROVE THE DECISION TABLE. agents.DecideReconcile bounds the
// ActionRetryKickoff branch ONLY by `now − updated_at > ProvisioningStuckTimeout`,
// so a write here that bumped it reset that clock on every failed retry and made
// ActionError unreachable — an agent whose send failed every tick stayed in
// `provisioning` for ever. A failed send is evidence, not activity.
// RecordKickoffDelivery still bumps it: a delivery IS a transition. Pinned by
// TestSetKickoffErrorDoesNotMoveTheDwellClock (and, behaviourally, by
// internal/agentkickoff's dwell-boundary test against this store).
func (s *PGStore) SetKickoffError(ctx context.Context, id int64, msg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET kickoff_error=$2 WHERE id=$1`, id, msg)
	return err
}

// MarkKickoffStuck is the conditional `error` write for an owed row the deliverer
// judged stuck (see the Store contract). updated_at is compared for EQUALITY with
// the value the caller read: every writer that moves the row on (UpdateStatus,
// SetKickedOff, RecordKickoffDelivery) bumps it, and SetKickoffError, which a
// failed retry writes every tick, deliberately does not.
//
// ⚠ THERE IS NO `NOT kicked_off` CONJUNCT, AND THAT IS NOT AN OMISSION. The caller
// only judges rows it read with kicked_off=f, and the only writer of kicked_off
// (SetKickedOff) bumps updated_at, so the equality already refuses a stamped row; a
// second conjunct could not change any outcome and no test could fail without it.
// The claim conjunct is separate because ClaimKickoff deliberately does NOT bump
// updated_at.
func (s *PGStore) MarkKickoffStuck(ctx context.Context, id int64, seenUpdatedAt time.Time, errMsg string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET status=$2, last_output='', error_message=$3, updated_at=now()
		 WHERE id=$1
		   AND updated_at=$4
		   AND (kickoff_claim_expires_at IS NULL OR kickoff_claim_expires_at <= now())`,
		id, StatusError, errMsg, seenUpdatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkRunning promotes a provisioning row the caller saw at seenUpdatedAt to
// running (see the Store contract). updated_at is bumped: this IS a status
// transition, and it is what moves agents.LastActivity for the Agents list.
func (s *PGStore) MarkRunning(ctx context.Context, id int64, seenUpdatedAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET status=$2, error_message='', updated_at=now()
		 WHERE id=$1
		   AND status=$3
		   AND updated_at=$4`,
		id, StatusRunning, StatusProvisioning, seenUpdatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClearKickoffDelivery drops the recipient stamp (see the Store contract). The
// attempt counter and the last send error are left alone — they are history.
func (s *PGStore) ClearKickoffDelivery(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE agents SET kickoff_pod='', kickoff_restarts=0, updated_at=now() WHERE id=$1`, id)
	return err
}

// ClaimKickoff takes the agent's cross-process kickoff claim (see the Store
// contract; the columns are in internal/db/migrations/0001_init.sql). One atomic
// conditional UPDATE: it wins iff the
// claim is unheld (NULL) or already expired, and holds NO connection afterwards
// — which is the whole reason this is a row and not pg_try_advisory_lock (a
// session lock would pin one of MaxConns=8 for up to KickoffWorkTimeout).
//
// `now()` is Postgres's clock on BOTH sides of the comparison, so two pods with
// skewed system clocks still agree about whether a claim has expired.
//
// updated_at is deliberately NOT bumped: a claim is bookkeeping about who is
// working, not a state transition worth surfacing as agent activity (and the
// renew ticker would otherwise make every long kickoff look continuously busy).
func (s *PGStore) ClaimKickoff(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET kickoff_claim_owner=$2, kickoff_claim_expires_at=now()+make_interval(secs => $3)
		 WHERE id=$1
		   AND (kickoff_claim_expires_at IS NULL OR kickoff_claim_expires_at <= now())`,
		id, owner, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RenewKickoffClaim extends this owner's claim (see the Store contract). The
// owner predicate is load-bearing: without it a process whose TTL lapsed would
// stamp over the claim another pod legitimately took, and two kickoffs would run.
func (s *PGStore) RenewKickoffClaim(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET kickoff_claim_expires_at=now()+make_interval(secs => $3)
		 WHERE id=$1 AND kickoff_claim_owner=$2`,
		id, owner, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseKickoffClaim clears this owner's claim (see the Store contract). Same
// owner predicate, same reason: releasing someone else's claim would hand a
// third process a green light while the second is still working.
func (s *PGStore) ReleaseKickoffClaim(ctx context.Context, id int64, owner string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET kickoff_claim_owner='', kickoff_claim_expires_at=NULL
		 WHERE id=$1 AND kickoff_claim_owner=$2`, id, owner)
	return err
}

// ClearKickoffClaimsByOwnerPrefix clears claims whose owner begins with prefix
// (see the Store contract). An empty prefix is REFUSED rather than treated as
// "match everything": `LIKE '%'` would clear every live peer pod's claim.
//
// The prefix is escaped for LIKE, so a hostname containing % or _ (legal in a
// container hostname override) cannot widen the match.
func (s *PGStore) ClearKickoffClaimsByOwnerPrefix(ctx context.Context, prefix string) (int64, error) {
	if prefix == "" {
		return 0, errors.New("clear kickoff claims: empty owner prefix would clear every pod's claims")
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents
		   SET kickoff_claim_owner='', kickoff_claim_expires_at=NULL
		 WHERE kickoff_claim_owner LIKE $1 || '%'`, esc)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PGStore) Delete(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	return err
}

const sessionCols = `id, agent_id, session_key, title, created_at, updated_at, read_at, runtime, model, is_default`

func scanSession(row interface {
	Scan(dest ...any) error
}) (ChatSession, error) {
	var c ChatSession
	err := row.Scan(&c.ID, &c.AgentID, &c.SessionKey, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ReadAt, &c.Runtime, &c.Model, &c.IsDefault)
	return c, err
}

// legacySessionKey is the pre-multi-session fixed gateway key. A pre-sessions
// agent's first auto-created session reuses it so its existing gateway context
// carries over; brand-new sessions get unique keys via newSessionKey.
func legacySessionKey(agentName string) string { return "agent:" + agentName + ":webchat" }

// newSessionKey is a fresh, unique gateway session key for a new conversation.
func newSessionKey(agentName string) string {
	return fmt.Sprintf("agent:%s:webchat:%d", agentName, time.Now().UnixNano())
}

func (s *PGStore) CreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, owner_id, session_key, title) VALUES ($1, `+ownerOfAgent+`, $2, '')
		RETURNING `+sessionCols, agentID, newSessionKey(agentName)))
}

func (s *PGStore) ListSessions(ctx context.Context, agentID int64) ([]ChatSession, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 ORDER BY updated_at DESC, id DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChatSession, 0)
	for rows.Next() {
		c, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// searchSnippetLateral picks the NEWEST matching text row of session s and
// cuts the excerpt in SQL (see SearchSnippetRadius): $2 the query, $3 its
// %pattern%, $4/$5 the radius and length. One copy, used by both reads.
const searchSnippetLateral = `
		LEFT JOIN LATERAL (
			SELECT substring(
			           cm.content
			           FROM greatest(1, position(lower($2) IN lower(cm.content)) - $4::int)
			           FOR $5::int
			       ) AS snippet
			FROM chat_messages cm
			WHERE cm.session_id = s.id
			  AND cm.kind = 'text'
			  AND cm.content ILIKE $3 ESCAPE '\'
			ORDER BY cm.created_at DESC, cm.id DESC
			LIMIT 1
		) m ON TRUE`

// searchRankedTitlesSQL is the RANKED read: the sessions whose TITLE starts
// with the query ($7, rank 0) or has a word that does ($8, rank 1), best then
// newest, at most $6 — and only those $6 have their messages read (the
// snippet's lateral runs over the bounded candidates, not every session).
//
// 🔴 ONE RANKED SCAN OVER TITLE-OR-BODY WAS A REGRESSION, MEASURED. Ordering the
// whole title-or-body match set by rank made Postgres evaluate the message
// lateral for EVERY session of the agent before the LIMIT could apply. Measured
// with EXPLAIN ANALYZE at 3000 sessions × 20 messages: that shape took 52–60 ms
// warm (947 ms cold); this read 2.2–2.5 ms, one recent batch 1.5–2.2 ms. (An
// earlier comment here said "~1.8 ms → ~200 ms"; that run was not reproduced,
// and these are the figures the commit quotes.) This read touches only the
// agent's session titles before its LIMIT. TestSearchSessionsReadsABoundedNumberOfMessageSets
// counts the sessions' messages each read looked at, on the plan the test
// server chose.
const searchRankedTitlesSQL = `
		WITH cand AS (
			SELECT s.id, s.agent_id, s.session_key, s.title, s.created_at, s.updated_at, s.read_at,
		       s.runtime, s.model, s.is_default,
			       CASE WHEN s.title ILIKE $7 ESCAPE '\' THEN 0 ELSE 1 END AS rank
			FROM chat_sessions s
			WHERE s.agent_id = $1
			  AND s.title ILIKE $3 ESCAPE '\'
			  AND (s.title ILIKE $7 ESCAPE '\' OR s.title ~* $8)
			ORDER BY rank, s.updated_at DESC, s.id DESC
			LIMIT $6
		)
		SELECT s.id, s.agent_id, s.session_key, s.title, s.created_at, s.updated_at, s.read_at,
		       s.runtime, s.model, s.is_default,
		       COALESCE(m.snippet, '') AS snippet
		FROM cand s` + searchSnippetLateral + `
		ORDER BY s.rank, s.updated_at DESC, s.id DESC`

// searchRecentSQL is the RECENT read: ONE BATCH of the agent's sessions,
// newest first, after the keyset cursor ($6 updated_at, $7 id; NULL for the
// first), at most $8 of them — each with its snippet and whether it matches
// (title or a text message). SearchSessions walks batches until it has its limit of
// matches or the sessions run out.
//
// THE BATCH'S LIMIT IS INSIDE THE SUBQUERY. The query's original
// single-statement shape (title-or-body, ORDER BY updated_at, LIMIT) stops
// early only when the planner walks idx_chat_sessions_agent; in one full
// package run it was MEASURED to choose a sequential scan and a sort instead
// and read all 1000 sessions' messages for a limit of 10. With the LIMIT in
// the subquery the lateral is written over at most $8 rows.
// ⚠ WHAT THE GUARD PROVES IS NARROWER: TestSearchSessionsReadsABoundedNumberOfMessageSets
// counts the loops on the plan its server chose for this fixture, and on PG16
// that plan is bounded for BOTH shapes — it does not pin the subquery's LIMIT as
// the reason, nor the bound under another plan. TestSearchSessionsIsTheTwoBoundedReads
// pins only that these are the statements SearchSessions sends.
const searchRecentSQL = `
		SELECT s.id, s.agent_id, s.session_key, s.title, s.created_at, s.updated_at, s.read_at,
		       s.runtime, s.model, s.is_default,
		       COALESCE(m.snippet, '') AS snippet,
		       (s.title ILIKE $3 ESCAPE '\' OR m.snippet IS NOT NULL) AS matched
		FROM (
			SELECT id, agent_id, session_key, title, created_at, updated_at, read_at, runtime, model, is_default
			FROM chat_sessions
			WHERE agent_id = $1
			  AND ($6::timestamptz IS NULL OR (updated_at, id) < ($6::timestamptz, $7::bigint))
			ORDER BY updated_at DESC, id DESC
			LIMIT $8
		) s` + searchSnippetLateral + `
		ORDER BY s.updated_at DESC, s.id DESC`

// searchRecentBatch is how many sessions the FIRST recent read looks at;
// nextRecentBatch is the next read's, doubling up to searchRecentMaxBatch.
//
// 🔴 GEOMETRIC, BECAUSE A SEARCH THAT FINDS FEW OR NOTHING READS EVERY SESSION.
// With fewer matches than the limit the walk only stops when the sessions run
// out, so its WORK is the agent's whole history whatever the batch — the same
// as main's single query, which also read every session's messages for such a
// search. What the batch size decides is the ROUND TRIPS: a fixed 2×limit was
// 30 of them at 3000 sessions (measured 114–144 ms for no match vs main's
// 60–72 ms). Doubling makes it logarithmic (5 at 3000 for limit 50), while a
// search that fills its limit early still stops after the first small read.
func searchRecentBatch(limit int) int { return 2 * limit }

const searchRecentMaxBatch = 3200

func nextRecentBatch(b int) int {
	if b*2 > searchRecentMaxBatch {
		return searchRecentMaxBatch
	}
	return b * 2
}

// SearchSessions implements Store.
//
// 🔴 A PLAIN ILIKE AND NO INDEX, AND THE DECISION IS MEASURED RATHER THAN
// ASSUMED. On the deployment this was extracted from: chat_messages held 34 rows
// in total (12 of them kind='text'), and the whole relation — table plus its two
// indexes — was 248 kB. A sequential scan of 248 kB is sub-millisecond, so an
// index here would be pure cost. pg_trgm IS available on that server (1.6,
// uninstalled) should the table ever grow enough to need one, but installing it
// means CREATE EXTENSION in a migration, which is a privilege a managed Postgres
// can refuse — so it is a deliberate later decision, not a default.
//
// ⚠ THE FIGURE TO RE-MEASURE BEFORE ADDING ONE IS THE kind='text' ROW COUNT,
// not the total: the scan this query does is already restricted to those rows by
// the subquery's predicate.
func (s *PGStore) SearchSessions(ctx context.Context, agentID int64, q string, limit int) ([]SessionMatch, error) {
	q = strings.TrimSpace(q)
	if q == "" || limit <= 0 {
		return []SessionMatch{}, nil
	}
	if limit > SearchSessionsMaxLimit {
		limit = SearchSessionsMaxLimit
	}
	pattern := "%" + db.LikeEscape(q) + "%"
	prefix := db.LikeEscape(q) + "%"
	wordStart := `(^|[^[:alnum:]])` + regexpLiteral(q)

	// TWO BOUNDED READS, NOT ONE RANKED SCAN — see searchRankedTitlesSQL. The
	// ranked read answers the title matches that rank 0 and 1, at most `limit`;
	// the recent read answers title-or-body matches newest first, at most
	// `limit`, stopping as soon as it has them (idx_chat_sessions_agent). What
	// the first did not answer is rank 2, in the second's (recency) order.
	ranked, err := s.searchRows(ctx, searchRankedTitlesSQL,
		agentID, q, pattern, SearchSnippetRadius, SearchSnippetLen, limit, prefix, wordStart)
	if err != nil {
		return nil, err
	}
	out := ranked
	if len(out) < limit {
		seen := make(map[int64]bool, len(out))
		for _, m := range out {
			seen[m.ID] = true
		}
		var after *time.Time
		var afterID int64
		batch := searchRecentBatch(limit)
		for len(out) < limit {
			rows, err := s.pool.Query(ctx, searchRecentSQL,
				agentID, q, pattern, SearchSnippetRadius, SearchSnippetLen, after, afterID, batch)
			if err != nil {
				return nil, err
			}
			n := 0
			for rows.Next() {
				var m SessionMatch
				var matched bool
				if err := rows.Scan(&m.ID, &m.AgentID, &m.SessionKey, &m.Title,
					&m.CreatedAt, &m.UpdatedAt, &m.ReadAt, &m.Runtime, &m.Model, &m.IsDefault, &m.Snippet, &matched); err != nil {
					rows.Close()
					return nil, err
				}
				n++
				at := m.UpdatedAt
				after, afterID = &at, m.ID
				if matched && !seen[m.ID] && len(out) < limit {
					m.Snippet = strings.Join(strings.Fields(m.Snippet), " ")
					seen[m.ID] = true
					out = append(out, m)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			if n < batch {
				break // the sessions ran out
			}
			batch = nextRecentBatch(batch)
		}
	}
	return out, nil
}

// searchRows runs one of the two search reads and scans its rows.
func (s *PGStore) searchRows(ctx context.Context, query string, args ...any) ([]SessionMatch, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionMatch, 0)
	for rows.Next() {
		var m SessionMatch
		if err := rows.Scan(&m.ID, &m.AgentID, &m.SessionKey, &m.Title,
			&m.CreatedAt, &m.UpdatedAt, &m.ReadAt, &m.Runtime, &m.Model, &m.IsDefault, &m.Snippet); err != nil {
			return nil, err
		}
		m.Snippet = strings.Join(strings.Fields(m.Snippet), " ")
		out = append(out, m)
	}
	return out, rows.Err()
}

// regexpLiteral is q as a Postgres regular expression that matches q itself:
// every character that is not a letter or a digit is backslash-escaped.
//
// ⚠ ONLY THOSE. In an ARE a backslash before a letter or digit is an escape of
// its own (\d, \w, \1), so escaping everything would turn "d" into a digit
// class; a backslash before anything else is that character, literally.
func regexpLiteral(q string) string {
	var b strings.Builder
	for _, r := range q {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *PGStore) GetSession(ctx context.Context, sessionID int64) (ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM chat_sessions WHERE id=$1`, sessionID))
}

// latestSessionOrder is what "an agent's latest session" means: the order
// LatestOrCreateSession takes its first row in (the session a bare
// GET /agents/{name} redirects to) and LatestSessionIDs takes each agent's first
// row in (the session an in-app link names, ui.AgentSessionHref). ONE spelling,
// so a link can never name a different session than the redirect would pick.
const latestSessionOrder = `updated_at DESC, id DESC`

// LatestSessionIDs returns, per requested agent id, the id of the session
// LatestOrCreateSession would return for it, in ONE query, WITHOUT creating
// anything: an agent with no session is absent from the map (its link stays the
// bare page, whose handler creates one). It exists so a list of agent links can
// name each agent's canonical ?session= URL instead of paying the bare URL's 303.
func (s *PGStore) LatestSessionIDs(ctx context.Context, ids []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (agent_id) agent_id, id FROM chat_sessions WHERE agent_id = ANY($1) ORDER BY agent_id, `+latestSessionOrder, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agentID, id int64
		if err := rows.Scan(&agentID, &id); err != nil {
			return nil, err
		}
		out[agentID] = id
	}
	return out, rows.Err()
}

// LatestOrCreateSession returns the agent's most-recently-updated session, or
// creates one. The created session uses the LEGACY key (so a pre-sessions agent
// keeps its gateway context) — the legacy key is UNIQUE, so a second call after
// one exists takes the latest branch and never collides.
func (s *PGStore) LatestOrCreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	c, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 ORDER BY `+latestSessionOrder+` LIMIT 1`, agentID))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChatSession{}, err
	}
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, owner_id, session_key, title) VALUES ($1, `+ownerOfAgent+`, $2, '')
		RETURNING `+sessionCols, agentID, legacySessionKey(agentName)))
}

// CreateSessionOn implements SessionRuntimeStore. The pair is validated here as
// well as by the caller: it is argv to a CLI (a Claude model) or a request
// header (an OpenRouter model), and the store is the last place a bad value
// could be stopped before it is written for good.
func (s *PGStore) CreateSessionOn(ctx context.Context, agentID int64, agentName, runtime, model string) (ChatSession, error) {
	model, err := ValidateSessionRuntime(runtime, model)
	if err != nil {
		return ChatSession{}, err
	}
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, owner_id, session_key, title, runtime, model) VALUES ($1, `+ownerOfAgent+`, $2, '', $3, $4)
		RETURNING `+sessionCols, agentID, newSessionKey(agentName), runtime, model))
}

// DefaultSession implements SessionRuntimeStore.
//
// 🔴 THE ELECTION IS THE MIGRATION'S RULE, IN ONE PLACE PER LAYER: the legacy-key
// session first, then the oldest. "Latest" would be wrong — it is whatever was
// touched last, which for a chief is usually a plugin's session — and a default
// that moved every time a message arrived would not be a default.
//
// ⚠ TWO CALLERS ELECTING AT ONCE: the partial unique index lets one UPDATE win;
// the other matches no row it may set (its WHERE re-checks that the agent still
// has no default) and falls through to the re-read, which returns the winner.
func (s *PGStore) DefaultSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	read := func() (ChatSession, error) {
		return scanSession(s.pool.QueryRow(ctx,
			`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 AND is_default`, agentID))
	}
	c, err := read()
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	if _, err := s.LatestOrCreateSession(ctx, agentID, agentName); err != nil {
		return ChatSession{}, err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE chat_sessions SET is_default = true
		WHERE id = (
			SELECT id FROM chat_sessions
			WHERE agent_id = $1
			ORDER BY (session_key = $2) DESC, created_at, id
			LIMIT 1)
		  AND NOT EXISTS (SELECT 1 FROM chat_sessions WHERE agent_id = $1 AND is_default)`,
		agentID, legacySessionKey(agentName))
	if err != nil && !isUniqueViolation(err) {
		return ChatSession{}, err
	}
	return read()
}

// isUniqueViolation: another election set the agent's default first
// (chat_sessions_one_default).
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "chat_sessions_one_default"
}

// SetDefaultSession implements SessionRuntimeStore.
func (s *PGStore) SetDefaultSession(ctx context.Context, agentID, sessionID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner int64
	if err := tx.QueryRow(ctx, `SELECT agent_id FROM chat_sessions WHERE id = $1`, sessionID).Scan(&owner); err != nil {
		return err
	}
	if owner != agentID {
		return fmt.Errorf("session %d is not agent %d's", sessionID, agentID)
	}
	if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET is_default = false WHERE agent_id = $1 AND is_default AND id <> $2`, agentID, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET is_default = true WHERE id = $1`, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SessionByKey implements SessionRuntimeStore.
func (s *PGStore) SessionByKey(ctx context.Context, agentID int64, sessionKey string) (ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 AND session_key=$2`, agentID, sessionKey))
}

// AddChatMessage inserts a message scoped to m.SessionID, then (in the same
// best-effort follow-up) bumps the session's updated_at and names it from the
// first user message when it's still untitled.
func (s *PGStore) AddChatMessage(ctx context.Context, m ChatMessage) (ChatMessage, error) {
	if m.Kind == "" {
		m.Kind = "text"
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_messages (agent_id, owner_id, session_id, role, content, kind, tool_id, tool_name, tool_ok)
		VALUES ($1, `+ownerOfAgent+`, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, agent_id, session_id, role, content, kind, tool_id, tool_name, tool_ok, created_at`,
		m.AgentID, m.SessionID, m.Role, m.Content, m.Kind, m.ToolID, m.ToolName, m.ToolOK).
		Scan(&m.ID, &m.AgentID, &m.SessionID, &m.Role, &m.Content, &m.Kind, &m.ToolID, &m.ToolName, &m.ToolOK, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	if m.SessionID != 0 {
		// Bump updated_at so the session sorts most-recent-first; set the title from
		// the first user message when it's still blank (COALESCE keeps any existing
		// title, the NULLIF/'' guard only names an as-yet-untitled session).
		var title string
		if m.Role == "user" {
			title = deriveSessionTitle(m.Content)
		}
		_, _ = s.pool.Exec(ctx, `
			UPDATE chat_sessions
			SET updated_at = now(),
			    title = CASE WHEN title = '' AND $2 <> '' THEN $2 ELSE title END
			WHERE id = $1`, m.SessionID, title)
	}
	return m, nil
}

func (s *PGStore) ListChatMessages(ctx context.Context, sessionID int64) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, agent_id, session_id, role, content, kind, tool_id, tool_name, tool_ok, created_at FROM chat_messages
		WHERE session_id=$1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChatMessage, 0)
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.AgentID, &m.SessionID, &m.Role, &m.Content, &m.Kind, &m.ToolID, &m.ToolName, &m.ToolOK, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListRecentChatMessages returns the most recent limit messages for ONE session
// in chronological order. A limit <= 0 falls back to the full session transcript.
//
// 🔴 IT IS ONE CALL TO ListChatMessagesPage RATHER THAN A SECOND COPY OF THE SAME
// QUERY. It used to open-code `ORDER BY id DESC LIMIT $2` + reverse; the paged
// reader needs exactly that plus a cursor predicate, and two spellings of "the
// newest N rows of a session, chronological" is two places for the ordering or the
// reversal to be got wrong — with the disagreement visible only as the page render
// and the machine read describing the same conversation differently.
func (s *PGStore) ListRecentChatMessages(ctx context.Context, sessionID int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		return s.ListChatMessages(ctx, sessionID)
	}
	return s.ListChatMessagesPage(ctx, sessionID, 0, limit)
}

// ListChatMessagesPage returns up to limit messages for ONE session in
// chronological order, ending immediately before beforeID (0 = the newest page).
//
// It selects newest-first with a LIMIT — so a huge transcript only ships the tail,
// and the `(session_id, created_at)` index's sibling primary key does the ordering
// — then reverses to chronological in Go. See the interface comment for why the
// cursor is the id and not a timestamp.
func (s *PGStore) ListChatMessagesPage(ctx context.Context, sessionID, beforeID int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		return []ChatMessage{}, nil
	}
	// 🔴 `$2 <= 0 OR id < $2` RATHER THAN TWO QUERIES. A separate no-cursor
	// statement would be a second place for the column list and the ordering to
	// drift, and the drift would present as the first page of a paged read
	// disagreeing with an unpaged one about what the conversation says.
	rows, err := s.pool.Query(ctx, `
		SELECT id, agent_id, session_id, role, content, kind, tool_id, tool_name, tool_ok, created_at FROM chat_messages
		WHERE session_id=$1 AND ($2 <= 0 OR id < $2) ORDER BY id DESC LIMIT $3`, sessionID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChatMessage, 0, limit)
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.AgentID, &m.SessionID, &m.Role, &m.Content, &m.Kind, &m.ToolID, &m.ToolName, &m.ToolOK, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverseChatChronological(out)
	return out, nil
}

// DeleteChatMessagesOlderThan removes chat messages created before cutoff.
func (s *PGStore) DeleteChatMessagesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM chat_messages WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MarkSessionRead stamps a session's read_at to now(), clearing its unread
// assistant replies.
func (s *PGStore) MarkSessionRead(ctx context.Context, sessionID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET read_at=now() WHERE id=$1`, sessionID)
	return err
}

// UnreadByAgent returns one row per agent that has at least one unread assistant
// message — the total unread count plus the agent's most-recently-active session
// id (the deep-link target). Unread = an assistant message created after its
// session's read_at. Ordered by most-recent unread message first.
//
// The per-agent count sums unread assistant messages across all the agent's
// sessions; the link target is the session with the newest updated_at (the one
// "New chat" / the detail page lands on), resolved via DISTINCT ON.
func (s *PGStore) UnreadByAgent(ctx context.Context) ([]AgentUnread, error) {
	rows, err := s.pool.Query(ctx, `
		WITH unread AS (
			SELECT cs.agent_id,
			       count(*)                AS cnt,
			       max(cm.created_at)      AS last_unread
			FROM chat_messages cm
			JOIN chat_sessions cs ON cs.id = cm.session_id
			WHERE cm.role = 'assistant' AND cm.created_at > cs.read_at
			GROUP BY cs.agent_id
		),
		target AS (
			SELECT DISTINCT ON (agent_id) agent_id, id AS session_id
			FROM chat_sessions
			ORDER BY agent_id, updated_at DESC, id DESC
		)
		SELECT u.agent_id, a.name, a.display_name, t.session_id, u.cnt
		FROM unread u
		JOIN agents a ON a.id = u.agent_id
		JOIN target t ON t.agent_id = u.agent_id
		ORDER BY u.last_unread DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentUnread, 0)
	for rows.Next() {
		var u AgentUnread
		if err := rows.Scan(&u.AgentID, &u.AgentName, &u.DisplayName, &u.SessionID, &u.Count); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func nonEmptyStatus(s string) string {
	if s == "" {
		return StatusPending
	}
	return s
}

var _ Store = (*PGStore)(nil)
