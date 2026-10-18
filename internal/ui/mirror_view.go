package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/web"
)

// THE RENDERED VIEW of a mirrored tmux session whose pane runs Claude Code:
// prompts and replies as chat bubbles, each tool call as ONE collapsed row
// (tool, target, ok / error / running), thinking never shown, live while a turn
// runs. It is the DEFAULT for such a session; the Terminal is one tap away.
//
// 🔴 IT DRAWS WITH THE AGENT CHAT'S OWN COMPONENTS (chatBubbleOf, toolChip), so
// a tool row reads the same in an agent's chat and in a mirrored session — the
// rows are rendered HERE, on the server, and sent to the page as HTML; the page
// only places them.

// MirrorViewPath is a mirrored session's rendered view (its active window).
func MirrorViewPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.ViewPath()
}

// MirrorViewWSPath is that page's transcript socket, before the page adds its
// resume point.
func MirrorViewWSPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.ViewWSPath()
}

// MirrorViewView is the data the rendered view's page renders.
type MirrorViewView struct {
	Host    string
	Session string
	// Window is the tmux window id the view is of; "" = the session's active
	// window (a per-session host). With Session it is the page's ADDRESS: its
	// socket and its composer name the same window.
	Window string
	// Title is what an entry with no Label is called ("<session>:<window
	// index>"); "" = the session name.
	Title string
	// Label is the host's display name for the session ("mango:1"), "" for
	// none: the page's title. Session stays the address.
	Label string
	// Qualifier is what tells this entry apart from another listed entry of the
	// same name ("" = none) — the SAME qualifier its Agents card shows
	// (mirror.Qualified): part of the header's title and of <title>.
	Qualifier string
	// Interactive: the host advertised -interactive, so the composer is offered
	// (each send is still gated on a fresh sign-in, server-side). False draws
	// it disabled, saying why.
	Interactive bool
	// StepUp is the "Confirm it's you" wiring (stepup_confirm.go): a send
	// refused as stale asks there — passkey or password — and retries. Unused
	// (no dialog is drawn) on a read-only host, which cannot be sent to at all.
	StepUp *StepUpView
	// Notice, NoticeOK and Nonce are the composer's (ComposerView).
	Notice   string
	NoticeOK bool
	Nonce    string
	// Dictate is the composer's (ComposerView.Dictate).
	Dictate string
	// Upload is the composer's (ComposerView.Upload).
	Upload string
	// Queue is the composer's send queue (ComposerView.Queue); nil for none.
	Queue *ComposerQueue
	// FocusHistory is the session's focus history, the newest page
	// (FocusHistoryPath); "" when the host reports no Claude session for the
	// window, which then has none. The index sheet's second tab.
	FocusHistory string
}

// MirrorReadOnlyNote is what a read-only host's composer says instead of
// accepting text.
const MirrorReadOnlyNote = "This host is read-only — its muster-mirror was not started with -interactive, so nothing can be sent from here."

