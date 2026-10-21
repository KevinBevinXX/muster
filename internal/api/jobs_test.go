package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// THE MACHINE JOB API (jobs.go), through the real mux and the real service-tier
// wrapper, against in-memory fakes of the job store, the agents store, the
// gateway and the workspace.
// ---------------------------------------------------------------------------

const testServiceToken = "job-api-test-service-token-0123456789abcdef"

// memJobs is jobs.Store in memory, with the PG store's conditional moves and
// per-owner idempotency (one owner here: the service tier is user 1).
type memJobs struct {
	mu   sync.Mutex
	rows map[int64]jobs.Job
	next int64
	// finishFailures is how many Finish calls fail before one succeeds.
	finishFailures int
	finishCalls    int
	// reapedAt records each ReapOverdue's clock.
	reapedAt []time.Time
}

func newMemJobs() *memJobs { return &memJobs{rows: map[int64]jobs.Job{}} }

func (m *memJobs) Create(_ context.Context, n jobs.NewJob) (jobs.Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n.IdempotencyKey != "" {
		for _, j := range m.rows {
			if j.IdempotencyKey == n.IdempotencyKey {
				return j, false, nil
			}
		}
	}
	active := 0
	for _, j := range m.rows {
		if j.AgentID == n.AgentID && !j.Terminal() {
			active++
		}
	}
	if active >= jobs.MaxActivePerAgent {
		return jobs.Job{}, false, jobs.ErrAgentAtCapacity
	}
	m.next++
	j := jobs.Job{ID: m.next, AgentID: n.AgentID, Agent: n.Agent, Prompt: n.Prompt, Repo: n.Repo, SHA: n.SHA,
		ResultPath: n.ResultPath, TimeoutSeconds: n.TimeoutSeconds, IdempotencyKey: n.IdempotencyKey,
		RequestedBy: n.RequestedBy, Status: jobs.StatusQueued, CreatedAt: time.Now()}
	m.rows[j.ID] = j
	return j, true, nil
}

func (m *memJobs) Get(_ context.Context, id int64) (jobs.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.rows[id]
	if !ok {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return j, nil
}

func (m *memJobs) Start(_ context.Context, id int64, acct string, sess int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j.Status != jobs.StatusQueued {
		return false, nil
	}
	j.Status, j.StartedAt, j.CCAccount, j.SessionID = jobs.StatusRunning, time.Now(), acct, sess
	m.rows[id] = j
	return true, nil
}

func (m *memJobs) Finish(_ context.Context, id int64, o jobs.Outcome) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishCalls++
	if m.finishFailures > 0 {
		m.finishFailures--
		return false, errors.New("memJobs: the database is unreachable")
	}
	j := m.rows[id]
	if j.Terminal() {
		return false, nil
	}
	j.Status, j.Reason, j.Error, j.FinishedAt = o.Status, o.Reason, o.Error, time.Now()
	if o.Status == jobs.StatusSucceeded {
		j.Result, j.HasResult = o.Result, true
	}
	m.rows[id] = j
	return true, nil
}

// ReapOverdue applies the PG store's rule (jobs_pg_test.go pins it there).
func (m *memJobs) ReapOverdue(_ context.Context, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapedAt = append(m.reapedAt, now)
	var n int64
	for id, j := range m.rows {
		overdue := (j.Status == jobs.StatusRunning && j.StartedAt.Add(time.Duration(j.TimeoutSeconds)*time.Second+jobs.ReapSlack).Before(now)) ||
			(j.Status == jobs.StatusQueued && j.CreatedAt.Add(jobs.MaxQueueWait).Before(now))
		if overdue {
			j.Status, j.Reason, j.Error, j.FinishedAt = jobs.StatusFailed, jobs.ReasonInterrupted, jobs.AbandonedRunningError, now
			m.rows[id] = j
			n++
		}
	}
	return n, nil
}

func (m *memJobs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func (m *memJobs) row(id int64) jobs.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[id]
}

// jobAgents serves two agents: keen-wren (claude-code, repository acme/widgets,
// account alpha) and plain-otter (gateway). Sessions are created per call and
// recorded with their runtime.
type jobAgents struct {
	stubAgentsStore
	mu       sync.Mutex
	sessions []agents.ChatSession
	msgs     []agents.ChatMessage
}

