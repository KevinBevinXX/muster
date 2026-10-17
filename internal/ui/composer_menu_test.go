package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE RENDERED VIEW COMPOSER'S TWO CONTROLS (composer_menu.go), as markup: the
// split Send/Queue button and the `/` and `:` menu's place. What they DO in a
// browser is e2e/tests/composer-menu.spec.ts.

// elem is the opening tag of the element with id in page.
func elem(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element %q", id)
	}
	start := strings.LastIndex(page[:i], "<")
	return page[start : i+strings.Index(page[i:], ">")+1]
}

func renderedView(t *testing.T, queue *ComposerQueue, interactive bool) string {
	t.Helper()
	return renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", Interactive: interactive,
		StepUp: stepUpFixture(), Queue: queue}))
}

// TestSendIsTheFaceAndQueueIsBehindTheCaret: the operator's "always Send" —
// with a queue the composer's first submit is Send, the caret beside it is a
// type=button (it cannot submit anything) that opens a popover menu, and Queue
// is the menu's one item, a submit to the queue route. Send comes BEFORE the
// queue in the form, so it is also the form's default button (an implicit
// submit is a Send, never a Queue).
func TestSendIsTheFaceAndQueueIsBehindTheCaret(t *testing.T) {
	page := renderedView(t, &ComposerQueue{Src: SendQueuePath + "?host=host-a"}, true)
	send, more, menu, queue := elem(t, page, "mv-send"), elem(t, page, "mv-send-more"), elem(t, page, "mv-send-menu"), elem(t, page, "mv-queue-send")
	if !strings.Contains(send, `type="submit"`) || strings.Contains(send, "formaction") || !strings.Contains(page, `aria-label="Send" class="`+composerSendSplitClass+`">Send</button>`) {
		t.Fatalf("Send: %s", send)
	}
	for _, a := range []string{`type="button"`, `popovertarget="mv-send-menu"`, `aria-haspopup="menu"`, `aria-expanded="false"`, `aria-controls="mv-send-menu"`, "min-h-[44px]", "min-w-[44px]"} {
		if !strings.Contains(more, a) {
			t.Errorf("the caret lacks %s: %s", a, more)
		}
	}
	if !strings.Contains(menu, `popover=""`) || !strings.Contains(menu, `role="menu"`) {
		t.Errorf("the menu: %s", menu)
	}
	if !strings.Contains(queue, `role="menuitem"`) || !strings.Contains(queue, `formaction="`+SendQueuePath+`"`) || !strings.Contains(queue, `type="submit"`) || !strings.Contains(queue, "min-h-[44px]") {
		t.Errorf("Queue: %s", queue)
	}
	if !strings.Contains(page, ">"+SendQueueLabel+"</button>") {
		t.Errorf("Queue does not read %q", SendQueueLabel)
	}
	// Queue is INSIDE the menu, and the menu after Send in the form.
	iSend, iMenu, iQueue := strings.Index(page, `id="mv-send"`), strings.Index(page, `id="mv-send-menu"`), strings.Index(page, `id="mv-queue-send"`)
	if !(iSend < iMenu && iMenu < iQueue) {
		t.Errorf("order Send %d, menu %d, Queue %d: want Send, then the menu holding Queue", iSend, iMenu, iQueue)
	}
	// Exactly one submit precedes Queue in the form: Send.
	form := page[strings.Index(page, `<form id="mv-form"`):strings.Index(page, `</form>`)]
	firstSubmit := regexp.MustCompile(`<button id="([^"]+)" type="submit"`).FindStringSubmatch(form)
	if firstSubmit == nil || firstSubmit[1] != "mv-send" {
		t.Errorf("the form's first submit is %v, want mv-send (the implicit-submit default)", firstSubmit)
	}
}

// TestNoQueueNoCaret: without a queue (no database) and on a read-only host,
// Send is the whole control, as before: no caret, no menu, no Queue.
func TestNoQueueNoCaret(t *testing.T) {
	for name, page := range map[string]string{
		"no queue":  renderedView(t, nil, true),
		"read-only": renderedView(t, &ComposerQueue{Src: SendQueuePath}, false),
	} {
		for _, id := range []string{"mv-send-more", "mv-send-menu", "mv-queue-send"} {
			if strings.Contains(page, `id="`+id+`"`) {
				t.Errorf("%s: carries %s", name, id)
			}
		}
		if !strings.Contains(elem(t, page, "mv-send"), composerSendClass) {
			t.Errorf("%s: Send is not the plain one: %s", name, elem(t, page, "mv-send"))
		}
	}
}

// TestTheMenuIsTheRenderedViewsAlone: the `/` and `:` menu is drawn on an
// enabled Rendered view composer, pointed at ITS window's catalog; not on a
// read-only host, and not on the Terminal view's composer (which pastes into
// whatever runs in the pane, not into Claude Code).
func TestTheMenuIsTheRenderedViewsAlone(t *testing.T) {
	page := renderedView(t, nil, true)
	box := elem(t, page, "mv-complete-box")
	if !strings.Contains(box, `data-src="`+MirrorCompletePath+`?host=host-a&amp;session=main&amp;window=%403"`) || !strings.Contains(box, `hidden`) || !strings.Contains(box, "bottom-full") {
		t.Fatalf("the menu box: %s", box)
	}
	if lb := elem(t, page, "mv-complete"); !strings.Contains(lb, `role="listbox"`) {
		t.Errorf("the list: %s", lb)
	}
	if !strings.Contains(page, "input.setAttribute('aria-autocomplete', 'list')") {
		t.Error("the menu script is not on the page")
	}
	// The composer is its positioning box, so the menu sits above IT.
	if c := elem(t, page, "mv-compose"); !strings.Contains(c, "fixed") {
		t.Errorf("the composer is not a containing block: %s", c)
	}
	for name, p := range map[string]string{
		"read-only": renderedView(t, nil, false),
		"terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
			Composer: &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Aria: "x"}})),
	} {
		if strings.Contains(p, "mv-complete") || strings.Contains(p, "aria-autocomplete") {
			t.Errorf("%s: carries the menu", name)
		}
	}
}

// TestBothDocumentsRankWithOneFunction: 🔴 a SEAM guard. The Rendered view is
// its own document, without appScript, so the menu embeds the ranker — and it
// must be the SAME text appScript carries, not a second copy that drifts from
// internal/searchrank's table (which e2e holds each document to).
func TestBothDocumentsRankWithOneFunction(t *testing.T) {
	view := renderedView(t, nil, true)
	app := renderString(t, appScript(Features{}))
	for name, page := range map[string]string{"rendered view": view, "app shell": app} {
		if n := strings.Count(page, rankJS); n != 1 {
			t.Errorf("%s carries rankJS %d times, want exactly 1", name, n)
		}
		if n := strings.Count(page, "window.musterRank = function"); n != 1 {
			t.Errorf("%s defines window.musterRank %d times, want 1 (a second definition is a second copy)", name, n)
		}
	}
}
