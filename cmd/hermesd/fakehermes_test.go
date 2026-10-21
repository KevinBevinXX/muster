package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHermes is Hermes's API server at the pin, as hermesd uses it, with no
// model behind it. Its behaviour is read from the pinned source
// (gateway/platforms/api_server_runs.py, api_server_authority_runs.py) and the
// Phase 0 measurements (S6):
//
//   - runs, idempotency keys, GET and resolve-unknown are scoped by the API key
//     (a run another key posted is 404, as `_request_owns_run` answers);
//   - an Idempotency-Key replays the run it admitted only for a request with the
//     same fingerprint (the JSON body); a different body is 409
//     idempotency_key_conflict;
//   - the approval route accepts EXACTLY {request_id, execution_generation,
//     choice}, else 400 invalid_params;
//   - resolve-unknown accepts EXACTLY {admission_id, execution_generation};
//   - a run posted on a session whose previous run is `unknown` reports
//     run.failed {error: unknown_execution} on its stream and closes it, while
//     its status reads `queued`; once the unknown run is resolved, it runs.
//
// What a run does is scripted by its input:
//
//	APPROVE <cmd>   tool.started, approval.request, then waits for the answer
//	SLOW            waits until the test calls release
//	FAIL <text>     run.failed {error: <text>}
//	EMPTY           run.completed with no output
//	anything else   message.delta ×2 and run.completed {output: "echo: <input>"}
type fakeHermes struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	keys      map[string]bool // API keys it accepts
	runs      map[string]*fakeRun
	order     []string             // run ids in admission order
	idem      map[string][2]string // scope|key → {fingerprint, run id}
	posts     []fakePost
	approvals []map[string]any
	resolves  []fakeResolve
	readMsgs  []string // session ids whose messages were read
	messages  map[string][]map[string]any
	gets      []string // run ids read with GET /v1/runs/{id}
	healthy   bool
	stops     []string // run ids POSTed to /stop
	// hangGets makes GET /v1/runs/{id} hang (a gateway that accepted the
	// connection and never answers); failGets answers it 500.
	hangGets, failGets bool
	// getStatus, when non-zero, answers GET /v1/runs/{id} with that status
	// (getCode its error code): a definite refusal.
	getStatus int
	getCode   string
	release   chan struct{}
	// onPost runs for every POST /v1/runs before it is admitted.
	onPost func(fakePost)
	// dropAnswers drops the HTTP answer of the next N admitted POSTs (the run IS
	// admitted): a lost acceptance.
	dropAnswers int
	// postStatus, when non-zero, is answered to every POST instead of admitting.
	postStatus int
	postCode   string
}

type fakePost struct {
	Scope string
	Key   string
	Body  []byte
}

type fakeResolve struct {
	RunID string
	Body  map[string]any
}

type fakeRun struct {
	id, scope, session, input string
	admission                 string
	gen                       int

	mu     sync.Mutex
	status string
	// pending is the run's pending_controls while it waits on an approval, in
	// the pin's shape (P:gateway/session_pending_controls.py register):
	// {kind, prompt_id, execution_generation, command, description, choices}.
	pending  []map[string]any
	stopped  chan struct{} // closed by POST /v1/runs/{id}/stop
	output   string
	errText  string
	events   []map[string]any
	closed   bool
	notify   chan struct{}
	approval chan string
	resolved chan struct{} // closed when this unknown run is resolved
}

func newFakeHermes(t *testing.T, key string) *fakeHermes {
	f := &fakeHermes{
		t: t, keys: map[string]bool{key: true}, runs: map[string]*fakeRun{}, idem: map[string][2]string{},
		messages: map[string][]map[string]any{}, healthy: true, release: make(chan struct{}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHermes) api(key string) *hermesAPI {
	return newHermesAPI(f.srv.URL, key)
}

func (r *fakeRun) emit(ev map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	ev["run_id"] = r.id
	r.events = append(r.events, ev)
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *fakeRun) finish(status string, fields map[string]any) {
	r.mu.Lock()
	r.status = status
	if o, ok := fields["output"].(string); ok {
		r.output = o
	}
	if e, ok := fields["error"].(string); ok {
		r.errText = e
	}
	r.mu.Unlock()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = "run." + status
	r.emit(fields)
	r.mu.Lock()
	r.closed = true
	close(r.notify)
	r.notify = make(chan struct{})
	r.mu.Unlock()
}

func (r *fakeRun) setStatus(s string) {
	r.mu.Lock()
	r.status = s
	r.mu.Unlock()
}

func fingerprint(body []byte) string {
	var v any
	_ = json.Unmarshal(body, &v)
	b, _ := json.Marshal(v) // Go sorts map keys: a canonical form, like sort_keys=True
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (f *fakeHermes) jsonErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code}})
}

