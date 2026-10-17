package ui

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
)

// AgentBrief is the minimal linked-agent info a task card renders: enough to
// show a status chip and link to the agent's chat, with NO live k8s call (the
// Status is the STORED agents.status, mirroring how the agents list avoids
// per-agent pod lookups).
type AgentBrief struct {
	ID          int64
	Name        string // slug — the /agents/{name} detail/chat link target
	DisplayName string
	Status      string // pending|provisioning|running|stopped|error
}

// TaskCardView carries one task plus its (optional) most-recent linked agent, so
// the card can reflect the dispatch state (status chip + Open chat) instead of a
// bare Dispatch button. Agent is nil when the task has no linked agent.
type TaskCardView struct {
	Note  notes.Note
	Agent *AgentBrief

	// Detail switches the card from its BOARD shape (a compact summary whose
	// title is a stretched link to /tasks/{id}) to its DETAIL shape (the full
	// markdown body, attachments, session thread, comments and the comment form,
	// with no navigation link because you are already on that page). It is the
	// ONE predicate the two shapes differ on — see internal/api's renderNoteCard,
	// which derives it from htmx's HX-Current-URL so every existing mutation
	// route (status PATCH, comment POST/DELETE, tag add/remove, edit) re-renders
	// the card in the shape of the document that asked, without duplicating a
	// single route. It is deliberately NOT read from a header the page's markup
	// stamps: internal/api/view.go records why that leaked onto the board.
	//
	// In detail shape the title is the document's ONE <h1>; on the board it is an
	// <h3> inside the list.
	Detail bool
}

// NotesPanel is the static shell for the Tasks tab. The + FAB (body-level, in
// Page) is the sole task-create path; only the inner #tasks-list refreshes.
// It lazy-loads /ui/tasks on first load, on tasks:changed (a local create/edit),
// on sse:task.changed (an agent mutated a task out-of-band), and on
// sse:agent.changed (a linked agent's status moved — so the card's status chip
// stays live without a reload).
// 🔴 IT TAKES NO PENDING COUNT, AND THE SEGMENTED CONTROL IS GONE WITH IT.
// Upstream rendered a [Requests | Tasks] segmented toggle at the top of this
// panel, carrying a live count of pending permission requests on the Requests
// half. muster has no permission-request surface, so the control would be a
// two-position switch with one position — and the count would be a badge
// permanently reading zero, which reads as "no requests" rather than "not a
// thing this service does".
func NotesPanel(active bool) g.Node {
	return Div(
		ID(panelID("tasks")),
		g.Attr("role", "tabpanel"),
		// min-h fills the viewport below the header, so an empty board still
		// owns the page's height. Inert while `hidden`.
		// destructiveSwap=true — AND THE PANEL'S OWN hx-target IS NOT WHY. Only the
		// inner #tasks-list refreshes, with morph:innerHTML. What destroys nodes
		// inside this panel is wired one level further down, in the card body:
		// noteDeleteButton's confirm carries `hx-swap: outerHTML swap:200ms`
		// targeting `#task-<id>`, which IS inside this panel. See panelClass.
		Class(panelClass(active, true)+" min-h-[calc(100dvh-6rem)]"),
		Div(
			ID("tasks-list"),
			Class("mt-4"),
			hx("hx-get", "/ui/tasks"),
			// 🔴 `muster:resync` IS WHY resyncScript EXISTS, AND THE BOARD WAS NOT
			// LISTENING FOR IT. That script dispatches the event on focus, on
			// visibility regain and on SSE reconnect, precisely because a connection
			// dropped while the app was backgrounded can MISS the sse:task.changed
			// events this list otherwise depends on — and its own header says it
			// "keeps the pending list fresh after the app is backgrounded and
			// reopened". On this document nothing had ever subscribed, so the whole
			// mechanism was inert here: a phone reopened after an hour showed
			// whatever the list held when it was put down, until something else
			// happened to fire. The task DETAIL card has carried this trigger all
			// along, which is what made the gap invisible — the feature demonstrably
			// worked, on the other document.
			hx("hx-trigger", "load, tasks:changed from:body, sse:task.changed from:body, sse:agent.changed from:body, muster:resync from:body"),
			hx("hx-target", "#tasks-list"),
			hx("hx-swap", "morph:innerHTML"),
			// 🔴 A FAILED load must not read as "still loading". htmx does not swap a
			// non-2xx response, so without these the skeleton below simply STAYS —
			// measured with /ui/tasks forced to 500: 282px of pulsing placeholders,
			// 0 articles, indefinitely. Before the skeleton existed the container was
			// merely empty, which was at least honest. Both htmx failure events are
			// covered: responseError (the server answered 4xx/5xx) and sendError (the
			// request never completed — offline / DNS / abort).
			// The timeout makes the failure BOUNDED: a request that never completes
			// (a hung upstream) would otherwise leave the skeleton pulsing forever
			// with no event to react to. 15s aborts it into the same error state.
			//
			// 🔴 hx-request IS INHERITED, and deliberately so. htmx resolves it with
			// getValuesForElement(), which walks straight up parentElement and does
			// NOT honour hx-disinherit — so this 15s cap also applies to every
			// request-issuing DESCENDANT of #tasks-list (the status PATCH, the
			// comment POST, delete, and the dispatch/edit GETs). That was checked
			// rather than assumed, and KEPT: those are all short local calls, and a
			// bounded failure beats an unbounded hang for them too. What it must not
			// be is SILENT — resyncScript toasted responseError and sendError but had
			// no htmx:timeout listener, so an aborted child request produced no
			// feedback at all. That gap is closed there (one rule, one place), not by
			// per-child hx-request overrides.
			hx("hx-request", `{"timeout":15000}`),
			// 🔴 event.target===this on ALL THREE. These are ELEMENT listeners and
			// htmx dispatches with bubbles:true, so every failing request issued by a
			// descendant surfaced here — measured with /tasks/*/status forced to 500:
			// changing ONE card's status wiped the whole list (articles 1 → 0) and
			// replaced it with "Couldn't load tasks.", a report of a failure that did
			// not happen, on top of the correct per-request toast. Same idiom as
			// dispatchCloseOnOwnSubmit (agents.go) and the edit form's after-request.
			hx("hx-on::response-error", "if(event.target===this && window.cgTasksListFailed)window.cgTasksListFailed(this)"),
			hx("hx-on::send-error", "if(event.target===this && window.cgTasksListFailed)window.cgTasksListFailed(this)"),
			hx("hx-on::timeout", "if(event.target===this && window.cgTasksListFailed)window.cgTasksListFailed(this)"),
			// 🔴 Layout RESERVATION, not a speed indicator (the partial is ~25ms).
			// #tasks-list rendered EMPTY and grew to full height the instant the lazy
			// GET landed, so the box (and anything below it) jumped. The skeleton is
			// the container's initial child, so it has a real height on first paint
			// and the swap replaces like with like. ONE wrapper element (not N
			// siblings) so idiomorph maps it 1:1 onto NotesCards' single root <div>
			// instead of structurally morphing several loose placeholders.
			tasksListSkeleton(),
		),
	)
}

// tasksListSkeletonID is the skeleton's id.
//
// 🔴 IT HAS ONE SO THE FIRST LOAD REPLACES IT RATHER THAN MORPHING IT.
// #tasks-list swaps with morph:innerHTML, and idiomorph "soft-matches" an old
// node to a new one of the same tag unless the OLD node has an id the new one
// lacks. Without the id, the skeleton's wrapper and its children were REUSED as
// the board's first rows — a skeleton card became the select toolbar, the pill
// row a filter row — and those reused nodes moving to their new places is a
// layout shift: 0.21 on the board at 1280×800, measured by e2e/tests/
// cls.spec.ts. With the id nothing is reused: the skeleton is removed and the
// board inserted, and an inserted node shifts nothing.
const tasksListSkeletonID = "tasks-list-skeleton"

// tasksListSkeleton is the height-reserving placeholder rendered as #tasks-list's
// initial child. It reuses the app's existing skeleton idiom (animate-pulse surface
// bars, same as recentSkeleton) at the collapsed task-card's shape, so the
// container occupies a plausible height before the lazy /ui/tasks GET lands.
//
// 🔴 IT MUST MODEL EVERY PART OF THE PARTIAL THAT ALWAYS RENDERS, NOT JUST THE
// CARDS — and that is not a stylistic point, it is what the reservation MEANS.
// Adding the status filter row made the loaded partial taller while this
// placeholder stayed the same shape, and the reservation ratio measured by
// e2e/tests/tasks-mobile.spec.ts ("#tasks-list reserves its height instead of
// jumping when the lazy GET lands") fell to 0.3435 against its 0.35 floor —
// deterministically, the identical value on both attempts. The box jumped again,
// which is exactly the defect the skeleton exists to prevent.
//
// The floor was NOT the thing to move. The chip row is: the tag and project rows
// render only when a board has tags/projects, but statusFilterRow renders
// ALWAYS — it is a fixed vocabulary, not one derived from the data — so it is
// height every board pays and every skeleton must reserve.
// TestTasksSkeletonReservesTheAlwaysRenderedChipRow pins the pairing.
func tasksListSkeleton() g.Node {
	bar := func(w string) g.Node {
		return Div(Class("h-3 " + w + " animate-pulse rounded bg-s3/50"))
	}
	card := func() g.Node {
		return Div(
			Class("flex flex-col gap-2 rounded-2xl border border-line bg-s1/70 p-4 ring-1 ring-line"),
			bar("w-2/3"),
			bar("w-full"),
			bar("w-1/3"),
		)
	}
	// h-11 (44px) + pb-1 + mb-3 mirrors the real chip row's box: its buttons carry
	// min-h-[44px] and chipScroller wraps it in mb-3. Four pills — a SKELETON
	// approximating the chip row's box, not a count of it: the real row renders
	// All plus one chip per taskstatus.All() entry, which is five today. A
	// skeleton that tracked the vocabulary exactly would be a second consumer of
	// it for no gain, so this is deliberately approximate and says so.
	pill := func(w string) g.Node {
		return Div(Class("h-11 " + w + " shrink-0 animate-pulse rounded-full bg-s3/50"))
	}
	return Div(
		ID(tasksListSkeletonID),
		g.Attr("data-tasks-skeleton", ""),
		g.Attr("aria-hidden", "true"),
		Class("flex flex-col gap-3"),
		Div(
			g.Attr("data-skeleton-chip-row", ""),
			Class("flex items-center gap-2 pb-1"),
			pill("w-12"), pill("w-16"), pill("w-24"), pill("w-16"),
		),
		card(), card(), card(),
	)
}

// tasksListScript is the #tasks-list load-failure escape hatch.
//
// 🔴 The skeleton makes a FAILED load indistinguishable from a slow one: htmx
// does not swap a non-2xx response, so the placeholders just keep pulsing
// (measured with /ui/tasks forced to 500 — 282px of skeleton, 0 articles,
// indefinitely). #tasks-list's hx-on::response-error / ::send-error / ::timeout
// call cgTasksListFailed, which replaces the skeleton with an honest error + a
// Retry.
//
// It defines window functions ONLY — no delegated listener, so htmx re-executing
// this <script> on a boosted body swap is a harmless re-assignment and there is
// no once-only guard to get wrong. The Retry button's handler is bound to that
// one element with addEventListener (not an inline onclick), which also keeps it
// inside the app's CSP.
func tasksListScript() g.Node {
	return Script(g.Raw(`
(function () {
  window.cgTasksListFailed = function (el) {
    var host = el || document.getElementById('tasks-list');
    if (!host) return;
    // Already showing the error: don't rebuild it (a retry that fails again
    // would otherwise recreate the node under the user's finger). RE-ARM it
    // instead — the retry disabled the button, and leaving it disabled after a
    // second failure would be the same dead end in a different shape.
    var shown = host.querySelector('[data-tasks-error]');
    if (shown) {
      var again = shown.querySelector('[data-tasks-retry]');
      if (again) { again.disabled = false; again.textContent = 'Retry'; }
      var sub = shown.querySelector('[data-tasks-error-sub]');
      if (sub) sub.textContent = 'Still failing. Your tasks are safe — this is a load error.';
      return;
    }
    host.textContent = '';
    var box = document.createElement('div');
    box.setAttribute('data-tasks-error', '');
    box.setAttribute('role', 'alert');
    box.className = 'mt-8 flex flex-col items-center justify-center gap-2 rounded-2xl border border-line bg-s1/70 p-6 text-center ring-1 ring-line';
    var p = document.createElement('p');
    p.className = 'text-sm font-medium text-fg2';
    p.textContent = 'Couldn’t load tasks.';
    var sub = document.createElement('p');
    sub.setAttribute('data-tasks-error-sub', '');
    sub.className = 'text-xs text-muted';
    sub.textContent = 'The list request failed. Your tasks are safe — this is a load error.';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute('data-tasks-retry', '');
    btn.className = 'press mt-1 inline-flex min-h-[44px] items-center justify-center rounded-xl bg-s2 px-4 text-sm font-medium text-fg ring-1 ring-inset ring-edge transition hover:bg-s3 active:scale-95';
    btn.textContent = 'Retry';
    btn.addEventListener('click', function () { window.cgTasksListRetry(); });
    box.appendChild(p); box.appendChild(sub); box.appendChild(btn);
    host.appendChild(box);
  };

  // Retry re-issues the SAME request the container is configured for, so the
  // active tag filter (which tagScript writes into hx-get) is preserved.
  //
  // 🔴 A retry that FAILED AGAIN used to leave #tasks-list completely empty —
  // no cards, no error, no Retry button, nothing to tap. Two causes, and the
  // empty result on its own could not tell them apart:
  //   1. the host was cleared BEFORE the request went out, and htmx never swaps
  //      a non-2xx response, so the cleared container just stayed cleared;
  //   2. htmx.ajax was called with NO 'source', and htmx defaults the source to
  //      document.body (issueAjaxRequest: 'if (elt == null) elt = body') and
  //      dispatches htmx:responseError on it. #tasks-list's failure handlers are
  //      ELEMENT listeners and body is its ANCESTOR — events propagate UP, never
  //      down — so cgTasksListFailed could not run.
  // Measured on the pre-fix build: the recorded event target was 'BODY'.
  //
  // So: pass 'source: host' (the events now fire ON #tasks-list, where the
  // listeners live and where their 'event.target===this' guard holds — and where
  // hx-request's 15s timeout is inherited from, which a body-sourced request also
  // missed), and DON'T pre-clear. A successful retry's morph:innerHTML replaces
  // the error box anyway; a failed one leaves it standing.
  window.cgTasksListRetry = function () {
    var host = document.getElementById('tasks-list');
    if (!host || !window.htmx) return;
    var box = host.querySelector('[data-tasks-error]');
    if (box) {
      var btn = box.querySelector('[data-tasks-retry]');
      if (btn) { btn.disabled = true; btn.textContent = 'Retrying…'; }
    }
    try {
      var p = window.htmx.ajax('GET', host.getAttribute('hx-get') || '/ui/tasks', {
        source: host, target: '#tasks-list', swap: 'morph:innerHTML',
      });
      // htmx.ajax returns a promise; a rejected one would surface as an
      // unhandled rejection. The failure is already handled by the element
      // listeners above.
      if (p && p.catch) p.catch(function () {});
    } catch (e) {}
  };
})();
`))
}

