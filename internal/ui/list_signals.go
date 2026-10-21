package ui

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// WHAT A CARD SAYS ABOUT ITS RECENCY, WHAT IT IS WAITING ON, AND HOW LONG ITS
// TITLE MAY BE. Each is decided here, once, as a Go table or function; the
// cards render from it and the list's scripts are handed it as JSON, so the
// page, the script and the tests read one definition.

// ---------------------------------------------------------------------------
// RECENCY: a small dot beside "active … ago", tinted by how long ago that was.
//
// The palette and the thresholds are the operator's tmux idle-fade window
// colours — the 8-step Gruvbox scale their tmux status bar tints each window
// with (the "Idle-Fade Window Colors" block of their tmux config and the
// tmux-idle-update.sh script it runs: `idle_secs < 600` bright green, … , else
// grey). Mirroring it means a window reads the same colour in tmux and here.
// The colours are Gruvbox's published palette; nothing in the table is private.
//
// 🔴 THE DOT IS NOT THE CARD'S BORDER. The border is reserved for a per-window
// tmux identity colour (a separate design); recency must not claim it.
//
// The dot is decoration (aria-hidden): the words beside it ("active 3m ago")
// carry the same fact, so colour is never the only signal.

// recencyStep is one bucket: an entry idle for less than Under (0 = no upper
// bound, the last bucket) is drawn with Class.
type recencyStep struct {
	Under time.Duration
	// Class is the dot's colour utility: a role token, bg-idle-<i>, whose
	// value is tmux-idle-update.sh's colour for the bucket, defined once in
	// web/css/input.css (--mu-idle-<i>, both themes) — this package names no
	// hue (TestNoHardCodedColourInUI). TestRecencyTokensAreTheTmuxPalette holds
	// those values to the script's.
	Class string
	// Name is the script's own word for the bucket: the dot's tooltip.
	Name string
}

// recencySteps is the scale, freshest first. Its boundaries are the script's
// `(( idle_secs < N ))` tests, exclusive, in seconds: 600, 1800, 3600, 7200,
// 14400, 28800, 86400 — and its colours, in order: bright green, green, aqua,
// yellow, orange, red, purple, grey.
var recencySteps = []recencyStep{
	{10 * time.Minute, "bg-idle-0", "fresh (under 10 min)"},
	{30 * time.Minute, "bg-idle-1", "active (10–30 min)"},
	{time.Hour, "bg-idle-2", "warm (30–60 min)"},
	{2 * time.Hour, "bg-idle-3", "cooling (1–2 h)"},
	{4 * time.Hour, "bg-idle-4", "idle (2–4 h)"},
	{8 * time.Hour, "bg-idle-5", "stale (4–8 h)"},
	{24 * time.Hour, "bg-idle-6", "dormant (8–24 h)"},
	{0, "bg-idle-7", "ancient (over 24 h)"},
}

// recencyBucket is the index in recencySteps of an entry last active at t, as
// of now; -1 for a zero t (unknown: no dot). A t in the future (clock skew) is
// the freshest bucket.
func recencyBucket(now, t time.Time) int {
	if t.IsZero() {
		return -1
	}
	d := now.Sub(t)
	for i, s := range recencySteps {
		if s.Under == 0 || d < s.Under {
			return i
		}
	}
	return len(recencySteps) - 1
}

// recencyDotClass is the dot's class for a bucket: the fixed box (so a
// re-tint never moves anything) and the bucket's colour.
func recencyDotClass(bucket int) string {
	return "inline-block h-2 w-2 shrink-0 rounded-full " + recencySteps[bucket].Class
}

// recencyDot is the dot for an entry last active at t, or nothing for a zero
// t (cardActivity prints no "active" then either). data-recency-ts lets the
// list's script re-tint it as time passes without a re-render.
func recencyDot(now, t time.Time) g.Node {
	b := recencyBucket(now, t)
	if b < 0 {
		return nil
	}
	return Span(
		g.Attr("data-recency", strconv.Itoa(b)),
		g.Attr("data-recency-ts", strconv.FormatInt(t.UnixMilli(), 10)),
		g.Attr("aria-hidden", "true"),
		g.Attr("title", recencySteps[b].Name),
		Class(recencyDotClass(b)),
	)
}

