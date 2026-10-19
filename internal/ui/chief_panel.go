package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE CHIEF SLIDE-OUT
//
// A right-side, resizable panel for talking to the chief agent without leaving
// the tmux grid. Its width and open/closed state are persisted and restored.
//
// 🔴 THREE CONSTRAINTS THIS ARC HAS ALREADY PAID FOR. NONE IS STYLISTIC, AND
// EACH ONE NAMES A SHIPPED DEFECT:
//
//  1. THE PANEL CANNOT HOST ITS OWN PERSISTENCE SCRIPT. #chief-panel-body is an
//     `hx-swap: innerHTML` target, so a <script> inside it is destroyed by the
//     very swap it exists to survive. chiefPanelScript therefore lives in the
//     PAGE SHELL (components.go's Page), beside the other shell scripts, and the
//     panel body carries only DOM.
//
//  2. THE STORAGE KEY IS A STABLE REFERENCE, NEVER A DESCRIPTION. Rank 11
//     shipped a drawer keyed on an archived COUNT — a number that changes on
//     exactly the swaps the memory was meant to survive, so it forgot precisely
//     when it mattered. These keys are built from the panel's own ELEMENT ID,
//     which is a constant of this file.
//
//  3. BIND ONCE, AND LISTEN ON `document`, NOT `document.body`. A once-guard
//     plus a body-scoped listener is worse than either alone: an hx-boost body
//     swap kills the listener while the guard suppresses the rebind, leaving a
//     panel that is permanently inert and a console with nothing in it.
//
// 🔴 AND IT DOES NOT SWALLOW INTERACTIONS IT ONLY OBSERVES — rank 11's mutant
// PD. The document-level click handler returns immediately for anything that is
// not one of this panel's own controls, and calls preventDefault on nothing
// else, so a <summary> click still toggles its <details> and a card's view
// toggle still flips.
//
// 🔴 IT RE-MOUNTS THE EXISTING AGENT CHAT RATHER THAN BUILDING A SECOND ONE.
// The body is agentChatPane + agentChatScript — the same #chat-log fed by
// GET /ui/agents/{name}/chat-log on `sse:chat.reply`, and the same WebSocket
// send path. A second chat would be a second transcript, a second unread ledger
// and a second place for a streaming bug to live.
// ---------------------------------------------------------------------------

const (
	// chiefPanelID is BOTH the element id and the reference every stored key is
	// built from. One constant, so the memory cannot come to be keyed on
	// something that describes the panel's contents.
	chiefPanelID     = "chief-panel"
	chiefPanelBodyID = "chief-panel-body"

	// chiefPanelActionsID is the TITLE-BAR slot the two launchers land in.
	//
	// 🔴 IT IS A SLOT FILLED OUT-OF-BAND, NOT MARKUP THE SHELL CAN RENDER, AND THE
	// REASON IS THE AGENT ID. ChiefPanel() is rendered by the shell on every tab,
	// and the shell deliberately does NOT resolve the chief — that lookup is what
	// makes the panel body lazy, and paying it on every page load would undo the
	// whole point. The new-thread control needs the chief's row id (it reuses
	// POST /agents/{id}/sessions), so it can only be built where that id is known:
	// in the body partial. The body's response therefore carries an
	// `hx-swap-oob` copy of this container, which is the established idiom in this
	// package (18 other uses in components.go).
	chiefPanelActionsID = "chief-panel-actions"

	// chiefThreadsToggleID is the checkbox-hack peer for the panel's thread list.
	chiefThreadsToggleID = "chief-threads"
	// chiefThreadResultsID is the swap target the search route answers into. It
	// holds the thread ROWS — the recent list on first paint, the matches after a
	// query — so one renderer serves both and a cleared query restores the list.
	chiefThreadResultsID = "chief-thread-results"
	// chiefThreadSearchID is the search input.
	chiefThreadSearchID = "chief-thread-search"

	// chiefThreadStateAttr names WHICH answer the results container is, as a machine
	// -readable state rather than as a sentence.
	//
	// 🔴 THE SENTENCES ALONE CANNOT BE GUARDED AND WERE ALREADY WRONG ONCE. Every
	// failure path in handleChiefThreadSearch — a Postgres error, a chief that has
	// gone missing — used to render "No thread matches that.", i.e. the answer to a
	// question the server never managed to ask. A guard on the WORDS is walkable by
	// rewording; this attribute is the state, so a copy edit cannot make a broken search
	// look like an empty one.
	//
	// ⚠ WHO READS IT, AND THE TWO TIERS DO NOT READ ALL FOUR STATES. Go asserts every
	// state including `error` (which needs an injected store failure, so only a Go
	// fixture can reach it: api.TestAFailedSearchIsNotRenderedAsAnANSWER,
	// api.TestAFailedThreadListReadIsNEVERRenderedAsAnEmptyHistory,
	// ui.TestAFailedReadIsAThirdStateAndNamesItself). The Playwright tier asserts the two
	// a browser can produce — `rows` and `no-match` — in e2e/tests/chief-threads.spec.ts.
	// This comment used to say "both tiers assert the same fact" while the attribute
	// appeared in NO spec at all; the overlap is real now, and it is an overlap rather
	// than a duplicate.
	chiefThreadStateAttr = "data-chief-threads-state"

	// The state vocabulary. `rows` and `empty` and `no-match` are ANSWERS; `error` is
	// the absence of one, and that difference is the whole point of the attribute.
	chiefThreadStateRows    = "rows"
	chiefThreadStateEmpty   = "empty"
	chiefThreadStateNoMatch = "no-match"
	chiefThreadStateError   = "error"

	// chiefThreadAttr marks a thread row IN THE PANEL. The agent-detail and
	// operator drawers render boosted <a href> rows and carry no such mark, which
	// is what lets one guard assert the two surfaces differ.
	chiefThreadAttr = "data-chief-thread"
	// chiefQuickActionAttr carries a quick action's prompt text. The attribute IS
	// the payload: the delegated handler reads it off the element it was clicked
	// on, so a button rebuilt by a swap needs no rebind (constraint 3).
	chiefQuickActionAttr = "data-chief-quick-action"
	// chiefIntroAttr marks the zero-message intro block.
	chiefIntroAttr = "data-chief-intro"
	// chiefActiveSessionAttr carries the session id the SERVER just answered with.
	//
	// 🔴 IT IS THE ONLY WRITER OF THE REMEMBERED THREAD, AND THAT IS WHAT MAKES A
	// DELETED THREAD SELF-HEAL. A script that stored whatever id it saw on the
	// clicked row would keep asking for a thread that no longer exists;
	// resolveSessions answers a stale or foreign id with the LATEST thread, so
	// storing the server's answer converges on something that is really there.
	chiefActiveSessionAttr = "data-chief-active-session"

	// The control vocabulary. Spelled as constants because this renderer, the Go
	// tests and the Playwright specs all assert on them.
	chiefPanelAttr        = "data-chief-panel"
	chiefPanelToggleAttr  = "data-chief-panel-toggle"
	chiefPanelCloseAttr   = "data-chief-panel-close"
	chiefPanelResizeAttr  = "data-chief-panel-resize"
	chiefPanelOpenAttr    = "data-chief-panel-open"
	chiefPanelMissingAttr = "data-chief-panel-missing"

	// chiefPanelBodyLoadedAttr marks a body element that a swap has filled. It is
	// set by the CLIENT and never rendered by the server: its whole job is to tell
	// a fresh element (no mark, must load) from a refreshed one (marked, must not
	// reload), and a server-rendered mark would make every body look already-loaded.
	chiefPanelBodyLoadedAttr = "data-chief-panel-loaded"

	// chiefPanelOpenEvent is dispatched on `document` when the panel opens. The
	// body's hx-trigger listens for it, which is what makes the chat LAZY: a
	// reader who never opens the panel never pays for the transcript fetch or for
	// the chat script.
	chiefPanelOpenEvent = "muster:chief-open"

	// Width bounds, in px. The default is wide enough for a conversation without
	// covering the card column it sits beside.
	chiefPanelMinWidth     = 320
	chiefPanelMaxWidth     = 900
	chiefPanelDefaultWidth = 460
)

