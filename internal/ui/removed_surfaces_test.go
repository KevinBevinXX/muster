package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE PRIVILEGE AND RUNBOOK SURFACES ARE GONE FROM EVERY DOCUMENT THE UI DRAWS.
//
// Privileges were superseded by connectors and runbooks were unused; both were
// removed whole. These assertions are about the RENDERED MARKUP, because that is
// what a user meets: a tab, a lazy mount, a form field or a disclosure that
// still names a removed route is a control that 404s when touched.
//
// removedMarkers are strings no rendered document may contain. Each is a route,
// a field name or a label that only the removed surfaces ever wrote.
// ---------------------------------------------------------------------------
var removedMarkers = []string{
	`/ui/runbooks`, `/ui/privileges`, `/ui/privilege-requests`, `/ui/agents/profiles`,
	`href="/runbooks"`, `href="/privileges"`, `/grants`,
	`name="grant_profile"`, `grant_profile`,
	`id="panel-runbooks"`, `id="panel-privileges"`, `id="privilege-requests"`,
	`Grant privileges`, `Privilege requests`, `Privilege profiles`,
	`>Privileges<`, `Edit Privileges`,
	`sse:privilege.created`,
}

func assertNoRemovedMarkers(t *testing.T, what, doc string) {
	t.Helper()
	if strings.TrimSpace(doc) == "" {
		t.Fatalf("%s rendered EMPTY; the absence checks below would prove nothing", what)
	}
	for _, m := range removedMarkers {
		if strings.Contains(doc, m) {
			t.Errorf("%s still renders %q — a removed privilege/runbook surface", what, m)
		}
	}
}

// TestNoTabForPrivilegesOrRunbooks: the tab registry (which also derives the
// shell routes) carries neither key.
func TestNoTabForPrivilegesOrRunbooks(t *testing.T) {
	keys := TabKeys()
	if len(keys) == 0 {
		t.Fatal("TabKeys is empty; the check below would pass over nothing")
	}
	for _, k := range keys {
		if k == "runbooks" || k == "privileges" {
			t.Errorf("tab %q is still registered", k)
		}
	}
}

// TestNoShellDocumentRendersARemovedSurface renders EVERY visible tab under
// every feature shape (the shell is one document; any tab's panel is in it).
func TestNoShellDocumentRendersARemovedSurface(t *testing.T) {
	for _, feat := range []Features{{}, {GitHub: true, Connectors: true}} {
		for _, tab := range visibleTabs(feat) {
			doc := renderString(t, Page(tab.Key, feat))
			// Positive control: the shell does mount the agents list, so an empty
			// or truncated render cannot pass the absence checks.
			if !strings.Contains(doc, `hx-get="/ui/agents"`) {
				t.Fatalf("Page(%q) does not mount /ui/agents; this render is not the shell", tab.Key)
			}
			assertNoRemovedMarkers(t, "Page("+tab.Key+")", doc)
		}
	}
}

// TestNoDispatchOrTaskFormRendersAPrivilegeField covers the four forms that
// carried the "Grant privileges" field or its disclosure: the FAB dispatch
// form, the task-scoped dispatch confirm, the new-task modal's Advanced
// disclosure and the task edit form.
func TestNoDispatchOrTaskFormRendersAPrivilegeField(t *testing.T) {
	cases := map[string]string{
		"DispatchModalBody":     renderString(t, DispatchModalBody()),
		"DispatchModalTaskBody": renderString(t, DispatchModalTaskBody(TaskDispatchView{NoteID: 4, Label: "x"})),
		"NotesModalBody":        renderString(t, NotesModalBody(nil, false)),
		"NotesEditModalBody":    renderString(t, NotesEditModalBody(NoteEditView{Note: notes.Note{ID: 4, Body: "x"}})),
	}
	for name, doc := range cases {
		// Positive control: each of these still draws its Advanced/Model controls,
		// so the absence below is the privilege field alone going, not the form.
		if !strings.Contains(doc, `name="model"`) {
			t.Fatalf("%s rendered no model field; it is not the form this test means", name)
		}
		assertNoRemovedMarkers(t, name, doc)
	}
}

// TestAStaleRunbookTagRendersAsAPlainTag: tasks written before the removal can
// still carry `runbook:<name>`. Nothing routes on it any more, so the card must
// render it as an ordinary descriptive chip — never as a routing chip promising
// a dispatch that no longer exists — and must render at all.
func TestAStaleRunbookTagRendersAsAPlainTag(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 1, Body: "x", Tags: []string{"runbook:deploy"}})
	if !strings.Contains(out, `data-tag="runbook:deploy"`) {
		t.Fatalf("the stale tag is not rendered at all:\n%s", out)
	}
	if !strings.Contains(out, `data-tag-kind="descriptive"`) || strings.Contains(out, `data-tag-kind="routing"`) {
		t.Errorf("a stale runbook: tag must render as a DESCRIPTIVE chip:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "dispatch via") {
		t.Errorf("the chip still promises a runbook dispatch:\n%s", out)
	}
	if notes.IsRoutingTag("runbook:deploy") {
		t.Error("notes.IsRoutingTag still treats runbook: as routing")
	}
}
