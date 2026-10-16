package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
)

// THE FOCUS TOOLS, on BOTH transports: the gateway's in-process dispatch
// (AgentToolDispatch — what a gateway agent's /v1/responses turn runs) and
// POST /mcp (what a claude-code agent calls), against a real Postgres.

// fakeFocusSummarizer is focus.Summarizer, recording what it was asked.
type fakeFocusSummarizer struct {
	mu    sync.Mutex
	calls int
	tails []string
}

func (f *fakeFocusSummarizer) Summarize(_ context.Context, _ int64, tail string) (focus.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tails = append(f.tails, tail)
	return focus.Summary{Title: "Reconciling the Stripe invoices", Description: "Matching payouts to the ledger."}, nil
}

// surfaces calls tool as agent a on each transport and returns each answer's
// text: "gateway" (the dispatch closure) and "mcp" (POST /mcp with token).
func (g *gatedFixture) surfaces(t *testing.T, a agents.Agent, token, tool, args string) map[string]string {
	t.Helper()
	out := map[string]string{"gateway": g.srv.AgentToolDispatch(g.asOwner, a)(tool, args)}
	r, _ := g.mcp(t, token, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`, nil)
	if r == nil {
		t.Fatalf("mcp %s: no JSON-RPC answer", tool)
	}
	if r.Error != nil {
		out["mcp"] = "rpc-error " + strconv.Itoa(r.Error.Code) + " " + r.Error.Message
	} else {
		out["mcp"], _ = toolText(t, r)
	}
	return out
}

func TestAgentsListAndFocusSearchOnBothSurfaces(t *testing.T) {
	g := newGatedFixture(t)
	if _, err := g.focus.Upsert(g.asOwner, focus.AgentSubject(g.worker.ID), focus.Write{Title: "Kernel upgrade on the GPU node",
		Description: "Draining workloads first.", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.focus.Upsert(g.asOwner, focus.AgentSubject(g.chief.ID), focus.Write{Title: "Triage the inbox", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	workerID := "agent:" + strconv.FormatInt(g.worker.ID, 10)
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "agents_list", `{}`) {
		var res struct {
			Sessions []agentsListRow `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatalf("%s: %s", surface, text)
		}
		found := false
		for _, row := range res.Sessions {
			if row.ID == workerID {
				found = row.Focus == "Kernel upgrade on the GPU node" && row.Kind != "" && row.Name != ""
			}
		}
		if !found {
			t.Fatalf("%s: the worker's row with its focus title is missing: %s", surface, text)
		}
		if strings.Contains(text, "Draining") {
			t.Fatalf("%s: agents_list carries descriptions; it is titles only: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"kernel","limit":3}`) {
		var res struct {
			Results []struct{ ID, Kind, Title, Snippet string } `json:"results"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil || len(res.Results) != 1 || res.Results[0].ID != workerID ||
			res.Results[0].Kind != "agent" || res.Results[0].Snippet != "Draining workloads first." {
			t.Fatalf("%s: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"zeppelin"}`) {
		if text != `{"results":[]}` {
			t.Fatalf("%s: a non-match returned %s", surface, text)
		}
	}
}

// TestFocusRefreshOnBothSurfaces: a refresh of one subject reads the agent's
// tail, calls the summarizer, stores the result; a second refresh over the
// SAME tail is the fingerprint skip — no further summarizer call.
func TestFocusRefreshOnBothSurfaces(t *testing.T) {
	g := newGatedFixture(t)
	sess, err := g.srv.ext.Agents.CreateSession(g.asOwner, g.worker.ID, g.worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []agents.ChatMessage{{Role: "user", Content: "reconcile the March invoices"},
		{Role: "assistant", Content: "Pulling the payout report now."}} {
		m.AgentID, m.SessionID = g.worker.ID, sess.ID
		if _, err := g.srv.ext.Agents.AddChatMessage(g.asOwner, m); err != nil {
			t.Fatal(err)
		}
	}
	args := `{"subject":"agent:` + strconv.FormatInt(g.worker.ID, 10) + `"}`
	got := g.surfaces(t, g.chief, g.chiefTok, "focus_refresh", args)
	if !strings.Contains(got["gateway"], `"outcome":"updated"`) || !strings.Contains(got["gateway"], "Reconciling the Stripe invoices") {
		t.Fatalf("gateway: %s", got["gateway"])
	}
	if !strings.Contains(got["mcp"], `"outcome":"unchanged"`) {
		t.Fatalf("mcp (same tail): %s", got["mcp"])
	}
	if g.summ.calls != 1 || !strings.Contains(g.summ.tails[0], "user: reconcile the March invoices") {
		t.Fatalf("summarizer: %d call(s), tails %q", g.summ.calls, g.summ.tails)
	}
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_refresh", `{"subject":"pod:1"}`) {
		if !isToolErr(text) {
			t.Fatalf("%s: a bad subject was accepted: %s", surface, text)
		}
	}
}

// TestTheFocusChiefToolsRefuseAWorker: a worker is neither offered nor able to
// call agents_list, focus_search or focus_refresh — on the gateway the dispatch
// refuses with the chief-only error, on /mcp the name is unknown.
func TestTheFocusChiefToolsRefuseAWorker(t *testing.T) {
	g := newGatedFixture(t)
	for _, tool := range []string{"agents_list", "focus_search", "focus_refresh"} {
		got := g.surfaces(t, g.worker, g.workerTok, tool, `{"query":"x"}`)
		if got["gateway"] != toolErr("%s is available to your owner's chief agent only", tool) {
			t.Errorf("gateway %s as a worker: %s", tool, got["gateway"])
		}
		if !strings.HasPrefix(got["mcp"], "rpc-error -32602 unknown tool") {
			t.Errorf("mcp %s as a worker: %s", tool, got["mcp"])
		}
		// Positive control: the chief is answered by the tool itself.
		if c := g.surfaces(t, g.chief, g.chiefTok, tool, `{"query":"x"}`); strings.Contains(c["gateway"], "chief agent only") ||
			strings.HasPrefix(c["mcp"], "rpc-error") {
			t.Errorf("the chief was refused %s: %v", tool, c)
		}
	}
}

// TestFocusRefreshRequiresASubject: there is no whole-owner form (the periodic
// pass covers it); with no subject both surfaces refuse, and nothing runs.
func TestFocusRefreshRequiresASubject(t *testing.T) {
	g := newGatedFixture(t)
	for _, args := range []string{`{}`, `{"subject":"  "}`} {
		for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_refresh", args) {
			if text != toolErr("subject is required: the session to refresh (agent:<id> or mirror:<claude session id>)") {
				t.Errorf("%s %s: %s", surface, args, text)
			}
		}
	}
	if g.summ.calls != 0 {
		t.Fatalf("a refresh with no subject called the summarizer %d time(s)", g.summ.calls)
	}
	for _, d := range g.srv.AgentToolDefsFor(g.asOwner, g.chief) {
		if d.Name == "focus_refresh" {
			if req, _ := d.Parameters["required"].([]string); len(req) != 1 || req[0] != "subject" {
				t.Fatalf("focus_refresh's schema does not require subject: %v", d.Parameters)
			}
		}
	}
}

// TestAgentsListSaysWhyAFocusIsMissingOrStale: a session whose only refresh
// failed lists the reason and no focus; one whose refresh failed AFTER its
// focus lists both; a current focus lists no problem — on both surfaces.
func TestAgentsListSaysWhyAFocusIsMissingOrStale(t *testing.T) {
	g := newGatedFixture(t)
	worker, chief := focus.AgentSubject(g.worker.ID), focus.AgentSubject(g.chief.ID)
	if err := g.focus.RecordError(g.asOwner, worker, focus.Reason(focus.ErrNoKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.focus.Upsert(g.asOwner, chief, focus.Write{Title: "Triage the inbox", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := g.focus.RecordError(g.asOwner, chief, focus.Reason(focus.ErrRateLimited)); err != nil {
		t.Fatal(err)
	}
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "agents_list", `{}`) {
		var res struct {
			Sessions []agentsListRow `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatalf("%s: %s", surface, text)
		}
		got := map[string]agentsListRow{}
		for _, r := range res.Sessions {
			got[r.ID] = r
		}
		if r := got[worker.String()]; r.Focus != "" || r.FocusProblem != focus.ReasonNoKey {
			t.Errorf("%s: missing focus row = %+v", surface, r)
		}
		if r := got[chief.String()]; r.Focus != "Triage the inbox" || r.FocusProblem != focus.ReasonRateLimited {
			t.Errorf("%s: stale focus row = %+v", surface, r)
		}
	}
	// A later success clears it.
	if _, err := g.focus.Upsert(g.asOwner, chief, focus.Write{Title: "Triage the inbox", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Sessions []agentsListRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(g.srv.AgentToolDispatch(g.asOwner, g.chief)("agents_list", `{}`)), &after); err != nil {
		t.Fatal(err)
	}
	for _, r := range after.Sessions {
		if r.ID == chief.String() && (r.FocusProblem != "" || r.Focus != "Triage the inbox") {
			t.Fatalf("after a success the row still lists a problem: %+v", r)
		}
	}
}
