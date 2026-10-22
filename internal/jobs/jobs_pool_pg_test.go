package jobs

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestThePGStoreCountsActiveJobsAndFindsAKey: ActiveCounts splits each named
// agent's unfinished jobs into queued and running — an agent whose jobs are all
// finished, an agent not asked about, and another owner's agent are all absent
// (row-level security). ByIdempotencyKey answers the caller's job, ErrNotFound
// for another owner's key or none. The worker_pool a job was submitted to is
// read back, and so is a sha job's pull request; a negative one is refused.
func TestThePGStoreCountsActiveJobsAndFindsAKey(t *testing.T) {
	s, pool := pgStore(t)
	a, b, c := mustAgent(t, pool, 1, "job-count-a"), mustAgent(t, pool, 1, "job-count-b"), mustAgent(t, pool, 1, "job-count-c")
	m := mustAgent(t, pool, 2, "job-count-m")
	mk := func(uid int64, ag agents.Agent, key, workerPool string) Job {
		t.Helper()
		j, _, err := s.Create(as(uid), NewJob{AgentID: ag.ID, Agent: ag.Name, WorkerPool: workerPool, Prompt: "p",
			TimeoutSeconds: 60, IdempotencyKey: key})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return j
	}
	a1 := mk(1, a, "count-a1", "reviewers")
	withPR, _, err := s.Create(as(1), NewJob{AgentID: c.ID, Agent: c.Name, Prompt: "p", Repo: "acme/widgets", SHA: fullSHA,
		PR: 7, TimeoutSeconds: 60, IdempotencyKey: "count-pr"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(as(1), withPR.ID); err != nil || got.PR != 7 || got.SHA != fullSHA {
		t.Fatalf("a sha job read back with pr %d (sha %q), %v; want 7", got.PR, got.SHA, err)
	}
	if _, _, err := s.Create(as(1), NewJob{AgentID: c.ID, Agent: c.Name, Prompt: "p", PR: -1, TimeoutSeconds: 60}); err == nil ||
		!strings.Contains(err.Error(), "jobs_pr_check") {
		t.Fatalf("pr -1: %v, want jobs_pr_check", err)
	}
	mk(1, a, "", "")
	a3 := mk(1, a, "", "")
	b1 := mk(1, b, "", "")
	mk(1, c, "", "")
	mk(2, m, "count-m1", "")
	for _, id := range []int64{a1.ID, b1.ID} {
		if ok, err := s.Start(as(1), id, "alpha", 0); !ok || err != nil {
			t.Fatalf("start %d: %v %v", id, ok, err)
		}
	}
	for _, id := range []int64{a3.ID, b1.ID} {
		if ok, err := s.Finish(as(1), id, Failed("x", "y")); !ok || err != nil {
			t.Fatalf("finish %d: %v %v", id, ok, err)
		}
	}
	got, err := s.ActiveCounts(as(1), []int64{a.ID, b.ID, m.ID})
	// (c's jobs, the pr one included, are not asked about.)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[a.ID] != (ActiveCount{Queued: 1, Running: 1}) {
		t.Fatalf("ActiveCounts = %v, want only %d: {Queued:1 Running:1}", got, a.ID)
	}
	if got, err := s.ActiveCounts(as(1), nil); err != nil || len(got) != 0 {
		t.Fatalf("ActiveCounts(nil) = %v, %v", got, err)
	}
	j, err := s.ByIdempotencyKey(as(1), "count-a1")
	if err != nil || j.ID != a1.ID || j.WorkerPool != "reviewers" || j.Agent != "job-count-a" {
		t.Fatalf("ByIdempotencyKey = %+v, %v; want job %d in pool reviewers on job-count-a", j, err, a1.ID)
	}
	for _, key := range []string{"count-m1", "no-such-key"} {
		if _, err := s.ByIdempotencyKey(as(1), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("key %q = %v, want ErrNotFound", key, err)
		}
	}
}

// TestAWorkerPoolIsAClaudeCodeAgentsOnly: migration 0041 stores a claude-code
// agent's worker_pool and refuses one on a gateway agent or outside the
// pattern; a job's worker_pool outside the pattern is refused too.
func TestAWorkerPoolIsAClaudeCodeAgentsOnly(t *testing.T) {
	s, pool := pgStore(t)
	st := agents.NewPG(pool)
	w, err := st.Create(as(1), agents.Agent{Name: "job-worker", Namespace: "devpod-job-worker", DisplayName: "w",
		Status: agents.StatusRunning, HooksToken: "hooks-job-worker", Kind: agents.KindClaudeCode, CCAccount: "alpha",
		WorkerPool: "reviewers"})
	if err != nil {
		t.Fatalf("a claude-code worker: %v", err)
	}
	if got, err := st.Get(as(1), w.ID); err != nil || got.WorkerPool != "reviewers" {
		t.Fatalf("read back = %q, %v; want reviewers", got.WorkerPool, err)
	}
	for _, a := range []agents.Agent{
		{Name: "job-gw-worker", Kind: agents.KindGateway, WorkerPool: "reviewers"},
		{Name: "job-bad-pool", Kind: agents.KindClaudeCode, CCAccount: "alpha", WorkerPool: "-bad"},
	} {
		a.Namespace, a.DisplayName, a.Status, a.HooksToken = "devpod-"+a.Name, a.Name, agents.StatusRunning, "hooks-"+a.Name
		if _, err := st.Create(as(1), a); err == nil || !strings.Contains(err.Error(), "agents_worker_pool_check") {
			t.Errorf("%s: %v, want agents_worker_pool_check", a.Name, err)
		}
	}
	_, _, err = s.Create(as(1), NewJob{AgentID: w.ID, Agent: w.Name, WorkerPool: "Bad Pool", Prompt: "p", TimeoutSeconds: 60})
	if err == nil || !strings.Contains(err.Error(), "jobs_worker_pool_check") {
		t.Fatalf("a job in pool %q: %v, want jobs_worker_pool_check", "Bad Pool", err)
	}
}

// TestCreateChosenRecordsOnlyACandidateAndAnswersAKeyWithoutChoosing: the
// choice is given each candidate's unfinished jobs (an agent with none absent)
// and recorded; a choice outside the candidates, or one that changes the key,
// records nothing; an error from the choice comes back as itself; and a
// repeated key is answered without choosing again.
func TestCreateChosenRecordsOnlyACandidateAndAnswersAKeyWithoutChoosing(t *testing.T) {
	s, pool := pgStore(t)
	a, b, other := mustAgent(t, pool, 1, "job-chosen-a"), mustAgent(t, pool, 1, "job-chosen-b"), mustAgent(t, pool, 1, "job-chosen-x")
	if _, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	base := NewJob{WorkerPool: "reviewers", Prompt: "p", TimeoutSeconds: 60, IdempotencyKey: "chosen-1"}
	on := func(ag agents.Agent, key string) NewJob {
		n := base
		n.AgentID, n.Agent, n.IdempotencyKey = ag.ID, ag.Name, key
		return n
	}
	var seen map[int64]ActiveCount
	j, created, err := s.CreateChosen(as(1), base, []int64{a.ID, b.ID}, func(active map[int64]ActiveCount) (NewJob, error) {
		seen = active
		return on(b, "chosen-1"), nil
	})
	if err != nil || !created || j.AgentID != b.ID || j.WorkerPool != "reviewers" {
		t.Fatalf("chosen = agent %d pool %q created %v err %v; want a new job on %d", j.AgentID, j.WorkerPool, created, err, b.ID)
	}
	if len(seen) != 1 || seen[a.ID] != (ActiveCount{Queued: 1}) {
		t.Fatalf("the choice saw %v, want only %d: {Queued:1}", seen, a.ID)
	}
	called := false
	again, created, err := s.CreateChosen(as(1), base, []int64{a.ID, b.ID}, func(map[int64]ActiveCount) (NewJob, error) {
		called = true
		return on(a, "chosen-1"), nil
	})
	if err != nil || created || again.ID != j.ID || called {
		t.Fatalf("a repeated key = id %d created %v err %v chose %v; want id %d, not chosen again", again.ID, created, err, called, j.ID)
	}
	fresh := base
	fresh.IdempotencyKey = "chosen-2"
	for name, pick := range map[string]NewJob{
		"an agent not among the candidates": on(other, "chosen-2"),
		"a changed key":                     on(a, "chosen-other"),
	} {
		if _, _, err := s.CreateChosen(as(1), fresh, []int64{a.ID, b.ID}, func(map[int64]ActiveCount) (NewJob, error) {
			return pick, nil
		}); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	refusal := errors.New("every worker is full")
	if _, _, err := s.CreateChosen(as(1), fresh, []int64{a.ID, b.ID}, func(map[int64]ActiveCount) (NewJob, error) {
		return NewJob{}, refusal
	}); err != refusal {
		t.Fatalf("the choice's error came back as %v, want it unchanged", err)
	}
	got, err := s.ActiveCounts(as(1), []int64{a.ID, b.ID, other.ID})
	if err != nil || len(got) != 2 || got[a.ID].Active() != 1 || got[b.ID].Active() != 1 {
		t.Fatalf("after the refusals: %v, %v; want one job each on a and b and none on x", got, err)
	}
}
