package flows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/metrics"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members; 4 is a disabled member.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member'), (4, 'member')
		ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = now() WHERE id = 4`)
	// Each test starts from an empty queue: the tables are this package's own.
	dbtest.OwnerExec(t, dsn, `TRUNCATE flow_jobs, flow_runner_tokens, flow_requests`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'flow.request.%'`)
	return NewPG(pool), pool
}

// as is a request principal for user uid through door via.
func as(uid int64, via authz.Via) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: via})
}

func session(uid int64) context.Context { return as(uid, authz.ViaSession) }
func runner(uid int64) context.Context  { return as(uid, authz.ViaRunner) }

func pollReq(host, instance string) Request {
	return Request{Flow: "muse.poll", Host: host, Instance: instance, Params: map[string]string{"thread": "isolation canary"}}
}

func sendReq(host, instance string) Request {
	return Request{Flow: "muse.send", Host: host, Instance: instance, Params: map[string]string{"text": "hi"}}
}

func mustEnqueue(t *testing.T, s *PGStore, ctx context.Context, r Request) Job {
	t.Helper()
	j, err := s.Enqueue(ctx, r, "session", "test")
	if err != nil {
		t.Fatalf("enqueue %s: %v", r.Flow, err)
	}
	return j
}

func TestEnqueueIsOwnedAndInvisibleToAnotherOwner(t *testing.T) {
	s, pool := pgStore(t)
	j := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if j.OwnerID != 2 || j.Status != StatusQueued || j.Class != ClassRead || j.Params["thread"] != "isolation canary" {
		t.Fatalf("enqueued job = %+v", j)
	}
	if j.ExpiresAt.Sub(j.CreatedAt) < DefaultTTL-time.Second || j.ExpiresAt.Sub(j.CreatedAt) > DefaultTTL+time.Second {
		t.Fatalf("expires %s after creation; want the default ttl %s", j.ExpiresAt.Sub(j.CreatedAt), DefaultTTL)
	}
	if _, err := s.Get(session(3), j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 Get(user 2's job) = %v; want ErrNotFound", err)
	}
	if _, err := s.Get(session(2), j.ID); err != nil {
		t.Fatalf("control: the owner cannot Get its own job: %v", err)
	}
	// RAW SQL, under user 3's principal: the rows are not there for it at all.
	var n int
	if err := pool.QueryRow(session(3), `SELECT count(*) FROM flow_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user 3 sees %d flow_jobs rows (%v); want 0", n, err)
	}
	if err := pool.QueryRow(session(2), `SELECT count(*) FROM flow_jobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: user 2 sees %d rows (%v); want 1", n, err)
	}
	// And with NO principal, an insert is refused rather than landing as somebody.
	if _, err := pool.Exec(context.Background(), `INSERT INTO flow_jobs (flow, class, host, instance, requested_via, expires_at)
		VALUES ('muse.poll', 'read', 'laptop', 'personal', 'x', now() + interval '1 hour')`); err == nil {
		t.Fatal("an insert with no principal succeeded")
	}
}

// TestAnotherOwnersRunnerClaimsNothing is the cross-owner guarantee at the
// claim: user 3's runner on the SAME host and instance sees user 2's queued
// job as absent — through the store, and through raw SQL as that principal.
func TestAnotherOwnersRunnerClaimsNothing(t *testing.T) {
	s, pool := pgStore(t)
	j := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if _, ok, err := s.Claim(runner(3), "laptop", []string{"personal"}, 0); err != nil || ok {
		t.Fatalf("user 3's runner claimed (ok=%v, %v)", ok, err)
	}
	tag, err := pool.Exec(runner(3), `UPDATE flow_jobs SET status = 'running', lease_until = now() + interval '1 minute' WHERE id = $1`, j.ID)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("raw UPDATE of user 2's job as user 3: %d rows, %v; want 0 rows", tag.RowsAffected(), err)
	}
	got, err := s.Get(session(2), j.ID)
	if err != nil || got.Status != StatusQueued {
		t.Fatalf("user 2's job after user 3's attempts: %+v %v; want still queued", got, err)
	}
	// Control: the owner's own runner takes it.
	c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0)
	if err != nil || !ok || c.ID != j.ID || c.Status != StatusRunning || c.LeaseUntil == nil || c.Attempts != 1 {
		t.Fatalf("control: the owner's runner claim = %+v ok=%v %v", c, ok, err)
	}
}

