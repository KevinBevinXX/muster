package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

func hermesKinds() stubKinds {
	return stubKinds{enabled: []string{agents.KindGateway, agents.KindClaudeCode, agents.KindHermes}, pick: "work"}
}

// A hermes dispatch stores the kind, NO Claude account (the pool is never
// asked), and keeps its model and repository — and, like a claude-code one,
// takes only a connected repository: hermesd checks it out and Hermes runs in
// it, loading its AGENTS.md.
func TestAHermesDispatchStoresItsKindModelAndConnectedRepository(t *testing.T) {
	var pins []string
	k := hermesKinds()
	k.pins = &pins
	s, h, store := kindsServer(t, k)
	connectRepos(t, s, "example-org/tide")
	rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}, "model": {"openrouter/x/y"},
		"repo": {"example-org/tide"}, "repo_branch": {"trunk"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("hermes dispatch: %d %s", rec.Code, rec.Body)
	}
	rows := store.rows()
	if len(rows) != 1 || rows[0].Kind != agents.KindHermes || rows[0].CCAccount != "" || rows[0].Model != "openrouter/x/y" ||
		rows[0].Repo != "example-org/tide" || rows[0].RepoBranch != "trunk" {
		t.Fatalf("rows = %+v", rows)
	}
	if len(pins) != 0 {
		t.Fatalf("the Claude account pool was asked for a hermes dispatch: %q", pins)
	}
	rec = postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}, "repo": {"stranger/x"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), chiefDispatchRepo) || len(store.rows()) != 1 {
		t.Fatalf("hermes + unconnected repo: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "cc_account": {"work"}}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "only be pinned") {
		t.Fatalf("a Claude account pinned on a hermes agent: %d %s", rec.Code, rec.Body)
	}
	// Not enabled: refused before the row.
	s2, h2, store2 := kindsServer(t, stubKinds{})
	if rec := postDispatch(t, s2, h2, url.Values{"kind": {"hermes"}}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "not enabled") || len(store2.rows()) != 0 {
		t.Fatalf("hermes not enabled: %d %s", rec.Code, rec.Body)
	}
}

// A hermes chief is refused before the old chief is touched.
func TestAHermesChiefIsRefusedBeforeAnythingChanges(t *testing.T) {
	s, _, store := kindsServer(t, hermesKinds())
	code, out := s.replaceChief(context.Background(), 1, chiefReplaceRequest{Kind: agents.KindHermes, Confirm: "replace"})
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "cannot be the chief") || len(store.rows()) != 0 {
		t.Fatalf("a hermes chief: %d %v", code, out)
	}
}

// task_dispatch offers the hermes kind in its schema, and the delivery lease
// outlasts the longest kind's turn (the hermes budget among them).
func TestTaskDispatchOffersHermesAndTheLeaseOutlastsItsTurn(t *testing.T) {
	var found bool
	for _, d := range taskRoutingToolDefs(true) {
		if d.Name != taskDispatchTool {
			continue
		}
		props, _ := d.Parameters["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]string)
		for _, k := range enum {
			found = found || k == agents.KindHermes
		}
	}
	if !found {
		t.Fatal("task_dispatch's kind enum does not offer hermes")
	}
	if attentionDeliveryLease() <= agentTurnBudget(agents.KindHermes) {
		t.Fatalf("the delivery lease %s does not outlast a hermes turn %s", attentionDeliveryLease(), agentTurnBudget(agents.KindHermes))
	}
}
