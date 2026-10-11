package ui

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
)

// --- Task tag UI --------------------------------------------------------------
//
// Two visual classes, because they MEAN different things:
//   - ROUTING chips (reserved namespaces: initiative:/gate:/auto:) drive
//     behaviour, so they are colour-coded and carry a title explaining the effect.
//   - DESCRIPTIVE chips are pure labels: muted, uniform, no promise of behaviour.
// Confusing the two is the main way a tags UI misleads, so the distinction is
// pinned by a render test.

// maxCardTagChips caps how many chips a COLLAPSED card renders before collapsing
// the remainder into a "+N" chip. The card is deliberately dense (0.7.x removed
// the directory chip as clutter), so tags get a small, fixed budget.
const maxCardTagChips = 3

// tagChipClass returns the chip styling + a hover title for one tag, keyed on
// whether it is routing (behaviour-bearing) or descriptive.
func tagChipClass(tag string) (class, title string) {
	ns, val := notes.ParseTag(tag)
	base := "inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-xs "
	switch {
	case ns == notes.NSGate:
		return base + "bg-st-error-bg font-medium text-st-error-fg ring-1 ring-inset ring-st-error-fg/40",
			"Gated: not dispatchable — " + val
	case ns == notes.NSInitiative:
		return base + "bg-st-progress-bg font-medium text-st-progress-fg ring-1 ring-inset ring-st-progress-fg/40",
			"Routing: initiative \"" + val + "\" (resolved by the initiatives ledger)"
	case ns == notes.NSAuto:
		return base + "bg-st-warning-bg font-medium text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40",
			"Routing: auto-dispatch marker (disabled unless MUSTER_TAG_AUTODISPATCH is on)"
	case ns == notes.NSProject:
		// Reserved but NOT routing: a project is a grouping label, so it gets its
		// own colour without the routing chip's "this changes behaviour" promise.
		return base + "bg-st-running-bg font-medium text-st-running-fg ring-1 ring-inset ring-st-running-fg/40",
			"Project: " + val
	default:
		return base + "bg-s2 text-muted ring-1 ring-inset ring-line", "Tag: " + tag
	}
}

// tagChip renders one read-only chip. data-tag-kind lets tests (and CSS) tell the
// two classes apart without parsing colour utility strings.
func tagChip(tag string) g.Node {
	class, title := tagChipClass(tag)
	kind := "descriptive"
	if notes.IsRoutingTag(tag) {
		kind = "routing"
	}
	return Span(
		g.Attr("data-tag", tag),
		g.Attr("data-tag-kind", kind),
		g.Attr("title", title),
		Class(class),
		g.Text(tag),
	)
}

// cardTagChips renders a card's tag chips: routing tags FIRST (they carry
// behaviour, so they must never be the ones hidden behind "+N"), then descriptive
// ones, capped at maxCardTagChips with a "+N" overflow chip. Zero tags renders
// NOTHING at all (no empty container) so an untagged card looks exactly as before.
func cardTagChips(tags []string) g.Node {
	if len(tags) == 0 {
		return g.Text("")
	}
	ordered := make([]string, 0, len(tags))
	ordered = append(ordered, notes.RoutingTags(tags)...)
	for _, t := range tags {
		if !notes.IsRoutingTag(t) {
			ordered = append(ordered, t)
		}
	}
	shown := ordered
	overflow := 0
	if len(ordered) > maxCardTagChips {
		shown = ordered[:maxCardTagChips]
		overflow = len(ordered) - maxCardTagChips
	}
	nodes := make([]g.Node, 0, len(shown)+1)
	for _, t := range shown {
		nodes = append(nodes, tagChip(t))
	}
	if overflow > 0 {
		nodes = append(nodes, Span(
			g.Attr("data-tag-overflow", strconv.Itoa(overflow)),
			g.Attr("title", strings.Join(ordered[maxCardTagChips:], ", ")),
			Class("inline-flex shrink-0 items-center rounded-md bg-s2 px-1.5 py-0.5 text-xs text-muted ring-1 ring-inset ring-line"),
			g.Text("+"+strconv.Itoa(overflow)),
		))
	}
	return g.Group(nodes)
}

