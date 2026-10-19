package focus

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Store is focus persistence.
//
// 🔴 EVERY METHOD BUT Owners, PruneAll AND PruneHistory RUNS UNDER THE CALLER'S PRINCIPAL.
// agent_focus is under FORCE row-level security, so a caller reads and writes
// only its own user's rows, and a row's owner is the principal's user
// (owner_id's default), never a parameter. The refresher acts as each owner in
// turn (internal/api's focusPrincipal); only Owners — which owners have
// anything to refresh — and PruneAll and PruneHistory — the retention sweep,
// which deletes by age alone and returns a count — reach across users, under
// the system scope.
type Store interface {
	Get(ctx context.Context, s Subject) (Focus, bool, error)
	List(ctx context.Context) ([]Focus, error)
	// Search: live is the caller's mirrored sessions visible now; a mirrored
	// row outside it is never a hit.
	Search(ctx context.Context, query string, live []string, limit int) ([]Hit, error)
	// Upsert writes the focus and, in the same transaction, appends it to the
	// subject's history when its title or description differs from the newest
	// history row (migration 0022).
	Upsert(ctx context.Context, s Subject, w Write) (Focus, error)
	RecordError(ctx context.Context, s Subject, msg string) error
	// Touch marks the caller's mirrored subjects seen now; PruneAll deletes
	// EVERY owner's mirrored rows last seen before before (the retention sweep).
	Touch(ctx context.Context, subs []Subject) error
	PruneAll(ctx context.Context, before time.Time) (int, error)
	Owners(ctx context.Context) ([]int64, error)
	// History is one page of the caller's focus history for s, newest first,
	// starting after the cursor ("" = the newest).
	History(ctx context.Context, s Subject, cursor string, limit int) (HistoryPage, error)
	// PruneHistory deletes EVERY owner's history rows written before before
	// (the retention sweep, under the system scope).
	PruneHistory(ctx context.Context, before time.Time) (int, error)
	// SearchHistory, MatchHistory and RecentTitles read the caller's history
	// written at or after since, for live sessions only (history_search.go).
	SearchHistory(ctx context.Context, query string, live Live, since time.Time, limit int) ([]HistoryHit, error)
	MatchHistory(ctx context.Context, text string, live Live, since time.Time, minTerms, limit int) ([]Match, error)
	RecentTitles(ctx context.Context, live Live, since time.Time) (map[Subject][]TitleAt, error)
}

// Write is one focus to store. Title and Description go through Clean.
type Write struct {
	Title, Description string
	Source             Source
	// Fingerprint is the tail it was computed from ("" for a self or manual write).
	Fingerprint string
}

// MaxSearchResults bounds focus_search.
const MaxSearchResults = 20

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

const cols = `subject_kind, subject_key, owner_id, title, description, source, fingerprint, updated_at,
	last_error, coalesce(error_at, 'epoch'::timestamptz)`

func scan(row pgx.Row) (Focus, error) {
	var f Focus
	var kind, src string
	err := row.Scan(&kind, &f.Subject.Key, &f.Owner, &f.Title, &f.Description, &src, &f.Fingerprint, &f.UpdatedAt,
		&f.LastError, &f.ErrorAt)
	f.Subject.Kind, f.Source = Kind(kind), Source(src)
	if f.ErrorAt.Unix() == 0 {
		f.ErrorAt = time.Time{}
	}
	return f, err
}

// agentArg is the agent_id column for s: its id for an agent, NULL otherwise.
func agentArg(s Subject) any {
	if s.Kind == KindAgent {
		return s.AgentID()
	}
	return nil
}

