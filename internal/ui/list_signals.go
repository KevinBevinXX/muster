package ui

import (
	"encoding/json"
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
// WAITING: whether the card is blocked on the operator, from its attention
// badge (CardAttention, composed in internal/api from the attention queue's
// ONE read — never re-derived here). One word, written into `data-wait`:
//
//	you   it is waiting on YOU, any of:
//	      - muster's attention_ask (the queue's "Question" row): a dispatched
//	        agent or a mirrored Claude session called attention_ask and is
//	        waiting for your answer;
//	      - a mirrored Claude Code session stopped at a prompt (status
//	        "waiting": a permission prompt, the session ASKING to go on; the
//	        queue's "Session waiting");
//	      - Claude Code's own AskUserQuestion picker, held open by a mirrored
//	        session (the queue's "Session question");
//	      - a dispatched agent blocked at a permission prompt (the "awaiting
//	        approval" badge, AgentCardView.AwaitingApproval) — not an attention
//	        item, but the same kind of block as a mirrored session's prompt.
//
// These were two words ("ask", "question") behind two chips until an operator
// ask merged the chips: whichever way a card asks, it is waiting on you.
// A reply draft (kind "plugin") is not: nothing is blocked on it.

// waitKinds maps each attention kind a card can count to the wait word it
// files under ("" = none).
var waitKinds = map[string]string{
	"ask":      "you",
	"session":  "you",
	"question": "you",
	"plugin":   "",
}

// waitTokens is a card's `data-wait`: "you" when any of its attention items'
// kinds files under it, or the agent is awaiting approval; "" for none.
func waitTokens(a *CardAttention, awaitingApproval bool) string {
	if awaitingApproval {
		return "you"
	}
	if a != nil && a.Count > 0 {
		for _, k := range a.Kinds {
			if w := waitKinds[k]; w != "" {
				return w
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// THE FILTER CHIPS: presets above the list.
//
// SEMANTICS, stated once: the chips are ORed — a card is shown when it
// matches ANY chosen chip ("waiting on you, OR have been idle for a certain
// period"). The idle chips are exclusive with each other (choosing one clears the others:
// they are nested thresholds, so two ORed would just be the smaller). No chip
// chosen: no filter. The chips as a whole AND with the search box and the host
// tab, like any other term.
//
// chipSelector is the ONLY statement of that rule: the script looks each
// selection's selector up (chipSelectorsJSON), and the tests evaluate the
// selectors themselves.

// listChip is one chip.
type listChip struct {
	ID, Label, Help string
	// Group is "wait" or "idle"; Token is the word the card's data-<group>
	// carries for it.
	Group, Token string
}

var listChips = []listChip{
	{"waiting", "Waiting on you", "An agent or a session asked you something (attention_ask or an AskUserQuestion picker), or a Claude Code session or a dispatched agent is stopped at a permission prompt", "wait", "you"},
	{"idle-15m", "Idle >15m", "No activity for more than 15 minutes", "idle", "15m"},
	{"idle-1h", "Idle >1h", "No activity for more than an hour", "idle", "1h"},
	{"idle-1d", "Idle >1d", "No activity for more than a day", "idle", "1d"},
}

// chipAliases maps a chip id this device may still have stored from before
// (localStorage, agentsChipsStorageKey) to the chip that replaced it: the
// "Waiting on ask" and "Waiting on question" chips became "Waiting on you",
// so a remembered selection of either is that chip, not dropped.
var chipAliases = map[string]string{
	"ask":      "waiting",
	"question": "waiting",
}

// chipAliasesJSON is chipAliases for the script (chipsOf).
func chipAliasesJSON() string {
	b, _ := json.Marshal(chipAliases)
	return string(b)
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

// chipSelector is the selector a SHOWN card matches under the chips ids ("" =
// no chip, nothing to match): every chosen chip's attribute test, ORed in one
// :is() — or the test alone when only one chip is chosen.
func chipSelector(ids []string) string {
	var parts []string
	for _, id := range strings.Split(chipKey(ids), ",") {
		if c, ok := chipByID(id); ok {
			parts = append(parts, `[data-`+c.Group+`~="`+c.Token+`"]`)
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return ":is(" + strings.Join(parts, ",") + ")"
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
		// Clear: every chip off at once, shown only while one is on (the
		// script toggles the `hidden` ATTRIBUTE — no display utility here, which
		// would beat it). At the END of the row, so its appearing moves nothing.
		Button(
			ID("agents-chips-clear"),
			Type("button"),
			g.Attr("hidden", ""),
			g.Attr("aria-label", "Clear the quick filters"),
			Class("press min-h-[44px] shrink-0 whitespace-nowrap rounded-full px-3 text-sm font-medium text-muted underline transition hover:text-fg"),
			g.Text("Clear"),
		),
	)
}

// ---------------------------------------------------------------------------
// LONG TITLES: what heads a card — its NAME, or its FOCUS title when it has one
// (focus.go) — is clamped to cardTitleMax characters; past that it ends in a
// "…" BUTTON that opens the whole title in a popover. Both go through ONE
// mechanism, cardTitleRow: the row, the clamp, the "…" control and the popover.
//
// The row sits OUTSIDE the card's nav anchor (a button inside an <a> is
// invalid and its tap would open the card), so a card with a long name draws
// its name in a title row of its own above the anchor — the place its focus
// row already had. A short name is drawn inside the anchor as it always was.

// cardTitleMax is the most characters (runes) of a title a card draws.
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

// cardTitleRow is a card's title row: heading(short) — the title clamped by
// clampTitle, drawn by the caller (a link to the card for a name, the popover
// trigger for a focus title) — then, when the title was clamped, the "…"
// button, then the popover both open.
//
// The "…" is the SAME control on every card: a real <button> (keyboard
// operable), a 44px target, aria-haspopup/aria-controls/aria-expanded, opening
// pop — a native popover="auto" (an outside tap or Escape closes it) placed by
// floatingMenuScript, which also keeps every trigger's aria-expanded in step.
// It never navigates: it is not inside any link.
func cardTitleRow(slot, pop, full, pad, reserve string, heading func(short string) g.Node, popover g.Node) g.Node {
	short, clamped := clampTitle(full)
	return Div(
		cardSlot(slot),
		Class("flex items-start gap-0.5 px-4 "+pad+" "+reserve),
		heading(short),
		g.If(clamped, Button(
			Type("button"),
			g.Attr("data-title-more", ""),
			g.Attr("popovertarget", pop),
			g.Attr("aria-haspopup", "dialog"),
			g.Attr("aria-controls", pop),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", "Show the full title"),
			Class("press -my-1.5 inline-flex h-11 min-w-[44px] shrink-0 items-start justify-center rounded-lg pt-1.5 text-base font-semibold leading-tight text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus aria-expanded:bg-s2"),
			g.Text("…"),
		)),
		popover,
	)
}

// cardLongTitle is a long NAME's title row (cardTitleRow): the clamped name,
// itself a link to the card's page (so tapping the words still opens the
// card), title= the whole name (the desktop's hover tooltip), and a popover
// holding the whole name.
//
// subtitle draws it as a subtitle (a card headed by its focus); extra is drawn
// after the name inside the link (a mirrored window's qualifier and session).
func cardLongTitle(cardID, href, full string, subtitle bool, reserve string, extra ...g.Node) g.Node {
	pop := cardID + "-title"
	class, pad := cardTitleClass, "pt-3"
	if subtitle {
		class, pad = cardSubtitleClass, "pt-1"
	}
	return cardTitleRow("title", pop, full, pad, reserve,
		func(short string) g.Node {
			return A(
				Href(href),
				hx("hx-boost", "false"),
				g.Attr("data-card-title", ""),
				g.Attr("title", full),
				Class(strings.Replace(class, "mr-auto ", "min-w-0 py-1 ", 1)),
				g.Text(short),
				g.Group(extra),
			)
		},
		Div(
			ID(pop),
			g.Attr("popover", "auto"),
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Full title"),
			g.Attr("data-floating-menu", ""),
			g.Attr("data-title-full", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-72 max-w-[calc(100vw-16px)] flex-col gap-1.5 rounded-xl border border-line bg-s1 p-3 text-fg shadow-2xl ring-1 ring-black/20"),
			Span(Class("break-words text-sm font-semibold"), g.Text(full)),
		))
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
