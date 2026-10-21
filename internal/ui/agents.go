package ui

import (
	"io"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// AgentCardView is the per-agent data the card grid renders.
type AgentCardView struct {
	ID int64
	// Kind and CCAccount are the agent's kind and Claude account name.
	Kind        string
	CCAccount   string
	Name        string
	DisplayName string
	Repo        string
	Status      string // pending|provisioning|running|stopped|error
	KickedOff   bool
	// KickoffOwed is agents.KickoffOwed for this agent: a kickoff message is
	// stored and nothing delivered it. It renders as a badge BESIDE the status
	// dot, never as a status value — see kickoffOwedBadge.
	//
	// 🔴 IT IS NOT DERIVED HERE, AND IT MUST NOT BE. The note text is not on this
	// view at all (it is operator-authored instruction text; agents.Agent tags it
	// `json:"-"` and this package would put it in HTML), so the boolean is
	// composed in internal/api's cardViewIndexed from the stored row and arrives
	// already decided.
	KickoffOwed bool
	// KickoffFailed is agents.KickoffFailed: the first turn was handed to a gateway
	// (the row is stamped kicked_off, so KickoffOwed is false) and it did not
	// complete. It renders as a "kickoff failed" badge beside the status dot plus the
	// failure text and the remedy — see kickoffFailedBadge and kickoffFailureDetail.
	// Mutually exclusive with KickoffOwed by construction.
	KickoffFailed bool
	// KickoffFailure is the text to show for a failed kickoff:
	// agents.KickoffFailureText, composed in internal/api's cardViewIndexed with the
	// pending note already SCRUBBED out (agents.ScrubNote). This package never sees
	// the note and must not be handed raw kickoff_error.
	KickoffFailure string
	// KickoffResendSafe is agents.KickoffResendSafe: the failure proves nothing was
	// sent, so the remedy may say re-sending is safe. Otherwise the remedy says to
	// check whether the agent is already working first. See KickoffFailedRemedy.
	KickoffResendSafe bool
	// KickoffAwaitingGrant is agents.KickoffAwaitingGrant: the first turn is held,
	// unsent, until the owner approves this agent's github connector request (its
	// repository cannot be checked out before). It renders as a "waiting for your
	// github approval" line linking to Connectors — see kickoffAwaitingGrantNotice.
	// Composed in internal/api's cardViewIndexed; only the boolean crosses.
	KickoffAwaitingGrant bool
	// GitHubProblem is why the GitHub App cannot currently serve this agent's
	// repository (its credentials refused at the token mint, a rate limit, the
	// repository outside the installation), "" when nothing is known to be wrong
	// or no App is configured. Composed in internal/api's cardViewIndexed from
	// githubapp's operator-facing text, which never carries a token.
	GitHubProblem string
	// Namespace is the agent's stored namespace, used only to spell the log command
	// in the failed-kickoff remedy.
	Namespace string
	// Model is the agent's configured model slug ("" = cluster default). Surfaced
	// as a small read-only line on the card so it's visible at a glance.
	Model string
	// NoteID is the source task this agent was dispatched from (nil = none). When
	// set, the card shows a "Task #<id>" badge linking to the Tasks tab.
	NoteID *int64
	// CreatedAt backs the card's relative "age". Zero renders nothing.
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastActiveAt is agents.LastActivity for this agent — the time the Agents
	// list is ORDERED by — and it is what the footer's "active … ago" prints, so
	// the order and the label on screen are one number. Composed in internal/api's
	// cardViewIndexed; a zero value (a minimal test view) falls back to UpdatedAt.
	LastActiveAt time.Time
	// IsChief marks the fleet's chief agent; its ⋯ menu offers no Delete. It is
	// the answer of internal/api's ONE chief predicate (isChiefAmong), composed in
	// cardViewIndexed and never re-derived here — the server refuses the delete
	// regardless, so this only keeps the UI from offering what it would refuse.
	IsChief bool
	// Pinned: the viewing user pinned this agent (internal/pins). The list draws
	// pinned entries first (AgentsCardsWithMirrors), the card a pin glyph, and the
	// ⋯ menu offers Unpin instead of Pin. Composed in cardViewIndexed.
	Pinned bool
	// Focus is what the agent is working on (internal/focus), nil for none.
	// Composed in cardViewIndexed; shown as the footer's focus chip, and found
	// by the filter.
	Focus *FocusView
	// Attention is the card's attention badge (cards_attention.go), nil for
	// none. Composed in internal/api from the attention queue's own read.
	Attention *CardAttention
}

// lastActive is the card's "active" time: LastActiveAt, or UpdatedAt when the
// view was built without one.
func (a AgentCardView) lastActive() time.Time {
	if a.LastActiveAt.IsZero() {
		return a.UpdatedAt
	}
	return a.LastActiveAt
}

// NoteOption is a selectable existing note for the dispatch modal.
type NoteOption struct {
	ID    int64
	Label string
}

// ComboOption is one option in a custom type-to-filter combobox. Value is the
// submitted value (carried by the hidden input); Label is the visible text the
// user filters against. For repos Value == Label; for notes they differ (Value
// is the note id, Label is the directory / body preview).
type ComboOption struct {
	Value string
	Label string
}

// AgentsPanel is the static shell for the Agents tab. The tab is OPERATOR-FIRST:
// the operator (the privileged orchestrator you chat with) leads, with quick
// chips under it that seed an operator conversation. The running-agents list
// sits below. The dispatch form lives behind the FAB.
//
// Only the inner card list (#agents-list) and the operator slot poll/lazy-load;
// the FAB and dispatch modal live in the shell (Page) so the 4s status refresh
// never rebuilds them. The card list uses morph swaps so live updates patch in
// place (no scroll jump, no teardown of hover/expanded state).
func AgentsPanel(active bool) g.Node { return agentsPanel(active, nil) }

// agentsPanel is AgentsPanel with the "Recently viewed" strip's entries drawn on
// the first paint (ShellData).
func agentsPanel(active bool, recent []recentItem) g.Node {
	return Div(
		ID(panelID("agents")),
		g.Attr("role", "tabpanel"),
		// destructiveSwap=true — AND THE PANEL'S OWN hx-target IS NOT WHY. This
		// element is never its own target (the inner #agents-list morphs
		// itself). What destroys nodes inside it is wired
		// INSIDE its body, one level down, and it was measured rather than argued:
		//
		//	agents.go cardStatusIcon    hx-target=this  innerHTML             load, every 10s
		//	agents.go agentRenameButton #agent-<id>     outerHTML             click
		//	agents.go agentDeleteConfirm #agent-<id>    outerHTML swap:200ms  click
		//
		// The first is a TIMER, one instance per agent card, so this panel discards
		// nodes inside itself every 10 seconds with nobody touching it. See panelClass.
		Class(panelClass(active, true)),
		// ⚠ THE OPERATOR FAB THAT USED TO SIT ON THIS TAB IS GONE (task #633): there
		// is no operator chat page to open. The tab leads with the running-agents
		// list. (The pending privilege-request queue that used to sit above it was
		// removed with the privilege subsystem; connectors replaced it.)
		// The running-agents list sits directly below the operator.
		Div(
			Class("mb-2 flex items-center gap-2"),
			H2(Class("text-xs font-medium uppercase tracking-wide text-muted"), g.Text("Agents")),
		),
		// Recently viewed (recents.go): above the filter and OUTSIDE #agents-list —
		// the filter's `#agents-list > article` rule assumes every child is a card,
		// and the strip is navigation, not something to filter. Drawn with its
		// entries when the shell has them (ShellData — so its arrival moves nothing
		// below it), and replaced out of band by every list render.
		recentStrip(recent, false),
		// The filter: a search box that narrows the cards as you type. OUTSIDE
		// #agents-list, so the list's own refresh never touches it (agents_filter.go).
		agentsFilterBar(),
		Div(
			ID("agents-list"),
			Class("grid grid-cols-1 gap-3 lg:grid-cols-2"),
			hx("hx-get", "/ui/agents"),
			// Event-driven only — NO timer. A 4s poll churned the list constantly;
			// status stays live via each card's own status-icon poll
			// (cardStatusIcon), and the list re-renders on actual lifecycle changes.
			//
			// 🔴 THE TWO SSE EVENTS ARE WHAT MAKE THE ORDER LIVE, AND BEFORE THEM THE
			// LIST HEARD NO SERVER EVENT AT ALL. `agents:changed` is only ever an
			// HX-Trigger RESPONSE header — it fires in the tab that made the request
			// (a rename, a model change). agent.changed (a status write, a kickoff
			// stamp, the provisioning→running promotion) and chat.reply (a persisted
			// turn, which moves agents.LastActivity) are SSE events, and nothing
			// turned either into `agents:changed`: the Tasks board listened for
			// sse:agent.changed, this list did not, so a list left open never
			// re-ordered and never showed a status change it had not caused itself.
			hx("hx-trigger", agentsListTrigger),
			hx("hx-target", "#agents-list"),
			hx("hx-swap", "morph:innerHTML"),
		),
		// ⚠ AN "ADVANCED" DISCLOSURE USED TO SIT HERE, mounting the runbook and
		// privilege-profile registries a second time. Both subsystems are removed.
		//
		// FAB + modal are rendered at the body level by Page (shellFAB +
		// agentModalShell) so fixed positioning is reliable and they survive the
		// 4s list refresh.
	)
}

// AgentsCards is the /ui/agents partial: just the cards (or the empty state),
// morphed into #agents-list.
func AgentsCards(list []AgentCardView) g.Node {
	if len(list) == 0 {
		return Div(
			// span both grid columns so the empty state stays centered.
			Class("col-span-full mt-16 flex flex-col items-center justify-center gap-2 text-center"),
			Div(Class("text-4xl"), g.Text("🧩")),
			P(Class("text-lg font-medium text-fg2"), g.Text("No agents yet")),
			P(Class("text-sm text-muted"), g.Text("Tap + to dispatch an agent for a repo.")),
		)
	}
	return g.Group(g.Map(list, agentCard))
}

// RenderAgentCard writes a single agent card (the #agent-{id} <article>). Used
// to re-render one card after an inline rename (save) or to restore it on
// cancel, swapped as outerHTML of #agent-{id}.
func RenderAgentCard(w io.Writer, a AgentCardView) error {
	return agentCard(a).Render(w)
}

// RenderAgentRename writes the /ui/agents/{id}/rename partial: a tiny inline
// rename form that REPLACES the card (outerHTML swap of #agent-{id}). It is a
// full <article> with the same id so the card morph/refresh keeps targeting it.
// Save hx-posts to /agents/{id}/name; cancel re-fetches the card. htmx only
// (boost-safe). The input is prefilled with the current DisplayName (or the
// slug Name when unset).
func RenderAgentRename(w io.Writer, a AgentCardView) error {
	return agentRenameForm(a).Render(w)
}

// agentRenameForm is the inline-edit card: a name input + Save/Cancel, posting
// to POST /agents/{id}/name. The slug Name is immutable; only DisplayName edits.
func agentRenameForm(a AgentCardView) g.Node {
	ids := strconv.FormatInt(a.ID, 10)
	return Article(
		ID("agent-"+ids),
		Class("group overflow-hidden rounded-2xl border border-accent/20 bg-s1/70 shadow-lg shadow-black/20 ring-1 ring-accent/10"),
		Form(
			Class("flex flex-col gap-3 p-4"),
			hx("hx-post", "/agents/"+ids+"/name"),
			hx("hx-target", "#agent-"+ids),
			hx("hx-swap", "outerHTML"),
			Label(
				Class("flex flex-col gap-1 text-xs font-medium text-muted"),
				Span(g.Text("Rename agent")),
				Input(
					Type("text"),
					Name("name"),
					Value(displayOr(a.DisplayName, a.Name)),
					g.Attr("aria-label", "Agent display name"),
					g.Attr("autocomplete", "off"),
					g.Attr("autofocus", ""),
					Placeholder(a.Name),
					Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
			),
			Span(Class("break-all text-[11px] text-muted"), g.Text("slug: "+a.Name+" (fixed)")),
			Div(
				Class("flex items-center gap-2"),
				Button(
					Type("submit"),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					hx("hx-disabled-elt", "this"),
					g.Text("Save"),
				),
				Button(
					Type("button"),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl px-4 text-sm font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2"),
					// Cancel: re-fetch this card (boost-safe; no JS).
					hx("hx-get", "/ui/agents/"+ids+"/card"),
					hx("hx-target", "#agent-"+ids),
					hx("hx-swap", "outerHTML"),
					g.Text("Cancel"),
				),
			),
		),
	)
}

// agentCard is the per-agent card. The card BODY is a real boosted <a> that
// opens the detail page on click/tap (status icon + name + repo chip). There is
// NO chat or log preview on a card — an operator decision: the list is for
// finding an agent, the detail page for reading it. The interactive controls —
// the inline rename pencil and the ⋯ context menu (Start/Stop/Delete) — sit
// OUTSIDE that anchor in a top-right control cluster, so tapping them never
// triggers card navigation. All
// interactivity is boost-safe: a native popover menu (placed by
// floatingMenuScript) and htmx.
//
// 🔴 ITS SHAPE IS mirrorCard's, SLOT FOR SLOT (cardSlot): controls, body,
// header, footer. TestAgentAndMirrorCardsRenderTheSameSlots pins it, so the two
// kinds of card cannot drift into two layouts again.
func agentCard(a AgentCardView) g.Node {
	ids := strconv.FormatInt(a.ID, 10)
	// markdownPlain, exactly as agentTitle on the detail page does. A
	// task-derived display name routinely carries `**bold**` and `code`, and
	// this rendered it RAW while the detail header rendered it as markup — one
	// string, two answers, from the same click. Stripping on both surfaces is
	// what makes them agree; see markdownPlain for why stripping was chosen over
	// rendering markup here.
	name := markdownPlain(displayOr(a.DisplayName, a.Name))
	// A name past cardTitleMax is drawn in a title row of its own, clamped,
	// with a "…" button that opens it whole (cardLongTitle).
	short, long := clampTitle(name)
	reserve := cardReserve(a.Attention)
	bodyClass := cardBodyClass(a.Focus, reserve)
	if long {
		bodyClass = titledBodyClass(reserve)
	}
	return Article(
		ID("agent-"+ids),
		g.If(a.Pinned, g.Attr("data-pinned", "")),
		// What the list's recency filter chips read (list_signals.go).
		cardActivityAttrs(time.Now(), a.lastActive()),
		g.If(waitTokens(a.Attention) != "", g.Attr("data-wait", waitTokens(a.Attention))),
		// What the list's filter finds this card by (agents_filter.go).
		cardFilterAttrs(agentFields(a), append([]string{
			displayOr(a.DisplayName, a.Name), a.Name, a.Repo, agents.KindLabel(a.Kind), a.CCAccount, a.Status, "agent"},
			focusHaystack(a.Focus)...)...),
		Class("group relative overflow-hidden rounded-2xl border border-line bg-s1/70 shadow-lg shadow-black/20 ring-1 ring-line"),
		// The control cluster (rename + ⋯ menu) is positioned over the card's
		// top-right corner, OUTSIDE the nav anchor, so its clicks don't navigate.
		cardControls(
			cardAttentionBadge(a.Attention),
			agentRenameButton(ids),
			agentMenu(a),
		),
		// With a focus, it heads the card (cardFocusTitle): a popover trigger
		// OUTSIDE the nav anchor, and the name below becomes its subtitle.
		cardFocusTitle("agent-"+ids, a.Focus, reserve),
		g.If(long, cardLongTitle("agent-"+ids, "/agents/"+a.Name, name, short, cardHasFocusTitle(a.Focus), reserve)),
		// The card body is the navigation surface: a REAL-load link to the detail
		// page (hx-boost="false"). The card sits inside #agents-list, which carries
		// hx-get/hx-target="#agents-list"/hx-swap; htmx attrs are INHERITED by
		// descendants, so a boosted click here would swap the standalone detail
		// document into #agents-list (the wrong target) — the URL changes but the
		// detail UI never renders. Disabling boost makes it a plain navigation that
		// ignores the inherited target and loads the detail page reliably. Its
		// right padding (pr-28, see cardControls) keeps the title out from under
		// the controls.
		A(
			Href("/agents/"+a.Name),
			hx("hx-boost", "false"),
			cardSlot("body"),
			// A long name is in the title row above, so the anchor is named here.
			g.If(long, g.Attr("aria-label", name)),
			Class(bodyClass),
			Div(
				cardSlot("header"),
				Class("flex flex-wrap items-center gap-2"),
				cardStatusIcon(a),
				g.If(a.Pinned, pinnedGlyph()),
				// The owed-kickoff badge sits OUTSIDE cardStatusIcon's span on
				// purpose: that span polls /ui/agents/{id}/status every 10s and
				// swaps its own innerHTML, so anything nested inside it is erased
				// on the first tick. As a sibling it survives, and the card list's
				// agents:changed re-render is what keeps it current.
				g.If(a.KickoffOwed, kickoffOwedBadge()),
				// Same placement, same reason: a sibling of the polling span.
				g.If(a.KickoffFailed, kickoffFailedBadge()),
				// Its host's icon, beside its name (host_icons.go): a dispatched
				// agent's host is the cluster.
				clusterMark(),
				g.If(!long, Span(
					Class(cardNameClass(a.Focus)),
					g.If(cardHasFocusTitle(a.Focus), g.Attr("data-card-subtitle", "")),
					g.Text(name),
				)),
				g.If(a.Repo != "", chip("repo", a.Repo)),
				// Only a non-default kind is labelled, so a gateway-only deployment's
				// cards are unchanged. The account is a NAME, never a token.
				g.If(agents.ResolveKind(a.Kind) == agents.KindClaudeCode,
					chip("kind", agents.KindLabel(a.Kind)+" · "+a.CCAccount)),
			),
			g.If(a.KickoffFailed, kickoffFailureDetail(a.KickoffFailure,
				KickoffFailedRemedy(a.KickoffResendSafe, a.Namespace, a.Name))),
			g.If(a.GitHubProblem != "", githubProblemDetail(a.GitHubProblem)),
		),
		// OUTSIDE the nav anchor: it carries a link (Connectors), and an anchor
		// nested in the card's <a> is invalid markup whose taps open the card.
		g.If(a.KickoffAwaitingGrant, kickoffAwaitingGrantNotice()),
		// Card footer (OUTSIDE the nav anchor so its task-badge link doesn't nest
		// inside the card's <a> and its clicks don't trigger card navigation): the
		// agent's age + last-activity relative times and, when dispatched from a
		// task, a "Task #<id>" badge linking to the Tasks tab.
		agentCardFooter(a),
	)
}

// agentFields is what an agent card is filtered by: its data-f-* (and data-l-*)
// attributes, which the filter's options are read from.
func agentFields(a AgentCardView) cardFields {
	return cardFields{Host: podHost, Type: "Agent", Name: filterHaystack(a.DisplayName, a.Name),
		Status: a.Status, Kind: agents.KindLabel(a.Kind), Repo: a.Repo, Account: a.CCAccount, NS: a.Namespace,
		Titles: []string{a.DisplayName, a.Name}}
}

// agentCardFooter renders the card's bottom meta row: age (CreatedAt) + last
// activity (agents.LastActivity, the list's sort key) as live <time data-ts> widgets, plus a source-task badge
// when the agent was dispatched from a task. Renders nothing when there's no
// timestamp and no source task (keeps test/minimal views clean).
func agentCardFooter(a AgentCardView) g.Node {
	if a.CreatedAt.IsZero() && a.lastActive().IsZero() && a.NoteID == nil && a.Focus == nil {
		return g.Text("")
	}
	// Nil-guard the badge (g.If evaluates its node arg eagerly, so build it first).
	var taskBadge g.Node = g.Text("")
	if a.NoteID != nil {
		taskBadge = taskSourceBadge(*a.NoteID)
	}
	return cardFooter(
		g.If(!a.CreatedAt.IsZero(), cardMeta("age", cardTime(a.CreatedAt))),
		cardActivity(a.lastActive()),
		cardFocus("agent-"+strconv.FormatInt(a.ID, 10), a.Focus),
		Span(Class("flex-1")),
		taskBadge,
	)
}

// cardSlot names one structural region of a list card: "controls", "body",
// "header" or "footer". Both card kinds are built from the same slots
// (agentCard, mirrorCard), and the attribute is what a test compares them by.
func cardSlot(name string) g.Node { return g.Attr("data-card-slot", name) }

// cardControls is a card's control cluster: positioned over its top-right
// corner, OUTSIDE the card's nav anchor, so tapping a control never opens the
// card. Each card's nav anchor reserves pr-28 (112 px) on its right: the
// cluster is right-3 (12 px) plus two 44 px controls and a 4 px gap, 104 px,
// so a long title wraps before it reaches the first control, and a tap on the
// title's end opens the card rather than a control. A third control needs a
// wider reservation: the attention badge's is cardReserve's pr-44.
func cardControls(children ...g.Node) g.Node {
	return Div(append([]g.Node{cardSlot("controls"), Class("absolute right-3 top-3 z-10 flex items-center gap-1")}, children...)...)
}

// cardFooter is a card's bottom meta row, OUTSIDE the nav anchor so a link in
// it (the task badge) is not a nested anchor and its taps do not open the card.
func cardFooter(children ...g.Node) g.Node {
	return Div(append([]g.Node{cardSlot("footer"),
		Class("flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line px-4 py-2 text-xs text-muted")}, children...)...)
}

// cardMeta is one labelled item of a card footer: "age 3h", "host tower".
func cardMeta(label string, value g.Node) g.Node {
	return Span(
		Class("inline-flex items-center gap-1"),
		Span(Class("text-muted"), g.Text(label)),
		value,
	)
}

// cardActivity is the footer's "active <relative time> ago", for BOTH card
// kinds: an agent's agents.LastActivity, a mirrored window's host-reported
// activity. Nothing for a zero time (the host did not say).
func cardActivity(t time.Time) g.Node {
	if t.IsZero() {
		return g.Text("")
	}
	return Span(
		g.Attr("data-card-active", ""),
		Class("inline-flex items-center gap-1"),
		// The recency dot, tinted on the operator's tmux idle-fade scale
		// (list_signals.go). Its box is fixed (h-2 w-2), drawn with the label,
		// so it never shifts anything in.
		recencyDot(time.Now(), t),
		Span(Class("text-muted"), g.Text("active")),
		cardTime(t),
		Span(Class("text-muted"), g.Text("ago")),
	)
}

// taskSourceBadge is the "Task #<id>" chip linking a dispatched agent back to the
// Tasks tab. It sets hx-boost="false" (like the card nav anchor) so the click is
// a plain navigation to /tasks rather than a boosted body-swap into #agents-list
// (the inherited htmx target). It lives in the card footer, OUTSIDE the card's
// nav <a>, so it is not an invalid nested anchor.
func taskSourceBadge(noteID int64) g.Node {
	return A(
		Href("/tasks"),
		hx("hx-boost", "false"),
		g.Attr("aria-label", "Source task #"+strconv.FormatInt(noteID, 10)),
		Class("press min-h-[44px] inline-flex items-center gap-1 rounded-full bg-s2 px-2 py-0.5 font-mono text-[11px] font-medium text-muted ring-1 ring-inset ring-line transition hover:bg-accent/10 hover:text-fg"),
		g.Text("Task #"+strconv.FormatInt(noteID, 10)),
	)
}

// agentRenameButton is the small pencil that swaps in the inline rename form
// (GET /ui/agents/{id}/rename) targeting the card's name row. htmx only
// (boost-safe). It targets the whole card so the swapped-in form replaces the
// card body with the edit affordance and a save/cancel.
func agentRenameButton(ids string) g.Node {
	return Button(
		Type("button"),
		g.Attr("aria-label", "Rename agent"),
		Class("press inline-flex h-11 w-11 items-center justify-center rounded-xl text-muted transition hover:bg-s2 hover:text-fg"),
		hx("hx-get", "/ui/agents/"+ids+"/rename"),
		hx("hx-target", "#agent-"+ids),
		hx("hx-swap", "outerHTML"),
		g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M13.5 3.5l3 3L7 16l-4 1 1-4z"/></svg>`),
	)
}

// agentMenu is the ⋯ context menu: a trigger button plus a FLOATING menu
// holding Start/Stop and Delete. Start/Stop occupy one slot (the agent's
// current lifecycle action); Delete uses a two-step in-menu confirm (no native
// confirm()) and is ABSENT for the chief agent (AgentCardView.IsChief), which
// the server refuses to delete anyway.
//
// 🔴 THE MENU IS A NATIVE POPOVER, NOT AN ABSOLUTE DROPDOWN, AND THE CARD IS WHY.
// It used to be a <details> whose panel was `absolute right-0` inside the card —
// and agentCard is `overflow-hidden` (its rounded corners clip the footer), so
// the panel was cut off at the card's bottom edge, and the two-step delete,
// which GROWS the panel, was cut off worst. A popover renders in the TOP LAYER:
// no ancestor's overflow, stacking context or transform can clip or bury it.
// popover="auto" is also what closes it on an outside tap and on Escape, with
// no listener of ours. Placement (below the trigger, flipped above it near the
// viewport bottom, clamped inside the viewport at phone width) is
// floatingMenuScript's, and is re-run whenever the menu's size can change.
//
// `invisible` is the other half of the placement contract: the menu is drawn
// only once floatingMenuScript has put it beside its trigger (see there).
//
// ⚠ `hidden [&:popover-open]:flex`, NOT `flex`. The UA hides a closed popover
// with a UA-origin rule, and ANY author display utility beats it — a plain
// `flex` here renders every closed menu inline in every card.
func agentMenu(a AgentCardView) g.Node {
	ids := strconv.FormatInt(a.ID, 10)
	menuID := "agent-menu-" + ids
	var lifecycle g.Node
	switch a.Status {
	case "running", "provisioning":
		lifecycle = agentMenuItem("Stop", "/agents/"+ids+"/stop", "text-fg")
	default: // stopped/pending/error
		lifecycle = agentMenuItem("Start", "/agents/"+ids+"/start", "text-accent")
	}
	return g.Group{
		Button(
			Type("button"),
			g.Attr("aria-label", "Agent actions"),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("popovertarget", menuID),
			Class("press flex h-11 w-11 cursor-pointer items-center justify-center rounded-xl text-muted transition hover:bg-s2 hover:text-fg"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><circle cx="4" cy="10" r="1.6"/><circle cx="10" cy="10" r="1.6"/><circle cx="16" cy="10" r="1.6"/></svg>`),
		),
		Div(
			ID(menuID),
			g.Attr("popover", "auto"),
			g.Attr("role", "menu"),
			g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-44 flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			lifecycle,
			agentPinMenuItem(a),
			g.If(!a.IsChief, Div(Class("my-0.5 border-t border-line"))),
			g.If(!a.IsChief, agentDeleteConfirm(ids)),
		),
	}
}

// agentMenuItem is a lifecycle action (Start/Stop) inside the ⋯ menu. It posts
// to its endpoint and morphs the agents list; floatingMenuScript closes the menu
// as the request starts, so a failed request does not leave it open.
func agentMenuItem(label, postURL, tone string) g.Node {
	return Button(
		Type("button"),
		Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-sm font-medium transition hover:bg-s2 disabled:opacity-60 "+tone),
		hx("hx-post", postURL),
		hx("hx-target", "#agents-list"),
		hx("hx-swap", "morph:innerHTML"),
		hx("hx-disabled-elt", "this"),
		g.Text(label),
	)
}

// agentDeleteConfirm is a boost-safe, native-confirm-free two-step delete inside
// the ⋯ menu: a "Delete" affordance whose nested <details> reveals an explicit
// "Confirm delete" that hx-deletes the card. No JS, no hx-confirm popup.
func agentDeleteConfirm(ids string) g.Node {
	return Details(
		Class("group/del"),
		Summary(
			Class("press flex min-h-[44px] cursor-pointer list-none items-center rounded-lg px-3 text-sm font-medium text-muted transition hover:bg-s2 hover:text-st-error-fg [&::-webkit-details-marker]:hidden"),
			g.Text("Delete"),
		),
		Button(
			Type("button"),
			Class("press mt-1 flex min-h-[44px] w-full items-center justify-center rounded-lg bg-st-error-bg px-3 text-sm font-semibold text-st-error-fg ring-1 ring-inset ring-st-error-fg/40 transition hover:bg-st-error-bg"),
			hx("hx-delete", "/agents/"+ids),
			hx("hx-target", "#agent-"+ids),
			hx("hx-swap", "outerHTML swap:200ms"),
			g.Text("Confirm delete"),
		),
	)
}

// kickoffOwedBadge is the "kickoff owed" badge: an agent whose stored first
// message was never delivered. It renders BESIDE the status dot and says nothing
// about the status itself.
//
// 🔴 IT CARRIES NO PART OF THE NOTE. The text is a FIXED label; the only thing
// this surface learns from the row is a boolean. The note is operator-authored
// instruction text and this repository is public, so putting it in markup would be
// the leak the gate exists for — see Agent.PendingNote's own comment and
// api.TestAnOwedKickoffIsReportedWithoutTheNotesText.
//
// ⚠ ITS CLASSES WERE CHOSEN TO MATCH AN EXISTING PILL'S, down to the spelling. Every Tailwind class in this package is emitted
// into the COMMITTED web/static/app.css, and `make css-check` rebuilds and refuses
// a stale copy — reusing an existing pill's classes means this badge adds zero
// bytes and the committed stylesheet needs no rebuild. A new colour here is not
// free; it is a stylesheet change.
//
// The data attribute is the handle a test parses, so a guard can ask "is THIS
// badge present" rather than grepping for a word any other element could spell.
func kickoffOwedBadge() g.Node {
	return Span(
		g.Attr("data-kickoff-owed", ""),
		g.Attr("title", "This agent's first message was never delivered — it has not been told what to do"),
		g.Attr("aria-label", "Kickoff owed: this agent's first message was never delivered"),
		glyphKnock(glyphWarning),
		Class("inline-flex items-center gap-1.5 rounded-full bg-st-warning-bg px-2.5 py-1 text-xs font-semibold text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		statusGlyph(glyphWarning, "h-3 w-3", false),
		g.Text("kickoff owed"),
	)
}

// KickoffRemedyResendSafe is the remedy under a failed kickoff whose record proves
// the task never reached the agent (agents.KickoffResendSafe).
const KickoffRemedyResendSafe = "Not retried automatically. The task never reached the agent, so " +
	"re-sending cannot pay for the task twice: open this agent's chat and send the task again."

// KickoffFailedRemedy is the remedy line under a failed kickoff. Nothing re-sends
// automatically: the deliverer stamps kicked_off before the paid turn so a turn is
// never run twice, and a turn that fails after the stamp is not retried (an
// operator decision).
//
// 🔴 IT TELLS THE OPERATOR TO CHECK BEFORE RE-SENDING UNLESS NOTHING WAS SENT. A
// shutdown, a timeout or a transport error after the request was written can leave
// the runtime still running the turn, and an empty reply can hide a turn the runtime
// retried and finished; re-sending then pays for the task twice. Only a connection
// that was never opened, or a runtime that refused the turn as not_ready for the
// whole delivery, proves otherwise (resendSafe).
//
// ⚠ ITS ONLY DYNAMIC PARTS ARE THE AGENT'S NAME AND NAMESPACE, which the card and
// GET /api/agents already show. No part of the note or of kickoff_error is in it;
// api.TestAnOwedKickoffIsReportedWithoutTheNotesText scans the rendered remedy.
func KickoffFailedRemedy(resendSafe bool, namespace, name string) string {
	if resendSafe {
		return KickoffRemedyResendSafe
	}
	return "Not retried automatically, so it is never paid twice. The agent may still be " +
		"working on this turn. Before re-sending, check its logs (kubectl -n " + namespace +
		" logs deploy/" + name + " -c agent) or its chat transcript, and re-send through its " +
		"chat only if it is not working on the task."
}

// kickoffFailedBadge is the "kickoff failed" badge: the first turn was handed to a
// gateway and did not complete. It renders BESIDE the status dot, exactly like
// kickoffOwedBadge, and for the same reasons — additive, never a status value,
// never inside cardStatusIcon's polling span (TestTheKickoffBadgesAreNotInsideThePollingStatusSpan).
//
// 🔴 IT EXISTS BECAUSE THE OWED BADGE GOES AWAY ON THE STAMP. Before it, a turn
// that failed after the stamp turned "kickoff owed" off and left the card reading
// healthy over an agent that was never told its task.
//
// The data attribute is the handle a test parses — see kickoffOwedBadge.
func kickoffFailedBadge() g.Node {
	return Span(
		g.Attr("data-kickoff-failed", ""),
		g.Attr("title", "This agent's first message was handed to its gateway and the turn did not complete"),
		g.Attr("aria-label", "Kickoff failed: this agent's first turn did not complete and is not retried automatically"),
		glyphKnock(glyphError),
		Class("inline-flex items-center gap-1.5 rounded-full bg-st-error-bg px-2.5 py-1 text-xs font-semibold text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		statusGlyph(glyphError, "h-3 w-3", false),
		g.Text("kickoff failed"),
	)
}

// kickoffAwaitingGrantNotice says why an owed first turn has not been sent: the
// agent's repository cannot be checked out until the owner approves its github
// connector request, and the turn is held (unsent, unstamped) until then —
// agents.DecideFirstTurn, for at most agents.CheckoutWaitTimeout. The approval is
// the owner's only move, so the line links straight to Connectors, where the
// request is.
//
// Its text is FIXED: no part of the note or of ccd's detail is in it.
func kickoffAwaitingGrantNotice() g.Node {
	// The wrapper's padding matches the card body's (cardBodyClass), with classes
	// already in the committed stylesheet (see kickoffOwedBadge on why that matters).
	return Div(Class("px-4 pb-4"), P(
		g.Attr("data-kickoff-awaiting-grant", ""),
		Class("break-words rounded-lg bg-st-warning-bg px-3 py-2 text-xs text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		Span(Class("font-semibold"), g.Text("Waiting for your github approval: ")),
		g.Text("this agent's repository is checked out, and its first message sent, once you approve its "+
			"github connector request. "),
		A(Href("/connectors"), Class("font-semibold underline"), g.Text("Open Connectors")),
	))
}

// githubProblemDetail is the agent card's line for a GitHub App failure that
// keeps this agent from reaching its repository (AgentCardView.GitHubProblem).
// The same classes as kickoffFailureDetail, so it needs nothing new in the
// committed stylesheet.
func githubProblemDetail(msg string) g.Node {
	return P(
		g.Attr("data-github-problem", ""),
		Class("break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		Span(Class("font-semibold"), g.Text("GitHub: ")),
		g.Text(msg),
	)
}

// kickoffFailureDetail renders the recorded failure and the remedy under the
// card header.
//
// 🔴 msg ARRIVES SCRUBBED. It is agents.KickoffFailureText, which removes any run
// of the pending note — a failed turn's error can quote the runtime's response
// body, and a runtime that echoes its request would otherwise publish the note.
// gomponents escapes it as text, so a body carrying markup renders inert.
func kickoffFailureDetail(msg, remedy string) g.Node {
	return P(
		g.Attr("data-kickoff-failure", ""),
		Class("break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		Span(Class("font-semibold"), g.Text("Kickoff failed: ")),
		g.Text(msg),
		g.Text(" "),
		Span(g.Attr("data-kickoff-remedy", ""), Class("font-semibold"), g.Text(remedy)),
	)
}

// statusDot is the labelled status pill (glyph + text). Used where space
// allows the word.
//
// The glyph is the status SHAPE (agentGlyphKind), so the pill does not rely on
// colour alone; provisioning pulses because work is in flight.
func statusDot(status string) g.Node {
	label := statusLabel(status)
	return Span(
		Class("inline-flex items-center gap-1.5 rounded-full bg-s2 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		statusGlyph(agentGlyphKind(status), "h-2.5 w-2.5", status == "provisioning"),
		g.Text(label),
	)
}

// statusIcon is the COMPACT status indicator for the agent card: just the glyph
// (no text label box), with the status word carried as a title / aria-label
// tooltip. Same mapping as statusDot.
func statusIcon(status string) g.Node {
	label := statusLabel(status)
	return Span(
		g.Attr("title", label),
		g.Attr("aria-label", "Status: "+label),
		g.Attr("role", "img"),
		Class("relative flex h-3 w-3 shrink-0"),
		statusGlyph(agentGlyphKind(status), "h-3 w-3", status == "provisioning"),
	)
}

// RenderStatusIcon writes the compact status icon (the colored dot, no label) for
// a status. It is the exported render entry for the live-status poll endpoint
// (GET /ui/agents/{id}/status), reusing statusIcon so the polled icon matches the
// first-paint icon in the agent-detail / operator header exactly.
//
// awaitingApproval adds the "awaiting approval" badge beside the dot: the agent's
// terminal is blocked on a permission prompt, or it has a request open on the
// operator's approval queue. 🔴 THE BADGE LIVES IN THIS POLLED RESPONSE ON
// PURPOSE — it is a live state, so the 10s poll is what both shows and clears it.
// It is the opposite of the owed-kickoff badge, which must sit OUTSIDE the
// polling span because this endpoint never rendered it.
func RenderStatusIcon(w io.Writer, status string, awaitingApproval bool) error {
	if !awaitingApproval {
		return statusIcon(status).Render(w)
	}
	return g.Group([]g.Node{statusIcon(status), AwaitingApprovalBadge()}).Render(w)
}

// AwaitingApprovalBadgeID is the data attribute the badge carries, so tests and
// scripts find it by STATE, not by its wording.
const AwaitingApprovalBadgeID = "awaiting-approval"

// AwaitingApprovalBadge is the small pill shown while an agent waits on a human
// to approve a tool call.
func AwaitingApprovalBadge() g.Node {
	return Span(
		g.Attr("data-badge", AwaitingApprovalBadgeID),
		g.Attr("title", "Blocked on a permission prompt: approve or deny it on your phone or in the agent's terminal"),
		Class("ml-1.5 inline-flex items-center gap-1.5 whitespace-nowrap rounded-full bg-st-warning-bg px-2.5 py-1 text-xs font-semibold text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		g.Text("awaiting approval"),
	)
}

// cardStatusIcon wraps the compact status icon in a self-polling span so each
// agent card keeps its status live (every 10s) on its own — without the list
// re-rendering on a timer. No shared id (hx-target=this) so it's safe once per
// card; the icon swaps in from GET /ui/agents/{id}/status.
func cardStatusIcon(a AgentCardView) g.Node {
	return Span(
		hx("hx-get", "/ui/agents/"+strconv.FormatInt(a.ID, 10)+"/status"),
		hx("hx-trigger", "load, every 10s"),
		hx("hx-target", "this"),
		hx("hx-swap", "innerHTML"),
		statusIcon(a.Status),
	)
}

// statusLabel is the agent-facing word for a stored agent status.
func statusLabel(status string) string {
	switch status {
	case "running", "provisioning", "error", "stopped":
		return status
	default:
		return "pending"
	}
}

// agentsListTrigger is #agents-list's hx-trigger: first load, a local
// lifecycle action's HX-Trigger, and the two server events that change what the
// list shows or how it is ordered. See AgentsPanel.
//
// sse:attention_queue.changed (internal/api's EventAttentionChanged, the event
// the sidebar's attention badge refreshes on) keeps each card's attention badge
// live: the queue moved, so the cards' counts may have. It is debounced
// (delay:50ms) for the reason attentionBadgeTrigger gives: the sidebar badge
// listens for the same event, so each one reaches this list twice.
const agentsListTrigger = "load, agents:changed from:body, sse:agent.changed from:body, sse:chat.reply from:body, sse:attention_queue.changed from:body delay:50ms, muster:resync from:body"

// agentModalShell is the static (always-present, hidden) dispatch modal. Its
// body is loaded on demand by the FAB so it stays out of the 4s poll cycle.
func agentModalShell() g.Node {
	return Div(
		ID("agent-modal"),
		Class("hidden fixed inset-0 z-40 flex items-end justify-center sm:items-center"),
		Div(
			Class("absolute inset-0 bg-black/60"),
			// Ambiguous dismiss → the SAME dirty guard the task sheet uses, so a
			// typed ad-hoc prompt is not destroyed by a stray tap.
			hx("hx-on:click", "if(window.cgModalDismiss){window.cgModalDismiss('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
		),
		Div(
			ID("agent-modal-body"),
			// Same height cap as #task-modal-body: an uncapped `items-end` sheet
			// pushes its own header (and the ✕) off the top of a phone viewport and
			// leaves no tappable backdrop, so the dialog becomes a dead end.
			// pb uses the safe-area inset for the same reason <main> and the FABs do:
			// this sheet sits flush against the viewport bottom, so a flat p-5 put its
			// last control (Dispatch) under the iOS home indicator.
			Class("relative z-10 max-h-[85dvh] w-full max-w-lg overflow-y-auto overscroll-contain rounded-t-2xl bg-s1 p-5 pb-[calc(1.25rem+env(safe-area-inset-bottom))] shadow-2xl ring-1 ring-line sm:rounded-2xl sm:pb-5"),
			// body injected via /ui/agents/new
		),
		modalDiscardBar("agent-modal"),
	)
}

// DispatchModalBody is the /ui/agents/new partial (FAB path): the full dispatch
// form. It renders INSTANTLY — the repo + task option lists lazy-load (with
// animated skeletons) and the model field searches OpenRouter on type, so the
// form never blocks on a slow upstream. Morphed into #agent-modal-body when the
// FAB is tapped. The Task-card Dispatch button uses the task-scoped variant
// (DispatchModalTaskBody) instead; this FAB form is unchanged.
func DispatchModalBody() g.Node { return DispatchModalBodyFor(KindChoices{}) }

// DispatchModalBodyFor is DispatchModalBody with the deployment's agent kinds;
// more than one renders the kind picker (see kindPicker).
func DispatchModalBodyFor(k KindChoices) g.Node {
	return g.Group{
		Div(
			Class("mb-4 flex items-center gap-3"),
			H2(Class("text-base font-semibold text-fg"), g.Text("Dispatch agent")),
			Span(Class("flex-1")),
			dispatchCloseButton(),
		),
		Form(
			hx("hx-post", "/agents"),
			hx("hx-target", "#agents-list"),
			hx("hx-swap", "morph:innerHTML"),
			// Close the modal after a successful dispatch — but ONLY for THIS form's
			// own request. htmx:afterRequest bubbles, so the lazy repo/task combobox
			// loads (hx-get on child <ul>s) would otherwise fire this handler and slam
			// the modal shut ~immediately after it opens (event.target is the child,
			// not the form). Guarding on event.target===this scopes it to the submit.
			hx("hx-on::after-request", dispatchCloseOnOwnSubmit),
			Class("flex flex-col gap-3"),
			// The "what to dispatch" (an existing task or an ad-hoc description) is the
			// primary decision, so it stays visible. Model/Repository are
			// tucked under a collapsed "Advanced" disclosure — they default to the
			// cluster's MUSTER_AGENT_MODEL + last-used repo (localStorage recents)
			// so a one-tap dispatch is sane without ever opening Advanced.
			labelledField("Existing task", lazyCombobox("note", "Existing task", "Search tasks… (optional)", "note_id", "/ui/agents/notes", "")),
			labelledField("Or ad-hoc task", Textarea(
				Name("note_text"),
				Rows("3"),
				Placeholder("Task for the agent (used if no task selected)…"),
				Class("w-full resize-y rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
			)),
			kindPicker(k),
			// The model is gateway-only (a claude-code dispatch is refused one, see
			// kindToggleScript); the repository is for both kinds — a claude-code
			// agent's session starts in a checkout of it.
			advancedDisclosure(
				gatewayOnly(labelledField("Model", modelField())),
				labelledField("Repository", lazyCombobox("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos")),
			),
			Div(
				// Sticky as a ROW: both actions stay in view when Advanced pushes them
				// below the sheet's fold (see TestEverySheetKeepsItsPrimaryActionInView).
				Class("sticky bottom-0 z-10 mt-1 grid grid-cols-2 gap-2 bg-s1 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))]"),
				Button(
					Type("submit"),
					Name("action"), Value("dispatch"),
					Class("press inline-flex items-center justify-center rounded-xl bg-accent px-4 py-3 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					// 🔴 No hx-disabled-elt="this" here. htmx reads hx-disabled-elt from
					// the element that ISSUES the request, and this POST is issued by the
					// FORM's submit event — so the attribute that used to sit on both
					// submit buttons could provably never fire (same finding as the task
					// edit form, notes.go). It was removed rather than left in place
					// reading as a working double-submit guard.
					//
					// 🔴 The justification that used to follow — "moving it to the <form>
					// is not a drop-in because hx-disabled-elt is inherited, so the lazy
					// children's hx-get on `load` would disable both buttons, and a
					// form-level guard needs hx-disinherit" — is RETRACTED. The attribute
					// IS inherited, but its VALUE is resolved against the issuing element
					// (htmx 2.0.4, findAttributeTargets): "this" resolves to the closest
					// ancestor-or-self CARRYING the attribute (the form, which has no
					// disabled state), and "find X" is scoped to the issuer, so a lazy
					// <ul>/<div> containing no submit button matches nothing. Neither
					// reaches these buttons. No hx-disinherit is required.
					//
					// What is actually true of the code below: the form has NO in-flight
					// double-submit guard at all. The task edit form's working idiom is
					// hx-disabled-elt="find button[type='submit']" ON THE FORM (notes.go).
					// That is the change to make here; it is simply not made yet, and it
					// is untested against this form's two lazy children (the task and
					// repo comboboxes each fire hx-get on `load`; the model field is
					// seeded, not lazy).
					g.Text("Dispatch"),
				),
				Button(
					Type("submit"),
					Name("action"), Value("save"),
					Class("press inline-flex items-center justify-center rounded-xl px-4 py-3 text-sm font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s2 active:scale-[0.98] disabled:opacity-60"),
					// See the Dispatch button above: hx-disabled-elt="this" on a submit
					// button is inert.
					g.Text("Save for later"),
				),
			),
		),
	}
}

// dispatchCloseOnOwnSubmit closes the dispatch modal after THIS form's own
// successful submit (guarded by event.target===this so bubbled child combobox
// loads don't slam it shut) and fires the dispatch analytics event. Shared by
// the FAB and task-scoped dispatch forms.
const dispatchCloseOnOwnSubmit = "if(event.target===this && event.detail.successful){if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}try{window.cgTrack('agent.dispatch',{});}catch(e){}}"

// dispatchCloseButton is the modal's top-right close (×).
func dispatchCloseButton() g.Node {
	return Button(
		Type("button"),
		g.Attr("aria-label", "Close"),
		Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
		hx("hx-on:click", "if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
	)
}

// TaskDispatchView is the resolved dispatch config for a Task-scoped dispatch
// confirm (the from-card path). Every field is pre-resolved server-side so the
// modal is a pre-filled confirm: Model/Repo/RepoBranch are the effective values
// (empty = cluster default / none / repo default).
type TaskDispatchView struct {
	NoteID     int64
	Label      string // the task's first line / directory (header subtitle)
	Model      string // resolved OpenRouter slug ("" = cluster default)
	Repo       string // resolved repo full name ("" = none / not inferred)
	RepoBranch string // resolved branch ("" = repo default, resolved server-side)

	// FromDetailPage switches the form's SWAP TARGET, and it is the difference
	// between Dispatch working and Dispatch being inert.
	//
	// 🔴 htmx resolves hx-target BEFORE it issues the request; when the selector
	// matches nothing it raises htmx:targetError and sends NOTHING. Nothing in
	// internal/ui listens for that event, so on a document without the target
	// there is no request, no error, no toast — the modal does not even close.
	// This form's default target, #agents-list, exists ONLY inside AgentsPanel,
	// i.e. only on the shell; on /tasks/{id} the Dispatch button opened a modal
	// whose submit did nothing at all.
	//
	// On the detail page the form therefore targets the task's own card, and
	// POST /agents answers with that card (see internal/api's handleAgentCreate):
	// the dispatch's real, visible effect there is the task flipping to
	// in-progress with its new agent's status chip.
	FromDetailPage bool

	// Kinds is the deployment's agent kinds; more than one renders the picker.
	Kinds KindChoices
}

// dispatchTarget is the (selector, swap) pair the task-scoped dispatch form
// posts into. ONE function, so the two documents cannot drift apart, and so the
// answer is derivable in a test rather than spelled twice.
func (v TaskDispatchView) dispatchTarget() (selector, swap string) {
	if v.FromDetailPage {
		return "#task-" + strconv.FormatInt(v.NoteID, 10), "morph:outerHTML"
	}
	return "#agents-list", "morph:innerHTML"
}

// DispatchModalTaskBody is the /ui/agents/new?note=<id> partial: a TASK-SCOPED
// dispatch confirm. The task is known, so instead of the full form's "Existing
// task" combobox + ad-hoc textarea, it shows two fixed rows — Model and
// Repository — each with the task's resolved value as static text plus an inline
// "Edit" that reveals the corresponding editable control (pre-seeded to the
// current value). The editable controls carry the same field names the dispatch
// POST reads (model / repo), and — being present-but-hidden — they
// submit the resolved values even when never opened, so this is a one-tap confirm.
// Overrides are THIS-DISPATCH-ONLY (never persisted back to the task). The task is
// submitted via a hidden note_id; repo_branch rides along as a hidden field.
func DispatchModalTaskBody(v TaskDispatchView) g.Node {
	ids := strconv.FormatInt(v.NoteID, 10)
	target, swap := v.dispatchTarget()

	// Static row displays.
	modelStatic := shortModelLabel(v.Model)
	if modelStatic == "" {
		modelStatic = defaultModelShortLabel()
	}
	repoStatic := v.Repo
	if repoStatic == "" {
		repoStatic = "none"
	}

	return g.Group{
		Div(
			Class("mb-4 flex items-start gap-3"),
			Div(
				Class("min-w-0"),
				H2(Class("text-base font-semibold text-fg"), g.Text("Dispatch · Task #"+ids)),
				g.If(v.Label != "", P(Class("mt-0.5 truncate text-xs text-muted"), g.Text(v.Label))),
			),
			Span(Class("flex-1")),
			dispatchCloseButton(),
		),
		Form(
			hx("hx-post", "/agents"),
			// See TaskDispatchView.FromDetailPage: #agents-list exists only on the
			// shell, and an unresolvable hx-target makes htmx drop the request
			// silently rather than fail loudly.
			hx("hx-target", target),
			hx("hx-swap", swap),
			hx("hx-on::after-request", dispatchCloseOnOwnSubmit),
			Class("flex flex-col gap-3"),
			// The task is known — submit it by id (no combobox, no ad-hoc textarea).
			Input(Type("hidden"), Name("note_id"), Value(ids)),
			// Resolved branch rides along (no visible row; the FAB form has no branch
			// field either — it resolves server-side when empty).
			Input(Type("hidden"), Name("repo_branch"), Value(v.RepoBranch)),
			kindPicker(v.Kinds),
			// Gateway-only, like the FAB form's model field: the task's model is
			// refused for a claude-code dispatch, so it leaves the screen when
			// Claude Code is picked (kindToggleScript). The repository stays: both
			// kinds work in it.
			gatewayOnly(
				Class("flex flex-col gap-3"),
				dispatchConfirmRow("Model", modelStatic, modelFieldFor(v.Model, "")),
			),
			dispatchConfirmRow("Repository", repoStatic,
				lazyComboboxPreselect("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos", v.Repo, v.Repo)),
			// Mobile-first action stack: a small secondary Cancel sits ABOVE the
			// full-width primary Dispatch, so the thumb lands on Dispatch and Cancel
			// is a deliberate, smaller reach.
			Div(
				Class("mt-1 flex flex-col gap-2"),
				Button(
					Type("button"),
					Class("press min-h-[44px] self-end inline-flex items-center justify-center rounded-lg px-3 py-1.5 text-xs font-medium text-muted ring-1 ring-inset ring-edge transition hover:bg-s2 active:scale-[0.98]"),
					hx("hx-on:click", "if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
					g.Text("Cancel"),
				),
				Button(
					Type("submit"),
					Name("action"), Value("dispatch"),
					Class("sticky bottom-0 z-10 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))] press w-full inline-flex items-center justify-center rounded-xl bg-accent px-4 py-3 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					hx("hx-disabled-elt", "this"),
					g.Text("Dispatch"),
				),
			),
		),
	}
}

// dispatchConfirmRow is one row of the task-scoped confirm: a label, the resolved
// value as static text, and an inline "Edit" that reveals the (pre-seeded)
// editable control. The editable control is present-but-hidden so it submits the
// resolved value even when never opened; Edit only unhides it for an override.
// The toggle is a CSP-safe hx-on:click (the app's CSP allows hx-on via
// unsafe-eval) that hides the static value + Edit and shows the control — the same
// data-attribute reveal pattern the delete control uses.
func dispatchConfirmRow(label, static string, editable g.Node) g.Node {
	const toggle = "var r=this.closest('[data-dispatch-row]');" +
		"r.querySelector('[data-dispatch-static]').classList.add('hidden');" +
		"r.querySelector('[data-dispatch-editable]').classList.remove('hidden');" +
		"this.classList.add('hidden');"
	return Div(
		g.Attr("data-dispatch-row", ""),
		Class("flex flex-col gap-1.5"),
		Div(
			Class("flex items-center gap-2"),
			Label(Class("text-xs font-medium text-muted"), g.Text(label)),
			Span(Class("flex-1")),
			Button(
				Type("button"),
				g.Attr("data-dispatch-edit", ""),
				g.Attr("aria-label", "Edit "+label),
				Class("press min-h-[44px] rounded-lg px-2 py-0.5 text-xs font-medium text-accent transition hover:bg-accent/10 hover:text-fg"),
				hx("hx-on:click", toggle),
				g.Text("Edit"),
			),
		),
		Div(
			g.Attr("data-dispatch-static", ""),
			Class("truncate text-sm text-fg"),
			g.Text(static),
		),
		Div(
			g.Attr("data-dispatch-editable", ""),
			Class("hidden"),
			editable,
		),
	)
}

// comboOption renders one combobox <li> (data-value + data-label).
func comboOption(o ComboOption) g.Node {
	return Li(
		g.Attr("role", "option"),
		g.Attr("data-combobox-option", ""),
		g.Attr("data-value", o.Value),
		g.Attr("data-label", o.Label),
		Class("cursor-pointer truncate px-3 py-2 text-sm text-fg hover:bg-accent/15 hover:text-fg data-[active=true]:bg-accent/15 data-[active=true]:text-fg"),
		g.Text(o.Label),
	)
}

// seededComboOption is a combobox <li> marked as a SEEDED quick-pick
// (data-combobox-seed): in a remote combobox these are preserved at the top of
// the list across remote searches (the combobox JS filters them by query and
// dedupes them against remote results) instead of being wiped on each fetch.
func seededComboOption(o ComboOption) g.Node {
	return Li(
		g.Attr("role", "option"),
		g.Attr("data-combobox-option", ""),
		g.Attr("data-combobox-seed", ""),
		g.Attr("data-value", o.Value),
		g.Attr("data-label", o.Label),
		Class("cursor-pointer truncate px-3 py-2 text-sm text-fg hover:bg-accent/15 hover:text-fg data-[active=true]:bg-accent/15 data-[active=true]:text-fg"),
		g.Text(o.Label),
	)
}

// RenderRepoOptions / RenderNoteOptions write the lazy-loaded combobox option
// lists (swapped in over the skeleton).
func RenderRepoOptions(w io.Writer, repos []RepoView) error {
	return comboOptionList(repoOptions(repos)).Render(w)
}

func RenderNoteOptions(w io.Writer, notes []NoteOption) error {
	return comboOptionList(noteOptions(notes)).Render(w)
}

func comboOptionList(opts []ComboOption) g.Node {
	if len(opts) == 0 {
		return Li(Class("px-3 py-2 text-sm text-muted"), g.Text("none"))
	}
	return g.Group(g.Map(opts, comboOption))
}

func repoOptions(repos []RepoView) []ComboOption {
	opts := make([]ComboOption, 0, len(repos))
	for _, r := range repos {
		opts = append(opts, ComboOption{Value: r.FullName, Label: r.FullName})
	}
	return opts
}

func noteOptions(notes []NoteOption) []ComboOption {
	opts := make([]ComboOption, 0, len(notes))
	for _, n := range notes {
		opts = append(opts, ComboOption{Value: strconv.FormatInt(n.ID, 10), Label: n.Label})
	}
	return opts
}

// comboboxSkeleton is the subtly-animated placeholder shown in a lazy combobox
// list (and inside the dropdown) until the real options load.
func comboboxSkeleton() g.Node {
	bar := func(w string) g.Node {
		return Li(Class("px-3 py-2"), Div(Class("h-3 "+w+" animate-pulse rounded bg-s3/60")))
	}
	return g.Group{bar("w-3/4"), bar("w-1/2"), bar("w-2/3")}
}

// CuratedModel is one option in the curated model dropdown.
type CuratedModel struct {
	Value string // submitted model slug; empty means "use the server default"
	Label string // visible option text
}

// curatedModels are the known-good models offered in the dispatch modal's common
// path. The first (empty value) is the default: posting an empty model lets the
// server's MUSTER_AGENT_MODEL win. The rest are named, vetted slugs. Power users
// can still reach ANY OpenRouter slug via the "search OpenRouter…" expander.
func curatedModels() []CuratedModel {
	out := []CuratedModel{
		{Value: "", Label: "default (" + defaultModelShortLabel() + ")"},
		{Value: "openrouter/anthropic/claude-haiku-4.5", Label: shortModelLabel("openrouter/anthropic/claude-haiku-4.5")},
		{Value: "openrouter/anthropic/claude-sonnet-4.6", Label: shortModelLabel("openrouter/anthropic/claude-sonnet-4.6")},
	}
	// Fold in the other known-good slugs the agents package already references
	// (deduped against the default), so this list tracks the dispatch suggestions.
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Value] = true
	}
	def := agents.DefaultModelOption()
	for _, m := range agents.ModelOptions {
		if m.Value == def || seen[m.Value] {
			continue
		}
		seen[m.Value] = true
		out = append(out, CuratedModel{Value: m.Value, Label: shortModelLabel(m.Value)})
	}
	return out
}

// defaultModelShortLabel is the short label for the server-default option.
func defaultModelShortLabel() string {
	if def := agents.DefaultModelOption(); def != "" {
		return shortModelLabel(def)
	}
	return "server default"
}

// shortModelLabel trims the leading "openrouter/" provider prefix from a slug for
// display, keeping the rest of the path intact
// (e.g. "openrouter/anthropic/claude-haiku-4.5" → "anthropic/claude-haiku-4.5").
// Slugs without the prefix (and non-model values like repo names) pass through.
func shortModelLabel(slug string) string {
	return strings.TrimPrefix(slug, "openrouter/")
}

// modelName is the AGENT-VIEW model display: just the model's own name — the
// last '/'-segment of an OpenRouter slug — e.g. "openrouter/deepseek/deepseek-v4-pro"
// → "deepseek-v4-pro". Empty in → empty out. Unlike shortModelLabel (which only
// strips the "openrouter/" provider prefix, keeping the vendor path) this drops
// everything before the final segment. Used ONLY for the agent-detail model
// control's visible label; the hidden input still submits the full slug.
func modelName(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ""
	}
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}

// agentModelLabel is the short display label for an agent's configured model:
// the slug with its leading "openrouter/" prefix trimmed, or "default" when unset
// (cluster default wins).
func agentModelLabel(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "default"
	}
	return shortModelLabel(slug)
}

// modelField is the LLM model selector: ONE consolidated searchable combobox
// (the 0.8.0 component). It shows the curated quick-picks as initial options,
// filters them as you type, AND searches any OpenRouter slug live (proxied via
// /api/openrouter/models). The visible input IS the submitted `model` field
// (id="model-input"); an empty value means "use the cluster default".
func modelField() g.Node { return modelFieldFor("", "") }

// modelFieldWithDisplay is modelFieldFor with an explicit visible-label override.
// When display is non-empty it is shown in the visible input verbatim (the hidden
// input still submits the FULL `selected` slug); when empty the default display
// (shortModelLabel / cluster-default label) is used. The agent-detail control
// passes modelName(selected) so the visible label is just the model name while
// switching still posts the full slug.
func modelFieldWithDisplay(selected, note, display string) g.Node {
	return modelFieldForDisplay(selected, note, display)
}

// modelFieldFor renders the consolidated searchable model combobox, pre-seeded
// with `selected` (an OpenRouter slug; "" = the cluster default). Used by both
// the dispatch modal (selected="") and the agent-detail change control
// (selected=agent.Model) so the control reflects the agent's current model.
//
// It reuses the existing custom combobox component (data-combobox +
// data-combobox-input + data-combobox-list + data-combobox-remote) in
// hidden-value mode (data-combobox-hidden). The list is pre-seeded with the
// curated quick-picks as inline <li data-combobox-option> entries so they show
// without a remote round-trip; typing filters those AND fetches matching
// OpenRouter slugs from the proxy. A hidden input named `model` carries the
// submitted FULL slug; the visible input (id="model-input") shows the trimmed
// display label only. On pick, the combobox JS writes the option's data-value
// (full slug) into the hidden field and its data-label (trimmed) into the visible
// input. The hidden field is pre-filled with `selected` (the current slug) so an
// unchanged submit re-posts it; empty when nothing is selected (server default
// wins).
func modelFieldFor(selected, note string) g.Node {
	return modelFieldForDisplay(selected, note, "")
}

// modelFieldForDisplay is modelFieldFor with an optional visible-label override
// (displayOverride). Empty override → the default display (shortModelLabel of the
// selected slug, or the cluster-default label when unset). The hidden `model`
// input always carries the FULL `selected` slug regardless of the visible label.
func modelFieldForDisplay(selected, note, displayOverride string) g.Node {
	listID := "combobox-list-model"
	curated := curatedModels()

	// Seed the list with the curated quick-picks. The first entry (empty value)
	// is the cluster default; render it with the ◉ marker and a clear label so an
	// empty submit obviously means "default". The combobox is in hidden-value mode
	// (data-combobox-hidden), so each option's data-value is the FULL slug written
	// into the hidden `model` field on pick, while its data-label is the trimmed
	// display shown in the visible input. The empty default writes an empty value.
	seeded := make([]g.Node, 0, len(curated))
	for i, m := range curated {
		label := m.Label
		if i == 0 {
			label = "◉ " + label
		}
		seeded = append(seeded, seededComboOption(ComboOption{Value: m.Value, Label: label}))
	}

	// The visible input shows the CURRENT model rather than a bare placeholder:
	// the trimmed slug, or — when nothing is set — the effective cluster-default
	// model's short label (just the name, no "default (…)" wrapper). The hidden
	// field still carries the real submit value ("" = default), so display is
	// purely cosmetic; the dropdown's ◉ option is where "reset to default" lives.
	// An explicit override (agent-view name-only label) wins; otherwise the
	// default display: the trimmed slug, or the cluster-default label when unset.
	display := displayOverride
	if display == "" {
		display = shortModelLabel(selected)
		if display == "" {
			display = defaultModelShortLabel()
		}
	}

	// A passive (non-option) sticky note pinned to the top of the dropdown — used
	// for the agent-detail "selecting a model restarts the agent" warning. Empty
	// in the dispatch modal (creating a new agent restarts nothing). It carries no
	// data-combobox-option, so the filter/keyboard nav ignores it.
	var noteNode g.Node = g.Text("")
	if note != "" {
		noteNode = Li(
			g.Attr("aria-hidden", "true"),
			Class("sticky top-0 z-10 border-b border-line bg-s1 px-3 py-1.5 text-[11px] text-st-warning-fg"),
			g.Text("↳ "+note),
		)
	}

	return Div(
		g.Attr("data-combobox", ""),
		g.Attr("data-combobox-remote", "/api/openrouter/models"),
		Class("relative"),
		// Hidden field carries the SUBMITTED full slug ("" = cluster default). The
		// visible input below only drives display/search and does not submit.
		Input(Type("hidden"), Name("model"), g.Attr("data-combobox-hidden", ""), Value(selected)),
		Input(
			Type("text"),
			ID("model-input"),
			g.Attr("data-combobox-input", ""),
			g.Attr("role", "combobox"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", listID),
			g.Attr("aria-label", "Model — pick a quick-pick or type to filter/search"),
			g.Attr("autocomplete", "off"),
			// Pre-filled with the TRIMMED display of the current slug so the user sees
			// a clean label; the hidden field holds the full slug that submits. Empty
			// when nothing is selected → the cluster default wins unless changed.
			Value(display),
			Placeholder("Model — quick-picks, or type to filter/search…"),
			Class("w-full rounded-lg border-0 bg-bg px-3 py-2.5 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Ul(
			ID(listID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			// Pre-seeded curated quick-picks; typing filters these and also fetches
			// matching OpenRouter slugs from the remote proxy.
			Class("hidden absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			noteNode,
			g.Group(seeded),
		),
	)
}

// lazyCombobox is a type-to-filter combobox whose option list lazy-loads from
// optionsURL (hx-trigger load) into an animated skeleton — so the modal renders
// instantly and never blocks on the GitHub API / DB. A hidden input (hiddenName)
// carries the submitted value (full name for repos, note id for tasks). When
// recentsKey is non-empty, recently-picked values float to the top (localStorage,
// wired in appScript). Behaviour is delegated in appScript via data-combobox*.
func lazyCombobox(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey string) g.Node {
	return lazyComboboxPreselect(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey, "", "")
}

// lazyComboboxPreselect is lazyCombobox with an optional pre-selected value. When
// presetValue is non-empty the hidden input is pre-filled with presetValue and the
// visible input shows presetLabel — these are SIBLINGS of the lazy <ul>, so they
// survive the options swap and an unchanged submit re-posts the selection. A
// SEEDED quick-pick option (data-combobox-seed) for the same value is also rendered
// at the top of the list so the pick is visible immediately on open (the lazy
// hx-get later swaps the <ul> with the real task list, which includes this task as
// a normal option). When presetValue is empty it renders EXACTLY like the plain
// lazyCombobox (the FAB path stays byte-for-byte unchanged).
func lazyComboboxPreselect(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey, presetValue, presetLabel string) g.Node {
	listID := "combobox-list-" + fieldName
	wrapAttrs := []g.Node{g.Attr("data-combobox", ""), Class("relative")}
	if recentsKey != "" {
		wrapAttrs = append(wrapAttrs, g.Attr("data-combobox-recents", recentsKey))
	}

	hiddenAttrs := []g.Node{Type("hidden"), Name(hiddenName), g.Attr("data-combobox-hidden", "")}
	inputAttrs := []g.Node{
		Type("text"),
		g.Attr("data-combobox-input", ""),
		g.Attr("role", "combobox"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", listID),
		g.Attr("aria-label", label),
		g.Attr("autocomplete", "off"),
		Placeholder(placeholder),
		Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
	}
	// The seeded preselect option lives at the TOP of the list (before the
	// skeleton). The lazy load (hx-get) swaps the list's innerHTML, but
	// RenderNoteOptions renders only non-seed options and the combobox JS dedupes
	// against seeds — so the seed survives and the hidden value stays valid.
	var seed g.Node = g.Text("")
	if presetValue != "" {
		hiddenAttrs = append(hiddenAttrs, Value(presetValue))
		inputAttrs = append(inputAttrs, Value(presetLabel))
		seed = seededComboOption(ComboOption{Value: presetValue, Label: presetLabel})
	}

	wrapAttrs = append(wrapAttrs,
		Input(hiddenAttrs...),
		Input(inputAttrs...),
		Ul(
			ID(listID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			// Lazy-load the options when the modal opens (load fires on swap-in);
			// the skeleton shows until they arrive.
			hx("hx-get", optionsURL),
			hx("hx-trigger", "load"),
			// Target THIS <ul> explicitly. htmx hx-target is inherited by
			// descendants, and this combobox lives inside the dispatch form whose
			// hx-target is "#agents-list" — without this, the options would load into
			// #agents-list (turning the agents panel into a repo/task list) and the
			// skeleton here would never resolve.
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			Class("hidden absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			seed,
			comboboxSkeleton(),
		),
	)
	return Div(wrapAttrs...)
}

func displayOr(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}
