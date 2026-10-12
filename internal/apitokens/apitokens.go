// Package apitokens is muster's PERSONAL API TOKEN store (accounts PR 6): the
// `api_tokens` table from migration 0006, under the row-level security 0009
// put on it (a user reads and writes only rows whose user_id is its own).
//
// A personal token is `mst_` + 43 base64url characters (32 random bytes). Only
// its sha256 is stored; the token itself is returned ONCE, by Create, and
// nothing can read it back. It authenticates the machine tier (internal/api's
// requireHookToken family) AS ITS OWNER — that user's id and role — which is
// what lets a person run the CLI, or a script, against the LAN machine port
// without the shared hook secret that acts as the machine user.
//
// 🔴 THE MANAGEMENT METHODS (Create, List, Revoke) RUN AS THE CALLER'S OWN
// PRINCIPAL, AND THE DATABASE IS WHAT KEEPS THEM TO THE CALLER'S ROWS. Each
// also names the user in its WHERE clause, but that is a second wall, not the
// first: TestRowLevelSecurityKeepsTokensToTheirOwner reads the table with raw
// SQL as another user (and as an admin) and must see nothing.
//
// 🔴 Authenticate IS THE ONE LOOKUP THAT CANNOT RUN AS ITS OWNER, because it is
// how the owner is LEARNED. Its caller hands it a System context
// (internal/api.(*Server).apiTokenUser, a System-ledger entry) for that one
// call; nothing it reads is handed on except the owner's id.
//
// ⚠ NO CACHE, ON PURPOSE. Every authentication reads the row and the owner's
// users.disabled_at, so a revoke or a disable takes effect on the very next
// request. The cost is one indexed lookup per request (token_hash is UNIQUE),
// which is the price of "revocation is immediate".
package apitokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Prefix starts every personal token. It is how the machine tier tells a
// personal token from the shared hook secret before any database read, and how
// a leaked token is recognisable in a log or a paste.
const Prefix = "mst_"

// encodedLen is the base64url (no padding) length of 32 bytes.
const encodedLen = 43

// MaxNameLen bounds a token's label, in characters.
const MaxNameLen = 100

// LastUsedGranularity is how stale last_used_at may get before an
// authentication rewrites it. A token used in a tight loop would otherwise
// cost a row write per request; within this window the column is not touched.
const LastUsedGranularity = time.Minute

var (
	// ErrBadToken: the value is not a live token — malformed, unknown, or
	// revoked. ONE error for all three, so nothing about a refusal says which.
	ErrBadToken = errors.New("apitokens: invalid or revoked token")
	// ErrUserDisabled: the token is live but its owner is disabled.
	ErrUserDisabled = errors.New("apitokens: the token's user is disabled")
	// ErrNoUser: a management call with no user principal on its context (or
	// with the System principal, which is nobody's).
	ErrNoUser = errors.New("apitokens: no user principal on this context")
	// ErrNameTooLong: the label is longer than MaxNameLen characters.
	ErrNameTooLong = fmt.Errorf("apitokens: a token name is at most %d characters", MaxNameLen)
)

// Token is one token's record. It never carries the token or its hash.
type Token struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"userId"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

// Revoked reports whether the token is revoked.
func (t Token) Revoked() bool { return t.RevokedAt != nil }

// New mints a fresh token: Prefix + 43 base64url characters (256 bits).
func New() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("apitokens: mint a token: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// LooksLike reports whether s carries the personal-token prefix. It decides
// which DOOR a credential is for, not whether it is valid: a value with the
// prefix is a personal token or nothing (see ShapeOK).
func LooksLike(s string) bool { return strings.HasPrefix(s, Prefix) }

// ShapeOK reports whether token is shaped like one New mints. It is a FILTER,
// not authentication: it lets the machine tier refuse junk without a database
// read. The hash lookup is the security property.
func ShapeOK(token string) bool {
	rest, ok := strings.CutPrefix(token, Prefix)
	if !ok || len(rest) != encodedLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// Hash is the stored form of a token: its sha256 (api_tokens.token_hash is a
// 32-byte BYTEA).
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Store is the personal-token store.
type Store interface {
	// Authenticate resolves token to its record. ErrBadToken for a malformed,
	// unknown or revoked token (a malformed one WITHOUT a database read);
	// ErrUserDisabled when the owner is disabled. ctx must be able to read any
	// user's row (authz.System): the owner is what it learns.
	Authenticate(ctx context.Context, token string) (Token, error)
	// Create mints a token named name for ctx's user, records it in
	// audit_log, and returns the token ONCE with its record.
	Create(ctx context.Context, name string) (string, Token, error)
	// List returns ctx's user's tokens, newest first, revoked included.
	List(ctx context.Context) ([]Token, error)
	// Revoke revokes ctx's user's token id. found is false when the user has
	// no such token (another user's id included); wasLive is false when it
	// was already revoked. A revoke that changed something is audited.
	Revoke(ctx context.Context, id int64) (found, wasLive bool, err error)
}

// PGStore is Store over Postgres.
type PGStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns the store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool, now: time.Now} }

