package api

import (
	"encoding/json"
	"html"
	"regexp"
	"testing"
)

// TestTheAgentsListSendsTheFiltersOptions: GET /ui/agents carries, out of band,
// the filter's options derived from the cards it renders — the pod agents'
// "cluster" AND the mirror host "ws" in one host facet, so the seam between the
// two card sources is covered, not each alone. Red before: no facets at all.
func TestTheAgentsListSendsTheFiltersOptions(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, nil)
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	m := regexp.MustCompile(`<div id="agents-facets" hidden="" data-facets="([^"]*)" hx-swap-oob="true">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no out-of-band #agents-facets in the list:\n%s", body)
	}
	var f map[string][]struct{ V, L string }
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &f); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for _, o := range f["host"] {
		hosts[o.V] = true
	}
	if !hosts["cluster"] || !hosts["ws"] || len(hosts) != 2 {
		t.Errorf("host facet = %v, want exactly cluster and ws", f["host"])
	}
	types := map[string]bool{}
	for _, o := range f["type"] {
		types[o.V] = true
	}
	if !types["agent"] || !types["mirrored"] || len(types) != 2 {
		t.Errorf("type facet = %v, want agent and mirrored", f["type"])
	}
}