// TasksView is everything the /ui/tasks partial renders: the task cards, the tag
// vocabulary behind the filter chip row, and the ACTIVE filters. Every active
// filter is server-side state: the server reads it off the REQUEST URL and
// renders the chips from what it read, so the chips can never disagree with the
// list beside them.
//
// 🔴 THAT IS A CLAIM ABOUT CONSISTENCY, NOT ABOUT SURVIVAL. It used to read "so
// an SSE-driven list refresh can never silently drop it", which was false:
// server-rendered state is dropped by any refetch whose URL omits it, and until
// tagScript's htmx:configRequest listener landed that was EVERY trigger-driven
// refetch (htmx captures #tasks-list's verb path at process time, so writing
// hx-get moved the attribute and not the request). What preserves a filter
// across a refresh is that listener plus the hx-get URL, both in
// internal/ui/components.go — nothing in this struct.
type TasksView struct {
	Cards      []TaskCardView
	Vocabulary []notes.TagCount
	ActiveTags []string
	// Projects is the `project:` vocabulary, rendered as its OWN filter row above
	// the general tag chips so a project is not buried among descriptive tags.
	Projects []notes.ProjectCount
	// ActiveStatus is the selected task STATUS (one of taskstatus.All()), or ""
	// for the All chip.
	//
	// ⚠ IT USED TO BE A "LANE" — a separate, coarser vocabulary in which
	// in_progress and ready_for_review shared one bucket. That layer is deleted
	// (internal/taskstatus/label.go); the chip's value is the status itself now,
	// which is also what travels in ?status= and into the SQL predicate.
	ActiveStatus string
	// Limit is the cap the server actually applied (0 = unlimited), and Total is
	// how many tasks matched BEFORE that cap. `len(Cards) < Total` is the exact
	// condition for rendering the show-more control; both come from the same
	// query, so they cannot disagree across a concurrent write.
	Limit int
	Total int
	// NextLimit is the limit the show-more control requests, or 0 when there is no
	// larger page to ask for (the list is already at maxTaskLimit — see
	// nextTaskLimit in internal/api/tags.go). The SERVER computes it rather than
	// the browser adding a page size because the server is the only side that
	// knows Total and the only side that knows its own clamp.
	//
	// It lands in #tasks-list's hx-get URL, which — via tagScript's
	// htmx:configRequest listener — is what an SSE-triggered refetch then asks
	// for. This comment used to call the URL "the one place a value survives
	// #tasks-list's SSE-triggered refetch"; that was false until the listener
	// existed, and the tag filter had been silently resetting on the same channel
	// for as long as it had shipped.
	NextLimit int
}

// NotesCards is the /ui/tasks partial: the filter chip rows, then ONE flat list
// of task cards ordered strictly newest-activity-first, then the show-more
// control — morphed into #tasks-list.
//
// 🔴 IT USED TO BE THREE SECTIONS (In progress / Open / a collapsed Done
// <details>), and losing them is the point rather than a side effect. A section
// split orders by STATUS FIRST and time second, so the single most useful
// question a board answers — "what moved most recently?" — could not be read off
// it at all: a task an agent had been working for an hour sat below a task whose
// status happened to sort higher. Status is now a per-card attribute plus a
// filter chip row, which is strictly more information in less vertical space.
//
// 🔴 data-task-section IS GONE, AND WHAT REPLACES IT IS NOT A RENAME. The old
// attribute carried the HUMAN LABEL ("In progress"), which made every assertion
// over it a SPELLED guard: any feature that spelled those two words satisfied it,
// and a relabel would have silently emptied its coverage. Each CARD now carries
// `data-task-status="<raw status>"` — the enum the store persists — which cannot
// be satisfied by prose and survives any relabelling of the chips. Every
// assertion site migrated to it (internal/ui, e2e/tests/tasks.spec.ts,
// tasks-mobile.spec.ts, task-sse-regroup.spec.ts).
//
// 🔴 The two empty states are DELIBERATELY different: with a filter active, an
// empty result means "nothing matches this filter" (+ a Clear control), NOT the
// generic "no tasks yet" all-clear. Rendering all-clear under a filter is the
// classic trap — the user reads an empty queue when their work is merely hidden.
// "A filter is active" now includes a status chip, not only tags: a status-only
// filter that matched nothing used to fall through to the all-clear.
func NotesCards(v TasksView) g.Node {
	if len(v.Cards) == 0 {
		if v.filtered() {
			// The filter rows render here TOO: a control that disappears exactly
			// when its filter matched nothing would strand the user.
			return Div(
				projectFilterRow(v.Projects, v.ActiveTags),
				tagFilterRow(v.Vocabulary, v.ActiveTags),
				statusFilterRow(v.ActiveStatus),
				tasksFilteredEmpty(v.ActiveTags, v.ActiveStatus),
			)
		}
		return Div(
			Class("mt-16 flex flex-col items-center justify-center gap-2 text-center"),
			Div(Class("text-4xl"), g.Text("📋")),
			P(Class("text-lg font-medium text-fg2"), g.Text("No tasks yet")),
			P(Class("text-sm text-muted"), g.Text("Tap + to capture a task for a working directory.")),
		)
	}
	return Div(
		projectFilterRow(v.Projects, v.ActiveTags),
		tagFilterRow(v.Vocabulary, v.ActiveTags),
		statusFilterRow(v.ActiveStatus),
		// Merge (supersede) multi-select. Both render nothing below two MERGEABLE
		// cards — there is nothing to merge — so a single-task queue looks exactly as
		// before. 🔴 The count is mergeableCards, NOT len(v.Cards): complete cards are
		// in v.Cards but can be neither side of a merge, so counting them offered
		// "Select" over a queue with nothing selectable.
		taskMergeToolbar(mergeableCards(v.Cards)),
		taskMergeBar(mergeableCards(v.Cards)),
		Div(
			g.Attr("data-tasks-flat", ""),
			Class("grid grid-cols-1 gap-3 lg:grid-cols-2"),
			g.Map(v.Cards, noteCard),
		),
		tasksShowMore(v),
	)
}

// filtered reports whether ANY board filter is narrowing the list — which is
// what selects the "nothing matches this filter" empty state over the all-clear.
// It is one predicate rather than a condition repeated at each call site, so a
// filter added later cannot be forgotten at one of them.
func (v TasksView) filtered() bool {
	return len(v.ActiveTags) > 0 || v.ActiveStatus != ""
}

// statusFilterRow renders the [All][Open][In progress][Ready for review][Done]
// chip row — ONE chip per task status, derived from taskstatus.All().
//
// 🔑 It reuses the tag row's mechanism deliberately, exactly as projectFilterRow
// does: a chip carries `data-status-filter="<status>"` and the SAME delegated
// click handler in tagScript composes it with whatever tags are selected, then
// writes the result into #tasks-list's hx-get. Composition is therefore the
// default — status AND tag AND project all narrow together — rather than
// something each combination has to be remembered for.
//
// The All chip carries an EMPTY data-status-filter, which is the same value the
// server reads as "no status predicate". One vocabulary, no special case.
//
// 🔴 THE CHIPS ARE THE STATUSES, AND THAT IS A FIX. They used to be a separate
// "lane" vocabulary that GROUPED them: one chip covered in_progress AND
// ready_for_review, and complete was spelled `done`. Measured on a 525-task
// board, clicking "In progress" returned 30 ready_for_review cards beside 20
// in_progress ones, and neither state could be selected on its own. Building the
// row from All() means a status added to the vocabulary is a chip the same day,
// and the two cannot drift again. (test:
// TestTheStatusChipSetIsTheStatusEnum.)
//
// Unlike the tag and project rows this ALWAYS renders: the statuses are a fixed
// vocabulary, not one derived from what tasks happen to exist, so there is no
// "empty toolbar on a board that never used them" case to suppress.
func statusFilterRow(active string) g.Node {
	chip := func(status, label string) g.Node {
		on := status == active
		cls := "press inline-flex min-h-[44px] min-w-[44px] shrink-0 justify-center items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium transition "
		switch {
		case on && status == "":
			cls += "bg-s3 text-fg ring-1 ring-inset ring-edge"
		case on:
			// The selected chip takes ITS status's chip colours, glyph included —
			// the same pair the status select shows on a card.
			cls += glyphChip[taskGlyphKind(status)] + " ring-1 ring-inset"
		default:
			cls += "bg-s1 text-fg2 ring-1 ring-inset ring-line hover:bg-s2 hover:text-fg"
		}
		title := "Show all tasks"
		if status != "" {
			title = "Show only " + label + " tasks"
		}
		return Button(
			Type("button"),
			g.Attr("data-status-filter", status),
			g.Attr("aria-pressed", boolAttr(on)),
			g.Attr("title", title),
			g.If(on && status != "", glyphKnock(taskGlyphKind(status))),
			Class(cls),
			g.If(status != "", statusGlyph(taskGlyphKind(status), "h-3 w-3", false)),
			g.Text(label),
		)
	}
	chips := []g.Node{chip("", "All")}
	for _, status := range taskstatus.All() {
		chips = append(chips, chip(status, taskstatus.Label(status)))
	}
	return chipScroller(Div(
		ID("status-filter-row"),
		g.Attr("data-active-status", active),
		Class("-mx-1 flex items-center gap-2 overflow-x-auto px-1 pb-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"),
		// text-muted, the lowest text role the palette tests at 4.5:1 on every
		// surface (TestThemeTokensMeetWCAG). On the previous palette this label
		// was a darker grey that measured 2.66:1 on the live board, under the AA
		// floor for text this size — and this is the word that says what the row
		// next to it DOES. Same rule on the Project row.
		Span(Class("shrink-0 text-xs font-medium uppercase tracking-wide text-muted"), g.Text("Status")),
		g.Group(chips),
	), len(chips), "mb-3")
}

// tasksShowMore renders the paging control below the flat list.
//
// 🔴 THE NEXT PAGE'S URL IS SERVER-RENDERED INTO THE BUTTON, and that is the
// whole design. #tasks-list refetches /ui/tasks on `load`, `tasks:changed`,
// `sse:task.changed` and `sse:agent.changed` — so an "expanded" flag living
// anywhere else in the DOM is wiped by
// the next SSE event that anyone anywhere triggers. Putting the limit in the URL
// is the same trick the tag filter uses.
//
// ⚠ THE URL IS NECESSARY AND WAS NOT SUFFICIENT. This comment used to end "and
// it is the only one that survives a morph", citing the tag filter as working
// precedent. Both halves were wrong: htmx reads a node's verb path ONCE at
// process time and closes over it, so writing hx-get changed the attribute and
// NOT what a trigger requested — the tag filter had been resetting on every SSE
// event since it shipped, and the limit would have done the same. What makes the
// URL survive is tagScript's htmx:configRequest listener
// (internal/ui/components.go), which rewrites the request path from the
// container's CURRENT hx-get. Regression test:
// e2e/tests/task-board-paging.spec.ts.
//
// Renders NOTHING when the page is complete (len(Cards) >= Total) or when no
// limit was applied — a "show more" under a list that is already whole is a dead
// control the user has to tap to discover.
//
// 🔴 AND NO BUTTON WHEN THERE IS NO LARGER PAGE TO ASK FOR. NextLimit is 0 once
// the applied limit has reached maxTaskLimit (internal/api/tags.go), which is
// the one case where len(Cards) < Total but tapping cannot change anything: the
// server would clamp the request straight back to the limit already rendered.
// The COUNT still renders, because that case is precisely when the board is
// truncated and saying so is the whole point of the line. Unreachable at today's
// task counts; a dead control that silently does nothing is worse than an honest
// "N of M shown" with nothing to tap.
func tasksShowMore(v TasksView) g.Node {
	if v.Limit <= 0 || len(v.Cards) >= v.Total {
		return g.Text("")
	}
	kids := []g.Node{Class("mt-4 flex flex-col items-center justify-center gap-1")}
	if v.NextLimit > v.Limit {
		kids = append(kids, Button(
			Type("button"),
			// The URL, not a page number: the click handler applies it verbatim, so
			// there is no client-side arithmetic to get out of step with the server's
			// idea of the page size.
			g.Attr("data-tasks-more", boardURL(v.ActiveTags, v.ActiveStatus, v.NextLimit)),
			Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl bg-s2 px-4 text-sm font-medium text-fg ring-1 ring-inset ring-edge transition hover:bg-s3 active:scale-95"),
			g.Text("Show more"),
		))
	}
	// The count is what makes the cap HONEST rather than a silently short
	// board: without it, a truncated list is indistinguishable from a complete
	// one, which is the same class of lie as a filter rendering "all clear".
	kids = append(kids, P(
		g.Attr("data-tasks-count", ""),
		Class("text-xs text-muted"),
		g.Text(strconv.Itoa(len(v.Cards))+" of "+strconv.Itoa(v.Total)+" shown"),
	))
	return Div(kids...)
}

