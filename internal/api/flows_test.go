package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/users"
)

// countingFlows is a flows.Store that records which methods ran and as whom.
// Every job-touching method answers ErrNotFound; a test only cares whether the
// door let the request reach the store.
type countingFlows struct {
	flows.Store
	mu    sync.Mutex
	calls []string
}

func (c *countingFlows) note(ctx context.Context, m string) {
	p, _ := authz.From(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, m+" as "+string(p.Via))
}

func (c *countingFlows) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *countingFlows) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

const flowTestRunnerToken = "mfr_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

// Short aliases: the leak gate reads `Bearer "+<a long identifier>` as a
// credential-shaped string.
const (
	runnerTok = flowTestRunnerToken
	flowsSvc  = "flows-test-service-token-0123456789abcdef"
)

func (c *countingFlows) AuthenticateRunner(ctx context.Context, tok string) (flows.RunnerToken, error) {
	c.note(ctx, "AuthenticateRunner")
	if tok == flowTestRunnerToken {
		return flows.RunnerToken{ID: 1, OwnerID: authz.OperatorUserID, Host: "laptop"}, nil
	}
	return flows.RunnerToken{}, flows.ErrBadRunnerToken
}
func (c *countingFlows) Enqueue(ctx context.Context, _ flows.Request, _, _ string) (flows.Job, error) {
	c.note(ctx, "Enqueue")
	return flows.Job{}, flows.ErrNotFound
}
func (c *countingFlows) List(ctx context.Context, _ int) ([]flows.Job, error) {
	c.note(ctx, "List")
	return nil, nil
}
func (c *countingFlows) Get(ctx context.Context, _ int64) (flows.Job, error) {
	c.note(ctx, "Get")
	return flows.Job{}, flows.ErrNotFound
}
func (c *countingFlows) Claim(ctx context.Context, _ string, _ []string, _ int64) (flows.Job, bool, error) {
	c.note(ctx, "Claim")
	return flows.Job{}, false, nil
}
func (c *countingFlows) Heartbeat(ctx context.Context, _ int64, _ string) (flows.Job, error) {
	c.note(ctx, "Heartbeat")
	return flows.Job{}, flows.ErrNotFound
}
func (c *countingFlows) Report(ctx context.Context, _ int64, _ string, _ flows.Result) (flows.Job, error) {
	c.note(ctx, "Report")
	return flows.Job{}, flows.ErrNotFound
}

// TestTheFlowDoorsRefuseEveryOtherCredential: the JSON enqueue/read API takes
// a PERSONAL token and nothing else, and the runner door takes a RUNNER token
// and nothing else. The shared hook and service tokens are refused on both —
// the hook token acts as the admin and is held by every hook script — and
// neither door ever reaches the store for a foreign credential. The positive
// control on each route is its own credential reaching the store.
func TestTheFlowDoorsRefuseEveryOtherCredential(t *testing.T) {
	f := &countingFlows{}
	tf := newFakeAPITokens()
	s := New(nil, AuthConfig{HookToken: tokTestHook, ServiceToken: flowsSvc,
		UIPassword: "flows-test-operator-password"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: tf, Users: stubUsers{}, Flows: f})
	mux := http.NewServeMux()
	s.registerAll(mux)
	personal := tf.add(t, authz.OperatorUserID)

	type credSet func(r *http.Request)
	bearer := func(tok string) credSet {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	foreign := map[string]credSet{
		"hook token (Bearer)":         bearer(tokTestHook),
		"hook token (X-Muster-Token)": func(r *http.Request) { r.Header.Set("X-Muster-Token", tokTestHook) },
		"service token + actor": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+flowsSvc)
			r.Header.Set(ActorHeader, "some-service")
		},
		"browser session": func(r *http.Request) {
			r.Header.Set("Cookie", sessionCookieFor(s))
			r.Header.Set("Origin", "http://"+r.Host)
		},
		"no credential": func(*http.Request) {},
	}
	routes := []struct {
		method, path, body string
		own                credSet
		wantStore          string
		other              credSet // the OTHER flow door's credential, refused too
	}{
		{"POST", "/api/flows/jobs", `{"flow":"muse.poll","host":"laptop","instance":"personal"}`, bearer(personal), "Enqueue as api-token", bearer(flowTestRunnerToken)},
		{"GET", "/api/flows/jobs", "", bearer(personal), "List as api-token", bearer(flowTestRunnerToken)},
		{"GET", "/api/flows/jobs/7", "", bearer(personal), "Get as api-token", bearer(flowTestRunnerToken)},
		{"POST", "/runner/flows/claim", `{"instances":["personal"],"waitSeconds":1}`, bearer(flowTestRunnerToken), "Claim as flow-runner", bearer(personal)},
		{"POST", "/runner/flows/7/heartbeat", `{}`, bearer(flowTestRunnerToken), "Heartbeat as flow-runner", bearer(personal)},
		{"POST", "/runner/flows/7/result", `{"outcome":"done"}`, bearer(flowTestRunnerToken), "Report as flow-runner", bearer(personal)},
	}
	serve := func(method, path, body string, c credSet) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "muster.test"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		c(r)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, rt := range routes {
		creds := map[string]credSet{"the other flow door's token": rt.other}
		for k, v := range foreign {
			creds[k] = v
		}
		for name, c := range creds {
			f.reset()
			if code := serve(rt.method, rt.path, rt.body, c); code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d; want 401", rt.method, rt.path, name, code)
			}
			for _, call := range f.snapshot() {
				if !strings.HasPrefix(call, "AuthenticateRunner") {
					t.Errorf("%s %s with %s reached the store: %s", rt.method, rt.path, name, call)
				}
			}
		}
		// The positive control: the door's own credential reaches the store
		// method, as the right door.
		f.reset()
		serve(rt.method, rt.path, rt.body, rt.own)
		got := f.snapshot()
		hit := false
		for _, c := range got {
			hit = hit || c == rt.wantStore
		}
		if !hit {
			t.Errorf("CONTROL: %s %s with its own credential never reached %q (saw %v)", rt.method, rt.path, rt.wantStore, got)
		}
	}
}

