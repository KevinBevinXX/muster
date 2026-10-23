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

// insertAgedPoll inserts a queued muse.poll on instance created 19 minutes ago
// with the default 15-minute ttl, so its recorded expires_at passed 4 minutes
// ago (inserted directly: created_at and expires_at are immutable input).
func insertAgedPoll(t *testing.T, pool *pgxpool.Pool, instance string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(session(2), `INSERT INTO flow_jobs (flow, class, host, instance, params, requested_via, created_at, expires_at)
		VALUES ('muse.poll', 'read', 'laptop', $1, '{}', 'session', now() - interval '19 minutes', now() - interval '4 minutes')
		RETURNING id`, instance).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAJobQueuedBehindARunningJobDoesNotExpireWhileBlocked is round-1 finding
// F1: a muse dispatch holds its browser instance for up to wait+240 s (wait ≤
// 3600 s) plus the pacing gap, and a job queued behind it on the same
// owner/host/instance cannot be claimed meanwhile. Its ttl must not run while
// it is blocked. Scenario: a muse.send was claimed 20 minutes ago and is still
// running (heartbeating); a muse.poll queued 19 minutes ago with the default
// 15-minute ttl is still queued — not expired — and is claimed once the send
// reports. CONTROL: the same aged poll on an instance nothing is running on
// expires as before, so the hold is about the block, not a ttl that never
// fires.
func TestAJobQueuedBehindARunningJobDoesNotExpireWhileBlocked(t *testing.T) {
	s, pool := pgStore(t)
	send := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != send.ID {
		t.Fatalf("claim send: %+v ok=%v %v", c, ok, err)
	}
	if _, err := pool.Exec(authz.System(context.Background()),
		`UPDATE flow_jobs SET claimed_at = now() - interval '20 minutes' WHERE id = $1`, send.ID); err != nil {
		t.Fatal(err)
	}
	blocked := insertAgedPoll(t, pool, "personal")
	free := insertAgedPoll(t, pool, "work")

	if got, err := s.Get(session(2), blocked); err != nil || got.Status != StatusQueued {
		t.Fatalf("the poll queued behind the running send reads %+v %v; want still queued", got, err)
	}
	if got, err := s.Get(session(2), free); err != nil || got.Status != StatusExpired {
		t.Fatalf("CONTROL: the same aged poll on an idle instance reads %+v %v; want expired", got, err)
	}
	if _, err := s.Report(runner(2), send.ID, "laptop", Result{Outcome: OutcomeDone}); err != nil {
		t.Fatalf("report send: %v", err)
	}
	c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0)
	if err != nil || !ok || c.ID != blocked {
		t.Fatalf("after the send reported, claim = %+v ok=%v %v; want the blocked poll %d", c, ok, err, blocked)
	}
	// The pause is exactly the time it was blocked — all 19 minutes since it
	// was queued (the send was claimed before it) — not an unbounded hold: its
	// effective deadline is created + 15m ttl + 19m paused, i.e. ~15m from now.
	var left float64
	if err := pool.QueryRow(authz.System(context.Background()),
		`SELECT extract(epoch FROM (expires_at + ttl_paused) - now()) FROM flow_jobs WHERE id = $1`, blocked).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left < 14*60 || left > 15*60+5 {
		t.Fatalf("the unblocked poll has %.0fs of ttl left; want ~900s (blocked 19m, 15m ttl, created 19m ago)", left)
	}
}

