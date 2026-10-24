package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The terminal's quick keys (terminal.go quickKeyBar): operator ask "In the
// agent raw view show the up and down arrow keys and the return button. As
// large key buttons, and then collapse the rest into a three dots button
// context menu." Both views draw it — a dispatched agent's terminal and a
// mirrored window's (AgentTerminalPage either way) — so both are rendered.

// quickKeyLedger is EVERY quick key the bar had before this change, with the
// bytes it sent, as literal hex — NOT read from QuickKeys, so a key dropped
// from (or added to) the table, or a changed sequence, fails here.
var quickKeyLedger = map[string]string{
	"esc":   "1b",
	"tab":   "09",
	"stab":  "1b5b5a",
	"ctrlc": "03",
	"enter": "0d",
	"1":     "31",
	"up":    "1b5b41",
	"down":  "1b5b42",
	"left":  "1b5b44",
	"right": "1b5b43",
	"2":     "32",
	"3":     "33",
}

func termKeysPages() map[string]AgentTerminalView {
	return map[string]AgentTerminalView{
		"agent":  {Name: "demo-agent"},
		"mirror": {Name: "s1", WSPath: "/mirror/terminal/ws?host=hosta&session=s1", BackHref: "/agents", Composer: &MirrorComposerView{}, StepUp: &StepUpView{}},
	}
}

func termKeyByID(t *testing.T, doc *html.Node, id string) *html.Node {
	t.Helper()
	ns := pinFind(doc, func(n *html.Node) bool { v, _ := pinAttr(n, "id"); return v == id })
	if len(ns) != 1 {
		t.Fatalf("%d elements #%s, want 1", len(ns), id)
	}
	return ns[0]
}

// keysIn is the [data-key] buttons under n, in document order, as "id=hex".
func keysIn(n *html.Node) []string {
	var out []string
	for _, b := range pinFind(n, pinHas("data-key")) {
		k, _ := pinAttr(b, "data-key")
		s, _ := pinAttr(b, "data-seq")
		out = append(out, k+"="+s)
	}
	return out
}

// TestTheQuickKeyBarShowsUpDownReturnLargeAndTheRestInAMenu: the bar's own
// buttons are ↑ ↓ ⏎ (large, 56px) and a ⋯; the ⋯'s floating menu holds the
// other nine, in their old order; together they are the whole ledger, each
// key exactly once with exactly its old bytes. Red at main: the bar drew all
// twelve in one grid and had no ⋯ or menu.
func TestTheQuickKeyBarShowsUpDownReturnLargeAndTheRestInAMenu(t *testing.T) {
	for name, v := range termKeysPages() {
		t.Run(name, func(t *testing.T) {
			s := renderString(t, AgentTerminalPage(v))
			doc := pinParse(t, s)
			bar := termKeyByID(t, doc, "term-keys")
			menu := termKeyByID(t, doc, "term-keys-menu")

			// The large keys: the bar's DIRECT children with a data-key.
			var big []string
			for c := bar.FirstChild; c != nil; c = c.NextSibling {
				if c.Type != html.ElementNode {
					continue
				}
				if _, ok := pinAttr(c, "data-key"); ok {
					big = append(big, keysIn(c)...)
					cls, _ := pinAttr(c, "class")
					if !strings.Contains(cls, "h-14") || !strings.Contains(cls, "min-w-[44px]") {
						t.Errorf("large key %v is not at least 44px both ways: %q", keysIn(c), cls)
					}
				}
			}
			if got, want := strings.Join(big, " "), "up=1b5b41 down=1b5b42 enter=0d"; got != want {
				t.Errorf("large keys = %q, want %q", got, want)
			}

			rest := keysIn(menu)
			if got, want := strings.Join(rest, " "), "esc=1b tab=09 stab=1b5b5a ctrlc=03 1=31 left=1b5b44 right=1b5b43 2=32 3=33"; got != want {
				t.Errorf("menu keys = %q, want %q", got, want)
			}
			for _, b := range pinFind(menu, pinHas("data-key")) {
				if r, _ := pinAttr(b, "role"); r != "menuitem" {
					t.Errorf("menu key %v has role %q, want menuitem", keysIn(b), r)
				}
			}

			// The ledger: every [data-key] in the bar, each exactly once, with its bytes.
			all := keysIn(bar)
			seen := map[string]int{}
			for _, kv := range all {
				seen[kv]++
			}
			for id, hex := range quickKeyLedger {
				if seen[id+"="+hex] != 1 {
					t.Errorf("key %s=%s drawn %d times, want 1", id, hex, seen[id+"="+hex])
				}
			}
			if len(all) != len(quickKeyLedger) {
				t.Errorf("%d keys drawn, want %d: %v", len(all), len(quickKeyLedger), all)
			}

			// The ⋯ opens the menu, which is the floating menu (placed by its
			// script, which the page carries).
			more := termKeyByID(t, doc, "term-keys-more")
			if pt, _ := pinAttr(more, "popovertarget"); pt != "term-keys-menu" {
				t.Errorf("⋯ popovertarget = %q, want term-keys-menu", pt)
			}
			if _, ok := pinAttr(more, "data-key"); ok {
				t.Errorf("the ⋯ is not a key")
			}
			for _, k := range []string{"data-floating-menu", "popover"} {
				if _, ok := pinAttr(menu, k); !ok {
					t.Errorf("the menu lacks %s", k)
				}
			}
			if !strings.Contains(s, "__musterFloatingMenu") {
				t.Errorf("the page does not carry floatingMenuScript")
			}
		})
	}
}

