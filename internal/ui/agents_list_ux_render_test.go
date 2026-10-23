package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// THE AGENTS LIST UX BATCH, AS RENDERED. Every test here reads markup the
// list and the agent page really emit, and uses only API that existed before
// the batch, so each one compiles — and FAILS — on the tree before it:
//
//   - one flat list, most recently active first, pins on top (operator: "sort
//     agents by most recently active (flat, not separated by host)");
//   - a recency dot on the operator's tmux idle-fade scale;
//   - a long title clamped behind a tappable "…";
//   - Mark complete goes back OUT to the Agents list;
//   - the preset filter chips.
//
// The predicates under them are unit-tested in agents_list_ux_unit_test.go.

var articleIDRe = regexp.MustCompile(`<article id="([^"]+)"`)

// articleIDs is a rendered list's card ids, in order.
func articleIDs(html string) []string {
	var out []string
	for _, m := range articleIDRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestTheListIsOneFlatListMostRecentFirst: agents and mirrored windows are ONE
// list ordered by last activity, newest first, pinned entries on top (also by
// activity), an entry with no known activity last, and ties in the order the
// entries arrive (agents first, each kind in its own order). Every activity
// time is distinct except the two deliberate ties.
//
// Red before: every agent was drawn ahead of every window — a two-day-idle
// agent above a window active a minute ago.
func TestTheListIsOneFlatListMostRecentFirst(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	agents := []AgentCardView{
		{ID: 1, Name: "a1", Status: "running", LastActiveAt: ago(10 * time.Minute)},
		{ID: 4, Name: "a4", Status: "running", LastActiveAt: ago(10 * time.Minute)}, // ties a1: a1 first (input order)
		{ID: 2, Name: "a2", Status: "running", LastActiveAt: ago(3 * time.Hour)},
		{ID: 3, Name: "a3", Status: "stopped", LastActiveAt: ago(48 * time.Hour), Pinned: true},
	}
	mirrors := []MirrorCardView{
		{Host: "h", Session: "m1", Window: "@1", Online: true, ActiveAt: ago(time.Minute)},
		{Host: "h", Session: "m5", Window: "@5", Online: true, ActiveAt: ago(time.Minute)}, // ties m1: m1 first
		{Host: "h", Session: "m2", Window: "@2", Online: true, ActiveAt: ago(time.Hour), Pinned: true},
		{Host: "h", Session: "m3", Window: "@3", Online: true},                                  // no activity: last
		{Host: "h", Session: "m4", Window: "@4", Online: true, ActiveAt: ago(10 * time.Minute)}, // ties a1/a4: agents first
	}
	name := map[string]string{}
	for _, m := range mirrors {
		name[mirrorCardID(m)] = m.Session
	}
	var got []string
	for _, id := range articleIDs(renderNode(t, AgentsCardsWithMirrors(agents, mirrors))) {
		if n, ok := name[id]; ok {
			got = append(got, n)
		} else {
			got = append(got, strings.TrimPrefix(id, "agent-"))
		}
	}
	want := "m2,3,m1,m5,1,4,m4,2,m3"
	if strings.Join(got, ",") != want {
		t.Fatalf("list order = %s, want %s: pinned first (m2 an hour ago before agent 3 two days ago), then "+
			"one list newest first, agents and windows interleaved, ties in arrival order, unknown activity last",
			strings.Join(got, ","), want)
	}
}

// openTag is the first opening tag in html that carries attr.
func openTag(t *testing.T, html, attr string) string {
	t.Helper()
	i := strings.Index(html, attr)
	if i < 0 {
		t.Fatalf("no element carries %s", attr)
	}
	start := i
	if !strings.HasPrefix(attr, "<") {
		start = strings.LastIndex(html[:i], "<")
	}
	end := strings.Index(html[i:], ">")
	return html[start : i+end+1]
}

// TestEachCardDrawsARecencyDotOnTheTmuxScale: a card's "active … ago" has a dot
// beside it tinted by how long ago that was, on the operator's tmux idle-fade
// scale (bucket 2, aqua, for 45 minutes; bucket 4, orange, for 3 hours), and
// the card carries the idle thresholds it is past. A window whose host did not
// say when it was active has neither. The card's BORDER is untouched.
func TestEachCardDrawsARecencyDotOnTheTmuxScale(t *testing.T) {
	now := time.Now()
	agent := renderNode(t, agentCard(AgentCardView{ID: 9, Name: "dot", Status: "running", LastActiveAt: now.Add(-45 * time.Minute)}))
	dot := openTag(t, agent, "data-recency=")
	for _, want := range []string{`data-recency="2"`, "bg-idle-2", `aria-hidden="true"`, "h-2 w-2"} {
		if !strings.Contains(dot, want) {
			t.Errorf("the agent card's dot lacks %s: %s", want, dot)
		}
	}
	if !strings.Contains(openTag(t, agent, `id="agent-9"`), `data-idle="15m"`) {
		t.Errorf("an agent idle 45 minutes is not marked idle >15m: %s", openTag(t, agent, `id="agent-9"`))
	}
	// The dot sits in the "active … ago" label, before its words.
	act := agent[strings.Index(agent, "data-card-active"):]
	if di, wi := strings.Index(act, "data-recency"), strings.Index(act, ">active<"); di < 0 || wi < 0 || di > wi {
		t.Errorf("the dot is not drawn inside the active label, before its words")
	}

	win := renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "w", Window: "@1", Online: true, ActiveAt: now.Add(-3 * time.Hour)}))
	if d := openTag(t, win, "data-recency="); !strings.Contains(d, `data-recency="4"`) || !strings.Contains(d, "bg-idle-4") {
		t.Errorf("a window active 3 hours ago is not drawn in bucket 4: %s", d)
	}
	if !strings.Contains(openTag(t, win, "data-mirror-card"), `data-idle="15m 1h"`) {
		t.Errorf("a window idle 3 hours is not marked idle >15m and >1h: %s", openTag(t, win, "data-mirror-card"))
	}
	for _, html := range []string{agent, win} {
		if card := openTag(t, html, "<article"); strings.Contains(card, "border-idle") || strings.Contains(card, "ring-idle") {
			t.Errorf("recency claimed the card's border, which is reserved: %s", card)
		}
	}

	unknown := renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "u", Window: "@2", Online: true}))
	if strings.Contains(unknown, "data-recency") || strings.Contains(unknown, "data-idle") {
		t.Errorf("a window with no known activity has a dot or an idle mark")
	}
}

