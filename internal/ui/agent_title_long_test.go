package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/textplain"
)

// Long titles on the agent page (/agents/{name}): a dispatched agent's name is
// its task's title, unbounded in length, and the header drew it WHOLE — a long
// one wrapped into a wall of lines over the chat. The header now draws at most
// cardTitleMax runes and a "…" that opens the whole title in a popover; the
// e2e spec agent-long-title.spec.ts checks the layout at 390 and 1280.

var tagRE = regexp.MustCompile(`<[^>]*>`)

// agentHeaderTitleText is the header title's VISIBLE text: the page's one
// <h1>, tags removed (the "…" control and its popover sit outside it).
func agentHeaderTitleText(t *testing.T, page string) string {
	t.Helper()
	h := agentH1(t, page)
	return html.UnescapeString(strings.TrimSpace(tagRE.ReplaceAllString(h, "")))
}

// agentH1 is the page's <h1> element, open tag to close tag.
func agentH1(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "<h1")
	if i < 0 {
		t.Fatalf("the page has no <h1>")
	}
	end := strings.Index(page[i:], "</h1>")
	if end < 0 {
		t.Fatalf("no </h1>")
	}
	return page[i : i+end+len("</h1>")]
}

// popoverPanel is the element with id=id, up to its first </div>.
func popoverPanel(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element id=%s", id)
	}
	p := page[i:]
	return p[:strings.Index(p, "</div>")]
}

func detailPage(t *testing.T, display string, taskLinked bool) string {
	v := AgentDetailView{Name: "agent-long", Status: "running", DisplayName: display}
	if taskLinked {
		id := int64(803)
		v.NoteID = &id
	}
	return renderString(t, AgentDetailPage(v, Features{}))
}

func TestTheAgentPageClampsALongTitleBehindAnEllipsis(t *testing.T) {
	cases := map[string]string{
		"600 chars, no spaces": strings.Repeat("x", 600),
		"long words + markdown": "#803 · **Refactor** the `importerRetryBackoffControllerConfiguration` so " +
			strings.Repeat("supercalifragilisticexpialidocious ", 12) + "_drains_ on restart",
	}
	for name, raw := range cases {
		full := textplain.Title(raw)
		if n := utf8.RuneCountInString(full); n <= cardTitleMax {
			t.Fatalf("%s fixture: %d runes after plaining, want more than %d", name, n, cardTitleMax)
		}
		want := strings.TrimRight(string([]rune(full)[:cardTitleMax]), " \t")
		for _, linked := range []bool{false, true} {
			page := detailPage(t, raw, linked)
			if got := agentHeaderTitleText(t, page); got != want {
				t.Errorf("%s (task-linked=%v): header title reads %q (%d runes), want the first %d runes %q", name, linked, got, utf8.RuneCountInString(got), cardTitleMax, want)
			}
			btn := openTag(t, page, "data-title-more")
			for _, w := range []string{"<button", `type="button"`, `aria-expanded="false"`, `aria-label="Show the full title"`,
				`popovertarget="` + agentTitlePopoverID + `"`, `aria-controls="` + agentTitlePopoverID + `"`, "h-11", "min-w-[44px]"} {
				if !strings.Contains(btn, w) {
					t.Errorf("%s (task-linked=%v): the ellipsis button lacks %s: %s", name, linked, w, btn)
				}
			}
			if regexp.MustCompile(`\shidden(\s|=|>)`).MatchString(btn) || !strings.Contains(openTag(t, page, "data-agent-title"), "data-title-clamped") {
				t.Errorf("%s (task-linked=%v): a clamped title's ellipsis starts hidden or the box is not marked clamped: %s", name, linked, btn)
			}
			panel := popoverPanel(t, page, agentTitlePopoverID)
			if !strings.Contains(panel, `popover="auto"`) || !strings.Contains(html.UnescapeString(panel), full) {
				t.Errorf("%s (task-linked=%v): the popover does not hold the whole title: %s", name, linked, panel)
			}
			// The "…" must not sit inside the task button (a button in a button).
			title := page[strings.Index(page, "data-agent-title"):strings.LastIndex(page[:strings.Index(page, "data-title-more")], "<")]
			if o, c := strings.Count(title, "<button"), strings.Count(title, "</button>"); o != c {
				t.Errorf("%s (task-linked=%v): the ellipsis is inside a button (%d opened, %d closed)", name, linked, o, c)
			}
			if !strings.Contains(page, "<title>"+html.EscapeString(want)+"… · muster</title>") {
				t.Errorf("%s (task-linked=%v): the document <title> is not clamped to the same budget", name, linked)
			}
			// The popover is placed (and its trigger's aria-expanded kept in step)
			// by floatingMenuScript; without it the panel stays `invisible`.
			if !strings.Contains(page, "__musterFloatingMenu") {
				t.Errorf("%s (task-linked=%v): the page does not carry floatingMenuScript, so the popover is never shown", name, linked)
			}
		}
	}
}