// Get is the caller's focus for s.
func (s *PGStore) Get(ctx context.Context, sub Subject) (Focus, bool, error) {
	if err := sub.Valid(); err != nil {
		return Focus{}, false, err
	}
	f, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM agent_focus WHERE subject_kind = $1 AND subject_key = $2`,
		string(sub.Kind), sub.Key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Focus{}, false, nil
	}
	if err != nil {
		return Focus{}, false, errf("get: %w", err)
	}
	return f, true, nil
}

// List is every focus the caller has, newest first.
func (s *PGStore) List(ctx context.Context) ([]Focus, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM agent_focus ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, errf("list: %w", err)
	}
	defer rows.Close()
	var out []Focus
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, errf("list: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Search is the caller's focuses matching query, best first (ts_rank over the
// generated tsvector: title weighs A, description B), at most limit (bounded by
// MaxSearchResults). A query with no searchable word matches nothing.
//
// 🔴 A MIRRORED ROW MATCHES ONLY WHILE ITS SESSION IS LIVE (in live), AND THAT
// IS DECIDED IN SQL, BEFORE THE LIMIT. Filtering after it let dead sessions
// that outrank a live one fill every slot, and the search came back empty.
func (s *PGStore) Search(ctx context.Context, query string, live []string, limit int) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Hit{}, nil
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	rows, err := s.pool.Query(ctx, `SELECT subject_kind, subject_key, title, description, ts_rank(search, q) AS rank
		FROM agent_focus, websearch_to_tsquery('english', $1) q
		WHERE title <> '' AND search @@ q AND (subject_kind <> 'mirror' OR subject_key = ANY($3::text[]))
		ORDER BY rank DESC, updated_at DESC, id DESC LIMIT $2`, query, limit, live)
	if err != nil {
		return nil, errf("search: %w", err)
	}
	defer rows.Close()
	out := []Hit{}
	for rows.Next() {
		var h Hit
		var kind, desc string
		if err := rows.Scan(&kind, &h.Subject.Key, &h.Title, &desc, &h.Rank); err != nil {
			return nil, errf("search: %w", err)
		}
		h.Subject.Kind, h.Snippet = Kind(kind), Snippet(desc)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Upsert stores w as the caller's focus for sub, clearing any recorded error,
// and appends it to sub's history when it changed (appendHistorySQL) — in ONE
// transaction, so a focus is never stored without its history row or the
// reverse.
func (s *PGStore) Upsert(ctx context.Context, sub Subject, w Write) (Focus, error) {
	if err := sub.Valid(); err != nil {
		return Focus{}, err
	}
	title, desc, err := Clean(w.Title, w.Description)
	if err != nil {
		return Focus{}, err
	}
	if w.Source != SourceSummarizer {
		return Focus{}, errf("unknown source %q", w.Source)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Focus{}, errf("upsert: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	f, err := scan(tx.QueryRow(ctx, upsertFocusSQL, string(sub.Kind), sub.Key, agentArg(sub), title, desc, string(w.Source), w.Fingerprint))
	if err != nil {
		return Focus{}, errf("upsert: %w", err)
	}
	if _, err := tx.Exec(ctx, appendHistorySQL, string(sub.Kind), sub.Key, agentArg(sub), f.Title, f.Description, f.Owner); err != nil {
		return Focus{}, errf("upsert: history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Focus{}, errf("upsert: commit: %w", err)
	}
	return f, nil
}

// upsertFocusSQL writes the caller's focus for one subject, clearing any
// recorded error: ($1 kind, $2 key, $3 agent id or NULL, $4 title, $5
// description, $6 source, $7 fingerprint).
const upsertFocusSQL = `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, title, description,
		source, fingerprint, updated_at, last_error, error_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, now(), '', NULL)
	ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET seen_at = now(), title = EXCLUDED.title,
		description = EXCLUDED.description, source = EXCLUDED.source, fingerprint = EXCLUDED.fingerprint,
		updated_at = now(), last_error = '', error_at = NULL
	RETURNING ` + cols

// appendHistorySQL appends a focus to its subject's history unless the
// subject's NEWEST history row already says exactly that (title and
// description): an identical refresh writes nothing.
//
// 🔴 IT RUNS AFTER THE UPSERT, IN ITS TRANSACTION, so the agent_focus row lock
// the upsert took serialises two writers of one subject: the second one's
// statement (a fresh READ COMMITTED snapshot) sees the first one's row. And
// created_at is clock_timestamp(), not now(): now() is when the TRANSACTION
// began, before that lock wait, so a writer that waited would stamp its row
// EARLIER than the row it waited for and "newest" would name the wrong one.
const appendHistorySQL = `INSERT INTO agent_focus_history (subject_kind, subject_key, agent_id, title, description, created_at)
	SELECT $1, $2, $3, $4, $5, clock_timestamp()
	WHERE NOT EXISTS (
		SELECT 1 FROM (SELECT title, description FROM agent_focus_history
			WHERE owner_id = $6 AND subject_kind = $1 AND subject_key = $2
			ORDER BY created_at DESC, id DESC LIMIT 1) newest
		WHERE newest.title = $4 AND newest.description = $5)`

// RecordError records a failed refresh for sub — msg should be a Reason —
// leaving the focus it holds.
func (s *PGStore) RecordError(ctx context.Context, sub Subject, msg string) error {
	if err := sub.Valid(); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, last_error, error_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET last_error = EXCLUDED.last_error, error_at = now(),
			seen_at = now()`,
		string(sub.Kind), sub.Key, agentArg(sub), clampError(msg)); err != nil {
		return errf("record error: %w", err)
	}
	return nil
}

