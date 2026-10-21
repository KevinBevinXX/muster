package mirror

import "regexp"

// LIVE WORK COUNTS AND THE IDENTITY COLOUR (docs/design/agent-cards.md (a) and
// (b)): what a mirrored window's card says about the subagents and background
// shells its Claude Code session has running NOW, and the colour the host's
// tmux config gives the window.
//
// 🔴 BOTH TRAVEL ON MESSAGES OF THEIR OWN (TypeWork, TypeColors), NOT ON
// Window, exactly as the repo chip does (repos.go): the window list's worst
// case already fills 98% of MaxFrame (TestTheLargestWindowListFitsOneFrame),
// and `,"subagents":64,"shells":64` alone is 13.8 KB at MaxWindows. Each is
// the WHOLE map of window id → value, sent after the window list whenever it
// changed; an empty map clears it. Their worst cases are measured by
// TestTheLargestWorkAndColorsMessagesFitOneFrame (3.1% and 1.2% of MaxFrame).
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster's read
// loop has no case for either type and drops the message; an older host never
// sends one, and its cards carry no chip and no tint — they render as they did.
//
// 🔴 ABSENT MEANS "NOTHING TO SHOW", whether that is zero or unknown: muster
// never shows a count the host did not send.

// TypeWork is the host → muster message carrying Msg.Work.
const TypeWork = "work"

// TypeColors is the host → muster message carrying Msg.Colors.
const TypeColors = "colors"

// MaxWorkCount bounds each count; the card shows "99+" at it.
const MaxWorkCount = 99

// WindowWork is what one window's Claude Code session has running now.
//
// ShellsAtLeast: Shells is a LOWER BOUND ("1+ shell"), never an exact count.
// It is what the host reports whenever a foreground command could be among
// the shells it counted — any running turn (docs/design/agent-cards.md,
// operator decision 3): the process tree cannot yet tell a foreground Bash
// call from a background one.
type WindowWork struct {
	Subagents     int  `json:"subagents,omitempty"`
	Shells        int  `json:"shells,omitempty"`
	ShellsAtLeast bool `json:"shellsAtLeast,omitempty"`
}

// IsZero: nothing to show.
func (w WindowWork) IsZero() bool { return w.Subagents == 0 && w.Shells == 0 && !w.ShellsAtLeast }

// normalizeWork is one entry as the wire carries it: each count clamped to
// [0, MaxWorkCount], and a lower bound that is at least 1 (a "0+ shells" says
// nothing).
func normalizeWork(w WindowWork) WindowWork {
	clamp := func(n int) int { return min(max(n, 0), MaxWorkCount) }
	w.Subagents, w.Shells = clamp(w.Subagents), clamp(w.Shells)
	if w.ShellsAtLeast && w.Shells < 1 {
		w.Shells = 1
	}
	return w
}

// NormalizeWork is in restricted to windows (a host cannot report work for a
// window it does not list), each entry through normalizeWork, entries with
// nothing to show dropped; nil when nothing is left. Both halves apply it: the
// host to what it sends, muster to what it keeps. windows must be
// NormalizeWindows' output, so the result has at most MaxWindows entries.
func NormalizeWork(in map[string]WindowWork, windows []Window) map[string]WindowWork {
	if len(in) == 0 {
		return nil
	}
	var out map[string]WindowWork
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok {
			continue
		}
		if v = normalizeWork(v); v.IsZero() {
			continue
		}
		if out == nil {
			out = map[string]WindowWork{}
		}
		out[w.ID] = v
	}
	return out
}

// colorPattern is the only colour the wire carries: lowercase #rrggbb.
var colorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// ValidColor is whether c is a colour the wire carries.
func ValidColor(c string) bool { return colorPattern.MatchString(c) }

// NormalizeColors is in restricted to windows, each value exactly a lowercase
// `#rrggbb` (anything else is dropped, never repaired: the host lowercases
// before it sends); nil when nothing is left. Both halves apply it.
func NormalizeColors(in map[string]string, windows []Window) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok || !ValidColor(v) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[w.ID] = v
	}
	return out
}
