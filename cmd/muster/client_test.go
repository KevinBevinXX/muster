package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestExitCodeMapping pins the contract: each server condition maps to one
// documented exit code. A script branching on these is the reason they exist.
func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		args []string
		set  func(h *harness)
		want int
	}{
		{
			name: "200 JSON",
			args: []string{"task", "ls"},
			set:  func(h *harness) { h.json("GET /api/tasks", http.StatusOK, `[]`) },
			want: exitOK,
		},
		{
			name: "500 -> server error",
			args: []string{"task", "ls"},
			set:  func(h *harness) { h.json("GET /api/tasks", http.StatusInternalServerError, `{"error":"boom"}`) },
			want: exitServerError,
		},
		{
			// 🔴 A BARE 503 ON THE HOOK TIER IS AN OUTAGE, NOT AN ARMING PROBLEM.
			// This is the negative control for the exit-9 branch below: most of the
			// tier is enforce-when-set, so an unmarked 503 must stay exitServerError.
			name: "503 with no marker -> server error, NOT unarmed",
			args: []string{"task", "ls"},
			set: func(h *harness) {
				h.json("GET /api/tasks", http.StatusServiceUnavailable, `{"error":"database unavailable"}`)
			},
			want: exitServerError,
		},
		{
			// The fail-closed route's refusal, keyed on the MARKER rather than on
			// the status or the route. `plugins retry` is the remaining write
			// surface gated on requireArmedHookToken (agents.go).
			name: "503 carrying the unarmed marker -> exit 9",
			args: []string{"plugins", "retry", "6"},
			set: func(h *harness) {
				h.json("POST /api/plugins/messages/6/retry", http.StatusServiceUnavailable,
					`{"error":"this route requires a configured hook token","unarmed":true}`)
			},
			want: exitUnarmed,
		},
		{
			// And the SAME route with `unarmed:false` must NOT be read as unarmed —
			// the *bool distinction, exercised rather than asserted about.
			name: "503 with unarmed:false -> server error",
			args: []string{"plugins", "retry", "6"},
			set: func(h *harness) {
				h.json("POST /api/plugins/messages/6/retry", http.StatusServiceUnavailable,
					`{"error":"backend down","unarmed":false}`)
			},
			want: exitServerError,
		},
		{
			name: "400 -> usage",
			args: []string{"task", "ls", "--status", "nonsense"},
			set:  func(h *harness) { h.json("GET /api/tasks", http.StatusBadRequest, `{"error":"unknown status"}`) },
			want: exitUsage,
		},
		{
			name: "401 -> auth",
			args: []string{"task", "ls"},
			set: func(h *harness) {
				h.json("GET /api/tasks", http.StatusUnauthorized, `{"error":"invalid or missing hook token"}`)
			},
			want: exitAuth,
		},
		{
			name: "403 -> auth",
			args: []string{"task", "ls"},
			set:  func(h *harness) { h.json("GET /api/tasks", http.StatusForbidden, `{"error":"forbidden"}`) },
			want: exitAuth,
		},
		{
			name: "404 with a JSON body -> resource not found",
			args: []string{"task", "get", "999"},
			set:  func(h *harness) { h.json("GET /api/tasks/999", http.StatusNotFound, `{"error":"task not found"}`) },
			want: exitNotFound,
		},
		{
			// The router's own 404 is text/plain. That is the ONLY structural signal
			// separating "no such task" from "no such route".
			name: "404 text/plain from the router -> route absent",
			args: []string{"task", "get", "999"},
			set:  func(h *harness) {},
			want: exitRouteAbsent,
		},
		{
			name: "409 -> conflict",
			args: []string{"task", "create", "--body", "x"},
			set:  func(h *harness) { h.json("POST /api/tasks", http.StatusConflict, `{"error":"task is in progress"}`) },
			want: exitConflict,
		},
		{
			// An SSO proxy answering 401 with an HTML body and a cross-host
			// Location. That is the portal, not muster rejecting a token, so it must
			// not send the operator hunting a credential that is fine.
			name: "401 with an HTML body -> a portal in front, not a bad token",
			args: []string{"agent", "ls"},
			set: func(h *harness) {
				h.handle("GET /api/agents", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.Header().Set("Location", "https://login.example.test/?rd=%2Fapi%2Fagents&rm=GET")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`<a href="https://login.example.test/">401 Unauthorized</a>`))
				})
			},
			want: exitNonJSON,
		},
		{
			name: "200 text/html -> non-JSON",
			args: []string{"agent", "ls"},
			set: func(h *harness) {
				h.handle("GET /api/agents", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("<!DOCTYPE html><html><body>sign in</body></html>"))
				})
			},
			want: exitNonJSON,
		},
		{
			name: "application/json with a truncated body -> non-JSON",
			args: []string{"agent", "ls"},
			set:  func(h *harness) { h.json("GET /api/agents", http.StatusOK, `[{"id":1,`) },
			want: exitNonJSON,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.set(h)
			got := h.runCLI(tc.args...)
			if got.code != tc.want {
				t.Fatalf("exit = %d, want %d (stdout=%q stderr=%q)", got.code, tc.want, got.stdout, got.stderr)
			}
		})
	}
}

