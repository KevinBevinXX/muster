package push

import (
	"context"
	"errors"
	"io"
	"log"
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
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Users 2 and 3 are members (as the login role: users is not writable by a
	// principal). Each test starts from no subscriptions.
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE push_subscriptions, push_preferences`)
	return NewPG(pool), pool, dsn
}

// as is a request principal for user id (the role is irrelevant to row-level
// security, which keys on the user alone — the admin case is user 1).
func as(id int64) context.Context {
	role := authz.RoleMember
	if id == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: id, Role: role, Via: authz.ViaSession})
}

func sub(tail string) Subscription {
	var s Subscription
	s.Endpoint = "https://fcm.googleapis.com/fcm/send/" + tail
	s.Keys.P256dh, s.Keys.Auth = "p256-"+tail, "auth-"+tail
	return s
}

// TestPushSubscriptionsAreUnderRowLevelSecurity is the raw-SQL check: through
// the application pool, a member's principal and the ADMIN's principal (user
// 1 — admin is not "see all") each read only their own rows, cannot insert a
// row for someone else, and cannot update or delete another's; System sees all.
func TestPushSubscriptionsAreUnderRowLevelSecurity(t *testing.T) {
	s, pool, _ := pgStore(t)
	ctx := context.Background()
	for owner, tail := range map[int64]string{1: "admin-dev", 2: "member2-dev", 3: "member3-dev"} {
		if _, err := s.Claim(ctx, owner, sub(tail), "ua"); err != nil {
			t.Fatal(err)
		}
	}
	sys := authz.System(ctx)
	var all int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM push_subscriptions`).Scan(&all); err != nil || all != 3 {
		t.Fatalf("System sees %d rows (%v), want 3 — the positive control", all, err)
	}
	for _, uid := range []int64{1, 2} {
		rows, err := pool.Query(as(uid), `SELECT owner_id, endpoint FROM push_subscriptions`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var owner int64
			var ep string
			if err := rows.Scan(&owner, &ep); err != nil {
				t.Fatal(err)
			}
			if owner != uid {
				t.Errorf("user %d READ user %d's subscription %s", uid, owner, Short(ep))
			}
			n++
		}
		rows.Close()
		if n != 1 {
			t.Errorf("user %d sees %d rows, want exactly its own 1", uid, n)
		}
		if _, err := pool.Exec(as(uid), `INSERT INTO push_subscriptions (owner_id, endpoint, p256dh, auth)
			VALUES ($1, 'https://fcm.googleapis.com/fcm/send/forged', 'k', 'a')`, 5-uid); err == nil {
			t.Errorf("user %d inserted a subscription owned by user %d", uid, 5-uid)
		}
		tag, err := pool.Exec(as(uid), `UPDATE push_subscriptions SET fail_count = 99 WHERE owner_id <> $1`, uid)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("user %d updated %d of someone else's rows (%v)", uid, tag.RowsAffected(), err)
		}
		tag, err = pool.Exec(as(uid), `DELETE FROM push_subscriptions WHERE owner_id <> $1`, uid)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("user %d deleted %d of someone else's rows (%v)", uid, tag.RowsAffected(), err)
		}
	}
	// A principal-less context reads nothing and inserts nothing.
	var none int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions`).Scan(&none); err != nil || none != 0 {
		t.Errorf("no principal sees %d rows (%v)", none, err)
	}
	if err := pool.QueryRow(sys, `SELECT count(*) FROM push_subscriptions`).Scan(&all); err != nil || all != 3 {
		t.Fatalf("after the refused writes System sees %d rows, want 3", all)
	}

	// The same through the store's request methods.
	if d, _ := s.Devices(as(2)); len(d) != 1 || d[0].OwnerID != 2 {
		t.Errorf("Devices(user 2) = %+v", d)
	}
	theirs, _ := s.ForOwner(ctx, 3)
	if ok, _ := s.Remove(as(2), theirs[0].ID); ok {
		t.Error("user 2 removed user 3's device by id")
	}
	if ok, _ := s.Unsubscribe(as(2), theirs[0].Sub.Endpoint); ok {
		t.Error("user 2 unsubscribed user 3's endpoint")
	}
	if d, _ := s.ForOwner(ctx, 3); len(d) != 1 {
		t.Fatal("user 3's device is gone after user 2's refused removals")
	}
	if ok, err := s.Remove(as(3), theirs[0].ID); !ok || err != nil {
		t.Errorf("the owner's own Remove = %v, %v", ok, err)
	}
}

// TestClaimMovesASharedDeviceToTheNewUser: one browser, one endpoint, two
// users. When user 3 presents user 2's endpoint (signed in on 2's phone), the
// endpoint becomes 3's — 2 no longer has it, 3's sends reach it, 2's do not —
// and the device's delivery history starts over. Re-presenting by the same
// user is not a move.
func TestClaimMovesASharedDeviceToTheNewUser(t *testing.T) {
	s, _, _ := pgStore(t)
	ctx := context.Background()
	shared := sub("shared-phone")
	if prev, err := s.Claim(ctx, 2, shared, "Android Chrome"); err != nil || prev != 0 {
		t.Fatalf("first claim = %d, %v", prev, err)
	}
	d2, _ := s.ForOwner(ctx, 2)
	if err := s.MarkDelivered(ctx, d2[0]); err != nil {
		t.Fatal(err)
	}
	if prev, err := s.Claim(ctx, 2, shared, "Android Chrome"); err != nil || prev != 0 {
		t.Fatalf("a re-claim by the same user reported a move: %d, %v", prev, err)
	}
	if d, _ := s.ForOwner(ctx, 2); len(d) != 1 || d[0].LastOKAt == nil {
		t.Fatalf("a same-user re-claim lost its history: %+v", d)
	}

	rotated := shared
	rotated.Keys.P256dh = "rotated-key"
	prev, err := s.Claim(ctx, 3, rotated, "Android Chrome")
	if err != nil || prev != 2 {
		t.Fatalf("user 3's claim of user 2's endpoint = %d, %v; want previous owner 2", prev, err)
	}
	if d, _ := s.ForOwner(ctx, 2); len(d) != 0 {
		t.Fatalf("user 2 still holds the moved device: %+v", d)
	}
	d3, _ := s.ForOwner(ctx, 3)
	if len(d3) != 1 || d3[0].Sub.Keys.P256dh != "rotated-key" || d3[0].LastOKAt != nil || d3[0].ID != d2[0].ID {
		t.Fatalf("user 3's device = %+v (want the same row, new keys, no inherited last_ok_at)", d3)
	}
	// A device handle read before the move cannot touch the row after it.
	if ok, _ := s.Prune(ctx, d2[0]); ok {
		t.Fatal("a stale (pre-move) handle pruned user 3's device")
	}
	if err := s.MarkFailed(ctx, d2[0]); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.ForOwner(ctx, 3); d[0].FailCount != 0 {
		t.Fatal("a stale (pre-move) handle counted a failure on user 3's device")
	}
	if err := s.MarkDelivered(ctx, d2[0]); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.ForOwner(ctx, 3); d[0].LastOKAt != nil {
		t.Fatal("a stale (pre-move) handle stamped a delivery on user 3's device")
	}
	if ok, _ := s.Prune(ctx, d3[0]); !ok {
		t.Fatal("the current handle did not prune")
	}
}

func TestClaimRefusals(t *testing.T) {
	s, _, _ := pgStore(t)
	ctx := context.Background()
	if _, err := s.Claim(ctx, 0, sub("x"), ""); err == nil {
		t.Error("a claim for no user was accepted")
	}
	evil := sub("x")
	evil.Endpoint = "https://kubernetes.default.svc/api/v1/x"
	if _, err := s.Claim(ctx, 2, evil, ""); !errors.Is(err, ErrPushServiceNotAllowed) {
		t.Errorf("a non-push-service endpoint: %v", err)
	}
	for i := 0; i < MaxDevicesPerOwner; i++ {
		if _, err := s.Claim(ctx, 2, sub("cap-"+string(rune('a'+i))), ""); err != nil {
			t.Fatalf("device %d: %v", i, err)
		}
	}
	if _, err := s.Claim(ctx, 2, sub("cap-overflow"), ""); !errors.Is(err, ErrTooManyDevices) {
		t.Errorf("device %d: %v, want ErrTooManyDevices", MaxDevicesPerOwner+1, err)
	}
	// Re-presenting one the user already has is not a new device.
	if _, err := s.Claim(ctx, 2, sub("cap-a"), ""); err != nil {
		t.Errorf("re-claiming an existing device at the cap: %v", err)
	}
	long := strings.Repeat("é", maxUserAgent+50)
	if _, err := s.Claim(ctx, 3, sub("ua"), long); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.ForOwner(ctx, 3); len([]rune(d[0].UserAgent)) != maxUserAgent {
		t.Errorf("user agent stored at %d runes", len([]rune(d[0].UserAgent)))
	}
}

// TestForgetDeviceIsTheOwnersOnly: logout removes the signing-out browser's
// row by its endpoint hash — for that user only.
func TestForgetDeviceIsTheOwnersOnly(t *testing.T) {
	s, _, _ := pgStore(t)
	ctx := context.Background()
	a, b := sub("forget-a"), sub("forget-b")
	if _, err := s.Claim(ctx, 2, a, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, 3, b, ""); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ForgetDevice(ctx, 2, EndpointHash(b.Endpoint)); ok {
		t.Fatal("user 2's logout forgot user 3's device")
	}
	if ok, _ := s.ForgetDevice(ctx, 2, "short"); ok {
		t.Fatal("a malformed hash matched")
	}
	if ok, err := s.ForgetDevice(ctx, 2, EndpointHash(a.Endpoint)); !ok || err != nil {
		t.Fatalf("user 2's own device: %v, %v", ok, err)
	}
	if d, _ := s.ForOwner(ctx, 3); len(d) != 1 {
		t.Fatal("user 3's device went with user 2's logout")
	}
}

// TestPreferencesAreOwnedAndDefaultOn: no row means nothing muted; SetMuted
// keeps only known event names; one user's preferences are invisible to
// another (row-level security) and Muted(owner) reads exactly that owner's.
func TestPreferencesAreOwnedAndDefaultOn(t *testing.T) {
	s, _, _ := pgStore(t)
	ctx := context.Background()
	if m, err := s.Muted(ctx, 2); err != nil || len(m) != 0 {
		t.Fatalf("a user with no preferences: %v, %v (want nothing muted)", m, err)
	}
	if err := s.SetMuted(as(2), []string{EventComment, "bogus", EventCreated}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Muted(ctx, 2); strings.Join(m, ",") != EventCreated+","+EventComment {
		t.Fatalf("Muted(2) = %v", m)
	}
	if m, _ := s.MyMuted(as(3)); len(m) != 0 {
		t.Fatalf("user 3 reads user 2's preferences: %v", m)
	}
	if m, _ := s.Muted(ctx, 3); len(m) != 0 {
		t.Fatalf("Muted(3) = %v: the owner filter is not applied", m)
	}
	if err := s.SetMuted(as(2), nil); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MyMuted(as(2)); len(m) != 0 {
		t.Fatalf("clearing left %v", m)
	}
}
