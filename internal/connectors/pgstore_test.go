package connectors_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, connectors.KeySize) }

// testPool connects to this package's test database (internal/dbtest) and
// empties the connector tables, so each test starts from nothing.
func testPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	dbtest.OwnerExec(t, dsn, `TRUNCATE connectors, connector_grants, connector_audit RESTART IDENTITY`)
	return ctx, pool
}

// seedAgent INSERTs a minimal agent row (the grants' foreign key target) with a
// hooks token, and removes it when the test ends.
func seedAgent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, token string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO agents (name, namespace, hooks_token) VALUES ($1, $2, $3) RETURNING id`,
		name, "ns-"+name, token).Scan(&id); err != nil {
		t.Fatalf("seed agent %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, id)
	})
	return id
}

func newStore(t *testing.T, pool *pgxpool.Pool, k []byte) *connectors.PGStore {
	t.Helper()
	s, err := connectors.NewPG(pool, k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const fakeSecret = "ghp_FAKE-not-a-real-token-0123456789"

func createGitHub(t *testing.T, ctx context.Context, s connectors.Store) connectors.Connector {
	t.Helper()
	p := connectors.GitHubPreset
	c, err := s.CreateConnector(ctx, connectors.NewConnector{Name: "github", Platform: p.Key, Hosts: p.Hosts,
		Scheme: p.Scheme, Username: p.Username, Secret: fakeSecret})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return c
}

// TestACiphertextMovedToAnotherOwnersRowDoesNotOpen: the store seals as the
// owner it inserts the row as (user 1 today) and opens under the owner_id READ
// FROM THE ROW. So the same row re-owned by user 2 — the shape of one owner's
// ciphertext copied onto another owner's same-named connector — fails closed
// for both Credential and RotateSecret, and opens again once the owner is back.
func TestACiphertextMovedToAnotherOwnersRowDoesNotOpen(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	createGitHub(t, ctx, s)

	var owner int64
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM connectors WHERE name='github'`).Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("a created connector is owned by %d (%v), want 1", owner, err)
	}
	var two int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&two); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM connectors WHERE owner_id=$1`, two)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, two)
	})

	// The move and the reads after it run as authz.System (scope all): under
	// row-level security user 1 can neither hand a row to user 2 (WITH CHECK)
	// nor read it afterwards (it would be ErrNoRows, not the seal's refusal this
	// test is about). System sees the row as it really is — owner 2, owner 1's
	// ciphertext — which is exactly the tampered state the seal must refuse.
	sys := authz.System(ctx)
	if _, err := pool.Exec(sys, `UPDATE connectors SET owner_id=$1 WHERE name='github'`, two); err != nil {
		t.Fatal(err)
	}
	if _, secret, err := s.Credential(sys, "github"); !errors.Is(err, connectors.ErrDecrypt) || secret != "" {
		t.Fatalf("owner 1's ciphertext opened on owner %d's row: %q, %v; want ErrDecrypt", two, secret, err)
	}
	if err := s.RotateSecret(sys, "github", "ghp_FAKE-rotated-0123456789"); !errors.Is(err, connectors.ErrDecrypt) {
		t.Fatalf("RotateSecret re-sealed a ciphertext that does not open for the row's owner: %v", err)
	}

	if _, err := pool.Exec(sys, `UPDATE connectors SET owner_id=1 WHERE name='github'`); err != nil {
		t.Fatal(err)
	}
	if _, secret, err := s.Credential(ctx, "github"); err != nil || secret != fakeSecret {
		t.Fatalf("control: back on owner 1 the secret did not open: %q, %v", secret, err)
	}
}

// TestTheStoredRowNeverContainsThePlaintext reads the RAW row — every column,
// cast to text — and refuses any occurrence of the secret.
func TestTheStoredRowNeverContainsThePlaintext(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	createGitHub(t, ctx, s)
	var row string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(c)::text FROM connectors c WHERE name='github'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	var ct []byte
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connectors WHERE name='github'`).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	// Positive control: the row read is the row written.
	if !bytes.Contains([]byte(row), []byte("api.github.com")) || len(ct) == 0 {
		t.Fatalf("the raw read does not see the connector: %s", row)
	}
	for _, needle := range []string{fakeSecret, "not-a-real-token"} {
		if bytes.Contains([]byte(row), []byte(needle)) || bytes.Contains(ct, []byte(needle)) {
			t.Fatalf("the stored row contains %q", needle)
		}
	}
	_, got, err := s.Credential(ctx, "github")
	if err != nil || got != fakeSecret {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	// Rotation re-seals; the old secret is gone from the row too.
	if err := s.RotateSecret(ctx, "github", "ghp_FAKE-rotated-9876543210"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connectors WHERE name='github'`).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("rotated")) {
		t.Fatal("the rotated row contains the plaintext")
	}
	if _, got, _ := s.Credential(ctx, "github"); got != "ghp_FAKE-rotated-9876543210" {
		t.Fatalf("after rotate Credential = %q", got)
	}
}

func TestTheWrongKeyFailsClosedAtTheStore(t *testing.T) {
	ctx, pool := testPool(t)
	createGitHub(t, ctx, newStore(t, pool, key(1)))
	other := newStore(t, pool, key(2))
	c, secret, err := other.Credential(ctx, "github")
	if !errors.Is(err, connectors.ErrDecrypt) || secret != "" || c.Name != "" {
		t.Fatalf("Credential under the wrong key = %+v %q %v; want ErrDecrypt and nothing", c, secret, err)
	}
	// The public description is still readable — only the secret is sealed.
	if _, err := other.GetConnector(ctx, "github"); err != nil {
		t.Fatalf("GetConnector: %v", err)
	}
}

func TestADuplicateNameIsRefused(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	createGitHub(t, ctx, s)
	_, err := s.CreateConnector(ctx, connectors.NewConnector{Name: "github", Hosts: []string{"x.example.com"},
		Scheme: connectors.SchemeBearer, Secret: "0123456789abc"})
	if !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("duplicate = %v", err)
	}
}

func TestGrantLifecycle(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "grant-agent-a", "tok-a-0123456789")

	g, created, err := s.RequestGrant(ctx, a, "github", "need the repo")
	if err != nil || !created || g.Status != connectors.StatusPending || g.AgentName != "grant-agent-a" {
		t.Fatalf("first request = %+v created=%v err=%v", g, created, err)
	}
	g2, created, err := s.RequestGrant(ctx, a, "github", "again")
	if err != nil || created || g2.ID != g.ID {
		t.Fatalf("a second request while pending made a new row: %+v created=%v err=%v", g2, created, err)
	}
	pending, _ := s.ListPendingGrants(ctx)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	d, err := s.DecideGrant(ctx, g.ID, true, "operator")
	if err != nil || d.Status != connectors.StatusApproved || !d.Usable() {
		t.Fatalf("approve = %+v %v", d, err)
	}
	if _, err := s.DecideGrant(ctx, g.ID, false, "operator"); !errors.Is(err, connectors.ErrNotPending) {
		t.Fatalf("re-deciding = %v, want ErrNotPending", err)
	}
	r, err := s.RevokeGrant(ctx, g.ID, "operator")
	if err != nil || r.Status != connectors.StatusRevoked {
		t.Fatalf("revoke = %+v %v", r, err)
	}
	// A new request after revocation is a NEW row.
	g3, created, err := s.RequestGrant(ctx, a, "github", "")
	if err != nil || !created || g3.ID == g.ID {
		t.Fatalf("request after revoke = %+v created=%v err=%v", g3, created, err)
	}
}

func TestDeletingAConnectorDeletesItsGrants(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "grant-agent-del", "tok-del-0123456789")
	createGitHub(t, ctx, s)
	g, _, _ := s.RequestGrant(ctx, a, "github", "")
	if _, err := s.DecideGrant(ctx, g.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnector(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGrant(ctx, g.ID); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("the grant survived its connector: %v", err)
	}
	// Re-created under the same name, it inherits no approval.
	createGitHub(t, ctx, s)
	if _, err := s.LatestGrant(ctx, a, "github"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("a re-created connector inherited a grant: %v", err)
	}
}

func TestDestroyingTheAgentDeletesItsGrants(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "grant-agent-gone", "tok-gone-0123456789")
	g, _, _ := s.RequestGrant(ctx, a, "github", "")
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGrant(ctx, g.ID); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("the grant survived its agent: %v", err)
	}
}

func TestAuditRecordsAndPrunes(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	if err := s.Audit(ctx, connectors.AuditEntry{Kind: "proxy", AgentName: "a", Connector: "github",
		Method: "GET", Host: "api.github.com", Path: "/user", Status: 200}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(ctx, 10)
	if err != nil || len(list) != 1 || list[0].Path != "/user" || list[0].Status != 200 {
		t.Fatalf("audit = %+v %v", list, err)
	}
	n, err := s.PruneAudit(ctx, time.Now().Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
}
