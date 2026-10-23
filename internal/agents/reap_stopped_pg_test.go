package agents

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// The stopped-agent reaper's store half (reap_stopped.go) and migration 0045's
// stopped_at trigger, on real Postgres.
//
// 🔴 FIXTURE AGES OVERSHOOT AND UNDERSHOOT THE WINDOW, NEVER SIT ON IT. The
// window is 336h; a due row is aged 15 days and a not-yet-due one 13, so a
// comparison that is off by a day in either direction (or an inverted one)
// moves a row across the line, and no test depends on which side of an exact
// microsecond boundary a row lands.

const reapWindow = 14 * 24 * time.Hour

type reapFixture struct {
	ctx   context.Context
	store *PGStore
	stamp string
}

func newReapFixture(t *testing.T) *reapFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	t.Cleanup(cancel)
	return &reapFixture{ctx: ctx, store: requirePGStore(t, ctx), stamp: time.Now().Format("150405.000000")}
}

// created inserts a row the way every agent row is inserted: `provisioning`
// (a gateway agent, or a claude-code pool worker when pool is set).
func (f *reapFixture) created(t *testing.T, name, pool string) Agent {
	t.Helper()
	a := Agent{Name: name + "-" + f.stamp, Namespace: "ns-" + name, Status: StatusProvisioning}
	if pool != "" {
		a.Kind, a.CCAccount, a.WorkerPool = KindClaudeCode, "acct", pool
	}
	got, err := f.store.Create(f.ctx, a)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { cleanupAgent(f.ctx, f.store.pool, got.ID) })
	return got
}

