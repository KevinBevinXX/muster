package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/cctranscript"
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
	// Queue is the composer's send queue (ComposerView.Queue); nil for none.
	Queue *ComposerQueue
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
		Notice: v.Notice, NoticeOK: v.NoticeOK, Nonce: v.Nonce, Dictate: v.Dictate, Queue: v.Queue,
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
				Link(Rel("stylesheet"), Href("/static/app.css")),
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
						P(ID("mv-status"), Class("truncate text-xs text-muted"), g.Attr("aria-live", "polite"), g.Text("Connecting…")),
					),
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

// MirrorOp is one DOM operation the rendered view's page applies: append a node
// to the conversation, or replace the node with id (a running tool row whose
// result arrived).
type MirrorOp struct {
	Op   string `json:"op"` // "append" | "replace"
	ID   string `json:"id,omitempty"`
	HTML string `json:"html"`
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
}

const maxDrawnCalls = 2048

// Reset forgets every drawn call (the view starts over).
func (t *MirrorTranscript) Reset() { t.calls, t.order = nil, nil }

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
	var ops []MirrorOp
	for _, ev := range events {
		switch ev.Kind {
		case cctranscript.KindPrompt:
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("user", g.Attr("data-md-rendered", ""), renderMarkdown(ev.Text)))})
		case cctranscript.KindText:
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("assistant", g.Attr("data-md-rendered", ""), renderMarkdown(ev.Text)))})
		case cctranscript.KindError:
			ops = append(ops, MirrorOp{Op: "append", HTML: render(Div(
				g.Attr("data-mirror-error", ""),
				Class("max-w-[85%] self-start break-words rounded-2xl bg-danger/15 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-danger/40"),
				g.Text("⚠ "+ev.Text)))})
		case cctranscript.KindToolCall:
			call := ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool, ToolTarget: ev.Target, Content: ev.Input}
			id := toolRowID(ev.ToolID)
			if _, seen := t.calls[ev.ToolID]; !seen {
				t.order = append(t.order, ev.ToolID)
				if len(t.order) > maxDrawnCalls {
					delete(t.calls, t.order[0])
					t.order = t.order[1:]
				}
			}
			t.calls[ev.ToolID] = call
			ops = append(ops, MirrorOp{Op: "append", ID: id, HTML: render(toolChip(call, nil, id))})
		case cctranscript.KindToolResult:
			result := ChatLine{Kind: "tool_result", ToolID: ev.ToolID, ToolOK: ev.OK, Content: ev.Output}
			id := toolRowID(ev.ToolID)
			call, ok := t.calls[ev.ToolID]
			if !ok {
				// Its call fell before the backfill window: a row of its own.
				ops = append(ops, MirrorOp{Op: "append", ID: id, HTML: render(toolChip(ChatLine{Kind: "tool_call", ToolID: ev.ToolID}, &result, id))})
				continue
			}
			ops = append(ops, MirrorOp{Op: "replace", ID: id, HTML: render(toolChip(call, &result, id))})
		}
	}
	return ops
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
  var gen = '', offset = 0, retry = 0, closed = false, sock = null;

  function showError(msg) { errText.textContent = msg; errEl.hidden = false; }
  function clearError() { errEl.hidden = true; errText.textContent = ''; }
  function nearBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  function frag(html) { var t = document.createElement('template'); t.innerHTML = html; return t.content; }

  function apply(m) {
    var stick = nearBottom() || m.reset;
    if (m.reset) { log.innerHTML = ''; }
    (m.ops || []).forEach(function (op) {
      if (op.op === 'replace' && op.id) {
        var old = document.getElementById(op.id);
        if (old) {
          var f = frag(op.html), el = f.firstElementChild;
          if (el && old.open) el.open = true; // keep a row the reader expanded
          old.replaceWith(f);
          return;
        }
      }
      if (op.id) { var dup = document.getElementById(op.id); if (dup) dup.remove(); }
      log.appendChild(frag(op.html));
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