var _ Store = (*PGStore)(nil)

const cols = `id, user_id, name, created_at, last_used_at, revoked_at`

func scan(row pgx.Row) (Token, error) {
	var t Token
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	return t, err
}

// Authenticate implements Store.
//
// 🔴 THE SHAPE CHECK COMES FIRST AND COSTS NO I/O: a value that is not
// Prefix + 43 base64url characters is refused before the pool is touched
// (TestAMalformedTokenIsRefusedWithoutADatabaseRead).
//
// 🔴 REVOKED AND DISABLED ARE READ ON EVERY CALL — nothing is cached — so both
// take effect on the next request.
//
// ⚠ last_used_at IS BEST-EFFORT. It is rewritten only when it is older than
// LastUsedGranularity, and a failure to write it does NOT refuse the token: a
// bookkeeping column must never turn a valid credential into a 401.
func (s *PGStore) Authenticate(ctx context.Context, token string) (Token, error) {
	if !ShapeOK(token) {
		return Token{}, ErrBadToken
	}
	var t Token
	var disabled bool
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.user_id, t.name, t.created_at, t.last_used_at, t.revoked_at,
			u.disabled_at IS NOT NULL
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1`, Hash(token)).
		Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	if err != nil {
		return Token{}, err
	}
	if t.Revoked() {
		return Token{}, ErrBadToken
	}
	if disabled {
		return Token{}, ErrUserDisabled
	}
	if now := s.now(); t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= LastUsedGranularity {
		// ONE throttle, here, on the value just read. (A second copy in the
		// UPDATE's WHERE was unreachable — this branch already decided — so
		// no test could tell it was there; two concurrent first uses both
		// writing is harmless.)
		var stamped time.Time
		if err := s.pool.QueryRow(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1
			RETURNING last_used_at`, t.ID).Scan(&stamped); err == nil {
			t.LastUsedAt = &stamped
		}
	}
	return t, nil
}

// user is the acting user of a management call.
func user(ctx context.Context) (authz.Principal, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return authz.Principal{}, ErrNoUser
	}
	return p, nil
}

// CleanName trims a label and checks its length. Exported so a handler can
// refuse a bad one with a 400 before minting.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", ErrNameTooLong
	}
	return name, nil
}

// Create implements Store. The token row and its audit row are one
// transaction: a token is never live without its "created" record.
func (s *PGStore) Create(ctx context.Context, name string) (string, Token, error) {
	p, err := user(ctx)
	if err != nil {
		return "", Token{}, err
	}
	if name, err = CleanName(name); err != nil {
		return "", Token{}, err
	}
	tok, err := New()
	if err != nil {
		return "", Token{}, err
	}
	var t Token
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scan(tx.QueryRow(ctx, `INSERT INTO api_tokens (user_id, name, token_hash)
			VALUES ($1, $2, $3) RETURNING `+cols, p.UserID, name, Hash(tok)))
		if err != nil {
			return err
		}
		return audit(ctx, tx, p, "api_token.create", t.ID, map[string]any{"name": t.Name})
	})
	if err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

// List implements Store.
func (s *PGStore) List(ctx context.Context) ([]Token, error) {
	p, err := user(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM api_tokens WHERE user_id = $1
		ORDER BY created_at DESC, id DESC`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke implements Store.
func (s *PGStore) Revoke(ctx context.Context, id int64) (found, wasLive bool, err error) {
	p, err := user(ctx)
	if err != nil {
		return false, false, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var revoked *time.Time
		var name string
		err := tx.QueryRow(ctx, `SELECT revoked_at, name FROM api_tokens WHERE id = $1 AND user_id = $2 FOR UPDATE`,
			id, p.UserID).Scan(&revoked, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if revoked != nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		wasLive = true
		return audit(ctx, tx, p, "api_token.revoke", id, map[string]any{"name": name})
	})
	if err != nil {
		return false, false, err
	}
	return found, wasLive, nil
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

// Object is the audit_log `object` a token's rows carry.
func Object(id int64) string { return "api_token:" + strconv.FormatInt(id, 10) }

// audit appends one audit_log row as p (0009's self_append policy: a user
// appends only rows naming itself). The detail never carries the token or its
// hash — only the label.
func audit(ctx context.Context, tx pgx.Tx, p authz.Principal, action string, id int64, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (user_id, via, action, object, detail) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		p.UserID, string(p.Via), action, Object(id), string(b))
	return err
}