// MirrorViewPage is the rendered view's document.
//
// ⚠ ITS OWN DOCUMENT, like the terminal page: a real load (the links into it
// opt out of boost), with its own socket and script.
func MirrorViewPage(v MirrorViewView) g.Node {
	ref := MirrorRef{Host: v.Host, Session: v.Session, Window: v.Window}
	composer := ComposerView{SendPath: MirrorSendPath, Host: v.Host, Session: v.Session, Window: v.Window,
		Placeholder: "Message Claude…", Aria: "Message Claude in this session", DisabledPlaceholder: "Read-only host",
		Notice: v.Notice, NoticeOK: v.NoticeOK, Nonce: v.Nonce, Dictate: v.Dictate, Upload: v.Upload, Queue: v.Queue,
		// Pinned to the visible viewport, not in flow at the bottom of the body:
		// on a phone the body's bottom can be under the browser's chrome, and
		// this page cannot be scrolled to bring it back (composerPinScript).
		Pinned: true}
	if !v.Interactive {
		composer.Disabled = MirrorReadOnlyNote
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text(mirrorQualified(markdownPlain(displayOr(v.Label, displayOr(v.Title, v.Session))), v.Qualifier)+" · Mirrored · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				faroHead(),
			),
			Body(
				// The app shell (only #mv-log scrolls), sized to the visible
				// viewport and ending above the pinned composer.
				Class(pinnedShellClass+" bg-bg text-fg antialiased"),
				Header(
					Class("flex items-center gap-2 border-b border-line px-2 py-1.5"),
					A(
						ID("mv-back"),
						Href("/agents"),
						g.Attr("aria-label", "Back to the list"),
						Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
						g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
					),
					Div(
						Class("min-w-0 flex-1"),
						H1(Class("truncate text-sm font-semibold"), mirrorEntryTitle(v.Label, v.Title, v.Session, v.Qualifier, ""),
							Span(g.Attr("data-term-badge", ""), Class("ml-2"), mirrorBadge("Mirrored · "+v.Host))),
						Div(
							Class("flex min-w-0 items-center gap-2"),
							P(ID("mv-status"), Class("truncate text-xs text-muted"), g.Attr("aria-live", "polite"), g.Text("Connecting…")),
							// The context readout (TakeContext): hidden until a turn
							// has reported usage, and again after a reset.
							Span(ID("mv-context"), g.Attr("hidden", ""), g.Attr("title", "Context used"),
								Class("shrink-0 rounded-full bg-s2 px-2 py-0.5 text-[11px] tabular-nums text-fg2 ring-1 ring-inset ring-line")),
						),
					),
					chatIndexButton("mv-log"),
					viewToggle(true, ref.ViewPath(), ref.TerminalPath()),
				),
				Div(
					ID("mv-error"),
					g.Attr("role", "alert"),
					g.Attr("hidden", ""),
					Class("border-b border-line bg-danger/15 px-3 py-2 text-xs text-fg break-words"),
					Span(ID("mv-error-text")),
				),
				Main(
					ID("mv-log"),
					g.Attr("data-ws", ref.ViewWSPath()),
					g.Attr("aria-label", "Conversation"),
					Class("flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto overflow-x-hidden px-3 py-3"),
				),
				// The questions the window's ask hook is holding (mirror_ask.go).
				mirrorAsksRegion(ref),
				Div(
					ID("mv-working"),
					g.Attr("hidden", ""),
					g.Attr("role", "status"),
					Class("px-3 pb-1 text-xs text-muted"),
					Span(Class("animate-pulse"), g.Text("Claude is working…")),
				),
				mirrorComposer(composer),
				g.If(v.Interactive, confirmDialog(v.StepUp)),
				g.If(v.Interactive, stepUpScript()),
				mirrorViewScript(),
				// The question cards' poll and submit (mirror_ask.go).
				askScript(),
				// The tool groups' open/close animation (tool_groups.go).
				toolGroupScript(),
				bubbleClampScript(),
				chatIndexSheet(v.FocusHistory, "mirror-view:"+ref.query()),
				chatIndexScript(),
				mirrorComposerScript(),
				pwaChrome(),
			),
		),
	)
}

// viewToggle is the Rendered | Terminal switch in the header of anything that
// has both views — a mirrored Claude Code window, a claude-code agent: the
// current view (aria-current, not a link) and a real-load link to the other.
// The current one's href is unused.
func viewToggle(rendered bool, renderedHref, terminalHref string) g.Node {
	seg := func(id, label, href string, current bool) g.Node {
		// 44px: a thumb target, like every other control in these headers.
		cls := "press inline-flex min-h-[44px] items-center px-2.5 text-xs font-semibold transition "
		if current {
			return Span(ID(id), g.Attr("aria-current", "page"), Class(cls+"bg-accent text-on-accent"), g.Text(label))
		}
		return A(ID(id), Href(href), hx("hx-boost", "false"), Class(cls+"text-fg2 hover:bg-s3"), g.Text(label))
	}
	return Nav(
		g.Attr("aria-label", "View"),
		Class("inline-flex shrink-0 overflow-hidden rounded-lg bg-s2 ring-1 ring-inset ring-line"),
		seg("view-rendered", "Rendered", renderedHref, rendered),
		seg("view-terminal", "Terminal", terminalHref, !rendered),
	)
}

