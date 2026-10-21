package ui

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const swRegisterCall = "navigator.serviceWorker.register("

// TestEveryDocumentCarriesTheManifestAndTheWorker: every standalone document
// links the manifest WITH credentials, registers the worker exactly once, and
// carries the (hidden) update toast — an update must be offered wherever the
// operator is, not only on the shell.
func TestEveryDocumentCarriesTheManifestAndTheWorker(t *testing.T) {
	docs := allDocuments()
	if len(docs) < 5 {
		t.Fatalf("only %d documents to sweep; the registry is not being read", len(docs))
	}
	for _, d := range docs {
		h := renderString(t, d.node)
		if n := strings.Count(h, `<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">`); n != 1 {
			t.Errorf("%s: %d manifest links with use-credentials, want 1", d.name, n)
		}
		if n := strings.Count(h, swRegisterCall); n != 1 {
			t.Errorf("%s: the worker is registered %d times, want exactly 1", d.name, n)
		}
		toast := between(h, `<div id="sw-update-toast"`, ">")
		if toast == "" || !strings.Contains(toast, " hidden") {
			t.Errorf("%s: no hidden update toast: %q", d.name, toast)
		}
	}
}

// TestTheUpdateToastSitsAtTheTop: the update pill is anchored to the top-RIGHT
// corner (under the safe-area insets), never to the bottom, where the phone's
// composer, Send, the FAB and the chat button live; and it can be dismissed.
// (Where it lands on each page, clear of every control, is
// e2e/tests/update.spec.ts.)
func TestTheUpdateToastSitsAtTheTop(t *testing.T) {
	h := renderString(t, Page("tasks", Features{}))
	toast := between(h, `<div id="sw-update-toast"`, ">")
	if !strings.Contains(toast, "top-[calc(env(safe-area-inset-top)+0.5rem)]") {
		t.Errorf("the update toast is not anchored under the top safe-area inset: %q", toast)
	}
	if !strings.Contains(toast, "right-[calc(env(safe-area-inset-right)+0.75rem)]") {
		t.Errorf("the update pill is not anchored to the right safe-area inset: %q", toast)
	}
	if regexp.MustCompile(`\b(bottom-|inset-x-)`).MatchString(toast) {
		t.Errorf("the update toast is a bottom-anchored or full-width bar, not a corner pill: %q", toast)
	}
	if !strings.Contains(h, `data-sw-dismiss=""`) {
		t.Error("the update toast has no dismiss control")
	}
}

// TestPWAScriptRegistersBeforeThePushScript: pushScript consumes the
// registration pwaScript publishes, so the order on the shell is load-bearing.
func TestPWAScriptRegistersBeforeThePushScript(t *testing.T) {
	h := renderString(t, Page("tasks", Features{Push: true}))
	reg := strings.Index(h, "window.musterSWReady = registered;")
	use := strings.Index(h, "var registered = window.musterSWReady;")
	if reg < 0 || use < 0 {
		t.Fatalf("missing publisher (%d) or consumer (%d) of window.musterSWReady", reg, use)
	}
	if reg > use {
		t.Error("pushScript renders before pwaScript: it would read window.musterSWReady before it exists")
	}
	if strings.Count(h, swRegisterCall) != 1 {
		t.Errorf("the shell registers the worker %d times, want once", strings.Count(h, swRegisterCall))
	}
}

// TestTheInstallButtonStartsHidden: it appears only when the browser offers an
// install (beforeinstallprompt), never by default.
func TestTheInstallButtonStartsHidden(t *testing.T) {
	h := renderString(t, Page("tasks", Features{}))
	tags := regexp.MustCompile(`<button[^>]*data-install-app[^>]*>`).FindAllString(h, -1)
	if len(tags) != 1 {
		t.Fatalf("want exactly one install button, found %d", len(tags))
	}
	if !strings.Contains(tags[0], " hidden") {
		t.Errorf("the install button is not rendered hidden: %s", tags[0])
	}
}

// TestComposePageOpensTheComposerWithInertText: the composer URL carries the
// body query-escaped, the page never carries it as markup, and nothing else of
// the shell changes.
func TestComposePageOpensTheComposerWithInertText(t *testing.T) {
	body := "Shared <img src=x onerror=alert(1)> & \"quoted\"\nhttps://example.com/a?x=1&y=2"
	h := renderString(t, ComposePage(Features{}, body))
	m := regexp.MustCompile(`data-compose-url="([^"]*)"`).FindStringSubmatch(h)
	if m == nil {
		t.Fatal("ComposePage does not open the composer")
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil || u.Path != "/ui/tasks/new" || u.Query().Get("body") != body {
		t.Errorf("compose URL %q does not round-trip the body", m[1])
	}
	if strings.Contains(h, "<img src=x") {
		t.Error("the composed text reached the page as markup")
	}
	if n := strings.Count(h, `data-compose-url=`); n != 1 {
		t.Errorf("%d composer openers, want 1", n)
	}
	if strings.Contains(renderString(t, Page("tasks", Features{})), "data-compose-url") {
		t.Error("the plain shell opens the composer")
	}
	// The sheet body itself escapes the prefill.
	sheet := renderString(t, NotesModalBodyWith(nil, false, body))
	if !strings.Contains(sheet, "Shared &lt;img src=x onerror=alert(1)&gt; &amp; &#34;quoted&#34;\nhttps://example.com/a?x=1&amp;y=2</textarea>") {
		t.Errorf("the composer textarea does not carry the escaped prefill:\n%s", between(sheet, "<textarea", "</textarea>"))
	}
}

func TestClampComposeBody(t *testing.T) {
	if got := ClampComposeBody("short"); got != "short" {
		t.Errorf("ClampComposeBody(short) = %q", got)
	}
	long := strings.Repeat("ab€", 4000) // 20,000 bytes; € is three
	got := ClampComposeBody(long)
	if len(got) > composeMaxBytes || len(got) < composeMaxBytes-3 || !strings.HasPrefix(long, got) || strings.ToValidUTF8(got, "?") != got {
		t.Errorf("clamp: len %d, prefix %v, valid %v", len(got), strings.HasPrefix(long, got), strings.ToValidUTF8(got, "?") == got)
	}
}

// TestPushAffordancesFollowFeaturesPush: with no VAPID keys (Push false) the
// shell draws no Enable button, no "on" pill and no subscribe script — tapping
// Enable would ask for the OS permission and then fail on a 503 (the live
// symptom before muster served push: "Could not load the notification key").
// With keys, all three are drawn. The two renders differ in nothing else that
// this test reads, so each absence is a fact about the flag.
func TestPushAffordancesFollowFeaturesPush(t *testing.T) {
	markers := []string{`id="enable-push"`, `id="push-on"`, "var registered = window.musterSWReady;", "/api/push/vapid-public-key"}
	off := renderString(t, Page("tasks", Features{}))
	on := renderString(t, Page("tasks", Features{Push: true}))
	for _, m := range markers {
		if strings.Contains(off, m) {
			t.Errorf("Push=false still renders %q", m)
		}
		if !strings.Contains(on, m) {
			t.Errorf("Push=true does not render %q (the positive control)", m)
		}
	}
}
