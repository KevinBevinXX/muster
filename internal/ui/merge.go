package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/textplain"
)

// --- Task merge (supersede) UI -------------------------------------------------
//
// Merging two tasks needs the app's FIRST multi-select. There is no pattern to
// inherit, so this is deliberately the smallest thing that works:
//
//   - a "Select" toggle above the list turns selection mode on,
//   - each card grows a checkbox in its action row (hidden otherwise),
//   - with exactly TWO checked, a bar offers the two possible merges — one button
//     per WINNER ("Keep #12 · …"), because which task survives is the whole
//     decision and a single "Merge" button would have to guess it.
//
// 🔴 The bar is rendered INLINE at the top of the list, not as a fixed/sticky
// overlay. The app already has a `fixed bottom` FAB (z-30) and a `sticky top-0`
// header (z-20); a third floating layer is a real collision risk on a 390px
// phone, and this control is not worth introducing one for. The cost is that a
// selection made far down the list needs a scroll back up — noted rather than
// hidden.
//
// 🔴 Selection state lives ONLY in the DOM checkboxes and the toolbar/bar are
// rendered INSIDE #tasks-list, so any list refresh that actually REPLACES the
// container's contents (SSE task.changed, the tag filter, the post-merge
// tasks:changed) clears the whole affordance with it. That is the intended
// behaviour, not an oversight: the cards under a stale selection may have been
// regrouped or retired by another tab, and silently carrying ids across a refresh
// is how you merge the wrong pair.
//
// ⚠ Precisely: it is the SWAP that clears it, not the attempt. htmx does not swap
// a non-2xx response, so a FAILED refresh leaves the ticks and the bar standing —
// and the tag filter has already been rewritten underneath them. What bounds that
// is #tasks-list's own failure path (hx-on::response-error / ::send-error /
// ::timeout → cgTasksListFailed), which replaces the container's contents with an
// error box and so takes the checkboxes and the bar with it. That path only fires
// for a request SOURCED at #tasks-list — which is why tagScript's refresh() must
// pass `source: el` (components.go; see TestEveryTasksListAjaxIsSourcedAtTheContainer).

// focusRing is the keyboard-only focus treatment for the controls added here.
// The rest of the app's buttons have NO focus-visible styling, so a keyboard
// user driving this multi-select would otherwise have no idea what is focused —
// and unlike the existing single-button actions, this flow REQUIRES moving
// between several controls before committing. Scoped to the new controls
// deliberately (retro-fitting the whole app is a separate change).
const focusRing = " focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus focus-visible:ring-offset-2 focus-visible:ring-offset-bg"

// minMergeableTasks is how many MERGEABLE cards must be on screen before the merge
// affordance renders at all. Merging needs two tasks; offering "Select" over a
// single card is a dead end the user has to discover by tapping.
const minMergeableTasks = 2

// mergeableCards counts the cards a merge can actually involve.
//
// 🔴 It is NOT len(v.Cards). A COMPLETE task can be neither winner nor loser (the
// server refuses both: a complete loser means the merge already happened, and a
// complete winner would retire open work into a dead task), and complete cards are
// in v.Cards — they used to be the Done section's, and since the board flattened
// they simply sit inline with everything else. Counting them meant a queue of two Done cards rendered a
// "Select" toggle over nothing selectable, and a mixed queue actively offered a
// Done card as the winner. in_progress deliberately still COUNTS: that refusal is
// about an agent being mid-flight, it is transient, and the 409 explains it — the
// user picking two open-looking cards should not have one silently vanish because
// an agent started a second ago.
func mergeableCards(views []TaskCardView) int {
	n := 0
	for _, v := range views {
		if isMergeable(v.Note) {
			n++
		}
	}
	return n
}

// isMergeable is the ONE definition of "this task can take part in a merge",
// shared by the count above and by the per-card checkbox — so the toolbar can
// never offer a selection the cards cannot supply.
func isMergeable(n notes.Note) bool {
	return n.Status != notes.StatusComplete
}

// taskMergeToolbar is the "Select" toggle row. It renders NOTHING below
// minMergeableTasks cards.
func taskMergeToolbar(cards int) g.Node {
	if cards < minMergeableTasks {
		return g.Text("")
	}
	return Div(
		Class("mb-2 flex items-center justify-end"),
		Button(
			Type("button"),
			ID("tasks-select-toggle"),
			g.Attr("data-select-toggle", ""),
			g.Attr("aria-pressed", "false"),
			g.Attr("title", "Select tasks to merge"),
			// min-h-[44px] matches the app's touch-target standard.
			Class("press inline-flex min-h-[44px] items-center gap-1.5 rounded-xl px-3 text-xs font-medium text-muted ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"+focusRing),
			g.Text("Select"),
		),
	)
}

