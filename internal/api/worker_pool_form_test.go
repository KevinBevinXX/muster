package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestTheDispatchFormOffersAWorkerPoolForClaudeCode: GET /ui/agents/new on a
// deployment with the claude-code kind renders the worker_pool input POST
// /agents reads — DISABLED, so only the kind toggle can put it on the wire —
// and a gateway-only deployment's form has none.
func TestTheDispatchFormOffersAWorkerPoolForClaudeCode(t *testing.T) {
	s, h, _ := kindsServer(t, stubKinds{accounts: []string{"alpha"}})
	body := getOwed(t, s, h, "/ui/agents/new").Body.String()
	in := inputTag(body, `name="worker_pool"`)
	if in == "" {
		t.Fatalf("no worker_pool input on the dispatch form:\n%s", body)
	}
	if !strings.Contains(in, " disabled") {
		t.Fatalf("worker_pool input renders enabled (a gateway dispatch would carry it): %s", in)
	}

	s2, h2, _ := kindsServer(t, nil)
	if body := getOwed(t, s2, h2, "/ui/agents/new").Body.String(); strings.Contains(body, `name="worker_pool"`) {
		t.Fatalf("a deployment without claude-code renders a worker_pool input")
	}
}

// TestAPoolWorkerIsVisibleWhereAgentsAreListed: a dispatch with worker_pool set
// answers with the agents list, whose new card carries the pool chip; GET
// /api/agents carries workerPool. A second agent with no pool is the control:
// no chip, workerPool "".
func TestAPoolWorkerIsVisibleWhereAgentsAreListed(t *testing.T) {
	s, h, _ := kindsServer(t, stubKinds{pick: "work"})
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "worker_pool": {"reviewers"}})
	if rec.Code != 200 {
		t.Fatalf("dispatch: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `data-worker-pool="reviewers"`) {
		t.Fatalf("the agents list answered for the dispatch shows no pool chip:\n%s", rec.Body)
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}}); rec.Code != 200 {
		t.Fatalf("control dispatch: %d %s", rec.Code, rec.Body)
	}
	list := getOwed(t, s, h, "/ui/agents").Body.String()
	if n := strings.Count(list, `data-worker-pool=`); n != 1 {
		t.Fatalf("%d pool chips on a list of one pool worker and one plain agent, want 1", n)
	}

	var rows []struct {
		Name       string `json:"name"`
		WorkerPool string `json:"workerPool"`
	}
	if err := json.Unmarshal(getOwed(t, s, h, "/api/agents").Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	pools := map[string]int{}
	for _, r := range rows {
		pools[r.WorkerPool]++
	}
	if len(rows) != 2 || pools["reviewers"] != 1 || pools[""] != 1 {
		t.Fatalf("GET /api/agents pools = %v over %d rows, want one reviewers and one none", pools, len(rows))
	}
}

// inputTag returns the first <input …> tag in html containing marker, or "".
func inputTag(html, marker string) string {
	for rest := html; ; {
		i := strings.Index(rest, "<input")
		if i < 0 {
			return ""
		}
		rest = rest[i:]
		end := strings.Index(rest, ">")
		if end < 0 {
			return ""
		}
		if tag := rest[:end+1]; strings.Contains(tag, marker) {
			return tag
		}
		rest = rest[end+1:]
	}
}
