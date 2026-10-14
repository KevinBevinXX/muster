package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Store is the plugin tables (migration 0015). Every method acts as ctx's
// principal, and row-level security scopes it to that principal's rows — except
// Authenticate, which the caller runs under authz.System because it is how the
// owner is LEARNED.
type Store interface {
	// Create mints a plugin for ctx's principal and returns the token ONCE.
	Create(ctx context.Context, name, label, dataScope string) (string, Plugin, error)
	// List is the principal's plugins, newest first.
	List(ctx context.Context) ([]Plugin, error)
	// Revoke revokes one of the principal's plugins, reporting whether it has
	// such a plugin at all. Revoking twice keeps the first time.
	Revoke(ctx context.Context, id int64) (bool, error)
	// Authenticate resolves a token to its plugin and stamps last_seen_at. A
	// revoked or unknown token, and one whose owner is disabled, are ErrBadToken.
	Authenticate(ctx context.Context, token string) (Plugin, error)
	// Submit stores one inbound message for pluginID. created is false when the
	// plugin already sent this external_id: the stored row is returned, unchanged,
	// whatever the new body says — and that answer wins over the rate caps, so a
	// plugin retrying a POST it did not see answered is never refused for it.
	Submit(ctx context.Context, pluginID int64, in MessageIn) (m Message, created bool, err error)
	// Message reads one message.
	Message(ctx context.Context, id int64) (Message, error)
	// Plugin reads one plugin.
	Plugin(ctx context.Context, id int64) (Plugin, error)
	// Queued is a plugin's undelivered messages, oldest first, at most limit.
	Queued(ctx context.Context, pluginID int64, limit int) ([]Message, error)
	// SetChiefSession records the chief chat session a plugin's messages are
	// delivered into.
	SetChiefSession(ctx context.Context, pluginID, agentID, sessionID int64) error
	// RecordAttempt counts an attempt that provably delivered nothing; the
	// message stays queued.
	RecordAttempt(ctx context.Context, messageID int64) error
	// RecordDelivery settles a queued message as delivered or unconfirmed, names
	// the agent it went to, and appends the `plugin.delivered` audit row in the
	// same transaction. It reports false (and writes nothing) when the message was
	// no longer queued.
	RecordDelivery(ctx context.Context, messageID, agentID int64, outcome Status) (bool, error)
	// ReserveGatedTask spends one unit of an agent's gated-task budget (gated.go),
	// or returns ErrGatedTaskCap.
	ReserveGatedTask(ctx context.Context, agentID int64, messageID *int64) error

	// Reply drafts (drafts.go).

	// FileDraft stores an agent's reply draft to a message delivered to it,
	// superseding that message's pending draft (superseded counts them).
	FileDraft(ctx context.Context, agentID, ownerID int64, agentName string, messageID int64, body string) (d Draft, superseded int, err error)
	// Draft reads one draft.
	Draft(ctx context.Context, id int64) (Draft, error)
	// DraftCards is the principal's drafts for /plugins, those awaiting a
	// decision first.
	DraftCards(ctx context.Context, limit int) ([]DraftCard, error)
	// DecideDraft approves or rejects a pending draft.
	DecideDraft(ctx context.Context, id int64, approve bool) (Draft, error)
	// ReapproveDraft approves an unconfirmed draft again: a new decision.
	ReapproveDraft(ctx context.Context, id int64) (Draft, error)
	// ClaimDraft leases one approved, unclaimed draft of pluginID's.
	ClaimDraft(ctx context.Context, pluginID int64) (c Claim, ok bool, err error)
	// FinishDraft records pluginID's result for a draft it claimed.
	FinishDraft(ctx context.Context, pluginID, draftID int64, r DraftResult) (Draft, error)
	// QueuedForOwner is ownerID's undelivered messages across live plugins.
	QueuedForOwner(ctx context.Context, ownerID int64, limit int) ([]Message, error)
	// QueuedCounts is the principal's undelivered messages per plugin.
	QueuedCounts(ctx context.Context) (map[int64]int, error)
}

// PGStore is the Postgres Store.
type PGStore struct {
	pool *pgxpool.Pool
	// denyWords are the deployment's own names a reply draft may not carry
	// (WithAgentDenyWords; drafts.go).
	denyWords []string
}

// NewPG returns the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

const pluginCols = `id, owner_id, name, label, data_scope, created_at, last_seen_at, revoked_at, chief_agent_id, chief_session_id`

func scanPlugin(row pgx.Row) (Plugin, error) {
	var p Plugin
	err := row.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Label, &p.DataScope, &p.CreatedAt, &p.LastSeenAt, &p.RevokedAt,
		&p.ChiefAgentID, &p.ChiefSessionID)
	return p, err
}

const messageCols = `id, owner_id, plugin_id, external_id, sender, subject, body, received_at, created_at, status,
	attempts, last_attempt_at, delivered_at, agent_id, no_reply`

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	var st string
	err := row.Scan(&m.ID, &m.OwnerID, &m.PluginID, &m.ExternalID, &m.Sender, &m.Subject, &m.Body, &m.ReceivedAt,
		&m.CreatedAt, &st, &m.Attempts, &m.LastAttemptAt, &m.DeliveredAt, &m.AgentID, &m.NoReply)
	m.Status = Status(st)
	return m, err
}

// Object is the audit_log `object` naming a plugin.
func Object(id int64) string { return "plugin:" + strconv.FormatInt(id, 10) }

// MessageObject is the audit_log `object` naming a plugin message.
func MessageObject(id int64) string { return "plugin_message:" + strconv.FormatInt(id, 10) }

// audit appends one audit_log row as ctx's principal (0009's self_append).
func audit(ctx context.Context, tx pgx.Tx, action, object string, detail map[string]any) error {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return errors.New("plugins: an audited write needs a user principal")
	}
	return users.AuditTx(ctx, tx, users.Actor{UserID: p.UserID, Via: p.Via}, action, object, detail)
}

