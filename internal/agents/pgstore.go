package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// PGStore is the Postgres-backed agents Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs a Postgres-backed agents Store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const agentCols = `id, name, namespace, display_name, repo, repo_branch, note_id, note_text,
	pending_note, model, status, hooks_token, kicked_off, last_output, error_message, created_at, updated_at,
	kickoff_pod, kickoff_restarts, kickoff_attempts, kickoff_error, kind, cc_account`

func scanAgent(row interface {
	Scan(dest ...any) error
}) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.Name, &a.Namespace, &a.DisplayName, &a.Repo, &a.RepoBranch,
		&a.NoteID, &a.NoteText, &a.PendingNote, &a.Model, &a.Status, &a.HooksToken, &a.KickedOff,
		&a.LastOutput, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt,
		&a.KickoffPod, &a.KickoffRestarts, &a.KickoffAttempts, &a.KickoffError, &a.Kind, &a.CCAccount)
	return a, err
}

// Create inserts an agent and returns it with generated id/timestamps.
func (s *PGStore) Create(ctx context.Context, a Agent) (Agent, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO agents (name, namespace, display_name, repo, repo_branch, note_id, note_text, pending_note, model, status, hooks_token, kind, cc_account)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+agentCols,
		a.Name, a.Namespace, a.DisplayName, a.Repo, a.RepoBranch, a.NoteID, a.NoteText,
		a.PendingNote, a.Model, nonEmptyStatus(a.Status), a.HooksToken, ResolveKind(a.Kind), a.CCAccount)
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
// inheriting the per-instance namespace, ServiceAccount and cluster-scoped
// ClusterRoleBinding that were NAMED after the dead one. See
// internal/provision/k8s/driver.go's Destroy on why those objects outlive the
// row, and migration 0002 for the trigger that fills the ledger this reads.
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
func (s *PGStore) NameExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM agents WHERE name=$1)
		    OR EXISTS(SELECT 1 FROM agent_retired_names WHERE name=$1)`, name).Scan(&exists)
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

func (s *PGStore) SetKickedOff(ctx context.Context, id int64, v bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET kicked_off=$2, updated_at=now() WHERE id=$1`, id, v)
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

const sessionCols = `id, agent_id, session_key, title, created_at, updated_at, read_at`

func scanSession(row interface {
	Scan(dest ...any) error
}) (ChatSession, error) {
	var c ChatSession
	err := row.Scan(&c.ID, &c.AgentID, &c.SessionKey, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ReadAt)
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
		INSERT INTO chat_sessions (agent_id, session_key, title) VALUES ($1,$2,'')
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
	// The LATERAL picks the NEWEST matching body row per session and cuts the
	// excerpt in SQL (see SearchSnippetRadius). `m.snippet IS NOT NULL` in the
	// WHERE is what makes the join an OR over title-or-body rather than a filter.
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.agent_id, s.session_key, s.title, s.created_at, s.updated_at, s.read_at,
		       COALESCE(m.snippet, '') AS snippet
		FROM chat_sessions s
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
		) m ON TRUE
		WHERE s.agent_id = $1
		  AND (s.title ILIKE $3 ESCAPE '\' OR m.snippet IS NOT NULL)
		ORDER BY s.updated_at DESC, s.id DESC
		LIMIT $6`,
		agentID, q, pattern, SearchSnippetRadius, SearchSnippetLen, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionMatch, 0)
	for rows.Next() {
		var m SessionMatch
		if err := rows.Scan(&m.ID, &m.AgentID, &m.SessionKey, &m.Title,
			&m.CreatedAt, &m.UpdatedAt, &m.ReadAt, &m.Snippet); err != nil {
			return nil, err
		}
		m.Snippet = strings.Join(strings.Fields(m.Snippet), " ")
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PGStore) GetSession(ctx context.Context, sessionID int64) (ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM chat_sessions WHERE id=$1`, sessionID))
}

// LatestOrCreateSession returns the agent's most-recently-updated session, or
// creates one. The created session uses the LEGACY key (so a pre-sessions agent
// keeps its gateway context) — the legacy key is UNIQUE, so a second call after
// one exists takes the latest branch and never collides.
func (s *PGStore) LatestOrCreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	c, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 ORDER BY updated_at DESC, id DESC LIMIT 1`, agentID))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChatSession{}, err
	}
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, session_key, title) VALUES ($1,$2,'')
		RETURNING `+sessionCols, agentID, legacySessionKey(agentName)))
}

// AddChatMessage inserts a message scoped to m.SessionID, then (in the same
// best-effort follow-up) bumps the session's updated_at and names it from the
// first user message when it's still untitled.
func (s *PGStore) AddChatMessage(ctx context.Context, m ChatMessage) (ChatMessage, error) {
	if m.Kind == "" {
		m.Kind = "text"
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_messages (agent_id, session_id, role, content, kind, tool_id, tool_name, tool_ok)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
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