// taskMergeBar is the merge affordance itself: a live count, and — once exactly
// two tasks are checked — one submit button per possible WINNER. Hidden until
// selection mode is on. Rendered only alongside taskMergeToolbar.
//
// The form POSTs to /tasks/merge with hx-swap="none": the server answers 200 +
// `HX-Trigger: tasks:changed`, which refetches #tasks-list. Rendering a list
// body here would silently drop the filter. 🔴 The refetch itself carries the
// filter only via tagScript's `htmx:configRequest` listener — this comment used
// to credit "#tasks-list re-issues its OWN hx-get", which is false: htmx 2.0.4
// captures the verb path at process time, so the attribute alone moves nothing.
// A 4xx/409 is surfaced by resyncScript's htmx:responseError
// toast, which prints the server's plain-text message verbatim — which is why
// every merge refusal is worded for a human.
func taskMergeBar(cards int) g.Node {
	if cards < minMergeableTasks {
		return g.Text("")
	}
	keep := func(slot string) g.Node {
		return Button(
			Type("submit"),
			g.Attr("data-merge-keep", slot),
			// Populated by taskMergeScript with the chosen task's label. Hidden until
			// exactly two tasks are checked.
			Class("hidden press min-h-[44px] items-center justify-center rounded-xl bg-accent px-3 text-xs font-semibold text-on-accent transition hover:bg-accent/90"+focusRing),
		)
	}
	return Div(
		ID("task-merge-bar"),
		g.Attr("data-merge-bar", ""),
		Class("mb-3 hidden flex-col gap-2 rounded-2xl border border-line bg-s1/70 p-3 ring-1 ring-line"),
		P(
			g.Attr("data-merge-status", ""),
			// aria-live so a screen-reader user hears the count change as they check
			// boxes — without it the whole flow is silent until submit.
			g.Attr("role", "status"),
			g.Attr("aria-live", "polite"),
			Class("text-xs text-muted"),
			g.Text("Select two tasks to merge."),
		),
		FormEl(
			hx("hx-post", "/tasks/merge"),
			// No swap: the refresh is driven by the HX-Trigger response header, so the
			// tag filter survives (see the doc comment above).
			hx("hx-swap", "none"),
			// Form-level (NOT button-level): a button-level hx-disabled-elt was
			// measured inert on this app's other forms.
			//
			// 🔴 A BARE attribute selector, deliberately NOT "find button". htmx's
			// `find <sel>` is querySelector — the FIRST match only. Verified in the
			// vendored bundle (web/static/vendor/htmx.min.js): the resolver has
			// `r.indexOf("find ")===0 → e=u(f(t), …)` with
			// `function u(e,t){… return e.querySelector(t)}`, while a bare selector
			// falls through to `c.querySelectorAll(e)` and yields ALL matches. This
			// form renders TWO submit buttons, so "find button" disabled exactly one:
			// tapping "Keep #B" disabled "Keep #A" and left the button under the
			// user's finger live for a second tap.
			//
			// This is only the CLIENT half. The server refuses a merge whose loser or
			// winner is already complete (api/merge.go), which is what actually makes
			// a double-tap — including the two-button "Keep #A then Keep #B" variant
			// that produced a circular supersede — safe.
			hx("hx-disabled-elt", "[data-merge-keep]"),
			Class("flex flex-wrap items-center gap-2"),
			Input(Type("hidden"), Name("winner"), g.Attr("data-merge-winner", "")),
			Input(Type("hidden"), Name("loser"), g.Attr("data-merge-loser", "")),
			keep("first"),
			keep("second"),
		),
		P(
			Class("text-[11px] leading-snug text-muted"),
			// Says exactly what happens, because "merge" is ambiguous and the
			// destructive reading (one task disappears) is the one users assume.
			//
			// ⚠ "gains the other's tags" is literal and INCLUDES `project:`. A
			// projectless winner therefore adopts the loser's project — a real,
			// one-sided effect that the copy covers only implicitly. It is deliberate
			// (notes.MergeTaskTags refuses only a project CONFLICT, where both sides
			// name a different one, because there the union would silently pick the
			// alphabetically smaller) and pinned by a test. Named here so the next
			// reader does not have to rediscover that "tags" quietly means "including
			// which project this task belongs to".
			g.Text("The kept task gains the other's tags. The other is marked complete with a pointer to it — nothing is deleted."),
		),
	)
}

