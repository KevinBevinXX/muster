package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// One muster turn is one Hermes run.
//
//  1. The session's intent is written to the volume — idempotency key, the
//     EXACT body bytes, the API key's fingerprint — BEFORE the POST (store.go).
//  2. POST /v1/runs with those bytes and the Idempotency-Key header.
//  3. The run's events are read from its SSE stream: message.delta (text),
//     tool.started / tool.completed (drawn in muster as native tool rows),
//     approval.request (relayed to muster, answered with the exact body) and
//     approval.settled, until a terminal event.
//  4. The intent is closed; its run becomes the session's last run.
//
// 🔴 `run.failed {error: unknown_execution}` IS NOT A FAILURE. Measured at the
// pin (S6): after a SIGKILL a run is left `unknown` and every later run on its
// session is admitted but PARKED behind it, while its stream reports
// run.failed unknown_execution and closes. The parked run executes once the
// unknown one is resolved. Reporting it as a failure would let the person resend
// and run the turn twice; so hermesd resolves the predecessor and follows the
// queued run to its real end.

// runRequest is the body of POST /v1/runs.
type runRequest struct {
	Input        string `json:"input"`
	SessionID    string `json:"session_id"`
	Instructions string `json:"instructions,omitempty"`
}

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(runtimeHeader) != "" {
		writeFailure(w, notReady("this hermes pod runs no other runtime ("+runtimeHeader+": "+r.Header.Get(runtimeHeader)+")"))
		return
	}
	if !podagent.BearerOK(r, s.bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	var req responsesRequest
	if err := readBody(r, &req); err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "body: " + err.Error()})
		return
	}
	prompt, err := promptFrom(req.Input)
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: err.Error()})
		return
	}
	if strings.TrimSpace(prompt) == "" {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "empty prompt"})
		return
	}
	if why := s.readiness(); why != "" {
		writeFailure(w, notReady(why))
		return
	}
	key := r.Header.Get(sessionKeyHeader)
	if !s.claim(key) {
		writeFailure(w, &failure{Status: http.StatusConflict, Type: failBusy, Message: "a turn on this session is already being served"})
		return
	}
	defer s.release(key)
	items, id, f := s.runTurn(r.Context(), key, prompt)
	if f != nil {
		s.logf("hermesd: turn failed: %s", f.Error())
		writeFailure(w, f)
		return
	}
	writeResponse(w, id, items, req.Stream == nil || *req.Stream)
}

// runTurn records the intent, posts it and follows the run to its end.
func (s *server) runTurn(ctx context.Context, key, prompt string) ([]turnItem, string, *failure) {
	sess, err := s.store.ensure(key, s.keyFP)
	if err != nil {
		return nil, "", notReady("hermesd could not record the session: " + err.Error())
	}
	if sess.KeyFP != s.keyFP {
		// Recovery retires such a session before the gateway is ready; reaching
		// here would mean posting into a session the key cannot see.
		return nil, "", notReady("this session was posted under another API key and has not been retired yet")
	}
	instr, err := s.instructions()
	if err != nil {
		return nil, "", notReady("muster's instructions could not be read: " + err.Error())
	}
	body, err := json.Marshal(runRequest{Input: prompt, SessionID: sess.HermesID, Instructions: instr})
	if err != nil {
		return nil, "", &failure{Status: http.StatusBadGateway, Type: failRuntime, Message: err.Error()}
	}
	in := intent{IdempotencyKey: newIdempotencyKey(), Body: body, KeyFP: s.keyFP, At: s.now()}
	if err := s.store.beginIntent(key, in); err != nil {
		if errors.Is(err, errSessionBusy) {
			return nil, "", &failure{Status: http.StatusConflict, Type: failBusy, Message: err.Error()}
		}
		// Nothing was posted: without the intent on the volume a crash could
		// not be recovered, so the turn is not sent at all.
		return nil, "", notReady("hermesd could not record the turn before sending it: " + err.Error())
	}

	tctx, cancel := context.WithTimeout(ctx, s.cfg.TurnTimeout)
	defer cancel()
	runID, f := s.post(tctx, key, in)
	if f != nil {
		return nil, "", f
	}
	notice, _ := s.store.takeNotice(key)
	res := s.follow(tctx, key, sess, runID)
	if res.lost {
		// The run outlived this request (the turn budget, or the caller went
		// away): it keeps running in Hermes, the session stays busy, and a
		// watcher closes the intent with what happened, for the next reply.
		go s.watch(s.watchContext(), key, in.IdempotencyKey, runID)
		if ctx.Err() == nil {
			return nil, "", &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
				Message: fmt.Sprintf("the Hermes run %s did not finish within %s; it is still running, and its outcome "+
					"will be reported on this session's next reply", runID, s.cfg.TurnTimeout)}
		}
		return nil, "", &failure{Status: http.StatusBadGateway, Type: failMayHaveRun, Message: "the request ended: " + mayHaveRunText}
	}
	if err := s.store.closeIntent(key, in.IdempotencyKey, ""); err != nil {
		s.logf("hermesd: close the intent of %s: %v", runID, err)
	}
	if res.fail != nil {
		return nil, "", res.fail
	}
	items := res.items
	if notice != "" {
		items = append([]turnItem{{Text: notice}}, items...)
	}
	return items, runID, nil
}