// boardURL builds the /ui/tasks URL for a complete board query: the active tag
// set (repeated `tag=` params — the AND filter the store applies via
// `tags @> …`), the status, and the row cap.
//
// 🔴 ONE BUILDER FOR ALL THREE, because the URL *is* the board's state: it is
// what #tasks-list's hx-get holds, and — via tagScript's htmx:configRequest
// listener, which rewrites the request path from that attribute — the only thing
// that survives an SSE-triggered refetch. A helper that built tag-only URLs would
// silently DROP the status and the limit every time a tag was toggled, which is
// exactly how "show more" would collapse on the next event from any browser.
//
// ⚠ The listener is half of that sentence and it did not exist until the audit
// round on PR #727. Before it, the hx-get write reached the ATTRIBUTE only: htmx
// captures a node's verb path at process time, so every trigger-driven refetch
// asked for a bare /ui/tasks and the tag filter had been resetting on each SSE
// event since it shipped. Do not restate the URL alone as the mechanism.
//
// Empty tags + empty status + limit 0 is the canonical "/ui/tasks", so the
// static shell's hx-get and a freshly-cleared filter produce the same string.
func boardURL(tags []string, status string, limit int) string {
	q := url.Values{}
	for _, t := range tags {
		q.Add("tag", t)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) == 0 {
		return "/ui/tasks"
	}
	return "/ui/tasks?" + q.Encode()
}

// tagFilterRow renders the horizontally-scrollable multi-select filter chip row
// above the task groups. Selection is AND, is SERVER-rendered from the filter
// THIS REQUEST carried (so the chips can never disagree with the cards beside
// them), and is persisted client-side in localStorage
// `muster.tasks.tagfilter` by the tag-filter script — matching the existing
// `muster.recent.*` precedent.
//
// ⚠ Server-rendering is CONSISTENCY, not persistence: this used to read "so an
// SSE-driven list refresh keeps it", and a refresh keeps only what its URL
// carries. What makes the filter survive one is boardURL plus tagScript's
// htmx:configRequest listener, both of which had to change — see boardURL.
//
// An empty vocabulary with no active filter renders NOTHING (no empty toolbar on
// a queue that has never used tags).
func tagFilterRow(vocab []notes.TagCount, active []string) g.Node {
	if len(vocab) == 0 && len(active) == 0 {
		return g.Text("")
	}
	act := make(map[string]bool, len(active))
	for _, t := range active {
		act[t] = true
	}
	// Any ACTIVE tag missing from the vocabulary (e.g. a filter for a tag whose
	// last task was just deleted) still gets a chip, so the user can unselect it.
	shown := make([]string, 0, len(vocab)+len(active))
	seen := map[string]bool{}
	for _, tc := range vocab {
		// `project:` tags have their OWN row (projectFilterRow) — rendering them
		// here too would give one filter two chips that toggle the same state.
		if ns, _ := notes.ParseTag(tc.Tag); ns == notes.NSProject {
			continue
		}
		// 🔴 EXTERNAL-ID TAGS ARE NOT FILTERS, AND THEY WERE DROWNING THE ONES
		// THAT ARE. `clickup:<id>` / `superseded-by:<id>` name ONE task each, so a
		// chip for one can only ever narrow the board to the task you must already
		// be looking at to know the id. Measured live: 241 chips, 202 of them
		// clickup ids, a 33,967px row inside a 1,736px viewport with the scrollbar
		// suppressed and the `›` cue pointer-events-none — twenty screens of
		// controls with no way to reach them, hiding the ~39 real ones.
		//
		// They are removed from THIS ROW ONLY. Each stays on its card (tagChip),
		// stays in the editor and its datalist, and stays filterable by URL —
		// nothing that could act on one loses the ability to. See
		// notes.ExternalIDNamespaces for why this is a closed namespace rule and
		// not a "hide chips with a count of 1" heuristic.
		if notes.IsExternalIDTag(tc.Tag) {
			continue
		}
		shown = append(shown, tc.Tag)
		seen[tc.Tag] = true
	}
	for _, t := range active {
		if ns, _ := notes.ParseTag(t); ns == notes.NSProject {
			continue // owned by projectFilterRow, which always renders an active one
		}
		// An ACTIVE external-id tag DOES get a chip, and that asymmetry is the
		// point: the vocabulary loop above decides what to OFFER, this loop keeps
		// what is already ON reachable. A filter you cannot see is a filter you
		// cannot clear, and the board would look mysteriously empty.
		if !seen[t] {
			shown = append(shown, t)
			seen[t] = true
		}
	}

	// All-project vocabulary with nothing active: the general row has nothing to
	// say, so render no empty toolbar.
	if len(shown) == 0 && len(active) == 0 {
		return g.Text("")
	}

	chips := make([]g.Node, 0, len(shown)+1)
	for _, t := range shown {
		on := act[t]
		cls := "press inline-flex min-h-[44px] min-w-[44px] shrink-0 justify-center items-center gap-1 rounded-full px-3 py-1 text-xs font-medium transition "
		if on {
			cls += "bg-accent/15 text-fg ring-1 ring-inset ring-accent/40"
		} else {
			cls += "bg-s2 text-muted ring-1 ring-inset ring-line hover:bg-s3 hover:text-fg"
		}
		if notes.IsRoutingTag(t) && !on {
			// Routing tags stay visually distinct even in the filter row.
			cls = "press inline-flex min-h-[44px] min-w-[44px] shrink-0 justify-center items-center gap-1 rounded-full px-3 py-1 text-xs font-semibold text-st-review-fg ring-1 ring-inset ring-st-review-fg/40 transition hover:bg-st-review-bg"
		}
		chips = append(chips, Button(
			Type("button"),
			g.Attr("data-tag-filter", t),
			g.Attr("aria-pressed", boolAttr(on)),
			g.Attr("title", "Filter tasks by "+t),
			Class(cls),
			g.Text(t),
		))
	}
	if len(active) > 0 {
		chips = append(chips, tagFilterClearButton("Clear"))
	}
	return chipScroller(Div(
		ID("tag-filter-row"),
		g.Attr("data-active-tags", strings.Join(active, ",")),
		Class("-mx-1 flex items-center gap-2 overflow-x-auto px-1 pb-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"),
		g.Group(chips),
	), len(chips), "mb-3")
}

