package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// WORKER POOLS (jobs.go pickWorker / chooseWorker).
// ---------------------------------------------------------------------------

// worker is a pool member for chooseWorker's table.
func worker(id int64, name, account string) agents.Agent {
	return agents.Agent{ID: id, Name: name, Kind: agents.KindClaudeCode, CCAccount: account, Repo: "acme/widgets",
		Status: agents.StatusRunning, WorkerPool: "reviewers"}
}

var (
	t0900 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	t1000 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
)

// TestChooseWorkerPrefersTheIdleThenTheLeastQueuedThenTheMostRested pins the
// order with literal answers, each case built so that the rule it names is the
// ONLY one that decides it (the runner-up wins on every later rule).
func TestChooseWorkerPrefersTheIdleThenTheLeastQueuedThenTheMostRested(t *testing.T) {
	healthy := map[string]ccpool.AccountState{"alpha": {}, "beta": {}, "gamma": {}}
	for _, tc := range []struct {
		name     string
		members  []agents.Agent
		accounts map[string]ccpool.AccountState
		active   map[int64]jobs.ActiveCount
		want     string
	}{
		{"an idle worker beats a busy one that is better on every later rule",
			[]agents.Agent{worker(1, "a-busy", "alpha"), worker(2, "b-idle", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Running: 1}}, "b-idle"},
		{"of two busy workers, the fewer queued",
			[]agents.Agent{worker(1, "a-two", "alpha"), worker(2, "b-one", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Running: 1, Queued: 2}, 2: {Running: 1, Queued: 1}}, "b-one"},
		{"a queued-only worker is not idle",
			[]agents.Agent{worker(1, "a-queued", "alpha"), worker(2, "b-idle", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Queued: 1}}, "b-idle"},
		{"of two idle workers, the never rate-limited account",
			[]agents.Agent{worker(1, "a-limited", "alpha"), worker(2, "b-never", "beta")},
			map[string]ccpool.AccountState{"alpha": {RateLimitedAt: t0900}, "beta": {}},
			nil, "b-never"},
		{"of two rate-limited accounts, the longer ago",
			[]agents.Agent{worker(1, "a-recent", "alpha"), worker(2, "b-older", "beta")},
			map[string]ccpool.AccountState{"alpha": {RateLimitedAt: t1000}, "beta": {RateLimitedAt: t0900}},
			nil, "b-older"},
		{"otherwise the name, whatever the input order",
			[]agents.Agent{worker(2, "z-last", "beta"), worker(1, "a-first", "alpha"), worker(3, "m-mid", "gamma")},
			healthy, nil, "a-first"},
		{"an auth-failed account is skipped, though it would win every rule",
			[]agents.Agent{worker(1, "a-dead", "alpha"), worker(2, "b-ok", "beta")},
			map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{2: {Running: 1, Queued: 2}}, "b-ok"},
		{"a worker at its limit is skipped",
			[]agents.Agent{worker(1, "a-full", "alpha"), worker(2, "b-room", "beta")},
			// a-full, with none queued, would win on "fewest queued".
			healthy, map[int64]jobs.ActiveCount{1: {Running: 4}, 2: {Running: 1, Queued: 1}}, "b-room"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := chooseWorker("reviewers", tc.members, "", tc.accounts, tc.active)
			if why != "" || got.Name != tc.want {
				t.Fatalf("chose %q (%q), want %q", got.Name, why, tc.want)
			}
		})
	}
}

// TestChooseWorkerSaysWhyNoWorkerIsEligible: each kind of ineligibility, named
// per worker, in name order.
func TestChooseWorkerSaysWhyNoWorkerIsEligible(t *testing.T) {
	stopped := worker(2, "b-stopped", "beta")
	stopped.Status = agents.StatusStopped
	gw := worker(3, "c-gateway", "")
	gw.Kind = agents.KindGateway
	other := worker(4, "d-other", "beta")
	other.Repo = "acme/other"
	members := []agents.Agent{worker(6, "f-full", "beta"), other, gw, stopped, worker(1, "a-dead", "alpha"), worker(5, "e-unknown", "zeta")}
	accounts := map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {}}
	_, why := chooseWorker("reviewers", members, "acme/widgets", accounts, map[int64]jobs.ActiveCount{6: {Running: 1, Queued: 3}})
	const want = `no worker in pool "reviewers" can take the job: a-dead (its Claude account "alpha" failed authentication); ` +
		`b-stopped (status stopped); c-gateway (kind gateway); d-other (works on repository "acme/other"); ` +
		`e-unknown (its Claude account "zeta" is not configured); f-full (already has 4 jobs queued or running (the limit))`
	if why != want {
		t.Fatalf("reason = %q\nwant      %q", why, want)
	}
}

