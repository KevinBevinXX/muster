package hermeshost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// THE OVERLAY-STATE LEDGER (docs/design/hermes.md, "A ledger of overlay
// states"): every overlay or modal state attribute the pinned
// hermes_cli/cli_tui_mixin.py and cli_modal_mixin.py define (`self._*_state`);
// every child of the CLI's layout in its order (_build_tui_layout_children);
// every condition the input line's prefix branches on
// (_get_tui_prompt_fragments); every condition that blanks the status bar
// (_get_status_bar_fragments); and every attribute the chrome's visibility
// filters read — each with what it does to the screen the check reads and
// the fixtures that show it.
//
// ⚠ What it does NOT pin: key bindings (which overlay Enter or Escape goes
// to), skins and themes (the glyphs), and anything a plugin draws. Those are
// the live harness's to catch, by its fixtures.
//
// 🔴 IT IS A RELATIONSHIP WITH THE PINNED SOURCE, AND THE LIVE HALF HOLDS IT:
// TestOverlayLedgerMatchesThePinnedSource (build tag livehermes, `make
// test-livehermes`) extracts both lists from the installed Hermes and fails
// when either GROWS or SHRINKS (or the layout's order changes), so a new
// overlay cannot arrive with a pin bump unseen. This file's unit test holds
// the other side: every fixture named here exists and is in screens.json.

// overlayState is one `self._<name>_state` attribute.
type overlayState struct {
	Attr string
	// ChangesInputLine: the prompt prefix or placeholder changes while it is
	// set (P:hermes_cli/cli_tui_mixin.py:309-346); false for the overlays
	// that leave the input line alone.
	ChangesInputLine bool
	// Signal is what the screen check refuses it by.
	Signal   string
	Fixtures []string
}

// overlayStates is the ledger of state attributes, sorted.
var overlayStates = []overlayState{
	{"_approval_state", true, "NotIdle (⚠)", []string{"approval", "approval-de"}},
	{"_clarify_state", true, "NotIdle (? and ✎)", []string{"clarify-choices", "clarify-freetext", "clarify-choices-de"}},
	{"_command_palette_state", false, "NoStatusBar: the status bar renders nothing while it is set (cli_status_bar_mixin.py:1128-1133)", []string{"palette", "palette-filtered", "palette-over-draft", "palette-and-stash"}},
	{"_connection_state", true, "NotIdle (⚙)", []string{"connection"}},
	{"_model_picker_state", false, "NoStatusBar: the status bar renders nothing while it is set (cli_status_bar_mixin.py:1128-1133)", []string{"model-picker", "model-picker-filtered", "model-picker-reasoning", "model-picker-during-turn", "model-picker-over-hidden-draft"}},
	{"_secret_state", true, "NotIdle (🔑)", []string{"secret"}},
	{"_slash_confirm_state", true, "NotIdle (⚠)", []string{"new-confirm"}},
	{"_sudo_state", true, "NotIdle (🔐)", []string{"sudo"}},
}

// layoutChild is one child of the CLI's root HSplit, in order.
type layoutChild struct {
	Name string
	// Region: where the check sees it — "above" the status bar (not read
	// except through the status bar's own marks), "status", "rule", "image"
	// (between the upper rule and the prompt: refused), "input", "below"
	// (anything drawn there is refused).
	Region   string
	Fixtures []string
}

// layoutChildren is the ledger of layout children, in the pinned order.
var layoutChildren = []layoutChild{
	{"sudo_widget", "above", []string{"sudo"}},
	{"secret_widget", "above", []string{"secret"}},
	{"connection_widget", "above", []string{"connection"}},
	{"approval_widget", "above", []string{"approval"}},
	{"slash_confirm_widget", "above", []string{"new-confirm"}},
	{"clarify_widget", "above", []string{"clarify-choices", "clarify-freetext"}},
	{"model_picker_widget", "above", []string{"model-picker"}},
	{"command_palette_widget", "above", []string{"palette"}},
	{"spinner_widget", "above", []string{"busy"}},
	{"spacer", "above", []string{"approval"}},
	{"_get_extra_tui_widgets", "above", nil},
	{"_pet_widget", "above", nil},
	{"_stash_panel_widget", "above", []string{"stash", "stash-indicator-hidden"}},
	{"_subagent_dock_widget", "above", nil},
	{"status_bar", "status", []string{"idle", "vi-insert", "vi-trimmed"}},
	{"input_rule_top", "rule", []string{"idle"}},
	{"image_bar", "image", []string{"image-attached"}},
	{"input_area", "input", []string{"idle", "text-in-box"}},
	{"input_rule_bot", "rule", []string{"idle", "narrow"}},
	{"voice_status_bar", "below", []string{"voice", "voice-recording", "voice-processing"}},
	{"completions_menu", "below", []string{"completions-menu"}},
}

