package ui

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/notes"
)

// ChatLine is one persisted chat message rendered in the detail transcript. For
// a structured assistant turn (0.7.64) a line is one PART: Kind 'text' renders a
// bubble; 'tool_call'/'tool_result' render the same collapsed tool chip the live
// stream shows (Content = args / output). Legacy rows carry an EMPTY Kind, or
// "text".
//
// ⚠ THE EMPTY CASE IS SPELLED OUT IN WORDS ON PURPOSE. It used to name the
// empty string with a pair of adjacent ASCII single quotes, and gofmt's
// doc-comment reformatting rewrites that pair into a closing curly DOUBLE
// quote — so the line silently stopped naming the empty string at all. Nothing
// errors and nothing fails; the comment just quietly means something else, and
// the only tell is a character that looks almost identical to what was typed.
type ChatLine struct {
	Role     string // "user" | "assistant"
	Content  string
	Kind     string // "" | "text" | "tool_call" | "tool_result"
	ToolID   string
	ToolName string
	ToolOK   bool
	// ToolTarget is a tool_call's one-line summary (the file, the command). ""
	// derives it from Content (the args) with cctranscript.ToolTarget — what a
	// persisted row does; the mirrored view sets it from the host's own reading.
	ToolTarget string
}

// ChatSurface names WHERE a chat pane is being rendered.
//
// 🔴 IT EXISTS BECAUSE agentChatPane IS RE-MOUNTED IN A TRANSFORMED ANCESTOR AND
// ONE OF ITS CHILDREN IS `position: fixed`. sessionDrawer is `fixed inset-y-0
// right-0`, which resolves against the nearest ancestor that establishes a
// containing block — and ChiefPanel carries `translate-x-full` +
// `transition-transform`, so a transform on it makes the PANEL that ancestor.
// Inside the panel the drawer therefore sits at the panel's own right edge, and
// when the panel is shut it is translated off-screen and `inert` along with it:
// unreachable, with nothing on screen saying so. The panel gets a sub-view of its
// own column instead (chiefThreadList), which is what this field selects.
//
// ⚠ THE ZERO VALUE IS THE PAGE, DELIBERATELY. AgentDetailPage and OperatorPage
// construct this view in several places and none of them may change behaviour;
// a surface that had to be set explicitly would silently re-route one of them.
type ChatSurface string

const (
	// ChatSurfacePage is a standalone document — AgentDetailPage or OperatorPage.
	ChatSurfacePage ChatSurface = ""
	// ChatSurfaceChiefPanel is the chief slide-out inside the home shell.
	ChatSurfaceChiefPanel ChatSurface = "chief-panel"
)

// AgentDetailView is the data the agent detail page renders.
type AgentDetailView struct {
	ID          int64
	Name        string
	DisplayName string
	Status      string
	Repo        string
	Model       string // OpenRouter slug; "" = cluster default
	// Kind is the agent's kind. A claude-code agent shows its kind and account
	// instead of the model picker: it has no per-agent model (the server refuses
	// one), so a picker there would be a control that can only fail.
	Kind      string
	CCAccount string
	Messages  []ChatLine
	// Sessions are the agent's chat sessions for the switcher (most-recently-active
	// first); ActiveSessionID is the one whose transcript Messages holds.
	Sessions        []SessionView
	ActiveSessionID int64
	// Surface is where this pane is being rendered. See ChatSurface.
	Surface ChatSurface

	// Source task (Task 6): when this agent was dispatched from a Task, NoteID is
	// its task id and TaskStatus its current lifecycle status. Both are populated
	// best-effort by the handler; NoteID == nil (or a non-terminal status) means
	// no completion banner is rendered.
	NoteID     *int64
	TaskStatus string

	// SendNonce names one message for the chat form's FALLBACK post (see
	// agentChatPane and ComposerView.Nonce); "" = none. SendNotice is the
	// outcome of such a post, drawn above the form when the page was reached by
	// the redirect that answered it; SendNoticeOK says it is good news.
	SendNonce    string
	SendNotice   string
	SendNoticeOK bool
	// Dictate is where the chat form's microphone posts a recording
	// (TranscribePath), or "" for none. See dictate.go.
	Dictate string
}

// SessionView is one chat session in the switcher.
type SessionView struct {
	ID int64
	// Title is the session's name, derived by the store from its first user message.
	//
	// 🔴 AN EMPTY TITLE RENDERS DIFFERENTLY ON THE TWO SURFACES THAT DRAW THESE ROWS,
	// AND THAT IS DELIBERATE RATHER THAN DRIFT: sessionDrawer (the agent-detail and
	// operator pages) renders "New chat"; chiefThreadRows (the chief slide-out)
	// renders "New thread". Each matches the noun its own surface uses everywhere else
	// — the page's control is "New chat", the panel's is "New thread" and its list is
	// headed "Threads" — so making them agree would leave one of the two surfaces
	// naming a thing its own buttons do not. This comment is the only written answer
	// to "what does an untitled session show", so it names both; the pair is pinned
	// two-way by ui.TestAnUntitledSessionsPlaceholderIsPerSurfaceAndLedgered.
	//
	// 🔴 THAT GUARD DERIVES THE RENDERER SET FROM THE PACKAGE'S OWN AST rather than
	// enumerating the two it knows about — but its reach is ONE SYNTACTIC SHAPE, and
	// this comment is where that limit has to be stated because this is where the
	// promise is made. What it matches is exactly the shape both renderers are written
	// in: `title := x.Title`, then `if title == "" { title = "<word>" }`, inside a
	// FuncDecl of package `ui`. MEASURED to pass it, i.e. NOT caught: a fallback keyed
	// on the field directly (`if s.Title == "" { return "…" }`), one assigning a named
	// constant instead of a literal, and one testing `len(title) == 0`. So a third
	// renderer copying either of these two keeps the sentence honest; a third renderer
	// written differently does not, and nothing here would say so.
	//
	// ⚠ THE EARLIER CLAIM WAS WIDER THAN THE CODE, TWICE OVER. Before round 2 this
	// comment credited the guard with catching a third renderer while the test
	// hard-coded two surfaces and two literals; round 2 derived the set but the comment
	// then said the derivation "is what makes the promise true rather than merely
	// intended", which the measurement above does not support. Widening the scan to
	// every possible fallback shape is a parser nobody should own — the limit is worth
	// naming, not closing.
	Title      string
	Active     bool
	LastActive time.Time // session's last-active time (UpdatedAt), rendered relative
	// Snippet is a short excerpt around a SEARCH match in this thread's message
	// bodies. Empty on the plain list, and empty for a result matched by TITLE
	// alone — the title is already on screen, so repeating it as a snippet would
	// claim a body match that does not exist.
	Snippet string
}

// AgentDetailPage renders the full agent detail document: live logs (SSE) and a
// chat box (WebSocket) to inject messages.
func AgentDetailPage(v AgentDetailView, feat Features) g.Node {
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				// Stripped too: it is the same string on the same document, and a tab
				// title reading `**fix** the `+"`chip`"+` row · muster` is the same defect in
				// a third place.
				TitleEl(g.Text(markdownPlain(displayOr(v.DisplayName, v.Name))+" · muster")),
				Link(Rel("stylesheet"), Href("/static/app.css")),
				Script(Src("/static/vendor/htmx.min.js"), Defer()),
				Script(Src("/static/vendor/sse.js"), Defer()),
				faroHead(),
			),
			Body(
				// h-dvh (a DEFINITE height, not min-h-dvh) + overflow-hidden makes the
				// page a fixed-height app shell so #chat-log is the ACTUAL scroll
				// container (via the flex-1 + min-h-0 chain). With min-h-dvh the body
				// had no definite height, so the flex children grew with content and the
				// WINDOW scrolled instead — making logEl.scrollTop a no-op (autoscroll
				// silently did nothing). The input bar is an IN-FLOW flex-none child at
				// the bottom of the chat pane (agentChatPane) — it is not position:fixed.
				Class("flex h-dvh flex-col overflow-hidden bg-bg text-fg antialiased"),
				// SPA navigation: boost the sidebar tabs + any internal links into AJAX
				// body-swaps. The chat WebSocket is made boost-safe (torn down on the
				// outgoing full-body swap, re-connected via htmx:load on arrival) in
				// agentChatScript so navigating here via boost still connects.
				hx("hx-boost", "true"),
				// The shell's slide-out sidebar (position:fixed overlay; it doesn't
				// affect document flow). Its hamburger lives in agentDetailHeader and
				// its tabs are boosted <a>s that navigate to the shell, so the detail
				// page gets the same chrome as the home shell.
				//
				// currentTab = "agents": /agents/<name> is a child of the Agents
				// section AND appScript's tabFromPath('/agents/x') resolves to
				// 'agents', so the server's first paint and the client's show()
				// agree on which link is current. Marking the section index for a
				// detail view is standard practice.
				sidebar("agents", "agents", false, feat),
				// Content column: offset right of the persistent desktop sidebar (lg+),
				// mirroring Page's content column. The header and the main column each
				// carry lg:pl-72 so nothing sits under the sidebar.
				agentDetailHeader(v),
				// The detail page IS the chat: a single full-height chat pane between the
				// header and the input bar at its bottom (flex-1 + min-h-0 so it fills + scrolls).
				// 🔴 TWO ELEMENTS, NOT ONE, AND THAT IS THE FIX RATHER THAN A TIDY-UP.
				// This used to be a single <main class="mx-auto w-full max-w-xl … lg:pl-72">:
				// one box 576px wide with 288px of LEFT PADDING inside it. Measured on a
				// 2256px screen, #chat-log came out 272px wide — narrower than a phone —
				// with ~800px of dead space on either side, because max-w-xl caps the
				// BORDER box and lg:pl-72 is subtracted from the same 576px.
				//
				// The shell (components.go Page) already had the right shape and this
				// page was the odd one out: the sidebar offset goes on an OUTER column,
				// and the width ladder — contentWidth(), the app's ONE horizontal
				// measure — goes on the element inside it. Using the helper rather than
				// re-spelling a ladder here is what stops this page drifting away from
				// the shell a second time.
				//
				// The flex chain is preserved through BOTH elements (flex-1 + min-h-0 on
				// each): body is `h-dvh flex flex-col overflow-hidden`, and #chat-log is
				// the real scroll container only while every ancestor between them is a
				// definite-height flex item. A wrapper without flex-1/min-h-0 would grow
				// with content and silently hand the scroll back to the window, which is
				// the failure the Body comment above describes.
				Div(
					Class("flex flex-1 flex-col min-h-0 lg:pl-72"),
					Main(
						// pb-3 (not pb-28): the input bar is an IN-FLOW flex child at the
						// bottom of the chat pane, so #chat-log fills right down to it — no
						// reserved-space gap between the transcript and the input.
						Class(contentWidth()+" flex flex-1 flex-col gap-4 pb-3 pt-4 min-h-0"),
						agentChatPane(v),
					),
				),
				// Empty mount for the task-detail modal (populated by the header title's
				// hx-get /ui/agents/{name}/task). position:fixed inside overflow-hidden
				// body is fine — fixed escapes overflow clipping (no ancestor transform).
				Div(ID("task-modal")),
				agentChatScript(v.Name),
				// appScript wires the sidebar open/close + the SPA tab router AND the
				// model combobox (its combobox copy supports the hidden-mode autosave
				// used by the header model form). It is now the SINGLE combobox
				// implementation — the old standalone comboboxScript was deleted,
				// retiring the two-copies-of-the-combobox-JS debt.
				appScript(feat),
				// The worker, update toast and app badge — on every document, not
				// just the shell, so an update is offered wherever the operator is.
				pwaChrome(),
			),
		),
	)
}

