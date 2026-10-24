package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// The pinned row (pinned_chips.go): the operator ask "Pinned agents should
// show only the 'pretty Tmux' label (for example mango, vapor, wheat) not the
// full agent card". Every test here renders the LIST (AgentsCardsWithMirrors),
// so each one compiles against the code before the chips existed and fails
// there on what it asserts, not on a missing symbol. The codenames are
// synthetic.

// pinParse parses a rendered list fragment.
func pinParse(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<div id=root>" + s + "</div>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func pinAttr(n *html.Node, k string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val, true
		}
	}
	return "", false
}

// pinFind is every element under n (n included) for which ok holds, in
// document order.
func pinFind(n *html.Node, ok func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && ok(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func pinHas(k string) func(*html.Node) bool {
	return func(n *html.Node) bool { _, ok := pinAttr(n, k); return ok }
}

func pinText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// pinArticles is the list's top-level <article> children by id.
func pinArticles(t *testing.T, s string) map[string]*html.Node {
	t.Helper()
	out := map[string]*html.Node{}
	for _, a := range pinFind(pinParse(t, s), func(n *html.Node) bool { return n.Data == "article" }) {
		id, _ := pinAttr(a, "id")
		out[id] = a
	}
	return out
}

// pinnedLabels is the rendered chips' labels, in list order, read from the
// markup: the label span of every pinned chip.
func pinnedLabels(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, a := range pinFind(pinParse(t, s), pinHas("data-pinned-chip")) {
		ls := pinFind(a, pinHas("data-pinned-chip-label"))
		if len(ls) != 1 {
			t.Fatalf("a chip has %d label spans, want 1", len(ls))
		}
		out = append(out, pinText(ls[0]))
	}
	return out
}

// pinWin is a pinned window with a codename label, most recently active first
// by i (so the list's order is the fixtures' order).
func pinWin(i int, host, session, window, label string) MirrorCardView {
	return MirrorCardView{Host: host, Session: session, Window: window, Title: session + ":" + window[1:],
		Label: label, Online: true, Pinned: true, ActiveAt: time.Now().Add(-time.Duration(i+1) * time.Minute)}
}

// TestAPinnedWindowIsALabelOnlyChip: a pinned window carrying everything a
// card draws — a focus title, a repo, a kind, a host, work, a footer — is drawn
// as a chip whose WHOLE text is its codename, with no card slot, no host chip,
// no focus title and no footer; and it is not ALSO drawn as a card. Red before
// the chips: the pinned window was a full card (its text the focus title, the
// label, the host…), and no chip existed.
func TestAPinnedWindowIsALabelOnlyChip(t *testing.T) {
	m := pinWin(0, "hosta", "work", "@4", "mango:1")
	m.Focus = &FocusView{Title: "Refactoring the widget pipeline", UpdatedAt: time.Now()}
	m.Repo, m.Kind, m.Claude = "widgets", "Claude Code", true
	other := pinWin(1, "hosta", "work", "@5", "vapor:2")
	other.Pinned = false
	other.Focus = &FocusView{Title: "Something else entirely", UpdatedAt: time.Now()}
	s := renderString(t, AgentsCardsWithMirrors(nil, []MirrorCardView{m, other}))
	arts := pinArticles(t, s)

	chip := arts[mirrorCardID(m)]
	if chip == nil {
		t.Fatalf("the pinned window drew no article:\n%s", s)
	}
	if v, _ := pinAttr(chip, "data-pinned-chip"); v != "mirror" {
		t.Fatalf("the pinned window is not a chip (data-pinned-chip=%q):\n%s", v, s)
	}
	if got := pinText(chip); got != "mango" {
		t.Errorf("the chip's whole text = %q, want just the codename %q", got, "mango")
	}
	for _, k := range []string{"data-card-slot", "data-card-host", "data-card-focus", "data-mirror-card",
		"data-mirror-repo", "data-mirror-claude", "data-pinned-glyph", "data-pin-toggle"} {
		if n := pinFind(chip, pinHas(k)); len(n) != 0 {
			t.Errorf("the chip draws %s, which only a card draws", k)
		}
	}
	// Its host icon IS drawn (TestAPinnedChipDrawsItsHostIconBeforeItsLabel
	// pins where and which).
	if icons := pinFind(chip, pinHas("data-host-icon")); len(icons) != 1 {
		t.Errorf("the chip carries %d host icons, want 1", len(icons))
	}
	if strings.Contains(pinText(chip), "Refactoring") {
		t.Errorf("the chip shows the focus title")
	}
	// It is one entry, so ONE article: the chip replaces the card.
	if n := strings.Count(s, `id="`+mirrorCardID(m)+`"`); n != 1 {
		t.Errorf("the pinned window's id appears %d times, want 1 (no card beside the chip)", n)
	}
	// The unpinned window is still a full card (the control).
	card := arts[mirrorCardID(other)]
	if card == nil || len(pinFind(card, pinHas("data-mirror-card"))) != 1 {
		t.Fatalf("the unpinned window is not a card:\n%s", s)
	}
	if !strings.Contains(pinText(card), "Something else entirely") {
		t.Errorf("instrument check: the unpinned card should show its focus title, so its absence on the chip means something")
	}
}

// TestPinnedChipLabelsDropTheIndexUnlessTwoWouldReadTheSame: "mango:1" and
// "vapor:2" read "mango" and "vapor"; two windows of one codename keep their
// index ("mango:1", "mango:2") while an unrelated one is still short; the same
// codename and index on two hosts adds the list's qualifier. Red before the
// chips: no chip labels were drawn at all.
func TestPinnedChipLabelsDropTheIndexUnlessTwoWouldReadTheSame(t *testing.T) {
	cases := []struct {
		name string
		wins []MirrorCardView
		want []string
	}{
		{"distinct codenames", []MirrorCardView{
			pinWin(0, "hosta", "s1", "@1", "mango:1"),
			pinWin(1, "hosta", "s2", "@2", "vapor:2"),
			pinWin(2, "hosta", "s3", "@3", "wheat:13"),
		}, []string{"mango", "vapor", "wheat"}},
		{"one codename twice", []MirrorCardView{
			pinWin(0, "hosta", "s1", "@1", "mango:1"),
			pinWin(1, "hosta", "s1", "@2", "mango:2"),
			pinWin(2, "hosta", "s2", "@3", "vapor:3"),
		}, []string{"mango:1", "mango:2", "vapor"}},
		{"one label on two hosts", func() []MirrorCardView {
			a, b := pinWin(0, "hosta", "s1", "@1", "mango:1"), pinWin(1, "hostb", "s1", "@1", "mango:1")
			a.Qualifier, b.Qualifier = "hosta", "hostb"
			return []MirrorCardView{a, b}
		}(), []string{"mango:1 · hosta", "mango:1 · hostb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pinnedLabels(t, renderString(t, AgentsCardsWithMirrors(nil, c.wins)))
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("chip labels = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPinnedChipLabelFallbacks: a window with no codename shows its
// "<session>:<index>" (index kept: it is not a codename's suffix); a label that
// only repeats the session is no codename; a pinned agent shows its display
// name, else its name, clamped to the cards' 60 runes with a "…". Red before
// the chips.
func TestPinnedChipLabelFallbacks(t *testing.T) {
	noLabel := pinWin(0, "hosta", "build", "@7", "")
	sessionLabel := pinWin(1, "hosta", "deploy", "@8", "deploy")
	long := strings.Repeat("x", 70)
	agents := []AgentCardView{
		{ID: 1, Name: "named-agent", DisplayName: "Pretty agent", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-10 * time.Minute)},
		{ID: 2, Name: "bare-agent", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-11 * time.Minute)},
		{ID: 3, Name: "long-agent", DisplayName: long, Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-12 * time.Minute)},
		// The 60-rune cut lands inside a ZWJ family emoji: the clamp (#242's
		// grapheme-safe clampTitle) moves it back before the whole cluster.
		{ID: 4, Name: "emoji-agent", DisplayName: strings.Repeat("y", 59) + "\U0001F468\u200d\U0001F469\u200d\U0001F467 tail", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-13 * time.Minute)},
	}
	got := pinnedLabels(t, renderString(t, AgentsCardsWithMirrors(agents, []MirrorCardView{noLabel, sessionLabel})))
	want := []string{"build:7", "deploy:8", "Pretty agent", "bare-agent", strings.Repeat("x", 60) + "…", strings.Repeat("y", 59) + "…"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chip labels = %q, want %q", got, want)
	}
}

// cardHref is the href of an entry's CARD nav anchor (its body slot).
func cardHref(t *testing.T, s, id string) string {
	t.Helper()
	a := pinArticles(t, s)[id]
	if a == nil {
		t.Fatalf("no article %s", id)
	}
	for _, n := range pinFind(a, pinHas("data-card-slot")) {
		if v, _ := pinAttr(n, "data-card-slot"); v == "body" {
			h, _ := pinAttr(n, "href")
			return h
		}
	}
	t.Fatalf("article %s has no body anchor", id)
	return ""
}

// chipHref is the href of an entry's pinned chip.
func chipHref(t *testing.T, s, id string) string {
	t.Helper()
	a := pinArticles(t, s)[id]
	if a == nil {
		t.Fatalf("no article %s", id)
	}
	ls := pinFind(a, pinHas("data-pinned-chip-link"))
	if len(ls) != 1 {
		t.Fatalf("article %s has %d chip links, want 1:\n%s", id, len(ls), s)
	}
	h, _ := pinAttr(ls[0], "href")
	return h
}

// TestAPinnedChipOpensWhatItsCardOpens: the chip's link is the card's nav
// anchor's href, for a Claude Code window (the rendered view), a shell window
// (the terminal) and an agent (its page) — compared against the SAME entry
// rendered unpinned, never a literal the chip could share with a wrong
// implementation. And the chip carries the card's filter attributes verbatim,
// so the search, the host tabs and the filter chips treat it as its card.
func TestAPinnedChipOpensWhatItsCardOpens(t *testing.T) {
	claude := pinWin(0, "hosta", "s1", "@1", "mango:1")
	claude.Claude = true
	shell := pinWin(1, "hosta", "s2", "@2", "vapor:2")
	shell.Kind = "shell"
	ag := AgentCardView{ID: 9, Name: "nine", Status: "running", Pinned: true, LastActiveAt: time.Now()}

	unpin := func() ([]AgentCardView, []MirrorCardView) {
		a, c, sh := ag, claude, shell
		a.Pinned, c.Pinned, sh.Pinned = false, false, false
		return []AgentCardView{a}, []MirrorCardView{c, sh}
	}
	ua, um := unpin()
	cards := renderString(t, AgentsCardsWithMirrors(ua, um))
	chips := renderString(t, AgentsCardsWithMirrors([]AgentCardView{ag}, []MirrorCardView{claude, shell}))

	for _, id := range []string{mirrorCardID(claude), mirrorCardID(shell), "agent-9"} {
		want := cardHref(t, cards, id)
		if got := chipHref(t, chips, id); got != want || want == "" {
			t.Errorf("%s: chip href %q, card href %q", id, got, want)
		}
		// Every filter attribute the card carries, the chip carries the same.
		ca, cc := pinArticles(t, cards)[id], pinArticles(t, chips)[id]
		for _, at := range ca.Attr {
			if !(strings.HasPrefix(at.Key, "data-f-") || strings.HasPrefix(at.Key, "data-l-") ||
				at.Key == "data-filter" || at.Key == "data-wait" || at.Key == "data-active-ts" || at.Key == "data-rank-title") {
				continue
			}
			if v, ok := pinAttr(cc, at.Key); !ok || v != at.Val {
				t.Errorf("%s: the chip's %s = %q, the card's %q", id, at.Key, v, at.Val)
			}
		}
	}
	if !strings.Contains(chipHref(t, chips, mirrorCardID(claude)), "/mirror/view?") ||
		!strings.Contains(chipHref(t, chips, mirrorCardID(shell)), "/mirror/terminal?") {
		t.Errorf("instrument check: the two windows should open different pages")
	}
}

// TestAPinnedChipCanBeUnpinnedAndMarksWaiting: every chip carries the Unpin
// form for ITS entry (the × the "Edit pins" toggle reveals), the row has
// exactly one toggle, and a chip whose entry waits on you carries the accent
// dot while one that does not carries none.
func TestAPinnedChipCanBeUnpinnedAndMarksWaiting(t *testing.T) {
	w := pinWin(0, "hosta", "s1", "@1", "mango:1")
	w.Attention = &CardAttention{Count: 1, Kind: "session", Kinds: []string{"session"}, Href: "/attention#x"}
	q := pinWin(1, "hosta", "s2", "@2", "vapor:2")
	ag := AgentCardView{ID: 4, Name: "four", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-time.Hour)}
	s := renderString(t, AgentsCardsWithMirrors([]AgentCardView{ag}, []MirrorCardView{w, q}))
	arts := pinArticles(t, s)

	if n := strings.Count(s, `id="agents-pins-edit"`); n != 1 {
		t.Errorf("%d Edit pins toggles, want 1", n)
	}
	for id, fields := range map[string]string{
		mirrorCardID(w): `name="session" value="s1"`, mirrorCardID(q): `name="window" value="@2"`, "agent-4": `name="agent_id" value="4"`,
	} {
		var b strings.Builder
		if err := html.Render(&b, arts[id]); err != nil {
			t.Fatal(err)
		}
		chip := b.String()
		if !strings.Contains(chip, `action="/agents/pins/delete"`) || !strings.Contains(chip, fields) ||
			!strings.Contains(chip, `data-pinned-chip-unpin`) {
			t.Errorf("%s: the chip has no Unpin form for its own entry:\n%s", id, chip)
		}
	}
	if len(pinFind(arts[mirrorCardID(w)], pinHas("data-pinned-chip-wait"))) != 1 {
		t.Errorf("the waiting chip has no accent dot")
	}
	if len(pinFind(arts[mirrorCardID(q)], pinHas("data-pinned-chip-wait"))) != 0 {
		t.Errorf("a chip that waits on nobody has the accent dot")
	}
	// Nothing pinned: no toggle row.
	q.Pinned = false
	if s := renderString(t, AgentsCardsWithMirrors(nil, []MirrorCardView{q})); strings.Contains(s, "agents-pins-edit") {
		t.Errorf("an unpinned list draws the Edit pins toggle")
	}
}

// pinRender renders an html node back to markup.
func pinRender(t *testing.T, n *html.Node) string {
	t.Helper()
	var b strings.Builder
	if err := html.Render(&b, n); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestAPinnedAgentKeepsItsControlsBehindEditPins: a pinned agent's chip carries
// the card's own rename button and ⋯ menu (Stop for a running agent, Delete) —
// inside the chip's edit controls, which only "Edit pins" shows — and its ⋯
// menu does not offer "Pin to top"; outside them the chip is its label alone. A
// window's chip has only its ×. Red at 5643e772: no edit controls, no menu.
func TestAPinnedAgentKeepsItsControlsBehindEditPins(t *testing.T) {
	ag := AgentCardView{ID: 9, Name: "nine", Status: "running", Pinned: true, LastActiveAt: time.Now()}
	w := pinWin(0, "hosta", "s1", "@1", "mango:1")
	s := renderString(t, AgentsCardsWithMirrors([]AgentCardView{ag}, []MirrorCardView{w}))
	arts := pinArticles(t, s)

	edits := pinFind(arts["agent-9"], pinHas("data-pinned-chip-edit"))
	if len(edits) != 1 {
		t.Fatalf("the agent chip has %d edit-control groups, want 1:\n%s", len(edits), s)
	}
	edit := pinRender(t, edits[0])
	for _, want := range []string{`aria-label="Agent actions"`, `id="agent-menu-9"`, `hx-post="/agents/9/stop"`, `>Stop<`,
		`hx-delete="/agents/9"`, `aria-label="Rename agent"`, `action="/agents/pins/delete"`, `aria-label="Unpin nine"`} {
		if !strings.Contains(edit, want) {
			t.Errorf("the agent chip's edit controls lack %s:\n%s", want, edit)
		}
	}
	if strings.Contains(edit, "Pin to top") {
		t.Errorf("a pinned agent's menu offers Pin to top")
	}
	// Everything but the label is inside the edit controls.
	for _, k := range []string{"aria-haspopup", "popover", "hx-post", "hx-delete", "hx-get"} {
		for _, n := range pinFind(arts["agent-9"], pinHas(k)) {
			inside := false
			for p := n; p != nil; p = p.Parent {
				if _, ok := pinAttr(p, "data-pinned-chip-edit"); ok {
					inside = true
				}
			}
			if !inside {
				t.Errorf("the agent chip draws %s outside its edit controls", k)
			}
		}
	}
	medit := pinFind(arts[mirrorCardID(w)], pinHas("data-pinned-chip-edit"))
	if len(medit) != 1 || strings.Contains(pinRender(t, medit[0]), "Agent actions") ||
		!strings.Contains(pinRender(t, medit[0]), `aria-label="Unpin mango"`) {
		t.Errorf("a window's chip's edit controls should be its × alone")
	}
}

// TestAPinnedAgentChipMarksAStoppedOrFailedStatus: an agent in error, or
// stopped, carries a status dot named for what it is; a running one carries
// none. Red at 5643e772: no status mark on a chip.
func TestAPinnedAgentChipMarksAStoppedOrFailedStatus(t *testing.T) {
	for _, c := range []struct{ status, label string }{{"error", "Agent error"}, {"stopped", "Agent stopped"}, {"running", ""}, {"provisioning", ""}} {
		ag := AgentCardView{ID: 3, Name: "three", Status: c.status, Pinned: true, LastActiveAt: time.Now()}
		chip := pinArticles(t, renderString(t, AgentsCardsWithMirrors([]AgentCardView{ag}, nil)))["agent-3"]
		dots := pinFind(chip, pinHas("data-pinned-chip-status"))
		if c.label == "" {
			if len(dots) != 0 {
				t.Errorf("%s: a status dot on a healthy agent", c.status)
			}
			continue
		}
		if len(dots) != 1 {
			t.Errorf("%s: %d status dots, want 1", c.status, len(dots))
			continue
		}
		if l, _ := pinAttr(dots[0], "aria-label"); l != c.label {
			t.Errorf("%s: dot named %q, want %q", c.status, l, c.label)
		}
	}
}

// TestNoCardDrawsPinnedStateAndAPinnedAgentRendersAsItsChip: a card is only
// ever drawn for an unpinned entry, so the cards carry no pinned glyph, no
// data-pinned and no pressed/Unpin toggle — and RenderAgentCard (the rename
// save/cancel swap of #agent-<id>) draws a PINNED agent as its chip, so a
// rename can never swap a full card into the pinned row. Red at 5643e772:
// RenderAgentCard drew the full card, glyph and all.
func TestNoCardDrawsPinnedStateAndAPinnedAgentRendersAsItsChip(t *testing.T) {
	ag := AgentCardView{ID: 5, Name: "five", DisplayName: "Five", Status: "running", Pinned: true, LastActiveAt: time.Now()}
	var b strings.Builder
	if err := RenderAgentCard(&b, ag); err != nil {
		t.Fatal(err)
	}
	a := pinArticles(t, b.String())["agent-5"]
	if a == nil {
		t.Fatalf("no #agent-5 article:\n%s", b.String())
	}
	if v, _ := pinAttr(a, "data-pinned-chip"); v != "agent" || len(pinFind(a, pinHas("data-card-slot"))) != 0 {
		t.Errorf("RenderAgentCard drew a pinned agent as a card:\n%s", b.String())
	}
	if got := pinnedLabels(t, b.String()); len(got) != 1 || got[0] != "Five" {
		t.Errorf("RenderAgentCard's chip label = %q, want [Five]", got)
	}
	// Unpinned: the card, with no pinned state anywhere on either kind.
	ag.Pinned = false
	w := pinWin(0, "hosta", "s1", "@1", "mango:1")
	w.Pinned = false
	b.Reset()
	if err := RenderAgentCard(&b, ag); err != nil {
		t.Fatal(err)
	}
	cards := b.String() + renderString(t, AgentsCardsWithMirrors(nil, []MirrorCardView{w}))
	for _, bad := range []string{"data-pinned", "aria-pressed", ">Unpin<", `aria-label="Unpin"`} {
		if strings.Contains(cards, bad) {
			t.Errorf("a card draws pinned state %s", bad)
		}
	}
	if !strings.Contains(cards, ">Pin to top<") || !strings.Contains(cards, `aria-label="Pin to top"`) {
		t.Errorf("instrument check: the unpinned cards should offer Pin to top")
	}
}

// TestTheSameCodenameOnTwoHostsKeepsItsTextAndShowsItsIcon: the icon is
// ADDED, the text is not shortened (operator decision "Icon + keep text"). The
// same codename on two hosts that draw DIFFERENT icons, plus a second window of
// it on one host, still reads with its suffix and qualifier exactly as without
// the icon — the labels a surface reusing pinnedChipLabels (the session
// switcher) shows — and each chip draws its own host's icon. The labels part is
// an invariant guard (main read the same); the icon part is red at main (no
// chip drew its icon inside its link).
func TestTheSameCodenameOnTwoHostsKeepsItsTextAndShowsItsIcon(t *testing.T) {
	a1, a2 := pinWin(0, "hosta", "s1", "@1", "mango:1"), pinWin(1, "hosta", "s1", "@2", "mango:2")
	b := pinWin(2, "hostb", "s1", "@1", "mango:1")
	a1.HostIcon, a2.HostIcon, b.HostIcon = "laptop", "laptop", "server"
	a1.Qualifier, b.Qualifier = "hosta", "hostb"
	wins := []MirrorCardView{a1, a2, b}
	s := renderString(t, AgentsCardsWithMirrors(nil, wins))
	if got, want := strings.Join(pinnedLabels(t, s), "|"), "mango:1 · hosta|mango:2|mango:1 · hostb"; got != want {
		t.Errorf("chip labels = %q, want %q", got, want)
	}
	arts := pinArticles(t, s)
	for _, w := range wins {
		chip := arts[mirrorCardID(w)]
		if chip == nil {
			t.Fatalf("no chip for %s/%s", w.Host, w.Window)
		}
		links := pinFind(chip, pinHas("data-pinned-chip-link"))
		if len(links) != 1 {
			t.Fatalf("%s/%s: %d chip links", w.Host, w.Window, len(links))
		}
		icons := pinFind(links[0], pinHas("data-host-icon"))
		if len(icons) != 1 {
			t.Fatalf("%s/%s: %d host icons in the chip link, want 1", w.Host, w.Window, len(icons))
		}
		if k, _ := pinAttr(icons[0], "data-host-icon"); k != w.HostIcon {
			t.Errorf("%s/%s: icon %q, want %q", w.Host, w.Window, k, w.HostIcon)
		}
	}
}

// TestAPinnedChipDrawsItsHostIconBeforeItsLabel: every pinned chip draws ONE
// host icon, inside its tap target (the chip link) and before its label, not
// under any hidden wrapper: a window its host's icon (two hosts, two different
// icons, so a hardcoded key cannot pass), an agent the cluster's. Red at main:
// the icon sat outside the link in a class="hidden" span.
func TestAPinnedChipDrawsItsHostIconBeforeItsLabel(t *testing.T) {
	a := pinWin(0, "hosta", "s1", "@1", "mango:1")
	b := pinWin(1, "hostb", "s2", "@1", "vapor:1")
	a.HostIcon, b.HostIcon = "laptop", "globe"
	agents := []AgentCardView{{ID: 9, Name: "pinned-agent", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-10 * time.Minute)}}
	s := renderString(t, AgentsCardsWithMirrors(agents, []MirrorCardView{a, b}))
	arts := pinArticles(t, s)
	for id, want := range map[string]string{mirrorCardID(a): "laptop", mirrorCardID(b): "globe", "agent-9": "cluster"} {
		chip := arts[id]
		if chip == nil {
			t.Fatalf("no chip %s:\n%s", id, s)
		}
		links := pinFind(chip, pinHas("data-pinned-chip-link"))
		if len(links) != 1 {
			t.Fatalf("%s: %d chip links", id, len(links))
		}
		icons := pinFind(chip, pinHas("data-host-icon"))
		if len(icons) != 1 {
			t.Fatalf("%s: %d host icons, want 1", id, len(icons))
		}
		if k, _ := pinAttr(icons[0], "data-host-icon"); k != want {
			t.Errorf("%s: icon %q, want %q", id, k, want)
		}
		inLink := false
		for p := icons[0].Parent; p != nil && p != chip; p = p.Parent {
			if c, _ := pinAttr(p, "class"); strings.Contains(" "+c+" ", " hidden ") {
				t.Errorf("%s: the icon is under a hidden wrapper (%q)", id, c)
			}
			if p == links[0] {
				inLink = true
			}
		}
		if !inLink {
			t.Errorf("%s: the icon is not inside the chip's link", id)
		}
		// Before the label: the icon's position among the link's descendants.
		var order []string
		for _, n := range pinFind(links[0], func(n *html.Node) bool {
			_, i := pinAttr(n, "data-host-icon")
			_, l := pinAttr(n, "data-pinned-chip-label")
			return i || l
		}) {
			if _, ok := pinAttr(n, "data-host-icon"); ok {
				order = append(order, "icon")
			} else {
				order = append(order, "label")
			}
		}
		if strings.Join(order, ",") != "icon,label" {
			t.Errorf("%s: link order %v, want [icon label]", id, order)
		}
		// The 44px tap target is kept.
		if c, _ := pinAttr(links[0], "class"); !strings.Contains(c, "min-h-[44px]") || !strings.Contains(c, "min-w-[44px]") {
			t.Errorf("%s: the chip link lost its 44px target: %q", id, c)
		}
	}
}

// pinAccName approximates a link's accessible name as a screen reader builds
// it: text, an element's aria-label in place of its content, and nothing from
// an aria-hidden subtree.
func pinAccName(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
			return
		}
		if x.Type == html.ElementNode {
			if v, _ := pinAttr(x, "aria-hidden"); v == "true" {
				return
			}
			if x != n {
				if l, ok := pinAttr(x, "aria-label"); ok {
					b.WriteString(" " + l + " ")
					return
				}
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// TestAChipLinkNamesItsHostOnce: a screen reader hears a chip's host exactly
// once. A chip whose label is unique names it through the icon ("Host: hosta");
// a chip whose label already carries the host ("mango:1 · hosta") gets an
// aria-hidden icon (its tooltip kept), so the host is not read twice. Red at
// main: the icon was outside the link, so the unique chip named no host.
func TestAChipLinkNamesItsHostOnce(t *testing.T) {
	a, b := pinWin(0, "hosta", "s1", "@1", "mango:1"), pinWin(1, "hostb", "s1", "@1", "mango:1")
	a.HostIcon, b.HostIcon = "laptop", "server"
	a.Qualifier, b.Qualifier = "hosta", "hostb"
	solo := pinWin(2, "hostc", "s9", "@1", "vapor:1")
	solo.HostIcon = "globe"
	s := renderString(t, AgentsCardsWithMirrors(nil, []MirrorCardView{a, b, solo}))
	arts := pinArticles(t, s)
	for _, c := range []struct {
		w     MirrorCardView
		label string
		named bool // the label itself spells the host
	}{{a, "mango:1 · hosta", true}, {b, "mango:1 · hostb", true}, {solo, "vapor", false}} {
		chip := arts[mirrorCardID(c.w)]
		links := pinFind(chip, pinHas("data-pinned-chip-link"))
		if len(links) != 1 {
			t.Fatalf("%s: %d chip links", c.w.Host, len(links))
		}
		name := pinAccName(links[0])
		if got := strings.Count(name, c.w.Host); got != 1 {
			t.Errorf("%s: accessible name %q names the host %d times, want 1", c.w.Host, name, got)
		}
		if !strings.Contains(name, c.label) {
			t.Errorf("%s: accessible name %q lacks the label %q", c.w.Host, name, c.label)
		}
		icons := pinFind(links[0], pinHas("data-host-icon"))
		if len(icons) != 1 {
			t.Fatalf("%s: %d icons", c.w.Host, len(icons))
		}
		hidden, _ := pinAttr(icons[0], "aria-hidden")
		if (hidden == "true") != c.named {
			t.Errorf("%s: icon aria-hidden=%q, want hidden=%v", c.w.Host, hidden, c.named)
		}
		if title, _ := pinAttr(icons[0], "title"); title != "Host: "+c.w.Host {
			t.Errorf("%s: icon title %q, want the host kept as a tooltip", c.w.Host, title)
		}
	}
}
