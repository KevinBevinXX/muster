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
// 🔴 EVERY METHOD BUT Owners AND PruneAll RUNS UNDER THE CALLER'S PRINCIPAL.
// agent_focus is under FORCE row-level security, so a caller reads and writes
// only its own user's rows, and a row's owner is the principal's user
// (owner_id's default), never a parameter. The refresher acts as each owner in
// turn (internal/api's focusPrincipal); only Owners — which owners have
// anything to refresh — and PruneAll — the retention sweep, which deletes by
// age alone and returns a count — reach across users, under the system scope.
type Store interface {
	Get(ctx context.Context, s Subject) (Focus, bool, error)
	List(ctx context.Context) ([]Focus, error)
	// Search: live is the caller's mirrored sessions visible now; a mirrored
	// row outside it is never a hit.
	Search(ctx context.Context, query string, live []string, limit int) ([]Hit, error)
	Upsert(ctx context.Context, s Subject, w Write) (Focus, error)
	RecordError(ctx context.Context, s Subject, msg string) error
	// Touch marks the caller's mirrored subjects seen now; PruneAll deletes
	// EVERY owner's mirrored rows last seen before before (the retention sweep).
	Touch(ctx context.Context, subs []Subject) error
	PruneAll(ctx context.Context, before time.Time) (int, error)
	Owners(ctx context.Context) ([]int64, error)
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

// Upsert stores w as the caller's focus for sub, clearing any recorded error.
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
	f, err := scan(s.pool.QueryRow(ctx, `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, title, description,
			source, fingerprint, updated_at, last_error, error_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), '', NULL)
		ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET seen_at = now(), title = EXCLUDED.title,
			description = EXCLUDED.description, source = EXCLUDED.source, fingerprint = EXCLUDED.fingerprint,
			updated_at = now(), last_error = '', error_at = NULL
		RETURNING `+cols, string(sub.Kind), sub.Key, agentArg(sub), title, desc, string(w.Source), w.Fingerprint))
	if err != nil {
		return Focus{}, errf("upsert: %w", err)
	}
	return f, nil
}

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
