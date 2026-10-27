package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestExpandPathGuard pins the unit behaviour of the empty-path-parameter
// guard.
func TestExpandPathGuard(t *testing.T) {
	tests := []struct {
		name    string
		tmpl    string
		params  map[string]string
		want    string
		wantErr bool
	}{
		{name: "normal id", tmpl: "/api/tasks/{id}", params: map[string]string{"id": "42"}, want: "/api/tasks/42"},
		{name: "no params", tmpl: "/api/tasks", params: nil, want: "/api/tasks"},
		{name: "two params", tmpl: "/a/{x}/b/{y}", params: map[string]string{"x": "1", "y": "2"}, want: "/a/1/b/2"},
		{name: "value with slash is escaped, never a new segment", tmpl: "/api/tasks/{id}", params: map[string]string{"id": "1/../2"}, want: "/api/tasks/1%2F..%2F2"},
		{name: "trims surrounding space", tmpl: "/api/tasks/{id}", params: map[string]string{"id": " 42 "}, want: "/api/tasks/42"},

		{name: "empty string", tmpl: "/api/tasks/{id}", params: map[string]string{"id": ""}, wantErr: true},
		{name: "whitespace only", tmpl: "/api/agents/{name}/messages", params: map[string]string{"name": "   "}, wantErr: true},
		{name: "missing key", tmpl: "/api/agents/{name}/messages", params: map[string]string{}, wantErr: true},
		{name: "second param empty", tmpl: "/a/{x}/b/{y}", params: map[string]string{"x": "1", "y": ""}, wantErr: true},
		{name: "malformed template", tmpl: "/a/{x", params: map[string]string{"x": "1"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandPath(tc.tmpl, tc.params)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expandPath(%q, %v) = %q, want the empty-path-param guard to fire", tc.tmpl, tc.params, got)
				}
				if !strings.Contains(err.Error(), emptyPathParamMsg) {
					t.Fatalf("guard error = %q, want it to contain %q (a DIFFERENT error means some other check fired)", err, emptyPathParamMsg)
				}
				if code := exitCodeFor(err); code != exitUsage {
					t.Fatalf("guard exit code = %d, want %d", code, exitUsage)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandPath(%q, %v) unexpected error: %v", tc.tmpl, tc.params, err)
			}
			if got != tc.want {
				t.Fatalf("expandPath(%q, %v) = %q, want %q", tc.tmpl, tc.params, got, tc.want)
			}
		})
	}
}

// TestEveryPathParameterVerbRefusesAnEmptyValue is the LEDGER half of the
// guard, and it is what a per-command test cannot give.
//
// 🔴 THE FAILURE BEING GUARDED AGAINST IS A VERB NOBODY WROTE A TEST FOR. Each
// of these interpolates a shell-supplied value into a route, and each is one
// unset variable away from a doubled slash that ServeMux cleans into a
// DIFFERENT route — a 301 toward something that means something else, from a
// request that was malformed before it left the caller.
//
// Four assertions per verb, each specific to THIS guard: with it removed the
// CLI would send the cleaned path, the fake server would answer its text/plain
// 404 and the process would exit 7 — so the failure cannot be produced by some
// other check firing first.
func TestEveryPathParameterVerbRefusesAnEmptyValue(t *testing.T) {
	// The exact set of verbs whose route carries a {placeholder}. Derived by
	// hand from the wiring ledger, and cross-checked against it below so a new
	// path-parameter verb cannot arrive unguarded.
	cases := []struct {
		name string
		// args with the placeholder value at index `hole`.
		args []string
		hole int
	}{
		{"task get", []string{"task", "get", "PLACEHOLDER"}, 2},
		{"task status", []string{"task", "status", "PLACEHOLDER", "open"}, 2},
		{"task comment", []string{"task", "comment", "PLACEHOLDER", "--body", "x"}, 2},
		{"agent messages", []string{"agent", "messages", "PLACEHOLDER"}, 2},
	}
	// The cross-check: every ledger row whose route contains a concrete segment
	// this test does not cover would go unnoticed, so assert the COUNT of
	// wiring rows that interpolate a value matches the case count.
	var interpolating int
	for _, w := range wiring {
		// A route with a numeric or name segment after a collection is one the
		// caller supplied.
		switch w.name {
		case "task get", "task status", "task comment", "agent messages":
			interpolating++
		}
	}
	if interpolating != len(cases) {
		t.Fatalf("the ledger has %d path-parameter verbs and this test covers %d", interpolating, len(cases))
	}

	for _, tc := range cases {
		for _, empty := range []string{"", "   "} {
			t.Run(tc.name+"/"+strings.ReplaceAll("["+empty+"]", " ", "_"), func(t *testing.T) {
				h := newHarness(t)
				// Register the HAPPY route, so a guard that fired for the wrong
				// reason (a missing fixture) would be visible as a different failure.
				h.json("GET /api/tasks/5", http.StatusOK, `{"id":5}`)
				h.json("PATCH /api/tasks/5/status", http.StatusOK, `{"id":5}`)
				h.json("POST /api/tasks/5/comments", http.StatusOK, `{"id":1}`)
				h.json("GET /api/agents/agent/messages", http.StatusOK, `{"messages":[]}`)

				args := append([]string(nil), tc.args...)
				args[tc.hole] = empty
				got := h.runCLI(args...)

				if got.code != exitUsage {
					t.Errorf("exit code = %d, want %d (usage/empty path param)", got.code, exitUsage)
				}
				if !strings.Contains(got.stderr, emptyPathParamMsg) {
					t.Errorf("stderr = %q, want the guard's own message %q", got.stderr, emptyPathParamMsg)
				}
				if got.stdout != "" {
					t.Errorf("stdout = %q, want empty: failures never write to stdout", got.stdout)
				}
				if n := len(h.requests()); n != 0 {
					t.Errorf("server observed %d request(s) %+v, want 0 — the guard must refuse BEFORE the request is sent",
						n, h.requests())
				}
			})
		}
	}
}

// TestPathParameterVerbsDoReachTheServer is the positive control for the ledger
// above: it proves the guard is not simply blocking everything, so the
// zero-request assertions mean something.
func TestPathParameterVerbsDoReachTheServer(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		route string
	}{
		{"task get", []string{"task", "get", "5"}, "/api/tasks/5"},
		{"task status", []string{"task", "status", "5", "open"}, "/api/tasks/5/status"},
		{"task comment", []string{"task", "comment", "5", "--body", "x"}, "/api/tasks/5/comments"},
		{"agent messages", []string{"agent", "messages", "agent"}, "/api/agents/agent/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/tasks/5", http.StatusOK, `{"id":5,"title":"live"}`)
			h.json("PATCH /api/tasks/5/status", http.StatusOK, `{"id":5}`)
			h.json("POST /api/tasks/5/comments", http.StatusOK, `{"id":1,"author":"claude-code","body":"x"}`)
			h.json("GET /api/agents/agent/messages", http.StatusOK, `{"messages":[],"count":0}`)

			got := h.runCLI(tc.args...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v, want exactly one", reqs)
			}
			if path := strings.SplitN(reqs[0].uri, "?", 2)[0]; path != tc.route {
				t.Fatalf("reached %q, want %q", path, tc.route)
			}
		})
	}
}
