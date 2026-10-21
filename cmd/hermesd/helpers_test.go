package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "test-api-key-0123456789abcdef0123456789abcdef"
const testBearer = "test-bearer-5f1e"

// testConfig is a config whose every path is under one temp dir.
func testConfig(t *testing.T) config {
	t.Helper()
	root := t.TempDir()
	return config{
		HermesHome: filepath.Join(root, "data", "hermes"), StateDir: filepath.Join(root, "data", "hermesd"),
		Workspace: filepath.Join(root, "data", "workspace"), Instructions: filepath.Join(root, "etc", "instructions.md"),
		Listen: "127.0.0.1:0", APIPort: 8642, HermesBin: "hermes",
		RelayTimeout: 2 * time.Second,
	}
}

// newTestServer is a hermesd server driving fake's API with key, its gateway
// counted as ready (no supervisor), its state on a fresh volume.
func newTestServer(t *testing.T, fake *fakeHermes, key string) *server {
	t.Helper()
	cfg := testConfig(t)
	return newTestServerWith(t, cfg, fake, key)
}

func newTestServerWith(t *testing.T, cfg config, fake *fakeHermes, key string) *server {
	t.Helper()
	for _, d := range []string{cfg.StateDir, cfg.Workspace} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := openStore(cfg.storePath())
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(cfg, testBearer, key, st)
	s.api = fake.api(key)
	s.logf = t.Logf
	s.relay.logf = t.Logf
	s.watchPoll = 20 * time.Millisecond
	s.turnTimeout = 10 * time.Second
	return s
}

// turn sends one muster turn to s on session key and returns the status and body.
func turn(t *testing.T, s *server, key, prompt string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "m", "input": []map[string]any{{"type": "message", "role": "user", "content": prompt}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testBearer)
	req.Header.Set(sessionKeyHeader, key)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// replyText is the text muster assembles from a 200 stream: response.completed's
// output texts, joined by a blank line.
func replyText(t *testing.T, body string) string {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		if !strings.Contains(block, "event: response.completed") {
			continue
		}
		_, data, _ := strings.Cut(block, "data: ")
		var ev struct {
			Response struct {
				Output []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("response.completed is not JSON: %v", err)
		}
		var segs []string
		for _, it := range ev.Response.Output {
			for _, c := range it.Content {
				segs = append(segs, c.Text)
			}
		}
		return strings.Join(segs, "\n\n")
	}
	t.Fatalf("no response.completed in %q", body)
	return ""
}

func failureType(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error failure `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("failure body is not JSON: %q", body)
	}
	return e.Error.Type
}

// fakeMuster is muster's approval relay (internal/api/approvals.go) as the pod
// reaches it: files a request, answers its state.
type fakeMuster struct {
	srv *httptest.Server

	mu      sync.Mutex
	answer  string // "" leaves every request pending
	filed   []map[string]any
	deleted []string
}

func newFakeMuster(t *testing.T, answer string) *fakeMuster {
	m := &fakeMuster{answer: answer}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Muster-Token") != "agent-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/approvals":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			m.filed = append(m.filed, b)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"requestId": "req-1"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agent/approvals/"):
			if m.answer == "" {
				_ = json.NewEncoder(w).Encode(map[string]string{"state": "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "decided", "response": m.answer})
		case r.Method == http.MethodDelete:
			m.deleted = append(m.deleted, r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMuster) wire(s *server) {
	s.relay.base, s.relay.token, s.relay.poll = m.srv.URL, "agent-token", 10*time.Millisecond
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// setLastRun records runID as key's last run, durably, as a settled turn would.
func setLastRun(t *testing.T, s *server, key, runID string) {
	t.Helper()
	if err := s.store.commit(func(next map[string]*session) (bool, error) {
		next[key].LastRunID = runID
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// setOpenIntent records an open intent on key, durably — as a crash after its
// POST (runID set) or before its answer (runID "") would leave it.
func setOpenIntent(t *testing.T, s *server, key, runID, input string) intent {
	t.Helper()
	sess, err := s.store.ensure(key, s.keyFP)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runRequest{Input: input, SessionID: sess.HermesID})
	in := intent{IdempotencyKey: newIdempotencyKey(), Body: body, KeyFP: s.keyFP, At: s.now(), RunID: runID}
	if err := s.store.beginIntent(key, in); err != nil {
		t.Fatal(err)
	}
	return in
}