// TestNoQuickKeyCanVanishFromBothTheBarAndTheMenu: the ⋯ menu is the
// complement of the large keys, so WHATEVER the large-key list says — a
// different choice, an ID no key has, an ID twice, nothing at all — the bar
// still draws every ledgered key exactly once with its bytes, and the large
// keys are exactly the listed ones that exist. A second hand-kept list for the
// menu would fail the "a different pick" and "none" cases.
func TestNoQuickKeyCanVanishFromBothTheBarAndTheMenu(t *testing.T) {
	saved := quickKeysPrimaryOrder
	t.Cleanup(func() { quickKeysPrimaryOrder = saved })
	for name, c := range map[string]struct {
		order []string
		large string
	}{
		"as shipped":       {saved, "up=1b5b41 down=1b5b42 enter=0d"},
		"a different pick": {[]string{"esc", "3"}, "esc=1b 3=33"},
		"an unknown id":    {[]string{"up", "no-such-key"}, "up=1b5b41"},
		"an id twice":      {[]string{"down", "down"}, "down=1b5b42"},
		"none":             {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			quickKeysPrimaryOrder = c.order
			doc := pinParse(t, renderString(t, AgentTerminalPage(AgentTerminalView{Name: "demo-agent"})))
			bar := termKeyByID(t, doc, "term-keys")
			var large []string
			for ch := bar.FirstChild; ch != nil; ch = ch.NextSibling {
				if _, ok := pinAttr(ch, "data-key"); ch.Type == html.ElementNode && ok {
					large = append(large, keysIn(ch)...)
				}
			}
			if got := strings.Join(large, " "); got != c.large {
				t.Errorf("large keys = %q, want %q", got, c.large)
			}
			all := keysIn(bar)
			seen := map[string]int{}
			for _, kv := range all {
				seen[kv]++
			}
			for id, hex := range quickKeyLedger {
				if seen[id+"="+hex] != 1 {
					t.Errorf("key %s=%s drawn %d times, want 1", id, hex, seen[id+"="+hex])
				}
			}
			if len(all) != len(quickKeyLedger) {
				t.Errorf("%d keys drawn, want %d: %v", len(all), len(quickKeyLedger), all)
			}
		})
	}
}