var jobAgentRows = map[string]agents.Agent{
	"keen-wren":   {ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha", Repo: "acme/widgets", Status: agents.StatusRunning},
	"plain-otter": {ID: 8, Name: "plain-otter", Kind: agents.KindGateway, Status: agents.StatusRunning},
}

func (s *jobAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	a, ok := jobAgentRows[name]
	if !ok {
		return agents.Agent{}, pgx.ErrNoRows
	}
	return a, nil
}

func (s *jobAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range jobAgentRows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *jobAgents) newSession(agentID int64, runtime string) agents.ChatSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(400 + len(s.sessions) + 1)
	sess := agents.ChatSession{ID: id, AgentID: agentID, SessionKey: "job-key-" + strconv.FormatInt(id, 10), Runtime: runtime}
	s.sessions = append(s.sessions, sess)
	return sess
}

func (s *jobAgents) CreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	return s.newSession(agentID, ""), nil
}

// The 0030 half (agents.SessionRuntimeStore), so jobSession takes the explicit
// claude-code runtime path production's PG store takes.
func (s *jobAgents) CreateSessionOn(_ context.Context, agentID int64, _, runtime, _ string) (agents.ChatSession, error) {
	return s.newSession(agentID, runtime), nil
}
func (s *jobAgents) DefaultSession(context.Context, int64, string) (agents.ChatSession, error) {
	return agents.ChatSession{}, errors.New("jobAgents: not expected")
}
func (s *jobAgents) SetDefaultSession(context.Context, int64, int64) error {
	return errors.New("jobAgents: not expected")
}
func (s *jobAgents) SessionByKey(context.Context, int64, string) (agents.ChatSession, error) {
	return agents.ChatSession{}, pgx.ErrNoRows
}

func (s *jobAgents) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return m, nil
}

// jobGateway plays ccd: each turn answers reply, or fails with err, or — with
// block — waits for its context to end. It records each turn's agent, session
// key, message and the Claude Code session header the turn carried.
type jobGateway struct {
	mu    sync.Mutex
	reply string
	err   error
	block bool
	// release, when set, holds each turn until it is closed (or ctx ends).
	release chan struct{}
	// panics makes the turn panic.
	panics bool
	turns  []string
}

func (g *jobGateway) Chat(ctx context.Context, a agents.Agent, key, msg string, _ func(string)) (string, error) {
	route, _ := agents.TurnRouteFrom(ctx)
	g.mu.Lock()
	g.turns = append(g.turns, a.Name+" "+key+" cc-session="+route.Header.Get(agents.HeaderCCSession)+" "+msg)
	reply, err, block, release, panics := g.reply, g.err, g.block, g.release, g.panics
	g.mu.Unlock()
	if panics {
		panic("jobGateway: a bug in the turn")
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return reply, err
}

func (g *jobGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", errors.New("jobGateway: a job's turn is the tool-less one")
}

func (g *jobGateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.turns...)
}

// jobWorkspace plays the agent's instance: files by path, and the checkouts made.
type jobWorkspace struct {
	mu        sync.Mutex
	files     map[string]string
	checkouts []string
	// removes records each RemoveUntrackedWorkspaceFile; removeErr fails it;
	// tracked names the files the repository tracks (left in place).
	removes   []string
	removeErr error
	tracked   map[string]bool
}

func (w *jobWorkspace) RemoveUntrackedWorkspaceFile(_ context.Context, _ agents.Agent, rel string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removes = append(w.removes, rel)
	if w.removeErr != nil {
		return false, w.removeErr
	}
	if w.tracked[rel] {
		return false, nil
	}
	delete(w.files, rel)
	return true, nil
}

func (w *jobWorkspace) CheckoutCommit(_ context.Context, a agents.Agent, sha string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checkouts = append(w.checkouts, a.Name+"@"+sha)
	return nil
}

func (w *jobWorkspace) ReadWorkspaceFile(_ context.Context, _ agents.Agent, rel string, _ int) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	body, ok := w.files[rel]
	if !ok {
		return nil, errors.New("head: cannot open '" + rel + "' for reading: No such file or directory")
	}
	return []byte(body), nil
}

type jobFixture struct {
	s     *Server
	ts    *httptest.Server
	store *memJobs
	ags   *jobAgents
	gw    *jobGateway
	ws    *jobWorkspace
}

