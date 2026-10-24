package ui

import (
	"regexp"
	"strings"
	"testing"
)

// TestTheAsksHeadingCarriesAHideThatSendsNothing: the heading row's "Hide until
// next ask" is a plain type="button" — not a submit, not inside any form, no
// htmx verb — so pressing it cannot reach the queue's answer or dismiss route.
// The chip that reopens the block follows the block, drawn hidden, and so does
// the script; the script makes no request of any kind.
func TestTheAsksHeadingCarriesAHideThatSendsNothing(t *testing.T) {
	head := AskerAsksHeading("2 questions")
	btn := regexp.MustCompile(`<button type="button" data-asker-asks-hide class="[^"]*">` + askerAsksHideLabel + `</button>`).FindString(head)
	if btn == "" {
		t.Fatalf("no hide button in the heading:\n%s", head)
	}
	for _, banned := range []string{"<form", "hx-", "formaction", `type="submit"`} {
		if strings.Contains(head, banned) {
			t.Errorf("the heading carries %q", banned)
		}
	}
	if !strings.Contains(head, `data-asker-asks-heading>Waiting on your answer: 2 questions</h2>`) {
		t.Errorf("the count is not in the heading:\n%s", head)
	}
	if !strings.Contains(AskerAsksHeading("<b>"), "&lt;b&gt;") {
		t.Error("the count is not escaped")
	}

	for _, viaHTMX := range []bool{true, false} {
		page := renderString(t, AskerAsksBlock(head+`<ul><li id="ask-5" data-attention-item>q</li></ul>`, "/attention/asker?agent=a", viaHTMX))
		block, chip, script := strings.Index(page, `id="`+AskerAsksID+`"`), strings.Index(page, `id="`+AskerAsksID+`-chip"`), strings.Index(page, "<script>")
		if block < 0 || chip < block || script < chip {
			t.Fatalf("viaHTMX=%v: block %d, chip %d, script %d — want block, then chip, then its script", viaHTMX, block, chip, script)
		}
		chipTag := page[chip:]
		chipTag = chipTag[:strings.Index(chipTag, ">")]
		if !strings.Contains(chipTag, `type="button"`) || !strings.Contains(chipTag, `hidden=""`) {
			t.Errorf("viaHTMX=%v: the chip is not a hidden plain button: %s", viaHTMX, chipTag)
		}
		js := page[script:]
		// A guard an htmx history snapshot carries (an attribute) leaves a
		// restored block unbound after Back.
		if strings.Contains(js, "-bound") {
			t.Errorf("viaHTMX=%v: the hide script guards its binding with an attribute", viaHTMX)
		}
		for _, banned := range []string{"fetch(", "XMLHttpRequest", "htmx.ajax", "sendBeacon", ".submit(", "requestSubmit"} {
			if strings.Contains(js, banned) {
				t.Errorf("viaHTMX=%v: the hide script makes a request (%s)", viaHTMX, banned)
			}
		}
		// Per ask id: the hidden set is the items' ids, and the block is hidden
		// only while every one of them is in it.
		for _, want := range []string{`'` + askerAsksHiddenKey + `'`, `querySelectorAll('[data-attention-item][id]')`, `if (d.indexOf(k[i]) < 0) hide = false;`,
			`new MutationObserver(apply).observe(block, { childList: true })`, `' waiting'`, `block.__musterHideBound`,
			`window.addEventListener('storage'`} {
			if !strings.Contains(js, want) {
				t.Errorf("viaHTMX=%v: the script lacks %q", viaHTMX, want)
			}
		}
	}
}
