package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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

// task_dispatch does NOT offer the hermes kind (it is refused there until
// Hermes's MCP client against muster is verified), and the delivery lease
// outlasts the longest kind's turn (the hermes budget among them).
//
// ⚠ THE LEASE HALF IS AN INVARIANT GUARD, NOT REGRESSION COVERAGE: hermes's
// budget equals claude-code's today, so the pre-hermes formula (the max of
// claude-code's and gateway's) already outlasts it — the mutation sweep
// confirmed that formula passes here. It reddens only if a kind's budget ever
// outgrows the others' without the lease following.
func TestTaskDispatchDoesNotOfferHermesAndTheLeaseOutlastsItsTurn(t *testing.T) {
	for _, d := range taskRoutingToolDefs(true) {
		if d.Name != taskDispatchTool {
			continue
		}
		props, _ := d.Parameters["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]string)
		for _, k := range enum {
			if k == agents.KindHermes {
				t.Fatal("task_dispatch's kind enum offers hermes")
			}
		}
	}
	if attentionDeliveryLease() <= agentTurnBudget(agents.KindHermes) {
		t.Fatalf("the delivery lease %s does not outlast a hermes turn %s", attentionDeliveryLease(), agentTurnBudget(agents.KindHermes))
	}
}

// 🟡2 THE HERMES MODEL RULE AT DISPATCH AND AT A MODEL CHANGE: a model a hermes
// pod cannot run (not openrouter/<vendor>/<model>, case-sensitive) is refused
// before a row is created or a model stored.
func TestAHermesModelIsCheckedAtDispatchAndAtAModelChange(t *testing.T) {
	s, h, store := kindsServer(t, hermesKinds())
	for _, bad := range []string{"anthropic/claude-x", "OpenRouter/vendor/model", "openrouter/", "openrouter/a/b/c"} {
		rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}, "model": {bad}})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not an OpenRouter model") || len(store.rows()) != 0 {
			t.Fatalf("dispatch with model %q: %d %s, %d row(s)", bad, rec.Code, rec.Body, len(store.rows()))
		}
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}, "model": {"openrouter/x/y"}}); rec.Code != http.StatusOK {
		t.Fatalf("control: a good model: %d %s", rec.Code, rec.Body)
	}
	id := store.rows()[0].ID
	// The model route reads and writes the row: a store that does both.
	s.ext.Agents = &hermesModelStore{preflightStore: store}
	form := url.Values{"model": {"openrouter/a/b/c"}} // past ValidateModelCredential, not a hermes model
	req := httptest.NewRequest(http.MethodPost, "/agents/"+strconv.FormatInt(id, 10)+"/model", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not an OpenRouter model") || store.rows()[0].Model != "openrouter/x/y" {
		t.Fatalf("model change to a non-OpenRouter model: %d %s, stored %q", rec.Code, rec.Body, store.rows()[0].Model)
	}
}

// hermesModelStore is preflightStore with the row read and model write the model
// route uses.
type hermesModelStore struct {
	*preflightStore
}

func (m *hermesModelStore) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range m.rows() {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (m *hermesModelStore) SetModel(_ context.Context, id int64, model string) (agents.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.created {
		if m.created[i].ID == id {
			m.created[i].Model = model
			return m.created[i], nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}
