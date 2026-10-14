package ui

import (
	"strings"
	"testing"
)

// PRETTY NAMES on the mirrored surfaces: a host's label ("mango:1") is what a
// card and a page are TITLED by, the raw tmux session name stays visible beside
// it, and every link and socket keeps addressing the session by its NAME.

func TestAMirrorCardShowsTheLabelAndKeepsTheRawNameAndAddress(t *testing.T) {
	m := MirrorCardView{Host: "laptop", Session: "scratch13", Label: "mango:1", Online: true}
	html := renderNode(t, mirrorCard(m))
	for _, want := range []string{
		`<span data-mirror-label="">mango:1</span>`,
		`>scratch13</span>`,
		`title="tmux session scratch13"`,
		`href="` + strings.ReplaceAll(MirrorTerminalPath("laptop", "scratch13"), "&", "&amp;") + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("labelled card lacks %q:\n%s", want, html)
		}
	}
	// The id is the SESSION's, so a label change (a moved window) morphs the
	// same card instead of replacing it.
	if mirrorCardID(m) != mirrorCardID(MirrorCardView{Host: "laptop", Session: "scratch13", Label: "mango:7"}) {
		t.Error("the card id depends on the label")
	}
	if strings.Contains(MirrorCardHref(m), "mango") {
		t.Errorf("the card's link carries the label: %s", MirrorCardHref(m))
	}
}

func TestAnUnlabelledMirrorCardShowsTheRawNameOnly(t *testing.T) {
	html := renderNode(t, mirrorCard(MirrorCardView{Host: "laptop", Session: "infra-work", Online: true}))
	if !strings.Contains(html, `data-mirror-name="">infra-work</span>`) {
		t.Errorf("an unlabelled card does not show its raw name as its title:\n%s", html)
	}
	for _, absent := range []string{"data-mirror-label", "data-mirror-session", "tmux session"} {
		if strings.Contains(html, absent) {
			t.Errorf("an unlabelled card renders %q:\n%s", absent, html)
		}
	}
}

// TestALabelIsTextNotMarkup: a label is the host's string and is escaped like
// any other.
func TestALabelIsTextNotMarkup(t *testing.T) {
	html := renderNode(t, mirrorCard(MirrorCardView{Host: "laptop", Session: "s", Label: `<img src=x onerror=alert(1)>`}))
	if strings.Contains(html, "<img") || !strings.Contains(html, "&lt;img") {
		t.Fatalf("the label was not escaped:\n%s", html)
	}
}

func TestTheMirrorTerminalPageIsTitledByTheLabelWithTheRawName(t *testing.T) {
	html := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "scratch2", DisplayName: "Gold:8", RawName: "scratch2",
		Badge: "Mirrored · laptop", BackHref: "/agents", WSPath: MirrorTerminalWSPath("laptop", "scratch2")}))
	for _, want := range []string{"<title>Terminal · Gold:8 · muster</title>", `>Gold:8`, `data-mirror-session="" class="ml-1.5 text-xs text-muted">scratch2</span>`, `title="tmux session scratch2"`} {
		if !strings.Contains(html, want) {
			t.Errorf("terminal page lacks %q", want)
		}
	}
	// Control: without a label (DisplayName/RawName empty) it is the raw name only.
	plain := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "scratch2", Badge: "Mirrored · laptop", BackHref: "/agents",
		WSPath: MirrorTerminalWSPath("laptop", "scratch2")}))
	if !strings.Contains(plain, "<title>Terminal · scratch2 · muster</title>") || strings.Contains(plain, "data-mirror-session") {
		t.Errorf("an unlabelled terminal page is not titled by its raw name alone")
	}
}

func TestTheRenderedViewIsTitledByTheLabelAndAddressedByTheSession(t *testing.T) {
	html := renderNode(t, MirrorViewPage(MirrorViewView{Host: "laptop", Session: "scratch13", Label: "mango:1"}))
	for _, want := range []string{"<title>mango:1 · Mirrored · muster</title>", `<span data-mirror-label="">mango:1</span>`,
		`data-mirror-session="" class="ml-1.5 text-xs text-muted">scratch13</span>`, `data-ws="` + strings.ReplaceAll(MirrorViewWSPath("laptop", "scratch13"), "&", "&amp;") + `"`} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered view lacks %q", want)
		}
	}
	plain := renderNode(t, MirrorViewPage(MirrorViewView{Host: "laptop", Session: "scratch13"}))
	if !strings.Contains(plain, "<title>scratch13 · Mirrored · muster</title>") || strings.Contains(plain, "data-mirror-label") {
		t.Errorf("an unlabelled rendered view is not titled by its raw name alone")
	}
}

