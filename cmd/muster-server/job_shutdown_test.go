package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/jobs"
)

// shutdownJobStore is jobs.Store in memory, with the PG store's conditional
// moves. It EMBEDS jobs.Store, so any other call panics instead of passing.
type shutdownJobStore struct {
	jobs.Store
	mu   sync.Mutex
	rows map[int64]jobs.Job
	next int64
}

func (m *shutdownJobStore) Create(_ context.Context, n jobs.NewJob) (jobs.Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	j := jobs.Job{ID: m.next, AgentID: n.AgentID, Agent: n.Agent, Prompt: n.Prompt, TimeoutSeconds: n.TimeoutSeconds,
		Status: jobs.StatusQueued, CreatedAt: time.Now()}
	m.rows[j.ID] = j
	return j, true, nil
}

func (m *shutdownJobStore) Start(_ context.Context, id int64, acct string, sess int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j.Status != jobs.StatusQueued {
		return false, nil
	}
	j.Status, j.StartedAt = jobs.StatusRunning, time.Now()
	m.rows[id] = j
	return true, nil
}

func (m *shutdownJobStore) Finish(_ context.Context, id int64, o jobs.Outcome) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.rows[id]
	if !ok || j.Terminal() {
		return false, nil
	}
	j.Status, j.Reason, j.Error = o.Status, o.Reason, o.Error
	m.rows[id] = j
	return true, nil
}

func (m *shutdownJobStore) ReapOverdue(context.Context, time.Time) (int64, error) { return 0, nil }

func (m *shutdownJobStore) row(id int64) jobs.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[id]
}

// jobAgent serves one claude-code agent and the job's chat session. It EMBEDS
// agents.Store, so any other call panics instead of passing.
type jobAgent struct{ agents.Store }

var shutdownAgent = agents.Agent{ID: 41, Name: "late-tern", Kind: agents.KindClaudeCode, Status: agents.StatusRunning}

func (jobAgent) GetByName(_ context.Context, name string) (agents.Agent, error) {
	if name != shutdownAgent.Name {
		return agents.Agent{}, errors.New("no rows")
	}
	return shutdownAgent, nil
}
func (jobAgent) Get(context.Context, int64) (agents.Agent, error) { return shutdownAgent, nil }
func (jobAgent) CreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: 900, AgentID: agentID, SessionKey: "job-key-900"}, nil
}
func (jobAgent) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	return m, nil
}

// heldTurnGateway holds every turn until its context ends, signalling entry.
type heldTurnGateway struct{ entered chan struct{} }

func (g *heldTurnGateway) Chat(ctx context.Context, _ agents.Agent, _, _ string, _ func(string)) (string, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func (g *heldTurnGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", errors.New("not expected")
}

// TestShutdownFailsTheJobsThisProcessHolds: a job runs only in the process
// that accepted it. On a graceful shutdown (SIGTERM → app.shutdown), this
// process's running job and the job queued behind it are failed `interrupted`,
// each saying what happened, rather than holding the agent's queue until the
// deadline reaper. A job of ANOTHER process (a row this one never accepted —
// the other pod of a rolling update) is untouched.
func TestShutdownFailsTheJobsThisProcessHolds(t *testing.T) {
	const token = "job-shutdown-test-service-token-0123456789"
	logger := log.New(io.Discard, "", 0)
	store := &shutdownJobStore{rows: map[int64]jobs.Job{}}
	gw := &heldTurnGateway{entered: make(chan struct{}, 1)}
	srv := api.New(nil, api.AuthConfig{ServiceToken: token}, logger)
	srv.UseExtensions(api.Extensions{Jobs: store, Agents: jobAgent{}, Gateway: gw})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The other process's job, already in the table.
	other, _, _ := store.Create(context.Background(), jobs.NewJob{AgentID: shutdownAgent.ID, Agent: shutdownAgent.Name, Prompt: "theirs", TimeoutSeconds: 1800})

	submit := func(prompt string) int64 {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"agent": shutdownAgent.Name, "prompt": prompt})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/jobs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(api.ActorHeader, "review-orchestrator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /api/jobs = %d %v", resp.StatusCode, out)
		}
		return int64(out["job_id"].(float64))
	}
	running := submit("first")
	select {
	case <-gw.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("instrument check FAILED: the first job's turn never started")
	}
	queued := submit("second")
	if got := store.row(running).Status; got != jobs.StatusRunning {
		t.Fatalf("instrument check FAILED: the first job is %s, want running", got)
	}
	if got := store.row(queued).Status; got != jobs.StatusQueued {
		t.Fatalf("instrument check FAILED: the second job is %s, want queued", got)
	}

	a := &app{logger: logger, http: &http.Server{}, srv: srv}
	a.shutdown()

	for _, c := range []struct {
		name         string
		id           int64
		status, why  string
		errorMessage string
	}{
		{"this process's queued job", queued, jobs.StatusFailed, jobs.ReasonInterrupted,
			"the server holding this job shut down before starting it. Nothing was sent to the agent for it, so it is safe to re-submit"},
		{"this process's running job", running, jobs.StatusFailed, jobs.ReasonInterrupted,
			"the server running this job shut down before the agent's turn finished. The agent may have done some or all of the work, so re-submit it only if repeating that is safe"},
		{"another process's job", other.ID, jobs.StatusQueued, "", ""},
	} {
		got := store.row(c.id)
		if got.Status != c.status || got.Reason != c.why || got.Error != c.errorMessage {
			t.Errorf("%s after shutdown = %s/%s %q; want %s/%s %q", c.name, got.Status, got.Reason, got.Error, c.status, c.why, c.errorMessage)
		}
	}
}
