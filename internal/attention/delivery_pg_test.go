package attention

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// THE ANSWER'S DELIVERY STATE (migration 0031), on a real Postgres: which
// answers owe a delivery, and that every transition is won by exactly one of
// its racers.

// agentFor makes an agent of uid's, for an agent asker.
func agentFor(t *testing.T, pool *pgxpool.Pool, uid int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(as(uid), `INSERT INTO agents (name, namespace) VALUES ($1, $1) RETURNING id`,
		"deliv-agent-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, id)
	})
	return id
}

// answered is an agent's ask of uid's, answered.
func answered(t *testing.T, s *PGStore, uid, agent int64, answer string) Ask {
	t.Helper()
	a := mustAsk(t, s, uid, New{Asker: Asker{AgentID: agent, Label: "worker"}, Question: "which way?"})
	got, err := s.Answer(as(uid), a.ID, answer)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAnAnswerOwesADeliveryOnlyToAnAgent: Answer queues an agent's answer,
// leaves a mirrored session's for its wait (pull), and owes a system asker's
// nothing; an answer in session and a cancelled ask owe nothing either.
func TestAnAnswerOwesADeliveryOnlyToAnAgent(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	if a := answered(t, s, 2, agent, "left"); a.Delivery != DeliveryQueued {
		t.Errorf("an agent's answer: delivery %q, want queued", a.Delivery)
	}
	m := mustAsk(t, s, 2, New{Asker: session, Question: "which way?"})
	if a, err := s.Answer(as(2), m.ID, "left"); err != nil || a.Delivery != DeliveryPull {
		t.Errorf("a mirrored session's answer: %q %v, want pull", a.Delivery, err)
	}
	sy := mustAsk(t, s, 2, New{Asker: Asker{System: true, Label: SystemAskerLabel}, Question: "which way?"})
	if a, err := s.Answer(as(2), sy.ID, "left"); err != nil || a.Delivery != DeliveryNone {
		t.Errorf("a system asker's answer: %q %v, want none", a.Delivery, err)
	}
	c := mustAsk(t, s, 2, New{Asker: Asker{AgentID: agent}, Question: "which way?"})
	if a, err := s.Cancel(as(2), c.ID); err != nil || a.Delivery != DeliveryNone {
		t.Errorf("a cancelled ask: %q %v", a.Delivery, err)
	}
	ins := mustAsk(t, s, 2, New{Asker: Asker{AgentID: agent}, Question: "which way?"})
	time.Sleep(5 * time.Millisecond) // created_at strictly before the statement
	got, err := s.AnswerInSession(as(2), Asker{AgentID: agent}, "go left", 0)
	if err != nil || len(got) != 1 || got[0].ID != ins.ID || got[0].Delivery != DeliveryNone {
		t.Errorf("an answer in session: %+v %v", got, err)
	}
	// The database refuses a delivery on anything but an answer (0031's CHECK).
	if _, err := pool.Exec(as(2), `UPDATE attention_asks SET delivery = 'queued' WHERE id = $1`, c.ID); err == nil {
		t.Error("a cancelled ask took a delivery state")
	}
}

// TestTheReaderAndThePusherRaceToOneOutcome: a wait reading the answer
// (MarkWaited) and a delivery claiming it, concurrently, every time end with
// the answer READ and the attempt unable to settle it — the reader wins
// whether it ran before the claim or during it.
func TestTheReaderAndThePusherRaceToOneOutcome(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	for i := 0; i < 20; i++ {
		a := answered(t, s, 2, agent, "left")
		var read, claimed atomic.Bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ok, err := s.MarkWaited(as(2), a.ID)
			read.Store(err == nil && ok)
		}()
		go func() {
			defer wg.Done()
			_, ok, err := s.ClaimDelivery(as(2), a.ID, "p1", time.Minute, 0)
			claimed.Store(err == nil && ok)
		}()
		wg.Wait()
		if !read.Load() {
			t.Fatalf("round %d: the reader did not win (claimed=%v)", i, claimed.Load())
		}
		if held, err := s.FinishDelivery(as(2), a.ID, "p1", true, 7, ""); err != nil || held {
			t.Fatalf("round %d: the attempt settled an answer its asker read (claimed=%v)", i, claimed.Load())
		}
		if got, _ := s.Get(as(2), a.ID); got.Delivery != DeliveryWaited {
			t.Fatalf("round %d: %q", i, got.Delivery)
		}
	}
}