// post sends the intent and records the run id. A 409
// idempotency_key_conflict is "may have run": the key saw a different body,
// and hermesd never retries with a new key. A transport error is retried with
// the SAME key and bytes, which Hermes replays rather than admitting twice.
func (s *server) post(ctx context.Context, key string, in intent) (string, *failure) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !sleepCtx(ctx, time.Second) {
			break
		}
		runID, _, err := s.api.postRun(ctx, in.IdempotencyKey, in.Body)
		if err == nil {
			if err := s.store.setRunID(key, in.IdempotencyKey, runID); err != nil {
				s.logf("hermesd: record run %s: %v", runID, err)
			}
			return runID, nil
		}
		lastErr = err
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status >= 500 {
			continue // ambiguous: the POST may have been admitted; replay it
		}
		if ae.Status == http.StatusConflict && ae.Code == "idempotency_key_conflict" {
			_ = s.store.closeIntent(key, in.IdempotencyKey, "")
			return "", &failure{Status: http.StatusBadGateway, Type: failMayHaveRun, Code: ae.Code, Message: mayHaveRunText +
				" (Hermes says this turn's idempotency key was already used with a different request)"}
		}
		// Any other 4xx refused the run: nothing was admitted.
		_ = s.store.closeIntent(key, in.IdempotencyKey, "")
		if ae.Status == http.StatusTooManyRequests {
			return "", notReady("the Hermes API server refused the run for its concurrency limit: " + ae.Message)
		}
		return "", &failure{Status: http.StatusBadGateway, Type: failRuntime, Code: ae.Code, Message: "Hermes refused the run: " + ae.Error()}
	}
	// Ambiguous to the end. The intent stays open, so the run is never posted
	// under another key; a background replay settles it.
	go s.watchIntent(s.watchContext(), key, in.IdempotencyKey)
	return "", &failure{Status: http.StatusBadGateway, Type: failMayHaveRun,
		Message: fmt.Sprintf("%s (posting the run failed: %v)", mayHaveRunText, lastErr)}
}

// runResult is how a followed run ended.
type runResult struct {
	items []turnItem
	fail  *failure
	// lost: the run had not ended when ctx did.
	lost bool
}

// follow reads the run's events to its end, falling back to polling its status
// when the stream ends without a terminal event or the run is parked behind an
// unknown one.
func (s *server) follow(ctx context.Context, key string, sess session, runID string) runResult {
	ap := newApprovals(s, runID, sess.HermesID)
	defer ap.stop()
	var (
		items    []turnItem
		text     strings.Builder
		tools    = map[string]*toolCall{}
		terminal string
		final    struct {
			Output string `json:"output"`
			Error  string `json:"error"`
		}
	)
	flushText := func() {
		if strings.TrimSpace(text.String()) != "" {
			items = append(items, turnItem{Text: text.String()})
		}
		text.Reset()
	}
	err := s.api.streamEvents(ctx, runID, func(ev runEvent) bool {
		switch ev.Event {
		case "message.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(ev.Raw, &d)
			text.WriteString(d.Delta)
		case "tool.started":
			var t struct {
				Tool, Preview string
				ToolCallID    string `json:"tool_call_id"`
			}
			_ = json.Unmarshal(ev.Raw, &t)
			flushText()
			id := t.ToolCallID
			if id == "" {
				id = fmt.Sprintf("%s_%d", runID, len(items))
			}
			tc := &toolCall{ID: id, Name: t.Tool, Input: t.Preview, Status: "in_progress"}
			tools[id] = tc
			items = append(items, turnItem{Tool: tc})
		case "tool.completed":
			var t struct {
				ToolCallID string `json:"tool_call_id"`
				Error      bool   `json:"error"`
			}
			_ = json.Unmarshal(ev.Raw, &t)
			if tc := tools[t.ToolCallID]; tc != nil {
				tc.Status = "completed"
				if t.Error {
					tc.Status = "failed"
				}
			}
		case "approval.request":
			var a approvalRequest
			if json.Unmarshal(ev.Raw, &a) == nil {
				ap.start(a)
			}
		case "approval.settled", "approval.responded":
			var a struct {
				PromptID string `json:"prompt_id"`
			}
			_ = json.Unmarshal(ev.Raw, &a)
			ap.settled(a.PromptID)
		case "run.completed", "run.failed", "run.cancelled", "run.interrupted":
			terminal = strings.TrimPrefix(ev.Event, "run.")
			_ = json.Unmarshal(ev.Raw, &final)
			return false
		}
		return true
	})
	if err != nil {
		s.logf("hermesd: the event stream of %s ended: %v; following its status instead", runID, err)
	}
	if terminal == "failed" && strings.Contains(final.Error, "unknown_execution") {
		s.logf("hermesd: run %s is queued behind an unknown execution on session %s; resolving it", runID, sess.HermesID)
		s.resolvePredecessor(ctx, key)
		terminal = ""
	}
	if terminal == "" {
		st, err := s.pollRun(ctx, runID, ap)
		if err != nil {
			return runResult{lost: true}
		}
		terminal, final.Output, final.Error = st.Status, st.Output, st.Error
		text.Reset() // the polled status carries the final output
	}
	switch terminal {
	case "completed":
		if final.Output != "" {
			text.Reset()
			text.WriteString(final.Output)
		}
		flushText()
		hasText := false
		for _, it := range items {
			if it.Tool == nil {
				hasText = true
			}
		}
		if !hasText {
			return runResult{fail: &failure{Status: http.StatusBadGateway, Type: failEmptyReply,
				Message: "the Hermes run completed with no answer text"}}
		}
		return runResult{items: items}
	case "failed":
		return runResult{fail: classifyRunFailure(final.Error)}
	default: // cancelled, interrupted
		msg := "the Hermes run was interrupted"
		if final.Error != "" {
			msg += ": " + final.Error
		}
		return runResult{fail: &failure{Status: http.StatusBadGateway, Type: failInterrupted, Message: msg}}
	}
}

