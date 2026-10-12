package mirror

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool, string) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// A second user for the isolation checks (as the login role: users is not
	// writable by a principal).
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	return NewPG(pool), pool, dsn
}

func user(id int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: id, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// TestPGStoreTokensAreHashedOwnedAndIsolated: a token minted by the operator is
// owned by user 1 and stored as its hash only; authentication must run under
// authz.System (another user's principal cannot even find it — row-level
// security); user 2 can neither list nor revoke user 1's token; a revoked token
// stops authenticating; no principal cannot mint at all.
func TestPGStoreTokensAreHashedOwnedAndIsolated(t *testing.T) {
	s, pool, _ := pgStore(t)
	op := authz.Operator(context.Background())
	sys := authz.System(context.Background())

	tok, rec, err := s.CreateToken(op, "ws-iso")
	if err != nil {
		t.Fatal(err)
	}
	if rec.OwnerID != authz.OperatorUserID {
		t.Fatalf("an operator-minted token is owned by user %d, want %d", rec.OwnerID, authz.OperatorUserID)
	}
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) != len(TokenPrefix)+43 {
		t.Fatalf("minted token %q is not %s + 43 base64url chars", tok, TokenPrefix)
	}
	var row string
	if err := pool.QueryRow(sys, `SELECT row_to_json(t)::text FROM mirror_tokens t WHERE id = $1`, rec.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, strings.TrimPrefix(tok, TokenPrefix)) || !strings.Contains(row, HashToken(tok)) {
		t.Fatalf("the stored row is not hash-only: %s", row)
	}

	if got, err := s.Authenticate(sys, tok); err != nil || got.Host != "ws-iso" || got.OwnerID != 1 {
		t.Fatalf("Authenticate as System = %+v, %v", got, err)
	}
	if _, err := s.Authenticate(user(2), tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("user 2's principal FOUND user 1's token (%v): row-level security is not applying", err)
	}
	for name, bad := range map[string]string{
		"hook-shaped":  "hook-token-sentinel-0123456789",
		"unknown mmt_": TokenPrefix + strings.Repeat("A", 43),
		"hash itself":  HashToken(tok),
	} {
		if _, err := s.Authenticate(sys, bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("Authenticate(%s) = %v, want ErrBadToken", name, err)
		}
	}

	if list, err := s.ListTokens(user(2)); err != nil || len(list) != 0 {
		t.Fatalf("user 2 lists %v (%v), want none of user 1's", list, err)
	}
	if ok, _ := s.RevokeToken(user(2), rec.ID); ok {
		t.Fatal("user 2 revoked user 1's token")
	}
	if live, _ := s.TokenLive(sys, rec.ID); !live {
		t.Fatal("user 2's refused revoke still revoked it")
	}
	if ok, err := s.RevokeToken(op, rec.ID); !ok || err != nil {
		t.Fatalf("the owner's revoke = %v, %v", ok, err)
	}
	if _, err := s.Authenticate(sys, tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a REVOKED token still authenticates: %v", err)
	}
	if live, _ := s.TokenLive(sys, rec.ID); live {
		t.Fatal("TokenLive reports a revoked token live")
	}

	if _, _, err := s.CreateToken(context.Background(), "ws-noone"); err == nil {
		t.Fatal("a token was minted with NO principal (it would belong to nobody)")
	}
	if _, _, err := s.CreateToken(op, "Not_A_Host"); !errors.Is(err, ErrBadHost) {
		t.Fatalf("CreateToken(bad host) = %v, want ErrBadHost", err)
	}
}