// TestALapsedBlockerAlsoResumesTheTTLOfTheJobsBehindIt: the same pause, when
// the blocking job leaves `running` through the lease sweep instead of a
// Report (a runner that died mid-run).
func TestALapsedBlockerAlsoResumesTheTTLOfTheJobsBehindIt(t *testing.T) {
	s, pool := pgStore(t)
	send := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	if _, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok {
		t.Fatalf("claim send: ok=%v %v", ok, err)
	}
	if _, err := pool.Exec(authz.System(context.Background()),
		`UPDATE flow_jobs SET claimed_at = now() - interval '20 minutes' WHERE id = $1`, send.ID); err != nil {
		t.Fatal(err)
	}
	blocked := insertAgedPoll(t, pool, "personal")
	lapse(t, pool, send.ID)
	if got, err := s.Get(session(2), send.ID); err != nil || got.Status != StatusNeedsOperator {
		t.Fatalf("the lapsed send reads %+v %v; want needs_operator", got, err)
	}
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != blocked {
		t.Fatalf("after the send's lease lapsed, claim = %+v ok=%v %v; want the blocked poll %d", c, ok, err, blocked)
	}
}

// sys runs owner-level SQL (ttl_paused and claimed_at are not input, so the
// immutability trigger allows them).
func sys(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(authz.System(context.Background()), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func pausedSeconds(t *testing.T, pool *pgxpool.Pool, id int64) float64 {
	t.Helper()
	var s float64
	if err := pool.QueryRow(authz.System(context.Background()),
		`SELECT extract(epoch FROM ttl_paused) FROM flow_jobs WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// insertPoll inserts a queued muse.poll on "personal" created `age` ago with
// a ttl of `ttl` (inserted directly: created_at and expires_at are input).
func insertPoll(t *testing.T, pool *pgxpool.Pool, age, ttl time.Duration) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(session(2), `INSERT INTO flow_jobs (flow, class, host, instance, params, requested_via, created_at, expires_at)
		VALUES ('muse.poll', 'read', 'laptop', 'personal', '{}', 'session',
		        now() - make_interval(secs => $1), now() - make_interval(secs => $1) + make_interval(secs => $2))
		RETURNING id`, age.Seconds(), ttl.Seconds()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// runningSend enqueues and claims a muse.send on laptop/personal, then backdates
// its claim by `ago`.
func runningSend(t *testing.T, s *PGStore, pool *pgxpool.Pool, ago time.Duration) Job {
	t.Helper()
	send := mustEnqueue(t, s, session(2), sendReq("laptop", "personal"))
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != send.ID {
		t.Fatalf("claim send: %+v ok=%v %v", c, ok, err)
	}
	sys(t, pool, `UPDATE flow_jobs SET claimed_at = now() - make_interval(secs => $2) WHERE id = $1`, send.ID, ago.Seconds())
	return send
}

// The two expiry details, pinned whole: a reader must be able to tell a job
// that waited out a wedged blocker from one whose own ttl ran out.
const (
	wantBlockedExpiry = "not claimed within its ttl plus up to 90 minutes of credit for each running job it waited behind on the same browser, and the job blocking it has run longer than that; it will not run later than that"
	wantPlainExpiry   = "not claimed before its ttl (plus its credit for time spent blocked); it will not run later than that"
)

// TestABlockedJobStillExpiresAtTheAbsoluteCeiling (round-2 F-A, round-3 (b)):
// a runner that keeps heartbeating a wedged flow must not hold the jobs behind
// it forever. A poll blocked by ONE blocker for longer than MaxTTLPause, with
// its own ttl spent, expires even though that blocker is STILL running, and
// its detail is the blocked-branch message, whole. CONTROL: a poll blocked
// just inside the bound stays queued.
func TestABlockedJobStillExpiresAtTheAbsoluteCeiling(t *testing.T) {
	s, pool := pgStore(t)
	runningSend(t, s, pool, 3*time.Hour)
	past := insertPoll(t, pool, DefaultTTL+MaxTTLPause+time.Minute, DefaultTTL)
	inside := insertPoll(t, pool, DefaultTTL+MaxTTLPause-time.Minute, DefaultTTL)
	got, err := s.Get(session(2), past)
	if err != nil || got.Status != StatusExpired {
		t.Fatalf("a poll blocked past the ceiling reads %+v %v; want expired while the blocker still runs", got, err)
	}
	if got.Detail != wantBlockedExpiry {
		t.Fatalf("detail = %q\nwant      %q", got.Detail, wantBlockedExpiry)
	}
	if got, err := s.Get(session(2), inside); err != nil || got.Status != StatusQueued {
		t.Fatalf("CONTROL: a poll blocked just inside the ceiling reads %+v %v; want still queued", got, err)
	}
}

// TestAnUnblockedJobsExpiryDetailIsThePlainOne: the other branch, pinned whole.
func TestAnUnblockedJobsExpiryDetailIsThePlainOne(t *testing.T) {
	s, pool := pgStore(t)
	id := insertPoll(t, pool, 20*time.Minute, DefaultTTL)
	got, err := s.Get(session(2), id)
	if err != nil || got.Status != StatusExpired || got.Detail != wantPlainExpiry {
		t.Fatalf("an unblocked aged poll reads %+v %v; want expired with %q", got, err, wantPlainExpiry)
	}
}

// TestAJobBehindTwoLongDispatchesSurvives (round-3 (a)): two muse.send
// wait=3600 queued, then a muse.poll with the default 15-min ttl. Send 1 held
// the instance ~63 min (credited to the poll when it reported) and send 2 has
// now been running 64 min, legitimately. ~127 min of total waiting exceeds
// ttl + 90, but no single blocker exceeded MaxTTLPause, so the poll is still
// queued and is claimed once send 2 reports.
func TestAJobBehindTwoLongDispatchesSurvives(t *testing.T) {
	s, pool := pgStore(t)
	send2 := runningSend(t, s, pool, 64*time.Minute)
	poll := insertPoll(t, pool, 127*time.Minute, DefaultTTL)
	sys(t, pool, `UPDATE flow_jobs SET ttl_paused = interval '63 minutes' WHERE id = $1`, poll) // send 1's credit
	if got, err := s.Get(session(2), poll); err != nil || got.Status != StatusQueued {
		t.Fatalf("the poll behind the second long dispatch reads %+v %v; want still queued", got, err)
	}
	if _, err := s.Report(runner(2), send2.ID, "laptop", Result{Outcome: OutcomeDone}); err != nil {
		t.Fatalf("report send 2: %v", err)
	}
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != poll {
		t.Fatalf("after send 2 reported, claim = %+v ok=%v %v; want the poll %d", c, ok, err, poll)
	}
	if got := pausedSeconds(t, pool, poll); got < (63+64)*60-5 || got > (63+64)*60+5 {
		t.Fatalf("ttl_paused = %.0fs; want ~%ds (63m + 64m, each under the per-blocker cap)", got, (63+64)*60)
	}
}

// TestACreditedJobBlockedAgainHasTheDocumentedBound (round-3 (c)): the bound is
// expires_at + its credit so far + up to MaxTTLPause for the CURRENT blocker.
// A poll already credited 30 min, then blocked by a wedged send running 100
// min: expired once expires_at + 30m + 90m has passed; a twin 30 min younger is
// still queued.
func TestACreditedJobBlockedAgainHasTheDocumentedBound(t *testing.T) {
	s, pool := pgStore(t)
	runningSend(t, s, pool, 100*time.Minute)
	old := insertPoll(t, pool, 150*time.Minute, DefaultTTL)   // deadline -135+30+90 = -15m
	young := insertPoll(t, pool, 120*time.Minute, DefaultTTL) // deadline -105+30+90 = +15m
	sys(t, pool, `UPDATE flow_jobs SET ttl_paused = interval '30 minutes' WHERE id IN ($1, $2)`, old, young)
	if got, err := s.Get(session(2), old); err != nil || got.Status != StatusExpired || got.Detail != wantBlockedExpiry {
		t.Fatalf("the credited poll past its bound reads %+v %v; want expired (blocked branch)", got, err)
	}
	if got, err := s.Get(session(2), young); err != nil || got.Status != StatusQueued {
		t.Fatalf("CONTROL: the credited poll inside its bound reads %+v %v; want still queued", got, err)
	}
}

// TestTheCurrentBlockersCreditIsTheTimeItHasBlocked: while blocked, a job's
// clock is paused only for the time THIS blocker has actually held it (up to
// MaxTTLPause), not a flat MaxTTLPause. A poll whose ttl ran out while nothing
// blocked it (queued 30m ago, 15m ttl), then blocked 10m ago, is expired.
func TestTheCurrentBlockersCreditIsTheTimeItHasBlocked(t *testing.T) {
	s, pool := pgStore(t)
	runningSend(t, s, pool, 10*time.Minute)
	id := insertPoll(t, pool, 30*time.Minute, DefaultTTL) // deadline -15m + 10m blocked = -5m
	if got, err := s.Get(session(2), id); err != nil || got.Status != StatusExpired {
		t.Fatalf("a poll whose own ttl ran out before the blocker came reads %+v %v; want expired", got, err)
	}
}

// TestTheTTLCreditIsCappedAtMaxTTLPause: a poll blocked for longer than the
// ceiling's credit (but not yet past expires_at + MaxTTLPause) is credited
// exactly MaxTTLPause when its blocker reports, never more.
func TestTheTTLCreditIsCappedAtMaxTTLPause(t *testing.T) {
	s, pool := pgStore(t)
	send := runningSend(t, s, pool, 3*time.Hour)
	id := insertPoll(t, pool, MaxTTLPause+10*time.Minute, DefaultTTL) // blocked 100m of a 90m cap
	if _, err := s.Report(runner(2), send.ID, "laptop", Result{Outcome: OutcomeDone}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got := pausedSeconds(t, pool, id); got != MaxTTLPause.Seconds() {
		t.Fatalf("ttl_paused = %.0fs; want exactly the cap %.0fs", got, MaxTTLPause.Seconds())
	}
}

// TestTheCreditStartsWhenTheBlockerWasClaimed (round-2 F-B, mutant M4): a poll
// queued while the runner was offline (its ttl running, nothing blocking it),
// and only THEN blocked by a newly claimed send, is credited from the
// blocker's claim — not from its own creation.
func TestTheCreditStartsWhenTheBlockerWasClaimed(t *testing.T) {
	s, pool := pgStore(t)
	// The send was queued first (so it is claimed first) and the poll 20m
	// ago; the runner was offline until 10m ago, when it claimed the send.
	send := runningSend(t, s, pool, 10*time.Minute)
	id := insertPoll(t, pool, 20*time.Minute, 25*time.Minute)
	if _, err := s.Report(runner(2), send.ID, "laptop", Result{Outcome: OutcomeDone}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got := pausedSeconds(t, pool, id); got < 600-5 || got > 600+5 {
		t.Fatalf("ttl_paused = %.0fs; want ~600s (blocked only since the send's claim, not its 1200s age)", got)
	}
}

// TestACreditedReadJobWhoseLeaseLapsesIsRequeued (round-2 F-B, mutants M6/M6b):
// a read job claimed AFTER its raw expires_at — legitimately, on ttl credit —
// whose lease then lapses is re-queued, not expired: the lease sweep judges it
// on the effective deadline too.
func TestACreditedReadJobWhoseLeaseLapsesIsRequeued(t *testing.T) {
	s, pool := pgStore(t)
	id := insertPoll(t, pool, 20*time.Minute, 15*time.Minute) // raw deadline passed 5m ago
	sys(t, pool, `UPDATE flow_jobs SET ttl_paused = interval '15 minutes' WHERE id = $1`, id)
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != id {
		t.Fatalf("claim the credited poll: %+v ok=%v %v", c, ok, err)
	}
	lapse(t, pool, id)
	got, err := s.Get(session(2), id)
	if err != nil || got.Status != StatusQueued || got.FinishedAt != nil || !strings.Contains(got.Detail, "re-queued") {
		t.Fatalf("the credited read job whose lease lapsed reads %+v %v; want re-queued", got, err)
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