// chipScrollerOverflowAt is the chip count past which a filter row is assumed to
// overflow a phone-width viewport. At or below it the row fits and needs no cue.
//
// ⚠️ This is a HEURISTIC, not a measurement, and it is wrong on wide viewports:
// measured at 1280px with 6 short chips, scrollWidth == clientWidth (the row does
// NOT overflow) and the cue paints anyway. A server-side count cannot know the
// rendered width, so only a client-side measure could be honest. Left as-is
// deliberately: the cue is pointer-events-none and purely cosmetic, and the fix
// costs a new resize/measure listener (plus the once-only guard that comes with
// it) for a false-positive fade on desktop.
const chipScrollerOverflowAt = 4

// chipScroller wraps a horizontally-scrolling filter chip row and, when the row
// is long enough to overflow, adds a right-edge fade + chevron.
//
// The rows are `overflow-x-auto` with the scrollbar explicitly suppressed
// ([scrollbar-width:none] + ::-webkit-scrollbar:hidden), so past ~4 chips they
// scroll SILENTLY: there is no scrollbar, no fade and no arrow, and the chips
// beyond the fold are simply invisible. The cue is rendered SERVER-side off the
// chip count rather than measured in JS — deterministic, and no new listener to
// guard against htmx re-execution. It is pointer-events-none so it can never eat
// a chip tap.
func chipScroller(row g.Node, chips int, outerCls string) g.Node {
	if chips <= chipScrollerOverflowAt {
		return Div(Class(outerCls), row)
	}
	return Div(
		Class("relative "+outerCls),
		row,
		Div(
			g.Attr("data-chip-overflow", ""),
			g.Attr("aria-hidden", "true"),
			Class("pointer-events-none absolute inset-y-0 right-0 flex w-10 items-center justify-end bg-gradient-to-l from-bg via-bg/80 to-transparent pb-1 text-sm text-muted"),
			g.Text("›"),
		),
	)
}