func newJobFixture(t *testing.T, serviceToken string) *jobFixture {
	t.Helper()
	f := &jobFixture{store: newMemJobs(), ags: &jobAgents{}, gw: &jobGateway{}, ws: &jobWorkspace{files: map[string]string{}}}
	f.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, ServiceToken: serviceToken}, log.New(io.Discard, "", 0))
	f.s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          f.ags,
		SessionLiveness: stubLiveness{},
		Gateway:         f.gw,
		Jobs:            f.store,
		JobWorkspace:    f.ws,
	})
	f.ts = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.ts.Close)
	return f
}

// call sends one request on the service tier (authed) or with no credential.
func (f *jobFixture) call(t *testing.T, method, path string, body any, authed bool) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+testServiceToken)
		req.Header.Set(ActorHeader, "review-orchestrator")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (f *jobFixture) submit(t *testing.T, body map[string]any) int64 {
	t.Helper()
	code, out := f.call(t, http.MethodPost, "/api/jobs", body, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs = %d %v, want 202", code, out)
	}
	if out["status"] != jobs.StatusQueued {
		t.Fatalf("a new job's status = %v, want queued", out["status"])
	}
	return int64(out["job_id"].(float64))
}

// await polls GET /api/jobs/{id} until the job is terminal.
func (f *jobFixture) await(t *testing.T, id int64) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, out := f.call(t, http.MethodGet, "/api/jobs/"+strconv.FormatInt(id, 10), nil, true)
		if code != http.StatusOK {
			t.Fatalf("GET job %d = %d %v", id, code, out)
		}
		switch out["status"] {
		case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusTimedOut:
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d never finished: last read %v", id, out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestJobRoutesRefuseACallerWithoutTheServiceToken: both routes, no credential
// → 401 and nothing recorded or run; an unarmed server (no service token) → 503
// with the machine-readable marker, for a caller holding a token.
func TestJobRoutesRefuseACallerWithoutTheServiceToken(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	if code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x"}, false); code != http.StatusUnauthorized {
		t.Fatalf("POST with no token = %d %v, want 401", code, out)
	}
	if code, out := f.call(t, http.MethodGet, "/api/jobs/1", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("GET with no token = %d %v, want 401", code, out)
	}
	// The hook token is a different door and does not open this one.
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/api/jobs/1", nil)
	req.Header.Set("X-Muster-Token", testHookToken)
	req.Header.Set(ActorHeader, "review-orchestrator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET with the HOOK token = %d, want 401", resp.StatusCode)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d job(s) recorded by refused requests", n)
	}
	if got := f.gw.seen(); len(got) != 0 {
		t.Fatalf("refused requests ran turns: %q", got)
	}

	unarmed := newJobFixture(t, "")
	code, out := unarmed.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x"}, true)
	if code != http.StatusServiceUnavailable || out[HookUnarmedField] != true {
		t.Fatalf("POST on an unarmed server = %d %v, want 503 with %s:true", code, out, HookUnarmedField)
	}
}

// TestAJobRoundTripsAndStoresTheReplyVerbatim: submit → 202 queued → the turn
// runs on the named agent in a claude-code session of its own → GET returns
// succeeded with the reply byte for byte, the agent's Claude account, both
// timestamps, and who asked.
func TestAJobRoundTripsAndStoresTheReplyVerbatim(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	const reply = "  LGTM with one nit:\n\n- `foo()` leaks on error — ✓ fix\n\ttrailing tab\t\n"
	f.gw.reply = reply
	id := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review the change"})
	out := f.await(t, id)

	if out["status"] != jobs.StatusSucceeded {
		t.Fatalf("status = %v (%v), want succeeded", out["status"], out)
	}
	if out["result"] != reply {
		t.Fatalf("result = %q, want the reply verbatim %q", out["result"], reply)
	}
	if out["agent"] != "keen-wren" || out["cc_account"] != "alpha" {
		t.Fatalf("agent/cc_account = %v/%v, want keen-wren/alpha", out["agent"], out["cc_account"])
	}
	for _, k := range []string{"started_at", "finished_at", "created_at"} {
		if s, _ := out[k].(string); s == "" {
			t.Fatalf("%s = %v, want a timestamp", k, out[k])
		}
	}
	if _, ok := out["reason"]; ok {
		t.Fatalf("a succeeded job carries reason %v", out["reason"])
	}
	if got := f.store.row(id).RequestedBy; got != "review-orchestrator" {
		t.Fatalf("requested_by = %q, want the asserted actor", got)
	}
	// One turn, in a session of its own on the claude-code runtime: the session
	// key and the Claude Code session header are the same new key.
	if got, want := f.gw.seen(), []string{"keen-wren job-key-401 cc-session=job-key-401 review the change"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("turns = %q, want %q", got, want)
	}
	if got := out["session_id"]; got != float64(401) {
		t.Fatalf("session_id = %v, want 401", got)
	}
}

