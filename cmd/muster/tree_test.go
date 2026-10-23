package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// THE VERB LEDGER.
//
// 🔴 EXACT EQUALITY, IN THREE DIRECTIONS, AND EACH ONE CATCHES A DIFFERENT
// MISTAKE. This binary's verb set was decided in the extraction plan: twelve,
// named. Anything that changes that set has to change this file, which is a
// line a reviewer reads.
//
//	(1) len(registered) == wantVerbCount     — a verb cannot be ADDED silently,
//	                                           even WITH a ledger row, because
//	                                           the count is pinned to the number
//	                                           the plan names rather than to
//	                                           len(wiring).
//	(2) len(wiring)     == wantVerbCount     — and the ledger cannot drift from
//	                                           it either.
//	(3) the two NAME SETS are equal          — so a verb cannot be renamed, and
//	                                           a ledger row cannot name a verb
//	                                           that does not exist.
//
// ⚠ PINNING ONLY len(registered) == len(wiring) IS THE WEAK FORM, AND IT IS THE
// ONE THE UPSTREAM LEDGER USED. Under it, adding a verb and adding its ledger
// row in the same commit passes — which is exactly how a set that was supposed
// to be a decision becomes a default. The literal below is the decision.
//
// And each row is asserted by EXERCISING the command and observing which route
// the fake server actually received. Asserting existence alone would prove only
// that a name is registered; this proves the name reaches an endpoint.
// ---------------------------------------------------------------------------

// wantVerbCount is the number of verbs this binary is specified to have.
//
// 🔴 CHANGING IT IS A DESIGN DECISION, NOT A TEST FIX. The extraction split the
// upstream CLI's verbs between two binaries; this half is: task ls/get/create/
// status/comment, agent ls/resolve/messages, agent task get/comment/status, and
// chief ask. If a thirteenth is genuinely wanted, change this number, add its
// ledger row, and say so in the commit — do not discover it in a diff.
const wantVerbCount = 25

// minRunnableCommands is a hardcoded floor BELOW wantVerbCount. It exists so
// that a tree wired to NOTHING cannot pass this file vacuously: a tree that lost
// its subcommands would count 0 and fail here with a message about the wiring
// rather than about the ledger.
const minRunnableCommands = 8

// wiring is the command -> route map.
var wiring = []struct {
	name      string
	args      []string
	wantRoute string
}{
	{"agent ls", []string{"agent", "ls"}, "GET /api/agents"},
	{"agent resolve", []string{"agent", "resolve", "clever-fox"}, "GET /api/agents"},
	// The machine read of an agent's chat. It takes the NAME in the path, so
	// unlike `chief ask` it performs no resolve step — asserting it reaches
	// /api/agents/{name}/messages rather than /api/agents is what pins that.
	{"agent messages", []string{"agent", "messages", "chief"}, "GET /api/agents/chief/messages"},
	// The /agent/task* family carries no id: the token identifies the caller and
	// the server resolves the bound task from it. That these three reach
	// /agent/task* rather than /api/tasks/* is the whole point of the commands —
	// those routes are what preserve agent attribution and the complete gate.
	{"agent task get", []string{"agent", "task", "get"}, "GET /agent/task"},
	{"agent task comment", []string{"agent", "task", "comment", "--body", "x"}, "POST /agent/task/comment"},
	{"agent task status", []string{"agent", "task", "status", "in_progress"}, "PATCH /agent/task/status"},
	{"task ls", []string{"task", "ls"}, "GET /api/tasks"},
	{"task get", []string{"task", "get", "5"}, "GET /api/tasks/5"},
	{"task create", []string{"task", "create", "--body", "x"}, "POST /api/tasks"},
	{"task status", []string{"task", "status", "5", "in_progress"}, "PATCH /api/tasks/5/status"},
	{"task comment", []string{"task", "comment", "5", "--body", "x"}, "POST /api/tasks/5/comments"},
	// `chief ask` resolves the agent name over GET /api/agents first, so the
	// route asserted here is the SECOND of its two requests.
	{"chief ask", []string{"chief", "ask", "--text", "how are things?"},
		"POST /api/agents/chief/messages"},
	// Replacing the chief (chief_replace.go): without --confirm the server
	// changes nothing and answers the plan.
	{"chief replace", []string{"chief", "replace", "--kind", "claude-code", "--cc-account", "acct", "--confirm", "chief"}, "POST /api/chief/replace"},
	// Accounts PR 6: personal API tokens and the identity probe.
	{"token ls", []string{"token", "ls"}, "GET /api/tokens"},
	{"token revoke", []string{"token", "revoke", "9"}, "DELETE /api/tokens/9"},
	{"whoami", []string{"whoami"}, "GET /api/whoami"},
	// Browser flows (muster 0012): a personal token's queue and read verbs.
	{"flow enqueue", []string{"flow", "enqueue", "muse.poll", "--param", "thread=Side chat"}, "POST /api/flows/jobs"},
	{"flow ls", []string{"flow", "ls"}, "GET /api/flows/jobs"},
	{"flow status", []string{"flow", "status", "9"}, "GET /api/flows/jobs/9"},
	// The 22nd verb (muse dispatch): cancel a queued / dismiss a needs_operator job.
	{"flow cancel", []string{"flow", "cancel", "9"}, "POST /api/flows/jobs/9/cancel"},
	// Human-only approvals (muster 0045): raise one, read it, wait on it.
	{"attention ask", []string{"attention", "ask", "--title", "Approve?", "--payload-file", "-"}, "POST /api/attention/asks"},
	{"attention get", []string{"attention", "get", "9"}, "GET /api/attention/asks/9"},
	{"attention wait", []string{"attention", "wait", "9"}, "GET /api/attention/asks/9"},
	// Session routing: a mirrored session's reply_to_chief (route.go).
	{"route reply", []string{"route", "reply", "rt_0123456789abcdef01234567", "--body", "x"}, "POST /api/routes/rt_0123456789abcdef01234567/reply"},
	// Retrying an unconfirmed plugin message (plugin_retry.go).
	{"plugins retry", []string{"plugins", "retry", "6"}, "POST /api/plugins/messages/6/retry"},
}

// runnableCommands walks the tree and returns the full path of every command
// that actually does work. Our commands all use RunE; cobra's built-in `help`
// uses Run, so this is a structural filter rather than a name blocklist.
func runnableCommands(c *cobra.Command, prefix string) []string {
	var out []string
	name := strings.TrimSpace(prefix + " " + c.Name())
	if c.RunE != nil {
		out = append(out, name)
	}
	for _, sub := range c.Commands() {
		out = append(out, runnableCommands(sub, name)...)
	}
	return out
}

func TestTheVerbLedgerIsExactlyTheSpecifiedSet(t *testing.T) {
	a := inspectApp()
	root := newRootCmd(a)
	var registered []string
	for _, r := range runnableCommands(root, "") {
		registered = append(registered, strings.TrimPrefix(r, a.progName+" "))
	}
	t.Logf("%d commands registered, %d ledger rows, %d specified", len(registered), len(wiring), wantVerbCount)

	if len(registered) < minRunnableCommands {
		t.Fatalf("only %d runnable commands registered (%v), want at least %d — the tree is wired to nothing",
			len(registered), registered, minRunnableCommands)
	}
	// (1) and (2): both sides pinned to the SPECIFIED number, not to each other.
	if len(registered) != wantVerbCount {
		t.Fatalf("🔴 this binary registers %d verbs (%v) and is specified to have %d.\n"+
			"Adding or removing a verb is a design decision: change wantVerbCount, add or drop the "+
			"ledger row, and say so in the commit.", len(registered), registered, wantVerbCount)
	}
	if len(wiring) != wantVerbCount {
		t.Fatalf("🔴 the wiring ledger has %d rows and this binary is specified to have %d verbs",
			len(wiring), wantVerbCount)
	}

	// (3) the two NAME SETS are equal, with a duplicate check so two rows naming
	// one verb cannot stand in for a missing row.
	have := map[string]bool{}
	for _, r := range registered {
		if have[r] {
			t.Fatalf("the command tree reports %q twice; runnableCommands is not producing unique paths", r)
		}
		have[r] = true
	}
	want := map[string]bool{}
	for _, w := range wiring {
		if want[w.name] {
			t.Fatalf("the wiring ledger names %q twice — two rows for one verb would hide a missing row", w.name)
		}
		want[w.name] = true
	}
	for name := range want {
		if !have[name] {
			t.Errorf("🔴 the ledger names %q but the tree does not have it; the tree has %v", name, registered)
		}
	}
	for name := range have {
		if !want[name] {
			t.Errorf("🔴 the tree has %q and the ledger does not exercise it. Every verb must prove it "+
				"reaches an endpoint; an unexercised one is a name with no evidence behind it.", name)
		}
	}
}