// chiefQuickActions is THE list. One place, in Go.
//
// 🔴 A TAP FILLS THE INPUT AND FOCUSES IT. IT DOES NOT SEND. The panel is used
// from a phone, where a stray tap on a control that dispatched an orchestrator
// turn would spend a paid turn with no undo — so the operator always presses Send
// themselves.
//
// 🔴 AND "NO UNDO" IS THE WHOLE REASON, SO THE PREFILL MUST NOT DESTROY A DRAFT
// EITHER. The first version of this wrote the prompt over #chat-input
// unconditionally, which means a stray tap still cost something irrecoverable — the
// half-typed message — on the one surface where the intro renders, an EMPTY thread,
// with the three buttons sitting directly above the box at 390px. A non-empty draft
// is therefore kept and the prompt appended to it; see the click handler in
// chiefPanelScript, and e2e/tests/chief-threads.spec.ts for the browser fact.
//
// The prefill goes into the EXISTING #chat-input and rides #chat-form's
// existing WebSocket submit: the optimistic bubble, the mid-stream refresh guard
// and data-session are all already correct there, and a second write path would
// have to re-derive every one of them.
//
// ⚠ THE STRINGS ARE THE OPERATOR'S OWN WORDS AND THE ORDER IS THEIRS.
// TestTheChiefQuickActionsAreTheLedgeredThree pins this slice two-way, so adding
// a fourth without a test entry — or leaving an entry naming nothing — fails. It is
// `package ui` and reads this variable directly: unexported on purpose, because an
// exported accessor for one same-package test is public API nobody outside needs.
var chiefQuickActions = []string{
	"What needs my attention?",
	"Recap the fleet",
	"What's blocked right now?",
}

// ChiefPanelView is what the panel body needs.
type ChiefPanelView struct {
	// Configured is false when no agent carries the chief display name. The panel
	// then renders an honest explanation rather than an empty chat.
	Configured bool
	// Reason explains a missing chief, shown verbatim.
	Reason string
	// Chat is the chief's agent-detail view, re-used as-is.
	Chat AgentDetailView
	// ThreadsFailed says the SESSION-LIST read behind Chat.Sessions broke, so the
	// panel's thread list must render the failure state instead of an answer.
	//
	// 🔴 IT IS ON *THIS* STRUCT AND NOT ON THE SHARED AgentDetailView, AND THAT IS
	// WHAT KEEPS THE TWO PAGE SURFACES OUT OF IT. AgentDetailView is built by
	// AgentDetailPage and OperatorPage in several places; a failure flag there would
	// be a field those renderers could start reading, and the fix for the panel would
	// have a surface area of three screens. Only chiefPanelBody constructs a
	// ChiefPanelView, so this field can only ever change what the panel draws.
	//
	// 🔴 AND IT IS THE FREQUENT PATH, WHICH IS WHY IT EXISTS AT ALL. api.resolveSessions
	// discarded the ListSessions error, so a failed read reached this renderer as an
	// EMPTY list and #chief-panel-body — refetched on EVERY panel open — rendered
	// "No threads yet.": the operator was told their whole thread history was gone
	// because one read failed. The search route already said so (ChiefThreadsView.Failed);
	// the first paint did not, and the first paint is the path the operator takes first
	// and most often.
	ThreadsFailed bool
	// SessionRuntimes are the runtimes a NEW session may be started on, and
	// ClaudeModels the Claude Code models offered; none: no picker
	// (chief_sessions.go).
	SessionRuntimes []SessionRuntimeView
	ClaudeModels    []string
}

// ChiefPanelToggle is the launcher, rendered by the tmux panel body.
//
// ⚠ IT IS RENDERED INSIDE A SWAP TARGET AND THAT IS FINE. It is a plain
// <button> carrying a data attribute; the handler is delegated on `document`, so
// a button destroyed and rebuilt on every panel refresh keeps working without a
// rebind. That is the whole reason constraint 3 is worth obeying.
func ChiefPanelToggle() g.Node {
	return Button(
		Type("button"),
		g.Attr(chiefPanelToggleAttr, ""),
		g.Attr("aria-controls", chiefPanelID),
		g.Attr("aria-expanded", "false"),
		Class("press min-h-[44px] inline-flex items-center gap-1 rounded-md border border-edge bg-s1 px-2 py-1 text-[11px] font-medium text-fg2 transition hover:bg-s2 hover:text-fg"),
		g.Text("Ask chief"),
	)
}

