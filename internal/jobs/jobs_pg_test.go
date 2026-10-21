package jobs

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 is a
// member.
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
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE jobs`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'job-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaService})
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

// TestThePGStoreRoundTripsAJob: create → get → start → finish, each value read
// back as written, the result verbatim.
func TestThePGStoreRoundTripsAJob(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-wren")
	ctx := as(1)
	j, created, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review it", Repo: "acme/widgets",
		SHA: fullSHA, ResultPath: "out/r.md", TimeoutSeconds: 90, RequestedBy: "review-orchestrator"})
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	if j.Status != StatusQueued || j.HasResult || !j.StartedAt.IsZero() || !j.FinishedAt.IsZero() {
		t.Fatalf("a new job = %+v, want queued with no result and no timestamps", j)
	}
	ok, err := s.Start(ctx, j.ID, "alpha", 0)
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if ok, _ := s.Start(ctx, j.ID, "beta", 0); ok {
		t.Fatal("a running job started twice")
	}
	const result = " line one\n\ttwo — ✓\r\n"
	if ok, err := s.Finish(ctx, j.ID, Outcome{Status: StatusSucceeded, Result: result}); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	got, err := s.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || !got.HasResult || got.Result != result || got.CCAccount != "alpha" ||
		got.Agent != "job-wren" || got.AgentID != a.ID || got.Repo != "acme/widgets" || got.SHA != fullSHA ||
		got.ResultPath != "out/r.md" || got.TimeoutSeconds != 90 || got.RequestedBy != "review-orchestrator" ||
		got.Reason != "" || got.StartedAt.IsZero() || got.FinishedAt.IsZero() {
		t.Fatalf("read back %+v", got)
	}
	// A finished job does not finish again.
	if ok, _ := s.Finish(ctx, j.ID, Failed("x", "y")); ok {
		t.Fatal("a succeeded job was overwritten")
	}
}

// TestThePGStoreDedupsAnIdempotencyKeyPerOwner: the same key answers the first
// job for its owner; another owner's identical key is its own job.
func TestThePGStoreDedupsAnIdempotencyKeyPerOwner(t *testing.T) {
	s, pool := pgStore(t)
	a1 := mustAgent(t, pool, 1, "job-one")
	a2 := mustAgent(t, pool, 2, "job-two")
	first, created, err := s.Create(as(1), NewJob{AgentID: a1.ID, Agent: a1.Name, Prompt: "p", TimeoutSeconds: 60, IdempotencyKey: "k-1"})
	if err != nil || !created {
		t.Fatalf("first = %v, %v", created, err)
	}
	again, created, err := s.Create(as(1), NewJob{AgentID: a1.ID, Agent: a1.Name, Prompt: "different", TimeoutSeconds: 60, IdempotencyKey: "k-1"})
	if err != nil || created || again.ID != first.ID || again.Prompt != "p" {
		t.Fatalf("repeat = id %d created %v prompt %q err %v; want id %d, not created, the first prompt", again.ID, created, again.Prompt, err, first.ID)
	}
	other, created, err := s.Create(as(2), NewJob{AgentID: a2.ID, Agent: a2.Name, Prompt: "p", TimeoutSeconds: 60, IdempotencyKey: "k-1"})
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("another owner's key = id %d created %v err %v", other.ID, created, err)
	}
	// And the owners cannot see each other's jobs.
	if _, err := s.Get(as(2), first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 2 read user 1's job: %v", err)
	}
	// Nor name each other's agents.
	if _, _, err := s.Create(as(2), NewJob{AgentID: a1.ID, Agent: a1.Name, Prompt: "p", TimeoutSeconds: 60}); err == nil {
		t.Fatal("user 2 created a job on user 1's agent")
	}
}

// fullSHA is a commit id a job may name (jobs_sha_check, ValidSHA).
const fullSHA = "0123456789abcdef0123456789abcdef01234567"

// TestTheDatabaseRefusesAnAbbreviatedSHA: migration 0037's CHECK agrees with
// ValidSHA, so a short sha cannot be stored by a caller that skipped it.
func TestTheDatabaseRefusesAnAbbreviatedSHA(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-short-sha")
	if _, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", SHA: "0123abc", TimeoutSeconds: 60}); err == nil {
		t.Fatal("a 7-character sha was stored")
	}
	if _, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", SHA: fullSHA, TimeoutSeconds: 60}); err != nil {
		t.Fatalf("a 40-character sha was refused: %v", err)
	}
}

// TestReapOverdueFailsOnlyJobsPastTheirOwnDeadline: the reaper, across every
// owner, fails a running job only after started_at + timeout_s + ReapSlack and
// a queued one only after created_at + MaxQueueWait. A job another process is
// still running (a rolling update's old pod) is inside its deadline and is
// left alone — and that process's real outcome then lands.
func TestReapOverdueFailsOnlyJobsPastTheirOwnDeadline(t *testing.T) {
	s, pool := pgStore(t)
	a1 := mustAgent(t, pool, 1, "job-sweep-one")
	a2 := mustAgent(t, pool, 2, "job-sweep-two")
	mk := func(uid int64, a agents.Agent, timeout int) Job {
		j, _, err := s.Create(as(uid), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: timeout})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	start := func(uid int64, j Job) {
		if ok, err := s.Start(as(uid), j.ID, "", 0); !ok || err != nil {
			t.Fatalf("start %d: %v %v", j.ID, ok, err)
		}
	}
	queued, short, long, done, live := mk(1, a1, 60), mk(2, a2, 60), mk(1, a1, 2100), mk(1, a1, 60), mk(2, a2, 60)
	start(2, short)
	start(1, long)
	start(1, done)
	start(2, live)
	if ok, err := s.Finish(as(1), done.ID, Outcome{Status: StatusSucceeded, Result: "r"}); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	t0 := time.Now()
	status := func(uid int64, j Job) string {
		t.Helper()
		got, err := s.Get(as(uid), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status + "/" + got.Reason
	}
	// No principal: the reaper elevates itself. Each pass, at a chosen clock.
	reap := func(at time.Time, want int64) {
		t.Helper()
		n, err := s.ReapOverdue(context.Background(), at)
		if err != nil || n != want {
			t.Fatalf("ReapOverdue(t0%+v) = %d, %v; want %d", at.Sub(t0), n, err, want)
		}
	}
	// At boot of a new pod, with every job inside its deadline: nothing.
	reap(t0, 0)
	// Just inside the 60s jobs' deadline (60s + ReapSlack): still nothing.
	reap(t0.Add(60*time.Second+ReapSlack-30*time.Second), 0)
	// The live job's runner (the old pod) finishes now, and its result lands.
	if ok, err := s.Finish(as(2), live.ID, Outcome{Status: StatusSucceeded, Result: "the real review"}); !ok || err != nil {
		t.Fatalf("the old pod's finish = %v, %v; want it recorded", ok, err)
	}
	// Just past it: the abandoned 60s running job, and nothing else.
	reap(t0.Add(60*time.Second+ReapSlack+30*time.Second), 1)
	for _, c := range []struct {
		uid  int64
		j    Job
		want string
	}{
		{2, short, "failed/interrupted"}, {1, queued, "queued/"}, {1, long, "running/"},
		{1, done, "succeeded/"}, {2, live, "succeeded/"},
	} {
		if got := status(c.uid, c.j); got != c.want {
			t.Fatalf("after the running deadline, job %d = %s, want %s", c.j.ID, got, c.want)
		}
	}
	if got, _ := s.Get(as(2), short.ID); got.Error != AbandonedRunningError || got.FinishedAt.IsZero() {
		t.Fatalf("the reaped running job = error %q finished %v", got.Error, got.FinishedAt)
	}
	// Just inside the queue wait: the queued job stays; the 2100s job, whose
	// deadline is far earlier, goes.
	reap(t0.Add(MaxQueueWait-30*time.Second), 1)
	if got := status(1, queued); got != "queued/" {
		t.Fatalf("inside the queue wait, the queued job = %s", got)
	}
	// Just past it: the queued job, with the queued message.
	reap(t0.Add(MaxQueueWait+30*time.Second), 1)
	if got, _ := s.Get(as(1), queued.ID); got.Status+"/"+got.Reason != "failed/interrupted" || got.Error != AbandonedQueuedError {
		t.Fatalf("the reaped queued job = %s/%s %q", got.Status, got.Reason, got.Error)
	}
	// A late finish by a runner cannot overwrite the reaper.
	if ok, _ := s.Finish(as(2), short.ID, Outcome{Status: StatusSucceeded, Result: "late"}); ok {
		t.Fatal("a late finish overwrote the reaper")
	}
	// The job record outlives its agent.
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM agents WHERE name = 'job-sweep-one'`)
	got, err := s.Get(as(1), queued.ID)
	if err != nil || got.AgentID != 0 || got.Agent != "job-sweep-one" {
		t.Fatalf("after the agent was deleted: %+v %v", got, err)
	}
}