// TestALongTitleIsClampedBehindATappableEllipsis: a name past 60 characters is
// drawn clamped and ends in a "…" BUTTON — aria-expanded, aria-controls naming
// a popover that holds the whole name, a 44px target — OUTSIDE every link, so
// a tap on it never opens the card. The clamped words are still a link to the
// card, with the whole name as its tooltip. A short name is drawn as before.
func TestALongTitleIsClampedBehindATappableEllipsis(t *testing.T) {
	long := "Refactor the importer so retries back off exponentially and the dead-letter queue drains on restart"
	if n := len([]rune(long)); n <= 60 {
		t.Fatalf("fixture: %d characters, want more than 60", n)
	}
	cases := map[string]string{
		"agent":  renderNode(t, agentCard(AgentCardView{ID: 3, Name: "imp", DisplayName: long, Status: "running"})),
		"window": renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "s", Window: "@1", Label: long, Online: true})),
	}
	for kind, html := range cases {
		btn := openTag(t, html, "data-title-more")
		for _, want := range []string{"<button", `type="button"`, `aria-expanded="false"`, `aria-label="Show the full title"`, "h-11", "min-w-[44px]"} {
			if !strings.Contains(btn, want) {
				t.Errorf("%s: the ellipsis button lacks %s: %s", kind, want, btn)
			}
		}
		pop := regexp.MustCompile(`aria-controls="([^"]+)"`).FindStringSubmatch(btn)
		if pop == nil {
			t.Fatalf("%s: the button controls nothing: %s", kind, btn)
		}
		panel := html[strings.Index(html, `id="`+pop[1]+`"`):]
		panel = panel[:strings.Index(panel, "</div>")]
		if !strings.Contains(panel, `popover="auto"`) || !strings.Contains(panel, long) {
			t.Errorf("%s: the popover the button opens does not hold the whole title: %s", kind, panel)
		}
		// Not inside any link: every <a opened before the button is closed.
		before := html[:strings.Index(html, "data-title-more")]
		if open, closed := strings.Count(before, "<a "), strings.Count(before, "</a>"); open != closed {
			t.Errorf("%s: the ellipsis button is inside a link (%d <a opened, %d closed before it)", kind, open, closed)
		}
		link := openTag(t, html, "data-card-title")
		if !strings.Contains(link, `title="`+long) {
			t.Errorf("%s: the clamped link has no full-title tooltip: %s", kind, link)
		}
		text := html[strings.Index(html, link)+len(link):]
		text = text[:strings.IndexAny(text, "<")]
		if want := string([]rune(long)[:60]); strings.TrimSpace(text) != strings.TrimSpace(want) {
			t.Errorf("%s: the clamped title reads %q, want the first 60 characters %q", kind, text, want)
		}
	}
	short := renderNode(t, agentCard(AgentCardView{ID: 4, Name: "short", DisplayName: "Fix the login", Status: "running"}))
	if strings.Contains(short, "data-title-more") || !strings.Contains(short, "Fix the login") {
		t.Errorf("a short name is clamped or missing")
	}
}