// TestAuthPortalRedirectIsNotFollowed: an SSO proxy bounces an unauthenticated
// request to a login page. The CLI must NOT follow it and must NOT parse portal
// HTML as data.
func TestAuthPortalRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t)
	portalHit := false
	h.handle("GET /api/agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://login.example.test/?rd=%2Fapi%2Fagents")
		w.WriteHeader(http.StatusFound)
	})
	h.handle("GET /portal", func(w http.ResponseWriter, _ *http.Request) { portalHit = true })

	got := h.runCLI("agent", "ls")
	if got.code != exitNonJSON {
		t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitNonJSON, got.stderr)
	}
	if portalHit {
		t.Fatalf("the CLI followed the redirect")
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty", got.stdout)
	}
	if !strings.Contains(got.stderr, "--api-url at the service directly") {
		t.Fatalf("stderr = %q, want the bypass-the-proxy hint", got.stderr)
	}
	// Exactly one request: the redirect was recorded and not chased.
	if n := len(h.nonHealthRequests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

// TestPortal401DoesNotBlameTheToken: the operator must be pointed at the proxy,
// not at a credential that is perfectly valid.
func TestPortal401DoesNotBlameTheToken(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<a href="https://login.example.test/">401 Unauthorized</a>`))
	})

	got := h.runCLI("agent", "ls")
	if got.code != exitNonJSON {
		t.Fatalf("exit = %d, want %d", got.code, exitNonJSON)
	}
	if strings.Contains(got.stderr, "Check "+envToken) {
		t.Fatalf("stderr = %q, want it NOT to blame the token for a portal bounce", got.stderr)
	}
	if !strings.Contains(got.stderr, "in FRONT of muster") {
		t.Fatalf("stderr = %q, want it to name the gate in front", got.stderr)
	}
	// NEGATIVE CONTROL: muster's OWN 401 (a JSON body) still exits 3 and still
	// names the token, so the new branch has not swallowed the real auth-failure
	// case.
	h2 := newHarness(t)
	h2.json("GET /api/agents", http.StatusUnauthorized, `{"error":"invalid or missing hook token"}`)
	got2 := h2.runCLI("agent", "ls")
	if got2.code != exitAuth {
		t.Fatalf("muster's own 401 gave exit %d, want %d", got2.code, exitAuth)
	}
	if !strings.Contains(got2.stderr, "Check "+envToken) {
		t.Fatalf("stderr = %q, want the token hint on a genuine auth failure", got2.stderr)
	}
}

// TestNetworkFailure covers exit 6.
func TestNetworkFailure(t *testing.T) {
	h := newHarness(t)
	// Port 1 on loopback: nothing listens, so this is a connection refusal
	// rather than a timeout, and the test stays fast.
	got := h.runCLI("--api-url", "http://127.0.0.1:1", "agent", "ls")
	if got.code != exitNetwork {
		t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitNetwork, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty", got.stdout)
	}
}

// TestAClientDeadlineIsExitTenNotExitSix is the discriminating control for the
// two "something went wrong on the wire" codes.
//
// 🔴 A CONNECTION REFUSAL AND AN ELAPSED DEADLINE BOTH ARRIVE AS *url.Error,
// and collapsing them tells a caller whose request DID reach the server that
// the network is unreachable — after which a retry loop can repeat a write that
// already landed. The server here accepts the connection and then never answers.
func TestAClientDeadlineIsExitTenNotExitSix(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h.handle("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})

	got := h.runCLI("--timeout", "150ms", "task", "ls")
	if got.code != exitAborted {
		t.Fatalf("exit = %d, want %d (exitAborted) — an elapsed client deadline is not a dead "+
			"network.\nstderr=%q", got.code, exitAborted, got.stderr)
	}
	if !strings.Contains(got.stderr, "THIS CLIENT stopped waiting") {
		t.Fatalf("stderr = %q, want this branch's own message", got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty", got.stdout)
	}
}

// TestTheTimeoutFlagReachesTheTransport: the deadline above is only meaningful
// if the flag is what set it.
func TestTheTimeoutFlagReachesTheTransport(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	if got, code := h.runCapturingTimeout("task", "ls"); got != rootDefaultTimeout || code != exitOK {
		t.Fatalf("default: transport built with %s (exit %d), want %s", got, code, rootDefaultTimeout)
	}
	if got, code := h.runCapturingTimeout("--timeout", "7s", "task", "ls"); got != 7*time.Second || code != exitOK {
		t.Fatalf("explicit: transport built with %s (exit %d), want 7s", got, code)
	}
}

// TestUnknownFieldsArePassedThrough: a NEWER server adding fields must not
// break an older CLI, and the added fields must survive to stdout. The bytes
// are never re-marshalled, so this holds structurally.
func TestUnknownFieldsArePassedThrough(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`[{"id":7,"name":"clever-fox","aFieldFromTheFuture":{"nested":[1,2,3]},"anotherOne":true}]`)

	got := h.runCLI("agent", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
		t.Fatalf("stdout not JSON: %v", err)
	}
	if _, ok := out[0]["aFieldFromTheFuture"]; !ok {
		t.Fatalf("unknown field was dropped: %v", out[0])
	}
	if out[0]["anotherOne"] != true {
		t.Fatalf("unknown field mangled: %v", out[0])
	}
}

// TestTheSkewProbeIsUnauthenticatedAndHappensOnce: /health is open, so the CLI
// must not send the token there, and the probe must fire at most once per
// process. `agent messages` makes one API call per invocation.
func TestTheSkewProbeIsUnauthenticatedAndHappensOnce(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents/agent/messages", http.StatusOK, `{"messages":[],"count":0}`)

	got := h.runCLI("agent", "messages", "agent")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var health int
	for _, r := range h.requests() {
		if r.uri != "/health" {
			continue
		}
		health++
		if r.auth != "" {
			t.Fatalf("Authorization header sent to the open /health route: %q", r.auth)
		}
	}
	if health != 1 {
		t.Fatalf("the skew probe fired %d times for a single-request verb, want exactly 1", health)
	}
	// Positive control: the one API call DID happen, so "exactly one probe" is
	// not passing because nothing ran.
	if n := len(h.nonHealthRequests()); n != 1 {
		t.Fatalf("saw %d API requests, want 1 — the fixture did not exercise the verb", n)
	}
}

// TestUnarmedHookSurfaceDistinguishesAbsentFromFalse is the unit half of the
// *bool decision. The command-level cases above cover the two live paths; this
// covers the third state a plain bool cannot represent.
func TestUnarmedHookSurfaceDistinguishesAbsentFromFalse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"marker true", `{"unarmed":true}`, true},
		{"marker false", `{"unarmed":false}`, false},
		{"marker absent", `{"error":"database unavailable"}`, false},
		{"empty object", `{}`, false},
		{"not JSON at all", `502 Bad Gateway`, false},
		{"wrong type", `{"unarmed":"yes"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unarmedHookSurface([]byte(tc.body)); got != tc.want {
				t.Fatalf("unarmedHookSurface(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
