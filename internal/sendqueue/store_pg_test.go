package sendqueue

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

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE send_queue`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

var win = Target{Host: "laptop", Session: "scratch2", Window: "@7"}

func mirrorItem(body string) Item {
	return Item{Target: win, Body: body, StepUpAt: time.Now()}
}

// claimFor is instance by's claim at now, a minute's lease, no status time.
func claimFor(by string, now time.Time) Claim {
	return Claim{By: by, Until: now.Add(time.Minute)}
}

func mustEnqueue(t *testing.T, s *PGStore, uid int64, it Item) Item {
	t.Helper()
	out, err := s.Enqueue(as(uid), it)
	if err != nil {
		t.Fatalf("enqueue as %d: %v", uid, err)
	}
	return out
}

func bodies(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, string(it.State)+":"+it.Body)
	}
	return out
}

// TestAQueueIsInvisibleToAnotherOwner: user 2's queue for a mirrored window is
// not in user 3's list of the same address; 3's Get and Cancel of 2's message
// answer not found and leave it queued; and 3 queueing for the same address
// gets a queue of its own. The positive control is 2's own list, read after 3
// acted.
func TestAQueueIsInvisibleToAnotherOwner(t *testing.T) {
	s, _ := pgStore(t)
	mm := mustEnqueue(t, s, 2, mirrorItem("for the window"))
	if mm.Owner != 2 || mm.State != Queued {
		t.Fatalf("stored row = %+v; want owner 2, queued", mm)
	}
	if got, err := s.List(as(3), win); err != nil || len(got) != 0 {
		t.Fatalf("user 3 lists %s: %v, %v; want nothing", win, bodies(got), err)
	}
	if _, ok, err := s.HeadOf(as(3), win); err != nil || ok {
		t.Fatalf("user 3 has a head for %s: %v, %v", win, ok, err)
	}
	if _, err := s.Get(as(3), mm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 gets %d: %v, want ErrNotFound", mm.ID, err)
	}
	if _, err := s.Cancel(as(3), mm.ID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 cancels %d: %v, want ErrNotFound", mm.ID, err)
	}
	own := mustEnqueue(t, s, 3, mirrorItem("user 3's own"))
	if got, _ := s.List(as(3), win); len(got) != 1 || got[0].ID != own.ID {
		t.Fatalf("user 3's own queue for the same address = %v", bodies(got))
	}
	got, err := s.List(as(2), win)
	if err != nil || len(got) != 1 || bodies(got)[0] != "queued:for the window" {
		t.Fatalf("CONTROL: user 2 lists %s: %v, %v; want [queued:for the window]", win, bodies(got), err)
	}
}

// TestTheQueueIsBoundedPerTarget: MaxOpenPerTarget messages queue, the next is
// ErrFull and is NOT stored; another target is unaffected; a finished (here,
// cancelled) message frees its place.
func TestTheQueueIsBoundedPerTarget(t *testing.T) {
	s, _ := pgStore(t)
	var first Item
	for i := 0; i < MaxOpenPerTarget; i++ {
		it := mustEnqueue(t, s, 2, mirrorItem("m"))
		if i == 0 {
			first = it
		}
	}
	if _, err := s.Enqueue(as(2), mirrorItem("one too many")); !errors.Is(err, ErrFull) {
		t.Fatalf("message %d: %v, want ErrFull", MaxOpenPerTarget+1, err)
	}
	if got, _ := s.List(as(2), win); len(got) != MaxOpenPerTarget {
		t.Fatalf("a refused message was stored: %d open, want %d", len(got), MaxOpenPerTarget)
	}
	other := mirrorItem("another window")
	other.Target.Window = "@8"
	mustEnqueue(t, s, 2, other)
	if _, err := s.Cancel(as(2), first.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, s, 2, mirrorItem("fits again"))
}

// TestAClaimIsOnceAndOneTargetSendsOneAtATime: a message is claimed once (a
// second claim of it is refused); while it is being sent, the target's next
// message cannot be claimed (the one-sending index), another target's can;
// once it is finished, the next can.
func TestAClaimIsOnceAndOneTargetSendsOneAtATime(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	m1 := mustEnqueue(t, s, 2, mirrorItem("one"))
	m2 := mustEnqueue(t, s, 2, mirrorItem("two"))
	o := mirrorItem("other window")
	o.Target.Window = "@9"
	other := mustEnqueue(t, s, 2, o)
	now := time.Now()

	if it, ok, err := s.Claim(sys, m1.ID, now, claimFor("a", now)); err != nil || !ok || it.State != Sending || it.ClaimedAt.IsZero() {
		t.Fatalf("first claim = %+v, %v, %v", it, ok, err)
	}
	if _, ok, err := s.Claim(sys, m1.ID, now, claimFor("a", now)); err != nil || ok {
		t.Fatalf("a second claim of the same message = %v, %v; want refused", ok, err)
	}
	if _, ok, err := s.Claim(sys, m2.ID, now, claimFor("a", now)); err != nil || ok {
		t.Fatalf("the target's next message claimed while one is sending = %v, %v; want refused", ok, err)
	}
	if _, ok, err := s.Claim(sys, other.ID, now, claimFor("a", now)); err != nil || !ok {
		t.Fatalf("another target's message = %v, %v; want claimed", ok, err)
	}
	if _, err := s.Cancel(as(2), m1.ID, now); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancelling a message being sent = %v, want ErrNotCancellable", err)
	}
	// A dispatcher that read a head just before its owner cancelled it cannot
	// claim it afterwards.
	c := mirrorItem("cancelled under the dispatcher")
	c.Target.Window = "@10"
	cancelled := mustEnqueue(t, s, 2, c)
	if _, err := s.Cancel(as(2), cancelled.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(sys, cancelled.ID, now, claimFor("a", now)); err != nil || ok {
		t.Fatalf("claiming a cancelled message = %v, %v; want refused", ok, err)
	}
	if err := s.Finish(sys, m1.ID, "a", Outcome{Sent: true, Detail: "Sent."}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(sys, m2.ID, now, claimFor("a", now)); err != nil || !ok {
		t.Fatalf("the next message after the first finished = %v, %v; want claimed", ok, err)
	}
	got, _ := s.Get(as(2), m1.ID)
	if got.State != Sent || !got.MayHaveDelivered || got.FinishedAt.IsZero() || got.Detail != "Sent." {
		t.Fatalf("the finished message = %+v", got)
	}
}

// TestRecoverFailsAMessageLeftSendingAndNeverRequeuesIt: a row whose sender
// stopped — 'sending', its lease expired — is failed, may have been
// delivered, with the reason, never back to queued; and the status time its
// claim recorded is then what the target's next message waits behind
// (Head.TurnAfter).
func TestRecoverFailsAMessageLeftSendingAndNeverRequeuesIt(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	m1 := mustEnqueue(t, s, 2, mirrorItem("one"))
	m2 := mustEnqueue(t, s, 2, mirrorItem("two"))
	claimed := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if _, ok, err := s.Claim(sys, m1.ID, claimed, Claim{By: "a", Until: claimed.Add(30 * time.Second), Since: 1234}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	n, err := s.Recover(sys, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	got, _ := s.Get(as(2), m1.ID)
	if got.State != Failed || !got.MayHaveDelivered || got.Detail != RecoveredDetail {
		t.Fatalf("the recovered message = %+v", got)
	}
	heads, err := s.Heads(sys)
	if err != nil || len(heads) != 1 || heads[0].ID != m2.ID || heads[0].TurnAfter != 1234 {
		t.Fatalf("heads after recovery = %+v, %v; want m2 behind the recovered claim's status time 1234", heads, err)
	}
	if n, _ := s.Recover(sys, time.Now()); n != 0 {
		t.Fatalf("a second Recover moved %d rows", n)
	}
	// A sender that outlived the recovery (it was only slow) finishing late
	// does not rewrite the outcome: only a row still being sent is finished.
	if err := s.Finish(sys, m1.ID, "a", Outcome{Sent: true, Detail: "late"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(as(2), m1.ID); got.State != Failed || got.Detail != RecoveredDetail {
		t.Fatalf("a late Finish rewrote the recovered message: %+v", got)
	}
}

// TestTheTurnAfterIsThePasteThatMayHaveLanded: Head.TurnAfter is the newest
// status time recorded at a paste that may have reached the window — a
// claim's of a message that was sent or failed after it may have been
// delivered, NOT one that failed before reaching the target (it started no
// turn) — or the head's own: queued with one (Enqueue's AfterSince), or
// raised by a person's Send while it waited (NotePaste), which never lowers it.
func TestTheTurnAfterIsThePasteThatMayHaveLanded(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	claimFinish := func(at time.Time, since int64, o Outcome) {
		t.Helper()
		it := mustEnqueue(t, s, 2, mirrorItem("x"))
		if _, ok, err := s.Claim(sys, it.ID, at, Claim{By: "a", Until: at.Add(time.Minute), Since: since}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.Finish(sys, it.ID, "a", o, at); err != nil {
			t.Fatal(err)
		}
	}
	claimFinish(t0, 1000, Outcome{Sent: true})
	claimFinish(t0.Add(time.Minute), 2000, Outcome{MayHaveDelivered: true, Detail: "unconfirmed"})
	claimFinish(t0.Add(2*time.Minute), 3000, Outcome{Detail: "refused before the host"})
	next := mirrorItem("next")
	next.AfterSince = 1500
	n := mustEnqueue(t, s, 2, next)
	turnAfter := func(when string, want int64) {
		t.Helper()
		h, ok, err := s.HeadOf(as(2), win)
		if err != nil || !ok || h.ID != n.ID {
			t.Fatalf("%s: head = %+v, %v, %v", when, h, ok, err)
		}
		if h.TurnAfter != want {
			t.Fatalf("%s: TurnAfter = %d, want %d", when, h.TurnAfter, want)
		}
	}
	turnAfter("queued behind an older paste", 2000)
	if err := s.NotePaste(as(2), win, 2500); err != nil {
		t.Fatal(err)
	}
	turnAfter("after a Send at 2500", 2500)
	if err := s.NotePaste(as(2), win, 2200); err != nil {
		t.Fatal(err)
	}
	turnAfter("an older Send does not lower it", 2500)
	// Another owner's Send into the same address is not this queue's.
	if err := s.NotePaste(as(3), win, 9000); err != nil {
		t.Fatal(err)
	}
	turnAfter("another owner's Send", 2500)
	newer := mirrorItem("queued right after a Send")
	newer.Target.Window = "@8"
	newer.AfterSince = 4000
	mustEnqueue(t, s, 2, newer)
	h, ok, err := s.HeadOf(as(2), newer.Target)
	if err != nil || !ok || h.TurnAfter != 4000 {
		t.Fatalf("a message queued with the hub's paste time = %+v, %v, %v; want TurnAfter 4000", h, ok, err)
	}
}

// TestAMessageCarriesItsStepUpOrIsRefused: a message queued with no sign-in
// time is refused by the store, and the schema refuses one written without it
// directly. The control is the same message with it.
func TestAMessageCarriesItsStepUpOrIsRefused(t *testing.T) {
	s, pool := pgStore(t)
	it := mirrorItem("x")
	it.StepUpAt = time.Time{}
	if _, err := s.Enqueue(as(2), it); err == nil || !strings.Contains(err.Error(), "sign-in time") {
		t.Fatalf("a message with no step-up = %v, want refused", err)
	}
	_, err := pool.Exec(as(2), `INSERT INTO send_queue (mirror_host, mirror_session, mirror_window, body)
		VALUES ('laptop', 's', '', 'x')`)
	if err == nil || !strings.Contains(err.Error(), `"stepup_at"`) {
		t.Fatalf("the schema accepted a row with no stepup_at: %v", err)
	}
	stored := mustEnqueue(t, s, 2, mirrorItem("with"))
	if stored.StepUpAt.IsZero() {
		t.Fatal("CONTROL: the step-up time was not stored")
	}
}

// TestPruneDeletesOnlyOldFinishedMessages: a finished message older than the
// cut is deleted; a newer one, and an open one however old, are kept.
func TestPruneDeletesOnlyOldFinishedMessages(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	old := mustEnqueue(t, s, 2, mirrorItem("old"))
	if _, err := s.Cancel(as(2), old.ID, time.Now().Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recent := mustEnqueue(t, s, 2, mirrorItem("recent"))
	if _, err := s.Cancel(as(2), recent.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	open := mustEnqueue(t, s, 2, mirrorItem("open"))
	if n, err := s.Prune(sys, time.Now().Add(-Retention)); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want 1", n, err)
	}
	for id, want := range map[int64]bool{old.ID: false, recent.ID: true, open.ID: true} {
		if _, err := s.Get(as(2), id); (err == nil) != want {
			t.Errorf("message %d kept = %v, want %v", id, err == nil, want)
		}
	}
}