// TestMarkCompleteGoesBackToTheAgentsList: the agent page's Mark complete
// lands on the Agents list on both paths — the htmx path navigates to /agents
// after a successful PATCH (it used to open /tasks), and the form posts to
// /tasks/<id>/complete without JavaScript (it had no method or action).
func TestMarkCompleteGoesBackToTheAgentsList(t *testing.T) {
	id := int64(41)
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		page := documentSource(t, AgentDetailPage(AgentDetailView{ID: 2, Name: "done-agent", NoteID: &id, TaskStatus: "ready_for_review"}, feat))
		form := openTag(t, page, `hx-patch="/tasks/41/status"`)
		for _, want := range []string{`method="post"`, `action="/tasks/41/complete"`, `window.location.href=&#39;/agents&#39;`} {
			if !strings.Contains(form, want) {
				t.Errorf("%s: the Mark complete form lacks %s: %s", shape, want, form)
			}
		}
		if strings.Contains(form, "/tasks&#39;") || strings.Contains(form, "/tasks'") {
			t.Errorf("%s: Mark complete still navigates to /tasks: %s", shape, form)
		}
	})
}

// TestTheAgentsFilterHasThePresetChips: above the list, five toggle chips —
// Waiting on ask, Waiting on question, Idle >15m, Idle >1h, Idle >1d — each a
// 44px button with aria-pressed, explained by its title; the row is one 44px
// row (no shift when chips are pressed); the choice is remembered in
// localStorage, inside try/catch.
func TestTheAgentsFilterHasThePresetChips(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	at := strings.Index(page, `id="agents-chips"`)
	if at < 0 {
		t.Fatal("the Agents panel has no #agents-chips row")
	}
	row := page[at:]
	row = row[:strings.Index(row, "</div>")]
	labels := regexp.MustCompile(`data-chip="([a-z0-9-]+)"[^>]*>([^<]+)</button>`).FindAllStringSubmatch(row, -1)
	var got []string
	for _, l := range labels {
		got = append(got, l[1]+"="+l[2])
	}
	want := "ask=Waiting on ask,question=Waiting on question,idle-15m=Idle &gt;15m,idle-1h=Idle &gt;1h,idle-1d=Idle &gt;1d"
	if strings.Join(got, ",") != want {
		t.Fatalf("chips = %s, want %s", strings.Join(got, ","), want)
	}
	if n := strings.Count(row, `aria-pressed="false"`); n != 5 {
		t.Errorf("%d chips drawn unpressed, want 5", n)
	}
	if n := strings.Count(row, "min-h-[44px]"); n < 6 {
		t.Errorf("the row and its chips are not all 44px tall (%d)", n)
	}
	if n := strings.Count(row, ` title="`); n != 5 {
		t.Errorf("%d chips explain themselves, want 5", n)
	}
	script := scriptWith(t, page, "__musterAgentsFilter") + scriptWith(t, page, "__musterAgentsRules =")
	for _, want := range []string{"'muster:agents:chips'", "try { chips = R.chipsOf(localStorage.getItem(CHIPS_KEY)); }", "try { var v = JSON.parse(raw || '[]');",
		"try { if (cs.length) localStorage.setItem(CHIPS_KEY"} {
		if !strings.Contains(script, want) {
			t.Errorf("the filter script does not remember the chips safely: missing %s", want)
		}
	}
}