func agentDetailHeader(v AgentDetailView) g.Node {
	return Header(
		// lg:pl-72 clears the persistent desktop sidebar (mirrors Page's content
		// column); on mobile the sidebar is a slide-out overlay so no offset.
		Class("sticky top-0 z-20 border-b border-line bg-bg/80 backdrop-blur lg:pl-72"),
		Div(
			// contentWidth() — the SAME helper the <main> below it uses, so the
			// header's left edge and the chat column's left edge cannot drift apart.
			// This used to be its own `max-w-xl` spelling, which is exactly how the
			// two ended up disagreeing with the shell.
			Class(contentWidth()+" flex flex-col gap-1.5 py-2"),
			Div(
				Class("flex items-center gap-3"),
				// Hamburger: opens the shared slide-out sidebar (appScript's initPage
				// binds #sidebar-open). Replaces the old back-arrow — the sidebar's
				// boosted tabs are how you leave this page now. Same markup/classes as
				// the shell header()'s hamburger.
				sidebarOpenButton(),
				// Compact title: the DisplayName carries the whole "#<n> · <title>", so a
				// smaller weight/size keeps the header tight without truncating it. The
				// title is rendered as INLINE markdown (the task title often carries
				// **bold**/`code`), escape-first so it can't inject markup. When the agent
				// is task-linked, the title is a button that opens the full task detail in
				// a modal (hx-loads /ui/agents/{name}/task into #task-modal).
				agentTitle(v),
				newChatButton(v.ID),
				chatHistoryButton(sessionDrawerID),
			),
			g.If(agents.ResolveKind(v.Kind) != agents.KindClaudeCode, agentModelControl(v)),
			// 🔴 THE CHAT IS THE RENDERED VIEW, AND IT IS THE DEFAULT: an agent's
			// card opens this page. A claude-code agent also has a Terminal — the
			// same Rendered | Terminal switch a mirrored Claude Code window has
			// (viewToggle), on both of its pages. An agent of a kind with no
			// terminal has one view and no switch.
			g.If(agents.ResolveKind(v.Kind) == agents.KindClaudeCode,
				Div(Class("flex items-center gap-2"), chip("kind", agents.KindLabel(v.Kind)+" · "+v.CCAccount),
					Div(Class("ml-auto"), viewToggle(true, "", AgentTerminalPath(v.Name))))),
		),
	)
}

// agentTitle renders the header title. For a task-linked agent it is a clickable
// button opening the task-detail modal; otherwise a plain H1. Both render the
// title as inline markdown.
func agentTitle(v AgentDetailView) g.Node {
	// markdownPlain, the SAME treatment agentCard gives this string in the list.
	// It used to be mdInlineNode — inline markdown — which is how the list and the
	// detail page came to render one name two ways (raw `**` on the card, bold
	// here, and a backtick that survived on both). Stripping on both is the fix;
	// see markdownPlain.
	title := g.Text(markdownPlain(displayOr(v.DisplayName, v.Name)))
	if v.NoteID == nil {
		return H1(Class("mr-auto break-all text-sm font-medium"), title)
	}
	return H1(Class("mr-auto min-w-0 break-all"),
		Button(
			Type("button"),
			g.Attr("aria-label", "View task details"),
			g.Attr("title", "View task details"),
			Class("group flex min-h-[44px] w-full items-center gap-1 text-left text-sm font-medium text-fg transition hover:text-accent"),
			hx("hx-get", "/ui/agents/"+v.Name+"/task"),
			hx("hx-target", "#task-modal"),
			hx("hx-swap", "innerHTML"),
			Span(Class("min-w-0 break-all"), title),
			// A subtle chevron hints the title is tappable.
			g.Raw(`<svg class="h-3.5 w-3.5 shrink-0 text-muted transition group-hover:text-fg2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>`),
		),
	)
}

// TaskModalView is the data the task-detail modal renders (opened from the chat
// header title). It is a read-only view of the agent's linked task.
type TaskModalView struct {
	ID       int64
	Status   string
	Body     string // markdown
	Repo     string
	Model    string
	Comments []TaskModalComment
}

// TaskModalComment is one comment in the task-detail modal, oldest-first.
type TaskModalComment struct {
	Author string
	Body   string // markdown; always empty when Retracted
	When   string // preformatted timestamp label
	// Retracted renders the tombstone instead of Body. This page carries no
	// retraction CONTROL (retracting is done from the task card) and that
	// asymmetry is deliberate — but it must never render a retracted comment's
	// BODY, so the flag is carried here rather than assumed away.
	Retracted bool
}

// RenderTaskModal renders the task-detail modal fragment (overlay + panel) that
// the header title's hx-get swaps into #task-modal.
func RenderTaskModal(w io.Writer, v TaskModalView) error { return taskModal(v).Render(w) }

// taskModal is the overlay + panel: a scrollable markdown-rendered task body with
// status, repo/model meta, and the comment thread. The backdrop and an × button
// close it by clearing #task-modal (CSP allows hx-on). Slides up from the bottom
// on mobile (items-end) and centers on larger screens.
func taskModal(v TaskModalView) g.Node {
	const close = "document.getElementById('task-modal').innerHTML='';"
	meta := []g.Node{}
	if v.Repo != "" {
		meta = append(meta, taskMetaChip("repo", v.Repo))
	}
	if v.Model != "" {
		meta = append(meta, taskMetaChip("model", modelName(v.Model)))
	}
	comments := make([]g.Node, 0, len(v.Comments))
	for _, c := range v.Comments {
		comments = append(comments, Div(
			Class("rounded-xl bg-bg/50 p-3 ring-1 ring-inset ring-line"),
			g.If(c.Retracted, g.Attr("data-comment-retracted", "")),
			Div(Class("mb-1 flex items-center gap-2 text-[11px] text-muted"),
				Span(Class("font-medium text-muted"), g.Text(displayOr(c.Author, "comment"))),
				g.If(c.When != "", Span(g.Text("· "+c.When))),
			),
			// Same tombstone as the task card (commentTombstone, internal/ui/notes.go)
			// — one placeholder, not a second spelling that could drift.
			g.If(c.Retracted, commentTombstone()),
			g.If(!c.Retracted, Div(Class("text-sm text-fg [overflow-wrap:anywhere]"), renderMarkdown(c.Body))),
		))
	}
	return Div(
		ID("task-modal-overlay"),
		Class("fixed inset-0 z-50 flex items-end justify-center sm:items-center"),
		g.Attr("role", "dialog"), g.Attr("aria-modal", "true"),
		// Backdrop: click to close.
		Div(Class("absolute inset-0 bg-black/60 backdrop-blur-sm"), hx("hx-on:click", close)),
		Div(
			Class("relative z-10 flex max-h-[85dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 shadow-2xl ring-1 ring-line sm:rounded-2xl"),
			Div(
				Class("flex items-center gap-2 border-b border-line px-4 py-3"),
				H2(Class("text-sm font-semibold text-fg"), g.Text("Task #"+strconv.FormatInt(v.ID, 10))),
				taskStatusBadge(v.Status),
				Span(Class("flex-1")),
				Button(
					Type("button"),
					g.Attr("aria-label", "Close"),
					Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
					hx("hx-on:click", close),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
				),
			),
			Div(
				Class("flex-1 overflow-auto px-4 py-3"),
				g.If(len(meta) > 0, Div(Class("mb-3 flex flex-wrap gap-1.5"), g.Group(meta))),
				Div(Class("text-sm text-fg [overflow-wrap:anywhere]"), renderMarkdown(v.Body)),
				g.If(len(comments) > 0, Div(
					Class("mt-4 flex flex-col gap-2 border-t border-line pt-3"),
					Span(Class("text-[11px] font-medium uppercase tracking-wide text-muted"), g.Text("Comments")),
					g.Group(comments),
				)),
			),
		),
	)
}

func taskMetaChip(label, val string) g.Node {
	return Span(
		Class("inline-flex max-w-full items-center gap-1 truncate rounded-full bg-s2/80 px-2.5 py-1 text-[11px] text-muted ring-1 ring-inset ring-line"),
		Span(Class("text-muted"), g.Text(label)),
		g.Text(val),
	)
}

// taskStatusBadge renders a small status pill for the modal header: the task
// status's glyph and its stored word, on that status's chip colours.
func taskStatusBadge(status string) g.Node {
	label := status
	if label == "" {
		label = "open"
	}
	return statusChip(taskGlyphKind(status), label)
}

// sessionDrawerID is the checkbox-hack toggle for the PAGE surfaces' drawer.
//
// 🔴 THE PANEL USES A DIFFERENT ID (chiefThreadsToggleID) RATHER THAN SHARING
// THIS ONE. Only one of the two ever renders per document today, so sharing would
// work — and would make the first accidental overlap a duplicate-id bug that
// silently gives one <label for> two targets. Two ids cost nothing and make
// TestTheShellPlusAnOpenChiefPanelHasNoDuplicateIDs a real guard rather than a
// coincidence.
const sessionDrawerID = "session-drawer"

// agentSessionsPath is the ONE place the session-creation route is spelled.
//
// 🔴 ONE ROUTE, TWO CALLERS, AND THE SECOND ONE IS THE WHOLE POINT. The panel's
// new-thread control must reuse POST /agents/{id}/sessions — its empty-session
// reuse is behaviour the operator wants and a second creation path would have to
// re-derive it. Spelled here so a change to the route cannot move one caller and
// leave the other pointing at a 404.
func agentSessionsPath(agentID int64) string {
	return "/agents/" + strconv.FormatInt(agentID, 10) + "/sessions"
}

// chatHistoryButton is the header control that opens the session drawer. It is a
// <label for=...> driving the pure-CSS checkbox-hack toggle (sessionDrawer on the
// page surfaces, chiefThreadList in the panel) — the TOGGLE needs no JS and is
// boost-safe. ≥44px tap target.
//
// ⚠ forID IS A PARAMETER RATHER THAN THE CONSTANT IT USED TO BE, so the panel
// reuses this control instead of growing a second launcher with its own styling,
// its own tap target and its own a11y contract to keep in step.
//
// 🔴 role="button" + tabindex="0" is a WIDGET CONTRACT, and a <label> has no
// native keyboard activation, so those attributes alone would claim a contract
// this element does not honour (focusable, but Enter/Space do nothing). The
// contract is honoured by appScript's delegated `label[role="button"]` keydown
// handler, which maps Enter/Space to a click. Keep the three together: if the
// handler is ever removed, drop role/tabindex here too rather than leaving a
// focusable dead control. (The drawer stays keyboard-operable regardless via the
// #session-drawer checkbox itself, which is the JS-free path.)
func chatHistoryButton(forID string) g.Node {
	return Label(
		g.Attr("for", forID),
		g.Attr("aria-label", "Chat history"),
		g.Attr("role", "button"),
		g.Attr("tabindex", "0"),
		Class("press inline-flex h-11 w-11 min-h-[44px] cursor-pointer items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 3v5h5"/><path d="M3.05 13A9 9 0 1 0 6 5.3L3 8"/><path d="M12 7v5l3 2"/></svg>`),
	)
}

// newChatButton is the header control that creates a new chat session. It is a
// tiny form POSTing to /agents/{id}/sessions; the handler returns HX-Redirect to
// the new (or reused-empty) session, which htmx follows. The form uses
// class="contents" so it doesn't introduce a box into the header's flex row —
// the ≥44px Button is the visible/tappable control. Styled to match
// chatHistoryButton. Shared by the agent-detail and operator headers.
func newChatButton(agentID int64) g.Node {
	return Form(
		hx("hx-post", agentSessionsPath(agentID)),
		hx("hx-swap", "none"),
		Class("contents"),
		Button(
			Type("submit"),
			g.Attr("aria-label", "New chat session"),
			g.Attr("title", "New chat"),
			Class("press inline-flex h-11 w-11 min-h-[44px] cursor-pointer items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>`),
		),
	)
}

