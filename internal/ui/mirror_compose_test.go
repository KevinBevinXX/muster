package ui

import (
	"strings"
	"testing"
)

func stepUpFixture() *StepUpView {
	return &StepUpView{ConfirmPath: "/auth/confirm", PasskeyPath: "/auth/confirm/passkey", FreshFor: 0, RememberKey: "mirror:host-a", Window: "12 hours"}
}

// script is the body of the page's inline <script> that contains marker.
func scriptWith(t *testing.T, page, marker string) string {
	t.Helper()
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no script contains %q", marker)
	}
	start := strings.LastIndex(page[:i], "<script>")
	return page[start : i+strings.Index(page[i:], "</script>")]
}

// TestBothMirrorViewsShareOneComposerAndOneConfirm: 🔴 a SEAM guard. The
// Terminal view and the Rendered view are two documents; the composer and the
// "Confirm it's you" dialog on them must be the SAME code, not two copies that
// drift (each used to carry its own confirm handler). So the scripts are
// compared byte for byte across the two pages, each page carries each exactly
// once, and the two forms differ in their route and nothing structural.
func TestBothMirrorViewsShareOneComposerAndOneConfirm(t *testing.T) {
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", WSPath: "/mirror/terminal/ws?host=host-a&session=main",
		StepUp:   stepUpFixture(),
		Composer: &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Placeholder: "Message…", Aria: "Send a message into this session"}}))
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true, StepUp: stepUpFixture()}))

	for _, marker := range []string{"window.musterStepUp = (function", "stepUp.ask('', post)"} {
		if a, b := strings.Count(term, marker), strings.Count(view, marker); a != 1 || b != 1 {
			t.Fatalf("%q appears %d time(s) on the Terminal page and %d on the Rendered view; want once each", marker, a, b)
		}
		if scriptWith(t, term, marker) != scriptWith(t, view, marker) {
			t.Errorf("the script containing %q differs between the two views — it is supposed to be one piece of code", marker)
		}
	}
	// One submit handler for the dialog, on each page — not one per consumer.
	for name, page := range map[string]string{"terminal": term, "rendered": view} {
		if n := strings.Count(page, "getElementById('term-confirm-form')"); n != 1 {
			t.Errorf("%s: the confirm form is wired %d times, want 1", name, n)
		}
		if n := strings.Count(page, `<dialog id="term-confirm"`); n != 1 {
			t.Errorf("%s: %d confirm dialogs, want 1", name, n)
		}
		if n := strings.Count(page, `id="mv-form"`); n != 1 {
			t.Errorf("%s: %d composer forms, want 1", name, n)
		}
	}
	// The forms: the same element with a different route.
	formOf := func(page string) string {
		i := strings.Index(page, `<form id="mv-form"`)
		return page[i : i+strings.Index(page[i:], ">")]
	}
	tf, vf := formOf(term), formOf(view)
	if !strings.Contains(tf, `method="post" action="/mirror/terminal/send?`) || !strings.Contains(vf, `method="post" action="/mirror/view/send?`) {
		t.Fatalf("the forms do not post to their own routes:\n%s\n%s", tf, vf)
	}
	if strings.Replace(tf, "/mirror/terminal/send", "/mirror/view/send", 1) != vf {
		t.Errorf("the two composer forms differ in more than their route:\n%s\n%s", tf, vf)
	}
}

// TestTheComposerIsPhoneShaped: what the browser suite measures at 390×844,
// pinned where a class rename would lose it — a 16px input (iOS zooms the page
// on focusing a smaller one), 44px targets, and the safe-area inset on the form
// that is the bottom of the page. With a composer under it, the quick-key bar
// no longer carries that inset (there would be a gap between the two).
func TestTheComposerIsPhoneShaped(t *testing.T) {
	c := &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Placeholder: "Message…", Aria: "x"}
	with := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(), Composer: c}))
	without := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture()}))

	tag := func(page, id string) string {
		i := strings.Index(page, `id="`+id+`"`)
		if i < 0 {
			return ""
		}
		start := strings.LastIndex(page[:i], "<")
		return page[start : i+strings.Index(page[i:], ">")]
	}
	input := tag(with, "mv-input")
	for _, want := range []string{" text-base ", "min-h-[44px]"} {
		if !strings.Contains(input, want) {
			t.Errorf("the input lacks %q: %s", want, input)
		}
	}
	if strings.Contains(input, "text-sm") || strings.Contains(input, "text-xs") {
		t.Errorf("the input is smaller than 16px: %s", input)
	}
	if !strings.Contains(tag(with, "mv-send"), "min-h-[44px]") {
		t.Error("Send is not a 44px target")
	}
	if !strings.Contains(tag(with, "mv-form"), "env(safe-area-inset-bottom)") {
		t.Error("the composer does not carry the safe-area inset")
	}
	if strings.Contains(tag(with, "term-keys"), "safe-area-inset-bottom") {
		t.Error("the quick-key bar still carries the safe-area inset with a composer under it")
	}
	if !strings.Contains(tag(without, "term-keys"), "safe-area-inset-bottom") {
		t.Error("control: with no composer the quick-key bar lost its safe-area inset")
	}
	if tag(without, "mv-form") != "" {
		t.Error("control: a terminal with no Composer drew one")
	}
	// The composer is BELOW the keys: the last thing in the column.
	if strings.Index(with, `id="mv-form"`) < strings.Index(with, `id="term-keys"`) {
		t.Error("the composer is drawn above the quick-key bar")
	}
}

// TestADisabledComposerSaysWhy: a read-only host's rendered view (and an older
// muster-mirror's Terminal view) draws the composer disabled with its reason on
// screen, not merely greyed out.
func TestADisabledComposerSaysWhy(t *testing.T) {
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: false}))
	if !strings.Contains(view, `<p id="mv-readonly"`) || !strings.Contains(view, MirrorReadOnlyNote) {
		t.Fatal("a read-only host's rendered view does not say why nothing can be sent")
	}
	if strings.Contains(view, `<dialog id="term-confirm"`) || strings.Contains(view, "window.musterStepUp = (function") {
		t.Error("a read-only host's rendered view carries a confirm dialog it can never need")
	}
}
