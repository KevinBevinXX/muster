package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/users"
)

// PGStore is the Postgres-backed credential store (table user_credentials,
// migration 0011).
//
// 🔴 EVERY READ AND WRITE NAMES ITS OWNER EXPLICITLY (`owner_id = $n`), AS THE
// CONNECTOR STORE DOES. Row-level security already scopes a user principal to
// its own rows; the explicit predicate is what keeps a scope-all caller (the
// pod-spec path may run under authz.System) from ever resolving "a" credential
// across users. Writes take the owner from the acting principal and never from
// an argument: a member can only ever write their own.
type PGStore struct {
	pool *pgxpool.Pool
	seal sealer
}

// NewPG builds the store. key is the connector store's 32-byte key; this store
// derives its own from it (seal.go).
func NewPG(pool *pgxpool.Pool, key []byte) (*PGStore, error) {
	s, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	return &PGStore{pool: pool, seal: s}, nil
}

// errNoOwner: writes need a user principal (never System, never none).
var errNoOwner = errors.New("credentials: a credential belongs to a signed-in user; this request has none")

// writer is the acting user, for writes.
func writer(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return 0, errNoOwner
	}
	return p.UserID, nil
}

// List returns the acting user's credentials, Claude accounts by name then
// the OpenRouter key. No secret leaves this method.
func (s *PGStore) List(ctx context.Context) ([]Summary, error) {
	owner, err := writer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, name, fingerprint, created_at, updated_at FROM user_credentials
		WHERE owner_id = $1 ORDER BY kind, name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var c Summary
		if err := rows.Scan(&c.Kind, &c.Name, &c.Fingerprint, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutClaude adds (or replaces) the acting user's Claude account `name`.
func (s *PGStore) PutClaude(ctx context.Context, name, token string) error {
	owner, err := writer(ctx)
	if err != nil {
		return err
	}
	if err := ValidateAccountName(name); err != nil {
		return err
	}
	if err := ValidateClaudeToken(token); err != nil {
		return err
	}
	ct, err := s.seal.seal(binding{Owner: owner, Kind: KindClaude, Name: name}, token)
	if err != nil {
		return err
	}
	return s.upsert(ctx, owner, KindClaude, name, ct, ccpool.Fingerprint(token))
}

// Delete removes the acting user's credential (kind, name). Reports whether a
// row existed.
func (s *PGStore) Delete(ctx context.Context, kind, name string) (bool, error) {
	owner, err := writer(ctx)
	if err != nil {
		return false, err
	}
	if kind == KindOpenRouter {
		name = OpenRouterName
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var fp string
	err = tx.QueryRow(ctx, `DELETE FROM user_credentials WHERE owner_id = $1 AND kind = $2 AND name = $3
		RETURNING fingerprint`, owner, kind, name).Scan(&fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := audit(ctx, tx, owner, ActionDelete, kind, name, fp); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// PutOpenRouter stores (or replaces) the acting user's OpenRouter API key —
// an ORDINARY key, used as entered by their gateway agents.
//
// ⚠ AN AGENT ALREADY RUNNING KEEPS THE KEY ITS POD WAS GIVEN: Stop then Start
// the agent to re-apply its spec with the new key (agentprovision re-resolves
// on every build). Revoking the old key is the member's to do at OpenRouter.
func (s *PGStore) PutOpenRouter(ctx context.Context, key string) error {
	owner, err := writer(ctx)
	if err != nil {
		return err
	}
	if err := ValidateOpenRouterKey(key); err != nil {
		return err
	}
	ct, err := s.seal.seal(binding{Owner: owner, Kind: KindOpenRouter, Name: OpenRouterName}, key)
	if err != nil {
		return err
	}
	return s.upsert(ctx, owner, KindOpenRouter, OpenRouterName, ct, ccpool.Fingerprint(key))
}

// The audit_log actions a credential write records.
const (
	ActionSave   = "credential.save"
	ActionDelete = "credential.delete"
)

// upsert writes owner's sealed credential and its audit_log row in ONE
// transaction: a save that is not audited does not happen.
func (s *PGStore) upsert(ctx context.Context, owner int64, kind, name string, ct []byte, fingerprint string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_id, kind, name) DO UPDATE SET secret_ciphertext = EXCLUDED.secret_ciphertext,
			fingerprint = EXCLUDED.fingerprint, updated_at = now()`,
		owner, kind, name, ct, fingerprint); err != nil {
		return err
	}
	if err := audit(ctx, tx, owner, ActionSave, kind, name, fingerprint); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// audit appends one audit_log row for a credential write by owner (round-1
// audit on #69: saves and deletes were unrecorded), through users.AuditTx —
// the account packages' one audit writer — inside the write's transaction.
//
// 🔴 IT IS HANDED NO SECRET AND CANNOT WRITE ONE: its parameters are the kind,
// the name and the 48-bit fingerprint — the same three things the page lists.
// TestACredentialWriteIsAuditedWithoutItsSecret reads the rows back.
//
// The row is the actor's own (audit_log's self_append policy, 0009): user_id
// is the writer, via is how they were admitted.
func audit(ctx context.Context, tx pgx.Tx, owner int64, action, kind, name, fingerprint string) error {
	by := users.Actor{UserID: owner}
	if p, ok := authz.From(ctx); ok {
		by.Via = p.Via
	}
	return users.AuditTx(ctx, tx, by, action, kind+":"+name,
		map[string]any{"kind": kind, "name": name, "fingerprint": fingerprint})
}

// ClaudeTokens returns owner's Claude accounts, name → setup-token. An empty
// map (never an error) means the owner has none.
//
// 🔴 ONE OF THE TWO METHODS THAT OPEN A SECRET. Its caller is [Plane], whose
// callers are pod-spec building and account selection — never a page.
func (s *PGStore) ClaudeTokens(ctx context.Context, owner int64) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, secret_ciphertext FROM user_credentials
		WHERE owner_id = $1 AND kind = 'claude'`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name string
		var ct []byte
		if err := rows.Scan(&name, &ct); err != nil {
			return nil, err
		}
		tok, err := s.seal.open(binding{Owner: owner, Kind: KindClaude, Name: name}, ct)
		if err != nil {
			return nil, fmt.Errorf("credentials: Claude account %q of user %d: %w", name, owner, err)
		}
		out[name] = tok
	}
	return out, rows.Err()
}

// OpenRouterKey returns owner's OpenRouter API key — the one their gateway
// agents are given — or ok=false when they have none.
func (s *PGStore) OpenRouterKey(ctx context.Context, owner int64) (string, bool, error) {
	var ct []byte
	err := s.pool.QueryRow(ctx, `SELECT secret_ciphertext FROM user_credentials
		WHERE owner_id = $1 AND kind = 'openrouter' AND name = 'default'`, owner).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	key, err := s.seal.open(binding{Owner: owner, Kind: KindOpenRouter, Name: OpenRouterName}, ct)
	if err != nil {
		return "", false, fmt.Errorf("credentials: OpenRouter key of user %d: %w", owner, err)
	}
	return key, true, nil
}

// HasOpenRouterKey reports whether owner has an OpenRouter key, opening nothing.
func (s *PGStore) HasOpenRouterKey(ctx context.Context, owner int64) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_credentials
		WHERE owner_id = $1 AND kind = 'openrouter'`, owner).Scan(&n)
	return n > 0, err
}

// ClaudeNames returns owner's Claude account names, opening nothing.
func (s *PGStore) ClaudeNames(ctx context.Context, owner int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM user_credentials WHERE owner_id = $1 AND kind = 'claude' ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