// TestCreateCapsTheJobsActiveOnOneAgent: at most MaxActivePerAgent jobs queued
// or running per agent, even under concurrent submissions; a repeated key is
// answered, not refused; another agent is unaffected; a finished job frees a
// place.
func TestCreateCapsTheJobsActiveOnOneAgent(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-cap")
	b := mustAgent(t, pool, 1, "job-cap-other")
	const tries = 12
	type res struct {
		j   Job
		err error
	}
	out := make(chan res, tries)
	for i := 0; i < tries; i++ {
		go func(i int) {
			j, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60,
				IdempotencyKey: "cap-" + string(rune('a'+i))})
			out <- res{j, err}
		}(i)
	}
	var accepted []Job
	refused := 0
	for i := 0; i < tries; i++ {
		r := <-out
		switch {
		case r.err == nil:
			accepted = append(accepted, r.j)
		case errors.Is(r.err, ErrAgentAtCapacity):
			refused++
		default:
			t.Fatalf("create: %v", r.err)
		}
	}
	if len(accepted) != 4 || refused != 8 {
		t.Fatalf("%d accepted, %d refused for capacity; want 4 and 8", len(accepted), refused)
	}
	// A retry of an accepted submission is answered, at capacity.
	again, created, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60,
		IdempotencyKey: accepted[0].IdempotencyKey})
	if err != nil || created || again.ID != accepted[0].ID {
		t.Fatalf("a repeated key at capacity = id %d created %v err %v; want id %d", again.ID, created, err, accepted[0].ID)
	}
	if _, _, err := s.Create(as(1), NewJob{AgentID: b.ID, Agent: b.Name, Prompt: "p", TimeoutSeconds: 60}); err != nil {
		t.Fatalf("another agent was refused: %v", err)
	}
	if ok, err := s.Finish(as(1), accepted[1].ID, Failed("x", "y")); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if _, created, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60}); err != nil || !created {
		t.Fatalf("after one finished = %v, %v; want a new job", created, err)
	}
	if _, _, err := s.Create(as(1), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60}); !errors.Is(err, ErrAgentAtCapacity) {
		t.Fatalf("back at capacity = %v, want ErrAgentAtCapacity", err)
	}
}