func TestClaimIsAddressedByHostAndInstance(t *testing.T) {
	s, _ := pgStore(t)
	mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	for _, c := range []struct {
		host string
		ins  []string
	}{{"desktop", []string{"personal"}}, {"laptop", []string{"work"}}} {
		if _, ok, err := s.Claim(runner(2), c.host, c.ins, 0); err != nil || ok {
			t.Fatalf("a runner on %s/%v claimed a laptop/personal job (ok=%v %v)", c.host, c.ins, ok, err)
		}
	}
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"work", "personal"}, 0); err != nil || !ok {
		t.Fatalf("control: laptop with personal connected could not claim (ok=%v %v)", ok, err)
	}
	if _, _, err := s.Claim(runner(2), "laptop", nil, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a claim naming no instance = %v; want ErrInvalid", err)
	}
}

// TestOneRunningJobPerOwnerHostInstance: a second job on the same browser waits
// for the first; another instance is independent.
func TestOneRunningJobPerOwnerHostInstance(t *testing.T) {
	s, _ := pgStore(t)
	a := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	b := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	w := mustEnqueue(t, s, session(2), pollReq("laptop", "work"))
	first, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0)
	if err != nil || !ok || first.ID != a.ID {
		t.Fatalf("first claim = %+v ok=%v %v; want job %d (oldest first)", first, ok, err, a.ID)
	}
	if got, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || ok {
		t.Fatalf("a second job on a BUSY instance was claimed: %+v (%v)", got, err)
	}
	if got, ok, err := s.Claim(runner(2), "laptop", []string{"personal", "work"}, 0); err != nil || !ok || got.ID != w.ID {
		t.Fatalf("the idle instance's job was not claimable: %+v ok=%v %v", got, ok, err)
	}
	if _, err := s.Report(runner(2), a.ID, "laptop", Result{Outcome: OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || got.ID != b.ID {
		t.Fatalf("after the first finished, the waiting job was not claimed: %+v ok=%v %v", got, ok, err)
	}
}

// TestTheClaimRaceHasOneWinner: many runners of one owner claim at once with
// several jobs queued on ONE instance; exactly one job ends up running. Run
// with -race. The unique index is the backstop the NOT EXISTS cannot be.
func TestTheClaimRaceHasOneWinner(t *testing.T) {
	s, pool := pgStore(t)
	for i := 0; i < 4; i++ {
		mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	}
	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	wins := make(chan int64, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			j, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				wins <- j.ID
			}
		}()
	}
	close(start)
	wg.Wait()
	close(wins)
	close(errs)
	for err := range errs {
		t.Errorf("a racing claim errored: %v", err)
	}
	var won []int64
	for id := range wins {
		won = append(won, id)
	}
	if len(won) != 1 {
		t.Fatalf("%d claims won (%v); want exactly 1", len(won), won)
	}
	var running int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM flow_jobs WHERE status = 'running'`).Scan(&running); err != nil || running != 1 {
		t.Fatalf("%d rows running (%v); want 1", running, err)
	}
}

// TestTheIndexIsWhatMakesTheLoserOfATrueRaceLose builds the race the claim's
// NOT EXISTS cannot see, deterministically: another transaction has set job A
// running but not committed, so the claim's snapshot shows no running job,
// SKIP LOCKED steps past A, and the claim takes B on the SAME instance. Only
// the 0012 partial unique index stops that: the claim's update waits on the
// uncommitted index entry, fails with a unique violation once A commits, and
// Claim must report "nothing to take" — never an error, never a second job.
// (TestTheClaimRaceHasOneWinner's goroutines did not, measured, reach this
// interleaving: it stayed green with the index dropped.)
func TestTheIndexIsWhatMakesTheLoserOfATrueRaceLose(t *testing.T) {
	s, pool := pgStore(t)
	a := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	tx, err := pool.Begin(runner(2))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(runner(2), `UPDATE flow_jobs SET status = 'running', lease_until = now() + interval '1 minute' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	type res struct {
		ok  bool
		err error
	}
	done := make(chan res, 1)
	go func() {
		_, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0)
		done <- res{ok, err}
	}()
	time.Sleep(500 * time.Millisecond) // the claim is now waiting on A's index entry
	if err := tx.Commit(runner(2)); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.ok {
		t.Fatalf("the racing claim = ok %v, err %v; want nothing to take and no error", r.ok, r.err)
	}
	var running int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM flow_jobs WHERE status = 'running'`).Scan(&running); err != nil || running != 1 {
		t.Fatalf("%d jobs running (%v); want 1", running, err)
	}
}

// TestAQueuedJobPastItsTTLExpiresAndNeverRuns.
func TestAQueuedJobPastItsTTLExpiresAndNeverRuns(t *testing.T) {
	s, pool := pgStore(t)
	before := testutil.ToFloat64(metrics.FlowJobs.WithLabelValues("muse.send", "expired"))
	var id int64
	// A job whose ttl has already passed (inserted directly: no API takes a
	// ttl below a minute).
	if err := pool.QueryRow(session(2), `INSERT INTO flow_jobs (flow, class, host, instance, params, requested_via, expires_at)
		VALUES ('muse.send', 'write', 'laptop', 'personal', '{"text":"late"}', 'session', now() - interval '1 second')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if j, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || ok {
		t.Fatalf("an expired job was claimed: %+v (%v)", j, err)
	}
	j, err := s.Get(session(2), id)
	if err != nil || j.Status != StatusExpired || j.FinishedAt == nil {
		t.Fatalf("the expired job reads %+v %v; want status expired", j, err)
	}
	if got := testutil.ToFloat64(metrics.FlowJobs.WithLabelValues("muse.send", "expired")) - before; got != 1 {
		t.Fatalf("muster_flow_jobs_total{muse.send,expired} moved by %v; want 1", got)
	}
	// Control: a live job beside it IS claimed, so the refusal above is about
	// the ttl.
	live := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	if got, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || got.ID != live.ID {
		t.Fatalf("control: the live job was not claimed: %+v ok=%v %v", got, ok, err)
	}
}