// taskSelectControl is a card's merge checkbox. It is always in the DOM (so the
// toggle is pure class work, no re-fetch) and hidden until selection mode is on.
// data-task-select carries the id and data-task-select-label the display label,
// so the bar can name the two candidates without a second lookup.
// 🔴 The label is the task's TITLE or its id — deliberately NOT ui.TaskTitle,
// whose fallback is the DIRECTORY. The directory is intentionally kept off the
// card (the 0.7.x declutter removed the directory chip, and
// TestTaskTitlePrefersTitleOverDirectory pins that a directory-only card renders
// no heading and no directory text). Reusing TaskTitle here would put the raw
// path back onto every card in an attribute — which is exactly what that guard
// caught when this control first shipped. TaskTitle remains the right definition
// where a task needs a LABEL for a picker or a notification; here the card's own
// declutter rule wins.
//
// 🔴 A COMPLETE task renders NOTHING here. It can be neither side of a merge, and
// the card is complete (it used to sit in the Done section) where the affordance would otherwise appear
// on every retired task. Suppressing it and gating the toolbar on mergeableCards
// are two halves of one rule (isMergeable): either alone leaves a dead end.
func taskSelectControl(n notes.Note) g.Node {
	if !isMergeable(n) {
		return g.Text("")
	}
	ids := strconv.FormatInt(n.ID, 10)
	label := textplain.Title(n.Title)
	if label == "" {
		label = "#" + ids
	}
	return Label(
		g.Attr("data-task-select", ids),
		g.Attr("data-task-select-label", label),
		Class("hidden shrink-0 cursor-pointer items-center justify-center rounded-xl px-1"),
		Input(
			Type("checkbox"),
			g.Attr("data-task-select-box", ids),
			g.Attr("aria-label", "Select task #"+ids+" for merge"),
			Class("h-5 w-5 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing),
		),
	)
}

// taskMergeScript wires selection mode, the live count, and the two winner
// buttons.
//
// 🔴 The delegated document listeners are bound EXACTLY ONCE (window.__cgMergeInit).
// htmx re-executes in-body <script> tags on every hx-boost body swap while
// document-level listeners survive it, so an unguarded handler stacks up and a
// single click fires N times — the exact bug that made a tag chip toggle twice
// (net zero) in 0.7.75. Same discipline as appScript's __cgInit / tagScript's
// __cgTagInit.
//
// Selection mode itself is NOT persisted. A list refresh re-renders the toolbar
// in its default (off) state and clears the checkboxes, which is deliberate —
// see the file header.
func taskMergeScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__cgMergeInit) return;
  window.__cgMergeInit = true;

  function boxes() {
    return Array.prototype.slice.call(document.querySelectorAll('[data-task-select-box]'));
  }
  function checked() {
    return boxes().filter(function (b) { return b.checked; });
  }
  function labelFor(box) {
    var host = box.closest('[data-task-select]');
    var id = box.getAttribute('data-task-select-box');
    var text = host ? (host.getAttribute('data-task-select-label') || '') : '';
    return text && text !== ('#' + id) ? ('Keep #' + id + ' · ' + text) : ('Keep #' + id);
  }
  function selectionOn() {
    var t = document.querySelector('[data-select-toggle]');
    return !!(t && t.getAttribute('aria-pressed') === 'true');
  }

  // render() is the ONE place the bar's state is derived from the checkboxes, so
  // the count, the button labels and the hidden winner/loser inputs can never
  // disagree with what is actually ticked.
  function render() {
    var bar = document.querySelector('[data-merge-bar]');
    if (!bar) return;
    var sel = checked();
    var status = bar.querySelector('[data-merge-status]');
    var first = bar.querySelector('[data-merge-keep="first"]');
    var second = bar.querySelector('[data-merge-keep="second"]');
    var winner = bar.querySelector('[data-merge-winner]');
    var loser = bar.querySelector('[data-merge-loser]');
    if (!status || !first || !second || !winner || !loser) return;

    if (sel.length !== 2) {
      first.classList.add('hidden'); first.classList.remove('inline-flex');
      second.classList.add('hidden'); second.classList.remove('inline-flex');
      winner.value = ''; loser.value = '';
      status.textContent = sel.length === 0
        ? 'Select two tasks to merge.'
        : (sel.length === 1 ? 'Select one more task to merge.' : 'Select exactly two tasks (' + sel.length + ' selected).');
      return;
    }
    var a = sel[0], b = sel[1];
    first.textContent = labelFor(a);
    second.textContent = labelFor(b);
    first.classList.remove('hidden'); first.classList.add('inline-flex');
    second.classList.remove('hidden'); second.classList.add('inline-flex');
    // Default to the FIRST-listed task winning, so a submit triggered by Enter on
    // the form (rather than a button click) still carries a coherent pair.
    winner.value = a.getAttribute('data-task-select-box');
    loser.value = b.getAttribute('data-task-select-box');
    status.textContent = 'Two tasks selected — choose which one to keep.';
  }

  function setMode(on) {
    var toggle = document.querySelector('[data-select-toggle]');
    if (toggle) toggle.setAttribute('aria-pressed', on ? 'true' : 'false');
    // 🔴 The document-level 'selecting' class is what stops a card click
    // NAVIGATING mid-merge. A task card's whole surface is a stretched link to
    // /tasks/<id> (an <a class="card-link"> with an ::after overlay), and this
    // delegated listener only preventDefault()s [data-select-toggle] and
    // [data-merge-keep] — it never sees the anchor, so it cannot shield it.
    // The rule .selecting .card-link::after { pointer-events: none } in
    // web/css/input.css drops the overlay instead.
    //
    // 🔴 IT DOES NOT ROUTE THE CLICK TO THE CHECKBOX, and this comment used to
    // say it did ("so the click reaches the checkbox underneath"). There is no
    // checkbox underneath: taskSelectControl lives inside the action row, which
    // is already 'relative z-10' — ABOVE the overlay — and no handler here ticks
    // a box from a card-body click. Dropping the overlay makes a card-body click
    // do NOTHING, which is the whole and sufficient point: the destructive step
    // of a merge must not be one stray tap away from navigating off the board.
    // It is set on <html> rather than <body> because an hx-boost swap replaces
    // the body, which would silently drop the class mid-selection.
    document.documentElement.classList.toggle('selecting', !!on);
    var bar = document.querySelector('[data-merge-bar]');
    if (bar) {
      bar.classList.toggle('hidden', !on);
      bar.classList.toggle('flex', on);
    }
    Array.prototype.slice.call(document.querySelectorAll('[data-task-select]')).forEach(function (el) {
      el.classList.toggle('hidden', !on);
      el.classList.toggle('flex', on);
    });
    if (!on) boxes().forEach(function (b) { b.checked = false; });
    render();
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var toggle = t.closest('[data-select-toggle]');
    if (toggle) {
      e.preventDefault();
      setMode(toggle.getAttribute('aria-pressed') !== 'true');
      return;
    }
    // A winner button: stamp winner/loser BEFORE the form submits. This listener
    // runs during the BUBBLE phase of the click, which precedes the default
    // action (the form submit htmx is bound to), so the hidden inputs are always
    // current by the time the request is built.
    var keep = t.closest('[data-merge-keep]');
    if (!keep) return;
    var bar = keep.closest('[data-merge-bar]');
    var sel = checked();
    if (!bar || sel.length !== 2) { e.preventDefault(); return; }
    var winner = bar.querySelector('[data-merge-winner]');
    var loser = bar.querySelector('[data-merge-loser]');
    var slot = keep.getAttribute('data-merge-keep');
    var w = slot === 'second' ? sel[1] : sel[0];
    var l = slot === 'second' ? sel[0] : sel[1];
    winner.value = w.getAttribute('data-task-select-box');
    loser.value = l.getAttribute('data-task-select-box');
  });

  // Checkbox changes re-derive the bar. Delegated on the document because the
  // cards are re-rendered by every list swap.
  document.addEventListener('change', function (e) {
    var t = e.target;
    if (!t || !t.hasAttribute || !t.hasAttribute('data-task-select-box')) return;
    render();
  });

  // A list swap replaces the cards AND the toolbar/bar, so the mode resets to
  // off. Re-assert that explicitly rather than trusting the freshly-rendered
  // markup, so a partially-swapped list can never leave stray visible
  // checkboxes with a hidden bar.
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'tasks-list' && !selectionOn()) setMode(false);
  });
})();
`))
}