// pollRun reads the run's status until it is terminal or ctx ends, relaying the
// approvals it is waiting on. Errors reading it (the gateway restarting) are
// waited out.
func (s *server) pollRun(ctx context.Context, runID string, ap *approvals) (runStatus, error) {
	for {
		var st struct {
			runStatus
			Pending []struct {
				approvalRequest
				Kind string `json:"kind"`
			} `json:"pending_controls"`
		}
		err := s.api.do(ctx, http.MethodGet, "/v1/runs/"+runID, nil, nil, &st)
		if err == nil {
			if terminalStatus(st.Status) {
				return st.runStatus, nil
			}
			for _, p := range st.Pending {
				if p.Kind == "approval" {
					if p.ExecutionGeneration == "" {
						p.ExecutionGeneration = st.ExecutionGeneration
					}
					ap.start(p.approvalRequest)
				}
			}
		}
		if !sleepCtx(ctx, s.watchPoll) {
			return runStatus{}, ctx.Err()
		}
	}
}

// resolvePredecessor resolves the session's previous run when it is unknown:
// the run a queued one is parked behind. (The run being followed is still the
// open intent's, so LastRunID is the one before it.)
func (s *server) resolvePredecessor(ctx context.Context, key string) {
	sess, ok := s.store.get(key)
	if !ok || sess.LastRunID == "" {
		return
	}
	if _, err := s.resolveIfUnknown(ctx, sess.LastRunID); err != nil {
		s.logf("hermesd: resolve the run before %s: %v", key, err)
	}
}

// resolveIfUnknown resolves runID when Hermes reports it unknown, and reports
// what it found.
func (s *server) resolveIfUnknown(ctx context.Context, runID string) (runStatus, error) {
	st, err := s.api.getRun(ctx, runID)
	if err != nil {
		return st, err
	}
	if st.Status != "unknown" {
		return st, nil
	}
	outcome, err := s.api.resolveUnknown(ctx, runID, st.AdmissionID, st.ExecutionGeneration)
	if err != nil {
		return st, fmt.Errorf("resolve-unknown %s: %w", runID, err)
	}
	s.logf("hermesd: resolved unknown run %s (outcome %s)", runID, outcome)
	st.Status = "interrupted"
	return st, nil
}

// approvals relays one run's approval requests, each at most once.
type approvals struct {
	s                *server
	runID, sessionID string

	mu      sync.Mutex
	started map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func newApprovals(s *server, runID, sessionID string) *approvals {
	return &approvals{s: s, runID: runID, sessionID: sessionID, started: map[string]context.CancelFunc{}}
}

// start relays a, unless it already is: muster's answer (or hermesd's own deny)
// is sent with the exact body the pin accepts.
func (a *approvals) start(req approvalRequest) {
	if req.PromptID == "" {
		return
	}
	a.mu.Lock()
	if _, ok := a.started[req.PromptID]; ok {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.started[req.PromptID] = cancel
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		choice := "deny"
		if a.s.relay.decide(ctx, req, a.s.cfg.Workspace, a.sessionID) == relayApprove {
			choice = "once"
		}
		if ctx.Err() != nil {
			return // settled elsewhere, or the turn ended: nothing to answer
		}
		actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer acancel()
		if err := a.s.api.answerApproval(actx, a.runID, approvalBody{
			RequestID: req.PromptID, ExecutionGeneration: req.ExecutionGeneration, Choice: choice,
		}); err != nil {
			a.s.logf("hermesd: answer approval %s of run %s (%s): %v", req.PromptID, a.runID, choice, err)
			return
		}
		a.s.logf("hermesd: approval %s of run %s answered %s", req.PromptID, a.runID, choice)
	}()
}

// settled stops relaying promptID: Hermes says it is no longer pending.
func (a *approvals) settled(promptID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cancel, ok := a.started[promptID]; ok {
		cancel()
	}
}

func (a *approvals) stop() {
	a.mu.Lock()
	for _, cancel := range a.started {
		cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
}