// promptConditions is every state _get_tui_prompt_fragments branches on, in
// its order (P:hermes_cli/cli_tui_mixin.py:309-346): each changes the input
// line's prefix, which the screen check refuses on (NotIdle; the voice
// states also draw the voice bar below the input, Below).
var promptConditions = []struct {
	Attr, Prefix string
	Fixtures     []string
}{
	{"_voice_recording", "●", []string{"voice-recording"}},
	{"_voice_processing", "◉", []string{"voice-processing"}},
	{"_sudo_state", "🔐", []string{"sudo"}},
	{"_secret_state", "🔑", []string{"secret"}},
	{"_connection_state", "⚙", []string{"connection"}},
	{"_approval_state", "⚠", []string{"approval"}},
	{"_slash_confirm_state", "⚠", []string{"new-confirm"}},
	{"_clarify_freetext", "✎", []string{"clarify-freetext"}},
	{"_clarify_state", "?", []string{"clarify-choices"}},
	{"_command_running", "a spinner frame", nil},
	{"_agent_running", "☤", []string{"busy"}},
	{"_voice_mode", "🎤", []string{"voice"}},
}

// statusBarBlanking is every condition under which _get_status_bar_fragments
// renders NOTHING (P:hermes_cli/cli_status_bar_mixin.py:1128-1133): the
// screen check's NoStatusBar refusal of the palette and the model picker
// rests on exactly these.
var statusBarBlanking = []string{"_status_bar_visible", "_model_picker_state", "_command_palette_state"}

// chromeFilters is every attribute the chrome's ConditionalContainer filters
// read, from the voice bar to the stash panel (P:hermes_cli/cli_tui_mixin.py:
// 2165-2184): the voice bar below the input (Below), the status bar's
// visibility and its post-resize suppression (NoStatusBar / NoLayout), and
// the stash panel's open flag (its frame: Stash).
var chromeFilters = []string{"_voice_mode", "_status_bar_visible", "_status_bar_suppressed_after_resize", "_prompt_stash.panel_open", "_prompt_stash"}

// TestOverlayLedgerNamesRecordedFixtures: every fixture the ledger names is
// recorded (its .ansi) and expected (screens.json), and every overlay state
// has at least one.
func TestOverlayLedgerNamesRecordedFixtures(t *testing.T) {
	exp := screenExpectations(t)
	check := func(owner string, names []string) {
		for _, n := range names {
			if _, ok := exp[n]; !ok {
				t.Errorf("%s names fixture %q, which screens.json does not list", owner, n)
			}
			if _, err := os.Stat(filepath.Join("testdata", "screens", n+".ansi")); err != nil {
				t.Errorf("%s names fixture %q, which is not recorded: %v", owner, n, err)
			}
		}
	}
	for _, s := range overlayStates {
		if len(s.Fixtures) == 0 {
			t.Errorf("overlay state %s has no fixture", s.Attr)
		}
		check(s.Attr, s.Fixtures)
	}
	for _, c := range layoutChildren {
		check(c.Name, c.Fixtures)
	}
	for _, c := range promptConditions {
		check(c.Attr, c.Fixtures)
	}
}

// screenExpectation is screens.json's entry for a fixture.
type screenExpectation struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	How    string `json:"how"`
	Expect string `json:"expect"`
	Rule   string `json:"rule"`
	// Pasted, for a fixture after a paste: the text and CheckPasted's
	// expected answer.
	Pasted       string `json:"pasted,omitempty"`
	PastedExpect string `json:"pastedExpect,omitempty"`
}

func screenExpectations(t *testing.T) map[string]screenExpectation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "screens", "screens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []screenExpectation
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]screenExpectation{}
	for _, e := range list {
		if _, dup := out[e.Name]; dup {
			t.Fatalf("screens.json lists %q twice", e.Name)
		}
		out[e.Name] = e
	}
	if len(out) < 20 {
		t.Fatalf("instrument check: screens.json lists %d fixtures", len(out))
	}
	return out
}
