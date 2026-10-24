package suggest

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ZacxDev/muster/internal/focus"
)

// THE OFFLINE EVAL SET: SYNTHETIC conversations (testdata/eval/fixtures.yaml),
// each with the properties a useful, grounded set must have. Two layers read
// it:
//   - (a) TestTheEvalFixtures, always, in CI, with no key: every fixture's
//     input is built exactly as the server builds it, its pre-gate verdict and
//     its redaction are checked, and its canned answer (a synthetic model
//     answer, good and bad items mixed) goes through Filter;
//   - (b) cmd/muster-suggest-eval -live, by hand only: the real model on
//     every fixture, scored by Score, with latency and cost.
//
// 🔴 SYNTHETIC ONLY. This is a public repository: no fixture may hold a real
// transcript, name, host or credential.

// Fixture is one synthetic conversation and what a good set for it must do.
type Fixture struct {
	Name string `yaml:"name"`
	// FocusHistory is the session's focus over time, oldest first, each
	// written AgoMinutes before the fixture's fixed now.
	FocusHistory []struct {
		Title       string `yaml:"title"`
		Description string `yaml:"description"`
		AgoMinutes  int    `yaml:"ago_minutes"`
	} `yaml:"focus_history"`
	Asks []string       `yaml:"asks"`
	Tail []FixtureEntry `yaml:"tail"`
	// HandsBack is the pre-gate's expected verdict on the final reply; a
	// fixture that does not hand back is never sent to the model.
	HandsBack bool   `yaml:"hands_back"`
	Expect    Expect `yaml:"expect"`
	// Canned is a synthetic model answer for the offline layer, and
	// CannedKept how many of its items must survive Filter.
	Canned     []Raw `yaml:"canned"`
	CannedKept int   `yaml:"canned_kept"`
}

// FixtureEntry is one line of a fixture's conversation. A tool call names
// Tool and Input (the call's argument JSON); Output is its result, which the
// input must NEVER carry (it is there to prove that).
type FixtureEntry struct {
	Role   string `yaml:"role"`
	Text   string `yaml:"text"`
	Tool   string `yaml:"tool"`
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

// Expect is what a fixture's surviving set must satisfy.
type Expect struct {
	// MinKept: at least this many survivors.
	MinKept int `yaml:"min_kept"`
	// AnyKind: at least one survivor is one of these kinds.
	AnyKind []string `yaml:"any_kind"`
	// AnyContains: at least one survivor contains one of these (any case).
	AnyContains []string `yaml:"any_contains"`
	// CitesAsk: at least one survivor cites this ask id ("A3").
	CitesAsk string `yaml:"cites_ask"`
	// Forbid: no survivor contains any of these (any case) — the injected
	// action, the option the operator ruled out.
	Forbid []string `yaml:"forbid"`
}

// LoadFixtures reads a fixtures file.
func LoadFixtures(path string) ([]Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fs []Fixture
	if err := yaml.Unmarshal(raw, &fs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return fs, nil
}

// fixtureNow is the fixtures' fixed clock (a year-2000 date: synthetic).
var fixtureNow = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

// Input is the fixture as the server would hand it to Build: a tool call
// reduced by focus.ToolLine (name and file path), its output dropped.
func (f Fixture) Input() Input {
	in := Input{Asks: f.Asks, Now: fixtureNow}
	for _, h := range f.FocusHistory {
		in.Focus = append(in.Focus, FocusEntry{Title: h.Title, Description: h.Description,
			At: fixtureNow.Add(-time.Duration(h.AgoMinutes) * time.Minute)})
	}
	for _, e := range f.Tail {
		switch e.Role {
		case RoleTool:
			in.Tail = append(in.Tail, Entry{Role: RoleTool, Text: focus.ToolLine(e.Tool, e.Input)})
		default:
			in.Tail = append(in.Tail, Entry{Role: e.Role, Text: e.Text})
		}
	}
	return in
}

// Secrets is every string of the fixture's conversation that must never reach
// the model: tool outputs, and every argument of a tool call but its file path
// (a command, a body, a pattern).
func (f Fixture) Secrets() []string {
	var out []string
	for _, e := range f.Tail {
		if e.Role != RoleTool {
			continue
		}
		if e.Output != "" {
			out = append(out, e.Output)
		}
		var args map[string]any
		if json.Unmarshal([]byte(e.Input), &args) == nil {
			for k, v := range args {
				if str, ok := v.(string); ok && len(str) >= 6 && k != "file_path" && k != "notebook_path" && k != "path" {
					out = append(out, str)
				}
			}
		}
	}
	return out
}

// Result is one fixture scored.
type Result struct {
	Fixture string
	// Properties maps each property checked to whether it held.
	Properties map[string]bool
	// Kept is how many survived; Raw how many the model wrote; Drops the
	// reasons of the ones dropped.
	Kept, Raw int
	Drops     []Drop
}

// Pass reports whether every property held.
func (r Result) Pass() bool {
	for _, ok := range r.Properties {
		if !ok {
			return false
		}
	}
	return true
}

// Score checks a fixture's survivors against its Expect. raw is the model's
// answer; b the built input.
func Score(f Fixture, b Built, raw []Raw) Result {
	kept, dropped := Filter(raw, b)
	res := Result{Fixture: f.Name, Properties: map[string]bool{}, Kept: len(kept), Raw: len(raw)}
	for _, d := range dropped {
		res.Drops = append(res.Drops, d.Reason)
	}
	// The raw item each survivor came from (for its cites).
	rawOf := func(s Suggestion) Raw {
		for _, r := range raw {
			if strings.TrimSpace(r.Text) == s.Text && r.Kind == s.Kind {
				return r
			}
		}
		return Raw{}
	}
	ex := f.Expect
	res.Properties["min_kept"] = len(kept) >= ex.MinKept
	if len(ex.AnyKind) > 0 {
		ok := false
		for _, s := range kept {
			for _, k := range ex.AnyKind {
				ok = ok || s.Kind == k
			}
		}
		res.Properties["any_kind"] = ok
	}
	if len(ex.AnyContains) > 0 {
		ok := false
		for _, s := range kept {
			for _, w := range ex.AnyContains {
				ok = ok || strings.Contains(strings.ToLower(s.Text), strings.ToLower(w))
			}
		}
		res.Properties["any_contains"] = ok
	}
	if ex.CitesAsk != "" {
		ok := false
		for _, s := range kept {
			for _, c := range rawOf(s).Cites {
				ok = ok || c == ex.CitesAsk
			}
		}
		res.Properties["cites_ask"] = ok
	}
	if len(ex.Forbid) > 0 {
		ok := true
		for _, s := range kept {
			for _, w := range ex.Forbid {
				if strings.Contains(strings.ToLower(s.Text), strings.ToLower(w)) {
					ok = false
				}
			}
		}
		res.Properties["forbid"] = ok
	}
	// The global properties every survivor has (Filter guarantees them; they
	// are checked here so a filter regression shows in the eval too).
	shape := true
	for _, s := range kept {
		if startsSlashOrBang(s.Text) || strings.ContainsAny(s.Text, "\r\n") || isStock(s.Text) {
			shape = false
		}
	}
	res.Properties["shape"] = shape
	return res
}
