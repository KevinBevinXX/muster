package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// WORKER POOLS (jobs.go createPoolJob / chooseWorker).
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
		github   map[int64]string
		want     string
	}{
		{"an idle worker beats a busy one that is better on every later rule",
			[]agents.Agent{worker(1, "a-busy", "alpha"), worker(2, "b-idle", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Running: 1}}, nil, "b-idle"},
		{"of two busy workers, the fewer queued",
			[]agents.Agent{worker(1, "a-two", "alpha"), worker(2, "b-one", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Running: 1, Queued: 2}, 2: {Running: 1, Queued: 1}}, nil, "b-one"},
		{"a queued-only worker is not idle",
			[]agents.Agent{worker(1, "a-queued", "alpha"), worker(2, "b-idle", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{1: {Queued: 1}}, nil, "b-idle"},
		{"of two idle workers, the never rate-limited account",
			[]agents.Agent{worker(1, "a-limited", "alpha"), worker(2, "b-never", "beta")},
			map[string]ccpool.AccountState{"alpha": {RateLimitedAt: t0900}, "beta": {}},
			nil, nil, "b-never"},
		{"of two rate-limited accounts, the longer ago",
			[]agents.Agent{worker(1, "a-recent", "alpha"), worker(2, "b-older", "beta")},
			map[string]ccpool.AccountState{"alpha": {RateLimitedAt: t1000}, "beta": {RateLimitedAt: t0900}},
			nil, nil, "b-older"},
		{"otherwise the name, whatever the input order",
			[]agents.Agent{worker(2, "z-last", "beta"), worker(1, "a-first", "alpha"), worker(3, "m-mid", "gamma")},
			healthy, nil, nil, "a-first"},
		{"an auth-failed account is skipped, though it would win every rule",
			[]agents.Agent{worker(1, "a-dead", "alpha"), worker(2, "b-ok", "beta")},
			map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{2: {Running: 1, Queued: 2}}, nil, "b-ok"},
		{"a worker holding a GitHub grant is skipped, though it would win every rule",
			[]agents.Agent{worker(1, "a-granted", "alpha"), worker(2, "b-clean", "beta")},
			map[string]ccpool.AccountState{"alpha": {}, "beta": {RateLimitedAt: t1000}},
			map[int64]jobs.ActiveCount{2: {Running: 1, Queued: 2}}, map[int64]string{1: "github"}, "b-clean"},
		{"a worker at its limit is skipped",
			[]agents.Agent{worker(1, "a-full", "alpha"), worker(2, "b-room", "beta")},
			// a-full, with none queued, would win on "fewest queued".
			healthy, map[int64]jobs.ActiveCount{1: {Running: 4}, 2: {Running: 1, Queued: 1}}, nil, "b-room"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why, full := chooseWorker("reviewers", tc.members, tc.members, "", tc.accounts, tc.active, tc.github)
			if why != "" || full || got.Name != tc.want {
				t.Fatalf("chose %q (%q, full %v), want %q", got.Name, why, full, tc.want)
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
	members = append(members, worker(7, "g-granted", "beta"))
	_, why, full := chooseWorker("reviewers", members, members, "acme/widgets", accounts, map[int64]jobs.ActiveCount{6: {Running: 1, Queued: 3}},
		map[int64]string{7: "github"})
	const want = `no worker in pool "reviewers" can take the job: a-dead (its Claude account "alpha" failed authentication); ` +
		`b-stopped (status stopped); c-gateway (kind gateway); d-other (works on repository "acme/other"); ` +
		`e-unknown (its Claude account "zeta" is not configured); f-full (already has 4 jobs queued or running (the limit)); ` +
		`g-granted (holds a grant on connector "github", which reaches github.com: a pool worker holds no GitHub credential)`
	if why != want {
		t.Fatalf("reason = %q\nwant      %q", why, want)
	}
	// f-full is the one member eligible but for its cap: every such member is
	// at capacity, so the refusal is "full" (a 429).
	if !full {
		t.Fatal("f-full alone could take the job but for its cap, and the refusal is not full")
	}
	// Without it, no member is eligible even with room: not full (a 503).
	if _, why, full := chooseWorker("reviewers", members[1:], members[1:], "acme/widgets", accounts, nil, map[int64]string{7: "github"}); why == "" || full {
		t.Fatalf("without f-full: reason %q, full %v; want a reason and not full", why, full)
	}
}

// poolKinds is AgentKinds with the account marks a pool job reads.
// It is safe for concurrent submissions (jobs_pool_race_pg_test.go).
type poolKinds struct {
	AgentKinds
	mu     sync.Mutex
	states map[string]ccpool.AccountState
	owners []int64
}

func (k *poolKinds) ClaudeAccountStates(_ context.Context, owner int64) (map[string]ccpool.AccountState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
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
// auth-failed is 503 with the reason, recording nothing; a REPEATED key is
// still answered with its first job, though its pool now has no eligible
// worker.
func TestAPoolJobIsRefusedWhenNoWorkerCanTakeIt(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	kinds := &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.s.ext.Kinds = kinds
	first := f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "x", "idempotency_key": "pool-1"})
	f.await(t, first)

	kinds.states = map[string]ccpool.AccountState{"alpha": {AuthFailed: true}, "beta": {AuthFailed: true}}
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
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

// grantStore is connectors.Store with the reads the GitHub-grant rule makes —
// the live grants, the connectors' hosts, one grant — and a recorded decision.
type grantStore struct {
	connectors.Store
	grants  []connectors.Grant
	conns   []connectors.Connector
	decided []int64
	// getsBeforeFailing, when positive, is how many GetGrant calls succeed
	// before every later one fails.
	getsBeforeFailing int
	gets              int
}

func (g *grantStore) ListGrants(context.Context) ([]connectors.Grant, error) { return g.grants, nil }
func (g *grantStore) ListConnectors(context.Context) ([]connectors.Connector, error) {
	return g.conns, nil
}
func (g *grantStore) GetGrant(_ context.Context, id int64) (connectors.Grant, error) {
	g.gets++
	if g.getsBeforeFailing > 0 && g.gets > g.getsBeforeFailing {
		return connectors.Grant{}, errors.New("grantStore: the database is unreachable")
	}
	for _, gr := range g.grants {
		if gr.ID == id {
			return gr, nil
		}
	}
	return connectors.Grant{}, connectors.ErrNotFound
}
func (g *grantStore) GetConnector(_ context.Context, name string) (connectors.Connector, error) {
	for _, c := range g.conns {
		if c.Name == name {
			return c, nil
		}
	}
	return connectors.Connector{}, connectors.ErrNotFound
}
func (g *grantStore) DecideGrantFrom(_ context.Context, id int64, approve bool, _, _ string, _ connectors.DecisionSource) (connectors.Grant, error) {
	g.decided = append(g.decided, id)
	gr, err := g.GetGrant(context.Background(), id)
	if approve {
		gr.Status = connectors.StatusApproved
	}
	return gr, err
}
func (g *grantStore) Audit(context.Context, connectors.AuditEntry) error { return nil }
func (g *grantStore) DecideGrant(ctx context.Context, id int64, approve bool, by, src string) (connectors.Grant, error) {
	return g.DecideGrantFrom(ctx, id, approve, by, src, connectors.DecisionSource{})
}
func (g *grantStore) CreateConnector(_ context.Context, nc connectors.NewConnector) (connectors.Connector, error) {
	c := connectors.Connector{Name: nc.Name, Hosts: nc.Hosts}
	g.conns = append(g.conns, c)
	return c, nil
}
func (g *grantStore) ListPendingGrants(context.Context) ([]connectors.Grant, error) { return nil, nil }
func (g *grantStore) ListAudit(context.Context, int) ([]connectors.AuditEntry, error) {
	return nil, nil
}

// testConnectors: the App-backed github connector, a stored one with a
// github.com host, and one that reaches somewhere else.
var testConnectors = []connectors.Connector{
	{Name: "github", Hosts: []string{"api.github.com", "github.com"}},
	{Name: "gh-token", Hosts: []string{"github.com"}},
	{Name: "chat", Hosts: []string{"chat.example.com"}},
}

// TestAPoolWorkerHoldingAGitHubGrantIsSkipped: through the real handler, rev-a
// — first by name, otherwise winning — holds an APPROVED grant on a stored
// connector reaching github.com, so the job runs on rev-b. A PENDING github
// request (what ccd files by itself after a failed clone) does not disqualify
// it, nor does an approved grant on a connector reaching elsewhere.
func TestAPoolWorkerHoldingAGitHubGrantIsSkipped(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	st := &grantStore{conns: testConnectors,
		grants: []connectors.Grant{{ID: 1, AgentID: 20, AgentName: "rev-a", Connector: "gh-token", Status: connectors.StatusApproved}}}
	f.s.ext.Connectors = st
	f.gw.reply = "done"
	if out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"})); out["agent"] != "rev-b" {
		t.Fatalf("with rev-a holding an approved GitHub grant the job ran on %v, want rev-b", out["agent"])
	}
	for _, g := range []connectors.Grant{
		{ID: 1, AgentID: 20, AgentName: "rev-a", Connector: "github", Status: connectors.StatusPending},
		{ID: 1, AgentID: 20, AgentName: "rev-a", Connector: "chat", Status: connectors.StatusApproved},
	} {
		st.grants[0] = g
		if out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"})); out["agent"] != "rev-a" {
			t.Fatalf("with rev-a holding a %s grant on %s the job ran on %v, want rev-a", g.Status, g.Connector, out["agent"])
		}
	}
}

// TestAGitHubGrantIsNeverApprovedForAPoolWorker: the one decision every
// approval door takes (decideConnectorGrant: the Connectors tab and a mirrored
// session's connector_request_decide) refuses to approve a pool worker's grant
// on any connector reaching github.com — nothing is decided — while denying it,
// or approving a non-GitHub one, or approving a GitHub one for an agent in no
// pool, goes through. The rule fails closed on a connector it cannot see.
func TestAGitHubGrantIsNeverApprovedForAPoolWorker(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	st := &grantStore{conns: testConnectors, grants: []connectors.Grant{
		{ID: 1, AgentID: 20, Connector: "github", Status: connectors.StatusPending},
		{ID: 2, AgentID: 20, Connector: "gh-token", Status: connectors.StatusPending},
		{ID: 3, AgentID: 20, Connector: "chat", Status: connectors.StatusPending},
		{ID: 4, AgentID: 7, Connector: "github", Status: connectors.StatusPending},
	}}
	f.s.ext.Connectors = st
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		_, err := f.s.decideConnectorGrant(ctx, id, true, connectors.DecisionSource{})
		var refused errPoolWorkerGitHubGrant
		if !errors.As(err, &refused) {
			t.Errorf("approving grant %d of pool worker rev-a = %v, want errPoolWorkerGitHubGrant", id, err)
		}
	}
	if len(st.decided) != 0 {
		t.Fatalf("refused approvals reached the store: %v", st.decided)
	}
	for _, c := range []struct {
		id      int64
		approve bool
	}{{1, false}, {3, true}, {4, true}} {
		if _, err := f.s.decideConnectorGrant(ctx, c.id, c.approve, connectors.DecisionSource{}); err != nil {
			t.Errorf("grant %d approve=%v: %v", c.id, c.approve, err)
		}
	}
	if got := st.decided; len(got) != 3 {
		t.Fatalf("decided %v, want grants 1 (denied), 3 and 4", got)
	}
	if !grantReachesGitHub("vanished", map[string][]string{"chat": {"chat.example.com"}}) {
		t.Fatal("a connector whose hosts are unknown is not treated as reaching GitHub")
	}
}

// TestEnteringACredentialDoesNotApproveAPoolWorkersGitHubGrant: the third
// approval door — entering a credential for a pending request approves it in
// the same step — saves the connector (it reaches github.com) but does not
// approve the pool worker's request, and says why; the same form for an agent
// in no pool approves it.
func TestEnteringACredentialDoesNotApproveAPoolWorkersGitHubGrant(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	st := &grantStore{grants: []connectors.Grant{
		{ID: 5, AgentID: 20, AgentName: "rev-a", Connector: "gh-token", Status: connectors.StatusPending},
		{ID: 6, AgentID: 7, AgentName: "keen-wren", Connector: "gh-token2", Status: connectors.StatusPending},
	}}
	f.s.ext.Connectors = st
	for _, tc := range []struct {
		id      string
		decided bool
	}{{"5", false}, {"6", true}} {
		st.decided = nil
		form := url.Values{"request_id": {tc.id}, "preset": {"custom"}, "hosts": {"github.com"}, "scheme": {"bearer"},
			"secret": {"not-a-real-token"}}
		req := httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.s.handleConnectorCreate(w, req)
		if got := len(st.decided) == 1; got != tc.decided {
			t.Errorf("request %s: approved=%v, want %v (body %q)", tc.id, got, tc.decided, w.Body.String())
		}
		if !tc.decided && !strings.Contains(w.Body.String(), "a member of worker pool reviewers") {
			t.Errorf("request %s: the refusal does not say why: %q", tc.id, w.Body.String())
		}
	}
}

// TestEnteringACredentialApprovesNothingItCannotRead: the form reads the
// request (to show it), then the approval reads it again; when that second
// read fails, nothing is approved — not even for an agent in no pool, whose
// approval would otherwise go through — and the page says so.
func TestEnteringACredentialApprovesNothingItCannotRead(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	st := &grantStore{getsBeforeFailing: 1, grants: []connectors.Grant{
		{ID: 6, AgentID: 7, AgentName: "keen-wren", Connector: "gh-token2", Status: connectors.StatusPending},
	}}
	f.s.ext.Connectors = st
	form := url.Values{"request_id": {"6"}, "preset": {"custom"}, "hosts": {"github.com"}, "scheme": {"bearer"},
		"secret": {"not-a-real-token"}}
	req := httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.s.handleConnectorCreate(w, req)
	if len(st.decided) != 0 {
		t.Fatalf("an unreadable request was approved: %v", st.decided)
	}
	if !strings.Contains(w.Body.String(), "could not be read") {
		t.Fatalf("the page does not say the request could not be read: %q", w.Body.String())
	}
}
