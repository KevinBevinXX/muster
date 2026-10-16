package ui

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestComposerFitCapIsOneNumber holds the cap's two spellings together: the
// script's share of the visible viewport (composerFitShare) and the stylesheet's
// no-script fallback (`40dvh` in web/css/input.css). They are one rule — "40% of
// what is visible" — written in two languages, and a change to one alone would
// make a phone's composer cap at one height before the script runs and another
// after.
func TestComposerFitCapIsOneNumber(t *testing.T) {
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`max-height:\s*var\(--composer-max-h,\s*(\d+)dvh\)`).FindSubmatch(css)
	if m == nil {
		t.Fatal("web/css/input.css has no `max-height: var(--composer-max-h, <n>dvh)` rule for .composer-fit")
	}
	fallback, _ := strconv.Atoi(string(m[1]))
	share, err := strconv.ParseFloat(composerFitShare, 64)
	if err != nil {
		t.Fatal(err)
	}
	if int(share*100+0.5) != fallback || fallback != 40 {
		t.Errorf("the script caps at %.0f%% of the visible viewport and the stylesheet's fallback at %ddvh; "+
			"the operator's rule is 40%% for both", share*100, fallback)
	}
	if !strings.Contains(string(css), "."+composerFitClass) {
		t.Errorf("web/css/input.css has no .%s rule", composerFitClass)
	}
}

// TestBothComposersGrowByTheSameRule pins the seam between the two message boxes
// and the one implementation: each box wears composerFitClass (through
// composerInputClass), and each document that draws a box also carries the
// script — the agent page (and so the chief panel, which renders the same pane),
// the mirrored Rendered view, and the Terminal view.
func TestBothComposersGrowByTheSameRule(t *testing.T) {
	if !strings.Contains(composerInputClass, composerFitClass) {
		t.Fatalf("composerInputClass does not carry %s, so no box grows", composerFitClass)
	}
	script := renderString(t, composerFitScript())
	docs := map[string]string{
		"agent chat pane": renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1})),
		"mirrored composer": renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message"})),
	}
	for name, html := range docs {
		box := regexp.MustCompile(`<textarea[^>]*id="(chat-input|mv-input)"[^>]*>`).FindString(html)
		if box == "" {
			t.Errorf("%s: no message box rendered", name)
			continue
		}
		if !strings.Contains(box, composerFitClass) {
			t.Errorf("%s: its box does not wear %s:\n%s", name, composerFitClass, box)
		}
		if strings.Contains(box, "max-h-32") {
			t.Errorf("%s: its box still carries the old fixed max-h-32 cap", name)
		}
		if !strings.Contains(html, script) {
			t.Errorf("%s: the document does not carry composerFitScript", name)
		}
	}
}