// noteModalShell is the static (hidden) create-task dialog. Its body loads on
// demand from /ui/tasks/new (so the directory picker is fresh and it stays out
// of the list refresh cycle).
func noteModalShell() g.Node {
	return Div(
		ID("task-modal"),
		Class("hidden fixed inset-0 z-40 flex items-end justify-center sm:items-center"),
		Div(
			Class("absolute inset-0 bg-black/60"),
			// Dismiss is routed through cgTaskModalDismiss (taskModalScript) so an
			// in-progress EDIT is not silently discarded by a stray outside click; it
			// falls back to a plain hide if the script hasn't run yet.
			hx("hx-on:click", "if(window.cgTaskModalDismiss){window.cgTaskModalDismiss()}else{document.getElementById('task-modal').classList.add('hidden')}"),
		),
		Div(
			ID("task-modal-body"),
			// 🔴 max-h + overflow-y-auto is a SAFETY cap, not polish. The sheet has no
			// intrinsic height limit and the shell is `items-end`, so on a 390×844
			// phone the edit form's natural ~890px pushed the header — and with it the
			// ✕ — to y≈-36px, off-screen. The backdrop was 100%-covered by the body, so
			// there were ZERO tappable backdrop pixels either: the only in-app exit was
			// Save, i.e. a WRITE. Capping at 85dvh keeps ✕ on-screen, makes the body
			// scroll, and leaves a real backdrop strip to tap.
			// pb uses the safe-area inset for the same reason <main> and the FABs do:
			// the sheet is `items-end`, i.e. flush against the viewport bottom, so a
			// flat p-5 put Save under the iOS home indicator. sm:pb-5 restores the
			// symmetric padding once the sheet is centered rather than a bottom sheet.
			Class("relative z-10 max-h-[85dvh] w-full max-w-lg overflow-y-auto overscroll-contain rounded-t-2xl bg-s1 p-5 pb-[calc(1.25rem+env(safe-area-inset-bottom))] shadow-2xl ring-1 ring-line sm:rounded-2xl sm:pb-5"),
		),
		taskModalDiscardBar(),
	)
}

// taskModalDiscardBar is the task sheet's "you have unsaved changes" confirm.
func taskModalDiscardBar() g.Node { return modalDiscardBar("task-modal") }

// modalDiscardBar is the in-app "you have unsaved changes" confirm shown when a
// DIRTY sheet is dismissed by the backdrop or Escape. It is DOM-only for the same
// reason the delete control is: native confirm() is suppressed in an installed
// standalone PWA, so hx-confirm/window.confirm would never resolve and the user
// would be stuck. Hidden until cgModalDismiss(<modalID>) reveals it.
//
// 🔴 ONE pattern for BOTH sheets. The dispatch sheet holds the longest free text
// in the app (the ad-hoc prompt) and Escape used to hide it unconditionally —
// reopening refetches /ui/agents/new, so the prompt was gone. It gets this exact
// bar, keyed by id, rather than a second discard idiom.
//
// The id contract is <modalID>-discard, which cgModalDismiss/cgModalClose derive.
func modalDiscardBar(modalID string) g.Node {
	return Div(
		ID(modalID+"-discard"),
		g.Attr("data-discard-confirm", modalID),
		g.Attr("role", "alertdialog"),
		// 🔴 bottom uses the safe-area inset, not a hard-coded bottom-6: this bar is
		// position:absolute inside a full-viewport overlay, so on a notched phone a
		// literal 1.5rem put it under the home indicator.
		Class("hidden absolute bottom-[calc(1.5rem+env(safe-area-inset-bottom))] left-1/2 z-20 flex -translate-x-1/2 items-center gap-2 rounded-xl bg-s2 px-3 py-2 text-sm text-fg shadow-2xl shadow-black/50 ring-1 ring-inset ring-line"),
		Span(Class("pr-1"), g.Text("Discard unsaved changes?")),
		Button(
			Type("button"),
			g.Attr("data-discard-accept", ""),
			g.Attr("aria-label", "Discard changes"),
			Class("press inline-flex min-h-[44px] items-center justify-center rounded-lg bg-danger px-3 text-xs font-semibold text-on-danger transition hover:bg-danger/90 active:scale-95"),
			hx("hx-on:click", "window.cgModalClose && window.cgModalClose('"+modalID+"')"),
			g.Text("Discard"),
		),
		Button(
			Type("button"),
			g.Attr("data-discard-cancel", ""),
			g.Attr("aria-label", "Keep editing"),
			Class("press inline-flex min-h-[44px] items-center justify-center rounded-lg px-3 text-xs font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2 active:scale-95"),
			hx("hx-on:click", "document.getElementById('"+modalID+"-discard').classList.add('hidden')"),
			g.Text("Keep editing"),
		),
	)
}

