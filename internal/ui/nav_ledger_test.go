package ui

import (
	"sort"
	"strings"
	"testing"

	g "maragu.dev/gomponents"
)

// THE SIDE-NAV LEDGER: every full document this package emits either draws the
// navigation or is named here as BARE, with the reason.
//
// 🔴 IT IS KEYED BY THE SOURCE-DERIVED SET OF Doctype EMITTERS, SO IT FAILS
// WHEN THE SET GROWS OR SHRINKS. A new document with no decision is not
// "probably fine" — it is the exact shape that left /attention, /account and
// the mirrored views without a way back to the rest of the app. The api half
// (internal/api TestEveryAPIDocumentIsNavOrNamedBare) holds the string
// documents internal/api writes itself.

// navBare is a document that deliberately has no side nav, and why. Absent from
// this map (but in navLedgerNav) = draws the nav.
var navBare = map[string]string{
	"StepUpFormPage": "the no-script \"Confirm it's you\" fallback for a send: a one-question interstitial " +
		"whose only exits are its form and its Back link — a nav there invites leaving with the send unconfirmed",
	"SendQueueRetryConfirmPage": "the no-script \"send it again?\" confirm for a queued message: the same " +
		"one-question interstitial shape as StepUpFormPage",
}

// navLedgerNav is every document that draws the nav.
var navLedgerNav = []string{
	"PageWith", "ComposePage", "AgentDetailPage", "TaskDetailPage", "TaskNotFoundPage",
	"AgentTerminalPage", "MirrorViewPage", "NavDocument",
}

// TestEveryDocumentIsNavOrNamedBare is the ledger's two claims: the ledger names
// exactly the documents this package emits, and each RENDERS as its entry says.
func TestEveryDocumentIsNavOrNamedBare(t *testing.T) {
	derived := doctypeEmitters(t)
	if len(derived) < 2 {
		t.Fatalf("the source scan found %d Doctype emitters — it is not observing this package", len(derived))
	}
	var ledger []string
	ledger = append(ledger, navLedgerNav...)
	for fn := range navBare {
		ledger = append(ledger, fn)
	}
	sort.Strings(ledger)
	if strings.Join(ledger, ",") != strings.Join(derived, ",") {
		t.Fatalf("the side-nav ledger names %v; this package emits a Doctype from %v.\n"+
			"Every document must be in navLedgerNav (it draws the side nav) or in navBare with the reason it "+
			"does not — a document with neither is a page a signed-in person can reach and not leave.", ledger, derived)
	}

	// Each registered rendering — both states — against its ledger entry.
	// standaloneDocuments is itself pinned to the same derived set
	// (TestEveryStandaloneDocumentIsRegistered), so this covers every document.
	seen := map[string]bool{}
	for _, row := range standaloneDocuments {
		row := row
		_, bare := navBare[row.fn]
		seen[row.fn] = true
		for _, d := range documentsFrom([]documentRenderer{row}) {
			d := d
			t.Run(d.name, func(t *testing.T) {
				src := documentSource(t, d.node)
				roots, opens := strings.Count(src, `id="sidebar-root"`), strings.Count(src, `id="sidebar-open"`)
				if bare {
					if roots != 0 || opens != 0 {
						t.Errorf("%s is ledgered BARE (%s) but renders %d sidebar(s) and %d hamburger(s)",
							d.name, navBare[row.fn], roots, opens)
					}
					return
				}
				if roots != 1 || opens != 1 {
					t.Errorf("%s renders %d sidebar(s) and %d hamburger(s), want exactly one of each — the nav and "+
						"the control that opens it on a phone", d.name, roots, opens)
				}
				// The hamburger needs its script: appScript binds #sidebar-open.
				if n := strings.Count(src, "openBtn.addEventListener('click', function () { setOpen(true); })"); n != 1 {
					t.Errorf("%s carries the sidebar's open/close script %d times, want 1 — without it the "+
						"hamburger is a dead button", d.name, n)
				}
			})
		}
	}
	if len(seen) != len(derived) {
		t.Errorf("standaloneDocuments covers %d of the %d documents — a ledger entry with no rendering is unchecked", len(seen), len(derived))
	}
}

