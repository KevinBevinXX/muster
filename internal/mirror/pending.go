package mirror

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pending is WHAT a waiting Claude Code session is waiting on, as its host
// read it from the session's transcript (internal/mirrorhost's PendingOf):
// what the attention queue shows instead of a generic "waiting" line.
//
//   - Summary is the pending action in one short line: the last tool call that
//     has no result yet ("Bash: npm publish", "Edit internal/api/x.go", the
//     question an AskUserQuestion asks), "" when the transcript shows none.
//   - Last is the last thing Claude said (its last text), whitespace-collapsed.
//
// 🔴 TEXT FROM THE SESSION, SHOWN ON A PHONE: both fields are capped in RUNES
// (MaxPendingSummary, MaxPendingLast), stripped of every control character and
// whitespace-collapsed (CleanPending) — by the host before sending and by muster
// again on receipt — and never carry a tool's raw input beyond that.
//
// 🔴 BOUNDED LIST-WIDE, LIKE OtherClaude: only a window that is Claude and
// StatusWaiting keeps one, and at most MaxPendingWindows windows of a list
// (the most recently waiting) carry one, so the window list's worst case grows
// by a fixed ~16 KiB of the frame's last few percent: 1,028,215 of 1,048,576
// bytes with it (TestTheLargestWindowListFitsOneFrame encodes it at its bound).
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster ignores
// the field (unknown JSON key); an older host never sends it, and muster then
// shows its own generic line.
type Pending struct {
	Summary string `json:"summary,omitempty"`
	Last    string `json:"last,omitempty"`
}

// The Pending bounds.
const (
	// MaxPendingSummary bounds Pending.Summary, in runes.
	MaxPendingSummary = 120
	// MaxPendingLast bounds Pending.Last, in runes.
	MaxPendingLast = 200
	// MaxPendingWindows bounds how many windows of ONE list carry a Pending.
	MaxPendingWindows = 8
)

// CleanPending is p as the wire carries it: each field with every control
// character (C0, DEL, C1, and the Unicode line/paragraph separators) turned
// into a space, invalid UTF-8 dropped, whitespace collapsed, and capped at its
// rune bound (a cut ends "…"). nil when nothing is left of either field.
func CleanPending(p *Pending) *Pending {
	if p == nil {
		return nil
	}
	out := Pending{Summary: cleanLine(p.Summary, MaxPendingSummary), Last: cleanLine(p.Last, MaxPendingLast)}
	if out.Summary == "" && out.Last == "" {
		return nil
	}
	return &out
}

// cleanLine is s on one line: control characters as spaces, whitespace
// collapsed, at most max runes.
func cleanLine(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = strings.TrimRight(string([]rune(s)[:max-1]), " ") + "…"
	}
	return s
}

// boundPending applies the Pending rules to a normalised list: none on a
// window that is not a waiting Claude pane, each cleaned, and at most
// MaxPendingWindows kept — the windows that started waiting most recently,
// then list order — so which keep theirs does not depend on the host's order.
func boundPending(out []Window) {
	var with []int
	for i := range out {
		if !out[i].Claude || out[i].Status != StatusWaiting {
			out[i].Pending = nil
			continue
		}
		if out[i].Pending = CleanPending(out[i].Pending); out[i].Pending != nil {
			with = append(with, i)
		}
	}
	if len(with) <= MaxPendingWindows {
		return
	}
	sort.SliceStable(with, func(a, b int) bool { return out[with[a]].StatusSince > out[with[b]].StatusSince })
	for _, i := range with[MaxPendingWindows:] {
		out[i].Pending = nil
	}
}

// pendingEqual reports whether a and b carry the same Pending.
func pendingEqual(a, b *Pending) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// OneLine is s as one short, phone-safe line: control characters as spaces,
// whitespace collapsed, at most max runes (a cut ends "…"). The rule
// CleanPending applies to each field, for any other excerpt shown beside it.
func OneLine(s string, max int) string { return cleanLine(s, max) }