// Touch marks the caller's mirrored subjects seen now.
func (s *PGStore) Touch(ctx context.Context, subs []Subject) error {
	var keys []string
	for _, sub := range subs {
		if sub.Kind == KindMirror && sub.Valid() == nil {
			keys = append(keys, sub.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_focus SET seen_at = now()
		WHERE subject_kind = 'mirror' AND subject_key = ANY($1)`, keys); err != nil {
		return errf("touch: %w", err)
	}
	return nil
}

// PruneAll deletes EVERY owner's MIRRORED rows last seen before before, and
// reports how many: the retention sweep (Retainer), under the system scope, so
// a disabled owner's rows — or any owner no pass reaches — age out too. An
// agent's row is never pruned: it dies with its agent.
func (s *PGStore) PruneAll(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(sys(ctx), `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND seen_at < $1`, before)
	if err != nil {
		return 0, errf("prune: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PruneHistory deletes EVERY owner's focus history rows written before before,
// and reports how many: the retention sweep (Retainer), under the system scope,
// as PruneAll is.
//
// 🔴 EXCEPT A SUBJECT'S CURRENT FOCUS. A focus unchanged for longer than the
// retention has exactly one history row — itself — and deleting it would show
// "No focus history yet" beside a card headed by that very focus. So a
// subject's NEWEST row is kept while the subject still HAS a focus row. Once
// the focus row is gone (a mirrored session pruned, unseen for a week) the
// newest row ages out like the rest: keeping it then would keep every dead
// session's last focus forever.
func (s *PGStore) PruneHistory(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(sys(ctx), `DELETE FROM agent_focus_history h WHERE h.created_at < $1
		AND (EXISTS (SELECT 1 FROM agent_focus_history n
				WHERE n.owner_id = h.owner_id AND n.subject_kind = h.subject_kind AND n.subject_key = h.subject_key
				AND (n.created_at, n.id) > (h.created_at, h.id))
			OR NOT EXISTS (SELECT 1 FROM agent_focus f
				WHERE f.owner_id = h.owner_id AND f.subject_kind = h.subject_kind AND f.subject_key = h.subject_key
				AND f.title <> ''))`, before)
	if err != nil {
		return 0, errf("prune history: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// History is one page of the caller's history for sub, newest first: at most
// limit rows (bounded by MaxHistoryPage; <= 0 is DefaultHistoryPage) after
// cursor ("" = from the newest), and the cursor of the next page ("" = this
// is the oldest page). Another owner's subject reads as no rows (row-level
// security); telling "none" from "not yours" is the caller's job.
func (s *PGStore) History(ctx context.Context, sub Subject, cursor string, limit int) (HistoryPage, error) {
	if err := sub.Valid(); err != nil {
		return HistoryPage{}, err
	}
	c, err := parseHistoryCursor(cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	if limit <= 0 {
		limit = DefaultHistoryPage
	}
	if limit > MaxHistoryPage {
		limit = MaxHistoryPage
	}
	// One more than the page, to know whether there is a next one.
	rows, err := s.pool.Query(ctx, `SELECT id, title, description, created_at FROM agent_focus_history
		WHERE subject_kind = $1 AND subject_key = $2 AND (NOT $3 OR (created_at, id) < ($4, $5))
		ORDER BY created_at DESC, id DESC LIMIT $6`,
		string(sub.Kind), sub.Key, c.set, c.at, c.id, limit+1)
	if err != nil {
		return HistoryPage{}, errf("history: %w", err)
	}
	defer rows.Close()
	page := HistoryPage{Entries: []HistoryEntry{}}
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Description, &e.CreatedAt); err != nil {
			return HistoryPage{}, errf("history: %w", err)
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, errf("history: %w", err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.Next = historyCursorOf(page.Entries[limit-1])
	}
	return page, nil
}

// Owners is every ENABLED user with something focus could describe: an agent,
// or a live mirror token. Under the system scope (internal/authz's ledger names
// this one helper): it yields only owner ids, and the refresher then acts as
// each of them.
func (s *PGStore) Owners(ctx context.Context) ([]int64, error) {
	// A DISABLED owner is not refreshed: their sessions would spend their key
	// on an account that cannot sign in to read the result.
	rows, err := s.pool.Query(sys(ctx), `SELECT o FROM (SELECT owner_id AS o FROM agents
			UNION SELECT owner_id FROM mirror_tokens WHERE revoked_at IS NULL) x
		JOIN users u ON u.id = x.o WHERE u.disabled_at IS NULL ORDER BY 1`)
	if err != nil {
		return nil, errf("owners: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// sys is ctx under the system principal: Owners and PruneAll only.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }
