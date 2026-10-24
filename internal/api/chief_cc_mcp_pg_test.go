package api

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A CLAUDE-CODE CHIEF REACHES EVERY CHIEF TOOL OVER /mcp — the only route it has
// (its image carries no muster CLI). Real Postgres, every chief-tool subsystem
// wired at once (plugins, focus, routing, the attention queue), the chief and a
// worker both of kind claude-code.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: the chief predicate keys on the
// chief IDENTITY (display name / reserved slug), never on the kind, and that was
// already true before this change — so these pass on the base too. They pin that
// no kind check ever enters toolsFor, dispatchAgentTool or the attention scope;
// the mutation sweep in the PR shows each going red when one does.

type ccChiefFixture struct {
	*gatedFixture
	mirror *routeMirror
}

func newCCChiefFixture(t *testing.T) *ccChiefFixture {
	t.Helper()
	f := &ccChiefFixture{gatedFixture: &gatedFixture{}, mirror: &routeMirror{interactive: true, statusReport: true}}
	g := f.gatedFixture
	g.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		g.plugins, g.notes = plugins.NewPG(pool), notes.NewPG(pool)
		g.focus, g.summ = focus.NewPG(pool), &fakeFocusSummarizer{}
		ext.Plugins, ext.Notes, ext.Focus, ext.FocusSummarizer = g.plugins, g.notes, g.focus, g.summ
		ext.Attention = attention.NewPG(pool)
		ext.Routes, ext.Routing, ext.Mirror, ext.SendQueue = g.plugins, &Routing{}, f.mirror, sendqueue.NewPG(pool)
	})
	g.owner = g.newMember(t)
	g.asOwner = g.as(g.owner)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	g.chiefTok, g.workerTok = "cc-chief-token-"+suffix, "cc-worker-token-"+suffix
	mk := func(name, display, token string) agents.Agent {
		a, err := agents.NewPG(g.pool).Create(g.asOwner, agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: display,
			Status: agents.StatusRunning, HooksToken: token, Kind: agents.KindClaudeCode, CCAccount: "acct-x"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	g.chief = mk(agents.ChiefNameFor(g.owner), ChiefDisplayName, g.chiefTok)
	g.worker = mk("ccworker-"+suffix, "", g.workerTok)
	return f
}

func (f *ccChiefFixture) call(t *testing.T, token, name string, args any) (string, bool, *mcpError) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	r, code := f.mcp(t, token, string(raw), nil)
	if r == nil {
		t.Fatalf("/mcp %s: HTTP %d", name, code)
	}
	if r.Error != nil {
		return "", false, r.Error
	}
	text, isErr := toolText(t, r)
	return text, isErr, nil
}

func (f *ccChiefFixture) listed(t *testing.T, token string) map[string]bool {
	t.Helper()
	r, _ := f.mcp(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if r == nil || json.Unmarshal(r.Result, &res) != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	out := map[string]bool{}
	for _, tl := range res.Tools {
		out[tl.Name] = true
	}
	return out
}

// chiefToolNames are the chief-only tools, spelled here rather than read from
// the registry (a list derived from the implementation would agree with any bug
// in it).
var chiefToolNames = []string{"task_create_gated", "reply_draft", "fleet_list", "focus_search",
	"sessions_overview", "route_to_session", "attention_list"}

// TestAClaudeCodeChiefIsOfferedEveryChiefToolOverMCPAndAWorkerNone.
func TestAClaudeCodeChiefIsOfferedEveryChiefToolOverMCPAndAWorkerNone(t *testing.T) {
	f := newCCChiefFixture(t)
	chief, worker := f.listed(t, f.chiefTok), f.listed(t, f.workerTok)
	for _, n := range chiefToolNames {
		if !chief[n] {
			t.Errorf("the claude-code chief is not offered %s over /mcp", n)
		}
		if worker[n] {
			t.Errorf("a claude-code worker is offered the chief-only %s", n)
		}
	}
	for _, n := range []string{"tasks_list", "task_get", "attention_ask"} {
		if !chief[n] || !worker[n] {
			t.Errorf("%s: chief %v worker %v; every agent is offered it", n, chief[n], worker[n])
		}
	}
}

// TestAClaudeCodeChiefCallsEveryChiefToolOverMCP: each chief tool, called by the
// claude-code chief over /mcp, does its work — and the chief's scope over the
// attention queue and the board is the WHOLE owner's, which is decided by the
// chief identity: the same calls by the worker are refused or narrower.
func TestAClaudeCodeChiefCallsEveryChiefToolOverMCP(t *testing.T) {
	f := newCCChiefFixture(t)
	ok := func(name string, args any) string {
		t.Helper()
		text, isErr, rpc := f.call(t, f.chiefTok, name, args)
		if rpc != nil || isErr {
			t.Fatalf("%s by the claude-code chief: rpc=%+v text=%s", name, rpc, text)
		}
		return text
	}

	// The plugin pair, on a message delivered to the chief.
	m := f.deliveredMessage(t, f.owner, f.chief.ID, "cc-chief-1")
	ok("task_create_gated", map[string]any{"title": "cc gated", "body": "do the thing", "gate_reason": "approval", "message_id": m.ID})
	if tasks := f.ownerTasks(t); len(tasks) == 0 {
		t.Fatal("task_create_gated by the claude-code chief stored no task")
	}
	ok("reply_draft", map[string]any{"message_id": m.ID, "body": "Thanks, we will look at it."})
	// route_to_session hands a (separately) delivered message to a mirrored session.
	m2 := f.deliveredMessage(t, f.owner, f.chief.ID, "cc-chief-2")
	var routed struct{ State string }
	if err := json.Unmarshal([]byte(ok("route_to_session", json.RawMessage(routeArgs(m2, "mirror:"+routeSID)))), &routed); err != nil || routed.State != "routed" {
		t.Fatalf("route_to_session: %+v %v", routed, err)
	}

	// Focus: the whole fleet, the worker included.
	if text := ok("fleet_list", map[string]any{}); !strings.Contains(text, f.worker.Name) {
		t.Fatalf("fleet_list does not list the worker: %s", text)
	}
	ok("focus_search", map[string]any{"query": "anything"})

	// The attention queue: the WORKER asks; the chief lists it (whole board).
	// The worker cannot.
	askText, isErr, rpc := f.call(t, f.workerTok, "attention_ask", map[string]any{"question": "Which region, cc?", "options": []string{"us", "eu"}})
	if rpc != nil || isErr {
		t.Fatalf("worker ask: %v %s", rpc, askText)
	}
	if text := ok("attention_list", map[string]any{}); !strings.Contains(text, "Which region, cc?") {
		t.Fatalf("the claude-code chief's attention_list does not show the worker's ask: %s", text)
	}
	if _, _, rpc := f.call(t, f.workerTok, "attention_list", map[string]any{}); rpc == nil {
		t.Fatal("a claude-code worker called attention_list")
	}

	// The board: a task of the owner's that neither agent is bound to is on the
	// chief's tasks_list and NOT on the worker's.
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "cc unbound", Body: "an unbound task", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	needle := `"id":` + strconv.FormatInt(n.ID, 10) + `,`
	if text := ok("tasks_list", map[string]any{}); !strings.Contains(text, needle) {
		t.Fatalf("the claude-code chief's tasks_list lacks the owner's unbound task %d: %.400s", n.ID, text)
	}
	if text, _, _ := f.call(t, f.workerTok, "tasks_list", map[string]any{}); strings.Contains(text, needle) {
		t.Fatalf("the worker's tasks_list shows a task it is not bound to: %.400s", text)
	}
}