// TestADeliveryIsClaimedOnceAndSettledOnlyByItsHolder: the grace holds a fresh
// answer back; of two claimers one wins; only the lease holder settles it; a
// release puts it back with its reason.
func TestADeliveryIsClaimedOnceAndSettledOnlyByItsHolder(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	a := answered(t, s, 2, agent, "left")
	if _, ok, err := s.ClaimDelivery(as(2), a.ID, "p1", time.Minute, time.Hour); err != nil || ok {
		t.Fatalf("an answer younger than the grace was claimed: %v %v", ok, err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, by := range []string{"p1", "p2", "p3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := s.ClaimDelivery(as(2), a.ID, by, time.Minute, 0); err == nil && ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claimers won", wins.Load())
	}
	var holder string
	if err := pool.QueryRow(as(2), `SELECT delivery_claimed_by FROM attention_asks WHERE id = $1`, a.ID).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if held, err := s.FinishDelivery(as(2), a.ID, "not-the-holder", true, 7, ""); err != nil || held {
		t.Fatalf("a non-holder settled the delivery: %v %v", held, err)
	}
	if err := s.ReleaseDelivery(as(2), a.ID, holder, "the agent is not running (stopped)"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(as(2), a.ID)
	if got.Delivery != DeliveryQueued || got.DeliveryDetail != "the agent is not running (stopped)" || got.DeliveryAttempts != 1 {
		t.Fatalf("after a release: %+v", got)
	}
	if _, ok, err := s.ClaimDelivery(as(2), a.ID, "p9", time.Minute, 0); err != nil || !ok {
		t.Fatal("a released delivery could not be claimed again")
	}
	if held, err := s.FinishDelivery(as(2), a.ID, "p9", true, 7, ""); err != nil || !held {
		t.Fatalf("the holder could not settle it: %v %v", held, err)
	}
	got, _ = s.Get(as(2), a.ID)
	if got.Delivery != DeliveryDelivered || got.DeliveredSession != 7 || got.DeliveryDetail != "" {
		t.Fatalf("delivered: %+v", got)
	}
	// Settled is final: no claim, no wait, no retry moves it.
	if _, ok, _ := s.ClaimDelivery(as(2), a.ID, "p1", time.Minute, 0); ok {
		t.Error("a delivered answer was claimed again")
	}
	if ok, _ := s.MarkWaited(as(2), a.ID); ok {
		t.Error("a delivered answer was marked waited")
	}
	if _, err := s.RetryDelivery(as(2), a.ID); !errors.Is(err, ErrNotRetryable) {
		t.Errorf("a delivered answer was retried: %v", err)
	}
}

// TestALapsedDeliveryIsUnconfirmedNotResent: a delivery whose process died
// (its lease lapsed) settles unconfirmed, is never handed out again by the
// sweep, and comes back only through RetryDelivery.
func TestALapsedDeliveryIsUnconfirmedNotResent(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	a := answered(t, s, 2, agent, "left")
	fresh := answered(t, s, 2, agent, "right")
	if _, ok, err := s.ClaimDelivery(as(2), a.ID, "dead", time.Millisecond, 0); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimDelivery(as(2), fresh.ID, "alive", time.Hour, 0); err != nil || !ok {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	lapsed, err := s.RecoverDeliveries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lapsed) != 1 || lapsed[0].ID != a.ID || lapsed[0].Owner != 2 || lapsed[0].Delivery != DeliveryUnconfirmed ||
		lapsed[0].DeliveryDetail != DeliveryLapsedDetail {
		t.Fatalf("recovered %+v; want only ask %d, unconfirmed", lapsed, a.ID)
	}
	due, err := s.DueDeliveries(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == a.ID || d.ID == fresh.ID {
			t.Fatalf("the sweep was handed %d (%s)", d.ID, d.Delivery)
		}
	}
	// Another owner cannot retry it; its owner can, once.
	if _, err := s.RetryDelivery(as(3), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner retried it: %v", err)
	}
	if r, err := s.RetryDelivery(as(2), a.ID); err != nil || r.Delivery != DeliveryQueued {
		t.Fatalf("retry: %+v %v", r, err)
	}
	if _, err := s.RetryDelivery(as(2), a.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("a queued delivery was retried: %v", err)
	}
	due, _ = s.DueDeliveries(context.Background(), 0, 50)
	if len(due) != 1 || due[0].ID != a.ID || due[0].Owner != 2 {
		t.Fatalf("after the retry the sweep was handed %+v", due)
	}
}

// TestDeliveriesAreTheirOwnersAlone: another owner sees, claims, waits on and
// lists none of them.
func TestDeliveriesAreTheirOwnersAlone(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	a := answered(t, s, 2, agent, "left")
	if _, ok, err := s.ClaimDelivery(as(3), a.ID, "p", time.Minute, 0); err != nil || ok {
		t.Errorf("another owner claimed it: %v %v", ok, err)
	}
	if ok, err := s.MarkWaited(as(3), a.ID); err != nil || ok {
		t.Errorf("another owner marked it waited: %v %v", ok, err)
	}
	if got, err := s.Answered(as(3), time.Now().Add(-time.Hour), 20); err != nil || len(got) != 0 {
		t.Errorf("another owner's answered list: %+v %v", got, err)
	}
	got, err := s.Answered(as(2), time.Now().Add(-time.Hour), 20)
	if err != nil || len(got) != 1 || got[0].ID != a.ID || got[0].Answer != "left" {
		t.Errorf("the owner's answered list: %+v %v", got, err)
	}
}

// TestTheSweepTakesTheLeastTriedFirst: an old answer whose agent keeps
// refusing it (many attempts) does not hold a newer one out of the sweep's
// batch.
func TestTheSweepTakesTheLeastTriedFirst(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	stuck := answered(t, s, 2, agent, "left")
	for i := 0; i < 3; i++ {
		if _, ok, err := s.ClaimDelivery(as(2), stuck.ID, "p", time.Minute, 0); err != nil || !ok {
			t.Fatal(err)
		}
		if err := s.ReleaseDelivery(as(2), stuck.ID, "p", "the agent is not running (stopped)"); err != nil {
			t.Fatal(err)
		}
	}
	fresh := answered(t, s, 2, agent, "right")
	due, err := s.DueDeliveries(context.Background(), 0, 1)
	if err != nil || len(due) != 1 || due[0].ID != fresh.ID {
		t.Fatalf("a batch of one took %+v; want the untried answer %d before the thrice-refused %d", due, fresh.ID, stuck.ID)
	}
}

// TestAWaitDuringADeliveryAttemptIsNotPushedAgain: the asker reads the answer
// while a delivery holds its lease, and that attempt then gives up (the agent
// was busy — it was in that very wait). The answer is the waiter's: the give-up
// does not put it back in the queue, and no sweep hands it out again. And the
// other order: a wait arriving after the attempt delivered marks nothing.
func TestAWaitDuringADeliveryAttemptIsNotPushedAgain(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	a := answered(t, s, 2, agent, "left")
	if _, ok, err := s.ClaimDelivery(as(2), a.ID, "p1", time.Minute, 0); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := s.MarkWaited(as(2), a.ID); err != nil || !ok {
		t.Errorf("a wait during the attempt did not record the read: %v %v", ok, err)
	}
	if err := s.ReleaseDelivery(as(2), a.ID, "p1", "the agent is in the middle of a turn"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(as(2), a.ID)
	if got.Delivery != DeliveryWaited {
		t.Fatalf("after the attempt gave up: %q; want waited", got.Delivery)
	}
	due, err := s.DueDeliveries(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == a.ID {
			t.Fatal("the sweep would push an answer its asker already read")
		}
	}
	if held, err := s.FinishDelivery(as(2), a.ID, "p1", true, 7, ""); err != nil || held {
		t.Fatalf("a read answer was settled delivered by the attempt that lost it: %v %v", held, err)
	}
	b := answered(t, s, 2, agent, "right")
	if _, ok, _ := s.ClaimDelivery(as(2), b.ID, "p2", time.Minute, 0); !ok {
		t.Fatal("claim")
	}
	if held, err := s.FinishDelivery(as(2), b.ID, "p2", true, 7, ""); err != nil || !held {
		t.Fatal(err)
	}
	if ok, _ := s.MarkWaited(as(2), b.ID); ok {
		t.Error("a wait after the delivery marked a delivered answer read")
	}
}
