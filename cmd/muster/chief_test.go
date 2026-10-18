package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestChiefAskResolvesThenWritesToTheResolvedName is the two-step behaviour.
//
// 🔴 THE ROSTER READ IS NOT A LEFTOVER FROM WHEN THE ROUTE TOOK AN ID. It
// settles an ambiguous or display-name --agent against the real roster, so a
// capitalised name reaches the right agent instead of becoming a 404, and an
// ambiguous one is refused here rather than delivered to whichever row the
// server happened to match. The fixture's two agents have DIFFERENT ids and
// names so a verb that took whatever came first is distinguishable from one that
// resolved.
func TestChiefAskResolvesThenWritesToTheResolvedName(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`[{"id":11,"name":"clever-fox","displayName":"Clever Fox"},{"id":12,"name":"chief","displayName":"Chief"}]`)
	h.json("POST /api/agents/chief/messages", http.StatusOK,
		`{"agent":"chief","agentId":12,"sessionId":8,"reply":"all quiet"}`)

	got := h.runCLI("chief", "ask", "--text", "status?")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 2 {
		t.Fatalf("sent %d requests, want 2 (resolve then write): %+v", len(reqs), reqs)
	}
	if reqs[0].method+" "+reqs[0].uri != "GET /api/agents" {
		t.Fatalf("first request = %s %s, want the roster read", reqs[0].method, reqs[0].uri)
	}
	if reqs[1].method+" "+reqs[1].uri != "POST /api/agents/chief/messages" {
		t.Fatalf("second request = %s %s", reqs[1].method, reqs[1].uri)
	}
	// BOTH steps present the same single credential.
	for i, r := range reqs {
		if r.auth != "Bearer "+testToken {
			t.Fatalf("request %d sent Authorization %q, want the hook token — this verb takes ONE credential", i, r.auth)
		}
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[1].body), &sent); err != nil {
		t.Fatalf("write body not JSON: %v (%q)", err, reqs[1].body)
	}
	if len(sent) != 1 || sent["message"] != "status?" {
		t.Fatalf("write body = %v, want exactly {message: status?}", sent)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil || out["reply"] != "all quiet" {
		t.Fatalf("stdout = %q (err=%v)", got.stdout, err)
	}
}

// TestChiefAskAddressesAnyAgentNotOnlyTheDefault: --agent is a real parameter,
// not decoration. A verb that ignored it and always wrote to the reserved name
// would pass every other test here.
func TestChiefAskAddressesAnyAgentNotOnlyTheDefault(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`[{"id":11,"name":"clever-fox"},{"id":12,"name":"chief"}]`)
	h.json("POST /api/agents/clever-fox/messages", http.StatusOK, `{"reply":"ok"}`)

	got := h.runCLI("chief", "ask", "--agent", "clever-fox", "--text", "hello")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 2 || reqs[1].uri != "/api/agents/clever-fox/messages" {
		t.Fatalf("wrote to %+v, want /api/agents/clever-fox/messages", reqs)
	}
}

// TestChiefAskResolvesADisplayNameToTheSlug: the write is keyed on the resolved
// NAME, so a display name must become a slug rather than a 404.
func TestChiefAskResolvesADisplayNameToTheSlug(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`[{"id":11,"name":"clever-fox","displayName":"the-scout"},{"id":12,"name":"chief"}]`)
	h.json("POST /api/agents/clever-fox/messages", http.StatusOK, `{"reply":"ok"}`)

	got := h.runCLI("chief", "ask", "--agent", "the-scout", "--text", "hello")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 2 || reqs[1].uri != "/api/agents/clever-fox/messages" {
		t.Fatalf("wrote to %+v, want the resolved SLUG's route", reqs)
	}
}

// TestChiefAskRefusesLocallyBeforeSpendingATurn: each refusal must fire with its
// own message and cost no request at all — asking nothing still spends an agent
// turn, and an empty name is the doubled-slash class.
func TestChiefAskRefusesLocallyBeforeSpendingATurn(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"empty text", "--text is required", []string{"chief", "ask", "--text", "   "}},
		{"missing text", "--text is required", []string{"chief", "ask"}},
		{"empty agent", "--agent is empty", []string{"chief", "ask", "--text", "hi", "--agent", "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/agents", http.StatusOK, `[{"id":1,"name":"chief"}]`)
			h.json("POST /api/agents/chief/messages", http.StatusOK, `{"reply":"ok"}`)

			got := h.runCLI(tc.args...)
			if got.code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Fatalf("stderr = %q, want this guard's own message %q", got.stderr, tc.want)
			}
			if n := len(h.requests()); n != 0 {
				t.Fatalf("server saw %d requests, want 0 — a local refusal must cost nothing", n)
			}
		})
	}
}

