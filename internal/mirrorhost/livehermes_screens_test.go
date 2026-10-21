//go:build livehermes

package mirrorhost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
)

// TestLiveHermesScreens drives the PINNED Hermes into every screen state the
// design lists (docs/design/hermes.md, "The screen check"), and for each:
//
//  1. captures it — with MUSTER_LIVEHERMES_RECORD=1, scrubbed into
//     internal/hermeshost/testdata/screens/<name>.ansi, the fixtures the unit
//     tests (hermeshost's TestScreenFixtures) read;
//  2. measures the screen check on it: refused (and by which rule) or
//     admitted — against screens.json's expectation, written by hand;
//  3. measures what Escape does there: three Escapes, each followed by a wait
//     above prompt_toolkit's timeoutlen and a re-capture, as the gated send
//     sends them — the check's verdict after each — and what the REAL
//     sequence (abort at the first refused capture) would have done.
//
// Its table is the measurement the design's residual list records.
func TestLiveHermesScreens(t *testing.T) {
	r := newLiveRun(t)
	exp := loadScreenExpectations(t)
	homes := map[string]string{}
	home := func(name string, o homeOpts) string {
		if h, ok := homes[name]; ok {
			return h
		}
		o.name = name
		homes[name] = r.makeHome(o)
		return homes[name]
	}
	base := func() string { return home("base", homeOpts{}) }

	var results []screenResult
	record := os.Getenv("MUSTER_LIVEHERMES_RECORD") != ""
	only := os.Getenv("MUSTER_LIVEHERMES_ONLY")
	for _, sc := range liveScenarios(base, home) {
		if only != "" && !regexp.MustCompile(only).MatchString(sc.name) {
			continue
		}
		e, ok := exp[sc.name]
		if !ok {
			t.Errorf("%s: no expectation in screens.json — every scenario is written down by hand first", sc.name)
			continue
		}
		res := runScenario(t, r, sc, e)
		if record && res.reached {
			writeFixture(t, r, sc.name, res.raw)
		}
		results = append(results, res)
	}
	if only == "" {
		for name := range exp {
			found := false
			for _, res := range results {
				found = found || res.name == name
			}
			if !found {
				t.Errorf("%s: in screens.json but no scenario reaches it", name)
			}
		}
	}
	printScreenTable(t, results)
	for _, res := range results {
		if !res.ok {
			t.Errorf("%s: %s", res.name, res.why)
		}
	}
}

// liveScenario is one screen state and how to reach it.
type liveScenario struct {
	name string
	homeOf func() string
	w, h int
	args []string
	// reach drives a fresh CLI on pane into the state and reports whether
	// it got there; settle is extra time before the capture.
	reach func(r *liveRun, home, pane string) bool
	// grab, when set, replaces reach: it reaches the state AND captures it,
	// for a state too short-lived for reach's settle (a resize's hidden bar).
	grab func(r *liveRun, home, pane string) (string, bool)
	// marker is text on screen while the state's overlay is open: after the
	// Escapes the table says whether it still is.
	marker string
}

// screenExpectation is screens.json's entry for a fixture.
type screenExpectation struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	How    string `json:"how"`    // organic | injected
	Expect string `json:"expect"` // admit | refuse
	Rule   string `json:"rule"`   // the refusal's rule, by name (screenRules)
	// Pasted, for a fixture of the input box AFTER a paste: the text pasted
	// (bracketed, no Enter), and what CheckPasted must say of it.
	Pasted       string `json:"pasted,omitempty"`
	PastedExpect string `json:"pastedExpect,omitempty"`
}

// screenRules names the screen check's refusals in screens.json.
var screenRules = map[string]string{
	"NoLayout": hermeshost.ScreenNoLayout, "Below": hermeshost.ScreenBelow, "NoStatusBar": hermeshost.ScreenNoStatusBar,
	"Stash": hermeshost.ScreenStash, "Vi": hermeshost.ScreenVi, "Images": hermeshost.ScreenImages,
	"NotIdle": hermeshost.ScreenNotIdle, "Text": hermeshost.ScreenText, "NotPasted": hermeshost.ScreenNotPasted,
}