// poolKinds is AgentKinds with the account marks a pool job reads.
type poolKinds struct {
	AgentKinds
	states map[string]ccpool.AccountState
	owners []int64
}

func (k *poolKinds) ClaudeAccountStates(_ context.Context, owner int64) (map[string]ccpool.AccountState, error) {
	k.owners = append(k.owners, owner)
	return k.states, nil
}

// The pool members the HTTP tests use: two running workers of "reviewers" on
// accounts alpha and beta.
func init() {
	jobAgentRows["rev-a"] = agents.Agent{ID: 20, Name: "rev-a", Kind: agents.KindClaudeCode, CCAccount: "alpha",
		Repo: "acme/widgets", Status: agents.StatusRunning, WorkerPool: "reviewers"}
	jobAgentRows["rev-b"] = agents.Agent{ID: 21, Name: "rev-b", Kind: agents.KindClaudeCode, CCAccount: "beta",
		Repo: "acme/widgets", Status: agents.StatusRunning, WorkerPool: "reviewers"}
}

// TestAPoolJobRunsOnTheChosenWorkerAndRecordsIt: through the real handler, a
// job submitted to a pool runs on the member chosen (rev-a: both idle, both
// accounts rested, so the name decides) and the job records both the pool and
// the worker; with rev-a's account auth-failed, the next goes to rev-b. The
// account marks are read for the service tier's user.
func TestAPoolJobRunsOnTheChosenWorkerAndRecordsIt(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	kinds := &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.s.ext.Kinds = kinds
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"}))
	if out["status"] != jobs.StatusSucceeded || out["agent"] != "rev-a" || out["worker_pool"] != "reviewers" || out["cc_account"] != "alpha" {
		t.Fatalf("job = %v on %v (pool %v, account %v), want succeeded on rev-a in reviewers on alpha", out["status"], out["agent"], out["worker_pool"], out["cc_account"])
	}
	if got := f.store.row(1); got.AgentID != 20 || got.WorkerPool != "reviewers" {
		t.Fatalf("stored agent %d pool %q, want 20 reviewers", got.AgentID, got.WorkerPool)
	}
	kinds.states = map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {}}
	if out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"})); out["agent"] != "rev-b" {
		t.Fatalf("with alpha auth-failed the job ran on %v, want rev-b", out["agent"])
	}
	if len(kinds.owners) != 2 || kinds.owners[0] != 1 || kinds.owners[1] != 1 {
		t.Fatalf("account marks read for owners %v, want [1 1] (the service tier's user)", kinds.owners)
	}
}

// TestAPoolJobIsRefusedWhenNoWorkerCanTakeIt: every member's account
// auth-failed is 503 with the reason, recording nothing; a deployment that
// cannot read account marks is 503 too; a REPEATED key is still answered
// with its first job, though its pool now has no eligible worker.
func TestAPoolJobIsRefusedWhenNoWorkerCanTakeIt(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
	if code != http.StatusServiceUnavailable || !strings.Contains(out["error"].(string), "cannot read its Claude accounts' marks") {
		t.Fatalf("with no account marks = %d %v, want 503", code, out)
	}
	kinds := &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.s.ext.Kinds = kinds
	first := f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "x", "idempotency_key": "pool-1"})
	f.await(t, first)

	kinds.states = map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {AuthFailed: true}}
	code, out = f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
	const want = `no worker in pool "reviewers" can take the job: rev-a (its Claude account "alpha" failed authentication); ` +
		`rev-b (its Claude account "beta" failed authentication)`
	if code != http.StatusServiceUnavailable || out["error"] != want {
		t.Fatalf("all auth-failed = %d %v, want 503 %q", code, out, want)
	}
	if n := f.store.count(); n != 1 {
		t.Fatalf("%d jobs recorded, want 1", n)
	}
	code, out = f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x", "idempotency_key": "pool-1"}, true)
	if code != http.StatusOK || out["deduplicated"] != true || out["job_id"] != float64(first) {
		t.Fatalf("a repeated key with no eligible worker = %d %v, want 200 deduplicated job %d", code, out, first)
	}
}

// TestAgentAndWorkerPoolAreMutuallyExclusive: naming both is a 400 that says
// so, before anything is chosen or recorded — although each alone would be
// accepted.
func TestAgentAndWorkerPoolAreMutuallyExclusive(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.gw.release = make(chan struct{})
	defer close(f.gw.release)
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "worker_pool": "reviewers", "prompt": "x"}, true)
	const want = "agent and worker_pool are mutually exclusive: name an agent, or a pool to choose one from"
	if code != http.StatusBadRequest || out["error"] != want {
		t.Fatalf("both = %d %v, want 400 %q", code, out, want)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d jobs recorded, want 0", n)
	}
	f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "x"})
	f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "x"})
}