// frameablePages are the three documents the view swap opens in its frame.
// They are the same document framed or not — the frame's nav is the only one
// reachable while the frame is on screen (nav_layout.go).
func frameablePages() map[string]g.Node {
	return map[string]g.Node{
		"MirrorViewPage": MirrorViewPage(MirrorViewView{Host: "laptop", Session: "work", Interactive: true,
			Nav: NavChrome{Feat: featuresAllOn}}),
		"AgentTerminalPage": AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: "/agents/keen-wren",
			Nav: NavChrome{Feat: featuresAllOn}}),
		"AgentDetailPage": AgentDetailPage(AgentDetailView{ID: 7, Name: "keen-wren", DisplayName: "keen-wren", Status: "running",
			Kind: "claude-code"}, featuresAllOn),
	}
}

// streamsIn counts the server-sent-event streams a document opens: every htmx
// sse-connect, and every script that constructs an EventSource. Comments are
// stripped first (documentSource), so a sentence naming one is not one.
func streamsIn(t *testing.T, n g.Node) int {
	t.Helper()
	src := documentSource(t, n)
	return strings.Count(src, `sse-connect=`) + strings.Count(src, "new EventSource(")
}

// documentStreams is how many /events streams each document opens — the
// numbers it opened before the side nav, measured on main (74a7e059) with the
// same counter.
var documentStreams = map[string]int{
	"PageWith":                  1,
	"ComposePage":               1,
	"AgentDetailPage":           2,
	"TaskDetailPage":            1,
	"TaskNotFoundPage":          0,
	"AgentTerminalPage":         0,
	"MirrorViewPage":            0,
	"StepUpFormPage":            0,
	"SendQueueRetryConfirmPage": 0,
	"NavDocument":               0,
}

// TestTheNavAddsNoStream: the side nav opens no /events stream of its own — its
// badge refreshes on load and once a minute — so no document holds more streams
// than it did before the nav. A stream per tab is a connection per tab against
// the browser's per-host limit, and the mirrored views and terminals already
// hold their own socket.
func TestTheNavAddsNoStream(t *testing.T) {
	for _, row := range standaloneDocuments {
		want, ok := documentStreams[row.fn]
		if !ok {
			t.Errorf("%s has no entry in documentStreams — measure it", row.fn)
			continue
		}
		for _, d := range documentsFrom([]documentRenderer{row}) {
			if got := streamsIn(t, d.node); got != want {
				t.Errorf("%s opens %d /events stream(s), want %d", d.name, got, want)
			}
		}
	}
	// POSITIVE CONTROL: the counter sees a stream where one is.
	if n := streamsIn(t, Page(defaultTab, featuresAllOn)); n < 1 {
		t.Fatalf("the shell counts %d streams — the counter is blind", n)
	}
}

// TestASecretPageLoadsNoTelemetry: a server-authored page that can show a
// one-time secret loads no RUM script; one that cannot keeps the shell's.
func TestASecretPageLoadsNoTelemetry(t *testing.T) {
	saved := faroConfig
	t.Cleanup(func() { faroConfig = saved })
	SetFaroConfig(FaroSettings{URL: "https://faro.example.test/collect"})
	// POSITIVE CONTROL: with a collect URL set, the shell does load it.
	shell := renderString(t, Page(defaultTab, featuresAllOn))
	if !strings.Contains(shell, "faro.example.test") {
		t.Fatal("the shell does not load the configured RUM script — the needle below proves nothing")
	}
	for _, secret := range []bool{false, true} {
		doc := renderString(t, NavDocument(NavDocumentView{Title: "API tokens", Entry: NavAccount, Secret: secret, Main: "<h1>API tokens</h1>"}))
		if got := strings.Contains(doc, "faro.example.test"); got == secret {
			t.Errorf("secret=%v: the page loads the RUM script = %v", secret, got)
		}
	}
}

