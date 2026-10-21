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

// TestScreenConditionsMatchThePinnedSource extracts, from the pinned source,
// the conditions _get_tui_prompt_fragments branches on (in order), those
// that blank the status bar, and the attributes the chrome's visibility
// filters read, and fails when any list differs from the ledger.
func TestScreenConditionsMatchThePinnedSource(t *testing.T) {
	src, _ := pinnedSource(t)
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(src, "hermes_cli", f))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	between := func(body, from, to string) string {
		i := strings.Index(body, from)
		if i < 0 {
			t.Fatalf("the pinned source has no %q", from)
		}
		j := strings.Index(body[i+len(from):], to)
		if j < 0 {
			t.Fatalf("the pinned source has no %q after %q", to, from)
		}
		return body[i : i+len(from)+j]
	}
	attr := regexp.MustCompile(`(?:self\.|getattr\(self, ")(_\w+)`)
	tui := read("cli_tui_mixin.py")
	var prompt []string
	for _, line := range strings.Split(between(tui, "def _get_tui_prompt_fragments", "\n    def "), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "if ") {
			for _, m := range attr.FindAllStringSubmatch(l, -1) {
				prompt = append(prompt, m[1])
			}
		}
	}
	var want []string
	for _, c := range promptConditions {
		want = append(want, c.Attr)
	}
	if !slices.Equal(prompt, want) {
		t.Errorf("the pinned prompt conditions are\n  %q\nthe ledger's\n  %q", prompt, want)
	}
	var blank []string
	for _, m := range attr.FindAllStringSubmatch(between(read("cli_status_bar_mixin.py"), "def _get_status_bar_fragments", "return []"), -1) {
		blank = append(blank, m[1])
	}
	if !slices.Equal(blank, statusBarBlanking) {
		t.Errorf("the pinned status-bar blanking conditions are %q, the ledger's %q", blank, statusBarBlanking)
	}
	var filters []string
	ref := regexp.MustCompile(`cli_ref\.(_\w+(?:\.\w+)?)|getattr\(cli_ref, "(_\w+)"`)
	for _, m := range ref.FindAllStringSubmatch(between(tui, "voice_status_bar = ConditionalContainer(", "self._register_extra_tui_keybindings"), -1) {
		name := m[1] + m[2]
		if strings.HasPrefix(name, "_get_") {
			continue // a renderer, not a condition
		}
		filters = append(filters, name)
	}
	if !slices.Equal(filters, chromeFilters) {
		t.Errorf("the pinned chrome filters read %q, the ledger's %q", filters, chromeFilters)
	}
	if len(prompt) < 10 || len(blank) < 3 || len(filters) < 4 {
		t.Fatalf("instrument check: extracted %d/%d/%d", len(prompt), len(blank), len(filters))
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