// TestChiefAskFailsAtTheResolveStepWithoutWriting: an unknown agent must not
// produce a write at all. Without this, a verb that skipped the resolve and
// posted the raw flag would look identical on the happy path.
func TestChiefAskFailsAtTheResolveStepWithoutWriting(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, `[{"id":12,"name":"chief"}]`)
	h.json("POST /api/agents/nobody/messages", http.StatusOK, `{"reply":"should never happen"}`)

	got := h.runCLI("chief", "ask", "--agent", "nobody", "--text", "hi")
	if got.code != exitNotFound {
		t.Fatalf("exit = %d, want %d", got.code, exitNotFound)
	}
	for _, r := range h.nonHealthRequests() {
		if r.method == http.MethodPost {
			t.Fatalf("a POST was sent despite an unresolved agent: %+v", r)
		}
	}
}

// TestChiefFamilyIsAskAndReplace is the structural half of the split decision: the
// approval-gated write verbs stayed with the upstream CLI, and this binary must
// not grow them back without that being a deliberate, reviewed change. `replace`
// is that kind of change: it reaches muster's own POST /api/chief/replace.
func TestChiefFamilyIsAskAndReplace(t *testing.T) {
	root := newRootCmd(inspectApp())
	var names []string
	for _, c := range root.Commands() {
		if c.Name() != "chief" {
			continue
		}
		for _, sub := range c.Commands() {
			names = append(names, sub.Name())
		}
	}
	if strings.Join(names, ",") != "ask,replace" {
		t.Fatalf("the `chief` family has %v, want exactly [ask replace].\n"+
			"The approval-gated pane writes reach routes this project does not serve; a verb for "+
			"one here would land on muster and 404 as exit 7 — 'this client is newer than the "+
			"server' — which is a confident wrong diagnosis.", names)
	}
}

// TestChiefReplaceSendsItsFlagsAndShowsThePlanWithoutConfirm: the verb sends
// exactly its four fields (values pairwise distinct, so a swapped flag shows),
// and the server's no-confirm answer — 428 with the plan in its error — exits
// usage (nothing was changed) with that plan on stderr and nothing on stdout.
func TestChiefReplaceSendsItsFlagsAndShowsThePlanWithoutConfirm(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/chief/replace", http.StatusPreconditionRequired,
		`{"error":"nothing was changed: the confirmed call DELETES agent chief (id 104, kind gateway, status running). Repeat with confirm \"chief\" to apply it."}`)
	got := h.runCLI("chief", "replace", "--kind", "claude-code", "--cc-account", "acct-one")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d (usage: nothing changed); stderr=%q", got.code, exitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "DELETES agent chief (id 104") || got.stdout != "" {
		t.Fatalf("the plan did not reach stderr, or stdout was written: stdout=%q stderr=%q", got.stdout, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].method+" "+reqs[0].uri != "POST /api/chief/replace" || reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("requests = %+v", reqs)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"kind": "claude-code", "cc_account": "acct-one", "model": "", "confirm": ""}
	if len(sent) != len(want) {
		t.Fatalf("body = %v, want %v", sent, want)
	}
	for k, v := range want {
		if sent[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body %v)", k, sent[k], v, sent)
		}
	}

	h2 := newHarness(t)
	h2.json("POST /api/chief/replace", http.StatusOK, `{"deleted":{"id":104},"created":true,"agent":{"id":105,"name":"chief"}}`)
	got = h2.runCLI("chief", "replace", "--kind", "gateway", "--model", "m-two", "--confirm", "chief")
	if got.code != exitOK || !strings.Contains(got.stdout, `"created": true`) {
		t.Fatalf("confirmed run: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
	if err := json.Unmarshal([]byte(h2.nonHealthRequests()[0].body), &sent); err != nil || sent["confirm"] != "chief" ||
		sent["model"] != "m-two" || sent["kind"] != "gateway" {
		t.Fatalf("confirmed body = %v (%v)", sent, err)
	}
}

// TestChiefReplaceRefusesAMissingKindBeforeSending: no request without --kind.
func TestChiefReplaceRefusesAMissingKindBeforeSending(t *testing.T) {
	h := newHarness(t)
	got := h.runCLI("chief", "replace", "--confirm", "chief")
	if got.code != exitUsage || !strings.Contains(got.stderr, "--kind is required") || len(h.nonHealthRequests()) != 0 {
		t.Fatalf("exit %d stderr %q requests %d", got.code, got.stderr, len(h.nonHealthRequests()))
	}
}
