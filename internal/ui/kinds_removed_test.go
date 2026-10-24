package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheKindPickerOffersNoHermesOption: the dispatch form built from every kind
// this build knows (agents.Kinds) offers gateway and claude-code — the positive
// control that the picker renders at all — and no hermes option.
//
// RED before the kind's removal (agents.Kinds listed hermes, and the picker
// rendered a `data-kind-option="hermes"` radio labelled "Hermes").
func TestTheKindPickerOffersNoHermesOption(t *testing.T) {
	html := renderString(t, DispatchModalBodyFor(KindChoices{Kinds: agents.Kinds}))
	for _, k := range []string{agents.KindGateway, agents.KindClaudeCode} {
		if !strings.Contains(html, `data-kind-option="`+k+`"`) {
			t.Fatalf("instrument check: the picker has no %s option, so its absence of hermes proves nothing:\n%s", k, html)
		}
	}
	if strings.Contains(html, `data-kind-option="hermes"`) || strings.Contains(html, `value="hermes"`) {
		t.Fatalf("the dispatch form offers the removed hermes kind:\n%s", html)
	}
}