// agent is an agent that RAN — created, brought up (`provisioning`, then
// `running`, as a dispatch or a Start writes them) — and is now in status.
func (f *reapFixture) agent(t *testing.T, name, status, pool string) Agent {
	t.Helper()
	a := f.created(t, name, pool)
	for _, st := range []string{StatusProvisioning, StatusRunning, status} {
		if err := f.store.UpdateStatus(f.ctx, a.ID, st, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// saved is an agent saved for later and never started: inserted, then
// `stopped` with no instance built (Dispatch without a kickoff).
func (f *reapFixture) saved(t *testing.T, name string) Agent {
	t.Helper()
	a := f.created(t, name, "")
	if err := f.store.UpdateStatus(f.ctx, a.ID, StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	return a
}

// age moves agent id's stopped_at to `days` days before the database's now.
// It does not write status, so 0045's trigger (UPDATE OF status) is not run.
func (f *reapFixture) age(t *testing.T, id int64, days int) {
	t.Helper()
	if _, err := f.store.pool.Exec(f.ctx, `UPDATE agents SET stopped_at = now() - make_interval(days => $2) WHERE id = $1`, id, days); err != nil {
		t.Fatal(err)
	}
}

// stoppedAge is how long before the database's now agent id's stopped_at is
// (and whether it has one).
func (f *reapFixture) stoppedAge(t *testing.T, id int64) (time.Duration, bool) {
	t.Helper()
	var secs *float64
	if err := f.store.pool.QueryRow(f.ctx, `SELECT extract(epoch FROM now() - stopped_at)::float8 FROM agents WHERE id = $1`, id).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs == nil {
		return 0, false
	}
	return time.Duration(*secs * float64(time.Second)), true
}

// listed is the subset of ids ListReapableStopped returns (the package's
// database is shared with other tests, so only this test's rows are read).
func (f *reapFixture) listed(t *testing.T, ids ...int64) map[int64]StoppedAgent {
	t.Helper()
	all, err := f.store.ListReapableStopped(f.ctx, reapWindow)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := map[int64]StoppedAgent{}
	for _, a := range all {
		if want[a.ID] {
			out[a.ID] = a
		}
	}
	return out
}

func (f *reapFixture) claim(t *testing.T, id int64) bool {
	t.Helper()
	won, err := f.store.ClaimStoppedReap(f.ctx, id, reapWindow)
	if err != nil {
		t.Fatal(err)
	}
	return won
}

// TestOnlyAStoppedNonPoolAgentPastTheWindowIsReapable: of rows all carrying a
// stopped_at 15 days old, only the one whose stored status is `stopped` and
// that is not a pool worker is listed and claimable; a stopped row 13 days old
// is neither. Each other row was stopped (so the trigger stamped it), aged, and
// then moved to its status, so it carries the same old stopped_at as the due
// one and the STATUS is the only difference.
func TestOnlyAStoppedNonPoolAgentPastTheWindowIsReapable(t *testing.T) {
	f := newReapFixture(t)
	due := f.agent(t, "reap-due", StatusStopped, "")
	young := f.agent(t, "reap-young", StatusStopped, "")
	poolWorker := f.agent(t, "reap-pool", StatusStopped, "reviewers")
	f.age(t, due.ID, 15)
	f.age(t, young.ID, 13)
	f.age(t, poolWorker.ID, 15)
	others := map[string]Agent{}
	for _, st := range []string{StatusRunning, StatusError, StatusProvisioning, StatusPending} {
		a := f.agent(t, "reap-"+st, StatusStopped, "")
		f.age(t, a.ID, 15)
		if err := f.store.UpdateStatus(f.ctx, a.ID, st, "", ""); err != nil {
			t.Fatal(err)
		}
		if age, ok := f.stoppedAge(t, a.ID); !ok || age < 14*24*time.Hour {
			t.Fatalf("fixture: the %s row's stopped_at is not the old one (%v, %v): the status would not be the only difference", st, age, ok)
		}
		others[st] = a
	}

	ids := []int64{due.ID, young.ID, poolWorker.ID}
	for _, a := range others {
		ids = append(ids, a.ID)
	}
	got := f.listed(t, ids...)
	if _, ok := got[due.ID]; !ok || len(got) != 1 {
		t.Fatalf("listed %v, want exactly the due stopped agent %d", got, due.ID)
	}
	if !f.claim(t, due.ID) {
		t.Fatal("the due stopped agent could not be claimed")
	}
	if f.claim(t, young.ID) {
		t.Error("a stopped agent 13 days into a 14-day window was claimed")
	}
	for st, a := range others {
		if f.claim(t, a.ID) {
			t.Errorf("an agent in status %s was claimed", st)
		}
	}
	if f.claim(t, poolWorker.ID) {
		t.Error("a stopped pool worker was claimed")
	}
}

// TestStartResetsTheStoppedClock: a stopped agent 15 days into its window is
// Started (the provisioner writes `provisioning`, then `running`) and stopped
// again. It now carries a stopped_at from moments ago, is not listed, and a
// claim loses. CONTROL: re-persisting
// `stopped` over a stopped row (the reconciler's write) does NOT move the clock.
func TestStartResetsTheStoppedClock(t *testing.T) {
	f := newReapFixture(t)
	a := f.agent(t, "reap-restart", StatusStopped, "")
	f.age(t, a.ID, 15)

	if err := f.store.UpdateStatus(f.ctx, a.ID, StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	before := f.listed(t, a.ID)
	if _, ok := before[a.ID]; !ok {
		t.Fatal("control: re-persisting `stopped` over a stopped row reset its clock")
	}

	for _, st := range []string{StatusProvisioning, StatusRunning, StatusStopped} {
		if err := f.store.UpdateStatus(f.ctx, a.ID, st, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if age, ok := f.stoppedAge(t, a.ID); !ok || age < 0 || age > time.Minute {
		t.Fatalf("after Start then Stop, stopped_at is %v old (set %v); want a fresh stamp", age, ok)
	}
	if got := f.listed(t, a.ID); len(got) != 0 {
		t.Fatalf("a restarted and re-stopped agent is listed: %v", got)
	}
	if f.claim(t, a.ID) {
		t.Fatal("a restarted and re-stopped agent was claimed")
	}
}

// TestAConcurrentRestartBeatsTheClaim: the scan reads a due agent, then it is
// Started before the claim: the claim loses while it is provisioning or running,
// and still loses once it is stopped again (a new stopped_at).
func TestAConcurrentRestartBeatsTheClaim(t *testing.T) {
	f := newReapFixture(t)
	a := f.agent(t, "reap-race", StatusStopped, "")
	f.age(t, a.ID, 15)
	if _, ok := f.listed(t, a.ID)[a.ID]; !ok {
		t.Fatal("control: the due agent is not listed")
	}
	for _, st := range []string{StatusProvisioning, StatusRunning, StatusStopped} {
		if err := f.store.UpdateStatus(f.ctx, a.ID, st, "", ""); err != nil {
			t.Fatal(err)
		}
		if f.claim(t, a.ID) {
			t.Fatalf("the claim won over a Start, with the row %s", st)
		}
	}
}

// TestOnlyOneClaimWins: eight claims of one due agent at once — exactly one
// wins; a later claim inside the lease loses; once the claim is older than the
// lease, it can be claimed again.
func TestOnlyOneClaimWins(t *testing.T) {
	f := newReapFixture(t)
	a := f.agent(t, "reap-claim", StatusStopped, "")
	f.age(t, a.ID, 15)
	if _, ok := f.listed(t, a.ID)[a.ID]; !ok {
		t.Fatal("control: the due agent is not listed")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			won, err := f.store.ClaimStoppedReap(f.ctx, a.ID, reapWindow)
			if err != nil {
				t.Error(err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d of 8 concurrent claims won, want exactly 1", wins)
	}
	if f.claim(t, a.ID) {
		t.Fatal("a claim inside the lease won")
	}
	if got := f.listed(t, a.ID); len(got) != 0 {
		t.Fatalf("a claimed agent is still listed: %v", got)
	}
	if _, err := f.store.pool.Exec(f.ctx, `UPDATE agents SET reap_claimed_at = now() - make_interval(secs => $2) WHERE id = $1`,
		a.ID, (StoppedReapClaimLease + 30*time.Minute).Seconds()); err != nil {
		t.Fatal(err)
	}
	if !f.claim(t, a.ID) {
		t.Fatal("a claim older than the lease could not be taken over")
	}
}

// TestANeverStartedAgentIsNotReapable: two stopped agents, both 15 days into
// the window — one saved for later and never started, one that ran and was
// stopped. Only the one that ran is listed and claimable.
func TestANeverStartedAgentIsNotReapable(t *testing.T) {
	f := newReapFixture(t)
	saved := f.saved(t, "reap-saved")
	ran := f.agent(t, "reap-ran", StatusStopped, "")
	f.age(t, saved.ID, 15)
	f.age(t, ran.ID, 15)
	got := f.listed(t, saved.ID, ran.ID)
	if _, ok := got[saved.ID]; ok {
		t.Error("a never-started (saved) agent is listed for reaping")
	}
	if _, ok := got[ran.ID]; !ok {
		t.Error("control: a started-then-stopped agent is not listed")
	}
	if f.claim(t, saved.ID) {
		t.Error("a never-started (saved) agent was claimed")
	}
	if !f.claim(t, ran.ID) {
		t.Error("control: a started-then-stopped agent could not be claimed")
	}
}