// TestFullHeightViewsUseAnOverlayNav: the mirrored view and the terminal keep
// the sidebar an overlay on a desktop (their width is the window's), with a
// hamburger that is not hidden there; every other nav document keeps the rail.
func TestFullHeightViewsUseAnOverlayNav(t *testing.T) {
	for name, page := range frameablePages() {
		src := documentSource(t, page)
		overlay := name != "AgentDetailPage"
		if got := strings.Contains(src, `data-overlay`); got != overlay {
			t.Errorf("%s: data-overlay = %v, want %v", name, got, overlay)
		}
		btn := elementByID(t, src, "sidebar-open")
		if hidden := strings.Contains(btn, "lg:hidden"); hidden == overlay {
			t.Errorf("%s: the hamburger's lg:hidden = %v — an overlay page needs it on a desktop too, a rail page does not", name, hidden)
		}
	}
	doc := documentSource(t, NavDocument(NavDocumentView{Title: "Account", Entry: NavAccount, Main: "<h1>Account</h1>"}))
	if strings.Contains(doc, "data-overlay") {
		t.Error("NavDocument draws an overlay sidebar; it is an ordinary page with the desktop rail")
	}
}

// TestNavDocumentMarksItsEntry: each server-authored page marks the entry it
// belongs to — the accent and aria-current on that link and on no other — and a
// page with no entry marks none.
func TestNavDocumentMarksItsEntry(t *testing.T) {
	feat := Features{GitHub: true, Connectors: true, Admin: true, Attention: true}
	ids := map[string]string{NavAttention: "sidebar-attention", NavAccount: "sidebar-account", NavAdmin: "sidebar-admin"}
	for _, entry := range []string{NavAttention, NavAccount, NavAdmin, ""} {
		src := documentSource(t, NavDocument(NavDocumentView{Title: "x", Entry: entry, Feat: feat, Main: "<h1>x</h1>"}))
		nav := sidebarPanel(t, src)
		wantCurrent := 0
		if entry != "" {
			wantCurrent = 1
		}
		if n := strings.Count(nav, `aria-current="page"`); n != wantCurrent {
			t.Errorf("entry %q: %d links carry aria-current, want %d", entry, n, wantCurrent)
		}
		if n := strings.Count(nav, "bg-accent/15"); n != wantCurrent {
			t.Errorf("entry %q: %d links carry the accent, want %d", entry, n, wantCurrent)
		}
		for e, id := range ids {
			link := openTagByID(t, nav, id)
			if marked := strings.Contains(link, `aria-current="page"`) && strings.Contains(link, "bg-accent/15"); marked != (e == entry) {
				t.Errorf("entry %q: #%s marked = %v", entry, id, marked)
			}
		}
	}
}

// elementByID returns the element whose id is id, from its open tag to the
// END of the document — enough to search its subtree in, since every caller
// asks about something inside it, with a positive control that it exists.
func elementByID(t *testing.T, src, id string) string {
	t.Helper()
	i := strings.Index(src, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element with id %q in the document", id)
	}
	return src[strings.LastIndex(src[:i], "<"):]
}

// sidebarPanel is the #sidebar <nav> from its open tag to its close. It holds
// no nested <nav> (sidebar's comment says why), so the first </nav> ends it —
// and bounding it matters: appScript's code names the accent class too.
func sidebarPanel(t *testing.T, src string) string {
	t.Helper()
	el := elementByID(t, src, "sidebar")
	end := strings.Index(el, "</nav>")
	if end < 0 {
		t.Fatal("the #sidebar <nav> is not closed")
	}
	return el[:end]
}

// openTagByID is the OPEN TAG alone of the element whose id is id.
func openTagByID(t *testing.T, src, id string) string {
	t.Helper()
	el := elementByID(t, src, id)
	return el[:strings.Index(el, ">")+1]
}