// TestARepeatedIdempotencyKeyAnswersTheFirstJob: the second POST with the same
// key creates nothing, runs nothing, and answers the first job's id with 200
// and deduplicated:true.
func TestARepeatedIdempotencyKeyAnswersTheFirstJob(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	body := map[string]any{"agent": "keen-wren", "prompt": "review", "idempotency_key": "review-pr-42"}
	id := f.submit(t, body)
	f.await(t, id)

	code, out := f.call(t, http.MethodPost, "/api/jobs", body, true)
	if code != http.StatusOK || out["deduplicated"] != true || out["job_id"] != float64(id) {
		t.Fatalf("repeat POST = %d %v, want 200 deduplicated:true job_id %d", code, out, id)
	}
	if out["status"] != jobs.StatusSucceeded || out["result"] != "done" {
		t.Fatalf("the repeat answered %v/%v, want the first job's succeeded/done", out["status"], out["result"])
	}
	if n := f.store.count(); n != 1 {
		t.Fatalf("%d jobs recorded, want 1", n)
	}
	if n := len(f.gw.seen()); n != 1 {
		t.Fatalf("%d turns ran, want 1", n)
	}
	// A different key is a different job.
	other := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "idempotency_key": "review-pr-43"})
	if other == id {
		t.Fatalf("a new key answered the old job %d", id)
	}
}

// TestARateLimitedTurnFailsTheJobWithThatReason: ccd's typed rate_limited ends
// the job failed with reason rate_limited and ccd's own message, so a caller
// can re-queue it on another account. auth_failed likewise.
func TestARateLimitedTurnFailsTheJobWithThatReason(t *testing.T) {
	for _, tc := range []struct{ typ, msg string }{
		{"rate_limited", "You've hit your session limit · resets 9:45pm"},
		{"auth_failed", "Invalid API key · Please run /login"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			f := newJobFixture(t, testServiceToken)
			f.gw.err = &agents.RuntimeError{Status: 429, Type: tc.typ, Message: tc.msg}
			out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
			if out["status"] != jobs.StatusFailed || out["reason"] != tc.typ || out["error"] != tc.msg {
				t.Fatalf("job = %v/%v/%q, want failed/%s/%q", out["status"], out["reason"], out["error"], tc.typ, tc.msg)
			}
			if _, ok := out["result"]; ok {
				t.Fatalf("a failed job carries a result: %v", out["result"])
			}
			if out["cc_account"] != "alpha" {
				t.Fatalf("cc_account = %v, want alpha (the account to rest)", out["cc_account"])
			}
		})
	}
}

// TestAJobPastItsTimeoutEndsTimedOut: a turn that never answers is cut off at
// timeout_s (here 1 unit of 20ms) and the job ends timed_out, reason timeout.
func TestAJobPastItsTimeoutEndsTimedOut(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.jobSecond = 20 * time.Millisecond
	f.gw.block = true
	start := time.Now()
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "timeout_s": 1}))
	if out["status"] != jobs.StatusTimedOut || out["reason"] != jobs.ReasonTimeout {
		t.Fatalf("job = %v/%v (%v), want timed_out/timeout", out["status"], out["reason"], out["error"])
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the timeout took %v to land", el)
	}
}

