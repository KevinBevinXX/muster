package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func renderConnectorView(t *testing.T, f func(*bytes.Buffer) error) string {
	t.Helper()
	var b bytes.Buffer
	if err := f(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// TestTheCustomPlatformFormRenders: the enter-credential form for a connector
// that matches no preset (the custom path) renders the host, scheme and secret
// fields. It used to PANIC — g.If evaluated a preset field on a nil preset —
// which the browser saw as a 500 behind "Enter credential…".
func TestTheCustomPlatformFormRenders(t *testing.T) {
	out := renderConnectorView(t, func(b *bytes.Buffer) error {
		return RenderConnectorForm(b, ConnectorFormView{RequestID: 9, AgentName: "docs-sweeper", Name: "fakehub",
			Preset: "custom", Presets: []ConnectorPresetView{{Key: "github", Label: "GitHub", Hosts: []string{"api.github.com"}}}})
	})
	for _, want := range []string{`id="connector-hosts"`, `id="connector-scheme"`, `id="connector-secret"`, `type="password"`,
		`name="request_id" value="9"`, "Save and approve"} {
		if !strings.Contains(out, want) {
			t.Errorf("the custom form lacks %s", want)
		}
	}
}

func TestThePresetFormHidesTheCustomFields(t *testing.T) {
	out := renderConnectorView(t, func(b *bytes.Buffer) error {
		return RenderConnectorForm(b, ConnectorFormView{Name: "github", Preset: "github",
			Presets: []ConnectorPresetView{{Key: "github", Label: "GitHub", Help: "use a PAT", SecretLabel: "Personal access token",
				Hosts: []string{"api.github.com", "github.com"}}}})
	})
	if strings.Contains(out, `id="connector-hosts"`) || !strings.Contains(out, "Hosts: api.github.com, github.com") ||
		!strings.Contains(out, "Personal access token") || !strings.Contains(out, "Save connector") {
		t.Fatalf("the preset form is wrong:\n%s", out)
	}
}

func TestTheConnectorsPanelRendersEveryState(t *testing.T) {
	used := time.Now().Add(-time.Hour)
	out := renderConnectorView(t, func(b *bytes.Buffer) error {
		return RenderConnectors(b, ConnectorsContent{
			Notice:   "saved",
			Requests: []ConnectorRequestView{{ID: 1, AgentName: "a", Connector: "svc", Exists: true}, {ID: 2, AgentName: "b", Connector: "new-one"}},
			Connectors: []ConnectorView{{Name: "svc", Platform: "custom", Hosts: []string{"api.example.com"}, Scheme: "header",
				HeaderName: "X-Api-Key", LastUsedAt: &used,
				Grants: []ConnectorGrantView{{ID: 3, AgentName: "a", Status: "approved"}}}},
			Audit:      []ConnectorAuditView{{Kind: "proxy", AgentName: "a", Connector: "svc", Method: "GET", Host: "api.example.com", Path: "/x", Status: 200}},
			EditorForm: &ConnectorFormView{Name: "x", Preset: "custom", Error: "bad host"},
		})
	})
	for _, want := range []string{`data-approve="1"`, `data-enter-credential="2"`, `data-connector="svc"`, "data-rotated",
		`data-revoke="a"`, "GET api.example.com/x → 200", "bad host", `id="connector-form"`, "header X-Api-Key"} {
		if !strings.Contains(out, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	if !strings.Contains(renderConnectorView(t, func(b *bytes.Buffer) error { return RenderConnectors(b, ConnectorsContent{Unconfigured: "no key"}) }),
		"connectors-unconfigured") {
		t.Error("the unconfigured panel does not say so")
	}
}

func TestTheConnectorsTabIsDrawnOnlyWithAStore(t *testing.T) {
	has := func(f Features) bool {
		for _, tb := range visibleTabs(f) {
			if tb.Key == "connectors" {
				return true
			}
		}
		return false
	}
	if has(Features{}) || !has(Features{Connectors: true}) {
		t.Fatal("the Connectors tab is not gated on the store")
	}
}

// TestEveryConnectorControlNamesItsOwnTarget: inside the Connectors panel, every
// element that issues a request declares its own hx-target. The panel itself
// carries hx-target="#panel-connectors", which htmx INHERITS — the request list's
// background refresh once inherited it and replaced the whole panel with the
// (empty) list, measured in the browser suite.
func TestEveryConnectorControlNamesItsOwnTarget(t *testing.T) {
	out := renderConnectorView(t, func(b *bytes.Buffer) error {
		return RenderConnectors(b, ConnectorsContent{
			Requests:   []ConnectorRequestView{{ID: 1, AgentName: "a", Connector: "svc", Exists: true}, {ID: 2, AgentName: "b", Connector: "new-one"}},
			Connectors: []ConnectorView{{Name: "svc", Hosts: []string{"api.example.com"}, Scheme: "bearer", Grants: []ConnectorGrantView{{ID: 3, AgentName: "a", Status: "approved"}}}},
			EditorForm: &ConnectorFormView{Name: "x", Preset: "custom"},
		})
	})
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	issuing, missing := 0, []string{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
			}
			for _, verb := range []string{"hx-get", "hx-post", "hx-delete"} {
				if u, ok := attrs[verb]; ok {
					issuing++
					if _, ok := attrs["hx-target"]; !ok {
						missing = append(missing, n.Data+" "+verb+"="+u)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if issuing < 8 {
		t.Fatalf("only %d request-issuing elements found; the walk is not seeing the panel", issuing)
	}
	if len(missing) > 0 {
		t.Fatalf("controls that would inherit the panel's target:\n  %s", strings.Join(missing, "\n  "))
	}
}