func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PGStore) Create(ctx context.Context, name, label, dataScope string) (string, Plugin, error) {
	label = strings.TrimSpace(label)
	if err := ValidName(name); err != nil {
		return "", Plugin{}, err
	}
	if err := ValidLabel(label); err != nil {
		return "", Plugin{}, err
	}
	if err := ValidScope(dataScope); err != nil {
		return "", Plugin{}, err
	}
	tok, err := NewToken()
	if err != nil {
		return "", Plugin{}, err
	}
	var p Plugin
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPlugin(tx.QueryRow(ctx, `
			INSERT INTO plugins (name, label, token_hash, data_scope) VALUES ($1, $2, $3, $4) RETURNING `+pluginCols,
			name, label, HashToken(tok), dataScope))
		if err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.created", Object(p.ID), map[string]any{"name": name, "data_scope": dataScope})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "plugins_owner_name_live" {
		return "", Plugin{}, invalid("you already have a plugin named %q; revoke it first", name)
	}
	if err != nil {
		return "", Plugin{}, err
	}
	return tok, p, nil
}

func (s *PGStore) List(ctx context.Context) ([]Plugin, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pluginCols+` FROM plugins ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plugin{}
	for rows.Next() {
		p, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PGStore) Plugin(ctx context.Context, id int64) (Plugin, error) {
	p, err := scanPlugin(s.pool.QueryRow(ctx, `SELECT `+pluginCols+` FROM plugins WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrNotFound
	}
	return p, err
}

func (s *PGStore) Revoke(ctx context.Context, id int64) (bool, error) {
	found := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM plugins WHERE id = $1 FOR UPDATE`, id).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if revoked != nil {
			return nil // revoking twice keeps the first time, and audits once
		}
		if _, err := tx.Exec(ctx, `UPDATE plugins SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.revoked", Object(id), nil)
	})
	return found, err
}

func (s *PGStore) Authenticate(ctx context.Context, token string) (Plugin, error) {
	if !TokenShapeOK(token) {
		return Plugin{}, ErrBadToken
	}
	p, err := scanPlugin(s.pool.QueryRow(ctx, `
		UPDATE plugins p SET last_seen_at = now()
		  FROM users u
		 WHERE p.token_hash = $1 AND p.revoked_at IS NULL
		   AND u.id = p.owner_id AND u.disabled_at IS NULL
		RETURNING `+prefixed("p", pluginCols), HashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrBadToken
	}
	return p, err
}

func (s *PGStore) Submit(ctx context.Context, pluginID int64, in MessageIn) (Message, bool, error) {
	if err := in.Validate(); err != nil {
		return Message{}, false, err
	}
	var m Message
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE PLUGIN ROW IS LOCKED FIRST, so the caps below are checked and the
		// insert made by one transaction at a time per plugin: two concurrent POSTs
		// cannot both see 59 and both land the 60th-and-61st.
		var live bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL FROM plugins WHERE id = $1 FOR UPDATE`, pluginID).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m, err = scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages
			WHERE plugin_id = $1 AND external_id = $2`, pluginID, in.ExternalID))
		if err == nil {
			return nil // a repeat: the stored row is the answer
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var lastHour, queued int
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE created_at > now() - make_interval(secs => $2)),
				count(*) FILTER (WHERE status = 'queued')
			  FROM plugin_messages WHERE plugin_id = $1`, pluginID, rateWindow.Seconds()).Scan(&lastHour, &queued); err != nil {
			return err
		}
		switch {
		case lastHour >= MaxPerHour:
			return ErrRateLimited
		case queued >= MaxQueued:
			return ErrQueueFull
		}
		m, err = scanMessage(tx.QueryRow(ctx, `
			INSERT INTO plugin_messages (plugin_id, external_id, sender, subject, body, received_at, no_reply)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+messageCols,
			pluginID, in.ExternalID, in.Sender, in.Subject, in.Body, in.ReceivedAt, in.NoReply))
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Message{}, false, err
	}
	return m, created, nil
}

func (s *PGStore) Message(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

func (s *PGStore) Queued(ctx context.Context, pluginID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+messageCols+` FROM plugin_messages
		WHERE plugin_id = $1 AND status = 'queued' ORDER BY id LIMIT $2`, pluginID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PGStore) SetChiefSession(ctx context.Context, pluginID, agentID, sessionID int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE plugins SET chief_agent_id = $2, chief_session_id = $3 WHERE id = $1`,
		pluginID, agentID, sessionID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) RecordAttempt(ctx context.Context, messageID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE plugin_messages SET attempts = attempts + 1, last_attempt_at = now()
		WHERE id = $1 AND status = 'queued'`, messageID)
	return err
}

func (s *PGStore) RecordDelivery(ctx context.Context, messageID, agentID int64, outcome Status) (bool, error) {
	if outcome != StatusDelivered && outcome != StatusUnconfirmed {
		return false, invalid("a delivery settles as delivered or unconfirmed, not %q", outcome)
	}
	settled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var pluginID int64
		err := tx.QueryRow(ctx, `
			UPDATE plugin_messages SET status = $3, delivered_at = now(), agent_id = $2,
			       attempts = attempts + 1, last_attempt_at = now()
			 WHERE id = $1 AND status = 'queued'
			RETURNING plugin_id`, messageID, agentID, string(outcome)).Scan(&pluginID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		settled = true
		return audit(ctx, tx, "plugin.delivered", MessageObject(messageID), map[string]any{
			"plugin_id": pluginID, "agent_id": agentID, "outcome": string(outcome),
		})
	})
	return settled, err
}

// prefixed qualifies a column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