// TestAResultPathReturnsTheFileNotTheReply: with result_path the result is that
// file's contents verbatim, and a missing file fails the job.
func TestAResultPathReturnsTheFileNotTheReply(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "I wrote the review to out/review.md"
	const file = "# Review\n\n1. ok\r\n2. \"quoted\" <tag> & é\n"
	f.ws.files["out/review.md"] = file
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/review.md",
		"repo": "acme/widgets", "sha": testFullSHA}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != file {
		t.Fatalf("job = %v result %q, want succeeded with the file %q", out["status"], out["result"], file)
	}
	f.ws.mu.Lock()
	got := append([]string(nil), f.ws.checkouts...)
	f.ws.mu.Unlock()
	if len(got) != 1 || got[0] != "keen-wren@"+testFullSHA {
		t.Fatalf("checkouts = %q, want [keen-wren@%s]", got, testFullSHA)
	}

	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/missing.md"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonResultReadFailed {
		t.Fatalf("missing file: job = %v/%v, want failed/%s", out["status"], out["reason"], jobs.ReasonResultReadFailed)
	}
}

// TestJobSubmissionsTheAPIRefuses: each refusal is answered before anything is
// recorded.
func TestJobSubmissionsTheAPIRefuses(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"no prompt", map[string]any{"agent": "keen-wren"}, http.StatusBadRequest},
		{"no agent", map[string]any{"prompt": "x"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"agent": "keen-wren", "prompt": "x", "results_path": "a"}, http.StatusBadRequest},
		{"bad sha", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": "HEAD~1"}, http.StatusBadRequest},
		{"abbreviated sha", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": "0123abc"}, http.StatusBadRequest},
		{"39-hex sha", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": testFullSHA[:39]}, http.StatusBadRequest},
		{"escaping path", map[string]any{"agent": "keen-wren", "prompt": "x", "result_path": "../etc/passwd"}, http.StatusBadRequest},
		{"absolute path", map[string]any{"agent": "keen-wren", "prompt": "x", "result_path": "/etc/passwd"}, http.StatusBadRequest},
		{".git path", map[string]any{"agent": "keen-wren", "prompt": "x", "result_path": ".git/config"}, http.StatusBadRequest},
		{"timeout too long", map[string]any{"agent": "keen-wren", "prompt": "x", "timeout_s": jobs.MaxTimeoutSeconds + 1}, http.StatusBadRequest},
		{"worker pool", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, http.StatusNotImplemented},
		{"no such agent", map[string]any{"agent": "nobody", "prompt": "x"}, http.StatusNotFound},
		{"gateway-kind agent", map[string]any{"agent": "plain-otter", "prompt": "x"}, http.StatusConflict},
		{"another repo", map[string]any{"agent": "keen-wren", "prompt": "x", "repo": "acme/other"}, http.StatusConflict},
	}
	for _, tc := range cases {
		code, out := f.call(t, http.MethodPost, "/api/jobs", tc.body, true)
		if code != tc.code {
			t.Errorf("%s: POST = %d %v, want %d", tc.name, code, out, tc.code)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
	if code, _ := f.call(t, http.MethodGet, "/api/jobs/999", nil, true); code != http.StatusNotFound {
		t.Fatalf("GET of an unknown job = %d, want 404", code)
	}
}

// TestJobsOnOneAgentRunOneAtATime: two jobs on the same agent never have turns
// in flight together (a sha checkout moves the agent's one working tree).
func TestJobsOnOneAgentRunOneAtATime(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	var mu sync.Mutex
	inFlight, peak := 0, 0
	gw := &overlapGateway{enter: func() {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	f.s.ext.Gateway = gw
	ids := []int64{}
	for i := 0; i < 3; i++ {
		ids = append(ids, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review " + strconv.Itoa(i)}))
	}
	for _, id := range ids {
		if out := f.await(t, id); out["status"] != jobs.StatusSucceeded {
			t.Fatalf("job %d = %v", id, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("%d turns ran at once on one agent, want 1", peak)
	}
}

type overlapGateway struct{ enter func() }

func (g *overlapGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	g.enter()
	return "ok", nil
}

func (g *overlapGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", errors.New("not expected")
}

// testFullSHA is a sha a job may name.
const testFullSHA = "0123456789abcdef0123456789abcdef01234567"

// TestAnAbbreviatedSHAIsRefusedWithWhy: git cannot fetch an abbreviation, so a
// short sha is a 400 that says so, and nothing is recorded or checked out.
func TestAnAbbreviatedSHAIsRefusedWithWhy(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": "0123abc"}, true)
	const want = "sha must be a full commit id: 40 lowercase hex characters (an abbreviated sha cannot be fetched)"
	if code != http.StatusBadRequest || out["error"] != want {
		t.Fatalf("POST with a 7-hex sha = %d %v, want 400 %q", code, out, want)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d job(s) recorded", n)
	}
}

// TestAFailedOutcomeWriteIsRetried: the first two writes of the outcome fail
// (the database blipped); the third lands, so the job does not stay running.
func TestAFailedOutcomeWriteIsRetried(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.jobFinishBackoff = time.Millisecond
	f.gw.reply = "done"
	f.store.mu.Lock()
	f.store.finishFailures = 2
	f.store.mu.Unlock()
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != "done" {
		t.Fatalf("job = %v/%v, want succeeded/done", out["status"], out["result"])
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if f.store.finishCalls != 3 {
		t.Fatalf("%d outcome writes, want 3 (two failed, one landed)", f.store.finishCalls)
	}
}

// TestAPanicInTheTurnFailsTheJob: a panic while running a job is recovered by
// safeGo, and the job is recorded failed/internal instead of staying running
// until the reaper.
func TestAPanicInTheTurnFailsTheJob(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.panics = true
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonInternal ||
		out["error"] != "muster failed while running the job (a recovered panic)" {
		t.Fatalf("job = %v/%v %q, want failed/internal", out["status"], out["reason"], out["error"])
	}
	// The lane was released: the next job on the agent runs.
	f.gw.mu.Lock()
	f.gw.panics, f.gw.reply = false, "ok"
	f.gw.mu.Unlock()
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "again"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("the next job = %v", out)
	}
}

// TestTheReaperLeavesALiveJobAndItsResultLands: rolling update, scenario A. A
// job is running here when a reaper pass runs (as the new pod's boot pass
// would); it is inside its deadline, so it stays running, and when the turn
// ends its real result is recorded. The reaper reads its injected clock.
func TestTheReaperLeavesALiveJobAndItsResultLands(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "the real review"
	f.gw.release = make(chan struct{})
	id := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "timeout_s": 60})
	deadline := time.Now().Add(5 * time.Second)
	for f.store.row(id).Status != jobs.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("the job never started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	clock := time.Now()
	f.s.jobNow = func() time.Time { return clock }
	f.s.reapJobsOnce(context.Background())
	if got := f.store.row(id).Status; got != jobs.StatusRunning {
		t.Fatalf("a job inside its deadline was reaped: %s", got)
	}
	close(f.gw.release)
	if out := f.await(t, id); out["status"] != jobs.StatusSucceeded || out["result"] != "the real review" {
		t.Fatalf("job = %v/%v, want the real result", out["status"], out["result"])
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if len(f.store.reapedAt) != 1 || !f.store.reapedAt[0].Equal(clock) {
		t.Fatalf("ReapOverdue clocks = %v, want [%v] (the injected clock)", f.store.reapedAt, clock)
	}
}

// TestTheReaperRunsAtOnceAndOnItsInterval: RunJobReaper's first pass is
// immediate (the boot pass) and it keeps passing until its context ends; with
// no job store it returns at once.
func TestTheReaperRunsAtOnceAndOnItsInterval(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.s.RunJobReaper(ctx, 5*time.Millisecond) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.store.mu.Lock()
		n := len(f.store.reapedAt)
		f.store.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reaper passes in 5s, want at least 3", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJobReaper did not return when its context ended")
	}
	bare := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	returned := make(chan struct{})
	go func() { defer close(returned); bare.RunJobReaper(context.Background(), time.Millisecond) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJobReaper with no job store did not return")
	}
}

// TestASubmissionPastTheAgentsCapacityIs429: with MaxActivePerAgent jobs
// queued or running on the agent, the next submission is refused 429 and
// recorded nowhere; a repeated key is still answered.
func TestASubmissionPastTheAgentsCapacityIs429(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.release = make(chan struct{})
	defer close(f.gw.release)
	for i := 0; i < 4; i++ {
		f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "p", "idempotency_key": "k" + strconv.Itoa(i)})
	}
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "p"}, true)
	const want = `agent "keen-wren" already has 4 jobs queued or running (the limit); submit again once one finishes`
	if code != http.StatusTooManyRequests || out["error"] != want {
		t.Fatalf("the 5th submission = %d %v, want 429 %q", code, out, want)
	}
	if n := f.store.count(); n != 4 {
		t.Fatalf("%d jobs recorded, want 4", n)
	}
	if code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "p", "idempotency_key": "k0"}, true); code != http.StatusOK || out["deduplicated"] != true {
		t.Fatalf("a repeated key at capacity = %d %v, want 200 deduplicated", code, out)
	}
}