// The cut is on a rune boundary: a title of multi-byte runes (3- and 4-byte
// UTF-8) is cut to exactly cardTitleMax whole runes, never mid-sequence.
func TestTheAgentPageTitleClampIsRuneSafe(t *testing.T) {
	raw := strings.Repeat("界🚀é", 200) // 600 runes, 1800 bytes+
	page := detailPage(t, raw, false)
	got := agentHeaderTitleText(t, page)
	if !utf8.ValidString(got) {
		t.Fatalf("the clamped title is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != cardTitleMax || !strings.HasPrefix(raw, got) {
		t.Errorf("the clamped title is %d runes (%q), want the first %d runes of the title", n, got, cardTitleMax)
	}
	if !utf8.ValidString(page) {
		t.Errorf("the rendered page is not valid UTF-8")
	}
}

// A title within the budget is drawn whole. Its "…" and popover are still
// rendered — a phone's header clips a short title too, and the fit script
// shows the "…" then — but the button starts HIDDEN, and the box is not marked
// clamped (which would force the button on).
func TestTheAgentPageDrawsAShortTitleWholeWithAHiddenEllipsis(t *testing.T) {
	short := "#12 · Fix the login"
	for _, linked := range []bool{false, true} {
		page := detailPage(t, short, linked)
		if got := agentHeaderTitleText(t, page); got != short {
			t.Errorf("task-linked=%v: header reads %q, want %q", linked, got, short)
		}
		if btn := openTag(t, page, "data-title-more"); !regexp.MustCompile(`\shidden(\s|=|>)`).MatchString(btn) {
			t.Errorf("task-linked=%v: a short title's ellipsis is not hidden at first paint: %s", linked, btn)
		}
		if strings.Contains(openTag(t, page, "data-agent-title"), "data-title-clamped") {
			t.Errorf("task-linked=%v: a short title is marked clamped", linked)
		}
		if panel := popoverPanel(t, page, agentTitlePopoverID); !strings.Contains(html.UnescapeString(panel), short) {
			t.Errorf("task-linked=%v: the popover does not hold the short title: %s", linked, panel)
		}
		if !strings.Contains(page, "<title>"+html.EscapeString(short)+" · muster</title>") {
			t.Errorf("task-linked=%v: a short title's <title> is not the whole title", linked)
		}
		if !strings.Contains(page, "__musterAgentTitleFit") {
			t.Errorf("task-linked=%v: the page does not carry agentTitleFitScript, so a clipped short title never shows its \"…\"", linked)
		}
	}
}

// The popover's full title is escaped text: markup-like characters in a title
// that survive plaining are drawn as characters, never parsed as HTML.
func TestTheAgentPageFullTitlePopoverIsEscaped(t *testing.T) {
	raw := strings.Repeat(`if 1 < 2 && 3 > 2 then "q" & 'z' `, 6) + `<img src=x onerror=alert(1)>`
	full := textplain.Title(raw)
	if !strings.Contains(full, "<") || utf8.RuneCountInString(full) <= cardTitleMax {
		t.Fatalf("fixture: the plained title %q must keep a '<' and run past the budget", full)
	}
	page := detailPage(t, raw, true)
	panel := popoverPanel(t, page, agentTitlePopoverID)
	if strings.Contains(panel, "1 < 2") || strings.Contains(panel, "<img") {
		t.Errorf("the popover carries unescaped markup: %s", panel)
	}
	if !strings.Contains(panel, "1 &lt; 2") || html.UnescapeString(tagRE.ReplaceAllString(panel[strings.Index(panel, ">")+1:], "")) != full {
		t.Errorf("the popover does not hold the whole title as escaped text: %s", panel)
	}
}

// The header title's box is one line that can only shrink: no break-all wall,
// and min-w-0 on the box and the heading so a flex row cannot be widened.
func TestTheAgentPageTitleBoxIsOneShrinkableLine(t *testing.T) {
	for _, linked := range []bool{false, true} {
		page := detailPage(t, strings.Repeat("y", 600), linked)
		h1 := agentH1(t, page)
		if !strings.Contains(openTag(t, h1, "<h1"), "min-w-0") || strings.Contains(h1, "break-all") {
			t.Errorf("task-linked=%v: the heading is not a shrinkable single line: %s", linked, openTag(t, h1, "<h1"))
		}
		if !strings.Contains(h1, "whitespace-nowrap") || !strings.Contains(h1, "overflow-hidden") {
			t.Errorf("task-linked=%v: the title text is not held to one line (nowrap + overflow hidden)", linked)
		}
		// Every flex box around the heading in the header row must be able to
		// shrink below its content, or the min-content of the words widens it.
		if i := strings.Index(page, "data-agent-title"); i < 0 || !strings.Contains(openTag(t, page, "data-agent-title"), "min-w-0") {
			t.Errorf("task-linked=%v: the title's flex box is missing or not min-w-0", linked)
		}
	}
}
