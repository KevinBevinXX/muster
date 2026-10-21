package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE LIVE WORK CHIP (docs/design/agent-cards.md (a)): how many subagents and
// background shells a mirrored window's Claude Code session has running NOW,
// as its host last reported them (mirror.WindowWork) — "2 subagents · 1 shell",
// "1 subagent", "1+ shell".
//
// 🔴 "1+" IS A LOWER BOUND, AND SAYS SO. While a turn may be running the host
// cannot yet tell a foreground command from a background shell, so it reports
// ShellsAtLeast; the chip then says "1+ shell", never a number it does not
// know. No chip at all when both counts are zero, or when the host did not
// say (an older muster-mirror): absent means nothing to show.

// workCount is n for the chip, "99+" at the wire's bound.
func workCount(n int) string {
	if n >= mirror.MaxWorkCount {
		return strconv.Itoa(mirror.MaxWorkCount) + "+"
	}
	return strconv.Itoa(n)
}

// noun is one or many, by n.
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// workParts is the chip's two halves, each "" when it has nothing to say.
func workParts(w mirror.WindowWork) (subagents, shells string) {
	if w.Subagents > 0 {
		subagents = workCount(w.Subagents) + " " + noun(w.Subagents, "subagent", "subagents")
	}
	if w.Shells > 0 || w.ShellsAtLeast {
		n := max(w.Shells, 1)
		c := workCount(n)
		if w.ShellsAtLeast && !strings.HasSuffix(c, "+") {
			c += "+"
		}
		shells = c + " " + noun(n, "shell", "shells")
	}
	return subagents, shells
}

// workChipText is what the chip reads: "2 subagents · 1 shell".
func workChipText(w mirror.WindowWork) string {
	a, b := workParts(w)
	switch {
	case a != "" && b != "":
		return a + " · " + b
	case a != "":
		return a
	}
	return b
}

// workChipLabel is the chip's text for a screen reader: "2 subagents and 1
// background shell running", "at least 1 background shell running".
func workChipLabel(w mirror.WindowWork) string {
	var parts []string
	if w.Subagents > 0 {
		parts = append(parts, workCount(w.Subagents)+" "+noun(w.Subagents, "subagent", "subagents"))
	}
	if w.Shells > 0 || w.ShellsAtLeast {
		n := max(w.Shells, 1)
		s := workCount(n) + " background " + noun(n, "shell", "shells")
		if w.ShellsAtLeast && n < mirror.MaxWorkCount {
			s = "at least " + s
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " and ") + " running"
}

// workChip is the chip, or nothing when there is nothing running to show. It
// wears chip()'s classes (no new CSS) without chip()'s muted label.
func workChip(w mirror.WindowWork) g.Node {
	text := workChipText(w)
	if text == "" {
		return g.Group(nil)
	}
	return Span(
		g.Attr("data-mirror-work", ""),
		g.Attr("title", workChipLabel(w)),
		Class("inline-flex items-center gap-1 rounded-full bg-s2/80 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		Span(g.Attr("aria-hidden", "true"), g.Text(text)),
		Span(Class("sr-only"), g.Text(workChipLabel(w))),
	)
}
