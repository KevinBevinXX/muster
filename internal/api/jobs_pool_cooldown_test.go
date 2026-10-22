package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// THE RATE-LIMIT COOLDOWN AND THE PER-ACCOUNT CAP IN THE WORKER CHOICE (M4).
// ---------------------------------------------------------------------------

// TestChooseWorkerSkipsAWorkerWhoseAccountIsCoolingDown: a-cooling wins every
// ranking rule (idle, nothing queued, rate-limited longest ago, first by name),
// but its account is still cooling down, so the job goes to the busy b-ok.
func TestChooseWorkerSkipsAWorkerWhoseAccountIsCoolingDown(t *testing.T) {
	members := []agents.Agent{worker(1, "a-cooling", "alpha"), worker(2, "b-ok", "beta")}
	accounts := map[string]ccpool.AccountState{
		"alpha": {RateLimitedAt: t0900, CoolingUntil: t1000.Add(time.Hour)},
		"beta":  {RateLimitedAt: t1000},
	}
	active := map[int64]jobs.ActiveCount{2: {Running: 1, Queued: 2}}
	got, why, full := chooseWorker("reviewers", members, members, "", accounts, active, nil)
	if why != "" || full || got.Name != "b-ok" {
		t.Fatalf("chose %q (%q, full %v), want b-ok: a-cooling's account is cooling down", got.Name, why, full)
	}
	// The control: the window over (CoolingUntil zero), a-cooling wins again.
	accounts["alpha"] = ccpool.AccountState{RateLimitedAt: t0900}
	if got, _, _ := chooseWorker("reviewers", members, members, "", accounts, active, nil); got.Name != "a-cooling" {
		t.Fatalf("with alpha's cooldown over chose %q, want a-cooling", got.Name)
	}
}

// TestChooseWorkerCapsTheJobsAnAccountRunsAtOnce: alpha's cap is 1, and
// x-other — a worker of ANOTHER pool on alpha — is running a job, so the idle
// a-idle (which wins every ranking rule) would be alpha's second concurrent
// job, and is skipped. b-busy is already running a job: one more queues behind
// it on the same worker, so it adds nothing to beta's concurrency and is
// allowed at beta's cap of 1.
func TestChooseWorkerCapsTheJobsAnAccountRunsAtOnce(t *testing.T) {
	other := worker(9, "x-other", "alpha")
	other.WorkerPool = "other"
	members := []agents.Agent{worker(1, "a-idle", "alpha"), worker(2, "b-busy", "beta")}
	peers := append(append([]agents.Agent(nil), members...), other)
	accounts := map[string]ccpool.AccountState{
		"alpha": {MaxConcurrentJobs: 1},
		"beta":  {MaxConcurrentJobs: 1, RateLimitedAt: t1000},
	}
	active := map[int64]jobs.ActiveCount{9: {Running: 1}, 2: {Running: 1}}
	got, why, full := chooseWorker("reviewers", members, peers, "", accounts, active, nil)
	if why != "" || full || got.Name != "b-busy" {
		t.Fatalf("chose %q (%q, full %v), want b-busy: alpha already runs its 1 concurrent job on x-other", got.Name, why, full)
	}
	// Controls: x-other idle, or alpha's cap 2 — a-idle wins again.
	if got, _, _ := chooseWorker("reviewers", members, peers, "", accounts, map[int64]jobs.ActiveCount{2: {Running: 1}}, nil); got.Name != "a-idle" {
		t.Fatalf("with x-other idle chose %q, want a-idle", got.Name)
	}
	accounts["alpha"] = ccpool.AccountState{MaxConcurrentJobs: 2}
	if got, _, _ := chooseWorker("reviewers", members, peers, "", accounts, active, nil); got.Name != "a-idle" {
		t.Fatalf("with alpha's cap at 2 chose %q, want a-idle", got.Name)
	}
}

