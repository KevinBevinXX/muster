package push

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/users"
)

// TestClaimNativeNeverDeadlocksARevocation: a phone (re)registering its push
// endpoint (ClaimNative) races the three revocations that delete phones — Sign
// out everywhere, a disable and a password reset — for the same owner, in a
// loop. ClaimNative locks the device row before it touches a push row; a
// revocation's DELETE FROM devices locks the device row and then, by cascade,
// its push rows. With ClaimNative's lock taken after its push-row delete (the
// old order) this deadlocked (SQLSTATE 40P01) within seconds; it must now
// never.
//
// The controls: claims succeeded, and revocations ran, enough times for the
// zero to mean something (a run where nothing overlapped proves nothing).
func TestClaimNativeNeverDeadlocksARevocation(t *testing.T) {
	s, pool, _ := pgStore(t)
	s.WithAllowlist(NewAllowlist([]string{"ntfy.example.org"}))
	us := users.NewPG(pool)
	ctx := context.Background()
	const owner = 2
	t.Cleanup(func() {
		_, _ = pool.Exec(sys(ctx), `DELETE FROM devices WHERE owner_id = $1`, owner)
		_, _ = pool.Exec(sys(ctx), `UPDATE users SET disabled_at = NULL WHERE id = $1`, owner)
	})
	var (
		deadlocks, claims, revocations atomic.Int64
		first                          atomic.Value
	)
	note := func(who string, err error) bool {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "40P01" {
			if deadlocks.Add(1) == 1 {
				first.Store(who + ": " + pg.Message)
			}
			return true
		}
		return false
	}
	deadline := time.Now().Add(6 * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline) && deadlocks.Load() == 0; i++ {
				var id int64
				if err := pool.QueryRow(sys(ctx), `INSERT INTO devices (owner_id, token_sha256, paired_signin_at)
					VALUES ($1, sha256(convert_to($2, 'UTF8')), now() + interval '1 hour') RETURNING id`,
					owner, fmt.Sprintf("dl-%d-%d-%d", w, i, time.Now().UnixNano())).Scan(&id); err != nil {
					note("insert device", err)
					continue
				}
				for k := 0; k < 3; k++ { // re-registering replaces (deletes) the previous row first
					var sub Subscription
					sub.Endpoint = fmt.Sprintf("https://ntfy.example.org/upDL%d-%d-%d", w, i, k)
					sub.Keys.P256dh, sub.Keys.Auth = "k", "a"
					err := s.ClaimNative(ctx, owner, id, sub, "stress")
					if note("claim", err) {
						return
					}
					if err == nil {
						claims.Add(1)
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; time.Now().Before(deadline) && deadlocks.Load() == 0; i++ {
			var err error
			switch i % 4 {
			case 0:
				_, err = us.RecordSignOutEverywhere(ctx, owner)
			case 1:
				_, err = us.SetDisabled(ctx, owner, true)
			case 2:
				_, err = us.SetDisabled(ctx, owner, false)
			default:
				_, err = us.ResetCredentials(ctx, owner)
			}
			if note("revocation", err) {
				return
			}
			if err == nil {
				revocations.Add(1)
			}
		}
	}()
	wg.Wait()
	if n := deadlocks.Load(); n != 0 {
		t.Fatalf("%d deadlock(s) between ClaimNative and a revocation; first: %v", n, first.Load())
	}
	if claims.Load() < 50 || revocations.Load() < 50 {
		t.Fatalf("CONTROL: only %d claims and %d revocations ran", claims.Load(), revocations.Load())
	}
	t.Logf("%d claims, %d revocations, 0 deadlocks", claims.Load(), revocations.Load())
}

// TestClaimNativeLocksTheDeviceRowBeforeAnyPushRow pins the ORDER itself,
// deterministically: with the device row held FOR UPDATE by another
// transaction (as a revocation's DELETE FROM devices holds it), a claim that
// would replace the device's endpoint must be waiting WITHOUT having locked
// the old push row. The probe is a NOWAIT lock of that row from a third
// transaction: it fails (55P03) if the claim took the push row first — the
// half of the cycle that deadlocks against the revocation's cascade. The
// control is the same probe once the device lock is released: the claim
// completes and replaces the row.
func TestClaimNativeLocksTheDeviceRowBeforeAnyPushRow(t *testing.T) {
	s, pool, _ := pgStore(t)
	s.WithAllowlist(NewAllowlist([]string{"ntfy.example.org"}))
	ctx := context.Background()
	id := device(t, s, 2, "order")
	var old, next Subscription
	old.Endpoint, next.Endpoint = "https://ntfy.example.org/upOld", "https://ntfy.example.org/upNext"
	old.Keys.P256dh, old.Keys.Auth, next.Keys.P256dh, next.Keys.Auth = "k", "a", "k", "a"
	if err := s.ClaimNative(ctx, 2, id, old, "phone"); err != nil {
		t.Fatal(err)
	}
	hold, err := pool.Begin(sys(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(sys(ctx), `SELECT 1 FROM devices WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ClaimNative(ctx, 2, id, next, "phone") }()
	// Wait until the claim is blocked on a lock.
	waiting := false
	for i := 0; i < 100 && !waiting; i++ {
		time.Sleep(50 * time.Millisecond)
		// pg_locks is readable by every role (pg_stat_activity hides another
		// session's wait details from this one); scoped to this database.
		_ = pool.QueryRow(sys(ctx), `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database())`).Scan(&waiting)
	}
	if !waiting {
		t.Fatal("CONTROL: the claim never blocked on the held device row")
	}
	probe, err := pool.Begin(sys(ctx))
	if err != nil {
		t.Fatal(err)
	}
	_, perr := probe.Exec(sys(ctx), `SELECT 1 FROM push_subscriptions WHERE endpoint = $1 FOR UPDATE NOWAIT`, old.Endpoint)
	_ = probe.Rollback(ctx)
	if perr != nil {
		t.Fatalf("while waiting for the device row the claim already held the old push row (%v): it locks push rows before the device row", perr)
	}
	_ = hold.Rollback(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CONTROL: the claim after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CONTROL: the claim never finished")
	}
	devs, err := s.ForOwner(ctx, 2)
	if err != nil || len(devs) != 1 || devs[0].Sub.Endpoint != next.Endpoint {
		t.Fatalf("CONTROL: after the claim: %v, %v", devs, err)
	}
}