// liveStatusIcon is the header status indicator: a bare colored dot (no text
// label) that self-refreshes. It renders statusIcon(status) for an instant first
// paint, then polls GET /ui/agents/{id}/status on load + every 10s and swaps the
// fresh icon into itself — so the header reflects the agent's live pod state
// without a label. htmx only (boost-safe). Shared by the agent-detail and
// operator headers.
func liveStatusIcon(agentID int64, status string) g.Node {
	return Span(
		ID("agent-status"),
		hx("hx-get", "/ui/agents/"+strconv.FormatInt(agentID, 10)+"/status"),
		hx("hx-trigger", "load, every 10s"),
		hx("hx-target", "this"),
		hx("hx-swap", "innerHTML"),
		statusIcon(status),
	)
}

// agentModelControl renders the agent's model directly AS the searchable model
// combobox (no separate badge chip, no "change" expander): the combobox shows the
// current model as its trimmed display value and hx-posts the chosen model to
// POST /agents/{id}/model with form field `model` (empty = cluster default). A
// repo chip sits alongside when the agent is repo-backed. A successful post
// returns 200 + HX-Trigger: agents:changed; the list/detail refresh via the
// existing agents:changed/SSE wiring. Changing the model restarts the agent, so a
// clear hint says so and a brief "restarting…" state is shown after the request.
func agentModelControl(v AgentDetailView) g.Node {
	ids := strconv.FormatInt(v.ID, 10)
	return Div(
		// Single compact row: the model selector flexes to fill and the repo chip
		// sits alongside it (shrink-0, truncates) — keeps the header tight instead
		// of spending a second row on the repo.
		Class("flex items-center gap-2 pl-11 text-xs"),
		Form(
			hx("hx-post", "/agents/"+ids+"/model"),
			// Autosave: picking a model dispatches `change` on the hidden #model
			// input (cbPick); scope the trigger to that input so typing+blur in the
			// filter box never submits. Server returns 200 + HX-Trigger:
			// agents:changed (no body to swap; the trigger drives refreshes).
			hx("hx-trigger", "change from:[data-combobox-hidden]"),
			hx("hx-swap", "none"),
			hx("hx-disabled-elt", "find [data-combobox-input]"),
			hx("hx-on::after-request", "if(event.detail.successful){var s=this.querySelector('[data-model-saving]');if(s)s.classList.remove('hidden');try{window.cgTrack('model.switch',{agent:this.getAttribute('data-agent')||'',model:(this.querySelector('[data-combobox-hidden]')||{}).value||''});}catch(e){}}"),
			g.Attr("data-agent", v.Name),
			Class("min-w-0 flex-1"),
			// The searchable select IS the model display; the "restarts the agent"
			// warning lives inside its dropdown. No Save button — selecting a model
			// autosaves (and restarts the agent).
			// Agent-view display shows just the model NAME (last '/'-segment) — e.g.
			// "deepseek-v4-pro" — while the hidden input still submits the FULL slug
			// so model switching is unaffected. Empty model ("") → the default label.
			modelFieldWithDisplay(v.Model, "Selecting a model restarts the agent", modelName(v.Model)),
			Span(g.Attr("data-model-saving", ""), Class("mt-1 hidden text-[11px] text-st-warning-fg"), g.Text("↳ restarting…")),
		),
		// Surface the agent's repo alongside the model, value only (no "repo" label —
		// the slug speaks for itself). Operator has no repo → omitted. shrink-0 so
		// the model selector yields width first; truncates if the slug is long.
		g.If(v.Repo != "", Span(
			Class("inline-flex max-w-[45%] shrink-0 items-center truncate rounded-full bg-s2/80 px-2.5 py-1 text-[11px] text-muted ring-1 ring-inset ring-line"),
			g.Text(v.Repo),
		)),
	)
}

// ChatSurfaceQueryParam is the query-string name the chat-log partial reads its
// surface from. Exported because internal/api parses it and a second spelling
// there would silently answer with the page surface for ever.
const ChatSurfaceQueryParam = "surface"

// chatLogSurfaceQuery renders the surface as a query fragment, or nothing for the
// page surface — so every existing chat-log URL is byte-identical to what it was.
func chatLogSurfaceQuery(s ChatSurface) string {
	if s == ChatSurfacePage {
		return ""
	}
	return "&" + ChatSurfaceQueryParam + "=" + string(s)
}

func agentChatPane(v AgentDetailView) g.Node {
	return Section(
		// hx-ext="sse" + sse-connect establishes the /events EventSource on the
		// detail page (the home shell connects on its own wrapper; the standalone
		// chat doc had none), so the sse:chat.reply trigger below actually fires.
		// Events dispatched here bubble to <body>, where the #chat-log listener
		// (from:body) catches them.
		hx("hx-ext", "sse"),
		hx("sse-connect", "/events"),
		Class("flex flex-1 flex-col gap-2 min-h-0"),
		// 🔴 THE DRAWER IS OMITTED ON THE PANEL SURFACE AND THAT IS NOT A TIDY-UP.
		// sessionDrawer is `fixed inset-y-0 right-0`, and ChiefPanel's
		// `translate-x-full` makes the panel the containing block for every fixed
		// descendant — so inside the panel the drawer renders at the PANEL's right
		// edge and, when the panel is shut, is translated off-screen and inert with
		// it. The panel renders chiefThreadList instead: an absolutely-positioned
		// sub-view of the panel's OWN column, which needs no viewport-relative
		// positioning at all. See ChatSurface.
		g.If(v.Surface != ChatSurfaceChiefPanel, sessionDrawer(v)),
		Div(
			ID("chat-log"),
			Class("flex flex-1 flex-col gap-2 overflow-auto rounded-xl bg-s1/40 p-3 ring-1 ring-inset ring-line min-h-0"),
			// Live-refresh the transcript from the canonical server state when a chat
			// reply is persisted (chat.reply). This is what makes a KICKOFF turn — which
			// streams server-side, NOT over this page's WS — appear live without a
			// reload. chat.reply fires only AFTER a turn completes+persists, so swapping
			// to the server transcript is safe; agentChatScript additionally cancels
			// this refresh (htmx:beforeRequest) while a WS turn is mid-stream so it can
			// never clobber an in-flight streamed turn.
			// task:changed is dispatched on <body> by the completion banner's
			// Mark-complete button so the log (and its banner) re-fetch and reflect
			// the task's new status live; sse:chat.reply covers the agent-finished
			// reveal (a just-finished task surfaces its banner without a reload).
			// 🔴 THE SURFACE TRAVELS WITH THE REFETCH URL. This partial re-renders
			// #chat-log's children, and one of those children is the chief intro —
			// which must be absent the moment the thread HAS content. A refetch that
			// dropped the surface would render the intro on a surface that never shows
			// it (harmless) or, worse, keep rendering it in the panel after the first
			// message because the server no longer knew where it was answering.
			hx("hx-get", "/ui/agents/"+v.Name+"/chat-log?session="+strconv.FormatInt(v.ActiveSessionID, 10)+chatLogSurfaceQuery(v.Surface)),
			hx("hx-trigger", chatLogTrigger(v.Name)),
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			chatLogInner(v),
		),
		Form(
			ID("chat-form"),
			// 🔴 hx-boost="false" IS LOAD-BEARING AND IT IS A FIXED DEFECT, NOT A
			// PRECAUTION. This form carries no action, no method and no hx verb: its
			// submit is intercepted by agentChatScript and sent over the WebSocket. But
			// both hosting documents set hx-boost="true" on <body>, and htmx BOOSTS any
			// descendant form that has no verb of its own — inheritance, so nothing here
			// had to opt in. A boosted form with no action submits a GET of the CURRENT
			// URL, and the app's own `e.preventDefault()` does NOT stop it: htmx's submit
			// listener issues the request regardless of defaultPrevented.
			//
			// 🔴 AND THE RESPONSE LANDS WHEREVER THE NEAREST hx-target SAYS. On the
			// agent-detail and operator PAGES no ancestor declares one, so a boosted send
			// re-swapped the whole body with the same page — invisible, and the reason
			// this survived for as long as it did. Inside the chief slide-out,
			// #chief-panel-body declares hx-target="this" + hx-swap="innerHTML", and htmx
			// resolves an INHERITED "this" to the element that declared it — so pressing
			// Send fetched /tmux and swapped the ENTIRE PAGE into the panel body,
			// destroying #chat-form and #chat-input outright. The operator's message was
			// on the WebSocket, and the box they typed it in was gone.
			//
			// MEASURED in Chromium via a probe on the real click path, pinned to a
			// COMMIT rather than a day because that is the reproducible form:
			// htmx:configRequest on #chat-form with path=<the page url>, then
			// beforeSwap/afterSwap targeting #chief-panel-body, then no #chat-input in the
			// document — 4 of 4 runs at 6a7904957 and 2 of 2 at dcc080998, i.e. it predates
			// the commit CI first caught it on. The nearest hx-target ancestor was read
			// back as `chief-panel-body`, which is the half a reader would not guess.
			//
			// ⚠ IT IS ON THE FORM RATHER THAN ON THE PANEL BODY BECAUSE THE FORM IS WHAT
			// IS TRUE EVERYWHERE. One attribute fixes both surfaces and says the actual
			// invariant: this form is not an htmx request source. A blanket
			// hx-boost="false" on the panel body would also stop boosting <a> links inside
			// the panel, which is a different decision — note that any such link is still
			// exposed to the same inherited-target hazard, and would swap its destination
			// page into the panel.
			g.Attr("hx-boost", "false"),
			// 🔴 A REAL FORM UNDER THE SOCKET. The message normally travels over this
			// page's WebSocket (agentChatScript), which streams the reply back. But a
			// form with no action and no method can only be sent by that script: when
			// it had not run, or its socket was not open, pressing Send did NOTHING —
			// no message, no error. With method + action the browser can send it
			// itself (POST /agents/{name}/send, which runs the same turn and answers
			// with a redirect back here), and agentChatScript hands it over whenever
			// the socket cannot take it. The nonce makes a message that went both ways
			// one message (ComposerView.Nonce).
			Method("post"),
			Action(AgentSendPath(v.Name)+"?session="+strconv.FormatInt(v.ActiveSessionID, 10)),
			Input(Type("hidden"), Name("session"), Value(strconv.FormatInt(v.ActiveSessionID, 10))),
			g.If(v.SendNonce != "", Input(ID("chat-nonce"), Type("hidden"), Name("nonce"), Value(v.SendNonce))),
			// The chat WS target agent, read by agentChatScript so its boost-safe
			// init derives the agent from the LIVE form (never a stale closure from
			// a previously-visited page).
			g.Attr("data-agent", v.Name),
			// The active chat session id — appended to the WS URL so the socket talks
			// to the active session's gateway context. Switching sessions re-renders
			// the page (boosted nav) with a new data-session, so the boost-safe WS
			// re-inits onto the new session.
			g.Attr("data-session", strconv.FormatInt(v.ActiveSessionID, 10)),
			// In-flow flex-none input bar at the BOTTOM of the chat pane (was fixed
			// bottom-0 + a pb-28 spacer, which left a dead gap). Now #chat-log (flex-1)
			// fills down to it with no gap; h-dvh + the dynamic viewport handle the
			// mobile keyboard (the shell shrinks, keeping the input visible). It sits
			// inside Main (already contentWidth() inside the lg:pl-72 column), so no
			// width/offset classes here.
			Class("flex-none border-t border-line pt-3"),
			// The outcome of a send the browser submitted (see above), drawn by the
			// server on the page its redirect lands on.
			g.If(v.SendNotice != "", P(ID("chat-note"), g.Attr("role", "status"),
				g.If(v.SendNoticeOK, g.Attr("data-ok", "")),
				Class("pb-2 text-xs break-words "+chatNoteTone(v.SendNoticeOK)), g.Text(v.SendNotice))),
			dictateNote(v.Dictate, "chat-dictate-note", "pb-2"),
			Div(
				Class("flex w-full items-end gap-2"),
				// The SAME look as every other message box (composerInputClass,
				// composerSendClass): a 16px input — iOS zooms the page on focusing a
				// smaller one, and this one was 14px — and 44px targets.
				Textarea(
					ID("chat-input"),
					Name("text"),
					Rows("1"),
					g.Attr("enterkeyhint", "send"),
					Placeholder("Message the agent…"),
					Class("max-h-32 "+composerInputClass),
				),
				dictateControl(v.Dictate, "chat-input", "chat-dictate-note", "chat-dictate"),
				Button(
					Type("submit"),
					ID("chat-send"),
					Class(composerSendClass),
					g.Text("Send"),
				),
			),
		),
		// The microphone's script travels WITH the pane, so the chief panel —
		// which renders this pane into another page — gets it too. It is built
		// to run again on every swap (dictate.go).
		dictateScript(v.Dictate),
	)
}

