package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE TERMINAL PAGE NEVER CALLS ITSELF "READ-ONLY" WHERE IT CAN SEND.
//
// The switch used to read "Read-only" and the status "Connected · read-only"
// whenever live keys were off, while the composer under the terminal sends a
// message without the switch: a label telling a person they cannot send, on a
// page that can. The switch now names what it gates (Live keys off / on), and a
// host that genuinely cannot be sent to says so once, in its composer's
// disabled note.
//
// Red at origin/main 5252925 (the switch's label and both status strings carry
// "read-only"); green at HEAD.

var (
	scriptBlock = regexp.MustCompile(`(?s)<script\b[^>]*>.*?</script>`)
	jsLiteral   = regexp.MustCompile(`'[^'\n]*'`)
	anyTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	readOnlyRe  = regexp.MustCompile(`(?i)read[- ]only`)
)

// readOnlyWording is every place page SHOWS "read-only" to a person: its text
// outside scripts, its attributes a person reads (placeholder, title,
// aria-label), and the string literals its scripts put on screen. Script
// COMMENTS are not shown and are not counted.
func readOnlyWording(page string) []string {
	var out []string
	for _, s := range scriptBlock.FindAllString(page, -1) {
		for _, lit := range jsLiteral.FindAllString(s, -1) {
			if readOnlyRe.MatchString(lit) {
				out = append(out, "script literal "+lit)
			}
		}
	}
	rest := scriptBlock.ReplaceAllString(page, "")
	for _, attr := range regexp.MustCompile(`(?:placeholder|title|aria-label)="([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		if readOnlyRe.MatchString(attr[1]) {
			out = append(out, "attribute "+attr[0])
		}
	}
	text := anyTag.ReplaceAllString(rest, " ")
	for _, m := range readOnlyRe.FindAllStringIndex(text, -1) {
		lo, hi := max(0, m[0]-40), min(len(text), m[1]+40)
		out = append(out, "text …"+strings.Join(strings.Fields(text[lo:hi]), " ")+"…")
	}
	return out
}

// theSwitchSaysLiveKeys asserts the switch and the status line name live keys.
func theSwitchSaysLiveKeys(t *testing.T, what, page string) {
	t.Helper()
	label := page[strings.Index(page, `id="term-mode-label"`):]
	label = label[strings.Index(label, ">")+1 : strings.Index(label, "</span>")]
	if label != ui.LiveKeysOff {
		t.Errorf("%s: the switch opens labelled %q, want %q", what, label, ui.LiveKeysOff)
	}
	for _, lit := range []string{"'" + ui.LiveKeysOn + "'", "'" + ui.LiveKeysOff + "'", "'" + ui.StatusConnected + "'", "'" + ui.StatusLiveKeys + "'"} {
		if !strings.Contains(page, lit) {
			t.Errorf("%s: the page's script does not use %s", what, lit)
		}
	}
}

// TestAnInteractiveHostsTerminalPageSaysNothingIsReadOnly: the composer can
// send, so nothing on the page may read "read-only".
func TestAnInteractiveHostsTerminalPageSaysNothingIsReadOnly(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}})
	for _, path := range []string{chiefTermPage, mainPage} {
		_, page := mirrorGet(t, s, ts, path)
		if tagOf(page, "mv-input") == "" || disabledAttr.MatchString(tagOf(page, "mv-input")) {
			t.Fatalf("%s: fixture: the composer is not there and enabled", path)
		}
		if got := readOnlyWording(page); len(got) != 0 {
			t.Errorf("%s: a page whose composer can send says read-only:\n  %s", path, strings.Join(got, "\n  "))
		}
		theSwitchSaysLiveKeys(t, path, page)
	}
}

// TestAReadOnlyHostsOnlyExplanationIsItsComposersNote: on a host that genuinely
// cannot be sent to, the composer's disabled note (and its placeholder) is the
// ONE place the page says so — no switch, no status wording.
func TestAReadOnlyHostsOnlyExplanationIsItsComposersNote(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: false, v2: &fakeMirrorV2{}})
	_, page := mirrorGet(t, s, ts, chiefTermPage)
	if !strings.Contains(page, `<p id="mv-readonly"`) || !strings.Contains(page, ui.MirrorReadOnlyNote) {
		t.Fatal("a read-only host's page does not carry its composer's note")
	}
	if mode := tagOf(page, "term-mode"); !strings.Contains(mode, ` hidden`) {
		t.Errorf("a read-only host's page offers the Live keys switch: %s", mode)
	}
	// Remove the composer's own words (its note, which it also carries as a
	// tooltip, and its placeholder): nothing else may say it.
	if got := readOnlyWording(page); len(got) == 0 {
		t.Fatal("control: readOnlyWording does not see the composer's own note")
	}
	rest := strings.ReplaceAll(page, ui.MirrorReadOnlyNote, "")
	rest = strings.Replace(rest, `placeholder="Read-only host"`, "", 1)
	if got := readOnlyWording(rest); len(got) != 0 {
		t.Errorf("a read-only host's page says read-only outside its composer's note:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestAnAgentsTerminalPageSaysNothingIsReadOnly: an agent pod's terminal page
// has the same switch and a composer that sends.
func TestAnAgentsTerminalPageSaysNothingIsReadOnly(t *testing.T) {
	var b strings.Builder
	c := ui.ComposerView{SendPath: ui.AgentSendPath("keen-wren"), Placeholder: "Message…", Aria: "Send a message"}
	if err := ui.AgentTerminalPage(ui.AgentTerminalView{Name: "keen-wren", Composer: &c}).Render(&b); err != nil {
		t.Fatal(err)
	}
	if got := readOnlyWording(b.String()); len(got) != 0 {
		t.Errorf("an agent's terminal page says read-only:\n  %s", strings.Join(got, "\n  "))
	}
	theSwitchSaysLiveKeys(t, "an agent's terminal", b.String())
}
