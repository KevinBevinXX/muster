package uploads

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members.
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
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE uploads`)
	return NewPG(pool), pool, dsn
}

func asUser(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func row(t *testing.T) Upload {
	t.Helper()
	id, err := NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	return Upload{PublicID: id, ObjectKey: ObjectKeyFor(id), Filename: "shot.png", ContentType: "image/png", Size: 42,
		ExpiresAt: time.Now().Add(time.Hour)}
}

// TestAnUploadIsItsOwnersAlone: user 2's upload is created as user 2 (the
// owner is the principal, not a parameter); user 3 cannot read or revoke it;
// the signed link's system read finds it. The positive control is user 2's
// own read, after user 3 acted.
func TestAnUploadIsItsOwnersAlone(t *testing.T) {
	s, _, _ := pgStore(t)
	in := row(t)
	got, err := s.Create(asUser(2), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.OwnerID != 2 || got.PublicID != in.PublicID || got.Revoked() {
		t.Fatalf("created %+v", got)
	}
	if _, err := s.Get(asUser(3), in.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read user 2's upload: %v", err)
	}
	if _, err := s.Revoke(asUser(3), in.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 revoked user 2's upload: %v", err)
	}
	if _, err := s.Get(context.Background(), in.PublicID); err == nil {
		t.Error("a context with no principal read an upload")
	}
	mine, err := s.Get(asUser(2), in.PublicID)
	if err != nil || mine.Revoked() {
		t.Fatalf("CONTROL: user 2's own read after user 3 acted: %+v, %v", mine, err)
	}
	link, err := s.LinkTarget(context.Background(), in.PublicID)
	if err != nil || link.OwnerID != 2 {
		t.Errorf("the link's read: %+v, %v", link, err)
	}
	rev, err := s.Revoke(asUser(2), in.PublicID)
	if err != nil || !rev.Revoked() {
		t.Fatalf("owner's revoke: %+v, %v", rev, err)
	}
	again, err := s.Revoke(asUser(2), in.PublicID)
	if err != nil || !again.RevokedAt.Equal(*rev.RevokedAt) {
		t.Errorf("a second revoke moved revoked_at or failed: %v", err)
	}
	link, err = s.LinkTarget(context.Background(), in.PublicID)
	if err != nil || !link.Revoked() {
		t.Errorf("the link's read after revoke: %+v, %v", link, err)
	}
}

// TestTheTableRefusesAKeyThatIsNotTheID: the object key is the id and
// nothing else, enforced by the schema.
func TestTheTableRefusesAKeyThatIsNotTheID(t *testing.T) {
	s, _, _ := pgStore(t)
	in := row(t)
	in.ObjectKey = "uploads/shot.png"
	if _, err := s.Create(asUser(2), in); err == nil {
		t.Error("a key carrying a filename was stored")
	}
	in = row(t)
	in.PublicID = "short"
	in.ObjectKey = "uploads/short"
	if _, err := s.Create(asUser(2), in); err == nil {
		t.Error("a malformed public id was stored")
	}
}