// A QUALIFIED TITLE: an entry whose name another listed entry shares carries a
// qualifier (mirror.SessionView.Qualifier), drawn as PART OF THE TITLE — inside
// data-mirror-name, right after the name — on the card, the rendered view's
// header and <title>, and the terminal's. With none, nothing changes.
func TestAQualifiedMirrorTitleIsPartOfTheTitleEverywhere(t *testing.T) {
	const q = `<span data-mirror-qualifier=""> · tower</span>`

	labelled := renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "mango", Window: "@7", Label: "mango:1", Qualifier: "tower", Online: true}))
	if !strings.Contains(labelled, `<span data-mirror-label="">mango:1</span>`+q+`<span data-mirror-session=""`) {
		t.Errorf("a labelled card's qualifier is not between its label and its session:\n%s", labelled)
	}
	if !strings.Contains(labelled, `data-filter="mango:1 mango:1 · tower mango tower `) {
		t.Errorf("the filter does not carry the qualified title:\n%s", labelled)
	}
	unlabelled := renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "solo", Window: "@2", Title: "solo:3", Qualifier: "tower", Online: true}))
	if !strings.Contains(unlabelled, `data-mirror-name="">solo:3`+q+`</span>`) {
		t.Errorf("an unlabelled card's qualifier is not part of its name:\n%s", unlabelled)
	}

	view := renderNode(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "mango", Window: "@7", Label: "mango:1", Qualifier: "tower"}))
	for _, want := range []string{"<title>mango:1 · tower · Mirrored · muster</title>", `<span data-mirror-label="">mango:1</span>` + q} {
		if !strings.Contains(view, want) {
			t.Errorf("rendered view lacks %q", want)
		}
	}

	term := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "mango", DisplayName: "mango:1", RawName: "mango", TitleQualifier: "tower",
		Badge: "Mirrored · tower", BackHref: "/agents"}))
	for _, want := range []string{"<title>Terminal · mango:1 · tower · muster</title>", `<span data-mirror-label="">mango:1</span>` + q} {
		if !strings.Contains(term, want) {
			t.Errorf("terminal page lacks %q", want)
		}
	}
	// A window with no label: the title text, then the qualifier.
	termPlain := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "solo", DisplayName: "solo:3", TitleQualifier: "tower", BackHref: "/agents"}))
	if !strings.Contains(termPlain, "<title>Terminal · solo:3 · tower · muster</title>") || !strings.Contains(termPlain, `data-mirror-name="">solo:3`+q+`</span>`) {
		t.Errorf("an unlabelled window's terminal page is not titled solo:3 · tower")
	}

	// Control: no qualifier — no qualifier markup, and the titles as before.
	for name, html := range map[string]string{
		"card": renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "mango", Label: "mango:1", Online: true})),
		"view": renderNode(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "mango", Label: "mango:1"})),
		"term": renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "mango", DisplayName: "mango:1", RawName: "mango", BackHref: "/agents"})),
	} {
		if strings.Contains(html, "data-mirror-qualifier") || strings.Contains(html, " · tower ·") {
			t.Errorf("%s: an unqualified title drew a qualifier", name)
		}
	}
}

// THE HOST BADGE IS NOT THE GREY CHIP: on the card and on both pages it is the
// tinted, bold mirrorBadge (with its screen icon), so the host stands out on a
// phone among the kind/mode chips.
func TestTheMirroredHostBadgeIsDistinctFromTheGenericChip(t *testing.T) {
	const tint = "bg-st-progress-bg"
	grey := renderNode(t, chip("kind", "x"))
	if strings.Contains(grey, tint) {
		t.Fatal("CONTROL: the generic chip already carries the tint — this test cannot tell them apart")
	}
	badge := func(html, attr string) string {
		i := strings.Index(html, attr)
		if i < 0 {
			t.Fatalf("no %s in:\n%s", attr, html)
		}
		rest := html[i:]
		return rest[:strings.Index(rest, "</span></span>")]
	}
	for name, got := range map[string]string{
		"card": badge(renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "s", Online: true})), "data-mirror-badge"),
		"view": badge(renderNode(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "s"})), "data-term-badge"),
		"term": badge(renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "s", Badge: "Mirrored · tower", BackHref: "/agents"})), "data-term-badge"),
	} {
		for _, want := range []string{tint, "font-semibold", "<svg", "Mirrored · tower"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s badge lacks %q: %s", name, want, got)
			}
		}
		if strings.Contains(got, `<span class="text-muted">kind</span>`) {
			t.Errorf("%s badge is still the generic kind chip: %s", name, got)
		}
	}
}