// ChiefPanel is the slide-out itself, rendered at BODY level by the shell.
//
// 🔴 BODY LEVEL IS WHAT SATISFIES CRITERION 3. #panel-tmux replaces its entire
// innerHTML every 60s and on every snapshot push; the other four tab panels swap
// independently. A panel rendered inside <main> would be destroyed by any of
// them — along with the operator's half-typed message and the live WebSocket —
// so it is a sibling of the FABs and popovers, which are body-level for exactly
// this reason.
func ChiefPanel() g.Node {
	return Aside(
		ID(chiefPanelID),
		g.Attr(chiefPanelAttr, ""),
		// 🔴 THE PANEL IS ALWAYS IN THE DOM AND `inert` + translated OFF-SCREEN
		// WHEN SHUT, rather than `hidden`. `hidden` would drop the CSS transition
		// (there is nothing to animate from) and, more importantly, would unmount
		// the chat's scroll position every time the operator glanced away. `inert`
		// is what keeps a closed panel out of the tab order and off a screen
		// reader's radar, which `translate-x-full` alone does not do.
		g.Attr("inert", "inert"),
		g.Attr("aria-label", "Chief"),
		Class("fixed inset-y-0 right-0 z-40 flex w-full max-w-full translate-x-full flex-col border-l border-line bg-bg shadow-2xl transition-transform duration-200 sm:w-[28rem]"),
		// The resize grip: a thin strip down the left edge.
		//
		// ⚠ `role=separator` + `aria-orientation=vertical` + tabindex is the
		// accessible shape of a splitter, and the keyboard half is not optional —
		// a drag-only control is unreachable without a pointer. Left/Right arrows
		// move it; the script owns the step.
		Div(
			g.Attr(chiefPanelResizeAttr, ""),
			g.Attr("role", "separator"),
			g.Attr("aria-orientation", "vertical"),
			g.Attr("aria-label", "Resize the chief panel"),
			g.Attr("tabindex", "0"),
			Class("absolute inset-y-0 left-0 z-10 w-1.5 cursor-col-resize bg-transparent transition hover:bg-accent/40 focus:bg-accent/60 focus:outline-none"),
		),
		// ⚠ A <div>, NOT A <header>, AND THAT IS A GUARD'S FINDING RATHER THAN A
		// preference. This panel is rendered into the SHELL, which serves every tab
		// URL from one document and already has exactly one <header>;
		// TestHeaderAndContentShareOneWidth and TestRouteHeaderMatchesItsOwnContentColumn
		// both locate that header by being the only one, so a second banner element
		// here broke them on both counts. The title bar of a slide-out is not the
		// document's banner in any case.
		Div(
			Class("flex flex-none items-center justify-between gap-2 border-b border-line px-3 py-2"),
			Div(
				Class("flex items-baseline gap-2"),
				Span(Class("text-sm font-semibold text-fg"), g.Text("Chief")),
				Span(Class("text-[10px] text-muted"), g.Text("the fleet's orchestrator")),
			),
			// The launcher slot: new-thread + thread-list, filled out of band by the
			// body partial (see chiefPanelActionsID). It renders EMPTY here rather
			// than with disabled placeholders — a control that looks tappable and
			// does nothing until a fetch lands is worse than one that appears when
			// it works, and the panel is shut (and `inert`) until the body loads
			// anyway.
			Div(ID(chiefPanelActionsID), Class("ml-auto flex items-center gap-0.5")),
			Button(
				Type("button"),
				g.Attr(chiefPanelCloseAttr, ""),
				g.Attr("aria-label", "Close the chief panel"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-md text-muted transition hover:bg-s2 hover:text-fg"),
				g.Text("✕"),
			),
		),
		// The body: an hx-swap target, fetched the first time the panel opens and
		// on every open after that.
		//
		// 🔴 NO `once` ON THE TRIGGER, DELIBERATELY. A panel opened, left for an
		// hour and opened again should show the conversation as it is NOW; `once`
		// would show it as it was the first time, with the live `sse:chat.reply`
		// refresh inside it being the only thing that had moved. Re-fetching on
		// each open costs one request the operator explicitly asked for.
		Div(
			ID(chiefPanelBodyID),
			Class("flex min-h-0 flex-1 flex-col overflow-hidden p-3"),
			hx("hx-get", "/ui/chief/panel"),
			// 🔴 SPELLED AS A LITERAL, NOT CONCATENATED WITH chiefPanelOpenEvent.
			// The hx-trigger ledger (api.TestTheTmuxChangedSeamIsWiredEndToEnd) folds
			// literals and REFUSES anything it cannot fold, because a trigger it
			// cannot read is a subscription it cannot verify — it reported this
			// function by name the moment it was a concatenation. The constant is
			// still the single name the JS dispatches, and
			// TestTheChiefPanelTriggerAndTheDispatchedEventAreOneName pins the two
			// together, so the literal and the constant cannot silently disagree.
			hx("hx-trigger", "muster:chief-open from:document"),
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			P(Class("text-xs text-muted"), g.Text("Loading…")),
		),
	)
}

// RenderChiefPanelBody writes the panel body partial.
func RenderChiefPanelBody(w io.Writer, v ChiefPanelView) error {
	return chiefPanelBody(v).Render(w)
}

// chiefPanelBody is the chat, or an honest account of why there is none.
func chiefPanelBody(v ChiefPanelView) g.Node {
	if !v.Configured {
		reason := v.Reason
		if reason == "" {
			reason = "No agent carries the chief display name."
		}
		return g.Group{
			// 🔴 THE SLOT IS CLEARED, NOT SKIPPED. hx-swap-oob replaces the element it
			// names, so rendering an EMPTY actions container is what removes launchers
			// a previous (configured) render put there. Omitting the fragment would
			// leave a new-thread button in the title bar of a panel that has no agent
			// to create a thread on.
			chiefPanelActions(v),
			Div(
				g.Attr(chiefPanelMissingAttr, ""),
				Class("space-y-2 text-xs leading-relaxed text-muted"),
				P(g.Text(reason)),
				// 🔴 IT SAYS WHAT IS MISSING AND WHERE TO FIX IT. "Chief is
				// unavailable" with no further text is the shape that gets reported as
				// a broken feature; naming the DISPLAY NAME is what makes it a
				// configuration statement a reader can act on.
				P(g.Text("Dispatch an agent and set its display name to “chief”, then reopen this panel. "+
					"The agent's own slug name is generated and does not have to be “chief”.")),
			),
		}
	}
	return g.Group{
		chiefPanelActions(v),
		Div(
			// 🔴 `relative` IS LOAD-BEARING, NOT COSMETIC. It makes THIS element the
			// containing block for chiefThreadList's `absolute inset-0`, which is how the
			// thread list becomes a sub-view of the panel's own column rather than a
			// second viewport-relative overlay. Drop it and the list anchors to whatever
			// ancestor happens to be positioned — which, because ChiefPanel carries a
			// transform, is the panel: it would still LOOK right while being anchored to
			// something this file does not control.
			// 🔴 `overflow-hidden` IS PART OF THE SLIDE, NOT HOUSEKEEPING, AND IT WAS
			// ADDED AGAINST A MEASUREMENT. `translate-x-full` moves the list by 100% of
			// its OWN width, which lands its left edge exactly on this element's right
			// edge — and #chief-panel-body carries `p-3`, so that is 12px INSIDE the
			// panel. Measured in Chromium at 1280×720: the shut list occupied
			// 1268→1703 while the panel ended at 1280, leaving a 12px dark sliver of
			// its background and ring visible down the right of the conversation. This
			// clips it. (The panel's own overflow-hidden does not: the sliver is inside
			// the panel, not outside it.)
			Class("relative flex min-h-0 flex-1 flex-col overflow-hidden"),
			// The session the SERVER resolved — the only writer of the remembered
			// thread. See chiefActiveSessionAttr.
			g.Attr(chiefActiveSessionAttr, strconv.FormatInt(v.Chat.ActiveSessionID, 10)),
			// The thread list: checkbox, backdrop, sub-view — in that order, because the
			// peer-checked:* utilities on the two later siblings select off the checkbox.
			// The chat pane below carries no peer-checked class, so it is untouched.
			chiefThreadList(v),
			// The EXISTING chat pane, unmodified: #chat-log fed by
			// /ui/agents/{name}/chat-log on sse:chat.reply, plus #chat-form's WebSocket.
			agentChatPane(v.Chat),
			// 🔴 THE CHAT SCRIPT LIVES IN THE PANEL BODY AND THE PERSISTENCE SCRIPT
			// DOES NOT — the two are opposite cases and the difference is not a
			// judgement call. agentChatScript is BUILT to be re-executed: it guards its
			// document-lifetime hooks on window.__cgChatGlobals and its per-form wiring
			// on the form's own data-cg-chat marker, so running it again after a swap
			// re-binds the fresh form and does nothing else. chiefPanelScript keeps the
			// panel's OPEN/WIDTH memory, which is the thing the swap exists to survive,
			// so it cannot be here (constraint 1).
			//
			// ⚠ IT IS ALSO WHAT MAKES THE CHAT LAZY. This script is several hundred
			// lines including a markdown renderer; putting it in the shell would charge
			// every page load in the app for a panel most of them never open.
			agentChatScript(v.Chat.Name),
			// ⚠ NO bubbleClampScript HERE, deliberately: the panel's bubbles are
			// never clamped (bubble_clamp.go).
		),
	}
}

// chiefPanelActions renders the TITLE-BAR launcher pair, out of band.
//
// 🔴 BOTH LAUNCHERS, IN THE TITLE BAR, AND NEITHER IS A SECOND IMPLEMENTATION.
// The thread-list launcher IS chatHistoryButton — the same 44px control, the same
// icon and the same keyboard contract the agent-detail header uses, pointed at the
// panel's own checkbox. The new-thread control reuses POST /agents/{id}/sessions,
// the route the page surfaces already post to, so its empty-session reuse comes
// along rather than being re-derived.
func chiefPanelActions(v ChiefPanelView) g.Node {
	return Div(
		ID(chiefPanelActionsID),
		hx("hx-swap-oob", "true"),
		Class("ml-auto flex items-center gap-0.5"),
		g.If(v.Configured, chiefNewThreadButton(v.Chat.ID)),
		g.If(v.Configured, chatHistoryButton(chiefThreadsToggleID)),
	)
}

// chiefNewThreadButton opens a fresh thread WITHOUT leaving the grid.
//
// 🔴 IT TARGETS #chief-panel-body BECAUSE THE PAGE CONTRACT WOULD NAVIGATE AWAY.
// handleAgentSessionCreate answers HX-Redirect, which htmx follows with a real
// navigation — correct on /agents/{name} and /operator, and on /tmux it would
// throw away the grid, the panel and the operator's place in it. The handler grows
// a SURFACE-aware branch (?surface=chief-panel) that returns this partial instead;
// the redirect stays exactly as it was for the two page surfaces, which have their
// own tests pinning it.
func chiefNewThreadButton(agentID int64) g.Node {
	return Form(
		hx("hx-post", agentSessionsPath(agentID)+"?"+ChatSurfaceQueryParam+"="+string(ChatSurfaceChiefPanel)),
		hx("hx-target", "#"+chiefPanelBodyID),
		hx("hx-swap", "innerHTML"),
		// class="contents" so the form introduces no box into the title bar's flex
		// row — the Button is the visible, tappable control. Same shape as
		// newChatButton, for the same reason.
		Class("contents"),
		Button(
			Type("submit"),
			g.Attr("aria-label", "New thread"),
			g.Attr("title", "New thread"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-md text-muted transition hover:bg-s2 hover:text-fg"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>`),
		),
	)
}

// chiefIntro is the zero-message opener: the three quick actions, plus the one line
// saying what tapping one does.
//
// 🔴 IT DESCRIBES THE CONTROL, NOT THE AGENT'S CAPABILITIES. A prose restatement of
// what chief can reach lived here and was deleted: the reachable set is decided by
// the route table and by whether the chief door is armed on the live pod
// (the CLI's chief-capability reach test is the machine-checked ledger of it, and
// the deployment manifest carries the token as `optional: true`, so removing the
// secret key disarms the door with no manifest change). ChiefPanelView carries no armed field, so this renderer cannot qualify
// such a claim, and nothing binds the prose to the ledger — which is how the same
// sentence has already rotted elsewhere. Do not re-add it.
func chiefIntro() g.Node {
	actions := make([]g.Node, 0, len(chiefQuickActions))
	for _, a := range chiefQuickActions {
		actions = append(actions, Button(
			// 🔴 type="button", NOT submit. The quick actions sit inside #chat-log,
			// which is a sibling of #chat-form rather than a descendant — so a submit
			// here would not post today either. Spelling it is what keeps that true if
			// the two are ever nested: the operator must always press Send themselves.
			Type("button"),
			g.Attr(chiefQuickActionAttr, a),
			Class("press inline-flex min-h-[36px] items-center rounded-lg bg-s2/80 px-3 py-1.5 text-left text-xs text-fg ring-1 ring-inset ring-edge transition hover:bg-s3 hover:text-fg"),
			g.Text(a),
		))
	}
	return Div(
		g.Attr(chiefIntroAttr, ""),
		Class("flex flex-col gap-3 rounded-xl bg-s1/60 p-3 ring-1 ring-inset ring-line"),
		Div(
			Class("flex flex-wrap gap-2"),
			g.Group(actions),
		),
		P(
			Class("text-[10px] text-muted"),
			g.Text("Tapping one fills the box below — you send it."),
		),
	)
}

// ChiefThreadsView is the thread ROWS the panel's list shows.
type ChiefThreadsView struct {
	// Threads are the rows to render, most-recently-active first. Every one of them
	// is rendered: the panel applies no cap of its own, and a search answers with
	// its own bounded set (agents.SearchSessionsMaxLimit).
	//
	// 🔴 SO THE FIRST PAINT IS UNCAPPED WHILE SEARCH IS CAPPED AT 50, AND THE
	// MEASUREMENT THAT MAKES THAT SAFE BELONGS HERE RATHER THAN IN A SESSION'S HEAD.
	// The first paint is the FREQUENT path — #chief-panel-body refetches on every
	// open, and each of those renders this list in full — so "the cap only matters for
	// search" would have been the wrong way round. MEASURED against a live database
	// on the project muster was extracted from: the chief agent carried FOUR
	// chat_sessions rows, two of which were empty recap rows deleted in that arc.
	// ⚠ THAT IS A MEASUREMENT ON ANOTHER DEPLOYMENT, so it is the ORDER OF
	// MAGNITUDE that carries across, not the number. Threads are created by an operator
	// tapping New thread or by an agent's first webchat turn, so the row count is
	// operator-paced, not fleet-paced — nothing on a loop writes one.
	//
	// ⚠ IT IS NOT BOUNDED HERE BECAUSE THE LIST IS NOT THIS RENDERER'S TO TRUNCATE.
	// The rows come from api.resolveSessions, which also feeds the agent-detail and
	// operator drawers; capping it would silently drop threads from three surfaces,
	// and a cap with no "older threads" affordance hides history rather than bounding
	// a cost. If the count ever does grow, the bound belongs in resolveSessions with
	// an affordance beside it — and the search box above is already the way to reach
	// an old thread.
	Threads []SessionView
	// Query is what was searched, echoed so the empty state can name it.
	Query string
	// Searched distinguishes "no threads at all" from "no MATCHES", which are
	// different sentences and the reason this is not derived from Query != "".
	Searched bool
	// Failed says the server never got an answer — the read itself broke.
	//
	// 🔴 IT IS A THIRD STATE, NOT A FLAVOUR OF EMPTY, AND CONFLATING THEM IS THE
	// DEFECT THIS FIELD EXISTS TO CLOSE. The search route deliberately answers 200
	// with rows on every failure (htmx does not swap a non-2xx body, so a 500 leaves
	// the PREVIOUS query's matches on screen as though they were the answer) — and
	// that correct decision was rendering a Postgres error, a chief-resolution
	// failure and a genuinely empty result set as one sentence, "No thread matches
	// that.", traced only in a server log. The operator could not tell "nothing
	// matched" from "search is broken", and on the blank-query path they were told
	// their thread history was EMPTY when the list read had simply failed.
	//
	// ⚠ IT OUTRANKS Searched. A failed search has no result set to describe, so
	// there is no answer to describe and the query text is echoed only to say what was
	// being attempted.
	//
	// 🔴 IT ALSO OUTRANKS THE ROW COUNT, AND chiefThreadRows ENFORCES THAT RATHER THAN
	// TRUSTING THE CALLER — which this comment used to do, and which failed. It read
	// "Threads must be empty whenever this is set": a PRECONDITION, stated in prose,
	// honoured by the search route (it discards the list on every failure) and violated
	// by the first paint the moment that call site was added. api.resolveSessions returns
	// `views, err` from one read, and agents.PGStore.ListSessions ends `return out, rows.Err()`
	// — so a mid-stream failure (connection reset, server-side cancellation) hands the
	// panel a TRUNCATED slice AND a non-nil error. That rendered as state="rows" with no
	// failure sentence anywhere: a partial thread history presented as the whole of it.
	// The renderer now answers the failure state at EVERY row count, so a caller cannot
	// reintroduce it by forgetting; see chiefThreadRows for why the rows are dropped
	// rather than shown alongside the sentence.
	Failed bool
	// Reason replaces the failure SENTENCE when the absence of an answer is not a
	// transient read at all. Ignored unless Failed is set.
	//
	// 🔴 IT EXISTS BECAUSE "Try again." WAS A LIE IN ONE STATE. handleChiefThreadSearch
	// renders the failure state when s.ext.Agents is nil — a muster booted with no
	// database — and that read cannot succeed on a retry, not now and not ever: there is
	// no store to ask. Telling the operator to try again is an instruction that can only
	// waste their time. The STATE is still `error` (it is the absence of an answer, which
	// is exactly what that state means); only the sentence differs.
	Reason string
}

// RenderChiefThreadRows writes the thread-row partial the search route answers with.
func RenderChiefThreadRows(w io.Writer, v ChiefThreadsView) error {
	return chiefThreadRows(v).Render(w)
}

// chiefThreadRows renders one row per thread.
//
// 🔴 A ROW IS AN hx-get INTO THE PANEL BODY, NOT A LINK. The page surfaces use
// boosted <a href="/agents/<name>?session=<id>"> and must keep doing so — their
// tests pin it. In the panel a navigation would leave the grid, so a row asks the
// panel handler (which already honours ?session=) for that thread and swaps it
// into #chief-panel-body. It is a <button> rather than an <a> with hx-get because
// there is no URL it degrades to: without JS a bare href here would navigate,
// which is the behaviour being removed.
//
// 🔴 A FAILED READ IS ANSWERED BEFORE THE ROWS ARE EVEN COUNTED, AND THE ROWS ARE
// DROPPED. That ordering is the whole of round-3 finding 1. ChiefThreadsView.Failed
// documented "Threads must be empty whenever this is set" as a precondition on the
// CALLER; the search route honours it, the panel's first paint passed both — and a
// truncated read (agents.PGStore.ListSessions ends `return out, rows.Err()`, so a
// mid-stream failure yields rows AND an error) therefore rendered state="rows" with no
// failure sentence at all. Checking Failed here, first, makes the precondition a
// property of the renderer: no present or future call site can render a partial list
// as a complete one by forgetting to discard it.
//
// 🔴 AND THE ROWS THAT DID ARRIVE ARE NOT SHOWN BESIDE THE SENTENCE, DELIBERATELY.
// A broken read does not say how much of the answer it lost: `out, rows.Err()` can be
// the first row of hundreds or every row but one, and nothing in the error
// distinguishes them — so rows rendered here are a list of UNKNOWN completeness, drawn
// as tappable, active-highlighted, visually identical to a whole one. An operator who
// reads the rows and not the sentence above them draws exactly the false conclusion
// this arc exists to prevent. With the rows gone the warning sentence is the only thing
// in the container, so it cannot be skimmed past, and nothing reachable is lost.
//
// 🔴 AND THE RETRY IS IN PLACE, WHICH IS THE CHEAPEST ONE AND NOT THE ONE THIS COMMENT
// USED TO NAME. The search input above this list carries
// hx-trigger="input changed delay:300ms, search" against /ui/chief/threads/search, and a
// BLANK q on that route is the SAME agents.ListSessions read (api.handleChiefThreadSearch
// renders the recent list rather than an empty result set) — so a keystroke and a clear,
// or the type=search clear affordance, re-issues this read and swaps the answer over
// #chief-thread-results without the panel moving. Reopening the panel refetches the body
// too, but that is close-then-open — TWO taps, not the "one tap" this said before — and
// the threads drawer's own toggle is a pure-CSS checkbox that fetches nothing at all.
// A NON-blank query is a different read (SearchSessions) and reaches a named thread
// without this one ever succeeding.
func chiefThreadRows(v ChiefThreadsView) g.Node {
	if v.Failed {
		msg := "Could not search threads just now — this is not an empty result. Try again."
		// 🔴 "Try again." IS ONLY TRUE OF A TRANSIENT READ. A no-database boot cannot
		// answer this on a retry ever, so the caller may hand this renderer the
		// honest sentence instead. See ChiefThreadsView.Reason.
		if v.Reason != "" {
			msg = v.Reason
		}
		// Not the muted text colour: the operator has to be able to tell at a glance that they are
		// looking at a broken read rather than at an answer.
		return chiefThreadNotice(chiefThreadStateError, msg, "text-st-warning-fg")
	}
	if len(v.Threads) == 0 {
		// Two remaining facts, two different sentences, and the STATE on the element so
		// neither tier has to assert the wording.
		state, msg := chiefThreadStateEmpty, "No threads yet."
		if v.Searched {
			state, msg = chiefThreadStateNoMatch, "No thread matches that."
		}
		return chiefThreadNotice(state, msg, "text-muted")
	}
	rows := make([]g.Node, 0, len(v.Threads))
	for _, sess := range v.Threads {
		title := sess.Title
		if title == "" {
			title = "New thread"
		}
		cls := "press flex min-h-[44px] w-full flex-col items-start gap-0.5 rounded-xl px-3 py-2 text-left text-sm transition ring-1 ring-inset "
		if sess.Active {
			cls += "bg-accent/15 text-fg ring-accent/50"
		} else {
			cls += "bg-s1 text-fg2 ring-line hover:bg-s2"
		}
		id := strconv.FormatInt(sess.ID, 10)
		row := Button(
			Type("button"),
			g.Attr(chiefThreadAttr, id),
			g.If(sess.Active, g.Attr("aria-current", "true")),
			hx("hx-get", "/ui/chief/panel?session="+id),
			hx("hx-target", "#"+chiefPanelBodyID),
			hx("hx-swap", "innerHTML"),
			Class(cls),
			Div(
				Class("flex w-full items-baseline gap-2"),
				Span(Class("mr-auto truncate"), g.Text(title)),
				sessionDefaultBadge(sess),
				g.If(!sess.LastActive.IsZero(),
					Span(Class("shrink-0 text-xs text-muted"), g.Text(relTimeString(sess.LastActive)+" ago")),
				),
			),
			// The session's runtime and model, when it was started on one.
			sessionRuntimeLine(sess),
			// The matched excerpt, body matches only. A title-only match renders no
			// snippet — see SessionView.Snippet.
			g.If(sess.Snippet != "", Span(
				Class("line-clamp-2 w-full text-[11px] leading-snug text-muted [overflow-wrap:anywhere]"),
				g.Text(sess.Snippet),
			)),
		)
		// "Make default" sits BESIDE the row, never inside it: a form in a button is
		// not valid HTML, and a tap meant to open the thread must not move the default.
		if mk := makeDefaultControl(sess); mk != nil {
			rows = append(rows, Div(Class("flex items-stretch gap-1"), row, mk))
		} else {
			rows = append(rows, row)
		}
	}
	return Div(
		ID(chiefThreadResultsID),
		// The state is on EVERY answer, not only on the empty ones: a guard that reads
		// the attribute must be able to tell "rows" from "the attribute is missing",
		// and an absent mark would make a broken read indistinguishable from a full list.
		g.Attr(chiefThreadStateAttr, chiefThreadStateRows),
		Class("flex flex-1 flex-col gap-2 overflow-y-auto"),
		g.Group(rows),
	)
}

// chiefThreadNotice is the results container holding ONE sentence instead of rows —
// the shape all three non-rows answers take (empty, no-match, failed read).
//
// It exists so the three share one container: the id, the class list and the state
// attribute are what both test tiers read, and three copies of them is three chances
// for one answer to drop the mark and become indistinguishable from a full list.
func chiefThreadNotice(state, msg, cls string) g.Node {
	return Div(
		ID(chiefThreadResultsID),
		g.Attr(chiefThreadStateAttr, state),
		Class("flex flex-1 flex-col gap-2 overflow-y-auto"),
		P(Class("px-1 py-2 text-xs "+cls), g.Text(msg)),
	)
}

// chiefThreadList is the panel's thread picker: a sub-view of the panel's own
// column, driven by the same pure-CSS checkbox hack the page drawer uses.
//
// 🔴 `absolute inset-0`, NOT `fixed`. sessionDrawer's `fixed inset-y-0 right-0`
// is the hazard this function exists to avoid: a transform on ChiefPanel makes the
// panel the containing block for any fixed descendant, so the page drawer rendered
// inside the panel sits at the panel's right edge at w-80/85vw — two nested
// right-side slide-outs inside a 28rem column — and goes off-screen and inert with
// the panel when it shuts. An absolutely-positioned child of the panel body has no
// such dependence: it covers the panel's content area at every width, and the
// panel's own `overflow-hidden` clips it while it is translated away.
//
// 🔴 THE CHECKBOX SHAPE IS COPIED FROM sessionDrawer DELIBERATELY, INCLUDING WHAT
// IT DOES NOT CARRY. It is NOT aria-hidden and NOT tabindex="-1": sr-only is
// clip/1px rather than display:none, so the input stays natively focusable, and
// hiding a focusable element from the a11y tree is exactly what axe's
// aria-hidden-focus forbids — while stripping it from the tab order instead made
// the whole drawer pointer-only, because a <label for> has no native keyboard
// activation. Both findings are already paid for. Do not re-add either attribute.
//
// 🔴 IT TAKES THE PANEL VIEW, NOT THE CHAT VIEW, BECAUSE THE FIRST PAINT HAS TO BE
// ABLE TO SAY THE LIST READ FAILED. The rows below are the SAME renderer the search
// route answers with, and that route has carried ChiefThreadsView.Failed since the
// sentence conflation was fixed — while this call site passed Threads and nothing
// else, so a failed read arrived here as an empty slice and rendered "No threads
// yet." ThreadsFailed lives on ChiefPanelView (see its comment) precisely so this
// signature could widen without touching AgentDetailView, which the two page
// surfaces share.
func chiefThreadList(p ChiefPanelView) g.Node {
	v := p.Chat
	return g.Group{
		Input(Type("checkbox"), ID(chiefThreadsToggleID), Class("peer sr-only"),
			g.Attr("aria-label", "Threads")),
		// Backdrop: scoped to the panel's own column, not the viewport. NO
		// aria-label — a bare <label> has no widget role, so aria-label is
		// prohibited on it (axe aria-prohibited-attr) and the name has to come from
		// visually-hidden content.
		Label(
			g.Attr("for", chiefThreadsToggleID),
			Class("absolute inset-0 z-10 hidden bg-black/60 peer-checked:block"),
			Span(Class("sr-only"), g.Text("Close threads")),
		),
		Div(
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Threads"),
			Class("absolute inset-0 z-20 flex translate-x-full flex-col rounded-lg bg-bg ring-1 ring-line transition-transform duration-200 ease-out peer-checked:translate-x-0"),
			Div(
				Class("flex flex-none items-center gap-2 border-b border-line px-3 py-2"),
				H2(Class("mr-auto text-sm font-semibold text-fg"), g.Text("Threads")),
				// role="button" + tabindex="0" IS claimed here and IS honoured, by
				// appScript's delegated label[role="button"] Enter/Space handler — the
				// same contract chatHistoryButton relies on. The backdrop above is
				// deliberately left pointer-only (a full-screen tab stop is worse than
				// none), so this is the in-view keyboard close; Space on the checkbox
				// itself remains the JS-free fallback.
				Label(
					g.Attr("for", chiefThreadsToggleID),
					g.Attr("role", "button"),
					g.Attr("tabindex", "0"),
					Class("press inline-flex h-11 w-11 cursor-pointer items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
					Span(Class("sr-only"), g.Text("Close threads")),
				),
			),
			Div(
				Class("flex min-h-0 flex-1 flex-col gap-2 p-3"),
				// A new session on a runtime and a model (chief_sessions.go).
				chiefNewSessionPicker(v.ID, p.SessionRuntimes, p.ClaudeModels),
				// 🔴 SERVER-SIDE SEARCH, DEBOUNCED WITH htmx's OWN `delay:` MODIFIER
				// rather than a hand-rolled timer — the same idiom internal/ui/tmux.go
				// already uses on its snapshot trigger. `search` is in the trigger list
				// so the type=search clear affordance re-queries instead of leaving the
				// last matches on screen.
				Input(
					ID(chiefThreadSearchID),
					Type("search"),
					Name("q"),
					g.Attr("autocomplete", "off"),
					g.Attr("aria-label", "Search threads"),
					Placeholder("Search threads and messages…"),
					Class("min-h-[40px] w-full rounded-lg border-0 bg-s1 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
					// The ACTIVE thread travels in the url so the answer can highlight it
					// without the handler resolving (and therefore possibly CREATING) a
					// session on a read — see handleChiefThreadSearch.
					hx("hx-get", "/ui/chief/threads/search?session="+strconv.FormatInt(v.ActiveSessionID, 10)),
					hx("hx-trigger", "input changed delay:300ms, search"),
					hx("hx-target", "#"+chiefThreadResultsID),
					hx("hx-swap", "outerHTML"),
				),
				// 🔴 Failed TRAVELS WITH THE ROWS. The list and the reason it might be
				// empty are one answer, and this is the call site that used to pass only
				// half of it.
				//
				// ⚠ AND IT PASSES BOTH RATHER THAN DISCARDING THE LIST ITSELF, ON PURPOSE.
				// chiefThreadRows answers the failure state at every row count and drops the
				// rows there, so the rule lives in the renderer instead of in each caller —
				// this one got that rule wrong for a truncated read, and the next call site
				// would have had the same chance to.
				chiefThreadRows(ChiefThreadsView{Threads: v.Sessions, Failed: p.ThreadsFailed}),
			),
		),
	}
}

// chiefPanelScript owns the panel's open/closed state and its width.
//
// 🔴 IT LIVES IN THE SHELL, NOT IN THE PANEL BODY — constraint 1, and the reason
// this function is called from components.go's Page rather than from
// chiefPanelBody.
//
// 🔴 EVERY LISTENER IS ON `document`, BOUND ONCE — constraint 3. htmx's boost
// swaps the body's children; a listener on `document` is unaffected by that
// whatever htmx does with the body element itself, which is the property worth
// having. The once-guard is on `window`, which outlives every swap.
//
// 🔴 restore() RUNS ON EVERY EXECUTION, OUTSIDE THE GUARD. The guard suppresses
// re-BINDING; it must not suppress re-APPLYING. A boosted navigation re-executes
// this script against a fresh DOM, and that fresh panel starts shut at the
// default width until restore() puts the stored state back.
func chiefPanelScript() g.Node {
	return Script(g.Raw(`
(function () {
  var PANEL_ID = '` + chiefPanelID + `';
  // 🔴 THE KEYS ARE BUILT FROM THE ELEMENT ID — A STABLE REFERENCE. Rank 11
  // keyed a drawer on an archived COUNT, which changes on exactly the swaps the
  // memory was meant to survive, so it forgot precisely when it mattered. An id
  // is a constant of the markup: it changes only when someone renames the panel,
  // which is a deliberate act, and it cannot be moved by anything the operator
  // or the fleet does.
  //
  // 🔴 THE PREFIX IS 'muster.', AND IT USED TO BE THE PREFIX OF THE SERVICE THIS
  // ONE WAS EXTRACTED FROM. Every other key this app writes is already 'muster.'
  // (appScript's nav tab, nav scroll and tag filter), so three keys under the old
  // namespace were the last place a reader of the page source could see the
  // extraction had not finished. It is inert rather than dangerous — localStorage
  // is per-ORIGIN, so the two services could never have read each other's — which
  // is exactly why it survived: nothing ever broke.
  //
  // ⚠ THE RENAME DROPS WHAT WAS STORED, DELIBERATELY, AND NOTHING MIGRATES IT.
  // The three values are the panel's open state, its width, and the thread it was
  // last showing; each is rewritten by the next interaction with the panel, so
  // the whole cost is that the panel opens shut, at its default width, on the
  // latest thread, ONCE. A migration would be a read of the old key, a write of
  // the new one and a removal deadline nobody would come back for — more code
  // than the preference is worth, and the 'v1' segment exists so a reset is
  // sayable. Orphaned entries under the old prefix are left in place rather than
  // swept: a cleanup pass is a write on every page load to reclaim three short
  // strings.
  var KEY_OPEN = 'muster.chief.v1.open.' + PANEL_ID;
  var KEY_WIDTH = 'muster.chief.v1.width.' + PANEL_ID;
  // 🔴 THE PICKED THREAD, UNDER THE SAME STABLE REFERENCE — constraint 2. A
  // SESSION ID qualifies: it is a database identity, not a description of the
  // panel's contents, so it cannot be moved by the swaps the memory must survive.
  //
  // 🔴 AND IT IS NOT A CONVENIENCE. #chief-panel-body's hx-get carries NO 'once'
  // (see its comment), so the panel refetches on EVERY open. Without this key a
  // picked thread held until the operator shut the panel and then snapped back to
  // "latest" with nothing on screen saying so — the fifth instance of that bug
  // class in this repo.
  var KEY_SESSION = 'muster.chief.v1.session.' + PANEL_ID;
  var MIN = ` + strconv.Itoa(chiefPanelMinWidth) + `;
  var MAX = ` + strconv.Itoa(chiefPanelMaxWidth) + `;
  var DEFAULT = ` + strconv.Itoa(chiefPanelDefaultWidth) + `;
  var STEP = 32;
  var OPEN_EVENT = '` + chiefPanelOpenEvent + `';
  var BODY_ID = '` + chiefPanelBodyID + `';
  var LOADED_ATTR = '` + chiefPanelBodyLoadedAttr + `';
  var ACTIVE_ATTR = '` + chiefActiveSessionAttr + `';
  var PANEL_PATH = '/ui/chief/panel';

  // Every storage access is wrapped: localStorage throws on access in a
  // partitioned context and on setItem when the quota is full. A storage failure
  // must cost the operator their panel MEMORY, never the panel — the direction
  // to fail in is "the panel opens at its default", not "the page is broken".
  function store() { try { return window.localStorage; } catch (e) { return null; } }
  function get(k) { var s = store(); if (!s) return null; try { return s.getItem(k); } catch (e) { return null; } }
  function set(k, v) { var s = store(); if (!s) return; try { s.setItem(k, v); } catch (e) {} }

  function panel() { return document.getElementById(PANEL_ID); }
  function body() { return document.getElementById(BODY_ID); }

  // storedSession returns the remembered thread id, or 0.
  //
  // 🔴 A NON-NUMERIC OR NON-POSITIVE STORED VALUE IS 0, NOT NaN — the same
  // direction storedWidth fails in. '/ui/chief/panel?session=NaN' is a request the
  // server answers by falling back to the latest thread, so the bug would be
  // invisible rather than loud; 0 means "ask for the default" explicitly.
  function storedSession() {
    var n = parseInt(get(KEY_SESSION), 10);
    if (!isFinite(n) || n <= 0) return 0;
    return n;
  }

  // applySession writes the remembered thread into the body's hx-get.
  //
  // 🔴 SETTING THE ATTRIBUTE IS NOT ENOUGH ON ITS OWN, AND THIS PACKAGE HAS
  // ALREADY PAID FOR THAT. htmx captures the request path in a CLOSURE when it
  // processes the element — wt() in the vendored bundle reads hx-<verb> once and
  // hands that value to every later trigger — so an element that is never
  // re-processed keeps requesting the ORIGINAL url however many times the
  // attribute is rewritten. #chief-panel-body is swapped innerHTML, i.e. the
  // element itself is never replaced, so it is exactly that case. Measured on
  // PR #727 for #tasks-list: the attribute read '/ui/tasks?limit=100' while the
  // wire carried a bare '/ui/tasks'.
  //
  // The htmx:configRequest listener below is the second half, and the only point
  // at which the attribute can still become the request. Keep the two together:
  // either alone is a memory that silently does nothing.
  function applySession() {
    var b = body();
    if (!b) return;
    var n = storedSession();
    b.setAttribute('hx-get', n ? (PANEL_PATH + '?session=' + n) : PANEL_PATH);
  }

  // rememberSession stores the session the SERVER just answered with.
  //
  // 🔴 THE SERVER'S ANSWER, NOT THE CLICKED ROW'S ID, AND THAT IS WHAT MAKES A
  // DELETED THREAD SELF-HEAL. resolveSessions answers a stale, deleted or
  // foreign-agent id with the agent's LATEST thread rather than erroring, so
  // storing what came back converges on a thread that exists. Storing the click
  // would keep asking for a row that is gone, for ever, and the panel would look
  // correct while showing something else.
  function rememberSession() {
    var b = body();
    if (!b) return;
    var el = b.querySelector('[' + ACTIVE_ATTR + ']');
    if (!el) return;
    var n = parseInt(el.getAttribute(ACTIVE_ATTR), 10);
    if (!isFinite(n) || n <= 0) return;
    set(KEY_SESSION, String(n));
    applySession();
  }
  function clamp(n) { return Math.max(MIN, Math.min(MAX, n)); }

  function storedWidth() {
    var raw = get(KEY_WIDTH);
    var n = parseInt(raw, 10);
    // 🔴 A STORED VALUE THAT IS NOT A NUMBER FALLS BACK TO THE DEFAULT RATHER
    // THAN TO NaN. clamp(NaN) is NaN, and 'width: NaNpx' is an invalid
    // declaration the browser DROPS — so a corrupted key would produce a panel
    // at whatever the stylesheet says, silently, for ever.
    if (!isFinite(n) || n <= 0) return DEFAULT;
    return clamp(n);
  }
  function storedOpen() { return get(KEY_OPEN) === '1'; }

  // applyWidth paints a width WITHOUT writing it. The separation is deliberate:
  // restore() must never write, or a restore against a DOM that has not settled
  // would overwrite the operator's real choice with whatever it happened to read.
  function applyWidth(p, px) {
    p.style.width = clamp(px) + 'px';
    var grip = p.querySelector('[` + chiefPanelResizeAttr + `]');
    if (grip) grip.setAttribute('aria-valuenow', String(clamp(px)));
  }

  // applyOpen paints open/closed WITHOUT writing it, for the same reason.
  function applyOpen(p, open) {
    p.classList.toggle('translate-x-full', !open);
    p.classList.toggle('translate-x-0', !!open);
    if (open) { p.removeAttribute('inert'); } else { p.setAttribute('inert', 'inert'); }
    p.setAttribute('` + chiefPanelOpenAttr + `', open ? '1' : '0');
    // Every launcher on the page reports the state. There can be more than one
    // (the tmux body re-renders its own), and they are found by attribute rather
    // than held in a variable precisely because the panel outlives them.
    var toggles = document.querySelectorAll('[` + chiefPanelToggleAttr + `]');
    for (var i = 0; i < toggles.length; i++) {
      toggles[i].setAttribute('aria-expanded', open ? 'true' : 'false');
    }
  }

  // setOpen is the ONE writer of the open key.
  function setOpen(open) {
    var p = panel();
    if (!p) return;
    applyOpen(p, open);
    set(KEY_OPEN, open ? '1' : '0');
    if (open) {
      // 🔴 THE REMEMBERED THREAD IS APPLIED BEFORE THE FETCH IS ASKED FOR, NOT
      // after. A click-to-open is not an htmx settle, so restore() does not run on
      // this path — applying only there would leave the very first re-open (the
      // case the memory exists for) asking for the bare url.
      applySession();
      // Ask the body to (re)fetch. Dispatched on 'document' so the body's
      // 'from:document' trigger catches it wherever the panel sits.
      document.dispatchEvent(new CustomEvent(OPEN_EVENT, { bubbles: true }));
    }
  }

  // restore applies the stored state to whatever panel is in the DOM NOW. It
  // WRITES NOTHING.
  //
  // 🔴 IT STANDS DOWN WHILE A DRAG IS IN PROGRESS, AND THAT IS A MEASURED FIX,
  // NOT A PRECAUTION. restore() is bound to 'htmx:afterSettle', which fires on
  // EVERY settle anywhere in the app — and #panel-tmux settles on a 60s poll and
  // on every snapshot push from either host. A settle landing mid-drag re-applied
  // the STORED width (still the old one, because endDrag has not run yet), so the
  // panel snapped back under the operator's cursor and endDrag then read that
  // snapped-back value off the DOM and persisted it. The drag was silently
  // discarded. Reproduced in the e2e tier at 1 failure in 4 runs; the pointer
  // counters proved the events were arriving, which is what located it here
  // rather than in the event path.
  function restore() {
    if (dragging) return;
    var p = panel();
    if (!p) return;
    applyWidth(p, storedWidth());
    var open = storedOpen();
    applyOpen(p, open);
    // Re-point a FRESH body element at the remembered thread. A boosted
    // navigation builds a brand-new #chief-panel-body carrying the server's
    // default hx-get, so this runs on every restore, not only on the open path.
    applySession();
    // 🔴 A PANEL RESTORED OPEN MUST ALSO LOAD ITS BODY, or a reload leaves the
    // operator looking at 'Loading…' for ever: the body's only trigger is the
    // open event, and a restore is not a click. Dispatched here rather than by
    // calling setOpen, because setOpen WRITES and this path must not.
    //
    // 🔴 ONLY WHEN THE BODY HAS NOT LOADED. restore() is bound to
    // 'htmx:afterSettle', which fires on EVERY settle anywhere in the app — and
    // on the tmux page that is constant (60s poll, a snapshot push from either
    // host, the SSE-driven swaps, and one recap settle per card across ~78
    // cards). An unconditional dispatch here re-fetched /ui/chief/panel on every
    // one of them, and the body is an innerHTML swap whose content IS the chat
    // pane — so the operator's half-typed message was destroyed and re-rendered
    // continuously, and the input could not be typed into at all.
    //
    // MEASURED, both tiers. Live pod before the fix: 111 requests to
    // /ui/chief/panel in 5 minutes, one or two in every single second. In the e2e
    // tier, THREE grid settles produced 449 and 546 refetches on two runs of the
    // same probe — the cascade is far worse under a burst than the live average
    // suggests, because each refetch settles and re-arms the next.
    //
    // The guard is the loaded MARK below, not a 'once' on the trigger: a 'once'
    // would break the re-open case the body's own comment protects, where a
    // panel reopened an hour later must show the conversation as it is NOW.
    // setOpen still dispatches on every OPEN, which is that case; this path
    // fires only for a body that has never been filled — a fresh page, or the
    // fresh element a boosted navigation builds.
    if (open && !bodyLoaded()) {
      document.dispatchEvent(new CustomEvent(OPEN_EVENT, { bubbles: true }));
    }
  }

  // bodyLoaded reports whether the CURRENT body element has been filled by a
  // successful swap.
  //
  // 🔴 THE MARK LIVES ON THE ELEMENT, NOT IN A VARIABLE, AND THAT IS WHAT MAKES
  // THE BOOSTED-NAVIGATION CASE WORK. hx-swap="innerHTML" replaces the body's
  // CHILDREN and keeps the element, so a mark set on it survives every refresh;
  // a boosted navigation builds a BRAND NEW element, which carries no mark, so
  // the next restore() loads it exactly as it must. A module-level boolean
  // would still read true against that new element and leave it on 'Loading…'
  // for ever — the failure this whole path exists to prevent.
  function bodyLoaded() {
    var b = document.getElementById(BODY_ID);
    return !!(b && b.hasAttribute(LOADED_ATTR));
  }

  // --- drag-to-resize ---------------------------------------------------------
  //
  // 🔴 dragging IS DECLARED BEFORE restore() USES IT AND BEFORE THE ONCE-GUARD.
  // Both restore() and the pointer handlers close over this one variable; a
  // second copy inside the guard would leave restore() reading a 'dragging' that
  // is never set.
  var dragging = false;
  // dragWidth is the width the CURRENT drag has computed.
  //
  // 🔴 endDrag PERSISTS THIS, NOT A READ-BACK OF p.style.width. Reading the DOM
  // at the end of a drag persists whatever the element happens to say at that
  // instant — which, if anything else wrote to it mid-drag, is not what the
  // operator dragged to. That is exactly the failure the restore() guard above
  // describes, and fixing only one of the two leaves the other as a silent way to
  // persist a width nobody chose.
  var dragWidth = null;
  function widthFromPointer(clientX) {
    // The panel is right-anchored, so its width is the distance from the pointer
    // to the right edge of the viewport.
    return clamp(Math.round(window.innerWidth - clientX));
  }

  if (!window.__cgChiefPanelInit) {
    window.__cgChiefPanelInit = true;

    // 🔴 THE CLICK HANDLER OBSERVES AND DOES NOT SWALLOW — rank 11's mutant PD.
    // It returns immediately for anything that is not one of this panel's own
    // controls and calls preventDefault on NOTHING else, so a <summary> click
    // still toggles its <details> and a card's view toggle still flips. This is
    // the single most load-bearing line in the file and the easiest to "tidy"
    // into a page-wide preventDefault.
    document.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('[` + chiefPanelCloseAttr + `]')) { setOpen(false); return; }
      // 🔴 A QUICK ACTION PREFILLS AND FOCUSES. IT DOES NOT SEND, AND IT SUPPRESSES
      // NO DEFAULT — the button is type=button, so there is nothing to suppress,
      // and suppressing one here is exactly how this handler becomes the page-wide
      // swallow its own header warns about. The text goes into the EXISTING
      // #chat-input and the operator presses Send, so #chat-form's WebSocket path
      // stays the only way a chat message leaves this panel.
      // (TestTheChiefPanelObservesClicksWithoutSwallowingThem greps this handler's
      // SOURCE for the suppressing call's name, comments included — so describe the
      // property without spelling it.)
      // 🔴 AND IT NEVER DESTROYS A HALF-TYPED DRAFT. The reason prefill was chosen
      // over send-immediately is that a stray tap on a phone must not cost the
      // operator something they cannot undo — and an unconditional write to the box
      // costs them their draft, which is exactly that harm in a different currency.
      // The three buttons sit DIRECTLY ABOVE the input at 390px, and the intro
      // renders on an EMPTY thread, which is where the first message is being
      // composed. So a non-empty draft is KEPT and the prompt is appended after it,
      // on its own line: the tap still does something visible (no dead control), the
      // words the operator typed are still there, and #chat-form's Send is still the
      // only thing that puts either on the wire. Appending rather than inserting at
      // the caret is deliberate — a caret parked at position 0 would jam the prompt
      // in front of the draft, and selectionStart is not reliable across the focus
      // this handler is about to take.
      //
      // ⚠ A WHITESPACE-ONLY BOX COUNTS AS EMPTY. It carries no words to lose, and
      // treating it as a draft would leave a blank line above every prefill for ever.
      var qa = t.closest('[` + chiefQuickActionAttr + `]');
      if (qa) {
        var input = document.getElementById('chat-input');
        if (!input) return;
        var prompt = qa.getAttribute('` + chiefQuickActionAttr + `') || '';
        var draft = input.value || '';
        // The newline is safe in this control: #chat-input is a <textarea> whose
        // Enter-to-send lives on a keydown handler, so text CONTAINING a newline
        // sends nothing by itself.
        input.value = draft.trim() === '' ? prompt : draft.replace(/\s+$/, '') + '\n' + prompt;
        // An 'input' event so the box's listeners see the change — the composer's
        // autosize fallback (composer_fit.go) among them. A value assigned by script
        // fires nothing on its own.
        try { input.dispatchEvent(new Event('input', { bubbles: true })); } catch (err) {}
        input.focus();
        // Caret at the end, so a follow-on keystroke appends rather than replacing
        // a fully-selected value.
        try { input.setSelectionRange(input.value.length, input.value.length); } catch (err) {}
        return;
      }
      var toggle = t.closest('[` + chiefPanelToggleAttr + `]');
      if (!toggle) return;               // not ours — observed, not swallowed
      var p = panel();
      setOpen(!(p && p.getAttribute('` + chiefPanelOpenAttr + `') === '1'));
    });

    // Escape closes, but ONLY when the panel is open and only when the key was
    // not consumed by something else. A global Escape that always fired would
    // close the panel while the operator was dismissing a modal on top of it.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      var p = panel();
      if (p && p.getAttribute('` + chiefPanelOpenAttr + `') === '1') setOpen(false);
    });

    // --- the grip: pointer drag + keyboard -----------------------------------
    document.addEventListener('pointerdown', function (e) {
      var t = e.target;
      if (!t || !t.closest || !t.closest('[` + chiefPanelResizeAttr + `]')) return;
      dragging = true;
      dragWidth = null;
      // preventDefault HERE is correct and is not the mutant above: a drag on a
      // splitter must not also select text across the page. It is scoped to this
      // one element by the closest() test on the line before.
      e.preventDefault();
      try { t.setPointerCapture(e.pointerId); } catch (err) {}
    });
    document.addEventListener('pointermove', function (e) {
      if (!dragging) return;
      var p = panel();
      if (!p) return;
      dragWidth = widthFromPointer(e.clientX);
      applyWidth(p, dragWidth);
    });
    function endDrag() {
      if (!dragging) return;
      dragging = false;
      var p = panel();
      if (!p) return;
      // 🔴 THE WIDTH IS WRITTEN ONCE, AT THE END OF THE DRAG, NOT ON EVERY MOVE.
      // A pointermove fires per frame; writing localStorage per frame is a
      // synchronous main-thread write on the page that already measures 900ms
      // long tasks per panel swap.
      //
      // 🔴 AND IT WRITES WHAT THE DRAG COMPUTED, NOT WHAT THE DOM SAYS. Reading
      // p.style.width back persists whatever the element happens to carry at this
      // instant, which is not the operator's choice if anything else wrote to it
      // mid-drag — see the restore() guard above for the settle that did exactly
      // that. dragWidth is null when the press produced no movement at all, and
      // the DOM read is the fallback for that case only.
      var chosen = dragWidth;
      if (chosen === null) chosen = parseInt(p.style.width, 10) || DEFAULT;
      dragWidth = null;
      set(KEY_WIDTH, String(clamp(chosen)));
      applyWidth(p, chosen);
    }
    document.addEventListener('pointerup', endDrag);
    document.addEventListener('pointercancel', endDrag);

    document.addEventListener('keydown', function (e) {
      var t = e.target;
      if (!t || !t.closest || !t.closest('[` + chiefPanelResizeAttr + `]')) return;
      var d = 0;
      if (e.key === 'ArrowLeft') d = STEP;     // grip left = wider (right-anchored)
      else if (e.key === 'ArrowRight') d = -STEP;
      else return;
      e.preventDefault();
      var p = panel();
      if (!p) return;
      var next = clamp((parseInt(p.style.width, 10) || DEFAULT) + d);
      applyWidth(p, next);
      set(KEY_WIDTH, String(next));
    });

    // 🔴 MARK THE BODY THE MOMENT IT IS FILLED. This listener is what stops the
    // restore() dispatch above from firing on every settle; without it the mark
    // is never set, bodyLoaded() is false for ever, and the re-fetch storm this
    // fix removes comes straight back.
    //
    // It keys on the TARGET's id rather than on the event's detail, because a
    // swap anywhere else in the app also raises htmx:afterSwap on document and
    // must not be mistaken for this one.
    document.addEventListener('htmx:afterSwap', function (evt) {
      var t = evt && evt.target;
      if (!t || t.id !== BODY_ID) return;
      t.setAttribute(LOADED_ATTR, '');
      // The body that just landed names the thread the server resolved. Storing it
      // here — one writer, sourced from the server — is what makes a picked thread
      // survive a close/reopen and what makes a deleted one heal.
      rememberSession();
    });

    // 🔴 THE SECOND HALF OF applySession, AND NEITHER HALF WORKS ALONE. htmx
    // re-reads the request path from this event's detail immediately after
    // dispatching it, so this is the one point at which a rewritten hx-get can
    // still become the request the wire carries. Scoped BY ELEMENT ID, not by a
    // subtree test: every control inside the panel body issues its own requests
    // (the thread rows, the search input, the chat-log refresh), and a
    // subtree-wide rewrite would point all of them at the panel url.
    document.addEventListener('htmx:configRequest', function (evt) {
      var el = evt && evt.target;
      if (!el || el.id !== BODY_ID || !evt.detail) return;
      var want = el.getAttribute('hx-get');
      if (want) evt.detail.path = want;
    });

    // Re-apply after ANY htmx settle. The panel itself is body-level and is not
    // swapped, but a boosted navigation rebuilds the body's children — including
    // it — so the fresh element needs the stored state put back.
    document.addEventListener('htmx:afterSettle', restore);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', restore);
  } else {
    restore();
  }
})();
`))
}