// TestALongFocusTitleIsClampedThroughTheSameRow: a focus title past 60
// characters heads the card clamped, followed by the SAME "…" control a long
// name gets (cardTitleRow), which opens the focus popover holding the whole
// title. The heading keeps the whole title as its accessible name.
func TestALongFocusTitleIsClampedThroughTheSameRow(t *testing.T) {
	long := "Untangling the reconnect backoff so a flapping tunnel stops resetting its retry window, then proving it with a soak test"
	html := renderNode(t, agentCard(AgentCardView{ID: 5, Name: "focused", Status: "running", Focus: &FocusView{Title: long}}))
	heading := openTag(t, html, "data-card-focus")
	if !strings.Contains(heading, `aria-label="Focus: `+long) {
		t.Errorf("the focus heading lost the whole title from its name: %s", heading)
	}
	text := html[strings.Index(html, heading)+len(heading):]
	text = text[strings.Index(text, ">")+1:]
	text = text[:strings.Index(text, "<")]
	if want := string([]rune(long)[:60]); strings.TrimSpace(text) != strings.TrimSpace(want) {
		t.Errorf("the focus heading reads %q, want the first 60 characters %q", text, want)
	}
	btn := openTag(t, html, "data-title-more")
	if !strings.Contains(btn, `aria-controls="agent-5-focus"`) || !strings.Contains(btn, `aria-expanded="false"`) || !strings.Contains(btn, "min-w-[44px]") {
		t.Errorf("the focus title's ellipsis is not the shared control opening the focus popover: %s", btn)
	}
	pop := html[strings.Index(html, `id="agent-5-focus"`):]
	if !strings.Contains(pop[:strings.Index(pop, "</div>")], long) {
		t.Errorf("the focus popover does not hold the whole title")
	}
	// A short focus title draws no ellipsis.
	short := renderNode(t, agentCard(AgentCardView{ID: 6, Name: "f2", Status: "running", Focus: &FocusView{Title: "Fix the login"}}))
	if strings.Contains(short, "data-title-more") {
		t.Errorf("a short focus title has an ellipsis")
	}
}

// TestAnAgentAwaitingApprovalIsWaitingOnAnAsk: a dispatched agent blocked at a
// permission prompt carries data-wait="ask" — what the "Waiting on ask" chip
// selects — and its status poll says what the card was drawn with, so the
// server can ask for a re-render when that changes.
func TestAnAgentAwaitingApprovalIsWaitingOnAnAsk(t *testing.T) {
	blocked := renderNode(t, agentCard(AgentCardView{ID: 7, Name: "blocked", Status: "running", AwaitingApproval: true}))
	if !strings.Contains(openTag(t, blocked, `id="agent-7"`), `data-wait="ask"`) {
		t.Errorf("an agent awaiting approval is not waiting on an ask: %s", openTag(t, blocked, `id="agent-7"`))
	}
	if !strings.Contains(blocked, `hx-get="/ui/agents/7/status?awaiting=1&amp;work=0.0"`) {
		t.Errorf("the status poll does not carry the card's awaiting state")
	}
	free := renderNode(t, agentCard(AgentCardView{ID: 8, Name: "free", Status: "running"}))
	if strings.Contains(openTag(t, free, `id="agent-8"`), "data-wait") || !strings.Contains(free, `hx-get="/ui/agents/8/status?awaiting=0&amp;work=0.0"`) {
		t.Errorf("a free agent is waiting on something, or its poll lacks awaiting=0")
	}
	if !strings.Contains(agentsListTrigger, AgentsAwaitingEvent+" from:body") {
		t.Errorf("the list does not re-render on %s", AgentsAwaitingEvent)
	}
}

// TestTheChipBarHasAClearControl: one Clear button at the END of the chip row,
// hidden until a chip is on (the script shows it), with no display utility
// that would beat its hidden attribute.
func TestTheChipBarHasAClearControl(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	at := strings.Index(page, `id="agents-chips"`)
	if at < 0 {
		t.Fatal("no chip row")
	}
	row := page[at:]
	row = row[:strings.Index(row, "</div>")]
	clr := strings.Index(row, `id="agents-chips-clear"`)
	if clr < 0 || clr < strings.LastIndex(row, "data-chip=") {
		t.Fatalf("no Clear control at the end of the chip row: %s", row)
	}
	tag := openTag(t, row, `id="agents-chips-clear"`)
	for _, want := range []string{` hidden=""`, `type="button"`, "min-h-[44px]"} {
		if !strings.Contains(tag, want) {
			t.Errorf("Clear lacks %s: %s", want, tag)
		}
	}
	for _, display := range []string{"inline-flex", " flex ", " block "} {
		if strings.Contains(tag, display) {
			t.Errorf("Clear carries %q, which beats its hidden attribute", display)
		}
	}
	script := scriptWith(t, page, "__musterAgentsFilter")
	if !strings.Contains(script, "clr.hidden = !cs.length") || !strings.Contains(script, "clearChips(); apply();") {
		t.Errorf("the script does not show Clear with the chips or clear them on a tap")
	}
}