func (f *fakeHermes) scope(r *http.Request) (string, bool) {
	k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return k, f.keys[k]
}

func (f *fakeHermes) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		f.mu.Lock()
		ok := f.healthy
		f.mu.Unlock()
		if !ok {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	scope, ok := f.scope(r)
	if !ok {
		f.jsonErr(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
		f.handlePost(w, r, scope)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "runs":
		f.handleGet(w, r, parts[2], scope)
	case r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "events":
		f.handleEvents(w, r, parts[2], scope)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "approval":
		f.handleApproval(w, r, parts[2], scope)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "stop":
		f.mu.Lock()
		f.stops = append(f.stops, parts[2])
		f.mu.Unlock()
		run, ok := f.owned(parts[2], scope)
		if !ok {
			f.jsonErr(w, http.StatusNotFound, "not_found", "not_found")
			return
		}
		run.mu.Lock()
		select {
		case <-run.stopped:
		default:
			close(run.stopped)
		}
		run.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": parts[2], "status": "stopping"})
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "resolve-unknown":
		f.handleResolve(w, r, parts[2], scope)
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[3] == "messages":
		f.mu.Lock()
		f.readMsgs = append(f.readMsgs, parts[2])
		msgs, ok := f.messages[parts[2]]
		f.mu.Unlock()
		if !ok {
			f.jsonErr(w, http.StatusNotFound, "session_not_found", "Session not found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "session_id": parts[2], "data": msgs})
	default:
		http.Error(w, "404: Not Found", http.StatusNotFound)
	}
}

func (f *fakeHermes) owned(id, scope string) (*fakeRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[id]
	return run, ok && run.scope == scope
}

func (f *fakeHermes) handlePost(w http.ResponseWriter, r *http.Request, scope string) {
	body, _ := io.ReadAll(r.Body)
	idemKey := r.Header.Get("Idempotency-Key")
	p := fakePost{Scope: scope, Key: idemKey, Body: body}
	f.mu.Lock()
	f.posts = append(f.posts, p)
	hook := f.onPost
	status, code := f.postStatus, f.postCode
	f.mu.Unlock()
	if hook != nil {
		hook(p)
	}
	if status != 0 {
		f.jsonErr(w, status, code, "refused by the test")
		return
	}
	var req struct {
		Input        string `json:"input"`
		SessionID    string `json:"session_id"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Input == "" {
		f.jsonErr(w, http.StatusBadRequest, "", "Missing 'input' field")
		return
	}
	fp := fingerprint(body)
	f.mu.Lock()
	if idemKey != "" {
		if rec, ok := f.idem[scope+"|"+idemKey]; ok {
			f.mu.Unlock()
			if rec[0] != fp {
				f.jsonErr(w, http.StatusConflict, "idempotency_key_conflict", "Idempotency-Key was already used with a different request payload")
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": rec[1], "status": "queued", "replayed": true})
			return
		}
	}
	id := fmt.Sprintf("run_%d", len(f.order)+1)
	run := &fakeRun{id: id, scope: scope, session: req.SessionID, input: req.Input, admission: "adm_" + id, gen: 1,
		status: "running", notify: make(chan struct{}), approval: make(chan string, 1), resolved: make(chan struct{}), stopped: make(chan struct{})}
	// The run a new one on this session would park behind.
	var blocker *fakeRun
	for i := len(f.order) - 1; i >= 0; i-- {
		prev := f.runs[f.order[i]]
		if prev.session == req.SessionID {
			prev.mu.Lock()
			if prev.status == "unknown" {
				blocker = prev
			}
			prev.mu.Unlock()
			break
		}
	}
	f.runs[id] = run
	f.order = append(f.order, id)
	if idemKey != "" {
		f.idem[scope+"|"+idemKey] = [2]string{fp, id}
	}
	f.messages[req.SessionID] = append(f.messages[req.SessionID], map[string]any{"role": "user", "content": req.Input})
	drop := f.dropAnswers > 0
	if drop {
		f.dropAnswers--
	}
	f.mu.Unlock()
	go f.script(run, blocker)
	if drop {
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"run_id": id, "status": "started", "replayed": false})
}

func (f *fakeHermes) script(run *fakeRun, blocker *fakeRun) {
	if blocker != nil {
		run.setStatus("queued")
		run.finish0("failed", map[string]any{"error": "unknown_execution"})
		<-blocker.resolved
		run.setStatus("running")
		run.mu.Lock()
		run.status, run.output = "completed", "after the unknown run: "+run.input
		run.mu.Unlock()
		return
	}
	in := run.input
	switch {
	case strings.HasPrefix(in, "APPROVE "):
		run.emit(map[string]any{"event": "tool.started", "tool": "terminal", "preview": strings.TrimPrefix(in, "APPROVE "), "tool_call_id": "call_1"})
		run.setStatus("waiting_for_approval")
		run.emit(map[string]any{"event": "approval.request", "kind": "approval", "prompt_id": "prompt-" + run.id,
			"execution_generation": run.gen, "command": strings.TrimPrefix(in, "APPROVE "), "description": "recursive delete",
			"choices": []string{"once", "deny", "session", "always"}})
		choice := <-run.approval
		run.setStatus("running")
		run.emit(map[string]any{"event": "approval.settled", "prompt_id": "prompt-" + run.id, "execution_generation": run.gen})
		run.emit(map[string]any{"event": "tool.completed", "tool": "terminal", "duration": 0.1, "error": choice != "once", "tool_call_id": "call_1"})
		run.emit(map[string]any{"event": "message.delta", "delta": "choice was " + choice})
		run.finish("completed", map[string]any{"output": "choice was " + choice})
	case strings.HasPrefix(in, "POLLAPPROVE "):
		// The stream drops before the approval is announced on it; the approval
		// is visible only in the run's pending_controls.
		run.mu.Lock()
		run.closed, run.status = true, "waiting_for_approval"
		run.pending = []map[string]any{{"kind": "approval", "prompt_id": "prompt-" + run.id, "execution_generation": run.gen,
			"command": strings.TrimPrefix(in, "POLLAPPROVE "), "description": "recursive delete", "choices": []string{"once", "deny", "session", "always"}}}
		close(run.notify)
		run.notify = make(chan struct{})
		run.mu.Unlock()
		choice := <-run.approval
		run.mu.Lock()
		run.pending, run.status, run.output = nil, "completed", "polled choice was "+choice
		run.mu.Unlock()
	case in == "HANG":
		run.emit(map[string]any{"event": "message.delta", "delta": "working for ever"})
		<-run.stopped
		run.finish("cancelled", map[string]any{"error": "Stop requested via API"})
	case in == "LATE":
		// Completes just as it is stopped: the answer arrived between the turn
		// budget running out and the stop reaching the run.
		run.emit(map[string]any{"event": "message.delta", "delta": "almost"})
		<-run.stopped
		run.finish("completed", map[string]any{"output": "late answer"})
	case strings.HasPrefix(in, "TOOLFAIL"):
		run.emit(map[string]any{"event": "tool.started", "tool": "terminal", "preview": "false", "tool_call_id": "call_tf"})
		run.emit(map[string]any{"event": "tool.failed", "tool": "terminal", "tool_call_id": "call_tf"})
		run.finish("completed", map[string]any{"output": "the tool failed"})
	case in == "SLOW":
		run.emit(map[string]any{"event": "message.delta", "delta": "working"})
		<-f.release
		run.finish("completed", map[string]any{"output": "slow done"})
	case strings.HasPrefix(in, "FAIL "):
		run.finish("failed", map[string]any{"error": strings.TrimPrefix(in, "FAIL ")})
	case in == "EMPTY":
		run.finish("completed", map[string]any{"output": ""})
	default:
		run.emit(map[string]any{"event": "message.delta", "delta": "echo: "})
		run.emit(map[string]any{"event": "message.delta", "delta": in})
		run.finish("completed", map[string]any{"output": "echo: " + in})
	}
}

// finish0 publishes a terminal EVENT without settling the status: the
// unknown_execution report of a run that is really only queued.
func (r *fakeRun) finish0(status string, fields map[string]any) {
	fields["event"] = "run." + status
	r.emit(fields)
	r.mu.Lock()
	r.closed = true
	close(r.notify)
	r.notify = make(chan struct{})
	r.mu.Unlock()
}

func (f *fakeHermes) handleGet(w http.ResponseWriter, r *http.Request, id, scope string) {
	f.mu.Lock()
	f.gets = append(f.gets, id)
	f.mu.Unlock()
	f.mu.Lock()
	hang, fail := f.hangGets, f.failGets
	f.mu.Unlock()
	if hang {
		<-r.Context().Done() // until the caller gives up
		return
	}
	if fail {
		f.jsonErr(w, http.StatusInternalServerError, "storage_unavailable", "storage_unavailable")
		return
	}
	f.mu.Lock()
	gs, gc := f.getStatus, f.getCode
	f.mu.Unlock()
	if gs != 0 {
		f.jsonErr(w, gs, gc, "refused by the test")
		return
	}
	run, ok := f.owned(id, scope)
	if !ok {
		f.jsonErr(w, http.StatusNotFound, "run_not_found", "Run not found: "+id)
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	pending := []any{}
	for _, p := range run.pending {
		pending = append(pending, p)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"run_id": id, "status": run.status, "session_id": run.session, "admission_id": run.admission,
		"execution_generation": run.gen, "output": run.output, "error": run.errText, "pending_controls": pending,
	})
}

func (f *fakeHermes) handleEvents(w http.ResponseWriter, r *http.Request, id, scope string) {
	run, ok := f.owned(id, scope)
	if !ok {
		f.jsonErr(w, http.StatusNotFound, "run_not_found", "Run not found: "+id)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	_, _ = io.WriteString(w, ": open\n\n")
	fl.Flush()
	for i := 0; ; {
		run.mu.Lock()
		evs, closed, notify := run.events, run.closed, run.notify
		run.mu.Unlock()
		for ; i < len(evs); i++ {
			b, _ := json.Marshal(evs[i])
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i, b)
		}
		fl.Flush()
		if closed {
			_, _ = io.WriteString(w, ": stream closed\n\n")
			return
		}
		select {
		case <-notify:
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			_, _ = io.WriteString(w, ": keepalive\n\n")
		}
	}
}

func keySet(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func (f *fakeHermes) handleApproval(w http.ResponseWriter, r *http.Request, id, scope string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.approvals = append(f.approvals, body)
	f.mu.Unlock()
	run, ok := f.owned(id, scope)
	if !ok {
		f.jsonErr(w, http.StatusNotFound, "not_found", "not_found")
		return
	}
	if keySet(body) != "choice,execution_generation,request_id" {
		f.jsonErr(w, http.StatusBadRequest, "invalid_params", "invalid_params")
		return
	}
	if g, ok := body["execution_generation"].(float64); !ok || int(g) != run.gen {
		f.jsonErr(w, http.StatusConflict, "stale_generation", "stale_generation")
		return
	}
	if body["request_id"] != "prompt-"+id {
		f.jsonErr(w, http.StatusConflict, "approval_not_pending", "approval_not_pending")
		return
	}
	select {
	case run.approval <- body["choice"].(string):
	default:
		f.jsonErr(w, http.StatusConflict, "approval_not_pending", "approval_not_pending")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"run_id": id, "status": "resolved"})
}

func (f *fakeHermes) handleResolve(w http.ResponseWriter, r *http.Request, id, scope string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.resolves = append(f.resolves, fakeResolve{RunID: id, Body: body})
	f.mu.Unlock()
	run, ok := f.owned(id, scope)
	if !ok {
		f.jsonErr(w, http.StatusNotFound, "not_found", "not_found")
		return
	}
	if keySet(body) != "admission_id,execution_generation" || body["admission_id"] != run.admission {
		f.jsonErr(w, http.StatusBadRequest, "invalid_params", "invalid_params")
		return
	}
	run.mu.Lock()
	if run.status != "unknown" {
		run.mu.Unlock()
		f.jsonErr(w, http.StatusConflict, "stale_generation", "stale_generation")
		return
	}
	run.status = "cancelled"
	run.mu.Unlock()
	close(run.resolved)
	_ = json.NewEncoder(w).Encode(map[string]any{"run_id": id, "status": "terminal", "outcome": "interrupted"})
}

// admitUnknown records a run left `unknown` by a SIGKILL: posted under scope on
// session, never settled.
func (f *fakeHermes) admitUnknown(scope, session, input string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("run_%d", len(f.order)+1)
	f.runs[id] = &fakeRun{id: id, scope: scope, session: session, input: input, admission: "adm_" + id, gen: 1,
		status: "unknown", notify: make(chan struct{}), approval: make(chan string, 1), resolved: make(chan struct{}), stopped: make(chan struct{}), closed: true}
	f.order = append(f.order, id)
	return id
}

func (f *fakeHermes) snapshotPosts() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

func (f *fakeHermes) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}
