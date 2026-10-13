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