// projectFilterRow renders the PROJECT filter chips, above the general tag chip
// row. It is the payoff surface for the `project:` tag: without a way to act on
// it, the tag is write-only.
//
// 🔑 It deliberately reuses the EXISTING tag-filter mechanism rather than adding a
// parallel one: each chip carries `data-tag-filter="project:<slug>"`, so the same
// delegated click handler, the same localStorage key, the same hx-get URL
// building and the same filtered-empty state all apply with ZERO new JavaScript.
// The chip merely LABELS itself with the bare slug (+ count) — `project:` is a
// storage detail, not something a filter control should show.
//
// Renders nothing when no project exists and none is active (no empty toolbar on
// a queue that has never used projects).
func projectFilterRow(projects []notes.ProjectCount, active []string) g.Node {
	// An ACTIVE project missing from the vocabulary (its last task was just
	// dismissed) still needs a chip, or the user cannot unselect it.
	activeProject, hasActive := "", false
	for _, t := range active {
		if ns, val := notes.ParseTag(t); ns == notes.NSProject {
			activeProject, hasActive = val, true
			break
		}
	}
	if len(projects) == 0 && !hasActive {
		return g.Text("")
	}
	shown := make([]notes.ProjectCount, 0, len(projects)+1)
	seen := false
	for _, p := range projects {
		shown = append(shown, p)
		if p.Name == activeProject {
			seen = true
		}
	}
	if hasActive && !seen {
		shown = append(shown, notes.ProjectCount{Name: activeProject, Count: 0})
	}

	chips := make([]g.Node, 0, len(shown)+1)
	for _, p := range shown {
		on := p.Name == activeProject
		cls := "press inline-flex min-h-[44px] min-w-[44px] shrink-0 justify-center items-center gap-1 rounded-full px-3 py-1 text-xs font-medium transition "
		if on {
			cls += "bg-st-running-bg text-st-running-fg ring-1 ring-inset ring-st-running-fg/40"
		} else {
			cls += "bg-st-running-bg text-st-running-fg ring-1 ring-inset ring-st-running-fg/40 hover:bg-st-running-bg hover:text-st-running-fg"
		}
		chips = append(chips, Button(
			Type("button"),
			g.Attr("data-tag-filter", notes.NSProject+":"+p.Name),
			// 🔑 SINGLE-SELECT. A task carries AT MOST ONE project, so two active
			// project chips would AND together into a guaranteed-empty list — with
			// only the first chip rendering aria-pressed="true", because
			// `activeProject` takes the FIRST project tag it finds. Declaring the
			// group makes the shared click handler REPLACE a project rather than
			// add one; see tagScript's data-tag-filter-exclusive branch.
			g.Attr("data-tag-filter-exclusive", notes.NSProject+":"),
			g.Attr("data-project-filter", p.Name),
			g.Attr("aria-pressed", boolAttr(on)),
			g.Attr("title", "Show only tasks in the \""+p.Name+"\" project"),
			Class(cls),
			g.Text(p.Name),
			Span(Class("text-st-running-fg"), g.Text(strconv.FormatInt(p.Count, 10))),
		))
	}
	return chipScroller(Div(
		ID("project-filter-row"),
		g.Attr("data-active-project", activeProject),
		Class("-mx-1 flex items-center gap-2 overflow-x-auto px-1 pb-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"),
		// text-muted — see the identical note on the Status row label in
		// notes.go: the darkest text step measured 2.66:1 against a 4.5:1 floor.
		Span(Class("shrink-0 text-xs font-medium uppercase tracking-wide text-muted"), g.Text("Project")),
		g.Group(chips),
	), len(chips), "mb-2")
}

// tagFilterClearButton renders the "Clear filter" control. It is a real control
// (not just prose) so the filtered-empty state always offers a way out.
func tagFilterClearButton(label string) g.Node {
	return Button(
		Type("button"),
		g.Attr("data-tag-filter-clear", ""),
		g.Attr("aria-label", "Clear tag filter"),
		Class("press inline-flex min-h-[44px] min-w-[44px] shrink-0 justify-center items-center gap-1 rounded-full px-3 py-1 text-xs font-medium text-muted underline decoration-dotted underline-offset-2 transition hover:text-fg"),
		g.Text(label),
	)
}

// boolAttr renders a Go bool as the "true"/"false" string ARIA expects.
func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// tasksFilteredEmpty is the state shown when a filter is ACTIVE but matches
// nothing. It MUST be distinct from the generic "No tasks yet" all-clear — a
// filter silently rendering "all clear" is the classic trap here (the user
// concludes their queue is empty when it is merely filtered), so this copy names
// the ACTIVE FILTERS and ships a Clear control. Pinned by a render test + an e2e.
//
// 🔴 The status is named alongside the tags, not omitted. A status-only filter
// that matched nothing would otherwise render "No tasks match " with the
// sentence trailing off — copy that says a filter is on without saying WHICH,
// which is barely better than the all-clear this state exists to avoid.
//
// An unknown status yields an empty Label, which is what drops it from the
// sentence rather than printing "status: " with nothing after it.
func tasksFilteredEmpty(active []string, status string) g.Node {
	parts := append([]string(nil), active...)
	if label := taskstatus.Label(status); label != "" {
		parts = append(parts, "status: "+label)
	}
	return Div(
		g.Attr("data-tasks-filtered-empty", ""),
		Class("mt-12 flex flex-col items-center justify-center gap-2 text-center"),
		Div(Class("text-4xl"), g.Text("🔍")),
		P(Class("text-lg font-medium text-fg2"),
			g.Text("No tasks match "+strings.Join(parts, " + "))),
		P(Class("text-sm text-muted"), g.Text("Other tasks are hidden by this filter.")),
		Div(Class("mt-2"), tagFilterClearButton("Clear filter")),
	)
}

