//go:build livehermes

package hermeshost

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The ledgers held against the INSTALLED, PINNED Hermes (`make
// test-livehermes`): what this package hard-codes from the pin is re-read
// from the pin's own source and compared, so a pin bump that changes it
// fails here instead of being trusted.

const pin = "5fc490e2b34123212d73ef316086f939567a725a"

// pinnedSource is the installed Hermes's site-packages and bundled locales,
// after proving it is the pin.
func pinnedSource(t *testing.T) (src, locales string) {
	t.Helper()
	p, err := exec.LookPath("hermes")
	if err != nil {
		t.Fatalf("no hermes on PATH: %v", err)
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		m := regexp.MustCompile(`(?m)^export ` + name + `='([^']*)'`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("the hermes launcher %s exports no %s", p, name)
		}
		return string(m[1])
	}
	if rev := get("HERMES_REVISION"); rev != pin {
		t.Fatalf("hermes on PATH is %q, not the pin %s", rev, pin)
	}
	out, err := exec.Command(get("HERMES_PYTHON"), "-c", "import hermes_cli,os;print(os.path.dirname(os.path.dirname(hermes_cli.__file__)))").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), get("HERMES_BUNDLED_LOCALES")
}

// TestOverlayLedgerMatchesThePinnedSource extracts, from the pinned
// cli_tui_mixin.py and cli_modal_mixin.py, every `self._<name>_state`
// attribute and the layout's children in order, and fails when either
// differs from the ledger — grown, shrunk or reordered.
func TestOverlayLedgerMatchesThePinnedSource(t *testing.T) {
	src, _ := pinnedSource(t)
	var states []string
	for _, f := range []string{"cli_tui_mixin.py", "cli_modal_mixin.py"} {
		b, err := os.ReadFile(filepath.Join(src, "hermes_cli", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`self\.(_[a-z_]+_state)\b`).FindAllStringSubmatch(string(b), -1) {
			if !slices.Contains(states, m[1]) {
				states = append(states, m[1])
			}
		}
	}
	slices.Sort(states)
	var ledger []string
	for _, s := range overlayStates {
		ledger = append(ledger, s.Attr)
	}
	if !slices.Equal(states, ledger) {
		t.Errorf("the pinned source's overlay state attributes are\n  %q\nthe ledger's\n  %q\nA new or removed overlay: classify it, record its fixture (make test-livehermes), then update overlayStates", states, ledger)
	}
	if len(states) < 5 {
		t.Fatalf("instrument check: extracted %d state attributes", len(states))
	}

	b, err := os.ReadFile(filepath.Join(src, "hermes_cli", "cli_tui_mixin.py"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	i := strings.Index(body, "def _build_tui_layout_children(")
	if i < 0 {
		t.Fatal("no _build_tui_layout_children in the pinned cli_tui_mixin.py")
	}
	j := strings.Index(body[i:], "ordered = [")
	k := strings.Index(body[i+j:], "]\n")
	if j < 0 || k < 0 {
		t.Fatal("no `ordered = [...]` in _build_tui_layout_children")
	}
	var layout []string
	block := body[i+j+len("ordered = [") : i+j+k]
	item := regexp.MustCompile(`Window\(height=0\)|\*self\.(\w+)\(\)|getattr\(self, "(\w+)", None\)|(\w+)`)
	for _, m := range item.FindAllStringSubmatch(block, -1) {
		for _, g := range m[1:] {
			if g != "" {
				layout = append(layout, g)
			}
		}
	}
	var want []string
	for _, c := range layoutChildren {
		want = append(want, c.Name)
	}
	if !slices.Equal(layout, want) {
		t.Errorf("the pinned layout's children are\n  %q\nthe ledger's\n  %q", layout, want)
	}
}

// TestViLabelsMatchThePinnedLocales re-extracts every bundled language's vi
// labels (cli.status_bar.vim_*) and fails on any difference from ViLabels.
func TestViLabelsMatchThePinnedLocales(t *testing.T) {
	src, locales := pinnedSource(t)
	i18n, err := os.ReadFile(filepath.Join(src, "agent", "i18n.py"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)SUPPORTED_LANGUAGES: tuple\[str, \.\.\.\] = \((.*?)\)`).FindSubmatch(i18n)
	if m == nil {
		t.Fatal("no SUPPORTED_LANGUAGES in the pinned agent/i18n.py")
	}
	var langs []string
	for _, q := range regexp.MustCompile(`"([a-z-]+)"`).FindAllSubmatch(m[1], -1) {
		langs = append(langs, string(q[1]))
	}
	if len(langs) != len(ViLabels) {
		t.Errorf("the pin supports %d languages %q; ViLabels has %d", len(langs), langs, len(ViLabels))
	}
	for _, l := range langs {
		b, err := os.ReadFile(filepath.Join(locales, l+".yaml"))
		if err != nil {
			t.Errorf("%s: %v", l, err)
			continue
		}
		var doc struct {
			CLI struct {
				StatusBar map[string]string `yaml:"status_bar"`
			} `yaml:"cli"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Errorf("%s: %v", l, err)
			continue
		}
		sb := doc.CLI.StatusBar
		got := [3]string{sb["vim_insert"], sb["vim_replace"], sb["vim_normal"]}
		if got != ViLabels[l] {
			t.Errorf("%s: the pin's vi labels are %q, ViLabels has %q", l, got, ViLabels[l])
		}
	}
}