func screensDir(t *testing.T) string {
	return filepath.Join(moduleRoot(t), "internal", "hermeshost", "testdata", "screens")
}

func loadScreenExpectations(t *testing.T) map[string]screenExpectation {
	b, err := os.ReadFile(filepath.Join(screensDir(t), "screens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []screenExpectation
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]screenExpectation{}
	for _, e := range list {
		out[e.Name] = e
	}
	return out
}

// screenResult is one scenario's measurement.
type screenResult struct {
	name, state, how string
	reached          bool
	raw              string
	gate             string    // the check before any Escape
	esc              [3]string // the check after each of three Escapes
	sequence         string    // what the gated send's sequence would do
	after            string    // the overlay after three Escapes: open | closed | -
	ok               bool
	why              string
}

// verdict is the check's answer on raw: "admit" or "refuse: <rule name>".
func verdict(raw string) string {
	err := hermeshost.CheckIdle(hermeshost.ParseScreen(raw))
	if err == nil {
		return "admit"
	}
	rule, _ := hermeshost.IsScreenRefusal(err)
	return "refuse: " + ruleName(rule)
}

// ruleName is a refusal's name in screens.json.
func ruleName(rule string) string {
	for k, v := range screenRules {
		if v == rule {
			return k
		}
	}
	return rule
}

// escapeWait is the gated send's wait after each Escape: above
// prompt_toolkit's timeoutlen (1 s), so two Escapes are never one
// double-Escape (docs/design/hermes.md).
const escapeWait = 1500 * time.Millisecond

func runScenario(t *testing.T, r *liveRun, sc liveScenario, e screenExpectation) screenResult {
	res := screenResult{name: sc.name, state: e.State, how: e.How}
	home := sc.homeOf()
	w, h := sc.w, sc.h
	if w == 0 {
		w, h = 160, 45
	}
	args := sc.args
	if args == nil {
		args = []string{"chat"}
	}
	pane := r.cli(home, w, h, args)
	defer r.killWindow(pane)
	if sc.grab != nil {
		raw, ok := sc.grab(r, home, pane)
		if !ok {
			res.raw = raw
			res.why = "the scenario never reached its state; last screen:\n" + showScreen(res.raw, 14)
			return res
		}
		res.raw = raw
	} else {
		if !sc.reach(r, home, pane) {
			res.raw = r.capture(pane)
			res.why = "the scenario never reached its state; last screen:\n" + showScreen(res.raw, 14)
			return res
		}
		res.raw = r.capture(pane)
	}
	res.reached = true
	if e.Pasted != "" {
		// A paste fixture: the text is pasted as the gated send pastes it,
		// and the post-paste check is measured; no Escape follows a paste.
		if out, err := r.tmuxRun("set-buffer", "-b", "lh", "--", e.Pasted); err != nil {
			res.why = "set-buffer: " + out
			return res
		}
		if out, err := r.tmuxRun("paste-buffer", "-p", "-d", "-b", "lh", "-t", pane); err != nil {
			res.why = "paste-buffer: " + out
			return res
		}
		time.Sleep(2 * time.Second)
		res.raw = r.capture(pane)
		res.gate = "pasted: admit"
		if err := hermeshost.CheckPasted(hermeshost.ParseScreen(res.raw), e.Pasted); err != nil {
			rule, _ := hermeshost.IsScreenRefusal(err)
			res.gate = "pasted: refuse: " + ruleName(rule)
		}
		res.sequence, res.after = "(the post-paste check)", "-"
		want := "pasted: " + e.PastedExpect
		res.ok = res.gate == want
		if !res.ok {
			res.why = fmt.Sprintf("CheckPasted said %q, screens.json expects %q; screen:\n%s", res.gate, want, showScreen(res.raw, 14))
		}
		return res
	}
	res.gate = verdict(res.raw)
	for i := range res.esc {
		r.keys(pane, "\x1b")
		time.Sleep(escapeWait)
		res.esc[i] = verdict(r.capture(pane))
	}
	res.after = "-"
	if sc.marker != "" {
		res.after = "closed"
		if strings.Contains(stripSGR(r.capture(pane)), sc.marker) {
			res.after = "open"
		}
	}
	switch {
	case res.gate != "admit":
		res.sequence = "refused before any Escape"
	default:
		res.sequence = "admitted after 3 Escapes"
		for i, v := range res.esc {
			if v != "admit" {
				res.sequence = fmt.Sprintf("aborted after Escape %d (%s)", i+1, v)
				break
			}
		}
	}
	// A rule may name alternatives ("NoLayout|Below") for a transient state
	// whose capture can land in either phase; every alternative refuses.
	want := e.Expect
	res.ok = res.gate == want
	if e.Expect == "refuse" {
		want = "refuse: " + e.Rule
		for _, alt := range strings.Split(e.Rule, "|") {
			res.ok = res.ok || res.gate == "refuse: "+alt
		}
	}
	if !res.ok {
		res.why = fmt.Sprintf("the check said %q, screens.json expects %q; screen:\n%s", res.gate, want, showScreen(res.raw, 14))
	}
	return res
}

// printScreenTable prints the measurement, one row per state.
func printScreenTable(t *testing.T, rs []screenResult) {
	var b strings.Builder
	b.WriteString("\nLIVEHERMES SCREENS (pin " + hermesPin[:7] + ")\n")
	fmt.Fprintf(&b, "%-30s %-9s %-22s %-22s %-22s %-22s %-8s %-36s %s\n", "fixture", "how", "check (before Escape)", "after Escape 1", "after Escape 2", "after Escape 3", "overlay", "the gated send's sequence", "ok")
	pass := 0
	for _, x := range rs {
		ok := "FAIL"
		if x.ok {
			ok, pass = "ok", pass+1
		}
		fmt.Fprintf(&b, "%-30s %-9s %-22s %-22s %-22s %-22s %-8s %-36s %s\n", x.name, x.how, x.gate, x.esc[0], x.esc[1], x.esc[2], x.after, x.sequence, ok)
	}
	fmt.Fprintf(&b, "LIVEHERMES SCREENS: %d of %d states measured as expected\n", pass, len(rs))
	fmt.Print(b.String())
}

// showScreen is raw's last n rows, italic as «…» and long rules folded: what
// a failure message shows.
func showScreen(raw string, n int) string {
	s := hermeshost.ParseScreen(raw)
	rows := s.Rows
	for len(rows) > 0 && strings.TrimSpace(cellsText(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	var b strings.Builder
	rule := regexp.MustCompile(`─{8,}`)
	for _, row := range rows {
		var line strings.Builder
		it := false
		for _, c := range row {
			if c.Italic != it {
				if c.Italic {
					line.WriteString("«")
				} else {
					line.WriteString("»")
				}
				it = c.Italic
			}
			line.WriteRune(c.R)
		}
		if it {
			line.WriteString("»")
		}
		b.WriteString("  |" + rule.ReplaceAllString(strings.TrimRight(line.String(), " "), "─×") + "\n")
	}
	return b.String()
}

// statusRow is the text of the row above the input area's upper rule (the
// status bar's place), "" when s has no input area.
func statusRow(s hermeshost.Screen) string {
	in := hermeshost.InputRow(s)
	if in == "" {
		return ""
	}
	for i := len(s.Rows) - 1; i > 1; i-- {
		if strings.TrimRight(cellsText(s.Rows[i]), " ") == in && strings.HasPrefix(strings.TrimSpace(cellsText(s.Rows[i-1])), "──") {
			return cellsText(s.Rows[i-2])
		}
	}
	return ""
}

// stripSGR is raw without its escape sequences.
func stripSGR(raw string) string {
	var b strings.Builder
	for _, row := range hermeshost.ParseScreen(raw).Rows {
		b.WriteString(cellsText(row))
		b.WriteByte('\n')
	}
	return b.String()
}

func cellsText(row []hermeshost.Cell) string {
	var b strings.Builder
	for _, c := range row {
		b.WriteRune(c.R)
	}
	return b.String()
}

// fixtureRows is how many of a capture's bottom rows a fixture keeps: the
// app region (overlays, status bar, input area) and some transcript.
const fixtureRows = 34

// writeFixture writes raw, scrubbed, as <name>.ansi. It fails when anything
// host-specific survives the scrub.
func writeFixture(t *testing.T, r *liveRun, name, raw string) {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) > fixtureRows {
		lines = lines[len(lines)-fixtureRows:]
	}
	out := strings.Join(lines, "\n") + "\n"
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	home, _ := os.UserHomeDir()
	for _, s := range []struct{ re, with string }{
		{regexp.QuoteMeta(r.dir), "<SCRATCH>"},
		{regexp.QuoteMeta(home), "<HOME>"},
		{`local-[0-9a-f]{64}`, "local-<64hex>"},
		{`\b[0-9a-f]{32}\b`, "<32hex>"},
		{`/tmp/[^\s│\]]*`, "<TMP>"},
	} {
		out = regexp.MustCompile(s.re).ReplaceAllString(out, s.with)
	}
	for _, bad := range []string{user, host, "/home/", r.dir} {
		if bad != "" && strings.Contains(out, bad) {
			t.Fatalf("fixture %s: %q survived the scrub", name, bad)
		}
	}
	must(t, os.WriteFile(filepath.Join(screensDir(t), name+".ansi"), []byte(out), 0o644))
}

// ------------------------------------------------------------- the states

// typed is keys a person types: text, then Enter.
func typed(r *liveRun, pane, s string) { r.keys(pane, s); time.Sleep(300 * time.Millisecond); r.keys(pane, "\r") }

// inputStarts waits until the input area's first row starts with prefix.
func inputStarts(r *liveRun, pane, prefix string, d time.Duration) bool {
	_, ok := r.waitScreen(pane, d, func(_ string, s hermeshost.Screen) bool {
		return strings.HasPrefix(hermeshost.InputRow(s), prefix)
	})
	return ok
}

// shows waits until the screen holds text.
func shows(r *liveRun, pane, text string, d time.Duration) bool {
	_, ok := r.waitScreen(pane, d, func(raw string, s hermeshost.Screen) bool {
		for _, row := range s.Rows {
			if strings.Contains(cellsText(row), text) {
				return true
			}
		}
		return false
	})
	return ok
}

// settle lets a just-drawn state finish drawing.
func settle() { time.Sleep(1200 * time.Millisecond) }

func liveScenarios(base func() string, home func(string, homeOpts) string) []liveScenario {
	idle := func(r *liveRun, _, pane string) bool { return r.waitIdle(pane, 60*time.Second) }
	then := func(steps ...func(r *liveRun, home, pane string) bool) func(r *liveRun, home, pane string) bool {
		return func(r *liveRun, home, pane string) bool {
			for _, s := range steps {
				if !s(r, home, pane) {
					return false
				}
			}
			settle()
			return true
		}
	}
	key := func(k string) func(r *liveRun, _, pane string) bool {
		return func(r *liveRun, _, pane string) bool { r.keys(pane, k); time.Sleep(500 * time.Millisecond); return true }
	}
	typeLine := func(s string) func(r *liveRun, _, pane string) bool {
		return func(r *liveRun, _, pane string) bool { typed(r, pane, s); return true }
	}
	starts := func(p string) func(r *liveRun, _, pane string) bool {
		return func(r *liveRun, _, pane string) bool { return inputStarts(r, pane, p, 40*time.Second) }
	}
	see := func(text string) func(r *liveRun, _, pane string) bool {
		return func(r *liveRun, _, pane string) bool { return shows(r, pane, text, 30*time.Second) }
	}
	inject := func(state string) func(r *liveRun, home, pane string) bool {
		return func(r *liveRun, home, pane string) bool {
			r.injectState(home, state)
			_, err := os.Stat(filepath.Join(home, ".inject.ok"))
			for i := 0; i < 50 && err != nil; i++ {
				time.Sleep(100 * time.Millisecond)
				_, err = os.Stat(filepath.Join(home, ".inject.ok"))
			}
			_ = os.Remove(filepath.Join(home, ".inject.ok"))
			if b, e := os.ReadFile(filepath.Join(home, ".inject.err")); e == nil {
				_ = os.Remove(filepath.Join(home, ".inject.err"))
				fmt.Printf("livehermes: injecting %s failed: %s\n", state, b)
				return false
			}
			return err == nil
		}
	}
	stash := then(idle, key("draft one"), key("\x13"), key("draft two"), key("\x13"), key("\x13"), see("Stash (2 items)"))
	vi := func() string { return home("vi", homeOpts{display: map[string]any{"vim_mode": true}}) }
	de := func() string { return home("de", homeOpts{display: map[string]any{"language": "de"}}) }
	viDE := func() string {
		return home("vi-de", homeOpts{display: map[string]any{"vim_mode": true, "language": "de"}})
	}
	noStash := func() string {
		return home("nostash", homeOpts{display: map[string]any{"status_bar": map[string]any{"fields": []string{"model", "duration"}}}})
	}
	return []liveScenario{
		{name: "idle", homeOf: base, reach: then(idle)},
		{name: "idle-profile", homeOf: func() string { return home("coder", homeOpts{profile: "coder"}) }, reach: then(idle)},
		{name: "busy", homeOf: base, reach: then(idle, typeLine("SLOW10"), starts("☤"))},
		{name: "approval", marker: "Dangerous Command", homeOf: base, reach: then(idle, typeLine("APPROVE"), starts("⚠"))},
		{name: "clarify-choices", homeOf: base, reach: then(idle, typeLine("CLARIFY"), starts("?"))},
		{name: "clarify-freetext", homeOf: base, reach: then(idle, typeLine("CLARIFYFREE"), starts("✎"))},
		{name: "sudo", marker: "Sudo Password Required", homeOf: base, reach: then(idle, typeLine("SUDO"), see("Sudo Password Required"))},
		{name: "new-confirm", marker: "destroys", homeOf: base, reach: then(idle, typeLine("hello"), func(r *liveRun, _, p string) bool { time.Sleep(time.Second); return r.waitIdle(p, 40*time.Second) }, typeLine("/new"), starts("⚠"))},
		{name: "hook-consent", homeOf: func() string { return home("consent", homeOpts{strayHook: true}) }, reach: then(see("[y/N]"))},
		{name: "palette", marker: "Command Palette", homeOf: base, reach: then(idle, key("\x10"))},
		{name: "palette-filtered", marker: "Command Palette", homeOf: base, reach: then(idle, key("\x10"), key("mod"))},
		{name: "palette-over-draft", marker: "Command Palette", homeOf: base, reach: then(idle, key("my draft"), key("\x10"))},
		{name: "model-picker", marker: "Model Picker", homeOf: base, reach: then(idle, typeLine("/model"), see("Model Picker"))},
		{name: "model-picker-filtered", marker: "Model Picker", homeOf: base, reach: then(idle, typeLine("/model"), see("Model Picker"), key("\r"), key("stu"))},
		{name: "model-picker-reasoning", marker: "Model Picker", homeOf: base, reach: then(idle, typeLine("/model"), see("Model Picker"), key("\r"), key("\r"), see("easoning"))},
		{name: "model-picker-during-turn", marker: "Model Picker", homeOf: base, reach: then(idle, typeLine("SLOW8"), starts("☤"), typeLine("/model"), key("my draft"), see("Model Picker"))},
		// The design's hidden-draft case: the picker opening with a draft in
		// the box, which it snapshots and clears
		// (P:hermes_cli/cli_model_switch_mixin.py:620-622). Measured: /model runs
		// at once, in interrupt and queue mode alike, so a typed /model never
		// opens over a draft; the /model handler itself is called (injected)
		// with the draft in the box.
		{name: "model-picker-over-hidden-draft", marker: "Model Picker", homeOf: base, reach: then(idle, key("my hidden draft"), inject("model-picker"), see("Model Picker"))},
		{name: "stash", marker: "Stash (2 items)", homeOf: base, reach: stash},
		{name: "palette-and-stash", marker: "Stash (2 items)", homeOf: base, reach: then(stash, key("\x10"))},
		{name: "stash-indicator-hidden", marker: "Stash (2 items)", homeOf: noStash, reach: then(idle, key("draft one"), key("\x13"), key("draft two"), key("\x13"), key("\x13"), see("Stash (2 items)"))},
		{name: "secret", homeOf: base, reach: then(idle, inject("secret"))},
		{name: "connection", homeOf: base, reach: then(idle, inject("connection"), starts("⚙"))},
		{name: "voice", homeOf: base, reach: then(idle, inject("voice"))},
		{name: "voice-recording", homeOf: base, reach: then(idle, inject("voice-recording"), see("REC"))},
		{name: "voice-processing", homeOf: base, reach: then(idle, inject("voice-processing"), see("Transcribing"))},
		{name: "vi-insert", homeOf: vi, reach: then(func(r *liveRun, _, p string) bool { return shows(r, p, "INSERT", 60*time.Second) })},
		{name: "vi-normal", homeOf: vi, reach: then(func(r *liveRun, _, p string) bool { return shows(r, p, "INSERT", 60*time.Second) }, key("\x1b"), see("NORMAL"))},
		// A turn gives the session a title, right-aligned in the bar: the vi
		// label is then the first thing the trim drops.
		{name: "vi-trimmed", homeOf: vi, reach: then(func(r *liveRun, _, p string) bool { return shows(r, p, "INSERT", 60*time.Second) }, typeLine("hello"), func(r *liveRun, _, p string) bool {
			time.Sleep(2 * time.Second)
			_, ok := r.waitScreen(p, 40*time.Second, func(_ string, s hermeshost.Screen) bool { return hermeshost.InputRow(s) != "" && !strings.Contains(statusRow(s), "INSERT") })
			return ok
		})},
		// After a resize Hermes hides its status bar and both input rules
		// until a 0.35 s debounce clears it (P:hermes_cli/cli_terminal_mixin.py:
		// 395, 436-452): captured within that window.
		{name: "vi-hidden-after-resize", homeOf: vi, grab: func(r *liveRun, _, p string) (string, bool) {
			if !shows(r, p, "INSERT", 60*time.Second) {
				return r.capture(p), false
			}
			settle()
			for try := 0; try < 10; try++ {
				x := 150 - try
				if _, err := r.tmuxRun("resize-window", "-t", p, "-x", strconv.Itoa(x), "-y", "44"); err != nil {
					return "", false
				}
				for i := 0; i < 12; i++ {
					raw := r.capture(p)
					if !strings.Contains(raw, "INSERT") && !strings.Contains(raw, "☤ stub-model") {
						return raw, true
					}
					time.Sleep(25 * time.Millisecond)
				}
				time.Sleep(time.Second)
			}
			return r.capture(p), false
		}},
		{name: "vi-insert-de", homeOf: viDE, reach: then(func(r *liveRun, _, p string) bool { return shows(r, p, "EINFÜGEN", 60*time.Second) })},
		{name: "idle-de", homeOf: de, reach: then(idle)},
		{name: "approval-de", homeOf: de, reach: then(idle, typeLine("APPROVE"), starts("⚠"))},
		{name: "clarify-choices-de", homeOf: de, reach: then(idle, typeLine("CLARIFY"), starts("?"))},
		{name: "narrow", homeOf: base, w: 60, h: 40, reach: then(func(r *liveRun, _, p string) bool { time.Sleep(15 * time.Second); return true })},
		{name: "text-in-box", homeOf: base, reach: then(idle, key("half a thought"))},
		{name: "image-attached", homeOf: base, reach: then(idle, inject("image"))},
		{name: "pasted-short", homeOf: base, reach: then(idle)},
		{name: "pasted-multiline", homeOf: base, reach: then(idle)},
		{name: "pasted-wrapped", homeOf: base, reach: then(idle)},
		{name: "pasted-collapsed", homeOf: base, reach: then(idle)},
		{name: "pasted-scrolled", homeOf: base, reach: then(idle)},
		{name: "pasted-after-draft", homeOf: base, reach: then(idle, key("draft "))},
		{name: "completions-menu", homeOf: base, reach: then(idle, key("/mo"))},
	}
}
