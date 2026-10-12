package ui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// landingShapes is every caller shape `/` is served to. The member row is the
// one the defect was first seen by; the others are here because the default is
// derived from the drawn tab set, and each of these draws a different one.
func landingShapes() []featureShape {
	return []featureShape{
		{"a member (connectors built)", Features{Connectors: true}},
		{"an admin (connectors built)", Features{Connectors: true, Admin: true}},
		{"an admin, every subsystem built", Features{GitHub: true, Connectors: true, Admin: true}},
		{"nothing optional built", Features{}},
	}
}

// shownPanelKeys is the set of tab keys whose tabpanel a document rendered
// WITHOUT the `hidden` class, read out of the markup. hiddenCount is how many
// it rendered with it, so a caller can tell "one shown" from "nothing matched".
func shownPanelKeys(t *testing.T, html string) (shown []string, hiddenCount int) {
	t.Helper()
	for _, tb := range musterTabs {
		re := regexp.MustCompile(`<[a-z]+[^>]*\bid="` + regexp.QuoteMeta(panelID(tb.Key)) + `"[^>]*>`)
		tag := re.FindString(html)
		if tag == "" {
			continue
		}
		class := regexp.MustCompile(`\bclass="([^"]*)"`).FindStringSubmatch(tag)
		if class == nil {
			t.Fatalf("#%s has no class attribute in its opening tag %q, so its hidden state cannot be read", panelID(tb.Key), tag)
		}
		isHidden := false
		for _, c := range strings.Fields(class[1]) {
			if c == "hidden" {
				isHidden = true
			}
		}
		if isHidden {
			hiddenCount++
		} else {
			shown = append(shown, tb.Key)
		}
	}
	return shown, hiddenCount
}

// TestTheScriptsDefaultTabIsTheTabTheShellShowsAtRoot pins the RELATIONSHIP that
// was broken: the tab the browser script falls back to on `/` is the tab the
// server rendered un-hidden for `/`.
//
// 🔴 THE TWO USED TO BE DECIDED IN TWO PLACES. Go clamped `/` to "tasks" and
// drew that panel; the script clamped `/` to 'requests' — a tab of the product
// this was extracted from — found no such panel, and hid every panel and FAB.
// Each half was correct on its own terms and every existing guard looked at
// one half. The script's value is now EMITTED by the server (DEFAULT_TAB), and
// this compares it against the markup rather than against the function that
// produced it.
func TestTheScriptsDefaultTabIsTheTabTheShellShowsAtRoot(t *testing.T) {
	for _, shape := range landingShapes() {
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			// "" is what handleIndex passes for `/` (the first path segment).
			root := renderString(t, Page("", shape.feat))
			shown, hidden := shownPanelKeys(t, root)
			if len(shown) != 1 {
				t.Fatalf("the shell for / shows %d panels (%v), want exactly one", len(shown), shown)
			}
			if hidden == 0 {
				t.Fatal("CONTROL: the shell for / rendered no hidden panel, so `one shown` says nothing about a default")
			}

			js := mustStripJSComments(t, jsSource(t, appScript(shape.feat)))
			raw := jsValueAfter(t, js, "var DEFAULT_TAB =")
			var def string
			if err := json.Unmarshal([]byte(raw), &def); err != nil {
				t.Fatalf("the script's DEFAULT_TAB is %q, not a JSON string (%v) — the script has no "+
					"server-decided default, so `/` falls back to whatever the script spells", raw, err)
			}
			if def != shown[0] {
				t.Errorf("the script's DEFAULT_TAB is %q but the shell for / shows #%s — the script would "+
					"re-toggle the panels on load and hide the one the server drew", def, panelID(shown[0]))
			}
			var tabs []string
			if err := json.Unmarshal([]byte(jsValueAfter(t, js, "var TABS =")), &tabs); err != nil {
				t.Fatalf("TABS is not a JSON array: %v", err)
			}
			inTabs := false
			for _, k := range tabs {
				if k == def {
					inTabs = true
				}
			}
			if !inTabs {
				t.Errorf("DEFAULT_TAB %q is not in TABS %v — show() would be asked for a tab with no panel", def, tabs)
			}
		})
	}
}

// TestTheScriptSpellsNoTabKeyTheShellDoesNotHave is the other half: the script
// must not carry a tab key of its own.
//
// ⚠ A SPELLED GUARD, AND LABELLED AS ONE. It can only see the names listed, so
// it is a tripwire for the upstream keys coming back in a port, not proof that
// no stray literal exists. The behavioural claim — `/` is never blank — is
// e2e/tests/landing.spec.ts, in a real browser.
func TestTheScriptSpellsNoTabKeyTheShellDoesNotHave(t *testing.T) {
	js := mustStripJSComments(t, jsSource(t, appScript(Features{GitHub: true, Connectors: true, Admin: true})))
	// CONTROL: the scan can see a quoted key at all.
	if !strings.Contains(js, `"tasks"`) {
		t.Fatal(`CONTROL: the stripped script does not contain "tasks", so a scan of it for quoted keys proves nothing`)
	}
	for _, gone := range []string{"requests", "attention", "suggestions", "tmux"} {
		for _, q := range []string{`'` + gone + `'`, `"` + gone + `"`} {
			if strings.Contains(js, q) {
				t.Errorf("the script spells the tab key %s, which is not a muster tab — a path or a saved "+
					"value resolved through it names a panel this shell never draws", q)
			}
		}
	}
}

// TestDefaultVisibleTabIsAlwaysADrawnTab pins defaultVisibleTab's contract
// directly, including the arm no shipping Features value reaches.
func TestDefaultVisibleTabIsAlwaysADrawnTab(t *testing.T) {
	for _, shape := range landingShapes() {
		got := defaultVisibleTab(shape.feat)
		if got != "tasks" {
			t.Errorf("%s: default tab = %q, want %q", shape.name, got, "tasks")
		}
		if normalizeVisibleTab("requests", shape.feat) != got || normalizeVisibleTab("", shape.feat) != got {
			t.Errorf("%s: an unknown or empty tab does not clamp to the default %q", shape.name, got)
		}
	}
	// A hidden tab clamps to the default, not to itself.
	if got := normalizeVisibleTab("connectors", Features{}); got != "tasks" {
		t.Errorf("connectors with no connector store clamps to %q, want tasks", got)
	}
	if got := normalizeVisibleTab("connectors", Features{Connectors: true}); got != "connectors" {
		t.Errorf("connectors with a connector store clamps to %q, want connectors", got)
	}
}
