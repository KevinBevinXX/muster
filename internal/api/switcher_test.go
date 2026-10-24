package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// TestBothSessionPagesDrawTheSwitcherFromTheServersLists: a dispatched agent's
// chat page and a mirrored window's Rendered view each draw the switcher, in
// the document itself (no load fills it), from the server's own lists: Recent
// is the viewer's recently opened entries (the SAME store the Agents tab's
// strip reads), newest first and without the page itself; All is every entry
// with the page drawn as current. The visit each page records is what the
// NEXT page's Recent shows.
func TestBothSessionPagesDrawTheSwitcherFromTheServersLists(t *testing.T) {
	s, ts := queueServer(t, queueMirror(), &memQueue{}, &turnGateway{})
	mem := &memRecents{}
	s.ext.Recents = mem
	get := func(path string) string {
		t.Helper()
		resp, page := mirrorGet(t, s, ts, path)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		return page
	}
	row := func(page, key string) string {
		t.Helper()
		at := strings.Index(page, `data-switcher-list="`+key+`"`)
		if at < 0 {
			t.Fatalf("no %s row", key)
		}
		r := page[at:]
		return r[:strings.Index(r, "</div>")]
	}

	// First the agent page: nothing opened before it, so Recent is empty.
	agent := get("/agents/keen-wren?session=41")
	if !strings.Contains(agent, `<nav id="`+ui.SwitcherID+`"`) {
		t.Fatal("the agent page draws no switcher")
	}
	if r := row(agent, ui.SwitcherRecent); !strings.Contains(r, "No other recent sessions") {
		t.Errorf("Recent on the first page opened is not empty: %s", r)
	}
	all := row(agent, ui.SwitcherAll)
	if !strings.Contains(all, `data-switcher-current="" aria-current="page" title="keen-wren (this session)"`) || !strings.Contains(all, `href="/agents/plain-otter"`) {
		t.Errorf("All on keen-wren's page: %s", all)
	}
	view := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath()
	if !strings.Contains(all, `href="`+strings.ReplaceAll(view, "&", "&amp;")+`"`) {
		t.Errorf("All does not link the mirrored window: %s", all)
	}

	// Then the window's Rendered view: keen-wren, opened just before, leads
	// Recent; the window itself is current in All and absent from Recent.
	page := get(view)
	if !strings.Contains(page, `<nav id="`+ui.SwitcherID+`"`) {
		t.Fatal("the Rendered view draws no switcher")
	}
	if r := row(page, ui.SwitcherRecent); !strings.Contains(r, `href="/agents/keen-wren"`) || strings.Contains(r, "data-switcher-current") || strings.Contains(r, "mirror") {
		t.Errorf("Recent on the window's view: %s", r)
	}
	if a := row(page, ui.SwitcherAll); !strings.Contains(a, `data-switcher-entry="mirror" data-switcher-current=""`) {
		t.Errorf("the window is not current in All: %s", a)
	}

	// Back on the agent page, the window is in Recent too, newest first.
	again := row(get("/agents/keen-wren?session=41"), ui.SwitcherRecent)
	if w, k := strings.Index(again, `data-switcher-entry="mirror"`), strings.Index(again, "keen-wren"); w < 0 || k >= 0 {
		t.Errorf("Recent on keen-wren's page after the view: %s", again)
	}
}

// TestTheAsksBlockCarriesTheHideButton: the asker block's content, as the
// server draws it, opens with the heading row that holds "Hide until next
// ask" (ui.AskerAsksHeading) — so the button arrives with every refresh too.
func TestTheAsksBlockCarriesTheHideButton(t *testing.T) {
	out := askerAsksHTML([]attentionItem{{Key: "ask-3", Kind: attentionKindAsk, Text: "go?"}}, "/agents/a", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(out, ui.AskerAsksHeading("1 question")) || !strings.Contains(out, `<li id="ask-3"`) {
		t.Errorf("the block's content:\n%s", out)
	}
	if askerAsksHTML(nil, "/agents/a", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) != "" {
		t.Error("an empty block draws a heading")
	}
}