// lapse moves a running job's lease into the past (lease_until is not input,
// so the immutability trigger allows it).
func lapse(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE flow_jobs SET lease_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// TestALostWriteLeaseNeedsTheOperatorAndALostReadLeaseRequeues.
func TestALostWriteLeaseNeedsTheOperatorAndALostReadLeaseRequeues(t *testing.T) {
	s, pool := pgStore(t)
	send := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok {
		t.Fatalf("claim send: ok=%v %v", ok, err)
	}
	lapse(t, pool, send.ID)
	got, err := s.Get(session(2), send.ID)
	if err != nil || got.Status != StatusNeedsOperator || !strings.Contains(got.Detail, "may have been delivered") {
		t.Fatalf("a write job whose lease lapsed reads %+v %v; want needs_operator", got, err)
	}
	// It is never claimed again.
	if j, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || ok {
		t.Fatalf("the lapsed write job was claimed again: %+v (%v)", j, err)
	}

	poll := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok {
		t.Fatalf("claim poll: ok=%v %v", ok, err)
	}
	lapse(t, pool, poll.ID)
	got, err = s.Get(session(2), poll.ID)
	if err != nil || got.Status != StatusQueued || got.Attempts != 1 || got.LeaseUntil != nil {
		t.Fatalf("a read job whose lease lapsed reads %+v %v; want queued again", got, err)
	}
	// Re-claimed, lapsed until the attempts are spent: then failed, not queued.
	for i := 2; i <= maxReadAttempts; i++ {
		if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != poll.ID || c.Attempts != i {
			t.Fatalf("re-claim %d: %+v ok=%v %v", i, c, ok, err)
		}
		lapse(t, pool, poll.ID)
	}
	if got, err = s.Get(session(2), poll.ID); err != nil || got.Status != StatusFailed {
		t.Fatalf("after %d lost leases the read job reads %+v %v; want failed", maxReadAttempts, got, err)
	}
}

func TestReportAndHeartbeatAreConfinedToTheRunnersHost(t *testing.T) {
	s, _ := pgStore(t)
	j := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if _, err := s.Report(runner(2), j.ID, "laptop", Result{Outcome: OutcomeDone}); !errors.Is(err, ErrState) {
		t.Fatalf("Report on a QUEUED job = %v; want ErrState", err)
	}
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok {
		t.Fatalf("claim: ok=%v %v", ok, err)
	}
	for name, call := range map[string]func() error{
		"heartbeat from another host": func() error { _, err := s.Heartbeat(runner(2), j.ID, "desktop"); return err },
		"report from another host": func() error {
			_, err := s.Report(runner(2), j.ID, "desktop", Result{Outcome: OutcomeDone})
			return err
		},
		"heartbeat by another owner": func() error { _, err := s.Heartbeat(runner(3), j.ID, "laptop"); return err },
		"report by another owner": func() error {
			_, err := s.Report(runner(3), j.ID, "laptop", Result{Outcome: OutcomeDone})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v; want ErrNotFound (indistinguishable from no such job)", name, err)
		}
	}
	if _, err := s.Report(runner(2), j.ID, "laptop", Result{Outcome: "queued"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a runner reporting `queued` = %v; want ErrInvalid", err)
	}
	if hb, err := s.Heartbeat(runner(2), j.ID, "laptop"); err != nil || hb.LeaseUntil == nil {
		t.Fatalf("control: the right runner's heartbeat = %+v %v", hb, err)
	}
	code := 5
	big := strings.Repeat("é", MaxOutputBytes) // 2 bytes a rune: twice the cap
	done, err := s.Report(runner(2), j.ID, "laptop", Result{Outcome: OutcomeNeedsOperator, ExitCode: &code, Output: big, Detail: "approval pending"})
	if err != nil || done.Status != StatusNeedsOperator || done.ExitCode == nil || *done.ExitCode != 5 {
		t.Fatalf("report = %+v %v", done, err)
	}
	if len(done.Output) > MaxOutputBytes || len(done.Output) < MaxOutputBytes-1 || !strings.HasPrefix(big, done.Output) {
		t.Fatalf("output stored as %d bytes; want truncated at a rune boundary to <= %d", len(done.Output), MaxOutputBytes)
	}
	if _, err := s.Report(runner(2), j.ID, "laptop", Result{Outcome: OutcomeDone}); !errors.Is(err, ErrState) {
		t.Fatalf("a second report = %v; want ErrState", err)
	}
}

func TestCancel(t *testing.T) {
	s, _ := pgStore(t)
	q := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if _, err := s.Cancel(session(3), q.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's cancel = %v; want ErrNotFound", err)
	}
	if c, err := s.Cancel(session(2), q.ID); err != nil || c.Status != StatusCancelled {
		t.Fatalf("owner cancel = %+v %v", c, err)
	}
	r := mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok {
		t.Fatalf("claim: ok=%v %v", ok, err)
	}
	if _, err := s.Cancel(session(2), r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("cancelling a RUNNING job = %v; want ErrState", err)
	}
}

func TestThePendingCapIsPerOwner(t *testing.T) {
	s, _ := pgStore(t)
	for i := 0; i < MaxPendingPerOwner; i++ {
		mustEnqueue(t, s, session(2), pollReq("laptop", "personal"))
	}
	if _, err := s.Enqueue(session(2), pollReq("laptop", "personal"), "session", ""); !errors.Is(err, ErrPendingCap) {
		t.Fatalf("enqueue past the cap = %v; want ErrPendingCap", err)
	}
	if _, err := s.Enqueue(session(3), pollReq("laptop", "personal"), "session", ""); err != nil {
		t.Fatalf("another owner is not affected by user 2's queue: %v", err)
	}
}

func TestAJobsInputIsImmutable(t *testing.T) {
	s, pool := pgStore(t)
	j := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	sys := authz.System(context.Background())
	for _, set := range []string{`params = '{"text":"rewritten"}'`, `flow = 'muse.poll'`, `host = 'desktop'`,
		`instance = 'work'`, `class = 'read'`, `owner_id = 3`, `expires_at = now() + interval '9 hours'`} {
		if _, err := pool.Exec(sys, `UPDATE flow_jobs SET `+set+` WHERE id = $1`, j.ID); err == nil {
			t.Errorf("UPDATE ... SET %s succeeded; a job's input must be immutable", set)
		}
	}
	// Control: a non-input column can change (that is how a job runs).
	if _, err := pool.Exec(sys, `UPDATE flow_jobs SET detail = 'x' WHERE id = $1`, j.ID); err != nil {
		t.Fatalf("control: updating detail failed: %v", err)
	}
}

func TestRunnerTokensAreHashedOwnedAndIsolated(t *testing.T) {
	s, pool := pgStore(t)
	sys := authz.System(context.Background())
	tok, rec, err := s.CreateRunnerToken(session(2), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if rec.OwnerID != 2 || rec.Host != "laptop" || !RunnerTokenShapeOK(tok) {
		t.Fatalf("minted %q %+v", tok, rec)
	}
	var row string
	if err := pool.QueryRow(sys, `SELECT row_to_json(t)::text FROM flow_runner_tokens t WHERE id = $1`, rec.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, strings.TrimPrefix(tok, RunnerTokenPrefix)) || !strings.Contains(row, HashRunnerToken(tok)) {
		t.Fatalf("the stored row is not hash-only: %s", row)
	}
	if got, err := s.AuthenticateRunner(sys, tok); err != nil || got.OwnerID != 2 || got.Host != "laptop" {
		t.Fatalf("AuthenticateRunner under System = %+v %v", got, err)
	}
	if _, err := s.AuthenticateRunner(session(3), tok); !errors.Is(err, ErrBadRunnerToken) {
		t.Fatalf("user 3's principal FOUND user 2's runner token (%v)", err)
	}
	if list, err := s.ListRunnerTokens(session(3)); err != nil || len(list) != 0 {
		t.Fatalf("user 3 lists %v (%v); want none", list, err)
	}
	if found, err := s.RevokeRunnerToken(session(3), rec.ID); err != nil || found {
		t.Fatalf("user 3 revoked user 2's token: found=%v %v", found, err)
	}
	for name, bad := range map[string]string{
		"personal-token shaped": "mst_" + strings.TrimPrefix(tok, RunnerTokenPrefix),
		"unknown mfr_":          RunnerTokenPrefix + strings.Repeat("A", 43),
		"the hash itself":       HashRunnerToken(tok),
	} {
		if _, err := s.AuthenticateRunner(sys, bad); !errors.Is(err, ErrBadRunnerToken) {
			t.Errorf("AuthenticateRunner(%s) = %v; want ErrBadRunnerToken", name, err)
		}
	}
	if found, err := s.RevokeRunnerToken(session(2), rec.ID); err != nil || !found {
		t.Fatalf("owner revoke: found=%v %v", found, err)
	}
	if _, err := s.AuthenticateRunner(sys, tok); !errors.Is(err, ErrBadRunnerToken) {
		t.Fatalf("a revoked token still authenticates (%v)", err)
	}
	// A disabled owner's token is refused.
	dtok, _, err := s.CreateRunnerToken(session(4), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateRunner(sys, dtok); !errors.Is(err, ErrBadRunnerToken) {
		t.Fatalf("a disabled owner's runner token authenticated (%v)", err)
	}
}