// noteCard renders one task in one of TWO shapes, selected by v.Detail.
//
// BOARD (v.Detail == false) — the compact summary the /tasks list is made of:
// the scannable text (title and/or a clamped plain-text snippet), the task age,
// the demoted metadata row (#id, dispatch-config / provenance / sessions chips,
// tag chips, "· N comments / N files"), and the always-visible action row.
// The card no longer expands in place: its scannable text is a real anchor to
// /tasks/{id} whose ::after overlay is stretched across the whole <article>, so
// the entire card is one navigation target while keyboard focus, middle-click,
// ⌘-click and "open in new tab" all work for free.
//
// DETAIL (v.Detail == true) — the same card rendered as the body of the
// /tasks/{id} document: the title becomes the page's ONE <h1>, the snippet is
// dropped (the full rendered markdown takes over), the navigation link and the
// chevron are gone, and the full body — markdown, attachments, session thread,
// comments and the add-comment form — renders unconditionally.
//
// 🔴 ONE PREDICATE, ONE PLACE. Every mutation route (status PATCH, comment
// POST/DELETE, tag add/remove, edit) re-renders through internal/api's
// renderNoteCard, which derives Detail from the request's own HX-Current-URL.
// That is why posting a comment from the detail page does not collapse the card
// under the reader, and why none of those routes had to be duplicated. It is NOT
// derived from anything the page's markup stamps on its requests — see
// internal/api/view.go for the leak that ruled that design out.
//
// 🔴 THE <details>/<summary> DISCLOSURE IS GONE ON PURPOSE. The body is not
// duplicated between the board and the detail page; /tasks/{id} is the single
// place a task body, its session thread and its comments are rendered. The
// open-state-preserving taskCardScript that existed only to survive htmx morphs
// went with it.
func noteCard(v TaskCardView) g.Node {
	n := v.Note
	ids := strconv.FormatInt(n.ID, 10)
	snippet := markdownSnippet(n.Body, 120)
	// Count secondary content so the board card can hint "what's inside".
	extra := make([]g.Node, 0, 2)
	if c := len(n.Comments); c > 0 {
		extra = append(extra, summaryMeta(plural(c, "comment", "comments")))
	}
	if a := len(n.Attachments); a > 0 {
		extra = append(extra, summaryMeta(plural(a, "file", "files")))
	}

	// 🔴 TITLE-FIRST. The metadata row used to be the FIRST line and wrapped to
	// two lines on a phone, pushing the title — the thing you scan by — down to
	// line 3 and inflating the card to ~265px (one card per 390×844 screen; 32
	// tasks = 8.1 screens). The scannable text leads and the chips are DEMOTED
	// beneath it. No information is dropped.
	scannable := make([]g.Node, 0, 2)
	switch {
	case v.Detail:
		// The detail document's ONE <h1>. Always rendered (an untitled task falls
		// back to "Task #<id>"), because a document with zero — or with an empty —
		// h1 is the axe page-has-heading-one violation TestEveryDocumentHasExactly
		// OneH1 pins.
		scannable = append(scannable, H1(
			g.Attr("data-task-title", ""),
			Class("break-words text-base font-semibold leading-snug text-fg"),
			g.Text(taskDetailHeading(n)),
		))
	case strings.TrimSpace(n.Title) != "":
		// Display title (migration 0018). Rendered ONLY when the task carries an
		// explicit `title` — an untitled task shows just the snippet, so the
		// deliberate 0.7.x declutter (the directory chip was REMOVED) is not undone
		// for the ~all existing tasks that smuggle their title through `directory`.
		// TaskTitle() still prefers title and falls back to directory wherever a
		// display LABEL is needed.
		scannable = append(scannable, H3(
			g.Attr("data-task-title", ""),
			Class("break-words text-sm font-semibold leading-snug text-fg"),
			g.Text(strings.TrimSpace(n.Title)),
		))
	}
	// Snippet: BOARD only — the detail page renders the real body just below.
	// Omitted when the body is empty/all-markup. It clamps to ONE line when a
	// title already carries the scannable text and to two when the snippet IS the
	// headline.
	if !v.Detail && snippet != "" {
		scannable = append(scannable, P(
			Class(snippetClampClass(n)+" break-words text-sm leading-snug text-fg2"),
			g.Text(snippet),
		))
	}

	// The scannable block. On the board it IS the card's navigation link; on the
	// detail page it is plain text (you are already there).
	textBlockClass := "flex min-w-0 flex-1 flex-col gap-0.5"
	var textBlock g.Node
	if v.Detail {
		textBlock = Div(Class(textBlockClass), g.Group(scannable))
	} else {
		textBlock = A(
			Href("/tasks/"+ids),
			// 🔴 hx-boost="false" IS MANDATORY, NOT AN OPTIMISATION. This anchor
			// lives inside #tasks-list, which carries hx-get/hx-target/hx-swap, and
			// htmx attributes are INHERITED by descendants — a boosted click would
			// swap the whole /tasks/{id} DOCUMENT into #tasks-list. That is a silent
			// visual break, not an error: same trap already documented on
			// openChatButton and the session rows below.
			hx("hx-boost", "false"),
			g.Attr("data-task-link", ids),
			// card-link is the hook the merge-selection CSS suppresses (see
			// web/css/input.css): while selection mode is on, `.selecting
			// .card-link::after` drops the overlay's pointer-events so a card click
			// ticks its checkbox instead of navigating away mid-merge.
			Class("card-link "+textBlockClass+" after:absolute after:inset-0 after:content-['']"),
			g.Group(scannable),
			// A task with neither a title nor a renderable snippet would otherwise
			// give this link no accessible name at all.
			g.If(len(scannable) == 0, Span(Class("sr-only"), g.Text("Task #"+ids))),
		)
	}

	return Article(
		ID("task-"+ids),
		// 🔴 THE CARD'S VERSION, AND THE ONLY THING THAT ORDERS TWO WRITERS OF THIS
		// ELEMENT. #task-{id} has more than one unsynchronised writer: the response
		// to a mutation (status PATCH, comment POST, POST /agents from this page)
		// and the SSE-triggered re-fetch in taskDetailLive. Both morph:outerHTML
		// into the same node, and until this attribute existed the one that ARRIVED
		// last won — regardless of which read the store last.
		//
		// Measured on the unmutated tree (Playwright request/response trace of a
		// dispatch from /tasks/{id}): the SSE re-fetch was ISSUED at +233ms, BEFORE
		// the POST it is reacting to had returned, and read the task while it was
		// still `open`; the POST answered `in_progress` at +278ms. 38ms apart, in
		// the harmless order. Reverse them and the page shows `open` for a task the
		// store has as `in_progress`, and nothing corrects it — taskDetailLive
		// re-fetches only on the NEXT SSE event or a focus/visibility resync. That
		// is the cause of the FLAKY e2e spec "Dispatch from a task page actually
		// dispatches" (10s of expect timeout, 14 polls, `select[name=status]` stuck
		// at `open`).
		//
		// notes.updated_at is the version because the STORE maintains it, not the
		// renderer: SetStatus, Edit, AddTags, RemoveTags and AddComment all write
		// `updated_at=now()` in the same statement as the change (pgstore.go), so
		// it moves strictly forward with the note's own data and cannot be stamped
		// by a render that read stale rows. The htmx:beforeSwap listener in
		// resyncScript (components.go) refuses a swap whose rev is STRICTLY OLDER
		// than the one already on screen — ties always apply, so a write
		// that legitimately does not move updated_at (a comment retraction, which
		// tombstones in note_comments only) is never dropped.
		// 🔴 OMITTED ENTIRELY FOR A ZERO TIMESTAMP — do not "just format it".
		// time.Time{}.UnixNano() is -6795364578871345152, and that value WEDGES the
		// card: the guard refuses a swap whose incoming rev is strictly older, so a
		// server that renders this rev has every one of its responses refused, and
		// nothing on the page can recover except a full reload (a reload is not a
		// swap). The direction matters and is easy to get backwards — a zero rev
		// already ON SCREEN is harmless, because then everything looks newer.
		//
		// Absent is the SAFE state, not a degraded one: musterRefuseStaleCardSwap
		// returns early when either side has no rev, so the card falls back to
		// pre-guard behaviour (always apply) instead of never applying.
		//
		// Not reachable from any STORE path today — NO projection omits updated_at
		// any more. ListSummaries was the last one that did; it gained the column
		// when the idle-task reaper switched to it (it selects on updated_at, and a
		// zero is before every cutoff), so the only two ways to get a zero rev are
		// a hand-built fixture and a new projection that forgets the field. ⚠ The
		// fixture half is trivially reachable, and saying "not reachable today"
		// without that qualifier already cost
		// something: the sweep's own renderEmpty fixture was a bare
		// notes.Note{ID: 1}, so this omission silently dropped that document out of
		// TestEveryDocumentThatRendersATaskCardCarriesTheStaleGuard's reach.
		// Both are fixed (the fixture carries a timestamp; the seam now
		// cross-checks the wrapper against the rev), but the lesson is the
		// qualifier — "unreachable" was true of production and false of the tests,
		// and the tests are what this attribute's coverage runs through.
		// Pinned by TestZeroUpdatedAtOmitsTheCardRev.
		g.If(!n.UpdatedAt.IsZero(),
			g.Attr("data-card-rev", strconv.FormatInt(n.UpdatedAt.UnixNano(), 10)),
		),
		// 🔴 THE CARD'S STATUS, AS THE STORED ENUM — the structural replacement for
		// the deleted [data-task-section] wrapper.
		//
		// The old attribute lived on the SECTION and held the human LABEL ("In
		// progress"), so every assertion over it was a SPELLED guard: it could be
		// satisfied by any feature that spelled those two words, and a relabelling
		// of the section headers would have emptied its coverage without a single
		// test going red. This holds the value the STORE persists, on the element
		// the claim is actually about. It cannot be satisfied by prose, it survives
		// any amount of relabelling, and a card can no longer be "in" a status
		// without literally carrying it.
		//
		// It is the RAW status (open / in_progress / ready_for_review / complete).
		//
		// ⚠ THIS USED TO WARN AGAINST COLLAPSING IT INTO A FILTER "LANE", because
		// in_progress and ready_for_review once shared one, and an attribute that
		// collapsed them would lose the distinction that ranks a review-ready task
		// above a just-started one. The lane vocabulary is gone (internal/
		// taskstatus/label.go) and the filter chips ARE the statuses now, so there
		// is nothing left to collapse INTO — the hazard is closed rather than
		// merely avoided here.
		//
		// Rendered UNCONDITIONALLY, including for the "" a bare test fixture
		// carries: an attribute that vanishes for some cards is one a `closest()`
		// or attribute-selector probe silently skips, which is the failure mode
		// that makes a selector-based test pass while looking at nothing.
		g.Attr("data-task-status", n.Status),
		// `relative` is what the stretched link's absolutely-positioned ::after
		// resolves against — without it the overlay would size to the viewport.
		//
		// 🔴 The completed card is DIMMED rather than hidden. It used to live inside
		// a collapsed <details>; in a flat list it sits among live work, and without
		// a visual difference a board of finished tasks reads as a board of pending
		// ones. This is a treatment of the SAME card, not a second status label —
		// the card's status is already legible in the always-visible status
		// <select>, and TestNoteCardRendersStatusAndComments deliberately forbids a
		// duplicate static pill.
		Class(taskCardClass(n.Status)),
		Div(
			// data-task-summary marks the compact header block — the part of the
			// card that is identical on the board and on the detail page. It is a
			// STRUCTURAL hook: it replaced </summary> as the boundary tests slice on
			// when the disclosure was removed, so "is this chip in the scannable
			// header or buried in the body?" stays answerable.
			g.Attr("data-task-summary", ""),
			Class("flex flex-col gap-1.5 px-4 py-3"),
			// Line 1: the scannable text + age (+ a chevron on the board).
			Div(
				Class("flex items-start gap-2"),
				textBlock,
				// A live relative timestamp ('10s'/'30m'/'2h'/'3d'), kept fresh
				// client-side by relTimeScript (same widget the agent cards use).
				//
				// 🔴 ON THE BOARD IT IS THE TIME THE BOARD IS ORDERED BY
				// (notes.Note.LastActivity), NOT THE TASK'S AGE. It used to print
				// CreatedAt over a list sorted by most recent activity, so a 3-day-old
				// task an agent just worked sat above a 10-minute-old idle one with
				// "3d" beside it — and the board read as unsorted. The detail page,
				// which has no list to explain, keeps the creation age.
				taskCardTime(n, v.Detail),
				// Chevron: the "this card goes somewhere" affordance. Static now —
				// it used to rotate as the disclosure opened.
				g.If(!v.Detail, Span(
					Class("shrink-0 text-muted"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 5l6 5-6 5"/></svg>`),
				)),
			),
			// Line 2: demoted metadata.
			//
			// 🔴 What actually keeps this row to ONE line is cardTagChips capping the
			// set at 3 chips + a "+N" overflow chip — NOT min-w-0/overflow-hidden.
			// flex-wrap is still on the element and would wrap freely if the cap were
			// removed; overflow-hidden never engages at the chip counts this renders.
			// Keep the cap if you care about the board card's height.
			Div(
				// 🔴 `relative z-10 pointer-events-none [&>*]:pointer-events-auto` —
				// three utilities doing ONE job: let the chips be HOVERED without
				// letting them swallow the card's click.
				//
				// The comment on the action row below used to say these chips "need no
				// such raise — they are inert <span>s". Inert for CLICKS; not for
				// HOVER. The card-link's stretched ::after is a positioned descendant,
				// so it paints — and hit-tests — above this row, and a browser resolves
				// a title tooltip from the element the pointer actually hits. Measured
				// in Chromium 1228 on the real rendered card:
				// elementFromPoint() at the centre of the `+N` chip returned
				// <a class="card-link">, and walking up from it found NO title at all,
				// while the same probe over the Delete button (already inside the
				// z-10 action row) resolved "Delete task permanently".
				//
				// That cost the only two on-board explanations there are: the `+N`
				// overflow chip's title is the ONLY way to read a card's 4th+ tags, and
				// `gate:hold`'s title is the ONLY on-board reason a greyed-out Dispatch
				// is greyed out.
				//
				// pointer-events keeps the GAPS working: the row is raised but
				// TRANSPARENT to the pointer, so a tap between two chips still hits the
				// overlay and opens the task. Only the chip rectangles themselves opt
				// back in.
				//
				// 🔴 THAT IS NOT THE WHOLE STORY, AND THIS COMMENT USED TO CLAIM IT
				// WAS. It read "…and they carry no click behaviour to lose". The chips
				// carry none of their OWN — but they were part of the CARD's, and a
				// chip that hit-tests as itself no longer hits the anchor.
				// cardMetaNavScript (components.go) routes a chip click back to
				// `a.card-link`; the two changes are one fix and neither is correct
				// without the other. Both halves are measured in
				// e2e/tests/tasks.spec.ts.
				//
				// 🔴 HOW MUCH OF THE STRIP — THE ONE PLACE THIS IS QUOTED. The figure
				// used to read "the chips span 334px and the gaps only 30px, so ~91%",
				// repeated in four files, and it COULD NOT BE REPRODUCED because it
				// named neither its fixture nor its denominator. Re-derived at 390px on
				// the compiled CSS, board card seeded via POST /api/tasks with tags
				// [gate:hold alpha beta gamma delta]:
				//
				//	with X-Muster-Source (the e2e fixture, so a "from Claude Code"
				//	provenance chip):  row 324px, 6 chips totalling 329.5px — the row
				//	                   WRAPS, leaving essentially no gap to hit
				//	without it:        row 324px, 5 chips totalling 209.5px = 64.7%
				//
				// So the ~91% was chips as a share of chips+inter-chip-gaps, not of the
				// row's width — both readings are true of different denominators, which
				// is exactly why an unpinned number in four places was worthless. What
				// survives either reading is the claim that matters: the chips are the
				// DOMINANT surface of the strip, tooltips are hover-only, so raising
				// them alone put the whole cost on touch. Do not re-quote a percentage
				// elsewhere — cite this block, and trust the e2e test, which measures
				// the behaviour directly instead of inferring it from a ratio.
				Class("card-meta relative z-10 flex min-w-0 flex-wrap items-center gap-1.5 overflow-hidden "+
					"pointer-events-none text-xs [&>*]:pointer-events-auto"),
				// The task's durable id, shown as a muted monospace chip so a card
				// can be referenced by number (e.g. "Task #39" from a producer that
				// POSTed it and stored the returned id).
				Span(
					Class("shrink-0 rounded-md bg-s2 px-1.5 py-0.5 font-mono text-xs text-muted"),
					g.Text("#"+ids),
				),
				// Compact dispatch-config chip (Phase 1): when the task carries any
				// dispatch config, surface it as "→ model · repo · 🔒" so the card
				// shows at a glance how a Dispatch will be pre-filled.
				taskConfigChip(n),
				// Read-only source-provenance chip (migration 0017): "from <producer>
				// · session abc12345…" when the task carries a source_type, else
				// nothing (pre-0017 / header-less tasks). Nil-safe internally.
				taskProvenanceChip(n),
				// Task-thread COUNT only (migration 0023) — one chip, beside the
				// provenance chip and reusing its classes. The per-session detail is
				// one tap away on the detail page.
				taskSessionsChip(n),
				// Tag chips (migration 0018): up to 3, routing tags first, then a
				// "+N" overflow chip. An untagged task renders NOTHING here.
				cardTagChips(n.Tags),
				// The status is conveyed by the interactive status SELECT in the
				// always-visible action row below — a static status pill here would be
				// redundant. The directory (which for drafter tasks is the
				// "{cls} · {ticket_id}" id) is intentionally NOT surfaced as a chip.
				g.Group(extra),
			),
		),
		// The full body — DETAIL ONLY. This is the single place a task body, its
		// attachments, its session thread and its comments are rendered.
		g.If(v.Detail, Div(
			g.Attr("data-task-body", ""),
			Class("flex flex-col gap-3 px-4 pb-4"),
			Div(
				Class("markdown-body break-words text-sm leading-relaxed text-fg"),
				renderMarkdown(n.Body),
			),
			g.If(len(n.Attachments) > 0, noteAttachments(n.Attachments)),
			taskSessionsSection(n),
			taskComments(n.ID, n.Comments),
			taskCommentForm(n.ID),
		)),
		// Always-visible primary action row: the manual status select is always
		// kept (so the human can still mark Complete); the trailing control is the
		// Dispatch button when the task has no agent, or a live status chip +
		// "Open chat" once an agent is linked.
		// flex-wrap + justify-end so the wide in-progress trailing (status chip +
		// Open-chat CTA) drops to its OWN right-aligned row on narrow cards instead of
		// overlapping the status control (the old no-wrap row was too tight). The left
		// group (dismiss + status) is pushed left with mr-auto.
		//
		// 🔴 `relative z-10` RAISES THE WHOLE ROW ABOVE THE STRETCHED LINK'S
		// OVERLAY. Without it every control here sits UNDER the card-link ::after
		// and a tap on Dispatch / the status select / Delete would navigate to the
		// detail page instead of acting.
		//
		// This used to end "the metadata chips above need no such raise — they are
		// inert <span>s". That was wrong, and it cost both of the card's hover
		// explanations: inert for CLICKS is not inert for HOVER, and a title
		// tooltip resolves from whatever the pointer actually hits. The chip row
		// now raises too, transparently — see its own comment.
		Div(
			// data-task-actions marks the always-visible action row. Same reason as
			// data-task-summary: it is the boundary tests use to assert a control is
			// in the action row rather than inside the (detail-only) body.
			g.Attr("data-task-actions", ""),
			Class("relative z-10 flex flex-wrap items-center justify-end gap-2 border-t border-line px-4 py-2"),
			// 🔴 DELETE IS LAST. It used to be the FIRST child here — hence the first
			// tab stop of the action row — rendered as a bare ✕ exactly where a
			// "dismiss this card" affordance lives, ~50px from Edit. DELETE tears down
			// running agent pods, so it must not sit in the accidental-tap slot: it is
			// now last in DOM/tab order, moved past the status select (so it is no
			// longer adjacent to Edit), and draws a TRASH glyph rather than an ✕.
			// The two-step DOM confirm is unchanged — hx-confirm must never come back
			// (native confirm() is suppressed in the installed standalone PWA).
			Div(
				Class("mr-auto flex min-w-0 items-center gap-2"),
				// Merge checkbox — hidden unless selection mode is on. It leads the row
				// (a non-destructive control in the leftmost slot) so DELETE keeps its
				// deliberate LAST position rather than being pushed around.
				taskSelectControl(n),
				noteEditButton(n),
				taskStatusSelect(n.ID, n.Status),
				noteDeleteButton(n.ID),
			),
			taskActionTrailing(v),
		),
	)
}

// taskCardClass is the card's own class list, dimmed for a COMPLETE task.
//
// 🔴 It is keyed on the STATUS, not on a boolean the caller passes, so the
// treatment and the data-task-status attribute can never disagree about which
// card is finished.
//
// A dimmed card, not a hidden one: completed tasks used to sit inside a
// collapsed <details>, and in a flat list they sit among live work. Without a
// visual difference a board of finished tasks reads as a board of pending ones.
//
// 🔴 THE DIMMING IS A COLOUR CHANGE, NOT OPACITY. It used to be `opacity-60`,
// which scales down EVERY text pair inside the card: measured on the previous
// palette, the timestamp fell from 7.27:1 to 3.25:1, under AA. The finished card
// now sits on the page background (`bg-bg`, one step down from the live cards'
// `bg-s1`) with its title in the secondary text colour — both pairs the palette
// tests at 4.5:1 (TestThemeTokensMeetWCAG) — and every control stays at full
// strength. Pinned by TestACompletedCardIsDimmedByColourNotOpacity.
func taskCardClass(status string) string {
	base := "group relative overflow-hidden rounded-2xl border border-line shadow-lg shadow-black/20 ring-1 ring-line"
	if status == notes.StatusComplete {
		return base + " bg-bg [&_[data-task-title]]:text-fg2"
	}
	return base + " bg-s1"
}

// taskDetailHeading is the /tasks/{id} document's <h1> text: the task's display
// label, falling back to "Task #<id>" when it has neither a title nor a
// directory. The fallback is load-bearing, not cosmetic — an <h1> that renders
// no text is the same axe page-has-heading-one violation as having none, and
// TestEveryDocumentHasExactlyOneH1 asserts both halves.
func taskDetailHeading(n notes.Note) string {
	if t := strings.TrimSpace(TaskTitle(n)); t != "" {
		return t
	}
	return "Task #" + strconv.FormatInt(n.ID, 10)
}

// taskActionTrailing is the right-hand side of a task card's action row. With NO
// linked agent it is the Dispatch button (unchanged behaviour). Once an agent is
// linked it becomes a live agent-status chip (task-facing label) plus an "Open
// chat" button that navigates to the agent's chat/detail view — replacing
// Dispatch, so the card reflects that the task is being worked.
func taskActionTrailing(v TaskCardView) g.Node {
	if v.Agent == nil {
		return noteDispatchButton(v.Note)
	}
	a := v.Agent
	// shrink-0 pins the whole trailing cluster (chip + CTA) to the FAR RIGHT of the
	// now-non-wrapping action row. The chip conveys STATE; the button is the ACTION.
	return Div(
		Class("flex shrink-0 items-center gap-2"),
		agentStatusChip(a.Status),
		openChatButtonPrimary(a.Name),
	)
}

// agentStatusChip renders a task-facing chip for a linked agent's STORED
// status: provisioning→"Provisioning", running→"In progress", stopped→"Done",
// error→"Error", pending→"Queued". Each carries the status glyph
// (agentGlyphKind), so the state reads from the shape as well as the colour;
// the glyph pulses while provisioning to signal in-flight work.
func agentStatusChip(status string) g.Node {
	label := agentTaskChipLabel(status)
	return Span(
		g.Attr("aria-label", "Agent: "+label),
		Class("inline-flex items-center gap-1.5 rounded-full bg-s2 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		statusGlyph(agentGlyphKind(status), "h-2.5 w-2.5", status == agents.StatusProvisioning),
		g.Text(label),
	)
}

// agentTaskChipLabel maps an agent status to its TASK-facing label (distinct
// from the agent-facing statusLabel words).
func agentTaskChipLabel(status string) string {
	switch status {
	case agents.StatusProvisioning:
		return "Provisioning"
	case agents.StatusRunning:
		return "In progress"
	case agents.StatusStopped:
		return "Done"
	case agents.StatusError:
		return "Error"
	default: // pending (or any unknown) → queued
		return "Queued"
	}
}

// openChatButton links a task card to its dispatched agent's chat/detail view
// (GET /agents/{name}, which redirects to the agent's active session). Like the
// agent card's nav anchor it sets hx-boost="false": the button lives inside
// #tasks-list (which carries hx-get/hx-target), and htmx attrs are inherited by
// descendants, so a boosted click would swap the detail document into #tasks-list
// (wrong target). Disabling boost makes it a plain navigation to the detail page.
func openChatButton(name string) g.Node {
	return A(
		Href("/agents/"+name),
		hx("hx-boost", "false"),
		g.Attr("aria-label", "Open agent chat"),
		Class("press inline-flex h-11 shrink-0 items-center justify-center gap-1.5 rounded-full px-3 text-xs font-semibold text-accent ring-1 ring-inset ring-accent/30 transition hover:bg-accent/10 hover:text-fg"),
		g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 4h12v9H7l-3 3z"/></svg>`),
		g.Text("Open chat"),
	)
}

// openChatButtonPrimary is the LARGE, filled-accent "Open chat" CTA a task card
// shows once it has an active linked agent (provisioning/running) — the card's
// main action while work is in flight. It's the same link/target/a11y as the small
// openChatButton (plain nav, boost off), styled up: filled accent bg, taller
// (h-11), larger semibold text, more padding — clearly the primary CTA, with the
// agentStatusChip sitting alongside it (chip = state, button = action).
func openChatButtonPrimary(name string) g.Node {
	return A(
		Href("/agents/"+name),
		hx("hx-boost", "false"),
		g.Attr("aria-label", "Open agent chat"),
		Class("press inline-flex h-11 shrink-0 items-center justify-center gap-2 rounded-full bg-accent px-4 text-sm font-semibold text-on-accent shadow-sm shadow-accent/20 transition hover:bg-accent/90 active:scale-[0.98]"),
		g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 4h12v9H7l-3 3z"/></svg>`),
		g.Text("Open chat"),
	)
}

// taskConfigChip renders the compact dispatch-config chip for the collapsed card
// header: "→ <model-short> · <repo-short>", omitting any empty segment. Model is
// trimmed to its display label's
// last segment and repo to its last path segment to stay short. A task with NO
// dispatch config renders nothing.
func taskConfigChip(n notes.Note) g.Node {
	model := strings.TrimSpace(n.Model)
	repo := strings.TrimSpace(n.Repo)
	if model == "" && repo == "" {
		return g.Text("")
	}
	segs := make([]string, 0, 2)
	if model != "" {
		segs = append(segs, path.Base(shortModelLabel(model)))
	}
	if repo != "" {
		segs = append(segs, path.Base(repo))
	}
	return Span(
		Class("shrink-0 truncate rounded-md bg-s2 px-1.5 py-0.5 font-mono text-xs text-muted"),
		g.Text("→ "+strings.Join(segs, " · ")),
	)
}

// taskProvenanceChip renders the read-only source-provenance chip (migration
// 0017): "from <producer> · session abc12345…". It renders ONLY when the task
// carries a source_type (a named producer — extension/drafter/repo-cos/claude-code);
// a pre-0017 / header-less task (SourceType nil) renders nothing, matching pre-0017
// behavior. Nil-safe INTERNALLY (returns g.Text("") on nil) rather than relying on
// a g.If guard, because g.If evaluates its node argument eagerly — dereferencing a
// nil *string inside a g.If node would panic. All values are escaped via g.Text.
func taskProvenanceChip(n notes.Note) g.Node {
	if n.SourceType == nil {
		return g.Text("")
	}
	label := sourceTypeLabel(*n.SourceType)
	text := "from " + label
	if n.SourceSessionID != nil {
		// truncate appends the ellipsis itself when it cuts (a real session id is a
		// long uuid → "abc12345…"; a short one renders whole with no ellipsis).
		if sid := strings.TrimSpace(*n.SourceSessionID); sid != "" {
			text += " · session " + truncate(sid, 8)
		}
	}
	return Span(
		g.Attr("title", "Task source: "+label),
		Class("inline-flex shrink-0 items-center gap-1 rounded-md bg-s2 px-1.5 py-0.5 text-xs text-muted"),
		g.Text(text),
	)
}

// sourceTypeLabel maps a stored source_type producer id to a friendly display
// label. An unrecognized value falls back to the raw id (still escaped by g.Text
// at the call site), so a future producer isn't hidden.
func sourceTypeLabel(source string) string {
	switch source {
	case "claude-code":
		return "Claude Code"
	case "drafter":
		return "Task drafter"
	case "repo-cos":
		return "Repo chief-of-staff"
	case "extension":
		return "Browser extension"
	case "api":
		return "API"
	default:
		return source
	}
}

// taskSessionsChip renders the collapsed card's task-thread chip (migration
// 0023): "👥 3 sessions", and NOTHING else. A task with no thread renders nothing
// at all, so the collapsed card of every pre-0023 task is byte-identical to what
// it was.
//
// It reuses taskProvenanceChip's classes on purpose — the two sit adjacent, and a
// second visual weight there would read as a different KIND of information rather
// than a sibling fact. The count is the only thing on the collapsed card because
// the metadata row is already the card's densest line: it wraps at 390px, and the
// 3-chip cap on cardTagChips is what currently holds it to one line.
//
// Nil-safe by construction (len of a nil slice is 0), following the warning on
// taskProvenanceChip: g.If evaluates its node argument EAGERLY, so a guard must be
// internal rather than wrapped around a node that dereferences.
func taskSessionsChip(n notes.Note) g.Node {
	c := len(n.Sessions)
	if c == 0 {
		return g.Text("")
	}
	label := plural(c, "session", "sessions")
	return Span(
		g.Attr("data-task-sessions-chip", ""),
		g.Attr("title", "Claude Code sessions on this task's thread"),
		Class("inline-flex shrink-0 items-center gap-1 rounded-md bg-s2 px-1.5 py-0.5 text-xs text-muted"),
		g.Text("👥 "+label),
	)
}

// taskSessionsSection renders the task's thread in the EXPANDED card body: one row
// per session, oldest-joined first, each linking to that session's scroll-back page
// at /suggestions/{sessionID}.
//
// 🔴 THREE STATES, NOT TWO — and for a while this comment claimed three while
// taskSessionRow implemented two. See there for what each one now renders.
//
// The first distinction is the one migration 0023 is built around: a session whose
// cc_sessions row has been swept still renders a ROW, because "this task was worked
// by a session whose transcript is gone" and "this task was never worked" are
// completely different facts about the task. The link row is self-sufficient —
// role, project, cwd and both timestamps are stored on it — so a transcript-less
// row is still informative.
//
// The block is its own nested <details> so there is a real, session-specific
// expand control to attach the telemetry to (a cgTrack on the CARD's summary
// would fire for every expand, measuring nothing about this feature), and so the
// thread can still be folded away on a task with a long one.
//
// 🔴 IT RENDERS `open`, AND THAT IS NOT COSMETIC — a COLLAPSED <details> is what
// generated a false "the Open session chip is painted but unclickable" bug report
// (muster task 486). This used to say the collapse existed "so the thread does
// not lengthen every EXPANDED CARD by N rows". That reason died with #654: the
// section is rendered only under `g.If(v.Detail, …)`, so there is exactly ONE
// card on the page and nothing for it to lengthen. The rationale outlived the
// board-card layout it was written for (#357).
//
// The reporting trap is worth stating once, because it will recur on any of this
// page's other disclosures: Chromium gives a closed <details>'s content
// `content-visibility: hidden`, under which a descendant STILL reports a
// plausible non-zero getBoundingClientRect(), a single getClientRects() entry, a
// non-null offsetParent, and `pointerEvents: auto` / `visibility: visible` /
// `opacity: 1` — the exact three properties a prober reaches for to conclude "not
// hidden, just covered". Only `el.checkVisibility()` (false vs true, measured on
// live 0.8.23) and an actual elementFromPoint separate the two. Probe a closed
// disclosure and you get a stale rect over whatever really occupies that
// coordinate; here that was the comments block, which reads exactly like an
// overlap bug.
func taskSessionsSection(n notes.Note) g.Node {
	if len(n.Sessions) == 0 {
		return g.Text("")
	}
	ids := strconv.FormatInt(n.ID, 10)
	return g.El("details",
		g.Attr("data-task-sessions", ""),
		// Default-open: see the block comment above.
		g.Attr("open", ""),
		Class("group/s rounded-lg border-t border-line pt-3"),
		// 🔴 `toggle` on the <details>, guarded by `this.open` — NOT `click` on the
		// <summary>. With `open` rendered, the first click COLLAPSES, and a click
		// handler would have fired 'task.sessions.expanded' for it: an event whose
		// name asserts the opposite of what happened. `toggle` fires after the state
		// settles, so `this.open` is the state the user actually landed on.
		hx("hx-on:toggle", "try{if(this.open)window.cgTrack('task.sessions.expanded',{task_id:"+jsonString(ids)+",sessions:"+strconv.Itoa(len(n.Sessions))+"});}catch(e){}"),
		g.El("summary",
			g.Attr("data-task-sessions-toggle", ""),
			Class("flex cursor-pointer list-none items-center gap-2 text-xs font-semibold text-muted marker:content-['']"),
			Span(Class("shrink-0 text-muted transition group-open/s:rotate-90"),
				g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 5l6 5-6 5"/></svg>`),
			),
			g.Text("👥 "+plural(len(n.Sessions), "session", "sessions")),
		),
		Div(
			Class("mt-2 flex flex-col gap-1.5"),
			g.Map(n.Sessions, taskSessionRow),
		),
	)
}

// taskSessionRow renders ONE thread row: role · project · cwd · relative
// last-seen, linking to the session's transcript page when one still exists.
//
// 🔴 THREE STATES, AND THEY ARE THREE IN THE CODE BELOW — this function used to
// implement TWO while its own header claimed three, which is worse than claiming
// two, because a comment asserting coverage stops anyone looking:
//
//	DetailAvailable                → a live <a> to /suggestions/{id}
//	!Available && DetailSeen       → "transcript expired"      · data-session-reaped
//	!Available && !DetailSeen      → "no transcript recorded"  · data-session-unrecorded
//
// The third state is not a nicety. DetailAvailable is one bit and cannot separate
// "reaped by the 14-day sweep" from "never recorded", so every absence rendered as
// the first — "transcript expired · kept for 14 days" — including on sessions
// MINUTES old. cc_sessions has exactly one producer (the Stop hook's POST
// /api/suggest); that hook was ~96% dead from 2026-06-14 and 81 of 83 links had no
// cc_sessions row, so the false sentence was the common case AND it disguised the
// outage as ordinary retention. DetailSeen (migration 0024) is the recorded second
// bit; see notes.SessionLink.DetailSeen for why it is stored rather than inferred
// from timestamps.
func taskSessionRow(l notes.SessionLink) g.Node {
	meta := make([]string, 0, 3)
	meta = append(meta, sessionRoleLabel(l.Role))
	if p := strings.TrimSpace(l.Project); p != "" {
		meta = append(meta, p)
	}
	if c := strings.TrimSpace(l.Cwd); c != "" {
		meta = append(meta, c)
	}
	if h := strings.TrimSpace(l.Host); h != "" {
		meta = append(meta, h)
	}
	inner := []g.Node{
		Span(Class("shrink-0 font-mono text-xs text-muted"), g.Text(truncate(l.SessionID, 8))),
		Span(Class("min-w-0 flex-1 truncate text-xs text-muted"), g.Text(strings.Join(meta, " · "))),
		cardTime(l.LastSeenAt),
	}
	const rowClass = "flex items-center gap-2 rounded-lg bg-bg/60 px-3 py-1.5 ring-1 ring-inset ring-line"
	if !l.DetailAvailable {
		// No transcript page to link to. Rendered either way, never hidden — but not
		// as a link: /suggestions/{id} would 404, and a dead link that looks live is
		// worse than a plain row. The reason is stated inline so the reader is not
		// left thinking the row is broken — and WHICH reason is the point, so the
		// attribute, the label and the tooltip all switch together rather than the
		// label alone (an attribute that survives a reword is what a test can pin).
		attr, label, why := "data-session-unrecorded",
			"no transcript recorded",
			"No transcript was ever stored for this session — the muster Stop hook never reported it (it is what writes the record this row links to)"
		if l.DetailSeen {
			attr, label, why = "data-session-reaped",
				"transcript expired",
				"Transcript no longer stored — Claude Code session records are kept for 14 days"
		}
		return Div(
			g.Attr("data-task-session", ""),
			g.Attr(attr, ""),
			g.Attr("title", why),
			Class(rowClass+" opacity-70"),
			g.Group(append(inner, Span(
				Class("shrink-0 text-xs italic text-muted"),
				g.Text(label),
			))),
		)
	}
	return A(
		g.Attr("data-task-session", ""),
		Href("/suggestions/"+url.PathEscape(l.SessionID)),
		// The card lives inside #tasks-list, which carries hx-get/hx-target, and htmx
		// attributes are INHERITED by descendants — a boosted click would swap the
		// session page into #tasks-list. Same reasoning as openChatButton.
		hx("hx-boost", "false"),
		g.Attr("aria-label", "Open session "+l.SessionID),
		Class(rowClass+" transition hover:bg-s1/80"),
		g.Group(inner),
	)
}

// sessionRoleLabel maps a stored thread role to its display label. An unknown
// value falls back to the raw role (still escaped by g.Text at the call site), so a
// future role is never silently hidden.
func sessionRoleLabel(role string) string {
	switch role {
	case notes.RoleCreated:
		return "created"
	case notes.RoleWorked:
		return "worked"
	case notes.RoleRead:
		return "read"
	default:
		return role
	}
}

// TaskTitle is the task's DISPLAY label: the explicit `title` (migration 0018)
// when set, falling back to `directory` — which is what every producer smuggled
// the title through while POST /api/tasks silently dropped a `title` key
// (PR #156). One helper so every label site agrees, and so producers can migrate
// to the real field at their own pace with no behaviour change.
//
// EXPORTED because internal/api needs it too. It previously did not, and both
// dispatch-picker labels (buildTaskDispatchView, handleAgentNoteOptions) read
// n.Directory directly — so a producer that correctly sent `title` instead of
// smuggling it lost its heading in the picker, which is exactly where telling
// captures apart matters. Any new label site MUST use this, not n.Directory.
func TaskTitle(n notes.Note) string {
	if t := strings.TrimSpace(n.Title); t != "" {
		return t
	}
	return n.Directory
}

// snippetClampClass picks the body-snippet's line clamp. A titled task already
// leads with its scannable headline, so its snippet is a ONE-line supporting
// detail; an untitled task's snippet IS the headline and keeps the two-line clamp
// (which the e2e suite pins as `.line-clamp-2`). This is the single biggest
// remaining line in the collapsed card, so the distinction is worth ~23px.
func snippetClampClass(n notes.Note) string {
	if strings.TrimSpace(n.Title) != "" {
		return "line-clamp-1"
	}
	return "line-clamp-2"
}

// summaryMeta renders a muted "· N comments / N files" hint chip for the
// collapsed summary so the user knows there's more inside before expanding.
func summaryMeta(text string) g.Node {
	return Span(Class("text-xs text-muted"), g.Text("· "+text))
}

// plural renders "1 file" / "3 files".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// taskStatusValues is the ordered list of statuses with display labels, used for
// the badge and the selector.
func taskStatusValues() [][2]string {
	return [][2]string{
		{notes.StatusOpen, "Open"},
		{notes.StatusInProgress, "In progress"},
		{notes.StatusReadyForReview, "Ready for review"},
		{notes.StatusComplete, "Complete"},
	}
}

// taskStatusSelect renders a status dropdown that PATCHes /tasks/{id}/status on
// change and swaps the refreshed card in place.
func taskStatusSelect(id int64, current string) g.Node {
	if current == "" {
		current = notes.StatusOpen
	}
	ids := strconv.FormatInt(id, 10)
	opts := make([]g.Node, 0, 4)
	for _, v := range taskStatusValues() {
		attrs := []g.Node{Value(v[0]), g.Text(v[1])}
		if v[0] == current {
			attrs = append(attrs, g.Attr("selected", ""))
		}
		opts = append(opts, Option(attrs...))
	}
	// min-w-0 on the wrapper + select lets the status control SHRINK (rather than
	// push the trailing action button onto a second wrapped row) when the card is
	// narrow — the action row no longer flex-wraps, so the button stays pinned right.
	// 🔴 THE GLYPH SITS BESIDE THE SELECT, NOT INSIDE IT: a native <select>
	// cannot hold an icon. It is the current status's SHAPE, re-rendered with the
	// card on every change (the select morphs the card), and it is decorative —
	// the select itself carries the status word. It is NOT a second status pill;
	// TestNoteCardRendersStatusAndComments forbids one.
	kind := taskGlyphKind(current)
	return Div(
		Class("flex min-w-0 items-center gap-2"),
		// The visible word is redundant beside the glyph and the select's own
		// aria-label, and at phone width it is what pushed the select into
		// truncating its value ("Ready fo…"), so it is visual-only from sm up.
		Label(Class("sr-only shrink-0 text-xs font-medium text-muted sm:not-sr-only"), g.Text("Status")),
		statusGlyph(kind, "h-3.5 w-3.5", false),
		Select(
			Name("status"),
			g.Attr("aria-label", "Task status"),
			hx("hx-patch", "/tasks/"+ids+"/status"),
			hx("hx-trigger", "change"),
			hx("hx-target", "#task-"+ids),
			// Idiomorph (morph) preserves element identity + htmx bindings across the
			// swap, matching the app's list refresh; plain outerHTML re-creates the
			// nested comment form and drops its submit binding.
			hx("hx-swap", "morph:outerHTML"),
			Class("min-h-[44px] min-w-0 rounded-lg border-0 px-2.5 py-1.5 text-sm font-semibold ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus "+glyphSelect[kind]),
			g.Group(opts),
		),
	)
}

// taskComments renders a task's thread. noteID is passed in rather than read off
// each Comment.NoteID because the delete control's URL must be right even for a
// comment a caller constructed without its parent id.
//
// 🔴 A RETRACTED COMMENT KEEPS ITS ROW AND LOSES ITS BODY. It renders as a
// tombstone (commentTombstone) in the same position, with the same timestamp, so
// the thread cannot silently shorten — which is the failure mode a plain hide
// would have: a reader has no way to tell a tidied thread from an untouched one.
//
// The store already hands us Body == "" for a retracted comment (the redaction
// lives in the SQL projection, internal/notes/pgstore.go commentCols), so the
// branch below is what to DRAW, never what to withhold — the retracted text is
// not in this process. Both halves are needed: the branch without the projection
// would be CSS-grade hiding, the projection without the branch would render an
// empty comment.
func taskComments(noteID int64, comments []notes.Comment) g.Node {
	if len(comments) == 0 {
		return g.Text("")
	}
	return Div(
		Class("flex flex-col gap-2 border-t border-line pt-3"),
		g.Map(comments, func(c notes.Comment) g.Node {
			return Div(
				g.Attr("id", "comment-"+strconv.FormatInt(c.ID, 10)),
				g.If(c.Retracted, g.Attr("data-comment-retracted", "")),
				Class("rounded-lg bg-bg/60 px-3 py-2 ring-1 ring-inset ring-line"),
				Div(
					Class("mb-1 flex items-center gap-2"),
					Span(Class("text-xs font-semibold text-fg2"), g.Text(commentAuthor(c.Author))),
					Span(Class("flex-1")),
					cardTime(c.CreatedAt),
					// No retraction control on an already-retracted comment: the store
					// answers a second retraction with ErrNoRows, so the button could
					// only ever produce a 404.
					g.If(!c.Retracted, commentDeleteButton(noteID, c.ID)),
				),
				g.If(c.Retracted, commentTombstone()),
				// Comments render MARKDOWN, exactly like the task body above — an
				// external agent's completion report ships a `[PR](url)` link, code
				// spans and lists, and those must read as formatted text, not raw
				// markers. renderMarkdown is escape-FIRST (internal/ui/markdown.go), so
				// an attacker-influenceable comment body still cannot inject HTML.
				// The renderer emits its own block elements (<p class="my-1">, lists,
				// <details> code) so this is a <div> wrapper, not a <p>, and the old
				// whitespace-pre-wrap is dropped (markdown owns the line breaks now).
				g.If(!c.Retracted, Div(
					Class("markdown-body break-words text-xs leading-relaxed text-fg2"),
					renderMarkdown(c.Body),
				)),
			)
		}),
	)
}

// commentTombstoneText is the placeholder shown where a retracted comment's body
// used to be. Short, past-tense and unmistakable — it has to read as "content was
// withdrawn", not as "this comment is empty" or "loading".
const commentTombstoneText = "comment retracted"

// commentTombstone is the de-emphasised placeholder that replaces a retracted
// comment's body. Styling is the app's existing secondary idiom (text-xs
// text-muted, as summaryMeta and the empty states use) rather than anything
// alarm-coloured: a retraction is routine tidying, not an error.
//
// 🔴 It is identified by data-comment-tombstone, not by its copy. A test (or a
// stylesheet) keying off the WORDS would be satisfied by any component that
// happens to spell them, and would break the moment the copy is reworded.
//
// Mobile-first: one short line inside the comment's existing box, so a retracted
// comment occupies LESS vertical space than a live one on a 390px screen while
// still holding its place in the thread.
func commentTombstone() g.Node {
	return Div(
		g.Attr("data-comment-tombstone", ""),
		Class("text-xs italic leading-relaxed text-muted"),
		g.Text(commentTombstoneText),
	)
}

// commentDeleteButton renders the per-comment retraction control: a small trash
// glyph that reveals an inline "Delete ✓ / ✕" pair, then fires
// hx-delete /tasks/{id}/comments/{cid}.
//
// It copies noteDeleteButton's TWO-STEP DOM CONFIRM verbatim in mechanism, and
// that is not stylistic: native confirm()/alert() are suppressed or auto-dismissed
// in an installed standalone PWA, so hx-confirm silently never resolves on the
// phone and the DELETE never fires. Do not "simplify" this to hx-confirm.
//
// Sizing is mobile-first — the tap targets are the same h-8/h-9 rounded controls
// the card's own actions use, so a thumb can hit them, and the trash sits in the
// comment's existing header row rather than adding a line.
//
// The retraction is SOFT (migration 0021): the row survives, so the copy says
// "Retract" rather than promising permanence, and the glyph is a trash can (an
// unlabelled ✕ in a header row reads as "collapse this").
func commentDeleteButton(noteID, commentID int64) g.Node {
	ids := strconv.FormatInt(noteID, 10)
	cids := strconv.FormatInt(commentID, 10)
	return Div(
		Class("flex shrink-0 items-center"),
		g.Attr("data-comment-delete-control", ""),
		Button(
			Type("button"),
			g.Attr("data-comment-delete-trigger", ""),
			g.Attr("title", "Retract this comment (hidden from the board; the row is kept)"),
			g.Attr("aria-label", "Delete comment"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-full text-muted transition hover:bg-s2 hover:text-st-error-fg"),
			hx("hx-on:click", "var c=this.closest('[data-comment-delete-control]');c.querySelector('[data-comment-delete-trigger]').classList.add('hidden');c.querySelector('[data-comment-delete-confirm]').classList.remove('hidden')"),
			g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6h12M8 6V4.5A.5.5 0 0 1 8.5 4h3a.5.5 0 0 1 .5.5V6M6 6l.7 9a1 1 0 0 0 1 .9h4.6a1 1 0 0 0 1-.9L14 6M8.5 9v4M11.5 9v4"/></svg>`),
		),
		Div(
			g.Attr("data-comment-delete-confirm", ""),
			Class("hidden flex items-center gap-1"),
			Button(
				Type("button"),
				g.Attr("aria-label", "Confirm delete comment"),
				Class("press inline-flex h-11 items-center justify-center gap-1 rounded-full bg-danger px-2.5 text-[0.7rem] font-semibold text-on-danger transition hover:bg-danger/90"),
				hx("hx-delete", "/tasks/"+ids+"/comments/"+cids),
				hx("hx-target", "#task-"+ids),
				// Morph, like the comment FORM's swap: a plain outerHTML swap of the
				// card drops the nested add-comment form's htmx submit binding, so the
				// next comment would post as a native form navigation.
				hx("hx-swap", "morph:outerHTML"),
				hx("hx-disabled-elt", "this"),
				g.Text("Retract ✓"),
			),
			Button(
				Type("button"),
				g.Attr("aria-label", "Cancel delete comment"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-full text-muted ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"),
				hx("hx-on:click", "var c=this.closest('[data-comment-delete-control]');c.querySelector('[data-comment-delete-confirm]').classList.add('hidden');c.querySelector('[data-comment-delete-trigger]').classList.remove('hidden')"),
				g.Text("✕"),
			),
		),
	)
}

func commentAuthor(author string) string {
	if author == "" {
		return "user"
	}
	return author
}

// taskCommentForm renders the inline add-comment form; on submit it swaps the
// refreshed card (with the new comment and a cleared form) in place.
func taskCommentForm(id int64) g.Node {
	ids := strconv.FormatInt(id, 10)
	return Form(
		hx("hx-post", "/tasks/"+ids+"/comments"),
		hx("hx-target", "#task-"+ids),
		// Morph (idiomorph) so the form keeps its htmx submit binding for the next
		// comment and the status select stays live after the swap.
		hx("hx-swap", "morph:outerHTML"),
		Class("flex items-end gap-2"),
		Textarea(
			Name("body"),
			Rows("1"),
			Required(),
			Placeholder("Add a comment…"),
			Class("min-h-[2.25rem] w-full resize-y rounded-lg border-0 bg-bg px-3 py-2 text-xs text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Button(
			Type("submit"),
			Class("press inline-flex min-h-[44px] shrink-0 items-center justify-center rounded-lg bg-s2 px-3 py-2 text-xs font-medium text-fg ring-1 ring-inset ring-edge transition hover:bg-s3"),
			hx("hx-disabled-elt", "this"),
			g.Text("Comment"),
		),
	)
}

func noteAttachments(atts []notes.Attachment) g.Node {
	return Div(
		Class("flex flex-wrap gap-2 pt-1"),
		g.Map(atts, func(a notes.Attachment) g.Node {
			return A(
				Href("/tasks/"+strconv.FormatInt(a.NoteID, 10)+"/attachments/"+strconv.FormatInt(a.ID, 10)),
				g.Attr("target", "_blank"),
				// Real load (boost off): this opens the raw attachment file in a new tab
				// (target=_blank). It is a binary download, not an SPA partial — boosting
				// it would try to body-swap file bytes into the document.
				hx("hx-boost", "false"),
				Class("inline-flex items-center gap-1.5 rounded-lg bg-s2/80 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s3/80 hover:text-fg"),
				g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><path d="M8 11.5l4-4a2 2 0 0 0-3-3l-5 5a3.5 3.5 0 0 0 5 5l4.5-4.5"/></svg>`),
				g.Text(a.Filename),
			)
		}),
	)
}

// noteDeleteButton renders an in-app, two-step delete control. The trash button
// reveals an inline "Confirm ✓ / Cancel ✕" pair (toggling a sibling's `hidden`
// class) instead of firing a native window.confirm(). Native confirm()/alert()
// are suppressed/auto-dismissed in installed standalone PWAs, so hx-confirm
// silently never resolved on mobile and the DELETE never fired — this replaces
// it with DOM-only confirmation that works in standalone display mode. Confirm
// fires hx-delete /tasks/{id}; hx-disabled-elt="this" guards against a double-tap.
func noteDeleteButton(id int64) g.Node {
	ids := strconv.FormatInt(id, 10)
	return Div(
		Class("flex shrink-0 items-center"),
		g.Attr("data-delete-control", ""),
		// Trigger: reveals the confirm pair, hides itself.
		//
		// 🔴 The glyph is a TRASH CAN, not an ✕. An unlabelled ✕ in a card's action
		// row reads as "dismiss this card"; this button DELETES the task and tears
		// down any running agent pod. aria-label stays the verbatim "Delete task"
		// (pinned by the e2e suite) and title now spells out the consequence.
		Button(
			Type("button"),
			g.Attr("data-delete-trigger", ""),
			g.Attr("title", "Delete task permanently"),
			g.Attr("aria-label", "Delete task"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-full text-muted transition hover:bg-s2 hover:text-st-error-fg"),
			hx("hx-on:click", "var c=this.closest('[data-delete-control]');c.querySelector('[data-delete-trigger]').classList.add('hidden');c.querySelector('[data-delete-confirm]').classList.remove('hidden')"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6h12M8 6V4.5A.5.5 0 0 1 8.5 4h3a.5.5 0 0 1 .5.5V6M6 6l.7 9a1 1 0 0 0 1 .9h4.6a1 1 0 0 0 1-.9L14 6M8.5 9v4M11.5 9v4"/></svg>`),
		),
		// Confirm pair: hidden until the trash is tapped.
		Div(
			g.Attr("data-delete-confirm", ""),
			Class("hidden flex items-center gap-1"),
			Button(
				Type("button"),
				g.Attr("aria-label", "Confirm delete task"),
				Class("press inline-flex h-11 items-center justify-center gap-1 rounded-full bg-danger px-3 text-xs font-semibold text-on-danger transition hover:bg-danger/90"),
				hx("hx-delete", "/tasks/"+ids),
				hx("hx-target", "#task-"+ids),
				hx("hx-swap", "outerHTML swap:200ms"),
				hx("hx-disabled-elt", "this"),
				g.Text("Confirm ✓"),
			),
			Button(
				Type("button"),
				g.Attr("aria-label", "Cancel delete task"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-full text-muted ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"),
				hx("hx-on:click", "var c=this.closest('[data-delete-control]');c.querySelector('[data-delete-confirm]').classList.add('hidden');c.querySelector('[data-delete-trigger]').classList.remove('hidden')"),
				g.Text("✕"),
			),
		),
	)
}

// noteDispatchButton opens the SHARED dispatch modal pre-selected to this task, so
// the operator adjudicates → dispatches an agent for it in one tap. It mirrors the
// FAB's exact open mechanism (hx-get the form body into #agent-modal-body, then
// un-hide #agent-modal) but adds ?note=<id>&label=<short> so the "Existing task"
// combobox is pre-filled. The label is the task's directory, falling back to a
// truncated first line of the body.
//
// A `gate:<reason>` routing tag makes the task NOT dispatchable: the button
// renders DISABLED with the reason as its title (and the dispatch endpoint 409s —
// this is a hint, not the enforcement).
func noteDispatchButton(n notes.Note) g.Node {
	if reason, gated := notes.GateReason(n.Tags); gated {
		return Button(
			Type("button"),
			Disabled(),
			g.Attr("data-dispatch-gated", reason),
			g.Attr("title", "Gated: "+reason+" — remove the gate: tag to dispatch"),
			g.Attr("aria-label", "Dispatch blocked: "+reason),
			Class("press inline-flex h-11 shrink-0 cursor-not-allowed items-center justify-center gap-1.5 rounded-full px-3 text-xs font-semibold text-muted ring-1 ring-inset ring-line"),
			g.Text("Dispatch"),
		)
	}
	label := TaskTitle(n)
	if label == "" {
		label = truncate(firstLine(n.Body), 60)
	}
	q := url.Values{}
	q.Set("note", strconv.FormatInt(n.ID, 10))
	q.Set("label", label)
	return Button(
		Type("button"),
		g.Attr("title", "Dispatch agent for this task"),
		g.Attr("aria-label", "Dispatch agent for this task"),
		Class("press inline-flex h-11 shrink-0 items-center justify-center gap-1.5 rounded-full px-3 text-xs font-semibold text-accent ring-1 ring-inset ring-accent/30 transition hover:bg-accent/10 hover:text-fg"),
		// Mirror shellFAB: load the dispatch form into the modal body, then reveal
		// the always-present modal shell.
		hx("hx-get", "/ui/agents/new?"+q.Encode()),
		hx("hx-target", "#agent-modal-body"),
		hx("hx-swap", "innerHTML"),
		hx("hx-on:click", modalOpenClick("agent-modal")),
		g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 15l4-4M9 11l5-9M9 11l-4 4M14 2l4 4-9 5"/></svg>`),
		g.Text("Dispatch"),
	)
}

// noteEditButton renders the card's Edit control. When the task is in progress it
// renders a DISABLED button with a hint (an in-progress task is immutable — the
// same guard the edit route enforces server-side); otherwise it opens the SHARED
// task modal pre-filled with the task's current values (mirrors the FAB: hx-get the
// edit form body into #task-modal-body, then un-hide #task-modal).
func noteEditButton(n notes.Note) g.Node {
	ids := strconv.FormatInt(n.ID, 10)
	if n.Status == notes.StatusInProgress {
		return Button(
			Type("button"),
			Disabled(),
			g.Attr("title", "Can't edit while in progress — change the status first"),
			g.Attr("aria-label", "Edit disabled while in progress"),
			Class("press inline-flex h-11 shrink-0 cursor-not-allowed items-center justify-center gap-1.5 rounded-full px-3 text-xs font-semibold text-muted ring-1 ring-inset ring-line"),
			g.Text("Edit"),
		)
	}
	return Button(
		Type("button"),
		g.Attr("data-edit-trigger", ""),
		g.Attr("title", "Edit task"),
		g.Attr("aria-label", "Edit task"),
		Class("press inline-flex h-11 shrink-0 items-center justify-center gap-1.5 rounded-full px-3 text-xs font-semibold text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"),
		hx("hx-get", "/ui/tasks/"+ids+"/edit"),
		hx("hx-target", "#task-modal-body"),
		hx("hx-swap", "innerHTML"),
		hx("hx-on:click", modalOpenClick("task-modal")),
		g.Text("Edit"),
	)
}

// NoteEditView carries everything the edit form needs pre-resolved server-side:
// the task (its current values) and the directory picker options.
type NoteEditView struct {
	Note notes.Note
	// Directories is the SEED set only (notes.DefaultDirectoryLimit most-recent
	// paths), never the whole archive — the rest is reached through the picker's
	// search route. See directoryCombobox.
	Directories []string
	// DirectoriesFailed reports that the directory READ ERRORED, as distinct from
	// "there are no directories". The two used to render identically, which is how
	// a picker that had been empty in production since it shipped stayed
	// unnoticed. See directoryPickerFailed.
	DirectoriesFailed bool
	// Vocabulary backs the tag input's <datalist> suggestions, so the human reuses
	// existing labels instead of minting near-duplicates.
	Vocabulary []notes.TagCount
}

// NotesEditModalBody is the /ui/tasks/{id}/edit partial: the EDIT-task form,
// pre-filled with the task's current directory/body/model/repo/branch,
// loaded into #task-modal-body when the card's Edit button is tapped. It posts
// (urlencoded) to /tasks/{id}/edit, morphs the refreshed single card into
// #task-{id}, and closes the modal on success. Only the EDITABLE fields appear —
// status, provenance, comments and attachments are not editable here. All values
// are escaped by g.Text / attribute encoding.
func NotesEditModalBody(v NoteEditView) g.Node {
	n := v.Note
	ids := strconv.FormatInt(n.ID, 10)
	return g.Group{
		Div(
			Class("mb-4 flex items-center gap-3"),
			H2(Class("text-base font-semibold text-fg"), g.Text("Edit task #"+ids)),
			Span(Class("flex-1")),
			Button(
				Type("button"),
				g.Attr("aria-label", "Close"),
				Class("press inline-flex h-11 w-11 min-h-[44px] items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
				// Close discards deliberately (the user aimed at ✕), so it closes
				// outright — only the ambiguous dismissals (backdrop / Escape) are
				// dirty-guarded.
				hx("hx-on:click", "if(window.cgTaskModalClose){window.cgTaskModalClose()}else{document.getElementById('task-modal').classList.add('hidden')}"),
				g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
			),
		),
		Form(
			// data-task-edit-form scopes the modal's DIRTY guard to the EDIT form.
			// The create form is deliberately excluded: advancedDefaults pre-fills
			// directory/model from localStorage recents, so a returning user's brand
			// new create form is "dirty" the instant it opens and the backdrop would
			// stop closing it.
			g.Attr("data-task-edit-form", ids),
			hx("hx-post", "/tasks/"+ids+"/edit"),
			// The refreshed single card morphs into #task-<id> (idiomorph preserves the
			// nested comment form's binding + the status select, like the status PATCH).
			hx("hx-target", "#task-"+ids),
			hx("hx-swap", "morph:outerHTML"),
			// 🔴 hx-disabled-elt belongs on the FORM, not on the submit button.
			// htmx reads hx-disabled-elt from the element that ISSUES the request;
			// a submit button's own `hx-disabled-elt="this"` only applies to a
			// request the BUTTON initiates, and this request is initiated by the
			// form's submit event — so the attribute that was already on the button
			// could provably never fire. Measured at 390×844: the button stayed
			// ENABLED for the whole in-flight window and the POST landed TWICE.
			hx("hx-disabled-elt", "find button[type='submit']"),
			// Close only on THIS form's own successful submit (afterRequest bubbles).
			// cgTaskModalSaved pulses the EXISTING .card-enter highlight (accent ring,
			// already in web/css/input.css and already honoured by the
			// prefers-reduced-motion block) on the morphed card — the save's only
			// success feedback, since the modal simply vanished before.
			hx("hx-on::after-request", "if(event.target===this && event.detail.successful){if(window.cgTaskModalSaved)window.cgTaskModalSaved('task-"+ids+"');if(window.cgTaskModalClose){window.cgTaskModalClose()}else{document.getElementById('task-modal').classList.add('hidden')}}"),
			Class("flex flex-col gap-3"),
			// Title (migration 0018): the real display title, no longer smuggled
			// through `directory`. Optional — empty falls back to the directory.
			labelledField("Title",
				Input(
					Type("text"),
					Name("title"),
					Value(n.Title),
					Placeholder("short title (optional)"),
					Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
			),
			labelledField("Task",
				Textarea(
					Name("body"),
					Rows("4"),
					Required(),
					g.Attr("autofocus", ""),
					Placeholder("What needs doing?"),
					Class("w-full resize-y rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
					g.Text(n.Body),
				),
			),
			// Tags: chip editor (hidden `tag` inputs = the submitted set) + a
			// vocabulary-backed suggestion list. Routing chips lock on an in-progress
			// task (the server enforces the same refinement of the 409 rule).
			labelledField("Tags", tagEditor(n.Tags, v.Vocabulary, n.Status == notes.StatusInProgress)),
			// Dispatch config (all editable) — shown expanded (not hidden behind
			// Advanced) since editing config is the whole point of the edit modal.
			labelledField("Directory", directoryCombobox(v.Directories, n.Directory, v.DirectoriesFailed)),
			labelledField("Model", modelFieldFor(n.Model, "")),
			labelledField("Repository",
				lazyComboboxPreselect("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos", n.Repo, n.Repo)),
			labelledField("Branch",
				Input(
					Type("text"),
					Name("repo_branch"),
					Value(n.RepoBranch),
					Placeholder("branch (optional — repo default)"),
					Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
			),
			// 🔴 NO hx-disabled-elt here. It carried `this` and was provably inert:
			// htmx reads hx-disabled-elt from the element that ISSUES the request, and
			// this request is issued by the FORM's submit event, so a button-level
			// `this` could never fire. The live attribute is the form's
			// `find button[type='submit']` above; leaving a dead copy here reads as a
			// second, working guard.
			Button(
				Type("submit"),
				Class("sticky bottom-0 z-10 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))] press mt-1 inline-flex items-center justify-center gap-2 rounded-xl bg-accent px-4 py-3 text-base font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
				g.Text("Save changes"),
			),
		),
	}
}

// NotesModalBody is the /ui/tasks/new partial: the create-task form, loaded into
// #task-modal-body when the FAB is tapped. It posts multipart/form-data to
// /tasks, morphs the result into #tasks-list, and closes the modal on success.
func NotesModalBody(directories []string, directoriesFailed bool) g.Node {
	return NotesModalBodyWith(directories, directoriesFailed, "")
}

// NotesModalBodyWith is NotesModalBody with the task text pre-filled — what a
// share or the "New task" shortcut opens (ComposePage). The text is a Textarea
// CHILD, so gomponents escapes it: shared markup arrives as inert characters.
func NotesModalBodyWith(directories []string, directoriesFailed bool, prefill string) g.Node {
	return g.Group{
		Div(
			Class("mb-4 flex items-center gap-3"),
			H2(Class("text-base font-semibold text-fg"), g.Text("New task")),
			Span(Class("flex-1")),
			Button(
				Type("button"),
				g.Attr("aria-label", "Close"),
				Class("press inline-flex h-11 w-11 min-h-[44px] items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
				hx("hx-on:click", "if(window.cgTaskModalClose){window.cgTaskModalClose()}else{document.getElementById('task-modal').classList.add('hidden')}"),
				g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
			),
		),
		Form(
			hx("hx-post", "/tasks"),
			hx("hx-encoding", "multipart/form-data"),
			hx("hx-target", "#tasks-list"),
			hx("hx-swap", "morph:innerHTML"),
			// Scope the close to THIS form's own submit: htmx:afterRequest bubbles, so
			// any child hx-get (e.g. a lazy field load) would otherwise close the modal
			// on swap-in. event.target===this fires only for the form's POST /tasks.
			hx("hx-on::after-request", "if(event.target===this && event.detail.successful) document.getElementById('task-modal').classList.add('hidden')"),
			Class("flex flex-col gap-3"),
			// The task text is the ONE thing that matters for a quick capture — it is
			// the sole visible field. Directory + the optional dispatch config
			// (model/repo) live under a collapsed "Advanced" disclosure so
			// the form is delightfully simple by default while power-use still works.
			labelledField("Task",
				Textarea(
					Name("body"),
					Rows("4"),
					Required(),
					g.Attr("autofocus", ""),
					Placeholder("What needs doing? (a sentence is plenty)"),
					Class("w-full resize-y rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
					g.If(prefill != "", g.Text(ClampComposeBody(prefill))),
				),
			),
			// Advanced: directory + optional dispatch config (model/repo).
			// A task can carry the model/repo it should dispatch with, so
			// its Dispatch button becomes a pre-filled confirm. Reuses the exact
			// dispatch-modal controls; their field names (directory / model / repo)
			// match what handleNoteCreate parses. advancedDefaults JS
			// pre-fills directory/model from localStorage recents (last-used) so a
			// returning user's sane defaults ride along even while collapsed.
			advancedDisclosure(
				labelledField("Directory", directoryCombobox(directories, "", directoriesFailed)),
				labelledField("Model", modelField()),
				labelledField("Repository", lazyCombobox("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos")),
			),
			// labelledFieldFor (not labelledField): the file input had NO associated
			// label at all, which axe reports as a CRITICAL `label` violation — an
			// unlabelled file picker announces as a bare "button". The visible
			// "Attachments" text is now a real <label for>.
			labelledFieldFor("task-attachments", "Attachments",
				Input(
					ID("task-attachments"),
					Type("file"),
					Name("attachments"),
					g.Attr("multiple", ""),
					Class("block w-full text-sm text-muted file:mr-3 file:rounded-lg file:border-0 file:bg-s2 file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-fg hover:file:bg-s3"),
				),
			),
			Button(
				Type("submit"),
				Class("sticky bottom-0 z-10 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))] press mt-1 inline-flex items-center justify-center gap-2 rounded-xl bg-accent px-4 py-3 text-base font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
				hx("hx-disabled-elt", "this"),
				g.Text("Save task"),
			),
		),
	}
}

// DirectorySearchPath is the route the directory combobox searches against. It
// is a CONSTANT shared by the renderer and the route table so the two cannot
// drift — a picker pointed at a path nothing serves degrades to "seed only"
// with no error anywhere, which is the failure mode this whole component exists
// to stop happening twice.
const DirectorySearchPath = "/api/directories"

// directoryCombobox renders the task directory picker.
//
// 🔴 IT IS A SEED PLUS A SERVER-SIDE SEARCH, NOT A DUMP. This used to be a
// <datalist> holding EVERY known directory as an <option>. The live archive
// holds 1,532 of them totalling 114,122 characters of path text, so at 26 bytes
// of markup per <option> that shape is ~154 KB shipped into the DOM on every
// single modal open — and the picker was empty anyway, because the query behind
// it read a table that is empty by design (see notes.PGStore.Directories).
// Repointing the query without changing the shape would have turned an invisible
// bug into a 154 KB one.
//
// The shape instead:
//
//   - `seed` — notes.DefaultDirectoryLimit most-recently-used directories,
//     rendered INLINE as data-combobox-seed options. They are in the markup
//     before any network call, so the picker is never empty while a request is
//     in flight, and they cost 6,789 bytes rather than ~154 KB (measured by
//     TestDirectoryPickerPayloadStaysSmall, which also logs both numbers).
//   - EVERYTHING ELSE — reached by typing. The combobox's remote mode
//     (data-combobox-remote, internal/ui/components.go) fires a 200 ms-debounced,
//     per-query-cached GET to DirectorySearchPath?q=…, which substring-matches
//     the WHOLE archive server-side and answers at most
//     notes.MaxDirectoryResults paths as a JSON array — ~1.9 KB. So no single
//     interaction is large, and no directory is unreachable.
//   - ANYTHING AT ALL — typed free text. This is VALUE MODE: there is no
//     data-combobox-hidden sibling, so the visible input IS the submitted
//     `directory` field (cbPick writes the picked path into it). A path that has
//     never appeared in a permission request still submits exactly as typed,
//     which is the property the old <input list=…> had and must not lose.
//
// `failed` is the read's error state travelling WITH the field — see the
// directoryPickerFailed notice for why silence was not an option.
func directoryCombobox(seed []string, current string, failed bool) g.Node {
	const listID = "combobox-list-directory"

	opts := make([]g.Node, 0, len(seed))
	for _, d := range seed {
		opts = append(opts, seededComboOption(ComboOption{Value: d, Label: d}))
	}

	state := "ok"
	if failed {
		state = "failed"
	}

	return Div(
		g.Attr("data-combobox", ""),
		g.Attr("data-combobox-remote", DirectorySearchPath),
		// 🔴 THE STATE, AS AN ATTRIBUTE, NOT AS THE NOTICE'S WORDS. A test (or an
		// operator with devtools) reads data-directories="failed"; the sentence
		// below can be reworded without either losing its handle.
		g.Attr("data-directories", state),
		Class("relative"),
		Input(
			Type("text"),
			Name("directory"),
			g.Attr("data-combobox-input", ""),
			g.Attr("role", "combobox"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", listID),
			// labelledField's <label> is a SIBLING, not an ancestor, so it confers no
			// accessible name (see labelledFieldFor). The old input had none at all.
			g.Attr("aria-label", "Directory — recent directories, or type to search"),
			g.Attr("autocomplete", "off"),
			Value(current),
			Placeholder("/path/to/project (optional)"),
			Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Ul(
			ID(listID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			Class("hidden absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			g.Group(opts),
		),
		directoryPickerFailed(failed),
	)
}

// directoryPickerFailed renders the warning-coloured one-liner shown when the directory
// read ERRORED.
//
// 🔴 THIS IS THE SIGNAL THAT WAS MISSING, AND THE REASON IS WORTH READING.
// Both call sites used to do `dirs = nil // non-fatal: the picker is just empty`
// and log. That is two distinct facts collapsed into one observable: "you have
// no directory history" and "the read broke" both render as an empty picker,
// and an operator cannot tell them apart. The log line was there the whole time
// and nobody read it — logs are where a UI defect goes to be invisible.
//
// So the state travels with the field and says which one it is. This is the same
// discipline chiefThreadRows already applies to the thread list ("Could not
// search threads just now — this is not an empty result"), and it deliberately
// does NOT block anything: the input above stays editable and the form still
// submits, because a missing suggestion list must never stop someone filing a
// task.
//
// ⚠ AND IT IS HONEST ABOUT WHAT IT DOES NOT COVER. The bug this component was
// built for produced NO ERROR — the old query succeeded and returned zero rows —
// so this notice would never have fired for it, and nothing rendered at runtime
// could have. That case is covered by a TEST instead
// (notes.TestDirectoriesReadsRequestHistory), which seeds request_history with
// `requests` left empty and demands a non-empty answer. Do not read this notice
// as a guard against the empty-picker class generally; it guards the half a
// runtime signal CAN see.
func directoryPickerFailed(failed bool) g.Node {
	if !failed {
		return g.Text("")
	}
	return P(
		Class("text-xs text-st-warning-fg"),
		g.Text("Directory suggestions could not be loaded — this is not an empty list. Type a path to file the task anyway."),
	)
}

func labelledField(label string, control g.Node) g.Node {
	return Div(
		Class("flex flex-col gap-1.5"),
		Label(Class("text-xs font-medium text-muted"), g.Text(label)),
		control,
	)
}

// labelledFieldFor is labelledField with a REAL for/id association. Use it for
// any control whose accessible name must come from the label — labelledField's
// bare <label> only associates when the control is a DESCENDANT of it, which it
// is not here, so a control rendered through labelledField has no accessible
// name unless it supplies its own. forID must match the control's id.
func labelledFieldFor(forID, label string, control g.Node) g.Node {
	return Div(
		Class("flex flex-col gap-1.5"),
		Label(g.Attr("for", forID), Class("text-xs font-medium text-muted"), g.Text(label)),
		control,
	)
}

// advancedDisclosure wraps power-user fields (directory / model / repo) in a
// collapsed native <details> so the default form is just the
// essentials. No JS — the same disclosure primitive the agent menu's Delete step
// and the chat's tool chips use (boost-safe, keyboard/a11y-correct). data-advanced marks it for
// the e2e state-machine assertions. Subtle styling so it never competes with the
// primary action.
func advancedDisclosure(fields ...g.Node) g.Node {
	return Details(
		g.Attr("data-advanced", ""),
		Class("group/adv rounded-lg ring-1 ring-inset ring-line"),
		Summary(
			g.Attr("aria-label", "Advanced options"),
			Class("press flex cursor-pointer list-none items-center gap-1.5 rounded-lg px-3 py-2 text-xs font-medium text-muted transition hover:text-fg [&::-webkit-details-marker]:hidden"),
			Span(Class("text-sm"), g.Text("⚙")),
			g.Text("Advanced"),
			Span(Class("flex-1")),
			Span(Class("text-xs opacity-70 transition group-open/adv:rotate-180"), g.Text("▾")),
		),
		Div(Class("flex flex-col gap-3 px-3 pb-3 pt-1"), g.Group(fields)),
	)
}

// taskCardTime is a task card's header timestamp: on the BOARD, the task's last
// activity (the board's sort key, labelled as such for a screen reader and on
// hover); on the DETAIL page, its creation age. See the call site in noteCard.
func taskCardTime(n notes.Note, detail bool) g.Node {
	if detail {
		return cardTime(n.CreatedAt)
	}
	t := n.LastActivity()
	if t.IsZero() {
		return g.Text("")
	}
	return Span(
		g.Attr("data-task-activity", strconv.FormatInt(t.UnixMilli(), 10)),
		g.Attr("aria-label", "last activity"),
		Class("shrink-0 self-center"),
		cardTime(t),
	)
}