// chatLogTrigger is #chat-log's hx-trigger: the events on which the transcript
// re-fetches its partial from the server.
//
//   - sse:chat.reply — a turn was persisted (the kickoff turn included).
//   - task:changed — the completion banner's Mark-complete button.
//   - sse:agent.changed for THIS agent — its status moved.
//
// 🔴 THE agent.changed ARM IS WHAT CLEARS "Provisioning agent…" WHEN NO MESSAGE
// EVER ARRIVES. The provisioning row and the empty state are both decided
// server-side from the status (chatIsProvisioning), and before this arm the
// only things that re-rendered them were a persisted message (chat.reply) or a
// streamed delta (agentChatScript's hideProvisioning). An agent brought up with
// no kickoff note — a Start of a saved agent, a claude-code agent — reaches
// `running` with neither, so a page opened while it was provisioning showed the
// row for ever and never showed "No messages yet". internal/agentstatus's
// promoter broadcasts agent.changed on exactly that provisioning → running move
// (api.Server.BroadcastAgentChanged), so re-fetching on it re-renders both from
// the one predicate rather than a second copy of it in JS.
//
// ⚠ THE FILTER KEEPS IT TO THIS AGENT. agent.changed fires for every agent in the
// fleet, with Data {"id":"<name>"} (api.Server.broadcast); unfiltered, every
// other agent's churn would re-fetch this transcript. A refetch while a WS turn
// is mid-stream is still cancelled by agentChatScript's htmx:beforeRequest guard,
// which keys on the element, not the event.
//
// ⚠ IT IS ONLY AS LIVE AS THE BROADCASTS. A restart of an agent that was already
// kicked off is NOT covered: agentprovision.Adapter.Start stores `running` for it
// at once, the promoter (which only promotes stored `provisioning`) never fires,
// and nothing broadcasts when its instance later becomes ready — so a page opened
// in that window still holds the row until a message or a reload.
// chatNoteTone is the colour of the chat form's notice: plain for good news.
func chatNoteTone(ok bool) string {
	if ok {
		return "text-fg2"
	}
	return "text-st-error-fg"
}

func chatLogTrigger(agentName string) string {
	quoted, _ := json.Marshal(agentName) // a string always marshals
	return "sse:chat.reply from:body, task:changed from:body, " +
		"sse:agent.changed[JSON.parse(detail.data).id===" + string(quoted) + "] from:body"
}