// tagDatalist renders the <datalist> of known tags (the vocabulary, most-used
// first) that backs the editor's tag input. Served standalone at GET /ui/tags and
// embedded in the edit modal.
func tagDatalist(vocab []notes.TagCount) g.Node {
	opts := make([]g.Node, 0, len(vocab))
	for _, tc := range vocab {
		opts = append(opts, Option(Value(tc.Tag)))
	}
	return g.El("datalist", ID("task-tags-vocabulary"), g.Group(opts))
}

// tagEditor renders the edit modal's tag control: the current tags as removable
// chips (each backed by a HIDDEN `tag` input, so the form submits the full set),
// plus a text input where typing a tag and pressing Enter (or comma) appends a new
// chip. Suggestions come from the vocabulary datalist.
//
// Routing-tag INPUT is disabled while the task is in progress: a label is not a
// spec change (so descriptive edits stay allowed), but changing where a task
// routes mid-flight is — the server enforces the same rule and 409s.
func tagEditor(tags []string, vocab []notes.TagCount, inProgress bool) g.Node {
	chips := make([]g.Node, 0, len(tags))
	for _, t := range tags {
		chips = append(chips, tagEditorChip(t, inProgress && notes.IsRoutingTag(t)))
	}
	hint := "Type a tag and press Enter. Reserved: initiative: gate: auto:"
	if inProgress {
		hint = "Task is in progress — descriptive tags only (routing tags are locked)."
	}
	return Div(
		g.Attr("data-tag-editor", ""),
		g.Attr("data-tag-editor-locked", boolAttr(inProgress)),
		Class("flex flex-col gap-2"),
		Div(
			g.Attr("data-tag-chips", ""),
			// 🔴 data-tag-baseline is the SUBMITTED tag set as the server rendered it,
			// sorted + comma-joined. It exists because the modal's dirty guard cannot
			// see a tag change field-by-field: removing a chip DELETES its hidden
			// <input name="tag">, so there is no element left whose .value could
			// differ from its .defaultValue, and the guard read a removal as clean
			// (measured: remove a chip, tap the backdrop, the removal is gone).
			// Comparing the set against this baseline catches add AND remove.
			g.Attr("data-tag-baseline", tagBaseline(tags)),
			Class("flex flex-wrap gap-1.5"),
			g.Group(chips),
		),
		Input(
			Type("text"),
			g.Attr("data-tag-input", ""),
			g.Attr("list", "task-tags-vocabulary"),
			g.Attr("autocomplete", "off"),
			// NO name attribute: this field is a chip COMPOSER, never submitted. The
			// hidden `tag` inputs are the form's actual value, so a half-typed tag
			// left in the box can't silently become a tag.
			Placeholder("add a tag…"),
			Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		P(Class("text-xs text-muted"), g.Text(hint)),
		tagDatalist(vocab),
	)
}

// tagBaseline renders the initial tag set the way the client-side dirty guard
// recomputes it: sorted and comma-joined, so the comparison is order-independent
// (chips are appended in tap order, not in the server's order). It sorts a COPY —
// the caller's slice is the note's own Tags and must not be reordered under it.
func tagBaseline(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	cp := append([]string(nil), tags...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// tagEditorChip is one removable chip in the editor: the visible label, a hidden
// `tag` input carrying the value into the form submit, and a × remove button
// (which removes the whole chip, hidden input included). A LOCKED chip (a routing
// tag on an in-progress task) keeps its hidden input but drops the × so it
// survives the submit unchanged.
func tagEditorChip(tag string, locked bool) g.Node {
	class, title := tagChipClass(tag)
	kind := "descriptive"
	if notes.IsRoutingTag(tag) {
		kind = "routing"
	}
	nodes := []g.Node{
		g.Attr("data-tag-chip", tag),
		g.Attr("data-tag-kind", kind),
		g.Attr("title", title),
		Class(class + " gap-1.5 py-1"),
		g.Text(tag),
		Input(Type("hidden"), Name("tag"), Value(tag)),
	}
	if !locked {
		nodes = append(nodes, Button(
			Type("button"),
			g.Attr("data-tag-remove", ""),
			g.Attr("aria-label", "Remove tag "+tag),
			Class("press -mr-0.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-current opacity-60 transition hover:opacity-100"),
			g.Text("✕"),
		))
	}
	return Span(nodes...)
}
