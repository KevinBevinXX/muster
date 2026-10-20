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
		SHA: "0123abc", ResultPath: "out/r.md", TimeoutSeconds: 90, RequestedBy: "review-orchestrator"})
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
		got.Agent != "job-wren" || got.AgentID != a.ID || got.Repo != "acme/widgets" || got.SHA != "0123abc" ||
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

// TestFailInterruptedFailsEveryUnfinishedJob: queued and running rows of every
// owner become failed/interrupted; finished rows are untouched.
func TestFailInterruptedFailsEveryUnfinishedJob(t *testing.T) {
	s, pool := pgStore(t)
	a1 := mustAgent(t, pool, 1, "job-sweep-one")
	a2 := mustAgent(t, pool, 2, "job-sweep-two")
	mk := func(uid int64, a agents.Agent) Job {
		j, _, err := s.Create(as(uid), NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "p", TimeoutSeconds: 60})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	queued, running, done := mk(1, a1), mk(2, a2), mk(1, a1)
	if ok, err := s.Start(as(2), running.ID, "", 0); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	if ok, err := s.Start(as(1), done.ID, "", 0); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	if ok, err := s.Finish(as(1), done.ID, Outcome{Status: StatusSucceeded, Result: "r"}); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	n, err := s.FailInterrupted(authz.System(context.Background()))
	if err != nil || n != 2 {
		t.Fatalf("FailInterrupted = %d, %v; want 2", n, err)
	}
	for _, c := range []struct {
		uid    int64
		id     int64
		status string
		reason string
	}{{1, queued.ID, StatusFailed, ReasonInterrupted}, {2, running.ID, StatusFailed, ReasonInterrupted}, {1, done.ID, StatusSucceeded, ""}} {
		got, err := s.Get(as(c.uid), c.id)
		if err != nil || got.Status != c.status || got.Reason != c.reason {
			t.Fatalf("job %d = %s/%s (%v), want %s/%s", c.id, got.Status, got.Reason, err, c.status, c.reason)
		}
	}
	// A sweep that found something to fail cannot be raced by the runner's
	// late finish.
	if ok, _ := s.Finish(as(2), running.ID, Outcome{Status: StatusSucceeded, Result: "late"}); ok {
		t.Fatal("a late finish overwrote the sweep")
	}
	// The job record outlives its agent.
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM agents WHERE name = 'job-sweep-one'`)
	got, err := s.Get(as(1), queued.ID)
	if err != nil || got.AgentID != 0 || got.Agent != "job-sweep-one" {
		t.Fatalf("after the agent was deleted: %+v %v", got, err)
	}
}
