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
	j := jobs.Job{ID: m.next, AgentID: n.AgentID, Agent: n.Agent, WorkerPool: n.WorkerPool, Prompt: n.Prompt, Repo: n.Repo, SHA: n.SHA, PR: n.PR,
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

func (m *memJobs) ByIdempotencyKey(_ context.Context, key string) (jobs.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.rows {
		if j.IdempotencyKey == key {
			return j, nil
		}
	}
	return jobs.Job{}, jobs.ErrNotFound
}

func (m *memJobs) ActiveCounts(_ context.Context, ids []int64) (map[int64]jobs.ActiveCount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := map[int64]jobs.ActiveCount{}
	for _, j := range m.rows {
		if !want[j.AgentID] {
			continue
		}
		c := out[j.AgentID]
		switch j.Status {
		case jobs.StatusQueued:
			c.Queued++
		case jobs.StatusRunning:
			c.Running++
		default:
			continue
		}
		out[j.AgentID] = c
	}
	return out, nil
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

func (s *jobAgents) List(context.Context) ([]agents.Agent, error) {
	out := make([]agents.Agent, 0, len(jobAgentRows))
	for _, a := range jobAgentRows {
		out = append(out, a)
	}
	return out, nil
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
	// echo plays ccd's answer header (agents.HeaderCCWorkdirUsed) on an
	// explicit session's turn: "" echoes the workdir the turn named, as ccd
	// does; noEcho plays a ccd from before job worktrees, which sends none.
	echo   string
	noEcho bool
}

func (g *jobGateway) Chat(ctx context.Context, a agents.Agent, key, msg string, _ func(string)) (string, error) {
	route, _ := agents.TurnRouteFrom(ctx)
	turn := a.Name + " " + key + " cc-session=" + route.Header.Get(agents.HeaderCCSession)
	if wd := route.Header.Get(agents.HeaderCCWorkdir); wd != "" {
		turn += " workdir=" + wd
	}
	g.mu.Lock()
	g.turns = append(g.turns, turn+" "+msg)
	reply, err, block, release, panics := g.reply, g.err, g.block, g.release, g.panics
	echo, noEcho := g.echo, g.noEcho
	g.mu.Unlock()
	if route.OnResponse != nil && !noEcho {
		if echo == "" {
			echo = route.Header.Get(agents.HeaderCCWorkdir)
		}
		h := http.Header{}
		h.Set(agents.HeaderCCWorkdirUsed, echo)
		route.OnResponse(h)
	}
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

// jobWorkspace plays the agent's instance: files by path (a job's worktree's
// under "<job id>/"), and the worktrees made and removed.
type jobWorkspace struct {
	mu        sync.Mutex
	files     map[string]string
	worktrees []string
	removed   []string
	// prepareErr fails PrepareJobWorktree; removeWTErr fails RemoveJobWorktree.
	prepareErr, removeWTErr error
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

func (w *jobWorkspace) PrepareJobWorktree(_ context.Context, a agents.Agent, jobID int64, repo string, pr int, sha string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.worktrees = append(w.worktrees, a.Name+"#"+strconv.FormatInt(jobID, 10)+" "+repo+"#"+strconv.Itoa(pr)+"@"+sha)
	if w.prepareErr != nil {
		return "", w.prepareErr
	}
	return "/data/workspace/.muster-jobs/" + strconv.FormatInt(jobID, 10), nil
}

func (w *jobWorkspace) RemoveJobWorktree(_ context.Context, a agents.Agent, jobID int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removed = append(w.removed, a.Name+"#"+strconv.FormatInt(jobID, 10))
	return w.removeWTErr
}

func (w *jobWorkspace) ReadWorkspaceFile(_ context.Context, _ agents.Agent, jobID int64, rel string, _ int) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := rel
	if jobID != 0 {
		key = strconv.FormatInt(jobID, 10) + "/" + rel
	}
	body, ok := w.files[key]
	if !ok {
		return nil, errors.New("head: cannot open '" + rel + "' for reading: No such file or directory")
	}
	return []byte(body), nil
}

func (w *jobWorkspace) snapshot() (worktrees, removed []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.worktrees...), append([]string(nil), w.removed...)
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
// file's contents verbatim — for a sha job read from the job's own worktree,
// which was made before the turn, named on it, and removed after the read —
// and a missing file fails the job.
func TestAResultPathReturnsTheFileNotTheReply(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "I wrote the review to out/review.md"
	const file = "# Review\n\n1. ok\r\n2. \"quoted\" <tag> & é\n"
	f.ws.files["1/out/review.md"] = file
	f.ws.files["out/review.md"] = "the agent's checkout's copy, which a sha job must not read"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "result_path": "out/review.md",
		"repo": "acme/widgets", "sha": testFullSHA, "pr": 7}))
	if out["status"] != jobs.StatusSucceeded || out["result"] != file {
		t.Fatalf("job = %v result %q, want succeeded with the file %q", out["status"], out["result"], file)
	}
	made, removed := f.ws.snapshot()
	if len(made) != 1 || made[0] != "keen-wren#1 acme/widgets#7@"+testFullSHA {
		t.Fatalf("worktrees made = %q, want [keen-wren#1 acme/widgets#7@%s]", made, testFullSHA)
	}
	if len(removed) != 1 || removed[0] != "keen-wren#1" {
		t.Fatalf("worktrees removed = %q, want [keen-wren#1]", removed)
	}
	if got, want := f.gw.seen(), "keen-wren job-key-401 cc-session=job-key-401 workdir=/data/workspace/.muster-jobs/1 review"; len(got) != 1 || got[0] != want {
		t.Fatalf("turns = %q, want [%q]", got, want)
	}
	f.ws.mu.Lock()
	removes := append([]string(nil), f.ws.removes...)
	f.ws.mu.Unlock()
	if len(removes) != 0 {
		t.Fatalf("a sha job's result file was deleted from the agent's checkout: %q (it goes with the worktree)", removes)
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
		{"agent and worker pool", map[string]any{"agent": "keen-wren", "worker_pool": "reviewers", "prompt": "x"}, http.StatusBadRequest},
		{"bad pool name", map[string]any{"worker_pool": "Reviewers!", "prompt": "x"}, http.StatusBadRequest},
		{"pool sha without repo", map[string]any{"worker_pool": "reviewers", "prompt": "x", "sha": testFullSHA}, http.StatusBadRequest},
		{"no such pool", map[string]any{"worker_pool": "nobody-here", "prompt": "x"}, http.StatusNotFound},
		{"sha without pr", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": testFullSHA}, http.StatusBadRequest},
		{"pr without sha", map[string]any{"agent": "keen-wren", "prompt": "x", "pr": 7}, http.StatusBadRequest},
		{"pr zero", map[string]any{"agent": "keen-wren", "prompt": "x", "sha": testFullSHA, "pr": 0}, http.StatusBadRequest},
		{"repo not owner/name", map[string]any{"agent": "keen-wren", "prompt": "x", "repo": "acme/../x"}, http.StatusBadRequest},
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
	const wantTracked = "the result file CHANGELOG.md is tracked by the repository, so it was left in place, as the turn left it, in the agent's checkout"
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
	const wantErr = JobResultRemoveFailedPrefix + "out/review.md (a later job on this agent naming the same result_path could return it if its own turn writes none): rm: cannot remove: Read-only file system"
	if out["status"] != jobs.StatusSucceeded || out["result"] != "# Again" || out["error"] != wantErr {
		t.Fatalf("job = %v %q error %q; want succeeded with the note %q", out["status"], out["result"], out["error"], wantErr)
	}
	if _, ok := out["reason"]; ok {
		t.Fatalf("a succeeded job carries reason %v", out["reason"])
	}
}

// legacyJobAgents is the job fixture's store WITHOUT the 0030 half
// (agents.SessionRuntimeStore): a session it opens is a legacy one, whose turn
// ccd sends to the agent's primary CLI, in the agent's checkout.
type legacyJobAgents struct{ agents.Store }

// TestAShaJobNeverRunsInTheAgentsOwnSession: on a store that cannot open a
// Claude Code session of the job's own, a sha job fails before any worktree
// or turn — its turn would otherwise reach the CLI working in the agent's
// checkout. A job without a sha still runs there, as it always has.
func TestAShaJobNeverRunsInTheAgentsOwnSession(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Agents = legacyJobAgents{f.ags}
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonInternal || out["error"] != JobNoOwnSessionError {
		t.Fatalf("job = %v/%v %q, want failed/internal %q", out["status"], out["reason"], out["error"], JobNoOwnSessionError)
	}
	if made, _ := f.ws.snapshot(); len(made) != 0 {
		t.Fatalf("worktrees made = %q, want none", made)
	}
	if got := f.gw.seen(); len(got) != 0 {
		t.Fatalf("turns = %q, want none", got)
	}
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("a job without a sha = %v, want succeeded", out)
	}
}

// TestAWorktreeThatFailsToPrepareIsStillRemoved: PrepareJobWorktree may leave
// a half-made worktree when it fails, so the removal runs for that job too.
// And a removal that fails is noted in the job's error, without changing its
// status.
func TestAWorktreeThatFailsToPrepareIsStillRemoved(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.ws.prepareErr = errors.New("git worktree add: fatal: invalid reference")
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonCheckoutFailed {
		t.Fatalf("job = %v/%v, want failed/%s", out["status"], out["reason"], jobs.ReasonCheckoutFailed)
	}
	if _, removed := f.ws.snapshot(); len(removed) != 1 || removed[0] != "keen-wren#1" {
		t.Fatalf("worktrees removed = %q, want [keen-wren#1]", removed)
	}

	f.ws.mu.Lock()
	f.ws.prepareErr, f.ws.removeWTErr = nil, errors.New("rm: cannot remove: Device or resource busy")
	f.ws.mu.Unlock()
	f.gw.reply = "LGTM"
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}))
	const want = JobWorktreeRemoveFailedPrefix + "rm: cannot remove: Device or resource busy"
	if out["status"] != jobs.StatusSucceeded || out["result"] != "LGTM" || out["error"] != want {
		t.Fatalf("job = %v %q error %q, want succeeded LGTM with the note %q", out["status"], out["result"], out["error"], want)
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

// TestAShaJobWhoseRuntimeDoesNotConfirmTheWorktreeIsRefused: a ccd from before
// job worktrees ignores X-Muster-CC-Workdir and answers with no
// X-Muster-CC-Workdir-Used; one that names another directory ran elsewhere.
// Either way the turn's reply — however successful — is not the job's result:
// the job fails workdir_unconfirmed, saying the image is too old. With the
// echo matching, the same job succeeds.
func TestAShaJobWhoseRuntimeDoesNotConfirmTheWorktreeIsRefused(t *testing.T) {
	body := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}
	for _, tc := range []struct {
		name   string
		echo   string
		noEcho bool
		got    string
	}{
		{"no echo", "", true, "no " + agents.HeaderCCWorkdirUsed},
		{"another directory", "/data/workspace/widgets", false, agents.HeaderCCWorkdirUsed + ` "/data/workspace/widgets"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJobFixture(t, testServiceToken)
			f.gw.reply, f.gw.echo, f.gw.noEcho = "LGTM", tc.echo, tc.noEcho
			out := f.await(t, f.submit(t, body))
			want := "the agent's runtime did not confirm it ran the turn in the job's worktree /data/workspace/.muster-jobs/1 " +
				"(it answered with " + tc.got + "): its image is too old for sha jobs, so the turn's output was discarded; " +
				"update the agent's image"
			if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonWorkdirUnconfirmed || out["error"] != want {
				t.Fatalf("job = %v/%v %q\nwant failed/%s %q", out["status"], out["reason"], out["error"], jobs.ReasonWorkdirUnconfirmed, want)
			}
			if _, ok := out["result"]; ok {
				t.Fatalf("a refused job carries the turn's reply as its result: %v", out["result"])
			}
			if _, removed := f.ws.snapshot(); len(removed) != 1 {
				t.Fatalf("worktrees removed = %q, want the job's", removed)
			}
		})
	}
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "LGTM"
	if out := f.await(t, f.submit(t, body)); out["status"] != jobs.StatusSucceeded || out["result"] != "LGTM" {
		t.Fatalf("with the echo matching: job = %v/%v %q, want succeeded LGTM", out["status"], out["reason"], out["result"])
	}
}