// MirrorOp is one DOM operation the rendered view's page applies:
//
//   - append: add HTML to the conversation — or, with Into, to the element of
//     that id (a tool-call group's body);
//   - replace: put HTML in place of the element with ID (a running tool row
//     whose result arrived; a group's summary as its count ticks up);
//   - wrap: put HTML (a tool-call group) in place of the element with ID and
//     move that element into the group's body — the second call of a run;
//   - attr: set attribute Name to Value on the element with ID (a group's state);
//   - result: a tool row's result for a call an EARLIER socket drew (Resumed):
//     the row with ID takes HTML's status and output and keeps the rest; with
//     no such row on the page, it is an append.
type MirrorOp struct {
	Op    string `json:"op"` // "append" | "replace" | "wrap" | "attr" | "result"
	ID    string `json:"id,omitempty"`
	HTML  string `json:"html,omitempty"`
	Into  string `json:"into,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// MirrorTranscript is one rendered-view socket's drawing state: the tool calls
// on screen, so a result can redraw its call's row in place.
//
// 🔴 IN MEMORY, PER SOCKET, AND BOUNDED: it is dropped with the socket and
// reset with the view, and holds at most maxDrawnCalls calls (the oldest are
// forgotten; a result for one draws as its own row).
type MirrorTranscript struct {
	calls map[string]ChatLine
	order []string
	// The tool-call grouping (tool_groups.go): each drawn row's state, the
	// group a row was drawn into, and the run the conversation currently ends
	// in — the rows since the last thing that was not a tool row, and their
	// group once there are two. Bounded with calls (forget).
	states  map[string]string
	groupOf map[string]string
	groups  map[string][]string
	run     []string
	runGrp  string
	// Resumed says the page this socket draws for already holds a conversation
	// drawn by an EARLIER socket (it reconnected with gen and offset, and no
	// Reset came). This transcript has none of that page's calls, so a result
	// for one of them is not an orphan to draw at the end: it is redrawn where
	// the page has it (a `replace`), and no run is started from it.
	//
	// 🔴 WITHOUT IT A RESUME SCRAMBLED A RUN: calls a, b, c drawn as one group,
	// the socket dropped and resumed, then results a, b, c arrived — each taken
	// for an orphan and appended by the grouping rule, so b and c were MOVED out
	// of the group into a new one at the end (the client's dedupe removes the old
	// row before an append) under the SAME group id as the first: a duplicate id
	// and a split, reordered run. The page refreshes a redrawn row's group count
	// and state itself (toolGroupScript's refresh).
	Resumed bool
	// usage is the latest turn's context use, and fresh whether it changed
	// since TakeContext last reported it.
	usage *cctranscript.Usage
	fresh bool
	// asks is the AskUserQuestion calls drawn as question cards (mirror_ask.go),
	// with their questions: a result for one redraws its card. Bounded with
	// calls (forget).
	asks map[string][]askq.Question
}

const maxDrawnCalls = 2048

// MirrorPromptID is a mirrored prompt's element id, from its Event.Line: the
// ONE spelling both its bubble (Ops) and its prompt-index entry
// (NewMirrorIndex) use, so the message index finds the bubble a prompt has.
func MirrorPromptID(line int64) string { return "mv-p-" + strconv.FormatInt(line, 10) }

// MirrorIndex is the prompt index as the view's socket sends it: every prompt
// the host's transcript holds (the newest it kept), each with its bubble's
// element id, and how many earlier ones the host left out.
type MirrorIndex struct {
	Items []MirrorIndexItem `json:"items"`
	More  int               `json:"more"`
}

// MirrorIndexItem is one prompt: its bubble's id and its first line.
type MirrorIndexItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// NewMirrorIndex is refs as the view's index.
func NewMirrorIndex(refs []cctranscript.PromptRef, more int) *MirrorIndex {
	idx := &MirrorIndex{Items: make([]MirrorIndexItem, 0, len(refs)), More: max(more, 0)}
	for _, r := range refs {
		idx.Items = append(idx.Items, MirrorIndexItem{ID: MirrorPromptID(r.Line), Text: r.Text})
	}
	return idx
}

// Reset forgets every drawn call (the view starts over).
func (t *MirrorTranscript) Reset() {
	t.calls, t.order, t.asks = nil, nil, nil
	t.states, t.groupOf, t.groups, t.run, t.runGrp = nil, nil, nil, nil, ""
	t.Resumed = false
	t.usage, t.fresh = nil, false
}

// endRun records that something other than a tool row was drawn: the next
// call starts a run of its own.
func (t *MirrorTranscript) endRun() { t.run, t.runGrp = nil, "" }

// forget drops a call evicted past maxDrawnCalls from every map that names it.
func (t *MirrorTranscript) forget(toolID string) {
	delete(t.calls, toolID)
	delete(t.asks, toolID)
	delete(t.states, toolID)
	if gid, ok := t.groupOf[toolID]; ok {
		delete(t.groupOf, toolID)
		rows := t.groups[gid]
		for i, r := range rows {
			if r == toolID {
				rows = append(rows[:i:i], rows[i+1:]...)
				break
			}
		}
		if len(rows) == 0 {
			delete(t.groups, gid)
		} else {
			t.groups[gid] = rows
		}
	}
}

// groupStateOps redraws group gid's summary and state from its rows' states.
func (t *MirrorTranscript) groupStateOps(gid string) []MirrorOp {
	rows := t.groups[gid]
	states := make([]string, 0, len(rows))
	for _, r := range rows {
		states = append(states, t.states[r])
	}
	st := toolGroupState(states)
	return []MirrorOp{
		{Op: "replace", ID: gid + "-s", HTML: render(toolGroupSummary(gid, len(rows), st))},
		{Op: "attr", ID: gid, Name: "data-tool-status", Value: st},
	}
}

// appendRow draws a NEW tool row at the end of the conversation, by the
// grouping rule (tool_groups.go): the first of a run is a plain row; the second
// wraps the first in a group and joins it; every later one joins the group, and
// the group's count and state are redrawn.
func (t *MirrorTranscript) appendRow(toolID, state string, row g.Node) []MirrorOp {
	id := toolRowID(toolID)
	t.states[toolID] = state
	html := render(row)
	switch len(t.run) {
	case 0:
		t.run = []string{toolID}
		return []MirrorOp{{Op: "append", ID: id, HTML: html}}
	case 1:
		first := t.run[0]
		gid := toolGroupID(first)
		t.run = append(t.run, toolID)
		t.runGrp = gid
		t.groups[gid] = []string{first, toolID}
		t.groupOf[first], t.groupOf[toolID] = gid, gid
		return []MirrorOp{
			{Op: "wrap", ID: toolRowID(first), HTML: render(toolGroup(gid, []string{t.states[first], state}))},
			{Op: "append", Into: gid + "-b", ID: id, HTML: html},
		}
	default:
		gid := t.runGrp
		t.run = append(t.run, toolID)
		t.groups[gid] = append(t.groups[gid], toolID)
		t.groupOf[toolID] = gid
		return append([]MirrorOp{{Op: "append", Into: gid + "-b", ID: id, HTML: html}}, t.groupStateOps(gid)...)
	}
}

// ContextView is the header's context-usage readout (mirrorViewBatch.Context):
// the latest turn's context, as a count ("124k context").
type ContextView struct {
	Tokens int64  `json:"tokens"`
	Label  string `json:"label"`
}

// TakeContext is the context readout when Ops has drawn a newer one since the
// last call, else nil — so a batch carries it only when it changed.
func (t *MirrorTranscript) TakeContext() *ContextView {
	if !t.fresh || t.usage == nil {
		return nil
	}
	t.fresh = false
	return &ContextView{Tokens: t.usage.Tokens, Label: tokenCount(t.usage.Tokens) + " context"}
}

// tokenCount is n tokens, short: 950, 12k, 124k, 1M, 1.2M.
func tokenCount(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_500: // past it, rounding to k would print "1000k"
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	default:
		s := strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + "M"
	}
}

// toolRowID is a tool row's element id: the tool id hashed, since it is the
// host's text.
func toolRowID(toolID string) string {
	sum := sha256.Sum256([]byte(toolID))
	return "mt-" + hex.EncodeToString(sum[:8])
}

func render(n g.Node) string {
	var b bytes.Buffer
	_ = n.Render(&b)
	return b.String()
}

// Ops renders events as the page's DOM operations, in order.
func (t *MirrorTranscript) Ops(events []cctranscript.Event) []MirrorOp {
	if t.calls == nil {
		t.calls = map[string]ChatLine{}
	}
	if t.states == nil {
		t.states, t.groupOf, t.groups = map[string]string{}, map[string]string{}, map[string][]string{}
	}
	var ops []MirrorOp
	for _, ev := range events {
		if ev.Usage != nil {
			t.usage, t.fresh = ev.Usage, true
		}
		switch ev.Kind {
		case cctranscript.KindPrompt:
			// The bubble's id is the prompt's place in its transcript, the id the
			// prompt index names it by (MirrorPromptID). A host that sends no Line
			// gets none here, and the message index gives the bubble a
			// page-unique one (chatIndexScript).
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("user",
				g.If(ev.Line > 0, ID(MirrorPromptID(ev.Line))), g.Attr("data-md-rendered", ""), promptBody(ev.Text)))})
		case cctranscript.KindText:
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("assistant", g.Attr("data-md-rendered", ""), renderMarkdown(ev.Text)))})
		case cctranscript.KindError:
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(Div(
				g.Attr("data-mirror-error", ""),
				Class("max-w-[85%] self-start break-words rounded-2xl bg-danger/15 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-danger/40"),
				g.Text("⚠ "+ev.Text)))})
		case cctranscript.KindNotice:
			// A notice is drawn in the tool rows' frame but is not a call: it ends
			// a run like text does.
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(noticeRow(ev))})
		case cctranscript.KindToolCall:
			if ev.Ask != nil && len(ev.Ask.Questions) > 0 {
				ops = append(ops, t.askCall(ev)...)
				continue
			}
			call := ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool, ToolTarget: ev.Target, Content: ev.Input}
			id := toolRowID(ev.ToolID)
			if _, seen := t.calls[ev.ToolID]; seen {
				// The same call again: redrawn where it is, run and group untouched.
				t.calls[ev.ToolID] = call
				ops = append(ops, MirrorOp{Op: "replace", ID: id, HTML: render(toolChip(call, nil, id))})
				t.states[ev.ToolID] = toolStateRunning
				if gid, ok := t.groupOf[ev.ToolID]; ok {
					ops = append(ops, t.groupStateOps(gid)...)
				}
				continue
			}
			t.order = append(t.order, ev.ToolID)
			if len(t.order) > maxDrawnCalls {
				t.forget(t.order[0])
				t.order = t.order[1:]
			}
			t.calls[ev.ToolID] = call
			ops = append(ops, t.appendRow(ev.ToolID, toolStateRunning, toolChip(call, nil, id))...)
		case cctranscript.KindToolResult:
			if qs, isAsk := t.asks[ev.ToolID]; isAsk || (ev.Ask != nil && len(ev.Ask.Questions) > 0) {
				ops = append(ops, t.askResult(ev, qs, isAsk)...)
				continue
			}
			result := ChatLine{Kind: "tool_result", ToolID: ev.ToolID, ToolOK: ev.OK, Content: ev.Output}
			id := toolRowID(ev.ToolID)
			call, ok := t.calls[ev.ToolID]
			if !ok && t.Resumed {
				// Its call was drawn by the socket before this one: the result is
				// put INTO the row where the page has it (a `result`: its status and
				// output; the row keeps the name, target and args only the earlier
				// socket knew), appended only if the page has none, and no run is
				// started from it — see Resumed.
				ops = append(ops, MirrorOp{Op: "result", ID: id,
					HTML: render(toolChip(ChatLine{Kind: "tool_call", ToolID: ev.ToolID}, &result, id))})
				continue
			}
			if !ok {
				// Its call fell before the backfill window: a row of its own, drawn
				// at the end like any new row (and bounded with the calls).
				t.order = append(t.order, ev.ToolID)
				if len(t.order) > maxDrawnCalls {
					t.forget(t.order[0])
					t.order = t.order[1:]
				}
				ops = append(ops, t.appendRow(ev.ToolID, toolChipState(&result),
					toolChip(ChatLine{Kind: "tool_call", ToolID: ev.ToolID}, &result, id))...)
				continue
			}
			ops = append(ops, MirrorOp{Op: "replace", ID: id, HTML: render(toolChip(call, &result, id))})
			t.states[ev.ToolID] = toolChipState(&result)
			if gid, ok := t.groupOf[ev.ToolID]; ok {
				ops = append(ops, t.groupStateOps(gid)...)
			}
		}
	}
	return ops
}

// track records toolID as drawn (bounded with every other call).
func (t *MirrorTranscript) track(toolID string) {
	if _, seen := t.calls[toolID]; seen {
		return
	}
	t.order = append(t.order, toolID)
	if len(t.order) > maxDrawnCalls {
		t.forget(t.order[0])
		t.order = t.order[1:]
	}
}

// askCall draws an AskUserQuestion call as its question card: NOT a tool row,
// so it ends a run (a card is never folded into a collapsed group) and is
// never grouped itself.
func (t *MirrorTranscript) askCall(ev cctranscript.Event) []MirrorOp {
	if t.asks == nil {
		t.asks = map[string][]askq.Question{}
	}
	id := toolRowID(ev.ToolID)
	html := render(MirrorAskCall(id, ev.Ask.Questions))
	_, seen := t.calls[ev.ToolID]
	t.track(ev.ToolID)
	t.calls[ev.ToolID] = ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool}
	t.asks[ev.ToolID] = ev.Ask.Questions
	t.endRun()
	if seen {
		return []MirrorOp{{Op: "replace", ID: id, HTML: html}}
	}
	return []MirrorOp{{Op: "append", ID: id, HTML: html}}
}

// askResult draws an AskUserQuestion call's result as its card, read-only: the
// questions and answers the result records (Event.Ask, from the transcript's
// toolUseResult), else the call's own questions with no answer and the
// result's text (a picker dismissed in the terminal). In place of the call's
// card when it is on the page — a resumed socket's included: a "replace" of a
// row the page does not have is an append.
func (t *MirrorTranscript) askResult(ev cctranscript.Event, callQs []askq.Question, drawn bool) []MirrorOp {
	qs, answers, note := callQs, map[string]string(nil), ""
	if ev.Ask != nil && len(ev.Ask.Questions) > 0 {
		qs, answers = ev.Ask.Questions, ev.Ask.Answers
	}
	if len(answers) == 0 {
		note = ev.Output
	}
	id := toolRowID(ev.ToolID)
	html := render(MirrorAskResult(id, qs, answers, note))
	if drawn || t.Resumed {
		return []MirrorOp{{Op: "replace", ID: id, HTML: html}}
	}
	if t.asks == nil {
		t.asks = map[string][]askq.Question{}
	}
	t.track(ev.ToolID)
	t.calls[ev.ToolID] = ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: askq.ToolName}
	t.asks[ev.ToolID] = qs
	t.endRun()
	return []MirrorOp{{Op: "append", ID: id, HTML: html}}
}

// noticeRow draws a harness notice (cctranscript.KindNotice) as a compact
// collapsed row in the tool rows' frame — never as the human's bubble: an icon
// for its source, its one-line summary, and a task's status; expanding shows
// the detail (a task's result, a local command's output).
//
// ⚠ ONLY THE MIRRORED VIEW DRAWS ONE. A dispatched agent's chat is muster's own
// persisted messages plus ccd's reply stream, which carries a turn's text and
// tool calls and nothing the harness injects, so no notice ever reaches it.
func noticeRow(ev cctranscript.Event) g.Node {
	icon, label := "🔔", "task"
	if ev.Notice == cctranscript.NoticeCommandOutput {
		icon, label = "▸", "output"
	}
	// The status as the harness wrote it, drawn the tool rows' way: a known
	// good one ✓, a known bad one ✗, anything else by name.
	var status g.Node
	switch ev.Status {
	case "":
	case "completed", "success":
		status = Span(Class("ml-auto shrink-0 font-semibold text-accent"), g.Attr("aria-label", "status: "+ev.Status), g.Text("✓"))
	case "failed", "error":
		status = Span(Class("ml-auto shrink-0 font-semibold text-st-error-fg"), g.Attr("aria-label", "status: "+ev.Status), g.Text("✗"))
	default:
		status = Span(Class("ml-auto shrink-0 text-muted"), g.Text(ev.Status))
	}
	var body g.Node
	if strings.TrimSpace(ev.Output) != "" && ev.Output != ev.Text {
		body = Div(Class("flex flex-col gap-1 px-3 pb-2 pt-0"), toolPre("detail", ev.Output))
	}
	return Details(
		g.Attr("data-notice-row", ev.Notice),
		g.If(ev.Status != "", g.Attr("data-notice-status", ev.Status)),
		Class("w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line"),
		Summary(
			Class("flex min-h-[44px] min-w-0 cursor-pointer select-none items-center gap-2 px-3 py-2 font-medium text-fg2 hover:text-fg"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text(icon)),
			Span(Class("shrink-0 font-mono text-muted"), g.Text(label)),
			Span(g.Attr("data-notice-text", ""), Class("min-w-0 truncate"), g.Text(ev.Text)),
			status,
		),
		body,
	)
}

// mirrorViewScript connects the transcript socket and applies its batches, and
// reconnects resuming from the last offset. The composer is its own script
// (mirrorComposerScript), shared with the Terminal view.
//
// Every node it inserts is server-rendered HTML (MirrorTranscript.Ops), whose
// text was escaped by gomponents and renderMarkdown; the script adds none of
// its own from transcript text.
func mirrorViewScript() g.Node {
	return Script(g.Raw(`
(function () {
  var log = document.getElementById('mv-log');
  if (!log) return;
  var statusEl = document.getElementById('mv-status');
  var errEl = document.getElementById('mv-error');
  var errText = document.getElementById('mv-error-text');
  var working = document.getElementById('mv-working');
  var ctxEl = document.getElementById('mv-context');
  var gen = '', offset = 0, retry = 0, closed = false, sock = null;

  function showError(msg) { errText.textContent = msg; errEl.hidden = false; }
  function clearError() { errEl.hidden = true; errText.textContent = ''; }
  function nearBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  function frag(html) { var t = document.createElement('template'); t.innerHTML = html; return t.content; }

  function apply(m) {
    var stick = nearBottom() || m.reset;
    // The prompt index (chatIndexScript reads it): a Reset starts a transcript
    // over, so its index is gone until the host sends the new one — and a host
    // that predates it never does.
    if (m.reset) { log.innerHTML = ''; window.muMirrorIndex = null; ctxEl.hidden = true; ctxEl.textContent = ''; }
    if (m.index) { window.muMirrorIndex = m.index; }
    if (m.reset || m.index) document.dispatchEvent(new CustomEvent('muster:chat-index'));
    // Context usage: only a batch whose usage changed carries it; an older
    // muster or host sends none and the readout stays hidden.
    if (m.context && m.context.label) {
      ctxEl.textContent = m.context.label;
      ctxEl.hidden = false;
    }
    (m.ops || []).forEach(function (op) {
      // A group's state (tool_groups.go): one attribute, on the group.
      if (op.op === 'attr') {
        var at = op.id && document.getElementById(op.id);
        if (at) at.setAttribute(op.name, op.value);
        return;
      }
      // The second call of a run: the group takes the first row's place and the
      // row moves into its body (the call itself is the next op, appended into it).
      if (op.op === 'wrap' && op.id) {
        var first = document.getElementById(op.id);
        if (first) {
          var grp = frag(op.html).firstElementChild;
          first.replaceWith(grp);
          grp.querySelector('[data-tool-group-body]').appendChild(first);
          return;
        }
      }
      if (op.op === 'result' && op.id) {
        var row = document.getElementById(op.id);
        if (row) {
          var got = frag(op.html).firstElementChild;
          row.setAttribute('data-tool-status', got.getAttribute('data-tool-status') || '');
          var st = row.querySelector(':scope > summary [aria-label^="status:"]');
          var nst = got.querySelector(':scope > summary [aria-label^="status:"]');
          if (st && nst) st.replaceWith(nst);
          var body = row.querySelector(':scope > div'), nbody = got.querySelector(':scope > div');
          if (body && nbody) while (nbody.firstChild) body.appendChild(nbody.firstChild);
          if (window.musterToolGroups) window.musterToolGroups.refresh(row);
          return;
        }
      }
      if (op.op === 'replace' && op.id) {
        var old = document.getElementById(op.id);
        if (old) {
          var f = frag(op.html), el = f.firstElementChild;
          if (el && old.open) el.open = true; // keep a row the reader expanded
          old.replaceWith(f);
          // A row redrawn inside a group: the group's count and state follow it
          // (a resumed socket cannot redraw a group it never drew).
          if (el && window.musterToolGroups) window.musterToolGroups.refresh(el);
          return;
        }
      }
      if (op.id) { var dup = document.getElementById(op.id); if (dup) dup.remove(); }
      // Into a group's body when it is on the page; else at the end, as before.
      ((op.into && document.getElementById(op.into)) || log).appendChild(frag(op.html));
    });
    if (m.gen) gen = m.gen;
    if (typeof m.offset === 'number') offset = m.offset;
    working.hidden = !m.busy;
    if (!log.firstElementChild) {
      log.innerHTML = '<p data-mirror-empty class="m-auto text-sm text-muted">No conversation yet.</p>';
    } else {
      var empty = log.querySelector('[data-mirror-empty]'); if (empty && log.children.length > 1) empty.remove();
    }
    if (stick) log.scrollTop = log.scrollHeight;
  }

  function connect() {
    var base = log.getAttribute('data-ws');
    var url = (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + base +
      (gen ? '&gen=' + encodeURIComponent(gen) + '&offset=' + offset : '');
    statusEl.textContent = 'Connecting…';
    sock = new WebSocket(url);
    sock.onopen = function () { retry = 0; };
    sock.onmessage = function (ev) {
      var m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (m.type === 'batch') { clearError(); statusEl.textContent = 'Live'; apply(m); }
      else if (m.type === 'error') { showError(m.message || 'The view ended.'); }
    };
    sock.onclose = function () {
      statusEl.textContent = 'Disconnected — reconnecting…';
      working.hidden = true;
      if (closed) return;
      retry = Math.min(retry + 1, 6);
      setTimeout(connect, 500 * Math.pow(2, retry - 1));
    };
  }
  // The log's box shrinks when the keyboard opens or the composer grows: a
  // reader at the end stays at the end, rather than the last message sliding
  // under the box's bottom edge.
  var atEnd = true;
  log.addEventListener('scroll', function () { atEnd = nearBottom(); }, { passive: true });
  if (typeof ResizeObserver !== 'undefined') {
    new ResizeObserver(function () { if (atEnd) log.scrollTop = log.scrollHeight; }).observe(log);
  }
  window.addEventListener('pagehide', function () { closed = true; if (sock) sock.close(); });
  connect();

})();
`))
}
