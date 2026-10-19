package agentkickoff

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAHeldFirstTurnIsDeliveredOnceAfterTheGrant is the held kickoff against the
// REAL store, across a muster restart while it waits: one deliverer holds the
// turn (ccd says the github grant is pending), goes away, and a NEW deliverer —
// a new process, a new claim owner, nothing in memory — finds it still held, then
// delivers it exactly once when ccd reads ready. Later ticks, by it and by a third
// process, pay nothing.
//
// The row is backdated two hours: past ProvisioningStuckTimeout, so the old
// table would have failed it on the first tick; inside CheckoutWaitTimeout, so
// the new one must not.
func TestAHeldFirstTurnIsDeliveredOnceAfterTheGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := agents.NewPG(pool)

	prefix := "held-" + time.Now().Format("150405") + "-"
	const note = "summarise the open pull requests in kelp-index"
	row, err := store.Create(ctx, agents.Agent{
		Name: "russet-tern", Namespace: prefix + "russet-tern", Status: agents.StatusRunning,
		Kind: agents.KindClaudeCode, CCAccount: "acct-w5", Repo: "example-org/kelp-index",
		PendingNote: note, HooksToken: "tok-russet-6a",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	if _, err := pool.Exec(ctx, `UPDATE agents SET updated_at = now() - interval '2 hours' WHERE id=$1`, row.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// The agent runtime (ccd), shared by every muster process: its checkout state
	// and the turns it was sent.
	rt := &fakeGateway{log: &recorder{}, reply: "3 open pull requests"}
	rt.setCheckout(agents.CheckoutStatus{State: agents.CheckoutAwaitingGrant, Detail: "approve the request ccd filed"}, nil)
	insts := &fakeInstances{insts: []provision.Instance{readyInstance("russet-tern", "russet-tern-7c1-b4", 0)}}
	process := func(owner string) *Deliverer {
		d, err := New(Config{Store: store, Instances: insts, Gateway: rt, NamespacePrefix: prefix, Owner: owner})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return d
	}
	tick := func(d *Deliverer) agents.Agent {
		t.Helper()
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		d.Wait()
		got, err := store.Get(ctx, row.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}

	first := process("held-test/before-restart")
	for i := 0; i < 2; i++ {
		got := tick(first)
		if got.KickedOff || got.Status == agents.StatusError || !agents.KickoffAwaitingGrant(got) || rt.callCount() != 0 {
			t.Fatalf("tick %d while the grant is pending: kicked_off=%t status=%q awaiting=%t turns=%d (kickoff_error=%q)",
				i, got.KickedOff, got.Status, agents.KickoffAwaitingGrant(got), rt.callCount(), got.KickoffError)
		}
	}

	// muster restarts while the turn is held.
	second := process("held-test/after-restart")
	if got := tick(second); got.KickedOff || !agents.KickoffAwaitingGrant(got) || rt.callCount() != 0 {
		t.Fatalf("after the restart, still pending: kicked_off=%t awaiting=%t turns=%d",
			got.KickedOff, agents.KickoffAwaitingGrant(got), rt.callCount())
	}

	// The owner approves; ccd checks the repository out.
	rt.setCheckout(agents.CheckoutStatus{State: agents.CheckoutReady}, nil)
	got := tick(second)
	if !got.KickedOff || got.KickoffError != "" || got.KickoffAttempts != 1 || got.KickoffPod != "russet-tern-7c1-b4" || rt.callCount() != 1 {
		t.Fatalf("after the grant: kicked_off=%t kickoff_error=%q attempts=%d pod=%q turns=%d; want true/\"\"/1/the recipient/1",
			got.KickedOff, got.KickoffError, got.KickoffAttempts, got.KickoffPod, rt.callCount())
	}
	if agents.KickoffOwed(got) || agents.KickoffFailed(got) || agents.KickoffAwaitingGrant(got) {
		t.Fatalf("a delivered row reads owed=%t failed=%t awaiting=%t",
			agents.KickoffOwed(got), agents.KickoffFailed(got), agents.KickoffAwaitingGrant(got))
	}

	// Exactly once: the same process and a third one tick again.
	tick(second)
	tick(process("held-test/third"))
	if n := rt.callCount(); n != 1 {
		t.Fatalf("the first turn was sent %d times", n)
	}
	sess, err := store.LatestOrCreateSession(ctx, row.ID, row.Name)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	msgs, err := store.ListChatMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	var lines []string
	for _, m := range msgs {
		lines = append(lines, m.Role+": "+m.Content)
	}
	if want := "user: " + note + "\nassistant: 3 open pull requests"; strings.Join(lines, "\n") != want {
		t.Fatalf("transcript = %q, want %q", lines, want)
	}
}

// TestAFirstTurnHeldPastADayIsErroredInPostgres: the 24-hour end of the wait
// against the real conditional write (MarkKickoffStuck refuses a CLAIMED row, so
// the verdict must be written after the deliverer released its claim).
func TestAFirstTurnHeldPastADayIsErroredInPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := agents.NewPG(pool)
	prefix := "heldday-" + time.Now().Format("150405") + "-"
	row, err := store.Create(ctx, agents.Agent{
		Name: "slate-curlew", Namespace: prefix + "slate-curlew", Status: agents.StatusRunning,
		Kind: agents.KindClaudeCode, CCAccount: "acct-j8", Repo: "example-org/dune-map",
		PendingNote: "map the dune-map build", HooksToken: "tok-slate-2f",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	if _, err := pool.Exec(ctx, `UPDATE agents SET updated_at = now() - interval '25 hours',
		kickoff_error = $2 WHERE id=$1`, row.ID, agents.KickoffAwaitingGrantReason+": approve it"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	rt := &fakeGateway{log: &recorder{}, reply: "never sent"}
	rt.setCheckout(agents.CheckoutStatus{State: agents.CheckoutAwaitingGrant}, nil)
	d, err := New(Config{Store: store, Gateway: rt, NamespacePrefix: prefix, Owner: "heldday/1",
		Instances: &fakeInstances{insts: []provision.Instance{readyInstance("slate-curlew", "slate-curlew-1aa-z0", 0)}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	d.Wait()
	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != agents.StatusError || !strings.Contains(got.ErrorMessage, "the github grant was not approved") ||
		got.KickedOff || rt.callCount() != 0 {
		t.Fatalf("status=%q error_message=%q kicked_off=%t turns=%d; want error naming the grant, nothing sent",
			got.Status, got.ErrorMessage, got.KickedOff, rt.callCount())
	}
}