// recencyScaleJSON is the scale as the script reads it: [{u: upper bound in
// ms (0 = none), c: the dot's full class, n: its name}, …].
func recencyScaleJSON() string {
	type step struct {
		U int64  `json:"u"`
		C string `json:"c"`
		N string `json:"n"`
	}
	out := make([]step, len(recencySteps))
	for i, s := range recencySteps {
		out[i] = step{s.Under.Milliseconds(), recencyDotClass(i), s.Name}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ---------------------------------------------------------------------------
// IDLE: how long since the entry's last activity (listEntry.activeAt — the time
// the list is ordered by and the card prints). A card carries one token per
// threshold it is PAST, in `data-idle` ("15m 1h" for a card idle two hours),
// so a filter chip is one attribute selector ([data-idle~="1h"]).

// idleThreshold is one of the filter's idle cut-offs.
type idleThreshold struct {
	Token string
	Over  time.Duration
}

// idleThresholds are the idle chips' cut-offs: idle MORE than Over.
var idleThresholds = []idleThreshold{
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
	{"1d", 24 * time.Hour},
}

// idleTokens is the `data-idle` value of an entry last active at t: every
// threshold it is strictly past, space-separated, "" when none — and "" for a
// zero t, so an entry whose activity is unknown never counts as idle.
func idleTokens(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	var out []string
	for _, th := range idleThresholds {
		if d > th.Over {
			out = append(out, th.Token)
		}
	}
	return strings.Join(out, " ")
}

// idleThresholdsJSON is idleThresholds as the script reads it: [{t, ms}, …].
func idleThresholdsJSON() string {
	type th struct {
		T  string `json:"t"`
		Ms int64  `json:"ms"`
	}
	out := make([]th, len(idleThresholds))
	for i, x := range idleThresholds {
		out[i] = th{x.Token, x.Over.Milliseconds()}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// cardActivityAttrs is a card's recency attributes: data-active-ts (ms; what
// the script re-derives the idle tokens and the dot from) and data-idle.
func cardActivityAttrs(now, t time.Time) g.Node {
	if t.IsZero() {
		return nil
	}
	return g.Group{
		g.Attr("data-active-ts", strconv.FormatInt(t.UnixMilli(), 10)),
		g.If(idleTokens(now, t) != "", g.Attr("data-idle", idleTokens(now, t))),
	}
}

// ---------------------------------------------------------------------------
// WAITING: what the card is blocked on the operator for, from its attention
// badge (CardAttention, composed in internal/api from the attention queue's
// ONE read — never re-derived here). Two words, written into `data-wait`:
//
//	ask       muster's attention_ask (the queue's "Question" row): a dispatched
//	          agent or a mirrored Claude session called attention_ask and is
//	          waiting for your answer — OR a mirrored Claude Code session is
//	          stopped at a prompt (status "waiting": a permission prompt, the
//	          session ASKING to go on; the queue's "Session waiting").
//	question  Claude Code's own AskUserQuestion picker, held open by a mirrored
//	          session (the queue's "Session question"): it is asking YOU a
//	          question with options, answered in the session.
//
// A reply draft (kind "plugin") is neither: nothing is blocked on it. A
// dispatched agent's "awaiting approval" badge is not an attention item and
// is not counted either.

// waitKinds maps each attention kind a card can count to the wait word it
// files under ("" = none).
var waitKinds = map[string]string{
	"ask":      "ask",
	"session":  "ask",
	"question": "question",
	"plugin":   "",
}

// waitTokens is a card's `data-wait`: the wait words of its attention items'
// kinds, in a fixed order ("ask question"), "" for none.
func waitTokens(a *CardAttention) string {
	if a == nil || a.Count <= 0 {
		return ""
	}
	has := map[string]bool{}
	for _, k := range a.Kinds {
		if w := waitKinds[k]; w != "" {
			has[w] = true
		}
	}
	var out []string
	for _, w := range []string{"ask", "question"} {
		if has[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// ---------------------------------------------------------------------------
// THE FILTER CHIPS: presets above the list.
//
// SEMANTICS, stated once: the WAITING chips are ORed with each other (a card
// waiting on an ask OR a question); an IDLE chip is ANDed with them (waiting
// AND idle past the threshold); the idle chips are exclusive (choosing one
// clears the others — they are nested, so two would AND to the larger). No
// chip chosen: no filter. The chips AND with the search box and the host tab
// like any other term.

// listChip is one chip.
type listChip struct {
	ID, Label, Help string
	// Group is "wait" or "idle"; Token is the word the card's data-<group>
	// carries for it.
	Group, Token string
}

var listChips = []listChip{
	{"ask", "Waiting on ask", "An agent or a session asked you something with attention_ask, or a Claude Code session is stopped at a permission prompt", "wait", "ask"},
	{"question", "Waiting on question", "A Claude Code session is holding an AskUserQuestion picker open for you", "wait", "question"},
	{"idle-15m", "Idle >15m", "No activity for more than 15 minutes", "idle", "15m"},
	{"idle-1h", "Idle >1h", "No activity for more than an hour", "idle", "1h"},
	{"idle-1d", "Idle >1d", "No activity for more than a day", "idle", "1d"},
}

// chipByID is the chip with that id.
func chipByID(id string) (listChip, bool) {
	for _, c := range listChips {
		if c.ID == id {
			return c, true
		}
	}
	return listChip{}, false
}

// chipKey is the canonical key of a selection: its known ids in listChips'
// order, comma-joined. The script looks a selection's selector up by it.
func chipKey(ids []string) string {
	on := map[string]bool{}
	for _, id := range ids {
		on[id] = true
	}
	var out []string
	for _, c := range listChips {
		if on[c.ID] {
			out = append(out, c.ID)
		}
	}
	return strings.Join(out, ",")
}

// chipPasses is THE predicate: whether a card whose data-wait is wait and
// whose data-idle is idle (space-separated words) is shown under the chips
// ids. chipSelector is the same rule as a CSS selector; a test holds the two
// to each other over every selection.
func chipPasses(ids []string, wait, idle string) bool {
	waits, idles := strings.Fields(wait), strings.Fields(idle)
	wantWait, anyWait := false, false
	for _, id := range ids {
		c, ok := chipByID(id)
		if !ok {
			continue
		}
		switch c.Group {
		case "wait":
			wantWait = true
			if slices.Contains(waits, c.Token) {
				anyWait = true
			}
		case "idle":
			if !slices.Contains(idles, c.Token) {
				return false
			}
		}
	}
	return !wantWait || anyWait
}

// chipSelector is the selector a SHOWN card matches under the chips ids
// ("" = no chip, nothing to match): the wait chips as one :is() list, each
// idle chip one more compound part.
func chipSelector(ids []string) string {
	var waits, idles []string
	for _, id := range strings.Split(chipKey(ids), ",") {
		c, ok := chipByID(id)
		if !ok {
			continue
		}
		part := `[data-` + c.Group + `~="` + c.Token + `"]`
		if c.Group == "wait" {
			waits = append(waits, part)
		} else {
			idles = append(idles, part)
		}
	}
	sel := ""
	switch len(waits) {
	case 0:
	case 1:
		sel = waits[0]
	default:
		sel = ":is(" + strings.Join(waits, ",") + ")"
	}
	return sel + strings.Join(idles, "")
}

// chipSelectorsJSON is every selection the chips can make — any set of wait
// chips with at most one idle chip — as {key: selector}: the script's whole
// filter logic is a lookup in it, so the rule lives here only.
func chipSelectorsJSON() string {
	var waits, idles []string
	for _, c := range listChips {
		if c.Group == "wait" {
			waits = append(waits, c.ID)
		} else {
			idles = append(idles, c.ID)
		}
	}
	out := map[string]string{}
	for mask := 0; mask < 1<<len(waits); mask++ {
		var ids []string
		for i, w := range waits {
			if mask&(1<<i) != 0 {
				ids = append(ids, w)
			}
		}
		for _, idle := range append([]string{""}, idles...) {
			sel := ids
			if idle != "" {
				sel = append(append([]string{}, ids...), idle)
			}
			if k := chipKey(sel); k != "" {
				out[k] = chipSelector(sel)
			}
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// agentsChipsStorageKey is where this device remembers the chosen chips
// (localStorage, tabs.go's prefix): a JSON array of chip ids.
const agentsChipsStorageKey = "muster:agents:chips"

// agentsFilterChips is the chip row: one toggle button per chip
// (aria-pressed), drawn unpressed; the script presses the remembered ones.
// It is ONE 44px row whatever is pressed — pressing changes colour, never
// size — and scrolls sideways inside itself at a phone's width, so it neither
// shifts the list nor scrolls the page.
func agentsFilterChips() g.Node {
	return Div(
		ID("agents-chips"),
		g.Attr("role", "group"),
		g.Attr("aria-label", "Quick filters"),
		Class("flex min-h-[44px] min-w-0 max-w-full flex-nowrap items-center gap-1.5 overflow-x-auto overflow-y-hidden overscroll-x-contain [scrollbar-width:none]"),
		g.Group(g.Map(listChips, func(c listChip) g.Node {
			return Button(
				Type("button"),
				g.Attr("data-chip", c.ID),
				g.Attr("data-chip-group", c.Group),
				g.Attr("aria-pressed", "false"),
				g.Attr("aria-controls", "agents-list"),
				g.Attr("title", c.Help),
				Class("press inline-flex min-h-[44px] shrink-0 items-center whitespace-nowrap rounded-full bg-s2 px-3 text-sm font-medium text-fg2 ring-1 ring-inset ring-line transition hover:text-fg aria-pressed:bg-accent aria-pressed:text-on-accent aria-pressed:ring-accent"),
				g.Text(c.Label),
			)
		})),
	)
}

// ---------------------------------------------------------------------------
// LONG TITLES: a card's name is clamped to cardTitleMax characters; past that
// it ends in a "…" BUTTON that opens the whole title in a popover. The button
// sits OUTSIDE the card's nav anchor (a button inside an <a> is invalid and its
// tap would open the card), so a card with a long name draws its name in a
// title row of its own above the anchor — cardLongTitle — exactly as a focus
// title does (cardFocusTitle). A short name is drawn as it always was.
//
// ⚠ ONLY THE NAME. A focus title is shown whole by design (its card heading is
// already the button that opens its popover); this clamps what the card is
// CALLED.

// cardTitleMax is the most characters (runes) of a name a card draws.
const cardTitleMax = 60

// clampTitle is s as a card draws it: whole when it fits, else its first
// cardTitleMax runes (trailing space trimmed) and true.
func clampTitle(s string) (string, bool) {
	r := []rune(s)
	if len(r) <= cardTitleMax {
		return s, false
	}
	return strings.TrimRight(string(r[:cardTitleMax]), " \t"), true
}

// cardLongTitle is a long name's title row: the clamped name, itself a link to
// the card's page (so tapping the words still opens the card), and the "…"
// button that opens the full name in a popover instead. The popover is the
// focus popover's: native popover="auto" (an outside tap or Escape closes it),
// placed by floatingMenuScript, which also keeps the button's aria-expanded in
// step. title= on the link is the desktop's hover tooltip.
//
// subtitle draws it as a subtitle (a card headed by its focus); extra is drawn
// after the name inside the link (a mirrored window's qualifier and session).
func cardLongTitle(cardID, href, full, short string, subtitle bool, reserve string, extra ...g.Node) g.Node {
	pop := cardID + "-title"
	class, pad := cardTitleClass, "pt-3"
	if subtitle {
		class, pad = cardSubtitleClass, "pt-1"
	}
	return Div(
		cardSlot("title"),
		Class("flex items-start gap-0.5 px-4 "+pad+" "+reserve),
		A(
			Href(href),
			hx("hx-boost", "false"),
			g.Attr("data-card-title", ""),
			g.Attr("title", full),
			Class(strings.Replace(class, "mr-auto ", "min-w-0 py-1 ", 1)),
			g.Text(short),
			g.Group(extra),
		),
		Button(
			Type("button"),
			g.Attr("data-title-more", ""),
			g.Attr("popovertarget", pop),
			g.Attr("aria-haspopup", "dialog"),
			g.Attr("aria-controls", pop),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", "Show the full title"),
			Class("press -my-1.5 inline-flex h-11 min-w-[44px] shrink-0 items-start justify-center rounded-lg pt-1.5 text-base font-semibold leading-tight text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus aria-expanded:bg-s2"),
			g.Text("…"),
		),
		Div(
			ID(pop),
			g.Attr("popover", "auto"),
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Full title"),
			g.Attr("data-floating-menu", ""),
			g.Attr("data-title-full", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-72 max-w-[calc(100vw-16px)] flex-col gap-1.5 rounded-xl border border-line bg-s1 p-3 text-fg shadow-2xl ring-1 ring-black/20"),
			Span(Class("break-words text-sm font-semibold"), g.Text(full)),
		),
	)
}

// titledBodyClass is the nav anchor's box under a title row: like a focus
// row, the row carries the top padding.
func titledBodyClass(reserve string) string {
	return "flex flex-col gap-3 px-4 pb-4 pt-1 " + reserve
}

// chipIDsJSON is the chips' ids in their canonical order, for the script.
func chipIDsJSON() string {
	ids := make([]string, len(listChips))
	for i, c := range listChips {
		ids[i] = c.ID
	}
	b, _ := json.Marshal(ids)
	return string(b)
}