func TestEveryCommandReachesItsRoute(t *testing.T) {
	for _, w := range wiring {
		t.Run(w.name, func(t *testing.T) {
			h := newHarness(t)
			// 🔴 TWO ROWS, AND THE IDS ARE DIFFERENT ON PURPOSE. `chief ask`
			// resolves a name against this roster before it writes, so a fixture
			// where both agents shared an id could not tell a verb that resolved the
			// RIGHT name from one that took whatever came first.
			h.json("GET /api/agents", http.StatusOK,
				`[{"id":11,"name":"clever-fox"},{"id":12,"name":"chief"}]`)
			h.json("GET /api/agents/chief/messages", http.StatusOK,
				`{"agent":"chief","agentId":12,"sessionId":8,"sessionCount":1,"sessions":[],`+
					`"messages":[{"id":1,"sessionId":8,"role":"user","kind":"text","content":"hi",`+
					`"toolId":"","toolName":"","toolOk":false,"createdAt":"2000-01-02T12:00:00Z"}],`+
					`"count":1,"limit":50,"maxLimit":200,"before":0,"hasMore":false,"nextBefore":0}`)
			h.json("POST /api/agents/chief/messages", http.StatusOK,
				`{"agent":"chief","agentId":12,"sessionId":8,"reply":"all quiet"}`)
			h.json("GET /api/tasks", http.StatusOK, `[]`)
			h.json("GET /api/tasks/5", http.StatusOK, `{"id":5}`)
			h.json("POST /api/tasks", http.StatusOK, `{"id":5}`)
			h.json("PATCH /api/tasks/5/status", http.StatusOK, `{"id":5}`)
			h.json("POST /api/tasks/5/comments", http.StatusOK, `{"id":5,"author":"claude-code"}`)
			// The /agent/task* family takes no id — the token identifies the caller
			// and the server resolves its bound task.
			h.json("GET /agent/task", http.StatusOK, `{"id":42,"status":"in_progress"}`)
			h.json("POST /agent/task/comment", http.StatusOK, `{"id":1,"noteId":42,"author":"spry-newt"}`)
			h.json("PATCH /agent/task/status", http.StatusOK, `{"id":42,"status":"in_progress"}`)
			h.json("GET /api/tokens", http.StatusOK, `[]`)
			h.json("DELETE /api/tokens/9", http.StatusOK, `{"id":9,"revoked":true}`)
			h.json("GET /api/whoami", http.StatusOK, `{"userId":7,"role":"member","via":"api-token"}`)
			h.json("POST /api/flows/jobs", http.StatusCreated, `{"id":9,"flow":"muse.poll","status":"queued"}`)
			h.json("GET /api/flows/jobs", http.StatusOK, `[]`)
			h.json("GET /api/flows/jobs/9", http.StatusOK, `{"id":9,"status":"done"}`)
			h.json("POST /api/flows/jobs/9/cancel", http.StatusOK, `{"id":9,"status":"cancelled"}`)
			h.json("POST /api/attention/asks", http.StatusCreated, `{"id":9,"status":"open","humanOnly":true}`)
			h.json("GET /api/attention/asks/9", http.StatusOK, `{"id":9,"status":"answered","approved":true}`)
			h.json("POST /api/routes/rt_0123456789abcdef01234567/reply", http.StatusOK, `{"route_id":"rt_0123456789abcdef01234567","state":"relayed","nested":{"a":1}}`)
			h.json("POST /api/plugins/messages/6/retry", http.StatusAccepted, `{"id":6,"status":"queued","retrying":true}`)
			h.json("POST /api/chief/replace", http.StatusOK, `{"deleted":null,"created":true,"agent":{"id":2,"name":"chief"}}`)

			got := h.runCLI(w.args...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			var hit bool
			for _, r := range h.requests() {
				if r.method+" "+strings.SplitN(r.uri, "?", 2)[0] == w.wantRoute {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("command %q never reached %s; observed %+v", w.name, w.wantRoute, h.requests())
			}
		})
	}
}

// TestHelpDocumentsExitCodes: the exit-code contract must be readable from the
// binary, not only from a design document.
func TestHelpDocumentsExitCodes(t *testing.T) {
	h := newHarness(t)
	got := h.runCLI("--help")
	if got.code != exitOK {
		t.Fatalf("exit = %d", got.code)
	}
	for _, want := range []string{"Exit codes:", "8  non-JSON", "empty path parameter", "route absent"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("--help output does not document %q:\n%s", want, got.stdout)
		}
	}
}
