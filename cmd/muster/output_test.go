package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSkewNoteGoesToStderrOnly is the stdout-discipline guard. A version-skew
// note is a diagnostic; it must appear on stderr and stdout must stay a single
// parseable JSON document, so a `| jq` pipeline cannot be corrupted.
//
// 🔴 The assertions are specific to THIS guard: stdout is unmarshalled (a
// diagnostic prepended to it makes the document invalid), stdout is separately
// checked not to contain the note's text, and stderr is checked to contain it.
// Wiring the client's diagnostic stream to a.stdout instead of a.stderr fails
// all three.
func TestSkewNoteGoesToStderrOnly(t *testing.T) {
	h := newHarness(t)
	h.version = "9.9.9-newer-than-this-cli"
	h.json("GET /api/agents", http.StatusOK, `[{"id":7,"name":"clever-fox","namespace":"agent-clever-fox","status":"running"}]`)

	got := h.runCLI("agent", "ls")

	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", got.code, got.stderr)
	}
	var agents []map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &agents); err != nil {
		t.Fatalf("stdout is not a single JSON document (%v); a diagnostic leaked onto stdout.\nstdout = %q", err, got.stdout)
	}
	if len(agents) != 1 || agents[0]["name"] != "clever-fox" {
		t.Fatalf("stdout JSON = %+v, want the one-agent roster", agents)
	}
	if strings.Contains(got.stdout, "note:") || strings.Contains(got.stdout, "9.9.9") {
		t.Fatalf("stdout contains the skew diagnostic: %q", got.stdout)
	}
	wantNote := "note: server 9.9.9-newer-than-this-cli, " + testProgName + " built for " + buildVersion
	if !strings.Contains(got.stderr, wantNote) {
		t.Fatalf("stderr = %q, want it to contain %q", got.stderr, wantNote)
	}
}

// TestNoSkewNoteWhenVersionsMatch is the negative control for the note itself:
// without it, a test asserting "the note is not on stdout" would pass even if
// the note were never emitted at all.
func TestNoSkewNoteWhenVersionsMatch(t *testing.T) {
	h := newHarness(t) // h.version defaults to buildVersion
	h.json("GET /api/agents", http.StatusOK, `[]`)

	got := h.runCLI("agent", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "note: server") {
		t.Fatalf("stderr = %q, want no skew note when versions agree", got.stderr)
	}
}

// TestASkewProbeFailureIsSilentAndHarmless: the probe must never turn a working
// command into a failing one, and must never print about itself.
func TestASkewProbeFailureIsSilentAndHarmless(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>the health route is sick</html>"))
	})
	h.json("GET /api/agents", http.StatusOK, `[{"id":1,"name":"x"}]`)

	got := h.runCLI("agent", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, want success — a broken /health must not fail an unrelated verb.\nstderr=%q",
			got.code, got.stderr)
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want silence: a probe failure is not the caller's problem", got.stderr)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil || len(out) != 1 {
		t.Fatalf("stdout = %q (err=%v)", got.stdout, err)
	}
}