// sessionDrawer renders the previous-sessions UI as a slide-out RIGHT-side
// drawer, toggled by the header's chatHistoryButton (a <label for="session-drawer">).
// It uses the pure-CSS checkbox-hack, so opening/closing needs no JS at all and
// is boost-safe. (Both hosting documents — AgentDetailPage and OperatorPage — do
// in fact include appScript; the Enter/Space activation of the label-buttons
// below rides on it. But the CSS toggle and the checkbox's own keyboard
// operability do not, so the drawer degrades correctly without it.)
//
//   - a visually-hidden peer checkbox #session-drawer (class="peer sr-only") —
//     which is ALSO the drawer's JS-free keyboard control: sr-only is clip/1px,
//     not display:none, so it stays natively focusable and Space toggles it,
//   - the backdrop and the panel are PEER-SIBLINGS after the checkbox, so
//     peer-checked:* selectors drive them,
//   - the panel slides in via translate-x-full → peer-checked:translate-x-0,
//   - the backdrop is hidden → peer-checked:block and is itself a <label> for the
//     same checkbox, so tapping it closes the drawer.
//
// The panel lists the agent's sessions (most-recently-active first). Each session
// is a BOOSTED SPA link to <base>?session=<id>, so picking one body-swaps the
// page with that session's transcript and the WS reconnects fresh via
// agentChatScript's boost-safe init. The active session is highlighted. Shared by
// the agent-detail and operator panes.
//
// ⚠ IT USED TO CARRY AN OPERATOR SPECIAL CASE, sending the reserved operator's
// sessions to /operator rather than /agents/{name}. Task #633 deleted that page,
// so every agent's drawer links into the agent-detail route.
func sessionDrawer(v AgentDetailView) g.Node {
	base := "/agents/" + v.Name

	links := make([]g.Node, 0, len(v.Sessions))
	for _, sess := range v.Sessions {
		title := sess.Title
		if title == "" {
			title = "New chat"
		}
		cls := "press flex min-h-[44px] items-center rounded-xl px-3 py-2 text-sm transition ring-1 ring-inset "
		if sess.Active {
			cls += "bg-accent/15 text-fg ring-accent/50"
		} else {
			cls += "bg-s1 text-fg2 ring-line hover:bg-s2"
		}
		links = append(links, A(
			Href(base+"?session="+strconv.FormatInt(sess.ID, 10)),
			// Boosted SPA nav: picking a session body-swaps the page (transcript +
			// fresh data-session); agentChatScript's boost-safe init reconnects the WS
			// to the picked session. The link is NOT inside #agents-list, so boosting
			// is a safe top-level body swap.
			Class(cls+" gap-2"),
			Span(Class("mr-auto truncate"), g.Text(title)),
			g.If(!sess.LastActive.IsZero(),
				Span(Class("shrink-0 text-xs text-muted"), g.Text(relTimeString(sess.LastActive)+" ago")),
			),
		))
	}

	return Div(
		// The checkbox + backdrop + panel live under this common parent so the
		// peer-checked:* utilities on the siblings select off the checkbox.
		//
		// 🔴 The checkbox is NOT aria-hidden and NOT tabindex="-1", and that is
		// load-bearing. sr-only is clip/1px, not display:none, so the checkbox is
		// natively focusable; hiding it from the a11y tree while leaving it
		// focusable is precisely what axe's aria-hidden-focus forbids. An earlier
		// revision closed that rule by ALSO stripping it from the tab order — and
		// that made the whole drawer mouse/touch-only, because a <label for> has
		// NO native keyboard activation and nothing implemented Enter/Space for
		// the labels. Do NOT re-add either attribute.
		//
		// The accessible form of the checkbox hack is the opposite: leave it in
		// the tab order and give it a real name (aria-label is permitted on an
		// <input>, unlike on a bare <label>). A keyboard user then Tabs to "Chat
		// history" and presses Space to open AND close the drawer, with no JS at
		// all. Focus is made VISIBLE by the #session-drawer:focus-visible rule in
		// web/css/input.css — an outline on a 1px-clipped element would not be.
		Input(Type("checkbox"), ID(sessionDrawerID), Class("peer sr-only"),
			g.Attr("aria-label", "Chat history")),
		// Backdrop: hidden until checked; itself a label that unchecks on tap.
		// NO aria-label: a bare <label> has no widget role, so aria-label is
		// PROHIBITED on it (axe aria-prohibited-attr) and is simply dropped by the
		// a11y tree — the name has to come from the label's own content, hence the
		// visually-hidden span.
		Label(
			g.Attr("for", sessionDrawerID),
			Class("fixed inset-0 z-30 hidden bg-black/60 peer-checked:block"),
			Span(Class("sr-only"), g.Text("Close chat history")),
		),
		// Panel: off-screen right, slides in when checked.
		Div(
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Chat history"),
			Class("fixed inset-y-0 right-0 z-40 flex w-80 max-w-[85vw] translate-x-full flex-col bg-s1 shadow-2xl shadow-black/50 ring-1 ring-line transition-transform duration-300 ease-out peer-checked:translate-x-0"),
			Div(
				Class("flex items-center gap-2 border-b border-line px-4 py-3"),
				H2(Class("mr-auto text-sm font-semibold text-fg"), g.Text("Chat history")),
				// Same rule as the backdrop label above: aria-label is prohibited on
				// a role-less <label>, so the accessible name is visually-hidden
				// content instead.
				//
				// role="button" + tabindex="0" IS claimed here — and honoured, by
				// appScript's delegated label[role="button"] Enter/Space handler
				// (see chatHistoryButton). Without it an open drawer could only be
				// closed with a pointer: the backdrop below is deliberately left
				// pointer-only (a full-screen tab stop is worse than none), so this
				// is the in-drawer keyboard close. Space on the #session-drawer
				// checkbox remains the JS-free fallback.
				Label(
					g.Attr("for", sessionDrawerID),
					g.Attr("role", "button"),
					g.Attr("tabindex", "0"),
					Class("press inline-flex h-11 w-11 cursor-pointer items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
					g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
					Span(Class("sr-only"), g.Text("Close chat history")),
				),
			),
			Div(
				Class("flex flex-1 flex-col gap-2 overflow-y-auto p-4"),
				// New chat is the header's ＋ button now (newChatButton); the drawer
				// just lists existing sessions.
				g.Group(links),
			),
		),
	)
}

// chatLogInner renders the INNER content of #chat-log: the persisted transcript
// bubbles, the provisioning indicator (while the agent is still coming up), and
// the hidden working indicator. It is shared by the full-page render
// (agentChatPane) and the GET /ui/agents/{name}/chat-log partial that the
// sse:chat.reply refresh swaps in — so a live refresh reproduces the exact same
// children (and re-includes #chat-working so the WS client re-finds it).
// isSentinelReply reports whether an assistant text is a gateway "no textual
// reply" marker (or empty/whitespace) that should NOT render as a chat bubble.
// The agent gateway emits a sentinel (observed "NO_REPL", i.e. a
// truncated-looking "NO_REPLY") when a turn completes via tool calls with no
// closing narration — showing it as a bubble is pure noise.
func isSentinelReply(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	switch strings.ToUpper(t) {
	case "NO_REPLY", "NO_REPL", "NO REPLY", "(NO REPLY)":
		return true
	}
	return false
}

func chatLogInner(v AgentDetailView) g.Node {
	msgs := make([]g.Node, 0, len(v.Messages)+2)
	paired := map[int]bool{} // tool_result rows already drawn with their call
	for i := 0; i < len(v.Messages); i++ {
		m := v.Messages[i]
		switch m.Kind {
		case "tool_call":
			// Pair with its tool_result (same ToolID) so the call + result render
			// as ONE collapsed chip, like the live stream. The result is looked for
			// AFTER the call, not only immediately after it: Claude Code issues
			// parallel calls before any result (call A, call B, result A, result B),
			// and a strictly-adjacent rule drew those as four chips.
			var result *ChatLine
			for j := i + 1; j < len(v.Messages) && m.ToolID != ""; j++ {
				if r := v.Messages[j]; r.Kind == "tool_result" && r.ToolID == m.ToolID && !paired[j] {
					paired[j] = true
					result = &r
					break
				}
			}
			msgs = append(msgs, chatToolChip(m, result))
		case "tool_result":
			if paired[i] {
				continue // drawn with its call
			}
			// Defensive: an unpaired result still renders as its own chip.
			r := m
			msgs = append(msgs, chatToolChip(ChatLine{Kind: "tool_call", ToolID: m.ToolID, ToolName: m.ToolName}, &r))
		default:
			// Skip an assistant "no reply" sentinel (or an empty bubble): the agent
			// gateway emits a marker (e.g. NO_REPLY) when the turn finished via tool
			// calls with no closing narration — rendering it as a bubble is just noise.
			if m.Role == "assistant" && isSentinelReply(m.Content) {
				continue
			}
			msgs = append(msgs, chatBubble(m.Role, m.Content))
		}
	}
	return g.Group{
		// 🔴 THE CHIEF INTRO IS INSIDE #chat-log AND NOT IN chiefPanelBody, AND THAT
		// PLACEMENT IS WHAT MAKES IT DISAPPEAR. The panel body is fetched on OPEN
		// only; #chat-log re-fetches this very partial on every `sse:chat.reply`. An
		// intro rendered one level up would therefore sit above a growing conversation
		// for the whole session — precisely the "no long conversation carries it"
		// property it exists to have. Here the first persisted turn removes it, from
		// the server's own count, with no client-side hiding rule to keep in step.
		//
		// ⚠ THE CONDITION COUNTS ROWS, NOT RENDERED BUBBLES, AND THE GAP IS UNREACHABLE
		// TODAY RATHER THAN CLOSED. The loop above DROPS an assistant sentinel
		// (isSentinelReply: NO_REPLY/NO_REPL/empty), so a thread holding rows that are
		// nothing but sentinels would render zero bubbles AND no intro: a blank panel with
		// no affordance at all. It cannot happen as the code stands — all four writers of
		// a transcript persist a `user` message before any assistant one, so a thread with
		// any rows has at least one user row, and user content is never dropped here.
		// The four, BY SYMBOL rather than by line number — this citation used to name
		// line numbers, and they went stale TWICE inside this one branch: once at the very
		// commit that wrote them, and again at the commit that recorded that fact while
		// quoting the replacements. A footnote arguing against line numbers is the last
		// place to carry any, so there are none:
		//
		//   agents.Provisioner.kickoffWhenReady    (internal/agents/provision.go)
		//   api.Server.handleAPIAgentSendMessage   (internal/api/machine_agents.go)
		//   api.Server.handleAgentWS               (internal/api/agents.go)
		//
		// ⚠ THERE WERE FOUR UNTIL muster task #653 phase two, and THREE IS A
		// RE-DERIVED COUNT, not the old list minus its deletions. Two writers went with
		// internal/api/operator.go — `api.Server.handleOpAgentMessage` and
		// `api.Server.dispatchOperatorTool` (its agent_message arm). The third name here
		// is in the set because re-running the grep found it, NOT because it replaced
		// them; the old list had simply missed it. The argument is unchanged: every one
		// of the three persists a `user` message before any assistant one.
		//
		// ⚠ NOTHING MACHINE-CHECKS THAT SET. `git grep -n AddChatMessage` over those two
		// packages is how it was derived and is how to re-derive it; a fourth writer will
		// not announce itself here.
		//
		// 🔴 DO NOT BUILD FOR IT — BUT A WRITER THAT PERSISTS AN ASSISTANT TURN FIRST
		// BREAKS THE ARGUMENT, NOT MERELY THE COUNT. If you add one, the fix is to
		// condition on what this function will actually RENDER rather than on
		// len(v.Messages), and to say so here: the failure is silent and reads as a
		// broken panel rather than as a bug in the new writer.
		g.If(v.Surface == ChatSurfaceChiefPanel && len(v.Messages) == 0, chiefIntro()),
		// Empty state for the PAGE surface. #chat-log is a flex-1 box filling the
		// whole viewport height, so a thread with nothing in it rendered as a tall
		// blank panel — the only thing inside it was the `hidden` working indicator,
		// which says nothing to a reader.
		//
		// 🔴 IT COUNTS RENDERED BUBBLES, NOT ROWS, which is what the chief-intro
		// branch above says to do when a second condition is added here: the loop
		// DROPS assistant sentinels, so a `len(v.Messages) == 0` test would leave a
		// sentinel-only thread with no affordance at all — the very gap that branch
		// records as unreachable-but-open. len(msgs) cannot disagree with the screen.
		//
		// The chief panel keeps chiefIntro() (its own, richer copy) and the
		// provisioning row below owns the still-coming-up case, so neither is
		// doubled up.
		g.If(v.Surface != ChatSurfaceChiefPanel && len(msgs) == 0 && !chatIsProvisioning(v.Status),
			chatEmptyState()),
		g.Group(msgs),
		// Completion banner (Task 6): rendered inside #chat-log so the existing
		// sse:chat.reply / task:changed re-fetch of this partial re-evaluates the
		// task status server-side — a just-finished task reveals its banner live.
		agentTaskBanner(v),
		// Provisioning indicator: shown on first paint (and after a refresh) while the
		// agent is still provisioning/pending, so a chat opened DURING provisioning
		// isn't an empty void. It self-clears: when the agent's status moves (the
		// agent.changed arm of chatLogTrigger) or the kickoff persists its first
		// message (chat.reply), this partial re-renders with status=running, so this
		// node is simply absent and the empty state takes its place when there is
		// nothing to show (agentChatScript also hides it on the first streamed
		// delta as a belt-and-suspenders).
		chatProvisioningIndicator(v.Status),
		// Working indicator: an animated "working…" row shown between send and the
		// first server message, hidden again on done/error. The script toggles its
		// `hidden` class and keeps it as the last child of #chat-log so it sits at
		// the bottom of the transcript. aria-live so assistive tech announces it.
		Div(
			ID("chat-working"),
			Class("hidden max-w-[85%] items-center gap-2 self-start rounded-2xl bg-s2 px-3 py-2 text-sm text-muted"),
			g.Attr("role", "status"),
			g.Attr("aria-live", "polite"),
			Span(Class("animate-pulse"), g.Text("working")),
			Span(
				g.Attr("aria-hidden", "true"),
				Class("inline-flex gap-1"),
				Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted [animation-delay:-0.3s]")),
				Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted [animation-delay:-0.15s]")),
				Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted")),
			),
		),
	}
}

// chatProvisioningIndicator renders an animated "Provisioning agent…" row while
// the agent is still coming up (status pending|provisioning), or nothing once it's
// running. It reuses the animate-bounce dots pattern from #chat-working. The
// #chat-provisioning id lets agentChatScript hide it the instant a real streamed
// message arrives.
// chatIsProvisioning reports whether the agent is still coming up. ONE
// predicate, read by both chatProvisioningIndicator (which renders the
// "Provisioning agent…" row) and chatLogInner's empty state (which must NOT
// render while that row is on screen) — two spellings of it would let the
// transcript show "no messages yet" beside a live provisioning indicator.
func chatIsProvisioning(status string) bool {
	return status == agents.StatusProvisioning || status == agents.StatusPending
}

// chatEmptyState is what an empty transcript says instead of nothing. It is a
// self-start bubble rather than a centred hero so it reads as the first line of
// the conversation, which is what the operator is about to continue.
func chatEmptyState() g.Node {
	return Div(
		g.Attr("data-chat-empty", ""),
		Class("m-auto flex max-w-sm flex-col items-center gap-2 text-center"),
		Div(Class("text-3xl"), g.Text("💬")),
		P(Class("text-sm font-medium text-fg2"), g.Text("No messages yet")),
		P(Class("text-xs text-muted"), g.Text("Send the agent a message to start this thread.")),
	)
}

func chatProvisioningIndicator(status string) g.Node {
	if !chatIsProvisioning(status) {
		return g.Text("")
	}
	return Div(
		ID("chat-provisioning"),
		Class("flex max-w-[85%] items-center gap-2 self-start rounded-2xl bg-s2 px-3 py-2 text-sm text-muted"),
		g.Attr("role", "status"),
		g.Attr("aria-live", "polite"),
		Span(Class("animate-pulse"), g.Text("Provisioning agent…")),
		Span(
			g.Attr("aria-hidden", "true"),
			Class("inline-flex gap-1"),
			Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted [animation-delay:-0.3s]")),
			Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted [animation-delay:-0.15s]")),
			Span(Class("h-1.5 w-1.5 animate-bounce rounded-full bg-muted")),
		),
	)
}

// agentTaskBanner renders the completion banner for a TASK-LINKED agent whose
// work is done — so the user isn't left staring at a still-"running" pod with no
// next step. It appears only when NoteID is set AND the task's status is
// ready_for_review or complete; otherwise nothing (open/in_progress agents, or a
// task-less chat, render no banner). It carries a [Mark complete] action (PATCH
// /tasks/<id>/status status=complete — hidden once already complete) and a
// feedback box (POST /tasks/<id>/comments). Both use hx-swap="none" (the note-card
// response is irrelevant here); Mark-complete dispatches `task:changed` on <body>
// so #chat-log re-fetches and the banner re-renders as complete.
//
// NoteID is nil-guarded up front (g.If evaluates eagerly, so the deref must never
// run for a task-less agent).
func agentTaskBanner(v AgentDetailView) g.Node {
	if v.NoteID == nil {
		return g.Text("")
	}
	complete := v.TaskStatus == notes.StatusComplete
	ready := v.TaskStatus == notes.StatusReadyForReview
	if !complete && !ready {
		return g.Text("")
	}
	id := strconv.FormatInt(*v.NoteID, 10)

	headline := "✅ Agent finished — ready for review"
	tone := "border-accent/30 bg-accent/10"
	if complete {
		headline = "✅ Task complete"
		tone = "border-accent/40 bg-accent/15"
	}

	children := []g.Node{
		Div(Class("text-sm font-medium text-fg"), g.Text(headline)),
	}
	// Mark-complete: hidden once the task is already complete.
	if !complete {
		children = append(children, Form(
			hx("hx-patch", "/tasks/"+id+"/status"),
			hx("hx-swap", "none"),
			// On success re-fetch #chat-log so the banner reflects complete (and the
			// button disappears). task:changed is caught by #chat-log's from:body trigger.
			// On success, leave the agent chat and go back to the Tasks view (the task is
			// done — the user shouldn't be left staring at the agent). Full nav to /tasks.
			hx("hx-on::after-request", "if(event.detail.successful){window.location.href='/tasks'}"),
			Class("contents"),
			Input(Type("hidden"), Name("status"), Value(notes.StatusComplete)),
			Button(
				Type("submit"),
				Class("press min-h-[44px] inline-flex items-center justify-center rounded-lg bg-accent px-3 py-1.5 text-xs font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98]"),
				g.Text("Mark complete"),
			),
		))
	}
	// Feedback box: a small textarea + Send. On success clear it and reveal a
	// subtle "added" confirmation (no banner refresh — a comment doesn't change
	// the task status).
	children = append(children, Form(
		hx("hx-post", "/tasks/"+id+"/comments"),
		hx("hx-swap", "none"),
		// On success clear the box, re-hide Send (empty again), and flash "added".
		hx("hx-on::after-request", "if(event.detail.successful){this.reset();var s=this.querySelector('[data-feedback-send]');if(s)s.classList.add('hidden');var a=this.querySelector('[data-feedback-added]');if(a)a.classList.remove('hidden');}"),
		Class("flex flex-col gap-1.5"),
		Textarea(
			Name("body"),
			Rows("2"),
			Placeholder("Give feedback…"),
			// Send only appears once there's non-whitespace input — an empty feedback
			// box shows no submit affordance. CSP allows hx-on (unsafe-eval).
			hx("hx-on:input", "var s=this.form.querySelector('[data-feedback-send]');if(s)s.classList.toggle('hidden', this.value.trim()==='');"),
			Class("w-full resize-none rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Div(
			Class("flex items-center gap-2"),
			Button(
				Type("submit"),
				g.Attr("data-feedback-send", ""),
				// Hidden until the textarea has content (revealed by the input handler).
				Class("press min-h-[44px] hidden items-center justify-center rounded-lg bg-s2 px-3 py-1.5 text-xs font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s3"),
				g.Text("Send"),
			),
			Span(g.Attr("data-feedback-added", ""), Class("hidden text-xs text-accent"), g.Text("Feedback added")),
		),
	))

	return Div(
		ID("task-banner"),
		Class("flex flex-col gap-2 rounded-xl border p-3 "+tone),
		g.Group(children),
	)
}

func chatBubble(role, content string) g.Node {
	return chatBubbleOf(role,
		// data-md: rendered as markdown client-side (mdConvert in agentChatScript);
		// g.Text keeps the raw text as a safe textContent fallback before JS runs.
		g.Attr("data-md", ""),
		g.Text(content),
	)
}

// chatBubbleOf is the chat bubble's frame — a user's on the right, an
// assistant's on the left — around body. The agent chat fills it with raw text
// its script renders as markdown; the mirrored view with server-rendered
// markdown (it has no chat script).
func chatBubbleOf(role string, body ...g.Node) g.Node {
	mine := role == "user"
	tone := "bg-s2 text-fg self-start"
	if mine {
		tone = "bg-accent/15 text-fg self-end ring-1 ring-inset ring-accent/30"
	}
	return Div(append([]g.Node{
		g.Attr("data-bubble", role),
		Class("max-w-[85%] break-words rounded-2xl px-3 py-2 text-sm " + tone),
	}, body...)...)
}

// chatToolChip renders a persisted tool call (+ its result) as the SAME collapsed
// chip the live stream shows: ONE line — 🔧 name, its target (the file, the
// command), a ✓/✗/… status — expanding to the args and result. call carries the
// args (Content), result (may be nil) the output + ok.
func chatToolChip(call ChatLine, result *ChatLine) g.Node { return toolChip(call, result, "") }

// toolChip is chatToolChip with an element id (the mirrored view replaces a
// running row in place when its result arrives).
func toolChip(call ChatLine, result *ChatLine, id string) g.Node {
	name := call.ToolName
	if name == "" && result != nil {
		name = result.ToolName
	}
	if name == "" {
		name = "tool"
	}
	target := call.ToolTarget
	if target == "" {
		target = cctranscript.ToolTarget(name, call.Content)
	}
	status, statusClass, state := "…", "ml-auto shrink-0 text-muted", "running"
	if result != nil {
		if result.ToolOK {
			status, statusClass, state = "✓", "ml-auto shrink-0 font-semibold text-accent", "ok"
		} else {
			status, statusClass, state = "✗", "ml-auto shrink-0 font-semibold text-st-error-fg", "error"
		}
	}
	body := []g.Node{}
	if strings.TrimSpace(call.Content) != "" {
		body = append(body, toolPre("args", call.Content))
	}
	if result != nil && strings.TrimSpace(result.Content) != "" {
		body = append(body, toolPre("result", result.Content))
	}
	return Details(
		g.If(id != "", ID(id)),
		g.Attr("data-tool-row", ""),
		g.Attr("data-tool-status", state),
		Class("w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line"),
		Summary(
			// min-h-[44px]: the row is a thumb target (the layout spec's rule for
			// every visible summary).
			Class("flex min-h-[44px] min-w-0 cursor-pointer select-none items-center gap-2 px-3 py-2 font-medium text-fg2 hover:text-fg"),
			Span(Class("shrink-0"), g.Text("🔧")),
			Span(Class("shrink-0 font-mono text-st-review-fg"), g.Text(name)),
			g.If(target != "", Span(g.Attr("data-tool-target", ""), Class("min-w-0 truncate font-mono text-muted"), g.Text(target))),
			Span(Class(statusClass), g.Attr("aria-label", "status: "+state), g.Text(status)),
		),
		Div(Class("flex flex-col gap-1 px-3 pb-2 pt-0"), g.Group(body)),
	)
}

// toolPre is one labeled, scrollable code block inside a persisted tool chip
// (matching the live stream's pre()). Long output is truncated in the DOM.
func toolPre(label, txt string) g.Node {
	return Div(
		Div(Class("text-[10px] uppercase tracking-wide text-muted"), g.Text(label)),
		Pre(Class("max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-bg/80 p-2 text-[11px] leading-relaxed text-muted"), g.Text(truncate(txt, 4000))),
	)
}

// agentChatScript opens a WebSocket to the chat proxy and renders the assistant
// turn. It handles the full server→client message contract: delta (assistant
// text), thinking (collapsed reasoning), tool_call/tool_result (collapsible tool
// chips correlated by id), error, and done. Each turn after a send is wrapped in
// its own container so thinking / tool chips / text group visually and interleave
// in arrival order. A "working…" indicator shows from send until done/error.
// Auto-scroll and WS auto-reconnect are preserved.
func agentChatScript(name string) g.Node {
	return Script(g.Raw(`
(function () {
  var AGENT = '` + name + `';

  // --- Minimal, XSS-safe markdown renderer (escape-first, dependency-free) ---
  // HTML is escaped BEFORE any markdown transform, so raw HTML / <script> /
  // onerror in agent or user text can never execute. Covers the common chat
  // cases: fenced + inline code, bold/italic, headings, ordered/unordered lists,
  // http(s) links, paragraphs/line breaks. Used for both the streamed messages
  // and the server-rendered transcript bubbles ([data-md], converted in start()).
  function mdEsc(s) {
    // Strip the private-use sentinel (\uE000) used by mdInline's tokenizer so a
    // crafted message can't collide with / spoof a placeholder token.
    return String(s == null ? '' : s)
      .replace(/\uE000/g, '')
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }
  function mdInline(s) { // s is ALREADY escaped
    // Tokenize FIRST: pull inline code spans (and markdown links) OUT of the
    // string into placeholder tokens before any other inline transform runs, so
    // md chars / bare URLs INSIDE code — and a URL already inside a [text](url)
    // link — are never re-processed (bold/italic/autolink run only on the rest).
    // Tokens are restored verbatim at the end. \uE000 is a private-use sentinel
    // that mdEsc strips from the raw input, so it can't appear in user text.
    var toks = [];
    function stash(html) { toks.push(html); return '\uE000' + (toks.length - 1) + '\uE000'; }
    // Inline ` + "`" + `code` + "`" + ` → token (verbatim; no further markdown/autolink).
    s = s.replace(/` + "`" + `([^` + "`" + `]+)` + "`" + `/g, function (_, c) {
      return stash('<code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">' + c + '</code>');
    });
    // [text](url) markdown links → token, so the bare-URL autolinker below can't
    // double-link the href.
    // Both groups exclude the \uE000 sentinel for the same reason the autolink below does: a
    // code span inside a link (label OR url) has already been stashed, so without
    // the exclusion its PLACEHOLDER is captured — the code span is restored first
    // (lower index) and the raw sentinel ends up inside the emitted href, or the
    // code text vanishes from the label. Excluding it leaves such input literal,
    // which is what it was before the tokenizer existed. Same fix as the Go
    // renderer's mdLink (internal/ui/markdown.go).
    s = s.replace(/\[([^\]\uE000]+)\]\((https?:\/\/[^\s)\uE000]+)\)/g, function (_, t, u) {
      return stash('<a href="' + u + '" target="_blank" rel="noopener noreferrer" class="text-accent underline">' + t + '</a>');
    });
    // Autolink bare http(s) URLs. Trailing punctuation (. , ) ] ! ? : ; ") stays
    // OUT of the href. Code + markdown-link URLs are already tokenized out above;
    // the \uE000 sentinel is excluded from the URL so a URL directly abutting a
    // token can't swallow the placeholder into the href.
    s = s.replace(/https?:\/\/[^\s<\uE000]+/g, function (m) {
      var trail = '';
      var mm = m.match(/[.,)\]!?:;"']+$/);
      if (mm) { trail = mm[0]; m = m.slice(0, m.length - trail.length); }
      return '<a href="' + m + '" target="_blank" rel="noopener noreferrer" class="text-accent underline">' + m + '</a>' + trail;
    });
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    s = s.replace(/(^|[^_])_([^_\n]+)_/g, '$1<em>$2</em>');
    // Restore tokens verbatim (their content was already escaped before stashing).
    s = s.replace(/\uE000(\d+)\uE000/g, function (_, i) { return toks[+i]; });
    return s;
  }
  // mdTableCells splits one pipe-table row into trimmed cells, dropping the
  // optional leading/trailing pipe (so both "| a | b |" and "a | b" work).
  function mdTableCells(line) {
    var s = line.trim();
    if (s.charAt(0) === '|') s = s.slice(1);
    if (s.charAt(s.length - 1) === '|') s = s.slice(0, -1);
    return s.split('|').map(function (c) { return c.trim(); });
  }
  // mdIsTableSep matches a GFM separator row: every cell is dashes with optional
  // surrounding colons (e.g. "| --- | :--: |" or "---|:--").
  function mdIsTableSep(line) {
    if (line.indexOf('|') < 0) return false;
    return mdTableCells(line).every(function (c) { return /^:?-+:?$/.test(c); });
  }
  function mdRender(src) {
    var lines = mdEsc(src).split('\n'), out = [], i = 0;
    while (i < lines.length) {
      var line = lines[i];
      if (/^` + "```" + `/.test(line)) { // fenced code block
        var code = []; i++;
        while (i < lines.length && !/^` + "```" + `\s*$/.test(lines[i])) { code.push(lines[i]); i++; }
        i++;
        out.push('<pre class="overflow-auto rounded-lg bg-bg/60 p-2 my-1 font-mono text-[0.85em] leading-relaxed"><code>' + code.join('\n') + '</code></pre>');
        continue;
      }
      // Thematic break (--- / *** / ___ alone on a line). Checked before the
      // list branches and excluded from the paragraph gather below, for the same
      // reason the Go renderer does both (internal/ui/markdown.go): the gather
      // runs last and would otherwise emit the rule as literal text.
      if (/^ {0,3}(-{3,}|\*{3,}|_{3,})[ \t]*$/.test(line)) { out.push('<hr class="my-2 border-line">'); i++; continue; }
      var h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) { out.push('<div class="mt-1 font-semibold text-fg ' + (h[1].length <= 2 ? 'text-base' : 'text-sm') + '">' + mdInline(h[2]) + '</div>'); i++; continue; }
      if (/^\s*[-*]\s+/.test(line)) {
        var ul = [];
        while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) { ul.push('<li>' + mdInline(lines[i].replace(/^\s*[-*]\s+/, '')) + '</li>'); i++; }
        out.push('<ul class="my-1 list-disc space-y-0.5 pl-5">' + ul.join('') + '</ul>'); continue;
      }
      if (/^\s*\d+\.\s+/.test(line)) {
        var ol = [];
        while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) { ol.push('<li>' + mdInline(lines[i].replace(/^\s*\d+\.\s+/, '')) + '</li>'); i++; }
        out.push('<ol class="my-1 list-decimal space-y-0.5 pl-5">' + ol.join('') + '</ol>'); continue;
      }
      // Pipe table: a header row containing '|' immediately followed by a
      // separator row (dashes + optional colons). Checked before the '|' line is
      // treated as a paragraph. Cells run through the inline-md pass (escape-first,
      // so still XSS-safe). Wrapped in overflow-x-auto so it scrolls on mobile.
      if (line.indexOf('|') >= 0 && i + 1 < lines.length && mdIsTableSep(lines[i + 1])) {
        var headCells = mdTableCells(line);
        i += 2; // consume header + separator
        var head = '<thead><tr>' + headCells.map(function (c) {
          return '<th class="border border-line px-2 py-1 text-left">' + mdInline(c) + '</th>';
        }).join('') + '</tr></thead>';
        var rows = [];
        while (i < lines.length && lines[i].indexOf('|') >= 0 && !/^\s*$/.test(lines[i])) {
          var cells = mdTableCells(lines[i]);
          rows.push('<tr>' + cells.map(function (c) {
            return '<td class="border border-line px-2 py-1">' + mdInline(c) + '</td>';
          }).join('') + '</tr>');
          i++;
        }
        out.push('<div class="overflow-x-auto my-1"><table class="my-1 w-full border-collapse text-xs">' + head + '<tbody>' + rows.join('') + '</tbody></table></div>');
        continue;
      }
      if (/^\s*$/.test(line)) { i++; continue; }
      var para = [];
      while (i < lines.length && !/^\s*$/.test(lines[i]) && !/^` + "```" + `/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i]) && !/^ {0,3}(-{3,}|\*{3,}|_{3,})[ \t]*$/.test(lines[i]) && !/^\s*[-*]\s+/.test(lines[i]) && !/^\s*\d+\.\s+/.test(lines[i]) && !(lines[i].indexOf('|') >= 0 && i + 1 < lines.length && mdIsTableSep(lines[i + 1]))) { para.push(lines[i]); i++; }
      out.push('<p class="my-1">' + mdInline(para.join('<br>')) + '</p>');
    }
    return out.join('');
  }
  // Convert any server-rendered transcript bubbles ([data-md]) once.
  function mdConvert(root) {
    (root || document).querySelectorAll('[data-md]:not([data-md-done])').forEach(function (el) {
      el.innerHTML = mdRender(el.textContent);
      el.setAttribute('data-md-done', '');
    });
  }

  // Boost-safe init: this page's body may be swapped in via hx-boost (no
  // DOMContentLoaded), and the prior page's WebSocket must be torn down so it
  // doesn't leak / keep reconnecting to a page that's gone. We run start() on
  // BOTH DOMContentLoaded and htmx:load, and dispose any previous connection
  // first. A single global htmx:beforeSwap listener (guarded) disposes on
  // navigate-away. window.__cgChat holds the live session.
  function disposeChat() {
    var c = window.__cgChat;
    if (!c) return;
    c.disposed = true;            // stop the reconnect loop
    try { if (c.ws) { c.ws.onclose = null; c.ws.close(); } } catch (e) {}
    // Tear down the kickoff-stream EventSource too, so a boosted navigate-away
    // doesn't leak it (mirrors the WS teardown).
    try { if (c.es) { c.es.close(); } } catch (e) {}
    window.__cgChat = null;
  }

  function start() {
  var form = document.getElementById('chat-form');
  var input = document.getElementById('chat-input');
  var sendBtn = document.getElementById('chat-send');
  var logEl = document.getElementById('chat-log');
  // Re-resolve #chat-working on each use: the sse:chat.reply refresh swaps
  // #chat-log's innerHTML, replacing this node — a captured reference would go
  // stale (detached), breaking show/hide + insertBefore.
  function workingEl() { return document.getElementById('chat-working'); }
  // Hide the "Provisioning agent…" indicator the moment a real streamed message
  // starts (belt-and-suspenders; the chat.reply refresh also drops it once the
  // agent is running).
  function hideProvisioning() {
    var p = document.getElementById('chat-provisioning');
    if (p) p.classList.add('hidden');
  }
  if (!form || !input || !logEl) return;
  // Render any server-rendered transcript bubbles as markdown (idempotent).
  mdConvert(logEl);
  // Already wired for this exact form (e.g. DOMContentLoaded + htmx:load both
  // fired for the same body): don't double-bind.
  if (form.hasAttribute('data-cg-chat')) return;
  // A different page's chat is live — tear it down before wiring this one.
  disposeChat();
  form.setAttribute('data-cg-chat', '1');

  // Derive the target agent from the LIVE form (boost-safe), falling back to the
  // server-rendered name. This is what lets a single global htmx:load->start
  // hook stay correct across boosted navigations between different agents.
  var agent = form.getAttribute('data-agent') || AGENT;
  // The active chat session id (from the LIVE form) selects which gateway context
  // the socket talks to. Switching sessions is a boosted nav that re-renders the
  // form with a fresh data-session, so this stays correct across navigations.
  var sessionId = form.getAttribute('data-session') || '';

  // streaming: true while a user-initiated WS turn is actively streaming. The
  // global htmx:beforeRequest hook reads it to CANCEL a sse:chat.reply #chat-log
  // refresh mid-stream (so it can't clobber the in-flight streamed turn); the
  // trailing chat.reply fired after 'done' then refreshes to canonical state.
  var session = { ws: null, es: null, disposed: false, streaming: false };
  window.__cgChat = session;

  function scheme() { return location.protocol === 'https:' ? 'wss:' : 'ws:'; }
  var ws = null;
  // Per-turn render state. turn = the container div for the active assistant turn;
  // text = the current text bubble (created lazily on first delta after any
  // chip/thinking); think = the thinking <details>'s body; tools = id->chip map.
  var turn = null, text = null, think = null, tools = {};

  // Sticky autoscroll: follow new content ONLY while the user is pinned near the
  // bottom. If they scroll up (to read back) session.stick goes false and new
  // messages stop yanking the view; scrolling back to the bottom re-engages it. A
  // programmatic scroll-to-bottom lands AT the bottom, so stick stays true.
  session.stick = true;
  function nearBottom() { return logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 48; }
  logEl.addEventListener('scroll', function () { session.stick = nearBottom(); }, { passive: true });
  function scroll() { if (session.stick) logEl.scrollTop = logEl.scrollHeight; }
  // forceScroll jumps to the bottom and re-engages stick — used when the user's
  // OWN message is (re)rendered (kickoff after provisioning, or a sent message):
  // their turn should always bring the latest into view regardless of prior scroll.
  function forceScroll() { session.stick = true; logEl.scrollTop = logEl.scrollHeight; }
  // Keep the working indicator pinned to the bottom of the log while visible.
  function showWorking() {
    var working = workingEl();
    if (!working) return;
    logEl.appendChild(working); // move to end
    working.classList.remove('hidden');
    working.classList.add('flex');
    // Force to the bottom (not sticky): the working indicator appears right after
    // the user's own turn starts, so it must always be brought into view — the
    // point is to SEE that the agent is working.
    forceScroll();
  }
  function hideWorking() {
    var working = workingEl();
    if (!working) return;
    working.classList.add('hidden');
    working.classList.remove('flex');
  }

  // A standalone bubble (user messages, and assistant text when there's no turn
  // container — defensive).
  function bubble(role) {
    var d = document.createElement('div');
    var mine = role === 'user';
    d.className = 'max-w-[85%] break-words rounded-2xl px-3 py-2 text-sm ' +
      (mine ? 'bg-accent/15 text-fg self-end ring-1 ring-inset ring-accent/30'
            : 'bg-s2 text-fg self-start');
    var w = workingEl();
    logEl.insertBefore(d, (w && w.parentNode === logEl) ? w : null);
    scroll();
    return d;
  }

  // Start a fresh assistant turn container. Chips/thinking/text are appended to it
  // in arrival order so they group and interleave naturally.
  function newTurn() {
    var t = document.createElement('div');
    t.className = 'flex max-w-[92%] flex-col gap-1.5 self-start';
    var w = workingEl();
    logEl.insertBefore(t, (w && w.parentNode === logEl) ? w : null);
    return t;
  }
  function ensureTurn() { if (!turn) { turn = newTurn(); hideProvisioning(); } return turn; }

  // The assistant text bubble inside the current turn (created on first delta).
  function ensureText() {
    if (!text) {
      ensureTurn();
      text = document.createElement('div');
      text.className = 'break-words rounded-2xl bg-s2 px-3 py-2 text-sm text-fg';
      turn.appendChild(text);
    }
    return text;
  }

  // The collapsed "💭 thinking" details, created only when thinking arrives.
  function ensureThink() {
    if (!think) {
      ensureTurn();
      var d = document.createElement('details');
      d.className = 'rounded-xl bg-bg/40 text-xs ring-1 ring-inset ring-line';
      var s = document.createElement('summary');
      s.className = 'cursor-pointer select-none px-3 py-2 font-medium text-muted hover:text-fg';
      s.textContent = '💭 thinking';
      var body = document.createElement('div');
      body.className = 'whitespace-pre-wrap break-words px-3 pb-2 pt-0 leading-relaxed text-muted';
      d.appendChild(s); d.appendChild(body);
      turn.appendChild(d);
      think = body;
    }
    return think;
  }

  // Truncate long tool output in the DOM (full text still expandable as the
  // <pre> scrolls); keeps the transcript compact on mobile.
  function clip(s) {
    s = String(s == null ? '' : s);
    return s.length > 4000 ? s.slice(0, 4000) + '\n…(truncated)' : s;
  }

  // Render (or fetch) the collapsible tool chip for a tool_call id.
  function toolChip(id, nm, target) {
    if (tools[id]) return tools[id];
    ensureTurn();
    var d = document.createElement('details');
    d.className = 'rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line';
    var s = document.createElement('summary');
    s.className = 'flex cursor-pointer select-none items-center gap-2 px-3 py-2 font-medium text-fg2 hover:text-fg';
    var icon = document.createElement('span'); icon.textContent = '🔧';
    var nameEl = document.createElement('span'); nameEl.className = 'shrink-0 font-mono text-st-review-fg'; nameEl.textContent = nm || 'tool';
    var stat = document.createElement('span'); stat.className = 'ml-auto shrink-0 text-muted'; stat.textContent = '…';
    s.appendChild(icon); s.appendChild(nameEl);
    if (target) { var tg = document.createElement('span'); tg.className = 'min-w-0 truncate font-mono text-muted'; tg.setAttribute('data-tool-target', ''); tg.textContent = target; s.appendChild(tg); }
    s.appendChild(stat);
    s.className += ' min-h-[44px] min-w-0';
    d.className += ' w-full max-w-[92%] min-w-0 self-start';
    d.setAttribute('data-tool-row', ''); d.setAttribute('data-tool-status', 'running');
    var body = document.createElement('div'); body.className = 'flex flex-col gap-1 px-3 pb-2 pt-0';
    d.appendChild(s); d.appendChild(body);
    turn.appendChild(d);
    var chip = { details: d, status: stat, body: body, args: null, result: null };
    tools[id] = chip;
    return chip;
  }
  function pre(label, txt) {
    var wrap = document.createElement('div');
    var lab = document.createElement('div'); lab.className = 'text-[10px] uppercase tracking-wide text-muted'; lab.textContent = label;
    var p = document.createElement('pre'); p.className = 'max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-bg/80 p-2 text-[11px] leading-relaxed text-muted';
    p.textContent = txt;
    wrap.appendChild(lab); wrap.appendChild(p);
    return wrap;
  }

  function resetTurn() { turn = null; text = null; think = null; tools = {}; }

  // Shared stream renderer for BOTH the user-turn WebSocket and the server-side
  // KICKOFF stream (SSE agent.stream): both translate their wire messages into this
  // same {type, content/id/name/args/ok/output} shape so a delta/thinking/tool_call/
  // tool_result/user/error/done render IDENTICALLY. This is the single renderer —
  // the SSE path maps agent.stream fields to this shape (see connectStream) rather
  // than duplicating the DOM logic.
  function renderStreamMsg(msg) {
    switch (msg.type) {
      case 'user': {
        // His kickoff message, streamed first so the chat shows the prompt before
        // the model runs (mdRender is escape-first, so it's XSS-safe). Force the
        // view to the bottom: once provisioning ends and his message renders, it
        // should be in view (req: scroll to bottom after my message renders).
        var ub = bubble('user'); ub.innerHTML = mdRender(msg.content || '');
        forceScroll();
        break;
      }
      case 'delta': {
        var dt = ensureText(); dt._raw = (dt._raw || '') + (msg.content || ''); dt.innerHTML = mdRender(dt._raw);
        scroll();
        break;
      }
      case 'thinking':
        ensureThink().textContent += msg.content || '';
        // Close the current text bubble so any text AFTER this thinking starts a
        // fresh bubble appended below it — keeps parts in arrival (timestamp) order.
        text = null;
        scroll();
        break;
      case 'tool_call': {
        var c = toolChip(msg.id, msg.name, msg.target);
        if (!c.args && msg.args) { c.args = pre('args', clip(msg.args)); c.body.appendChild(c.args); }
        // Interleave: text streamed AFTER this tool call must render in a NEW bubble
        // below the chip, not merged into the pre-tool text bubble (which made tool
        // calls look out of order vs the narration).
        text = null;
        scroll();
        break;
      }
      case 'tool_result': {
        var r = toolChip(msg.id, msg.name); // defensive: create if missing
        r.status.textContent = msg.ok ? '✓' : '✗';
        r.status.className = 'ml-auto shrink-0 font-semibold ' + (msg.ok ? 'text-accent' : 'text-st-error-fg');
        r.details.setAttribute('data-tool-status', msg.ok ? 'ok' : 'error');
        if (r.result) r.result.remove();
        r.result = pre('result', clip(msg.output));
        r.body.appendChild(r.result);
        text = null;
        scroll();
        break;
      }
      case 'error':
        var et = ensureText(); et._raw = (et._raw ? et._raw + '\n' : '') + '[error] ' + (msg.content || ''); et.innerHTML = mdRender(et._raw);
        hideWorking();
        resetTurn();
        session.streaming = false;
        sendBtn.disabled = false;
        scroll();
        break;
      case 'done':
        hideWorking();
        resetTurn();
        session.streaming = false;
        sendBtn.disabled = false;
        // Mark this session read: a reply we just watched shouldn't linger as
        // "unread" in the notification FAB. Best-effort; ignore errors.
        if (sessionId) {
          try {
            fetch('/agents/sessions/' + sessionId + '/read', { method: 'POST', credentials: 'include' }).catch(function () {});
          } catch (e) {}
        }
        break;
    }
  }

  // Re-fetch #chat-log to reconcile the streamed (kickoff) bubbles with the
  // canonical server transcript once the turn persists: this replaces the ephemeral
  // thinking/tool chips with the persisted user+assistant bubbles and reveals the
  // completion banner. Deterministic (driven off OUR ordered EventSource, after the
  // 'done' clears the streaming guard) so the guard can't cancel it. The existing
  // htmx sse:chat.reply refresh is redundant-but-harmless (idempotent GET).
  function reconcileChatLog() {
    if (!window.htmx) return;
    var el = document.getElementById('chat-log');
    var url = el && el.getAttribute('hx-get');
    if (!url) return;
    try { window.htmx.ajax('GET', url, { target: '#chat-log', swap: 'innerHTML' }); } catch (e) {}
  }

  // Live-render the server-side KICKOFF stream over the shared /events SSE. The
  // kickoff runs server-side (NOT over this page's user-turn WS), so without this
  // the chat jumps from "provisioning" straight to the persisted reply. A raw
  // EventSource (filtered to THIS agent) keeps agent.stream events strictly ordered
  // on one connection — his message → thinking → text → done — so the terminal
  // 'done' reliably clears the streaming guard before we reconcile. Best-effort: any
  // failure to open it must never break the chat.
  function connectStream() {
    if (session.disposed || !window.EventSource) return;
    var es;
    try { es = new EventSource('/events'); } catch (e) { return; }
    session.es = es;
    es.addEventListener('agent.stream', function (ev) {
      var d;
      try { d = JSON.parse(ev.data); } catch (e) { return; }
      if (!d || d.agent !== agent) return; // only THIS page's agent
      // First stream event: mark streaming (so the htmx sse:chat.reply refresh is
      // cancelled mid-stream and can't clobber the live bubbles), drop the
      // provisioning indicator, and show the animated "working…" row — the kickoff
      // often runs for a while (thinking / tool loop) BEFORE any text delta arrives,
      // and without this that gap showed nothing. It's cleared on 'done' (hideWorking
      // in renderStreamMsg) and stays pinned to the bottom as text streams above it.
      if (!session.streaming) { session.streaming = true; hideProvisioning(); showWorking(); }
      switch (d.kind) {
        case 'user': renderStreamMsg({ type: 'user', content: d.text }); break;
        case 'text': renderStreamMsg({ type: 'delta', content: d.text }); break;
        case 'thinking': renderStreamMsg({ type: 'thinking', content: d.text }); break;
        case 'tool_call': renderStreamMsg({ type: 'tool_call', id: d.toolId, name: d.toolName, args: d.toolArgs }); break;
        case 'tool_result': renderStreamMsg({ type: 'tool_result', id: d.toolId, name: d.toolName, ok: d.toolOk, output: d.toolOutput }); break;
        case 'done':
          renderStreamMsg({ type: 'done' }); // clears session.streaming
          reconcileChatLog();
          break;
      }
    });
    // A dropped SSE connection is non-fatal (the trailing chat.reply refresh still
    // reconciles); let the browser auto-reconnect. No manual retry loop needed.
  }

  function connect() {
    if (session.disposed) return;
    var url = scheme() + '//' + location.host + '/agents/' + agent + '/ws';
    if (sessionId) url += '?session=' + encodeURIComponent(sessionId);
    ws = new WebSocket(url);
    session.ws = ws;
    ws.onmessage = function (ev) {
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      renderStreamMsg(msg);
    };
    ws.onclose = function () { if (!session.disposed) setTimeout(connect, 2000); };
  }
  connect();
  // Open scrolled to the latest message: the transcript is converted + laid out
  // by now, so forceScroll() reads the full scrollHeight and pins the log to bottom.
  forceScroll();
  // Live-render the server-side kickoff stream (agent.stream over /events) with the
  // same renderer as the WS turn. Opened after the initial scroll so it never races
  // the open-at-bottom behaviour.
  connectStream();

  // 🔴 NEVER A SILENT SEND. With the socket open the message goes over it and
  // the reply streams back. With it NOT open — still connecting, dropped, or
  // refused — this used to return and do nothing at all. Now the browser sends
  // the form itself (POST, a redirect back here): the same turn, by the door
  // that needs no socket. The form's nonce is renewed after a socket send, so
  // that message and the next are never taken for one.
  function nativeSend() { HTMLFormElement.prototype.submit.call(form); }
  function renewNonce() {
    var n = document.getElementById('chat-nonce');
    if (!n) return;
    n.value = '';
    if (!window.crypto || !crypto.getRandomValues) return;
    var b = new Uint8Array(16), s = '';
    crypto.getRandomValues(b);
    for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? '0' : '') + b[i].toString(16);
    n.value = s;
  }
  function send() {
    var t = input.value.trim();
    if (!t) return;
    if (!ws || ws.readyState !== 1) { nativeSend(); return; }
    var ub = bubble('user'); ub.innerHTML = mdRender(t);
    forceScroll(); // his own send always brings the latest into view
    ws.send(JSON.stringify({ type: 'message', text: t }));
    try { window.cgTrack('chat.sent', { agent: (form.getAttribute('data-agent') || '') }); } catch (e) {}
    input.value = '';
    renewNonce();
    sendBtn.disabled = true;
    session.streaming = true;
    resetTurn();
    showWorking();
  }
  // preventDefault only once send() has taken the message: if it throws, the
  // event is left alone and the browser submits the form.
  form.addEventListener('submit', function (e) { send(); e.preventDefault(); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });
  // A notice the server drew for a send the browser made has been shown: take
  // it out of the address, so a reload does not repeat it.
  try {
    var here = new URL(location.href);
    if (here.searchParams.has('notice')) {
      here.searchParams.delete('notice');
      history.replaceState(history.state, '', here.pathname + here.search + here.hash);
    }
  } catch (e) {}
  } // end start

  // Global, document-lifetime hooks (the <body> survives hx-boost swaps, so
  // these must bind exactly once — guarded). start() reads its target agent from
  // the live #chat-form[data-agent], so a single htmx:load hook stays correct
  // across boosted navigations between different agents; disposeChat() is
  // page-agnostic (operates on window.__cgChat).
  if (!window.__cgChatGlobals) {
    window.__cgChatGlobals = true;
    // Tear the outgoing socket down when an hx-boost navigation swaps the whole
    // <body> away (target === body). Partial swaps within the page (grants pane,
    // model form, SSE log appends) target sub-elements and must NOT kill the chat.
    document.body.addEventListener('htmx:beforeSwap', function (e) {
      if (e.detail && e.detail.target === document.body) disposeChat();
    });
    // (Re)connect the chat after a boosted body-swap / history restore brings a
    // chat page in. Idempotent via the form's data-cg-chat marker.
    document.body.addEventListener('htmx:load', start);
    // Guard the sse:chat.reply #chat-log refresh: never let it fire WHILE a WS turn
    // is mid-stream (it would replace the in-flight streamed bubbles). chat.reply
    // fires again after 'done' (streaming=false), which refreshes canonically.
    document.body.addEventListener('htmx:beforeRequest', function (e) {
      var elt = e.detail && e.detail.elt;
      if (elt && elt.id === 'chat-log' && window.__cgChat && window.__cgChat.streaming) {
        e.preventDefault();
      }
    });
    // After a #chat-log refresh swap: convert the fresh transcript bubbles to
    // markdown and pin to the bottom (the WS client only runs mdConvert/scroll on
    // its own events, so a server-driven refresh needs this).
    document.body.addEventListener('htmx:afterSwap', function (e) {
      var t = e.detail && e.detail.target;
      if (t && t.id === 'chat-log') {
        mdConvert(t);
        // Respect sticky scroll: only pin to bottom if the user hasn't scrolled up
        // (stick !== false). Default to pinning when there's no live chat session.
        if (!window.__cgChat || window.__cgChat.stick !== false) t.scrollTop = t.scrollHeight;
      }
    });
  }

  // Arrival on a non-boosted (real) load: this <script> re-executes as part of
  // the page body, so just start once the DOM is ready.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
`))
}
