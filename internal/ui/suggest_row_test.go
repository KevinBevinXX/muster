package ui

import (
	"strings"
	"testing"
)

const suggestSrc = "/mirror/view/suggest?host=host-a&session=main&window=%403"

// TestTheChipRowIsDrawnHiddenAboveTheComposerAndLabelledAI: a Rendered view
// given the suggested replies' URL draws the row inside the pinned composer
// — hidden (no script: never shown), a group named as AI-suggested, out of
// flow above the composer (absolute bottom-full) at a fixed 44px, with the
// cutoff the server uses — and its script AFTER the form.
func TestTheChipRowIsDrawnHiddenAboveTheComposerAndLabelledAI(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", Interactive: true,
		StepUp: stepUpFixture(), Suggest: suggestSrc}))
	i := strings.Index(page, `<div id="mv-suggest"`)
	if i < 0 {
		t.Fatal("no chip row")
	}
	row := page[i : i+strings.Index(page[i:], ">")]
	for _, want := range []string{` hidden`, `role="group"`, `aria-label="AI-suggested replies"`, `data-cutoff-ms="8000"`,
		`data-event="/mirror/view/suggest/event"`, `absolute`, `bottom-full`, ` h-11 `} {
		if !strings.Contains(row, want) {
			t.Fatalf("the row lacks %s: %s", want, row)
		}
	}
	if !strings.Contains(page, `data-src="/mirror/view/suggest?host=host-a&amp;session=main&amp;window=%403"`) {
		t.Fatal("the row's source")
	}
	compose, form, script := strings.Index(page, `id="mv-compose"`), strings.Index(page, `id="mv-form"`), strings.Index(page, `window.musterSuggest = function`)
	if !(compose < i && i < form && form < script) {
		t.Fatalf("order: compose %d, row %d, form %d, script %d", compose, i, form, script)
	}
	if !strings.Contains(page, `>AI</span>`) || !strings.Contains(page, `aria-label="Hide suggestions for this turn"`) {
		t.Fatal("the AI mark or the ×")
	}
}

// TestNoChipRowWithoutASourceOnHermesOrReadOnly: no URL, a Hermes window, and
// a read-only host's (disabled) composer each draw no row and no script.
func TestNoChipRowWithoutASourceOnHermesOrReadOnly(t *testing.T) {
	for name, v := range map[string]MirrorViewView{
		"no source": {Host: "host-a", Session: "main", Interactive: true, StepUp: stepUpFixture()},
		"hermes":    {Host: "host-a", Session: "main", Interactive: true, Hermes: true, HermesSend: true, StepUp: stepUpFixture(), Suggest: suggestSrc},
		"read-only": {Host: "host-a", Session: "main", Interactive: false, Suggest: suggestSrc},
	} {
		page := renderString(t, MirrorViewPage(v))
		if strings.Contains(page, `id="mv-suggest"`) || strings.Contains(page, `window.musterSuggest = function`) {
			t.Fatalf("%s: a chip row was drawn", name)
		}
	}
}

// TestAPausedComposerSaysSoInOneLine: an auto-paused viewer's composer draws
// the paused line with the way back on (a 44px link to Account), and no row.
func TestAPausedComposerSaysSoInOneLine(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true, StepUp: stepUpFixture(), SuggestPaused: true}))
	if !strings.Contains(page, `id="mv-suggest-paused"`) || !strings.Contains(page, SuggestPausedText) ||
		!strings.Contains(page, `href="/account#suggest" class="inline-flex min-h-[44px]`) || strings.Contains(page, `id="mv-suggest"`) {
		t.Fatal("the paused line")
	}
	for name, v := range map[string]MirrorViewView{
		"not paused": {Host: "host-a", Session: "main", Interactive: true, StepUp: stepUpFixture()},
		"read-only":  {Host: "host-a", Session: "main", SuggestPaused: true},
		"hermes":     {Host: "host-a", Session: "main", Interactive: true, Hermes: true, HermesSend: true, StepUp: stepUpFixture(), SuggestPaused: true},
	} {
		if strings.Contains(renderString(t, MirrorViewPage(v)), `id="mv-suggest-paused"`) {
			t.Fatalf("%s: a paused line", name)
		}
	}
}

// TestATapFillsAndNeverSubmits pins the script's contract by its source: a
// chip is a type="button" built outside the form (never a submit), a tap
// fills through insertText on the box (Ctrl+Z restores the draft) below a
// draft on a new line, and nothing in it submits the form or posts the text.
// (The e2e suite drives the same in a browser: no send request is made.)
func TestATapFillsAndNeverSubmits(t *testing.T) {
	js := renderString(t, suggestRowScript())
	for _, want := range []string{`b.type = 'button'`, `execCommand('insertText', false, add)`, `(/\n$/.test(cur) ? '' : '\n') + text`,
		`chips.appendChild(b)`, `p.textContent = s.text`} {
		if !strings.Contains(js, want) {
			t.Fatalf("the script lacks %s", want)
		}
	}
	for _, never := range []string{`requestSubmit`, `.submit(`, `send.click`, `innerHTML`, `b.set('text'`} {
		if strings.Contains(js, never) {
			t.Fatalf("the script has %s", never)
		}
	}
}
