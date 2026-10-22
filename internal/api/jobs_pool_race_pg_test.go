package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// CONCURRENT POOL SUBMISSIONS, through the real handler and the real Postgres
// job and agent stores. A pool's choice reads each worker's queue, so two
// submissions that read before either inserts see the same queues: unless the
// read and the insert are one step, they all choose the same worker, its cap
// refuses the overflow with a 429, and the other worker is never tried
// (createPoolJob / jobs.Store.CreateChosen).
// ---------------------------------------------------------------------------

// poolRaceRig is a server on the real stores with two eligible workers in a
// pool of their own, whose turns are held until the test ends, so every job
// accepted stays queued or running — what the cap counts.
type poolRaceRig struct {
	pool     *pgxpool.Pool
	ts       *httptest.Server
	poolName string
	workers  []agents.Agent
}

func newPoolRaceRig(t *testing.T) *poolRaceRig {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	r := &poolRaceRig{pool: pool, poolName: "race-" + suffix}
	ags := agents.NewPG(pool)
	for _, w := range []struct{ name, account string }{{"race-a-" + suffix, "alpha"}, {"race-b-" + suffix, "beta"}} {
		a, err := ags.Create(asUser(1), agents.Agent{Name: w.name, Namespace: "devpod-" + w.name, DisplayName: w.name,
			Status: agents.StatusRunning, HooksToken: "hooks-" + w.name, Kind: agents.KindClaudeCode, CCAccount: w.account,
			Repo: "acme/widgets", WorkerPool: r.poolName})
		if err != nil {
			t.Fatalf("agent %s: %v", w.name, err)
		}
		r.workers = append(r.workers, a)
	}
	gw := &jobGateway{reply: "done", release: make(chan struct{})}
	s := New(nil, AuthConfig{ServiceToken: testServiceToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          ags,
		SessionLiveness: stubLiveness{},
		Gateway:         gw,
		Jobs:            jobs.NewPG(pool),
		Kinds:           &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}},
	})
	r.ts = httptest.NewServer(s.Handler())
	// Cleanups run last-registered first: release the held turns, wait for
	// every job's runner to record its outcome, then remove the rows — all
	// before the pool registered above is closed.
	t.Cleanup(func() {
		r.ts.Close()
		close(gw.release)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if r.unfinished(t) == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		bg := dbtest.AsOperator(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM jobs WHERE worker_pool = $1`, r.poolName)
		_, _ = pool.Exec(bg, `DELETE FROM agents WHERE worker_pool = $1`, r.poolName)
	})
	return r
}

// unfinished is how many of the rig's jobs are queued or running.
func (r *poolRaceRig) unfinished(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(dbtest.AsOperator(context.Background()),
		`SELECT count(*) FROM jobs WHERE worker_pool = $1 AND status IN ('queued', 'running')`, r.poolName).Scan(&n); err != nil {
		t.Fatalf("count unfinished: %v", err)
	}
	return n
}

// perWorker is how many of the rig's jobs each worker holds queued or running,
// read from the database rather than from the responses.
func (r *poolRaceRig) perWorker(t *testing.T) map[string]int {
	t.Helper()
	rows, err := r.pool.Query(dbtest.AsOperator(context.Background()),
		`SELECT agent_name, count(*) FROM jobs WHERE worker_pool = $1 AND status IN ('queued', 'running') GROUP BY agent_name`, r.poolName)
	if err != nil {
		t.Fatalf("count per worker: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type poolSubmitResult struct {
	code  int
	agent string
	err   string
}

// submit posts one pool job and returns the status and the worker it went to.
func (r *poolRaceRig) submit(t *testing.T) poolSubmitResult {
	body, _ := json.Marshal(map[string]any{"worker_pool": r.poolName, "prompt": "review"})
	req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/api/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	req.Header.Set(ActorHeader, "review-orchestrator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return poolSubmitResult{err: err.Error()}
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	agent, _ := out["agent"].(string)
	msg, _ := out["error"].(string)
	return poolSubmitResult{code: resp.StatusCode, agent: agent, err: msg}
}

// submitConcurrently releases n submissions at once (a barrier) and returns
// their results.
func (r *poolRaceRig) submitConcurrently(t *testing.T, n int) []poolSubmitResult {
	t.Helper()
	start := make(chan struct{})
	out := make([]poolSubmitResult, n)
	var ready, done sync.WaitGroup
	for i := 0; i < n; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			out[i] = r.submit(t)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return out
}

// TestConcurrentPoolSubmissionsSpreadAcrossTheWorkers: six submissions at the
// same moment to a pool of two idle workers with four places each are all
// accepted, both workers are used, and neither holds more than the cap. It runs
// the scenario three times, each on a fresh pool, since one round can win the
// race by luck.
func TestConcurrentPoolSubmissionsSpreadAcrossTheWorkers(t *testing.T) {
	for round := 0; round < 3; round++ {
		r := newPoolRaceRig(t)
		res := r.submitConcurrently(t, 6)
		for i, x := range res {
			if x.code != http.StatusAccepted {
				t.Fatalf("round %d: submission %d = %d %q (%s), want 202 for all six (8 places across 2 workers)",
					round, i, x.code, x.err, x.agent)
			}
		}
		got := r.perWorker(t)
		if len(got) != 2 {
			t.Fatalf("round %d: jobs per worker = %v, want both workers used", round, got)
		}
		for name, n := range got {
			if n > jobs.MaxActivePerAgent {
				t.Fatalf("round %d: worker %s holds %d jobs, over the cap %d", round, name, n, jobs.MaxActivePerAgent)
			}
		}
		// Not "three and three": the order prefers the fewest QUEUED, and how
		// many of each worker's jobs its runner has already moved to running is
		// timing, so four and two is also a correct answer.
		if got[r.workers[0].Name]+got[r.workers[1].Name] != 6 {
			t.Fatalf("round %d: jobs per worker = %v, want six in all", round, got)
		}
	}
}

// TestConcurrentPoolSubmissionsFillThePoolExactly: eight submissions at once to
// two workers of four places each — exactly the pool's capacity — are all
// accepted, four on each; the ninth, with every eligible worker full, is a 429.
func TestConcurrentPoolSubmissionsFillThePoolExactly(t *testing.T) {
	r := newPoolRaceRig(t)
	for i, x := range r.submitConcurrently(t, 8) {
		if x.code != http.StatusAccepted {
			t.Fatalf("submission %d = %d %q, want 202 for all eight", i, x.code, x.err)
		}
	}
	got := r.perWorker(t)
	if got[r.workers[0].Name] != 4 || got[r.workers[1].Name] != 4 {
		t.Fatalf("jobs per worker = %v, want 4 and 4", got)
	}
	ninth := r.submit(t)
	want := `no worker in pool "` + r.poolName + `" can take the job: ` + r.workers[0].Name + ` (already has 4 jobs queued or running (the limit)); ` +
		r.workers[1].Name + ` (already has 4 jobs queued or running (the limit))`
	if ninth.code != http.StatusTooManyRequests || ninth.err != want {
		t.Fatalf("the ninth = %d %q\nwant 429 %q", ninth.code, ninth.err, want)
	}
	if n := r.unfinished(t); n != 8 {
		t.Fatalf("%d jobs recorded, want 8", n)
	}
}