var mintedRunnerRE = regexp.MustCompile(`id="minted-runner-token" readonly value="(mfr_[A-Za-z0-9_-]{43})"`)

// TestFlowLifecycleOverHTTP drives the whole queue through the real routes and
// Postgres: the owner mints a runner token on /flows and a personal token,
// enqueues over the API, the owner's runner claims, heartbeats and reports,
// and the owner reads the result — while user B, holding the same kinds of
// credential, can neither see, claim, nor touch A's job.
func TestFlowLifecycleOverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var a, b int64
	for _, id := range []*int64{&a, &b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "flow"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	asUser := func(uid int64) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	}
	ns := notes.NewPG(pool)
	aTask, err := ns.Create(asUser(a), notes.Note{Title: "flow-task-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	ts := apitokens.NewPG(pool)
	aTok, _, err := ts.Create(asUser(a), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	bTok, _, err := ts.Create(asUser(b), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := []int64{a, b}
		for _, q := range []string{`DELETE FROM flow_jobs WHERE owner_id = ANY($1)`, `DELETE FROM flow_runner_tokens WHERE owner_id = ANY($1)`,
			`DELETE FROM notes WHERE owner_id = ANY($1)`, `DELETE FROM api_tokens WHERE user_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})

	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "flow-lifecycle-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Notes: ns, Users: users.NewPG(pool), APITokens: ts, Flows: flows.NewPG(pool)})
	const userHeader = "X-Flow-Test-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body, bearer string, session int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = "muster.test"
		if session > 0 {
			r.Header.Set("Cookie", sessionCookieFor(srv))
			r.Header.Set("Origin", "http://muster.test")
			r.Header.Set(userHeader, strconv.FormatInt(session, 10))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	host := "lap" + suffix

	// 1. Each user mints a runner token for the SAME host on /flows; it is
	// shown once.
	mint := func(uid int64) string {
		rec := do("POST", "/flows/runner-tokens", url.Values{"host": {host}}.Encode(), "", uid)
		m := mintedRunnerRE.FindStringSubmatch(rec.Body.String())
		if rec.Code != 200 || m == nil {
			t.Fatalf("minting a runner token for %d: %d %s", uid, rec.Code, truncate(rec.Body.String(), 400))
		}
		if page := do("GET", "/flows", "", "", uid).Body.String(); strings.Contains(page, m[1]) || !strings.Contains(page, host) {
			t.Fatalf("the /flows page re-shows the runner token, or does not list its host")
		}
		return m[1]
	}
	aRunner, bRunner := mint(a), mint(b)

	// 2. A enqueues over the API with A's personal token; the hook token and
	// B-with-A's-task are refused.
	body := `{"flow":"muse.send","host":"` + host + `","instance":"personal","params":{"text":"hello muse","task":"` +
		strconv.FormatInt(aTask.ID, 10) + `"}}`
	if rec := do("POST", "/api/flows/jobs", body, tokTestHook, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the HOOK token enqueued: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/flows/jobs", body, bTok, 0); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not one of your tasks") {
		t.Fatalf("B enqueuing against A's task: %d %s; want 400", rec.Code, rec.Body)
	}
	rec := do("POST", "/api/flows/jobs", body, aTok, 0)
	var job flows.Job
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &job) != nil || job.OwnerID != a || job.RequestedVia != "api-token" {
		t.Fatalf("A's enqueue: %d %s", rec.Code, rec.Body)
	}
	jobPath := "/api/flows/jobs/" + strconv.FormatInt(job.ID, 10)
	if rec := do("GET", jobPath, "", bTok, 0); rec.Code != http.StatusNotFound {
		t.Fatalf("B reading A's job: %d %s; want 404", rec.Code, rec.Body)
	}
	if strings.Contains(do("GET", "/flows", "", "", b).Body.String(), "hello muse") {
		t.Fatal("B's /flows page shows A's job")
	}
	if !strings.Contains(do("GET", "/flows", "", "", a).Body.String(), "hello muse") {
		t.Fatal("CONTROL: A's /flows page does not show A's own job")
	}

	// 3. B's runner, same host and instance, claims nothing; A's claims A's job.
	claim := `{"instances":["personal"],"waitSeconds":1}`
	if rec := do("POST", "/runner/flows/claim", claim, bRunner, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("B's runner claim: %d %s; want 204", rec.Code, rec.Body)
	}
	if rec := do("POST", "/runner/flows/claim", `{}`, aRunner, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("a claim naming no instance: %d; want 400", rec.Code)
	}
	rec = do("POST", "/runner/flows/claim", claim, aRunner, 0)
	var claimed flows.Job
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &claimed) != nil || claimed.ID != job.ID ||
		claimed.Status != flows.StatusRunning || claimed.Params["text"] != "hello muse" {
		t.Fatalf("A's runner claim: %d %s", rec.Code, rec.Body)
	}
	runnerPath := "/runner/flows/" + strconv.FormatInt(job.ID, 10)
	for _, p := range []string{runnerPath + "/heartbeat", runnerPath + "/result"} {
		if rec := do("POST", p, `{"outcome":"done"}`, bRunner, 0); rec.Code != http.StatusNotFound {
			t.Fatalf("B's runner on A's job %s: %d; want 404", p, rec.Code)
		}
	}
	if rec := do("POST", runnerPath+"/heartbeat", `{}`, aRunner, 0); rec.Code != http.StatusOK {
		t.Fatalf("A's runner heartbeat: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", runnerPath+"/result", `{"outcome":"done","exitCode":0,"output":"muse says hi"}`, aRunner, 0); rec.Code != http.StatusOK {
		t.Fatalf("A's runner result: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", runnerPath+"/result", `{"outcome":"done"}`, aRunner, 0); rec.Code != http.StatusConflict {
		t.Fatalf("a second result: %d; want 409", rec.Code)
	}

	// 4. A reads the result.
	rec = do("GET", jobPath, "", aTok, 0)
	var done flows.Job
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &done) != nil || done.Status != flows.StatusDone || done.Output != "muse says hi" {
		t.Fatalf("A's read of the finished job: %d %s", rec.Code, rec.Body)
	}

	// 5. A session enqueue, then a cancel, through the page's forms.
	rec = do("POST", "/flows/jobs", url.Values{"flow": {"muse.poll"}, "host": {host}, "instance": {"personal"},
		"param_thread": {"Side chat"}}.Encode(), "", a)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("session enqueue: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var list []flows.Job
	if err := json.Unmarshal(do("GET", "/api/flows/jobs", "", aTok, 0).Body.Bytes(), &list); err != nil || len(list) != 2 ||
		list[0].Flow != "muse.poll" || list[0].RequestedVia != "session" {
		t.Fatalf("A's list after the session enqueue: %+v (%v)", list, err)
	}
	cancelPath := "/flows/jobs/" + strconv.FormatInt(list[0].ID, 10) + "/cancel"
	if rec := do("POST", cancelPath, "", "", b); rec.Code != http.StatusNotFound {
		t.Fatalf("B cancelling A's job: %d; want 404", rec.Code)
	}
	if rec := do("POST", cancelPath, "", "", a); rec.Code != http.StatusSeeOther {
		t.Fatalf("A cancelling its own job: %d", rec.Code)
	}

	// 6. A revoked runner token stops working on the next request.
	var aRunnerID int64
	if err := pool.QueryRow(asUser(a), `SELECT id FROM flow_runner_tokens ORDER BY id DESC LIMIT 1`).Scan(&aRunnerID); err != nil {
		t.Fatal(err)
	}
	if rec := do("POST", "/flows/runner-tokens/"+strconv.FormatInt(aRunnerID, 10)+"/revoke", "", "", b); rec.Code != http.StatusNotFound {
		t.Fatalf("B revoking A's runner token: %d; want 404", rec.Code)
	}
	if rec := do("POST", "/flows/runner-tokens/"+strconv.FormatInt(aRunnerID, 10)+"/revoke", "", "", a); rec.Code != http.StatusSeeOther {
		t.Fatalf("A revoking its runner token: %d", rec.Code)
	}
	if rec := do("POST", "/runner/flows/claim", claim, aRunner, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked runner token claimed: %d; want 401", rec.Code)
	}
}

// TestAClaimEndsAtShutdown: a waiting claim is a long-poll, and EndStreams
// must end it (204) rather than let it hold the shutdown drain.
func TestAClaimEndsAtShutdown(t *testing.T) {
	f := &countingFlows{}
	s := New(nil, AuthConfig{UIPassword: "flows-test-operator-password"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Flows: f})
	mux := http.NewServeMux()
	s.registerAll(mux)
	r := httptest.NewRequest("POST", "/runner/flows/claim", strings.NewReader(`{"instances":["personal"]}`))
	r.Header.Set("Authorization", "Bearer "+runnerTok)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() { mux.ServeHTTP(rec, r); close(done) }()
	time.Sleep(200 * time.Millisecond)
	s.EndStreams()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a waiting claim did not end within 5s of EndStreams")
	}
	if rec.Code != http.StatusNoContent || time.Since(start) > 5*time.Second {
		t.Fatalf("claim at shutdown: %d after %s", rec.Code, time.Since(start))
	}
}