// TestChooseWorkerIsFullWhenEveryAccountIsCoolingDownOrCapped: no worker is
// eligible, and every one was skipped for the cooldown or the cap alone, so
// the refusal is "full" — the job API's 429, not a 503 and not a silent pick.
func TestChooseWorkerIsFullWhenEveryAccountIsCoolingDownOrCapped(t *testing.T) {
	other := worker(9, "y-other", "beta")
	other.WorkerPool = "other"
	members := []agents.Agent{worker(1, "a-cooling", "alpha"), worker(2, "b-capped", "beta")}
	peers := append(append([]agents.Agent(nil), members...), other)
	accounts := map[string]ccpool.AccountState{
		"alpha": {MaxConcurrentJobs: 1, RateLimitedAt: t0900, CoolingUntil: t1000.Add(time.Hour)},
		"beta":  {MaxConcurrentJobs: 1},
	}
	_, why, full := chooseWorker("reviewers", members, peers, "", accounts, map[int64]jobs.ActiveCount{9: {Running: 1}}, nil)
	const want = `no worker in pool "reviewers" can take the job: ` +
		`a-cooling (its Claude account "alpha" is cooling down after a rate limit until 2026-10-20T11:00:00Z); ` +
		`b-capped (its Claude account "beta" is already running 1 job(s) at once, its limit)`
	if why != want {
		t.Fatalf("reason = %q\nwant      %q", why, want)
	}
	if !full {
		t.Fatal("every worker was skipped for its account's cooldown or cap, and the refusal is not full (want a 429)")
	}
}

// TestAPoolJobIs429WhileEveryWorkersAccountIsCoolingDown: through the real
// handler, both members' accounts cooling down is the pool-full 429 with the
// reason, recording nothing; once the windows are over the same submission is
// accepted.
func TestAPoolJobIs429WhileEveryWorkersAccountIsCoolingDown(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	until := t1000.Add(time.Hour)
	kinds := &poolKinds{states: map[string]ccpool.AccountState{
		"alpha": {RateLimitedAt: t0900, CoolingUntil: until, MaxConcurrentJobs: 2},
		"beta":  {RateLimitedAt: t0900, CoolingUntil: until, MaxConcurrentJobs: 2},
	}}
	f.s.ext.Kinds = kinds
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
	const want = `no worker in pool "reviewers" can take the job: ` +
		`rev-a (its Claude account "alpha" is cooling down after a rate limit until 2026-10-20T11:00:00Z); ` +
		`rev-b (its Claude account "beta" is cooling down after a rate limit until 2026-10-20T11:00:00Z)`
	if code != http.StatusTooManyRequests || out["error"] != want {
		t.Fatalf("every account cooling down = %d %v, want 429 %q", code, out, want)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d jobs recorded, want 0", n)
	}
	kinds.mu.Lock()
	kinds.states = map[string]ccpool.AccountState{"alpha": {MaxConcurrentJobs: 2}, "beta": {MaxConcurrentJobs: 2}}
	kinds.mu.Unlock()
	if out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "x"})); out["agent"] != "rev-a" {
		t.Fatalf("with the cooldowns over the job ran on %v, want rev-a", out["agent"])
	}
}

// TestAPoolJobCountsTheAccountsJobsOutsideThePool: the seam between the
// handler and the choice. keen-wren is NOT in the pool, but runs on alpha —
// rev-a's account — and is holding a job. With alpha's cap at 1, the pool job
// must go to rev-b although rev-a wins on name: the handler has to hand the
// choice every worker on the account, not only the pool's members.
func TestAPoolJobCountsTheAccountsJobsOutsideThePool(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.release = make(chan struct{})
	defer close(f.gw.release)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {MaxConcurrentJobs: 1}, "beta": {MaxConcurrentJobs: 1}}}
	f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "hold alpha"})
	id := f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"})
	if got := f.store.row(id); got.AgentID != 21 {
		t.Fatalf("the pool job went to agent %d, want 21 (rev-b): alpha already runs its 1 job on keen-wren", got.AgentID)
	}
}
