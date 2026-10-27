package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAHermesDispatchIsRefusedAsAnUnknownKind: the hermes kind was removed, so
// a dispatch naming it is refused with a 409 naming the unknown kind before any
// row exists — even by a kinds service that still lists it. A gateway dispatch
// on the same server is the control.
//
// RED before the kind's removal (the dispatch answered 200 and stored a hermes
// row).
func TestAHermesDispatchIsRefusedAsAnUnknownKind(t *testing.T) {
	const want = `unknown agent kind "hermes"`
	s, h, store := kindsServer(t, stubKinds{enabled: []string{agents.KindGateway, agents.KindClaudeCode, "hermes"}, pick: "work"})
	rec := postDispatch(t, s, h, url.Values{"kind": {"hermes"}, "note_text": {"t"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) || len(store.rows()) != 0 {
		t.Fatalf("hermes dispatch: %d %q, %d row(s)", rec.Code, rec.Body, len(store.rows()))
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"gateway"}, "note_text": {"t"}}); rec.Code != http.StatusOK || len(store.rows()) != 1 {
		t.Fatalf("control: gateway dispatch: %d %s, %d row(s)", rec.Code, rec.Body, len(store.rows()))
	}
}
