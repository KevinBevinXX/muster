package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is muster's approval relay as the hook sees it: it files requests,
// answers polls with a scripted sequence of states, and records every call.
type fakeRelay struct {
	t         *testing.T
	createSt  int      // status for POST /agent/approvals (0 = 201)
	states    []string // successive GET bodies: "pending", "decided:approve", "gone", "404", "502"
	mu        sync.Mutex
	calls     []string
	created   map[string]any
	tokenSeen string
}

func (f *fakeRelay) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.tokenSeen = r.Header.Get("X-Muster-Token")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/approvals":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &f.created)
			if f.createSt != 0 {
				w.WriteHeader(f.createSt)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"requestId":"req-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agent/approvals/req-1":
			st := "pending"
			if len(f.states) > 0 {
				st, f.states = f.states[0], f.states[1:]
				if len(f.states) == 0 {
					f.states = []string{st} // the last state repeats
				}
			}
			switch {
			case st == "404":
				w.WriteHeader(http.StatusNotFound)
			case st == "502":
				w.WriteHeader(http.StatusBadGateway)
			case strings.HasPrefix(st, "decided:"):
				_, _ = w.Write([]byte(`{"state":"decided","response":"` + strings.TrimPrefix(st, "decided:") + `"}`))
			default:
				_, _ = w.Write([]byte(`{"state":"` + st + `"}`))
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/agent/approvals/req-1":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
}

func (f *fakeRelay) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newTestApprover(base string) approver {
	return approver{
		base: base, token: "agent-own-token", deadline: 2 * time.Second, poll: 10 * time.Millisecond,
		client: &http.Client{Timeout: 2 * time.Second}, logf: func(string, ...any) {},
	}
}

const bashEvent = `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"},"permission_mode":"default","cwd":"/data/workspace","session_id":"s1"}`

func decideWith(t *testing.T, f *fakeRelay, event string) approvalDecision {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var ev permissionEvent
	if err := json.Unmarshal([]byte(event), &ev); err != nil {
		t.Fatal(err)
	}
	return newTestApprover(srv.URL).decide(context.Background(), ev)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestApproveAllowsOnlyOnAnExplicitApprove(t *testing.T) {
	f := &fakeRelay{t: t, states: []string{"pending", "pending", "decided:approve"}}
	if d := decideWith(t, f, bashEvent); d != decisionAllow {
		t.Fatalf("decision %v, want allow", d)
	}
	if f.created["command"] != "rm -rf build" || f.created["tool"] != "Bash" {
		t.Fatalf("filed %v", f.created)
	}
	if f.tokenSeen != "agent-own-token" {
		t.Fatalf("the hook sent token %q, want the agent's own", f.tokenSeen)
	}
	if contains(f.callList(), "DELETE /agent/approvals/req-1") {
		t.Fatalf("a decided request was cleared: %v", f.callList())
	}
}

func TestApproveDeniesOnReject(t *testing.T) {
	f := &fakeRelay{t: t, states: []string{"decided:reject"}}
	if d := decideWith(t, f, bashEvent); d != decisionDeny {
		t.Fatalf("decision %v, want deny", d)
	}
}

// 🔴 EVERY NON-DECISION IS A DEFERRAL, NEVER AN ALLOW. Each row is a way the
// relay can fail to produce a human's approve.
func TestApproveDefersOnEveryNonDecision(t *testing.T) {
	cases := []struct {
		name       string
		f          *fakeRelay
		wantDelete bool
	}{
		{"dismissed card (ignore)", &fakeRelay{states: []string{"decided:ignore"}}, false},
		{"empty response", &fakeRelay{states: []string{"decided:"}}, false},
		{"gone (muster already cleared it; no DELETE)", &fakeRelay{states: []string{"gone"}}, false},
		{"muster forgot the id", &fakeRelay{states: []string{"404"}}, true},
		{"unknown state", &fakeRelay{states: []string{"weird"}}, true},
		{"no approval queue (503 on file)", &fakeRelay{createSt: http.StatusServiceUnavailable}, false},
		{"refused credential (403 on file)", &fakeRelay{createSt: http.StatusForbidden}, false},
		{"nobody answers before the deadline", &fakeRelay{states: []string{"pending"}}, true},
		{"the queue stays unreadable", &fakeRelay{states: []string{"502"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.f.t = t
			if d := decideWith(t, c.f, bashEvent); d != decisionDefer {
				t.Fatalf("decision %v, want defer", d)
			}
			if got := contains(c.f.callList(), "DELETE /agent/approvals/req-1"); got != c.wantDelete {
				t.Fatalf("card cleared=%v, want %v (calls %v)", got, c.wantDelete, c.f.callList())
			}
		})
	}
}

func TestApproveDefersWhenMusterIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens there now
	var ev permissionEvent
	_ = json.Unmarshal([]byte(bashEvent), &ev)
	if d := newTestApprover(base).decide(context.Background(), ev); d != decisionDefer {
		t.Fatalf("decision %v, want defer", d)
	}
}

// Prompts the relay must never see: the terminal handles them.
func TestApproveDefersWithoutContactingMuster(t *testing.T) {
	for name, ev := range map[string]string{
		"bypass mode":  strings.Replace(bashEvent, `"permission_mode":"default"`, `"permission_mode":"bypassPermissions"`, 1),
		"plan mode":    strings.Replace(bashEvent, `"permission_mode":"default"`, `"permission_mode":"plan"`, 1),
		"a question":   `{"tool_name":"AskUserQuestion","tool_input":{"questions":[]},"permission_mode":"default"}`,
		"no tool name": `{"tool_input":{},"permission_mode":"default"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRelay{t: t, states: []string{"decided:approve"}}
			if d := decideWith(t, f, ev); d != decisionDefer {
				t.Fatalf("decision %v, want defer", d)
			}
			if calls := f.callList(); len(calls) != 0 {
				t.Fatalf("muster was contacted: %v", calls)
			}
		})
	}
}

func TestApproveDefersAndClearsTheCardWhenInterrupted(t *testing.T) {
	f := &fakeRelay{t: t, states: []string{"pending"}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var ev permissionEvent
	_ = json.Unmarshal([]byte(bashEvent), &ev)
	a := newTestApprover(srv.URL)
	a.deadline = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel) // the CLI answered in the terminal and killed the hook
	if d := a.decide(ctx, ev); d != decisionDefer {
		t.Fatalf("decision %v, want defer", d)
	}
	if !contains(f.callList(), "DELETE /agent/approvals/req-1") {
		t.Fatalf("the card was not cleared: %v", f.callList())
	}
}

// The command as the CLI runs it: what reaches stdout, and the exit code.
func TestRunApproveWritesExactlyTheHookContract(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		stdout string
	}{
		{"approve", "decided:approve", approveAllow},
		{"reject", "decided:reject", approveDeny},
		{"gone", "gone", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRelay{t: t, states: []string{c.state}}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			t.Setenv(envConnectURL, srv.URL)
			t.Setenv(envAgentToken, "agent-own-token")
			var out, errb bytes.Buffer
			if code := runApprove(nil, strings.NewReader(bashEvent), &out, &errb); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if out.String() != c.stdout {
				t.Fatalf("stdout %q, want %q", out.String(), c.stdout)
			}
		})
	}
	t.Run("not offered the relay", func(t *testing.T) {
		t.Setenv(envConnectURL, "")
		t.Setenv(envAgentToken, "")
		var out, errb bytes.Buffer
		if code := runApprove(nil, strings.NewReader(bashEvent), &out, &errb); code != 0 || out.Len() != 0 {
			t.Fatalf("exit %d stdout %q, want 0 and nothing", code, out.String())
		}
	})
	t.Run("garbage payload", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runApprove(nil, strings.NewReader("not json"), &out, &errb); code != 0 || out.Len() != 0 {
			t.Fatalf("exit %d stdout %q, want 0 and nothing", code, out.String())
		}
	})
}

// The allow JSON is the CLI's documented PermissionRequest shape. Pinned whole.
func TestTheAllowAndDenyOutputsAreTheDocumentedShape(t *testing.T) {
	for out, behavior := range map[string]string{approveAllow: "allow", approveDeny: "deny"} {
		var v struct {
			H struct {
				Event    string `json:"hookEventName"`
				Decision struct {
					Behavior string `json:"behavior"`
				} `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil || v.H.Event != "PermissionRequest" || v.H.Decision.Behavior != behavior {
			t.Fatalf("%s: parsed %+v err %v", out, v, err)
		}
	}
}
