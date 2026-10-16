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

	"github.com/ZacxDev/muster/internal/agents"
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
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'sq-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func mustAgent(t *testing.T, pool *pgxpool.Pool, uid int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(pool).Create(as(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: name,
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

var win = Target{Kind: KindMirror, Host: "laptop", Session: "scratch2", Window: "@7"}

func agentTarget(a agents.Agent) Target { return Target{Kind: KindAgent, AgentID: a.ID} }

func mirrorItem(body string) Item {
	return Item{Target: win, Pane: PaneClaude, Body: body, StepUpAt: time.Now()}
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

// TestAQueueIsInvisibleToAnotherOwner: user 2's queue — for an agent and for a
// mirrored window — is not in user 3's list of the same targets; 3's Get and
// Cancel of 2's message answer not found and leave it queued; 3 cannot queue
// for 2's agent; and 3 queueing for the same WINDOW address gets a queue of its
// own. The positive control is 2's own list, read after 3 acted.
func TestAQueueIsInvisibleToAnotherOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "sq-a")
	am := mustEnqueue(t, s, 2, Item{Target: agentTarget(a), Body: "for the agent"})
	mm := mustEnqueue(t, s, 2, mirrorItem("for the window"))
	if am.Owner != 2 || mm.Owner != 2 || am.State != Queued {
		t.Fatalf("stored rows = %+v / %+v; want owner 2, queued", am, mm)
	}

	for _, tgt := range []Target{agentTarget(a), win} {
		if got, err := s.List(as(3), tgt); err != nil || len(got) != 0 {
			t.Fatalf("user 3 lists %s: %v, %v; want nothing", tgt, bodies(got), err)
		}
		if _, ok, err := s.HeadOf(as(3), tgt); err != nil || ok {
			t.Fatalf("user 3 has a head for %s: %v, %v", tgt, ok, err)
		}
	}
	for _, id := range []int64{am.ID, mm.ID} {
		if _, err := s.Get(as(3), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user 3 gets %d: %v, want ErrNotFound", id, err)
		}
		if _, err := s.Cancel(as(3), id, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user 3 cancels %d: %v, want ErrNotFound", id, err)
		}
	}
	if _, err := s.Enqueue(as(3), Item{Target: agentTarget(a), Body: "x"}); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("user 3 queues for user 2's agent: %v, want ErrNoSuchAgent", err)
	}
	own := mustEnqueue(t, s, 3, mirrorItem("user 3's own"))
	if got, _ := s.List(as(3), win); len(got) != 1 || got[0].ID != own.ID {
		t.Fatalf("user 3's own queue for the same address = %v", bodies(got))
	}

	for tgt, want := range map[Target]string{agentTarget(a): "queued:for the agent", win: "queued:for the window"} {
		got, err := s.List(as(2), tgt)
		if err != nil || len(got) != 1 || bodies(got)[0] != want {
			t.Fatalf("CONTROL: user 2 lists %s: %v, %v; want [%s]", tgt, bodies(got), err, want)
		}
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

	if it, ok, err := s.Claim(sys, m1.ID, now); err != nil || !ok || it.State != Sending || it.ClaimedAt.IsZero() {
		t.Fatalf("first claim = %+v, %v, %v", it, ok, err)
	}
	if _, ok, err := s.Claim(sys, m1.ID, now); err != nil || ok {
		t.Fatalf("a second claim of the same message = %v, %v; want refused", ok, err)
	}
	if _, ok, err := s.Claim(sys, m2.ID, now); err != nil || ok {
		t.Fatalf("the target's next message claimed while one is sending = %v, %v; want refused", ok, err)
	}
	if _, ok, err := s.Claim(sys, other.ID, now); err != nil || !ok {
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
	if _, ok, err := s.Claim(sys, cancelled.ID, now); err != nil || ok {
		t.Fatalf("claiming a cancelled message = %v, %v; want refused", ok, err)
	}
	if err := s.Finish(sys, m1.ID, Outcome{Sent: true, Detail: "Sent."}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(sys, m2.ID, now); err != nil || !ok {
		t.Fatalf("the next message after the first finished = %v, %v; want claimed", ok, err)
	}
	got, _ := s.Get(as(2), m1.ID)
	if got.State != Sent || !got.MayHaveDelivered || got.FinishedAt.IsZero() || got.Detail != "Sent." {
		t.Fatalf("the finished message = %+v", got)
	}
}

// TestRecoverFailsAMessageLeftSendingAndNeverRequeuesIt: a row a dead process
// left 'sending' is failed, may have been delivered, with the reason — never
// back to queued — and is then what the target's next message waits behind
// (Head.LastAttempt).
func TestRecoverFailsAMessageLeftSendingAndNeverRequeuesIt(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	m1 := mustEnqueue(t, s, 2, mirrorItem("one"))
	m2 := mustEnqueue(t, s, 2, mirrorItem("two"))
	claimed := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if _, ok, err := s.Claim(sys, m1.ID, claimed); err != nil || !ok {
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
	if err != nil || len(heads) != 1 || heads[0].ID != m2.ID || !heads[0].LastAttempt.Equal(claimed) {
		t.Fatalf("heads after recovery = %+v, %v; want m2 behind the recovered claim at %v", heads, err, claimed)
	}
	if n, _ := s.Recover(sys, time.Now()); n != 0 {
		t.Fatalf("a second Recover moved %d rows", n)
	}
	// A sender that outlived the recovery (it was only slow) finishing late
	// does not rewrite the outcome: only a row still being sent is finished.
	if err := s.Finish(sys, m1.ID, Outcome{Sent: true, Detail: "late"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(as(2), m1.ID); got.State != Failed || got.Detail != RecoveredDetail {
		t.Fatalf("a late Finish rewrote the recovered message: %+v", got)
	}
}

// TestTheLastAttemptIsASendThatMayHaveLanded: Head.LastAttempt is the latest
// claim of a message that was sent or failed after it may have been delivered,
// and NOT one that failed before reaching the target — that one started no
// turn to wait for.
func TestTheLastAttemptIsASendThatMayHaveLanded(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	claimFinish := func(at time.Time, o Outcome) {
		t.Helper()
		it := mustEnqueue(t, s, 2, mirrorItem("x"))
		if _, ok, err := s.Claim(sys, it.ID, at); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.Finish(sys, it.ID, o, at); err != nil {
			t.Fatal(err)
		}
	}
	claimFinish(t0, Outcome{Sent: true})
	claimFinish(t0.Add(time.Minute), Outcome{MayHaveDelivered: true, Detail: "unconfirmed"})
	claimFinish(t0.Add(2*time.Minute), Outcome{Detail: "refused before the host"})
	next := mustEnqueue(t, s, 2, mirrorItem("next"))
	h, ok, err := s.HeadOf(as(2), win)
	if err != nil || !ok || h.ID != next.ID {
		t.Fatalf("head = %+v, %v, %v", h, ok, err)
	}
	if want := t0.Add(time.Minute); !h.LastAttempt.Equal(want) {
		t.Fatalf("LastAttempt = %v, want %v (the unconfirmed one, not the refused one)", h.LastAttempt, want)
	}
}

// TestAMirrorMessageCarriesItsStepUpOrIsRefused: a mirror message queued with
// no sign-in time is refused by the store, and the schema refuses one written
// without it directly; an agent message needs none.
func TestAMirrorMessageCarriesItsStepUpOrIsRefused(t *testing.T) {
	s, pool := pgStore(t)
	it := mirrorItem("x")
	it.StepUpAt = time.Time{}
	if _, err := s.Enqueue(as(2), it); err == nil || !strings.Contains(err.Error(), "sign-in time") {
		t.Fatalf("a mirror message with no step-up = %v, want refused", err)
	}
	_, err := pool.Exec(as(2), `INSERT INTO send_queue (kind, mirror_host, mirror_session, mirror_window, mirror_pane, body)
		VALUES ('mirror', 'laptop', 's', '', 'claude', 'x')`)
	if err == nil || !strings.Contains(err.Error(), "send_queue_subject_check") {
		t.Fatalf("the schema accepted a mirror row with no stepup_at: %v", err)
	}
	stored := mustEnqueue(t, s, 2, mirrorItem("with"))
	if stored.StepUpAt.IsZero() {
		t.Fatal("the step-up time was not stored")
	}
	a := mustAgent(t, pool, 2, "sq-noauth")
	if got := mustEnqueue(t, s, 2, Item{Target: agentTarget(a), Body: "x"}); !got.StepUpAt.IsZero() {
		t.Fatalf("an agent message carries a step-up time: %v", got.StepUpAt)
	}
}

// TestAQueuedMessageDiesWithItsAgent: deleting the agent deletes its queue.
func TestAQueuedMessageDiesWithItsAgent(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "sq-gone")
	it := mustEnqueue(t, s, 2, Item{Target: agentTarget(a), Body: "x"})
	if err := agents.NewPG(pool).Delete(as(2), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(as(2), it.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the message of a deleted agent = %v, want gone", err)
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