// TestErrorsNeverTouchStdout sweeps the failure paths: every one of them must
// leave stdout completely empty.
func TestErrorsNeverTouchStdout(t *testing.T) {
	cases := []struct {
		name string
		args []string
		set  func(h *harness)
	}{
		{name: "auth failure", args: []string{"agent", "ls"}, set: func(h *harness) {
			h.json("GET /api/agents", http.StatusUnauthorized, `{"error":"invalid or missing hook token"}`)
		}},
		{name: "task not found", args: []string{"task", "get", "999"}, set: func(h *harness) {
			h.json("GET /api/tasks/999", http.StatusNotFound, `{"error":"task not found"}`)
		}},
		{name: "server error", args: []string{"task", "ls"}, set: func(h *harness) {
			h.json("GET /api/tasks", http.StatusInternalServerError, `{"error":"could not list tasks"}`)
		}},
		{name: "ambiguous agent name", args: []string{"agent", "resolve", "Dup"}, set: func(h *harness) {
			h.json("GET /api/agents", http.StatusOK, `[{"id":1,"name":"dup"},{"id":2,"name":"DUP"}]`)
		}},
		{name: "unknown flag", args: []string{"agent", "ls", "--nope"}, set: func(h *harness) {}},
		{name: "missing body on create", args: []string{"task", "create"}, set: func(h *harness) {}},
		{name: "unarmed write surface", args: []string{"chief", "ask", "--text", "hi"}, set: func(h *harness) {
			h.json("GET /api/agents", http.StatusOK, `[{"id":1,"name":"chief"}]`)
			h.json("POST /api/agents/chief/messages", http.StatusServiceUnavailable, `{"error":"nope","unarmed":true}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.set(h)
			got := h.runCLI(tc.args...)
			if got.code == exitOK {
				t.Fatalf("expected a failure, got exit 0 (stdout=%q)", got.stdout)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want empty on the %s path", got.stdout, tc.name)
			}
			if strings.TrimSpace(got.stderr) == "" {
				t.Fatalf("stderr is empty; the failure was reported nowhere")
			}
		})
	}
}

// TestTokenIsNeverEchoed checks the hygiene rule across the failure paths that
// are most tempted to print connection details.
func TestTokenIsNeverEchoed(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusForbidden, `{"error":"invalid or missing hook token"}`)

	got := h.runCLI("agent", "ls")
	if got.code != exitAuth {
		t.Fatalf("exit = %d, want %d", got.code, exitAuth)
	}
	if strings.Contains(got.stderr, testToken) || strings.Contains(got.stdout, testToken) {
		t.Fatalf("the token leaked into output: stderr=%q stdout=%q", got.stderr, got.stdout)
	}
	// POSITIVE CONTROL: the token WAS sent, so "it never appears in output" is
	// not passing merely because the token was never in play.
	reqs := h.nonHealthRequests()
	if len(reqs) == 0 || reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("Authorization header = %+v, want the bearer token to have been sent", reqs)
	}
}

// TestStdoutIsIndentedJSONForEveryVerb pins the output shape across the whole
// ledger rather than for one verb.
//
// ⚠ IT IS A LEDGER-DERIVED SWEEP, not a list: a verb added without an output
// assertion is exactly the one whose output nobody checked.
func TestStdoutIsIndentedJSONForEveryVerb(t *testing.T) {
	for _, w := range wiring {
		t.Run(w.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/agents", http.StatusOK,
				`[{"id":11,"name":"clever-fox","nested":{"a":1}},{"id":12,"name":"chief","nested":{"a":1}}]`)
			h.json("GET /api/agents/chief/messages", http.StatusOK, `{"messages":[],"count":0}`)
			h.json("POST /api/agents/chief/messages", http.StatusOK, `{"reply":"ok","nested":{"a":1}}`)
			h.json("GET /api/tasks", http.StatusOK, `[{"id":1,"nested":{"a":1}}]`)
			h.json("GET /api/tasks/5", http.StatusOK, `{"id":5,"nested":{"a":1}}`)
			h.json("POST /api/tasks", http.StatusOK, `{"id":5}`)
			h.json("PATCH /api/tasks/5/status", http.StatusOK, `{"id":5}`)
			h.json("POST /api/tasks/5/comments", http.StatusOK, `{"id":5,"author":"claude-code","body":"x"}`)
			h.json("GET /agent/task", http.StatusOK, `{"id":42,"nested":{"a":1}}`)
			h.json("POST /agent/task/comment", http.StatusOK, `{"id":1,"body":"x"}`)
			h.json("PATCH /agent/task/status", http.StatusOK, `{"id":42}`)
			h.json("GET /api/tokens", http.StatusOK, `[{"id":9,"nested":{"a":1}}]`)
			h.json("DELETE /api/tokens/9", http.StatusOK, `{"id":9,"revoked":true}`)
			h.json("GET /api/whoami", http.StatusOK, `{"userId":7,"nested":{"a":1}}`)
			h.json("POST /api/flows/jobs", http.StatusCreated, `{"id":9,"params":{"thread":"x"}}`)
			h.json("GET /api/flows/jobs", http.StatusOK, `[{"id":9,"nested":{"a":1}}]`)
			h.json("GET /api/flows/jobs/9", http.StatusOK, `{"id":9,"nested":{"a":1}}`)
			h.json("POST /api/flows/jobs/9/cancel", http.StatusOK, `{"id":9,"nested":{"a":1}}`)
			h.json("POST /api/routes/rt_0123456789abcdef01234567/reply", http.StatusOK, `{"route_id":"rt_0123456789abcdef01234567","state":"relayed","nested":{"a":1}}`)
			h.json("POST /api/plugins/messages/6/retry", http.StatusAccepted, `{"id":6,"status":"queued","nested":{"a":1}}`)
			h.json("POST /api/chief/replace", http.StatusOK, `{"deleted":null,"created":true,"nested":{"a":1}}`)

			got := h.runCLI(w.args...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if !json.Valid([]byte(got.stdout)) {
				t.Fatalf("stdout is not valid JSON: %q", got.stdout)
			}
			if !strings.HasSuffix(got.stdout, "\n") {
				t.Fatalf("stdout does not end in a newline: %q", got.stdout)
			}
			// Indented, not compact — `a.emit` runs json.Indent, and a verb that
			// bypassed it would write the server's bytes straight through.
			if strings.Contains(got.stdout, `"nested":{`) {
				t.Fatalf("stdout is compact, so this verb did not go through a.emit: %q", got.stdout)
			}
		})
	}
}