// TestAResultFileIsDeletedOnceRead: the runner deletes a job's result_path file
// after a SUCCESSFUL read only — a failed read leaves the agent's only copy, and
// is never even offered to the delete — and a tracked file or a delete that
// fails is noted in the job's error without unmaking its success.
func TestAResultFileIsDeletedOnceRead(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "wrote it"
	f.ws.files["out/review.md"] = "# Review"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/review.md"}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Review" {
		t.Fatalf("job = %v %q", out["status"], out["result"])
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("a clean success carries error %v", out["error"])
	}
	f.ws.mu.Lock()
	_, left := f.ws.files["out/review.md"]
	removes := append([]string(nil), f.ws.removes...)
	f.ws.mu.Unlock()
	if left || len(removes) != 1 || removes[0] != "out/review.md" {
		t.Fatalf("after the job: file left %v, removes %q; want it deleted once", left, removes)
	}

	// A failed read keeps the file: it is the only copy of the agent's output.
	f.ws.mu.Lock()
	f.ws.files["out/bin.dat"] = "\xff\xfe"
	f.ws.mu.Unlock()
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/bin.dat"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonResultReadFailed {
		t.Fatalf("non-UTF-8 job = %v/%v", out["status"], out["reason"])
	}
	f.ws.mu.Lock()
	_, left = f.ws.files["out/bin.dat"]
	removes = append([]string(nil), f.ws.removes...)
	f.ws.mu.Unlock()
	if !left || len(removes) != 1 {
		t.Fatalf("after a failed read: file left %v, removes %q; want it kept and no delete attempted", left, removes)
	}

	// A tracked file is left in place, with a note; the job still succeeds.
	f.ws.mu.Lock()
	f.ws.files["CHANGELOG.md"] = "# Changelog"
	f.ws.tracked = map[string]bool{"CHANGELOG.md": true}
	f.ws.mu.Unlock()
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "CHANGELOG.md"}))
	const wantTracked = "the result file CHANGELOG.md is tracked by the repository, so it was left in place; if the turn modified it, the tree stays dirty and the next sha job on this agent fails checkout_failed"
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Changelog" || out["error"] != wantTracked {
		t.Fatalf("tracked job = %v %q error %q; want succeeded with the note %q", out["status"], out["result"], out["error"], wantTracked)
	}
	f.ws.mu.Lock()
	_, left = f.ws.files["CHANGELOG.md"]
	f.ws.mu.Unlock()
	if !left {
		t.Fatal("a tracked result file was deleted")
	}

	// A delete that fails: the job still succeeds with its result, and error
	// says what was left behind.
	f.ws.mu.Lock()
	f.ws.files["out/review.md"] = "# Again"
	f.ws.removeErr = errors.New("rm: cannot remove: Read-only file system")
	f.ws.mu.Unlock()
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/review.md"}))
	const wantErr = JobResultRemoveFailedPrefix + "out/review.md (the next sha job on this agent may be refused as dirty): rm: cannot remove: Read-only file system"
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Again" || out["error"] != wantErr {
		t.Fatalf("job = %v %q error %q; want succeeded with the note %q", out["status"], out["result"], out["error"], wantErr)
	}
	if _, ok := out["reason"]; ok {
		t.Fatalf("a succeeded job carries reason %v", out["reason"])
	}
}

// TestAResultPathUnderDotGitIsRefused: the result file is deleted after it is
// read, so a path into a repository's own metadata is a 400 — at the top and
// nested, in any case — and nothing is recorded.
func TestAResultPathUnderDotGitIsRefused(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	for _, p := range []string{".git/config", "vendor/lib/.git/HEAD", ".GIT/index"} {
		code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x", "result_path": p}, true)
		const want = "result_path must be a clean relative path inside the agent's working directory " +
			"(no leading /, no ., .. or .git elements, at most 512 bytes)"
		if code != http.StatusBadRequest || out["error"] != want {
			t.Errorf("result_path %q = %d %v, want 400 %q", p, code, out, want)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d job(s) recorded", n)
	}
}
