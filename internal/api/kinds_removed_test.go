package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
)

// staleHermesKinds is a kinds service that still lists the REMOVED hermes kind
// — what a deployment with a leftover MUSTER_AGENT_KINDS entry would look like
// if anything let it through. The server must refuse on the kind itself.
func staleHermesKinds() stubKinds {
	return stubKinds{enabled: []string{agents.KindGateway, agents.KindClaudeCode, "hermes"}, pick: "work"}
}

const removedKindRefusal = `agent kind "hermes" was removed from muster`

// TestAHermesDispatchIsRefusedAsRemoved: a dispatch, or a chief replacement,
// naming the removed hermes kind is refused with a 409 saying it was removed,
// before any row exists. A gateway dispatch on the same server is the control.
//
// RED before the kind's removal (the dispatch answered 200 and stored a hermes
// row; the chief replacement refused with "cannot be the chief yet").
func TestAHermesDispatchIsRefusedAsRemoved(t *testing.T) {
	s, h, store := kindsServer(t, staleHermesKinds())
	rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), removedKindRefusal) || len(store.rows()) != 0 {
		t.Fatalf("hermes dispatch: %d %q, %d row(s)", rec.Code, rec.Body, len(store.rows()))
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"gateway"}, "note_text": {"t"}}); rec.Code != http.StatusOK || len(store.rows()) != 1 {
		t.Fatalf("control: gateway dispatch: %d %s, %d row(s)", rec.Code, rec.Body, len(store.rows()))
	}
	code, out := s.replaceChief(context.Background(), 1, chiefReplaceRequest{Kind: "hermes", Confirm: "replace"})
	if msg, _ := out["error"].(string); code != http.StatusConflict || !strings.Contains(msg, removedKindRefusal) || len(store.rows()) != 1 {
		t.Fatalf("hermes chief: %d %v, %d row(s)", code, out, len(store.rows()))
	}
}

// TestALegacyHermesRowCanNotBeStartedOrGivenAModel: an agent row of the removed
// kind (migration 0038 still admits it) is refused at Start and at a model
// change with what it can still do, and nothing is written. A gateway row in
// the same store is the control for the model change.
//
// RED before the kind's removal (both answered 200 and the model was stored).
func TestALegacyHermesRowCanNotBeStartedOrGivenAModel(t *testing.T) {
	s, h, _ := kindsServer(t, stubKinds{})
	legacy := &legacyRowStore{preflightStore: &preflightStore{}}
	legacy.created = []agents.Agent{
		{ID: 7001, Name: "old-hermes", Kind: "hermes", Model: "openrouter/x/y", Status: agents.StatusStopped},
		{ID: 7002, Name: "plain", Kind: agents.KindGateway, Status: agents.StatusStopped},
	}
	s.ext.Agents = legacy

	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const want = "can only be stopped or destroyed"
	if rec := post("/agents/7001/start", url.Values{}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("start a legacy hermes row: %d %q", rec.Code, rec.Body)
	}
	if rec := post("/agents/7001/model", url.Values{"model": {"openrouter/a/b"}}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), want) || legacy.rows()[0].Model != "openrouter/x/y" {
		t.Fatalf("model change on a legacy hermes row: %d %q, stored %q", rec.Code, rec.Body, legacy.rows()[0].Model)
	}
	if rec := post("/agents/7002/model", url.Values{"model": {"openrouter/a/b"}}); rec.Code != http.StatusOK || legacy.rows()[1].Model != "openrouter/a/b" {
		t.Fatalf("control: model change on a gateway row: %d %q, stored %q", rec.Code, rec.Body, legacy.rows()[1].Model)
	}
}

// legacyRowStore is preflightStore with the row read and model write the start
// and model routes use.
type legacyRowStore struct {
	*preflightStore
}

func (m *legacyRowStore) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range m.rows() {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (m *legacyRowStore) SetModel(_ context.Context, id int64, model string) (agents.Agent, error) {
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
