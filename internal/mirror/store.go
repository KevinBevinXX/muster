package mirror

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBadToken is the refusal for a mirror token that is unknown or revoked.
var ErrBadToken = errors.New("mirror: unknown or revoked mirror token")

// Token is one minted mirror token's record. The token itself is never stored:
// only its sha256, so a database read cannot be replayed as a credential.
type Token struct {
	ID int64 `json:"id"`
	// OwnerID is the user who minted it; the host's sessions are visible only to
	// that user's principal.
	OwnerID   int64      `json:"ownerId"`
	Host      string     `json:"host"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsedAt,omitempty"`
}

// Store is the plugin's persistence: tokens, and nothing else.
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL, AND mirror_tokens IS UNDER
// ROW-LEVEL SECURITY (migration 0008). Authenticate and TokenLive must find a
// token before anyone is known, so internal/mirror's Service calls them under
// authz.System; CreateToken, ListTokens and RevokeToken run as the minting user
// (authz.Operator for `muster-server mirror token …`), so a token lands owned by
// them and they see and revoke only their own.
type Store interface {
	// Authenticate resolves a presented token to its record, or ErrBadToken.
	Authenticate(ctx context.Context, token string) (Token, error)
	// TokenLive reports whether token id is still unrevoked.
	TokenLive(ctx context.Context, id int64) (bool, error)
	// CreateToken mints a token for host, owned by ctx's user, and returns it
	// ONCE, with its record.
	CreateToken(ctx context.Context, host string) (string, Token, error)
	ListTokens(ctx context.Context) ([]Token, error)
	// RevokeToken revokes token id, reporting whether it was live.
	RevokeToken(ctx context.Context, id int64) (bool, error)
}

// HashToken is the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a fresh token: TokenPrefix + 43 base64url characters (256 bits).
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mirror: mint a token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// TokenShapeOK reports whether token is shaped like one NewToken mints:
// TokenPrefix + 43 base64url characters. It is a FILTER, not authentication:
// it lets the connect route refuse junk without a database read.
func TokenShapeOK(token string) bool {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// PGStore is Store over Postgres (migration 0008).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG builds the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const tokenCols = `id, owner_id, host, created_at, revoked_at, last_used_at`

func scanToken(row pgx.Row) (Token, error) {
	var t Token
	err := row.Scan(&t.ID, &t.OwnerID, &t.Host, &t.CreatedAt, &t.RevokedAt, &t.LastUsed)
	return t, err
}

// Authenticate looks the token up by hash among unrevoked tokens and stamps its
// last use.
//
// ⚠ A STRING WITHOUT THE PREFIX IS REFUSED BEFORE THE QUERY. That is not the
// security property — the hash lookup is, and no other credential's hash is in
// this table — it only keeps a hook token pasted into the wrong config from
// costing a database round trip per reconnect.
func (s *PGStore) Authenticate(ctx context.Context, token string) (Token, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return Token{}, ErrBadToken
	}
	t, err := scanToken(s.pool.QueryRow(ctx, `
		UPDATE mirror_tokens SET last_used_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING `+tokenCols, HashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	return t, err
}

func (s *PGStore) TokenLive(ctx context.Context, id int64) (bool, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mirror_tokens WHERE id = $1 AND revoked_at IS NULL)`, id).Scan(&live)
	return live, err
}

func (s *PGStore) CreateToken(ctx context.Context, host string) (string, Token, error) {
	if err := ValidHost(host); err != nil {
		return "", Token{}, err
	}
	tok, err := NewToken()
	if err != nil {
		return "", Token{}, err
	}
	// owner_id is the column default: the acting principal's user (0008).
	t, err := scanToken(s.pool.QueryRow(ctx, `
		INSERT INTO mirror_tokens (host, token_hash) VALUES ($1, $2) RETURNING `+tokenCols,
		host, HashToken(tok)))
	if err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

func (s *PGStore) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenCols+` FROM mirror_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) RevokeToken(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE mirror_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
