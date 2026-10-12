package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/flows"
)

// BROWSER FLOWS (internal/flows): an owner queues a named flow for one of their
// hosts' browsers, and that host's runner claims and runs it.
//
// THREE DOORS, AND WHICH CREDENTIAL OPENS WHICH IS THE DESIGN:
//
//   - /flows (the page and its forms): a browser SESSION. Enqueue, cancel, and
//     minting/revoking runner tokens — minting a credential is a person's act.
//   - /api/flows/jobs: a PERSONAL API token (mst_…) ONLY, through
//     requireAPIToken. 🔴 THE SHARED HOOK AND SERVICE TOKENS ARE REFUSED BY
//     CONSTRUCTION: the hook token acts as the admin and is held by every hook
//     script on every host, so admitting it would let any of them drive the
//     operator's logged-in browser. A personal token acts as its owner.
//   - /runner/flows/*: a RUNNER token (mfr_…), through requireRunnerToken, and
//     nothing else — it can claim, heartbeat and report, and that is all. It
//     acts as its owner (row-level security confines it to the owner's jobs)
//     and is bound to one host (the handlers confine it to jobs for that host).
//     A personal token would let a compromised runner do anything its user
//     can; this one cannot enqueue, read a task, or reach any other route.

const (
	flowsPath         = "/flows"
	flowsAPIPath      = "/api/flows/jobs"
	runnerFlowsPrefix = "/runner/flows"
	// flowClaimWait is how long a claim long-polls for a job before answering
	// 204. Under any proxy's idle timeout, and the runner simply asks again.
	flowClaimWait = 25 * time.Second
	// flowClaimTick is how often a waiting claim re-checks the queue.
	flowClaimTick = time.Second
	// flowStaleNeedsOperator is when a job that needs the operator is flagged
	// as stale on /flows.
	flowStaleNeedsOperator = 6 * time.Hour
)

// FlowsUnconfigured is the 503 body when no flow store is wired (no database).
const FlowsUnconfigured = "this server does not serve browser flows (no database)"

func (s *Server) registerFlowRoutes(mux Mux) {
	mux.HandleFunc("GET "+flowsPath, s.requireSession(s.handleFlowsPage))
	mux.HandleFunc("POST "+flowsPath+"/jobs", s.requireOperatorWrite(s.handleFlowEnqueueForm))
	mux.HandleFunc("POST "+flowsPath+"/jobs/{id}/cancel", s.requireOperatorWrite(s.handleFlowCancelForm))
	mux.HandleFunc("POST "+flowsPath+"/runner-tokens", s.requireOperatorWrite(s.handleRunnerTokenCreate))
	mux.HandleFunc("POST "+flowsPath+"/runner-tokens/{id}/revoke", s.requireOperatorWrite(s.handleRunnerTokenRevoke))

	mux.HandleFunc("POST "+flowsAPIPath, s.requireAPIToken(s.handleFlowEnqueueAPI))
	mux.HandleFunc("GET "+flowsAPIPath, s.requireAPIToken(s.handleFlowListAPI))
	mux.HandleFunc("GET "+flowsAPIPath+"/{id}", s.requireAPIToken(s.handleFlowGetAPI))

	mux.HandleFunc("POST "+runnerFlowsPrefix+"/claim", s.requireRunnerToken(s.handleRunnerClaim))
	mux.HandleFunc("POST "+runnerFlowsPrefix+"/{id}/heartbeat", s.requireRunnerToken(s.handleRunnerHeartbeat))
	mux.HandleFunc("POST "+runnerFlowsPrefix+"/{id}/result", s.requireRunnerToken(s.handleRunnerResult))
}

// --- the runner door ---------------------------------------------------------

type runnerTokenCtxKey struct{}

// RunnerTokenRefusal is the 401 body for every refused runner credential —
// another kind of token, a malformed one, an unknown, revoked or disabled
// owner's — so a refusal says nothing about which.
const RunnerTokenRefusal = "this route takes a flow runner token (" + flows.RunnerTokenPrefix + "…), minted at " + flowsPath

// requireRunnerToken admits a RUNNER token and nothing else, acting as its
// owner through authz.ViaRunner. See the file header.
func (s *Server) requireRunnerToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := hookTokenFromRequest(r)
		if !flows.RunnerTokenShapeOK(tok) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": RunnerTokenRefusal})
			return
		}
		if s.ext.Flows == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
			return
		}
		t, err := s.runnerTokenFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, flows.ErrBadRunnerToken):
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": RunnerTokenRefusal})
			return
		default:
			s.logger.Printf("flows: runner token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the runner token"})
			return
		}
		ctx := context.WithValue(r.Context(), runnerTokenCtxKey{}, t)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), t.OwnerID, authz.ViaRunner))
	}
}

// runnerTokenFor resolves a runner token to its record.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON apiTokenUser DOES: it is how the
// request's owner is LEARNED, and flow_runner_tokens is under FORCE row-level
// security. The System context is used for this one store call and never
// handed on; the handler gets the request's context with a principal built
// from the row's owner_id. System-ledger entry.
func (s *Server) runnerTokenFor(ctx context.Context, tok string) (flows.RunnerToken, error) {
	return s.ext.Flows.AuthenticateRunner(authz.System(ctx), tok)
}

func runnerTokenFromContext(ctx context.Context) flows.RunnerToken {
	t, _ := ctx.Value(runnerTokenCtxKey{}).(flows.RunnerToken)
	return t
}

// handleRunnerClaim long-polls for the next job addressed to this runner's
// host, on one of the instances it says are connected. 200 with the job, or
// 204 when nothing arrived within flowClaimWait.
func (s *Server) handleRunnerClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Instances []string `json:"instances"`
		// WaitSeconds shortens the long-poll (0 = the default; a test or a
		// one-shot probe passes 0 < n <= the default).
		WaitSeconds int `json:"waitSeconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the claim body is {\"instances\":[…]}"})
		return
	}
	tok := runnerTokenFromContext(r.Context())
	wait := flowClaimWait
	if body.WaitSeconds > 0 && time.Duration(body.WaitSeconds)*time.Second < wait {
		wait = time.Duration(body.WaitSeconds) * time.Second
	}
	// A shutdown ends the wait (EndStreams) rather than holding the drain.
	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(flowClaimTick)
	defer tick.Stop()
	for {
		j, ok, err := s.ext.Flows.Claim(ctx, tok.Host, body.Instances, tok.ID)
		if errors.Is(err, flows.ErrInvalid) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Printf("flows: claim for host %s: %v", tok.Host, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the claim failed"})
			return
		}
		if ok {
			s.writeJSON(w, http.StatusOK, j)
			return
		}
		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) handleRunnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	j, err := s.ext.Flows.Heartbeat(r.Context(), id, runnerTokenFromContext(r.Context()).Host)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

func (s *Server) handleRunnerResult(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	var res flows.Result
	// The output cap is enforced by the store (truncated, never refused); this
	// only bounds what is read at all.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*flows.MaxOutputBytes)).Decode(&res); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the result body is {\"outcome\":…,\"exitCode\":…,\"output\":…,\"detail\":…}"})
		return
	}
	j, err := s.ext.Flows.Report(r.Context(), id, runnerTokenFromContext(r.Context()).Host, res)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

// --- the API (personal token) --------------------------------------------------

type flowEnqueueBody struct {
	Flow       string            `json:"flow"`
	Host       string            `json:"host"`
	Instance   string            `json:"instance"`
	Params     map[string]string `json:"params"`
	TTLSeconds int               `json:"ttlSeconds"`
}

func (s *Server) handleFlowEnqueueAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	var b flowEnqueueBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"flow\",\"host\",\"instance\",\"params\",\"ttlSeconds\"}"})
		return
	}
	by := ""
	if t, ok := apiTokenFromContext(r.Context()); ok {
		by = "api token " + strconv.FormatInt(t.ID, 10)
		if t.Name != "" {
			by += " (" + t.Name + ")"
		}
	}
	j, err := s.enqueueFlow(r.Context(), flows.Request{Flow: b.Flow, Host: b.Host, Instance: b.Instance,
		Params: b.Params, TTL: time.Duration(b.TTLSeconds) * time.Second}, by)
	s.writeFlowResult(w, j, err, http.StatusCreated)
}

// enqueueFlow is the ONE enqueue path for every door: it checks that a task
// the flow will post on is the caller's own, then queues as the caller.
func (s *Server) enqueueFlow(ctx context.Context, req flows.Request, by string) (flows.Job, error) {
	if s.ext.Flows == nil {
		return flows.Job{}, errFlowsUnconfigured
	}
	if task := req.Params["task"]; task != "" && s.ext.Notes != nil {
		id, err := strconv.ParseInt(task, 10, 64)
		if err == nil {
			_, err = s.ext.Notes.Get(ctx, id)
		}
		if err != nil {
			// Another user's task and no task at all are the same refusal.
			return flows.Job{}, fmt.Errorf("%w: task %s is not one of your tasks", flows.ErrInvalid, task)
		}
	}
	via := string(authz.MustFrom(ctx).Via)
	return s.ext.Flows.Enqueue(ctx, req, via, by)
}

var errFlowsUnconfigured = errors.New(FlowsUnconfigured)

func (s *Server) handleFlowListAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.ext.Flows.List(r.Context(), limit)
	if err != nil {
		s.logger.Printf("flows: list: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list flow jobs"})
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleFlowGetAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	j, err := s.ext.Flows.Get(r.Context(), id)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

func flowJobID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// writeFlowResult maps a flows result onto the wire. The one error mapping for
// every JSON door, so they cannot disagree.
func (s *Server) writeFlowResult(w http.ResponseWriter, j flows.Job, err error, okCode int) {
	switch {
	case err == nil:
		s.writeJSON(w, okCode, j)
	case errors.Is(err, errFlowsUnconfigured):
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
	case errors.Is(err, flows.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.Is(err, flows.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
	case errors.Is(err, flows.ErrState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the job is not in a state that allows this"})
	case errors.Is(err, flows.ErrPendingCap):
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
	default:
		s.logger.Printf("flows: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the flow request failed"})
	}
}

// --- the page (session) --------------------------------------------------------

func (s *Server) handleFlowsPage(w http.ResponseWriter, r *http.Request) {
	s.renderFlowsPage(w, r, http.StatusOK, "", "")
}

func (s *Server) handleFlowEnqueueForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.renderFlowsPage(w, r, http.StatusServiceUnavailable, "", "")
		return
	}
	params := map[string]string{}
	f, _ := flows.Lookup(r.PostFormValue("flow"))
	for _, p := range f.Params {
		if v := r.PostFormValue("param_" + p.Name); v != "" {
			params[p.Name] = strings.ReplaceAll(v, "\r\n", "\n")
		}
	}
	ttlMin, _ := strconv.Atoi(r.PostFormValue("ttl_minutes"))
	_, err := s.enqueueFlow(r.Context(), flows.Request{Flow: r.PostFormValue("flow"), Host: r.PostFormValue("host"),
		Instance: r.PostFormValue("instance"), Params: params, TTL: time.Duration(ttlMin) * time.Minute}, "browser session")
	switch {
	case err == nil:
		http.Redirect(w, r, flowsPath, http.StatusSeeOther)
	case errors.Is(err, flows.ErrInvalid), errors.Is(err, flows.ErrPendingCap):
		s.renderFlowsPage(w, r, http.StatusBadRequest, "", err.Error())
	default:
		s.logger.Printf("flows: enqueue: %v", err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, "", "The flow could not be queued.")
	}
}

func (s *Server) handleFlowCancelForm(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	var err error = flows.ErrNotFound
	if ok && s.ext.Flows != nil {
		_, err = s.ext.Flows.Cancel(r.Context(), id)
	}
	switch {
	case err == nil:
		http.Redirect(w, r, flowsPath, http.StatusSeeOther)
	case errors.Is(err, flows.ErrNotFound):
		s.renderFlowsPage(w, r, http.StatusNotFound, "", "No such job.")
	case errors.Is(err, flows.ErrState):
		s.renderFlowsPage(w, r, http.StatusConflict, "", "That job is running or already finished; a running job is the runner's until it reports (stop the runner to stop it now).")
	default:
		s.logger.Printf("flows: cancel %d: %v", id, err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, "", "The job could not be cancelled.")
	}
}

func (s *Server) handleRunnerTokenCreate(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.renderFlowsPage(w, r, http.StatusServiceUnavailable, "", "")
		return
	}
	tok, _, err := s.ext.Flows.CreateRunnerToken(r.Context(), strings.TrimSpace(r.PostFormValue("host")))
	switch {
	case errors.Is(err, flows.ErrInvalid):
		s.renderFlowsPage(w, r, http.StatusBadRequest, "", err.Error())
	case err != nil:
		s.logger.Printf("flows: runner token create: %v", err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, "", "The runner token could not be created.")
	default:
		// 200, NOT a redirect: the token is shown in THIS response only.
		s.renderFlowsPage(w, r, http.StatusOK, tok, "")
	}
}

func (s *Server) handleRunnerTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	found := false
	if ok && s.ext.Flows != nil {
		var err error
		found, err = s.ext.Flows.RevokeRunnerToken(r.Context(), id)
		if err != nil {
			s.logger.Printf("flows: runner token revoke %d: %v", id, err)
			s.renderFlowsPage(w, r, http.StatusInternalServerError, "", "The runner token could not be revoked.")
			return
		}
	}
	if !found {
		s.renderFlowsPage(w, r, http.StatusNotFound, "", "No such runner token.")
		return
	}
	http.Redirect(w, r, flowsPath, http.StatusSeeOther)
}

// Shared class strings (each already emitted by the stylesheet: see
// api_tokens.go, which these are copied from).
const (
	flowsInputClass  = `min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus`
	flowsButtonClass = `press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90`
	flowsQuietButton = `press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge`
	flowsCardClass   = `flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge`
)

// renderFlowsPage writes /flows. minted, when set, is a runner token just
// created — the only time it is ever rendered.
func (s *Server) renderFlowsPage(w http.ResponseWriter, r *http.Request, code int, minted, problem string) {
	var jobs []flows.Job
	var runners []flows.RunnerToken
	if s.ext.Flows == nil {
		problem = "This server has no database, so it cannot queue browser flows."
	} else {
		var err error
		if jobs, err = s.ext.Flows.List(r.Context(), 50); err != nil {
			s.logger.Printf("flows: page list: %v", err)
			problem = "The job list could not be read."
		}
		if runners, err = s.ext.Flows.ListRunnerTokens(r.Context()); err != nil {
			s.logger.Printf("flows: page runners: %v", err)
			problem = "The runner list could not be read."
		}
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="flows-page">
<h1 class="text-balance text-base font-semibold text-fg">Browser flows</h1>
<p class="text-sm text-fg2">Queue a named flow for one of your hosts' browsers. That host's runner claims it, runs it, and reports here. A job not claimed before its time limit expires and never runs late.</p>
`)
	b.WriteString(problemNode(problem))
	if minted != "" {
		b.WriteString(`<div class="flex flex-col gap-2 rounded-md bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-testid="minted-runner-token">
<p class="text-sm font-semibold text-fg">Copy it now — it will not be shown again. It can only claim and report this host's jobs.</p>
<input id="minted-runner-token" readonly value="` + esc(minted) + `" aria-label="New runner token"
 class="min-h-[44px] w-full rounded-md bg-s1 px-3 py-2 font-mono text-sm text-fg ring-1 ring-inset ring-edge" onclick="this.select()">
</div>
`)
	}

	// The enqueue form: one form, every catalogue parameter (each flow uses the
	// ones it names; the server refuses the rest).
	b.WriteString(`<form method="POST" action="` + flowsPath + `/jobs" class="flex flex-col gap-2" data-testid="flow-enqueue">
<label for="flow-name" class="text-sm font-medium text-fg">Flow</label>
<select id="flow-name" name="flow" class="` + flowsInputClass + `">`)
	seen := map[string]bool{}
	var params []flows.Param
	for _, f := range flows.Catalogue() {
		b.WriteString(`<option value="` + esc(f.Name) + `">` + esc(f.Name) + ` (` + string(f.Class) + `) — ` + esc(f.Help) + `</option>`)
		for _, p := range f.Params {
			if !seen[p.Name] {
				seen[p.Name] = true
				params = append(params, p)
			}
		}
	}
	b.WriteString(`</select>
<label for="flow-host" class="text-sm font-medium text-fg">Host</label>
<input id="flow-host" name="host" value="laptop" maxlength="63" class="` + flowsInputClass + `">
<label for="flow-instance" class="text-sm font-medium text-fg">Browser instance</label>
<input id="flow-instance" name="instance" value="personal" maxlength="32" class="` + flowsInputClass + `">
`)
	for _, p := range params {
		id := "flow-param-" + p.Name
		b.WriteString(`<label for="` + id + `" class="text-sm font-medium text-fg">` + esc(p.Name) + ` <span class="text-xs text-fg2">` + esc(p.Help) + `</span></label>
`)
		if p.Pattern == nil {
			b.WriteString(`<textarea id="` + id + `" name="param_` + esc(p.Name) + `" maxlength="` + strconv.Itoa(p.MaxLen) + `" rows="3" class="` + flowsInputClass + `"></textarea>
`)
		} else {
			b.WriteString(`<input id="` + id + `" name="param_` + esc(p.Name) + `" maxlength="` + strconv.Itoa(p.MaxLen) + `" class="` + flowsInputClass + `">
`)
		}
	}
	b.WriteString(`<label for="flow-ttl" class="text-sm font-medium text-fg">Time limit (minutes)</label>
<input id="flow-ttl" name="ttl_minutes" inputmode="numeric" value="` + strconv.Itoa(int(flows.DefaultTTL/time.Minute)) + `" class="` + flowsInputClass + `">
<button type="submit" class="` + flowsButtonClass + `">Queue flow</button>
</form>
`)

	// The jobs.
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Jobs</h2>
<ul class="flex flex-col gap-2" data-testid="flow-jobs">
`)
	if len(jobs) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No jobs yet.</li>`)
	}
	now := time.Now()
	for _, j := range jobs {
		id := strconv.FormatInt(j.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-flow-job-id="` + id + `" data-flow-status="` + esc(string(j.Status)) + `">
<span class="break-all text-sm font-semibold text-fg">#` + id + ` ` + esc(j.Flow) + ` · ` + esc(string(j.Status)) + `</span>
<span class="text-xs text-fg2">` + esc(j.Host) + `/` + esc(j.Instance) + ` · queued ` + j.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC via ` + esc(j.RequestedVia) + `</span>
`)
		if j.Status == flows.StatusNeedsOperator && j.FinishedAt != nil && now.Sub(*j.FinishedAt) > flowStaleNeedsOperator {
			b.WriteString(`<span class="text-xs font-semibold text-fg" data-testid="flow-stale">Waiting on you since ` + j.FinishedAt.UTC().Format("2006-01-02 15:04") + ` UTC</span>
`)
		}
		for _, k := range []string{"thread", "task", "text"} {
			if v := j.Params[k]; v != "" {
				if len([]rune(v)) > 300 {
					v = string([]rune(v)[:300]) + "…"
				}
				b.WriteString(`<span class="whitespace-pre-wrap break-words text-xs text-fg2">` + esc(k) + `: ` + esc(v) + `</span>
`)
			}
		}
		if j.Detail != "" {
			b.WriteString(`<span class="break-words text-xs text-fg2">` + esc(j.Detail) + `</span>
`)
		}
		if j.Output != "" {
			out := j.Output
			if len([]rune(out)) > 2000 {
				out = string([]rune(out)[:2000]) + "…"
			}
			b.WriteString(`<pre class="max-h-48 overflow-y-auto whitespace-pre-wrap break-words font-mono text-xs text-fg2">` + esc(out) + `</pre>
`)
		}
		if j.Status == flows.StatusQueued || j.Status == flows.StatusNeedsOperator {
			label := "Cancel"
			if j.Status == flows.StatusNeedsOperator {
				label = "Dismiss"
			}
			b.WriteString(`<form method="POST" action="` + flowsPath + `/jobs/` + id + `/cancel"><button type="submit" class="` + flowsQuietButton + `">` + label + `</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
`)

	// The runners.
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Runners</h2>
<p class="text-sm text-fg2">A runner token is for ONE host. It can claim and report that host's jobs and nothing else.</p>
<form method="POST" action="` + flowsPath + `/runner-tokens" class="flex flex-col gap-2" data-testid="runner-token-create">
<label for="runner-host" class="text-sm font-medium text-fg">Host</label>
<input id="runner-host" name="host" maxlength="63" placeholder="laptop" class="` + flowsInputClass + `">
<button type="submit" class="` + flowsButtonClass + `">Create runner token</button>
</form>
<ul class="flex flex-col gap-2" data-testid="runner-tokens">
`)
	if len(runners) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No runner tokens yet.</li>`)
	}
	for _, t := range runners {
		used := "never used"
		if t.LastUsed != nil {
			used = "last used " + t.LastUsed.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		b.WriteString(`<li class="` + flowsCardClass + `" data-runner-token-id="` + strconv.FormatInt(t.ID, 10) + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(t.Host) + `</span>
<span class="text-xs text-fg2">created ` + t.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC · ` + used + `</span>
`)
		if t.RevokedAt != nil {
			b.WriteString(`<span class="text-xs font-semibold text-fg2">revoked</span>`)
		} else {
			b.WriteString(`<form method="POST" action="` + flowsPath + `/runner-tokens/` + strconv.FormatInt(t.ID, 10) + `/revoke">
<button type="submit" class="` + flowsQuietButton + `">Revoke</button></form>`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>`)
	writeAuthDocument(w, code, "Browser flows", b.String())
}
