package ui

import (
	"encoding/hex"
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/termsize"
	"github.com/ZacxDev/muster/web"
)

// The vendored terminal emulator, served from /static/vendor/ like htmx.
//
// Provenance (byte-identical to the npm tarballs, so a diff against upstream
// is the audit):
//
//   - xterm.js, xterm.css — @xterm/xterm 5.5.0 (lib/xterm.js, css/xterm.css).
//     MIT. Its UMD build defines the global `Terminal`.
//   - addon-fit.js — @xterm/addon-fit 0.10.0 (lib/addon-fit.js). MIT. Defines
//     `FitAddon.FitAddon`.
//
// The trailing sourceMappingURL comments point at .map files that are not
// vendored; a browser's devtools 404 on them and nothing else asks.
var (
	xtermJS    = web.Asset("/static/vendor/xterm.js")
	xtermCSS   = web.Asset("/static/vendor/xterm.css")
	xtermFitJS = web.Asset("/static/vendor/addon-fit.js")
)

// QuickKey is one button on the terminal's quick-key bar: a key a phone's
// keyboard lacks, and the exact bytes it types.
type QuickKey struct {
	ID    string // data-key, the stable handle a test addresses
	Label string
	Aria  string
	Seq   string // the bytes sent, as typed by a VT100-style terminal
}

// QuickKeys is the bar, in display order. The sequences are what xterm itself
// sends for the same keys in normal cursor mode; tmux parses them either way.
// The digits answer the TUI's numbered permission prompts, and Shift-Tab
// cycles its permission mode.
var QuickKeys = []QuickKey{
	{ID: "esc", Label: "Esc", Aria: "Escape", Seq: "\x1b"},
	{ID: "tab", Label: "Tab", Aria: "Tab", Seq: "\t"},
	{ID: "stab", Label: "⇧Tab", Aria: "Shift Tab", Seq: "\x1b[Z"},
	{ID: "ctrlc", Label: "^C", Aria: "Control C", Seq: "\x03"},
	{ID: "enter", Label: "⏎", Aria: "Enter", Seq: "\r"},
	{ID: "1", Label: "1", Aria: "1", Seq: "1"},
	{ID: "up", Label: "↑", Aria: "Up arrow", Seq: "\x1b[A"},
	{ID: "down", Label: "↓", Aria: "Down arrow", Seq: "\x1b[B"},
	{ID: "left", Label: "←", Aria: "Left arrow", Seq: "\x1b[D"},
	{ID: "right", Label: "→", Aria: "Right arrow", Seq: "\x1b[C"},
	{ID: "2", Label: "2", Aria: "2", Seq: "2"},
	{ID: "3", Label: "3", Aria: "3", Seq: "3"},
}

// AgentTerminalView is the data the terminal page renders.
type AgentTerminalView struct {
	Name        string
	DisplayName string
	// Nav is the navigation the page draws: an overlay sidebar (sidebarIn's
	// overlay — the terminal's width is the far side's geometry, so no rail
	// takes from it) and its hamburger, framed by the view swap or not
	// (nav_layout.go says why).
	Nav NavChrome
	// Unavailable, when set, is why this deployment cannot attach a terminal;
	// the page says so instead of opening a socket that would be refused.
	Unavailable string
	// BackHref is where the back arrow goes ("" = the agent's own page). A
	// mirrored session has no agent page — its terminal is its page — so it goes
	// back to the list.
	BackHref string
	// Badge is a short label beside the title ("Mirrored · <host>"); "" = none.
	Badge string
	// BadgeHost and BadgeHostIcon are the mirrored host the badge names and its
	// icon key (HostIconFor): the badge leads with the icon its cards draw.
	BadgeHost, BadgeHostIcon string
	// RawName, when set, is shown small and muted after the title, and as its
	// tooltip (mirrorSessionTitle): a mirrored session titled by its host's
	// label ("mango:1") keeps its real tmux session name findable. "" = the
	// title alone.
	RawName string
	// TitleQualifier, when set, is drawn right after the title as part of it
	// (" · laptop") and added to <title>: a mirrored entry whose name another
	// listed entry shares (mirror.SessionView.Qualifier). "" = the title alone.
	TitleQualifier string
	// WSPath is the socket the page opens, WITHOUT its query ("" = the agent's
	// /agents/{name}/terminal/ws). A mirrored session's is
	// /mirror/terminal/ws?host=…&session=…, and the page appends mode/cols/rows.
	WSPath string
	// ReadOnlyOnly removes the Interactive switch: the far side cannot be typed
	// into (a mirrored host that did not advertise -interactive). The server
	// refuses an interactive attach regardless; this keeps the page from
	// offering one.
	ReadOnlyOnly bool
	// InteractiveNote, when set, is shown as the switch's tooltip and beside it
	// while interactive: what typing into this terminal really reaches.
	InteractiveNote string
	// RenderedHref, when set, draws the Rendered | Terminal switch: the session
	// also has a rendered view (a mirrored Claude Code pane), one tap away.
	RenderedHref string
	// StepUp, when set, puts the Interactive switch behind a FRESH sign-in
	// (internal/stepup): a mirrored session. nil = the switch attaches at once
	// (an agent pod's terminal).
	StepUp *StepUpView
	// Composer, when set, draws the message composer under the terminal: a
	// mirrored session of an -interactive host (mirror_compose.go). It needs
	// StepUp — a send refused as stale asks through the same dialog. nil = no
	// composer (an agent pod's terminal, a read-only host).
	Composer *MirrorComposerView
	// ScrollForm, when set, turns on drag-to-scroll: a vertical drag on the
	// terminal (and the mouse wheel) scrolls the session's pane through its
	// tmux history, by POSTing ScrollForm plus `lines` to
	// MirrorTerminalScrollPath. A host that cannot scroll (an older
	// muster-mirror, a read-only host) leaves it "", and the page then has no
	// gesture handlers at all — touch pans #term-scroll exactly as before.
	ScrollForm string
}

// MirrorTerminalScrollPath is where the Terminal view's drag-to-scroll posts
// (internal/api's handleMirrorTerminalScroll).
const MirrorTerminalScrollPath = "/mirror/terminal/scroll"

// ScrollGaveUpNote is what the Terminal view says when scrolls stopped reaching
// the host and the page dropped what it had not sent.
const ScrollGaveUpNote = "Scrolling is not reaching the host. Drag again to retry."

// ScrollRefusalHeader names, on a scroll's refusal, WHICH refusal it is (one
// of the ScrollRefusal* codes), so the page acts on a code rather than on a
// status a proxy could also answer. Only ScrollRefusalUnsupported stops the
// gesture; every other one shows its note, keeps the gesture wired and clears
// once a scroll lands.
const ScrollRefusalHeader = "X-Muster-Scroll-Refusal"

// The scroll route's refusal codes (ScrollRefusalHeader).
const (
	// ScrollRefusalUnsupported: this host cannot scroll (an older or read-only
	// muster-mirror). The page unwires the gesture; Reconnect wires it again.
	ScrollRefusalUnsupported = "unsupported"
	// ScrollRefusalOffline: the host is not connected now (it is reconnecting).
	ScrollRefusalOffline = "offline"
	// ScrollRefusalMode: the pane is in another tmux mode (a menu, choose-tree).
	ScrollRefusalMode = "mode"
	// ScrollRefusalGone: the session or window is no longer on the host.
	ScrollRefusalGone = "gone"
	// ScrollRefusalRefused: the host refused for another reason.
	ScrollRefusalRefused = "refused"
)

// The notes the page shows for each refusal that keeps the gesture wired.
const (
	ScrollOfflineNote = "Host reconnecting. Drag again once it is back."
	ScrollModeNote    = "The pane is in another mode (e.g. a tmux menu). Close it, then drag again."
	ScrollGoneNote    = "This window is no longer on the host."
	ScrollRefusedNote = "The host refused the scroll. Drag again to retry."
)

// The Live keys switch's words, and the status line's. Exported so a test pins
// the page against the same strings rather than a copy of them.
const (
	LiveKeysOff = "Live keys off"
	LiveKeysOn  = "Live keys on"
	// StatusConnected is the status while live keys are off. It says nothing
	// about sending: the composer can send either way.
	StatusConnected = "Connected"
	StatusLiveKeys  = "Connected · live keys"
	liveKeysTitle   = "Live keys: send your keystrokes and the quick keys straight into the session"
)

// termModeDisplay is the switch's display utility: gone when the far side is
// read-only-only.
func termModeDisplay(readOnlyOnly bool) string {
	if readOnlyOnly {
		return "hidden"
	}
	return "inline-flex"
}

// AgentTerminalPath is the terminal page of an agent.
func AgentTerminalPath(name string) string { return "/agents/" + url.PathEscape(name) + "/terminal" }

// AgentPath is an agent's own page: its chat, the RENDERED view of its work.
func AgentPath(name string) string { return "/agents/" + url.PathEscape(name) }

// AgentSendPath is where a message for an agent is posted as a form: the
// composer on its Terminal page, and its chat form when the page's socket
// cannot carry the message.
func AgentSendPath(name string) string { return "/agents/" + url.PathEscape(name) + "/send" }

// AgentSendFromTerminal is the `from` field of a send that came from the
// agent's Terminal page: where its answer returns the browser to. Anything
// else is the chat page.
const AgentSendFromTerminal = "terminal"

// AgentTerminalPage is a full-height document: a header with the Live keys
// switch, the terminal, the quick-key bar and — for a mirrored session that can
// be sent to — the message composer.
//
// 🔴 THE PAGE NEVER CALLS ITSELF "READ-ONLY". It used to: the switch read
// "Read-only" and the status "Connected · read-only" whenever live keys were
// off — while the composer under the terminal sends messages without the
// switch, so the label told a person they could not send when they could. What
// the switch gates is LIVE KEYS (keystrokes and the quick-key bar typed
// straight into the session), and it says exactly that. A host that genuinely
// cannot be sent to says so ONCE, in its composer's disabled note
// (MirrorReadOnlyNote).
//
// 🔴 IT IS ITS OWN DOCUMENT, NOT A PANEL OF THE AGENT PAGE. The agent page is
// hx-boost'ed and its chat script tears down and re-creates its socket on
// every boosted swap; a terminal living inside that lifecycle would be
// re-attached on every navigation. The links into this page opt out of boost,
// so it is always a real load with its own scripts. The Rendered | Terminal
// switch keeps that: it opens this page in a FRAME over the agent page — its
// own document still — rather than in the agent page's body (view_swap.go).
//
// ⚠ LIVE KEYS OFF — A READ-ONLY ATTACH — IS THE STATE THE PAGE OPENS IN, AND
// THE SERVER AGREES INDEPENDENTLY: the socket is opened with mode=ro until the
// switch is flipped,
// and the server attaches read-only for anything but mode=rw. (The one
// exception is a mirrored host whose Interactive choice this browser session
// remembers while its sign-in is still fresh — and the server re-checks that.)
//
// 🔴 A WIDER FAR SIDE IS SCROLLED, NOT SQUEEZED (internal/termsize). When the
// session's window is wider (or taller) than this page, the far side keeps its
// size and says so in-band; the page renders that size inside #term-scroll,
// which scrolls both ways and can be pinch-zoomed, and offers "Fit width".
func AgentTerminalPage(v AgentTerminalView) g.Node {
	// The page's name as a reader sees it (textplain): its <title>, its header.
	// 🔴 PLAINED EXACTLY ONCE PER SINK. mirrorTerminalTitle (→ mirrorEntryTitle)
	// applies the rule itself, so it takes the RAW name; handing it `title` ran
	// the rule twice, which is not idempotent (`\*\*x\*\*` → "**x**" → "x"),
	// and the tab and the header disagreed. TestTheTerminalTitleAndHeadingAgree.
	raw := displayOr(v.DisplayName, v.Name)
	title := plainOr(v.DisplayName, v.Name)
	back := v.BackHref
	if back == "" {
		back = AgentSessionHref(v.Name, 0)
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Terminal · "+mirrorQualified(title, v.TitleQualifier)+" · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Link(Rel("stylesheet"), Href(xtermCSS)),
				Script(Src(xtermJS)),
				Script(Src(xtermFitJS)),
				overlayNavHead(),
				faroHead(),
			),
			Body(
				Class("flex h-dvh flex-col overflow-hidden bg-bg text-fg antialiased"),
				// The navigation: position:fixed (no room taken in the column,
				// so the terminal keeps its size), an overlay at every width.
				overlayNav(v.Nav, "agents"),
				Header(
					// It WRAPS: with the view switch beside Fit width and the
					// Interactive switch, a phone's width does not hold them all
					// on one line, and a header that cannot wrap pushes the
					// document sideways instead.
					Class("flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-line px-2 py-1.5"),
					overlayNavButton(),
					A(
						ID("term-back"),
						Href(back),
						g.Attr("aria-label", "Back to the agent"),
						Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
						g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
					),
					Div(
						Class("min-w-[6rem] flex-1"),
						H1(Class("truncate text-sm font-semibold"),
							g.If(v.RawName == "" && v.TitleQualifier == "", g.Text(title)),
							// A mirrored entry: its label with the raw session name
							// beside it, and/or its qualifier — drawn like its card.
							g.If(v.RawName != "" || v.TitleQualifier != "", mirrorTerminalTitle(raw, v.RawName, v.TitleQualifier)),
							g.If(v.Badge != "", Span(g.Attr("data-term-badge", ""), Class("ml-2"), mirrorBadge(v.Badge, v.BadgeHost, v.BadgeHostIcon)))),
						P(ID("term-status"), Class("truncate text-xs text-muted"), g.Attr("aria-live", "polite"), g.Text("Not connected")),
					),
					// Fit width: shown only while the far side is wider than this
					// page. It scales the VIEW (the far side's size is untouched).
					Button(
						ID("term-fitw"),
						Type("button"),
						g.Attr("aria-pressed", "false"),
						g.Attr("hidden", ""),
						g.Attr("title", "Scale the terminal to the screen's width (the session keeps its size)"),
						Class("press min-h-[44px] shrink-0 items-center rounded-lg bg-s2 px-3 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition aria-pressed:bg-accent aria-pressed:text-on-accent"),
						g.Text("Fit width"),
					),
					// The switch. aria-checked is the LIVE-KEYS state, so
					// "false" — keys off, a read-only attach — is what the page
					// opens with. Its label names what it gates, never "read-only":
					// the composer sends with it off (see AgentTerminalPage).
					Button(
						ID("term-mode"),
						Type("button"),
						g.Attr("role", "switch"),
						g.Attr("aria-checked", "false"),
						g.Attr("aria-label", "Live keys: type keystrokes and the quick keys straight into the session"),
						g.If(v.Unavailable != "", g.Attr("disabled", "")),
						// ⚠ The `hidden` CLASS, not the attribute: an author display
						// utility (inline-flex) beats the UA's [hidden] rule.
						g.If(v.ReadOnlyOnly, g.Attr("hidden", "")),
						g.Attr("title", displayOr(v.InteractiveNote, liveKeysTitle)),
						g.If(v.InteractiveNote != "", g.Attr("data-interactive-note", v.InteractiveNote)),
						Class(termModeDisplay(v.ReadOnlyOnly)+" press min-h-[44px] shrink-0 items-center gap-2 rounded-lg bg-s2 px-3 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition aria-checked:bg-danger aria-checked:text-on-accent disabled:opacity-40"),
						Span(ID("term-mode-label"), g.Text(LiveKeysOff)),
					),
					g.If(v.RenderedHref != "", viewToggle(false, v.RenderedHref, "")),
				),
				Div(
					ID("term-error"),
					g.Attr("role", "alert"),
					g.If(v.Unavailable == "", g.Attr("hidden", "")),
					Class("border-b border-line bg-danger/15 px-3 py-2 text-xs text-fg break-words"),
					Span(ID("term-error-text"), g.Text(v.Unavailable)),
					Button(
						ID("term-reconnect"),
						Type("button"),
						g.Attr("hidden", ""),
						Class("press ml-2 inline-flex min-h-[32px] items-center rounded-md bg-s2 px-2 text-xs font-semibold text-fg ring-1 ring-inset ring-line"),
						g.Text("Reconnect"),
					),
				),
				Main(
					Class("relative min-h-0 flex-1 overflow-hidden bg-black"),
					// #term-scroll is the viewport onto the terminal: the size the
					// page has. It scrolls when the far side is bigger.
					Div(
						ID("term-scroll"),
						Class("absolute inset-0 overflow-auto px-1 py-1"),
						// #term-sizer carries the SCALED extent in Fit width, so
						// the scroll area matches what is drawn.
						Div(
							ID("term-sizer"),
							Class("h-full w-full"),
							Div(
								ID("term"),
								g.Attr("data-agent", v.Name),
								g.If(v.WSPath != "", g.Attr("data-ws", v.WSPath)),
								g.If(v.Unavailable != "", g.Attr("data-unavailable", "")),
								g.Attr("data-size-osc", strconv.Itoa(termsize.ReportOSC)),
								stepUpAttrs(v.StepUp),
								g.If(v.ScrollForm != "", g.Group([]g.Node{
									g.Attr("data-scroll", MirrorTerminalScrollPath),
									g.Attr("data-scroll-form", v.ScrollForm),
								})),
								Class("h-full w-full"),
							),
						),
					),
					// Drag-to-scroll's note: shown when scrolls stopped reaching
					// the host and the page gave up on them (see the script). Only
					// a page that can scroll has it.
					g.If(v.ScrollForm != "", P(
						ID("term-scroll-note"),
						g.Attr("role", "status"),
						g.Attr("hidden", ""),
						Class("pointer-events-none absolute inset-x-2 top-2 z-10 rounded-md bg-s2 px-2 py-1 text-xs text-fg ring-1 ring-inset ring-line"),
						g.Text(ScrollGaveUpNote),
					)),
				),
				quickKeyBar(v.Composer == nil),
				terminalComposer(v.Composer),
				confirmDialog(v.StepUp),
				g.If(v.StepUp != nil, stepUpScript()),
				agentTerminalScript(),
				g.If(v.Composer != nil, mirrorComposerScript()),
				overlayNavScript(v.Nav),
				pwaChrome(),
			),
		),
	)
}

// terminalComposer is the page's composer, nothing for nil.
func terminalComposer(c *MirrorComposerView) g.Node {
	if c == nil {
		return nil
	}
	return mirrorComposer(*c)
}

// quickKeyBar renders QuickKeys as a two-row grid that fits a phone's width
// without scrolling. The buttons start DISABLED: the page opens with live keys
// off.
//
// last says the bar is the bottom of the page, and so carries the safe-area
// inset; when a composer follows, the composer carries it instead.
func quickKeyBar(last bool) g.Node {
	pad := "pb-2"
	if last {
		pad = "pb-[max(0.5rem,env(safe-area-inset-bottom))]"
	}
	keys := make([]g.Node, 0, len(QuickKeys))
	for _, k := range QuickKeys {
		keys = append(keys, Button(
			Type("button"),
			g.Attr("data-key", k.ID),
			g.Attr("data-seq", hex.EncodeToString([]byte(k.Seq))),
			g.Attr("aria-label", k.Aria),
			g.Attr("disabled", ""),
			Class("press inline-flex min-h-[44px] min-w-0 items-center justify-center rounded-md bg-s2 font-mono text-sm font-semibold text-fg ring-1 ring-inset ring-line transition hover:bg-s3 disabled:opacity-40"),
			g.Text(k.Label),
		))
	}
	return Nav(
		ID("term-keys"),
		g.Attr("aria-label", "Terminal keys"),
		Class("grid grid-cols-6 gap-1 border-t border-line bg-bg px-2 pt-2 "+pad),
		g.Group(keys),
	)
}

// agentTerminalScript wires the terminal to its socket. See internal/api's
// terminal.go for the wire, internal/termsize for the size report, and
// internal/stepup for the confirm.
//
// THE SIZE POLICY ON THIS SIDE (internal/termsize is the authority; this only
// avoids asking for what the far side would refuse):
//
//   - `remote` is the client size the far side REPORTED for this attach
//     (null: it reports none — an older far side — and the page fits its
//     container exactly as it always did).
//   - When the container's fit COVERS remote in both dimensions, the page fits
//     the container and sends that size (a desktop: the session grows to it, as
//     before). Otherwise it renders remote's size in the scroll area and sends
//     NOTHING: a phone never shrinks the session's window.
//   - 🔴 A PINCH IS NOT A RESIZE. visualViewport fires `resize` when the page is
//     zoomed; that changes the scale, not the layout, so it neither refits nor
//     sends. A real viewport change (a rotation, the on-screen keyboard) changes
//     the layout box, which the ResizeObserver sees. And a size is only ever
//     sent when it differs from the last one sent.
func agentTerminalScript() g.Node {
	return Script(g.Raw(`
(function () {
  var host = document.getElementById('term');
  if (!host || typeof Terminal === 'undefined' || typeof FitAddon === 'undefined') {
    var e = document.getElementById('term-error');
    if (e) { e.hidden = false; document.getElementById('term-error-text').textContent = 'The terminal emulator did not load.'; }
    return;
  }
  var agent = host.getAttribute('data-agent');
  var unavailable = host.hasAttribute('data-unavailable');
  var scrollEl = document.getElementById('term-scroll');
  var sizer = document.getElementById('term-sizer');
  var statusEl = document.getElementById('term-status');
  var errEl = document.getElementById('term-error');
  var errText = document.getElementById('term-error-text');
  var reconnectBtn = document.getElementById('term-reconnect');
  var modeBtn = document.getElementById('term-mode');
  var modeLabel = document.getElementById('term-mode-label');
  var fitwBtn = document.getElementById('term-fitw');
  var keys = document.querySelectorAll('#term-keys [data-key]');

  var term = new Terminal({
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    scrollback: 3000,
    cursorBlink: false,
    disableStdin: true
  });
  var fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);

  var mode = 'ro';
  var ws = null;
  var enc = new TextEncoder();

  // --- size -------------------------------------------------------------------
  var oscPrefix = '\x1b]' + host.getAttribute('data-size-osc') + ';';
  var remote = null;     // {cols, rows} the far side reported for THIS attach
  var lastSent = '';     // "CxR" last sent on THIS attach
  var fitWidth = false;  // the Fit width toggle
  var overflowing = false;

  function proposed() {
    // Measured with #term back at the container's size: the fit is a property
    // of the page, not of what the terminal currently draws.
    sizer.style.width = ''; sizer.style.height = '';
    host.style.width = ''; host.style.height = ''; host.style.transform = '';
    var p = null;
    try { p = fit.proposeDimensions(); } catch (e) {}
    return (p && p.cols > 0 && p.rows > 0) ? p : null;
  }
  function sendSize(cols, rows) {
    var k = cols + 'x' + rows;
    if (k === lastSent || !ws || ws.readyState !== 1) return;
    lastSent = k;
    ws.send(JSON.stringify({ type: 'resize', cols: cols, rows: rows }));
  }
  function showOverflow(on) {
    overflowing = on;
    fitwBtn.hidden = !on;
    fitwBtn.classList.toggle('inline-flex', on);
    host.setAttribute('data-overflow', on ? '1' : '0');
  }
  function drawOverflow() {
    // Render the far side's size; the scroll area scrolls over it. Fit width
    // scales the drawing (never the far side) to the scroll area's width.
    var screen = host.querySelector('.xterm-screen');
    var vp = host.querySelector('.xterm-viewport');
    if (!screen) return;
    var sb = vp ? Math.max(0, vp.offsetWidth - vp.clientWidth) : 0;
    var w = screen.offsetWidth + sb, h = screen.offsetHeight;
    host.style.width = w + 'px';
    host.style.height = h + 'px';
    var k = 1;
    if (fitWidth) {
      var avail = scrollEl.clientWidth - 8;
      if (w > avail && avail > 0) k = avail / w;
    }
    host.style.transformOrigin = '0 0';
    host.style.transform = k === 1 ? '' : 'scale(' + k + ')';
    sizer.style.width = Math.ceil(w * k) + 'px';
    sizer.style.height = Math.ceil(h * k) + 'px';
    fitwBtn.setAttribute('aria-pressed', fitWidth ? 'true' : 'false');
    fitwBtn.textContent = fitWidth ? 'Actual size' : 'Fit width';
  }
  var layouts = 0;
  function layout() {
    // An instrument as much as a counter: the browser suite asserts a pinch
    // does not move it (no refit on zoom).
    host.setAttribute('data-layouts', String(++layouts));
    var p = proposed();
    if (!p) return;
    if (!remote || (p.cols >= remote.cols && p.rows >= remote.rows)) {
      // The page covers the far side (or it reports nothing): fit, as always.
      showOverflow(false);
      if (term.cols !== p.cols || term.rows !== p.rows) term.resize(p.cols, p.rows);
      sendSize(p.cols, p.rows);
      if (remote) remote = { cols: p.cols, rows: p.rows };
      return;
    }
    // The far side is bigger: draw it at ITS size and send nothing.
    if (term.cols !== remote.cols || term.rows !== remote.rows) term.resize(remote.cols, remote.rows);
    showOverflow(true);
    requestAnimationFrame(drawOverflow);
  }
  // readReport takes a size report off the FRONT of a frame (the far side
  // writes it as a frame of its own, before the session's first byte) and
  // returns the rest.
  function readReport(bytes) {
    if (bytes.length < oscPrefix.length + 4 || bytes[0] !== 27 || bytes[1] !== 93) return bytes;
    var head = '';
    for (var i = 0; i < Math.min(bytes.length, 32); i++) {
      if (bytes[i] === 7) {
        head = String.fromCharCode.apply(null, bytes.subarray(0, i));
        break;
      }
    }
    if (head.indexOf(oscPrefix) !== 0) return bytes;
    var m = /^(\d{1,5});(\d{1,5})$/.exec(head.slice(oscPrefix.length));
    if (!m) return bytes;
    remote = { cols: +m[1], rows: +m[2] };
    host.setAttribute('data-remote', remote.cols + 'x' + remote.rows);
    lastSent = '';
    layout();
    return bytes.subarray(head.length + 1);
  }

  // --- step-up (mirrored sessions) ------------------------------------------
  // The dialog, the freshness the page believes and both ways of confirming
  // live in window.musterStepUp (stepup_confirm.go), shared with the composer.
  var confirmPath = host.getAttribute('data-step-up');
  var stepUp = confirmPath ? window.musterStepUp : null;
  var justConfirmed = false;
  var rememberKey = 'muster.interactive.' + (host.getAttribute('data-remember') || '');
  function fresh() { return !!stepUp && stepUp.fresh(); }
  function remember(on) {
    if (!confirmPath) return;
    try { if (on) sessionStorage.setItem(rememberKey, '1'); else sessionStorage.removeItem(rememberKey); } catch (e) {}
  }
  function remembered() {
    try { return sessionStorage.getItem(rememberKey) === '1'; } catch (e) { return false; }
  }
  // openConfirm asks "Confirm it's you" and, once confirmed (passkey or
  // password), attaches interactively — the action that was waiting on it.
  function openConfirm(why) {
    if (!stepUp) return;
    stepUp.ask(why, function () { justConfirmed = true; goInteractive(); });
  }

  function setStatus(t) { statusEl.textContent = t; }
  function showError(msg, canReconnect) {
    errText.textContent = msg;
    reconnectBtn.hidden = !canReconnect;
    errEl.hidden = false;
  }
  function clearError() { errEl.hidden = true; reconnectBtn.hidden = true; }
  function applyMode() {
    var rw = mode === 'rw';
    modeBtn.setAttribute('aria-checked', rw ? 'true' : 'false');
    modeLabel.textContent = rw ? '` + LiveKeysOn + `' : '` + LiveKeysOff + `';
    term.options.disableStdin = !rw;
    for (var i = 0; i < keys.length; i++) { keys[i].disabled = !rw; }
  }
  function send(data) {
    // The page's half of read-only; the server drops input to a read-only
    // terminal regardless.
    if (mode !== 'rw' || !ws || ws.readyState !== 1) return;
    ws.send(typeof data === 'string' ? enc.encode(data) : data);
  }
  function hexBytes(h) {
    var out = new Uint8Array(h.length / 2);
    for (var i = 0; i < out.length; i++) { out[i] = parseInt(h.substr(i * 2, 2), 16); }
    return out;
  }

  function connect() {
    if (ws) { ws.onclose = null; ws.onmessage = null; try { ws.close(); } catch (e) {} }
    clearError();
    // A new attach: the far side reports its size afresh (or not at all).
    remote = null;
    host.removeAttribute('data-remote');
    var p = proposed();
    if (p && (term.cols !== p.cols || term.rows !== p.rows)) term.resize(p.cols, p.rows);
    showOverflow(false);
    lastSent = term.cols + 'x' + term.rows;
    var wsPath = host.getAttribute('data-ws');
    var base = wsPath ? wsPath + (wsPath.indexOf('?') < 0 ? '?' : '&')
      : '/agents/' + encodeURIComponent(agent) + '/terminal/ws?';
    var url = (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host +
      base + 'mode=' + mode + '&cols=' + term.cols + '&rows=' + term.rows;
    setStatus('Connecting…');
    var sock = new WebSocket(url);
    sock.binaryType = 'arraybuffer';
    var refused = false, opened = false;
    sock.onopen = function () { opened = true; };
    sock.onmessage = function (ev) {
      if (typeof ev.data === 'string') {
        var m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        if (m.type === 'ready') {
          justConfirmed = false;
          setStatus(m.mode === 'rw' ? '` + StatusLiveKeys + `' : '` + StatusConnected + `');
        } else if (m.type === 'error') {
          refused = true;
          showError(m.message, true);
        } else if (m.type === 'exit') {
          refused = true;
          showError(m.message, true);
        }
        return;
      }
      var bytes = readReport(new Uint8Array(ev.data));
      if (bytes.length) term.write(bytes);
    };
    sock.onclose = function () {
      if (ws !== sock) return;
      setStatus('Disconnected');
      // 🔴 A STEP-UP REFUSAL ARRIVES AS A REFUSED UPGRADE (403), which a
      // browser socket cannot read. An interactive mirrored attach that never
      // opened is therefore taken as "not fresh any more": back to keys off,
      // and ask — unless it was JUST confirmed, when asking again would loop.
      if (!opened && mode === 'rw' && confirmPath) {
        var again = !justConfirmed;
        if (stepUp) stepUp.stale();
        justConfirmed = false;
        mode = 'ro';
        remember(false);
        applyMode();
        connect();
        if (again) openConfirm('');
        else showError('The interactive terminal could not be opened.', true);
        return;
      }
      if (!refused) showError('The terminal connection closed.', true);
    };
    ws = sock;
  }

  function goInteractive() {
    mode = 'rw';
    remember(true);
    applyMode();
    connect();
  }

  term.onData(function (d) { send(d); });
  term.onBinary(function (d) {
    var b = new Uint8Array(d.length);
    for (var i = 0; i < d.length; i++) { b[i] = d.charCodeAt(i) & 255; }
    send(b);
  });
  for (var i = 0; i < keys.length; i++) {
    keys[i].addEventListener('click', function (ev) {
      send(hexBytes(ev.currentTarget.getAttribute('data-seq')));
    });
  }
  modeBtn.addEventListener('click', function () {
    if (mode === 'rw') {
      mode = 'ro';
      remember(false);
      applyMode();
      connect();
      return;
    }
    if (confirmPath && !fresh()) { openConfirm(''); return; }
    goInteractive();
  });
  reconnectBtn.addEventListener('click', connect);
  fitwBtn.addEventListener('click', function () {
    fitWidth = !fitWidth;
    if (overflowing) drawOverflow();
  });

  var pending = false;
  function scheduleLayout() {
    if (pending) return;
    pending = true;
    requestAnimationFrame(function () { pending = false; layout(); });
  }
  // The layout box: a rotation, a window resize, the on-screen keyboard.
  if (typeof ResizeObserver !== 'undefined') { new ResizeObserver(scheduleLayout).observe(scrollEl); }
  window.addEventListener('resize', scheduleLayout);
  if (window.visualViewport) {
    // 🔴 visualViewport also fires on a PINCH. A zoom changes vv.scale and not
    // the layout, so it is ignored here; only a same-scale change (a keyboard
    // on a browser that overlays it) re-lays out.
    var vvScale = window.visualViewport.scale;
    window.visualViewport.addEventListener('resize', function () {
      var s = window.visualViewport.scale;
      if (Math.abs(s - vvScale) > 0.001 || Math.abs(s - 1) > 0.001) { vvScale = s; return; }
      scheduleLayout();
    });
  }
  // --- drag-to-scroll (a mirrored host that can: data-scroll) ---------------
  // 🔴 THE HISTORY IS TMUX'S, NOT THIS TERMINAL'S. The viewer's tmux client
  // paints only the visible screen, so xterm's own scrollback holds no real
  // history. A vertical drag (or the mouse wheel) is turned into whole lines
  // and posted; the host scrolls the pane in tmux copy-mode, and the scrolled
  // view arrives on the socket like any other output. Without data-scroll (an
  // older muster-mirror, a read-only host, an agent pod) none of this is wired
  // and touch pans #term-scroll exactly as it always did.
  //
  // Coalesced: a drag accumulates pixels, whole lines are sent at most once per
  // animation frame, and only one request is in flight (the next waits for its
  // answer), so the order of scrolls is the order of the drag.
  //
  // 🔴 A SCROLL THAT DID NOT LAND IS PUT BACK, NOT DROPPED. Any answer but a 2xx
  // (a 429, a slow host's 504, a 502) and a network failure return the request's
  // lines to the pending count, to be sent again: on the next frame the first
  // time, then after 100, 200 and 400 ms. After scrollMaxFails failures in a
  // row the page gives up: it drops what is pending and shows
  // #term-scroll-note until a scroll lands again.
  //
  // 🔴 A REFUSAL IS READ FROM ITS CODE (the ScrollRefusalHeader), NOT ITS
  // STATUS, and only "this host cannot scroll" stops the gesture (Reconnect
  // wires it again). A host that is reconnecting, a pane in a tmux menu, a
  // window that is gone: each shows its note, drops what is pending (it was
  // meant for a view that is not there) and keeps the gesture — the next drag
  // asks again, and the note clears once one lands. A status with no code (a
  // proxy's 404) is retried like any other failure.
  var scrollPath = host.getAttribute('data-scroll');
  if (scrollPath) (function () {
    var form = host.getAttribute('data-scroll-form') || '';
    var note = document.getElementById('term-scroll-note');
    var maxLines = ` + strconv.Itoa(mirror.MaxScrollLines) + `;
    var scrollMaxFails = 5;
    var px = 0, lines = 0, sent = 0, asked = 0, fails = 0;
    var inFlight = false, frame = false, dead = true, waiting = false;
    var refusalHeader = ` + strconv.Quote(ScrollRefusalHeader) + `;
    var stopCode = ` + strconv.Quote(ScrollRefusalUnsupported) + `;
    var refusalNotes = {};
    refusalNotes[` + strconv.Quote(ScrollRefusalOffline) + `] = ` + strconv.Quote(ScrollOfflineNote) + `;
    refusalNotes[` + strconv.Quote(ScrollRefusalMode) + `] = ` + strconv.Quote(ScrollModeNote) + `;
    refusalNotes[` + strconv.Quote(ScrollRefusalGone) + `] = ` + strconv.Quote(ScrollGoneNote) + `;
    refusalNotes[` + strconv.Quote(ScrollRefusalRefused) + `] = ` + strconv.Quote(ScrollRefusedNote) + `;
    var gaveUpNote = ` + strconv.Quote(ScrollGaveUpNote) + `;
    function showNote(text) {
      if (!note) return;
      if (text) note.textContent = text;
      note.hidden = !text;
      // An instrument for the browser suite: which note is up.
      if (text) host.setAttribute('data-scroll-note', text); else host.removeAttribute('data-scroll-note');
    }
    function cellHeight() {
      // The drawn height (Fit width scales it), per row.
      var sc = host.querySelector('.xterm-screen');
      var h = sc ? sc.getBoundingClientRect().height / term.rows : 0;
      return h > 0 ? h : 16;
    }
    function schedule() {
      if (frame || dead) return;
      frame = true;
      requestAnimationFrame(function () { frame = false; flush(); });
    }
    function addLines(n) {
      if (!n) return;
      lines += n;
      // An instrument for the browser suite: every line a gesture asked for.
      asked += n;
      host.setAttribute('data-scroll-asked', String(asked));
      schedule();
    }
    function addPx(d) {
      // d > 0 is back in history.
      px += d;
      var c = cellHeight();
      var n = px > 0 ? Math.floor(px / c) : Math.ceil(px / c);
      px -= n * c;
      addLines(n);
    }
    function flush() {
      if (inFlight || waiting || dead || !lines) return;
      var n = Math.max(-maxLines, Math.min(maxLines, lines));
      lines -= n;
      inFlight = true;
      fetch(scrollPath, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: form + '&lines=' + n
      }).then(function (r) {
        if (r.ok) return 'landed';
        var code = r.headers.get(refusalHeader) || '';
        if (code === stopCode) return 'stop';
        if (refusalNotes.hasOwnProperty(code)) return 'refused:' + code;
        return 'retry';
      }, function () { return 'retry'; }).then(function (how) {
        inFlight = false;
        if (how === 'landed') {
          sent += n;
          fails = 0;
          host.setAttribute('data-scroll-sent', String(sent));
          showNote('');
        } else if (how === 'stop') {
          lines = 0;
          px = 0;
          fails = 0;
          stop();
          return;
        } else if (how.indexOf('refused:') === 0) {
          // Not this host's to scroll right now: say why, keep the gesture.
          lines = 0;
          px = 0;
          fails = 0;
          showNote(refusalNotes[how.slice('refused:'.length)]);
          return;
        } else {
          lines += n;
          fails++;
          if (fails >= scrollMaxFails) {
            lines = 0;
            px = 0;
            fails = 0;
            showNote(gaveUpNote);
            return;
          }
          if (fails > 1) {
            waiting = true;
            setTimeout(function () { waiting = false; schedule(); }, 100 * Math.pow(2, fails - 2));
            return;
          }
        }
        if (lines) schedule();
      });
    }
    // A far side taller than the page: the scroll area takes a drag first,
    // and only what is left of it scrolls the history.
    function panArea(d) {
      if (scrollEl.scrollHeight <= scrollEl.clientHeight) return d;
      var before = scrollEl.scrollTop;
      scrollEl.scrollTop = before - d;
      return d - (before - scrollEl.scrollTop);
    }
    // Touch: the AXIS IS DECIDED AT THE GESTURE'S START. A horizontal start is
    // left to the browser (touch-action pan-x: #term-scroll pans as before);
    // a vertical one is ours, and never reaches xterm, whose own touch scroll
    // has nothing real to show. Two fingers are a pinch, never a scroll.
    var start = null, axis = '', lastY = 0;
    function onStart(ev) {
      start = null;
      if (ev.touches.length !== 1) return;
      start = { x: ev.touches[0].clientX, y: ev.touches[0].clientY };
      axis = '';
    }
    function onMove(ev) {
      if (!start) return;
      if (ev.touches.length !== 1) { start = null; return; }
      var t = ev.touches[0];
      if (!axis) {
        var dx = t.clientX - start.x, dy = t.clientY - start.y;
        if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
        axis = Math.abs(dy) > Math.abs(dx) ? 'y' : 'x';
        lastY = start.y;
      }
      if (axis !== 'y') return;
      ev.stopPropagation();
      var d = panArea(t.clientY - lastY);
      lastY = t.clientY;
      if (d) addPx(d);
    }
    function onEnd() { start = null; }
    // The wheel: vertical only (a sideways one pans #term-scroll as before).
    function onWheel(ev) {
      if (Math.abs(ev.deltaX) > Math.abs(ev.deltaY) || !ev.deltaY) return;
      ev.preventDefault();
      ev.stopPropagation();
      if (ev.deltaMode === 1) addLines(-Math.round(ev.deltaY));
      else if (ev.deltaMode === 2) addLines(-Math.round(ev.deltaY) * term.rows);
      else addPx(panArea(-ev.deltaY));
    }
    var touchTargets = [scrollEl, host.querySelector('.xterm-viewport')];
    function stop() {
      if (dead) return;
      dead = true;
      scrollEl.removeEventListener('touchstart', onStart, true);
      scrollEl.removeEventListener('touchmove', onMove, true);
      scrollEl.removeEventListener('touchend', onEnd, true);
      scrollEl.removeEventListener('touchcancel', onEnd, true);
      scrollEl.removeEventListener('wheel', onWheel, true);
      touchTargets.forEach(function (el) { if (el) el.style.touchAction = ''; });
      host.removeAttribute('data-scroll-on');
      showNote('');
    }
    function wire() {
      if (!dead) return;
      dead = false;
      touchTargets.forEach(function (el) { if (el) el.style.touchAction = 'pan-x pinch-zoom'; });
      scrollEl.addEventListener('touchstart', onStart, { capture: true, passive: true });
      scrollEl.addEventListener('touchmove', onMove, { capture: true, passive: true });
      scrollEl.addEventListener('touchend', onEnd, { capture: true, passive: true });
      scrollEl.addEventListener('touchcancel', onEnd, { capture: true, passive: true });
      scrollEl.addEventListener('wheel', onWheel, { capture: true, passive: false });
      // An instrument for the browser suite: the handlers are wired.
      host.setAttribute('data-scroll-on', '1');
    }
    wire();
    // Reconnect wires the gesture again after "this host cannot scroll": the
    // host may have been upgraded or restarted with -interactive since.
    reconnectBtn.addEventListener('click', wire);
  })();

  // 🟢 OFF SCREEN, NO LIVE KEYS (view_swap.go): a terminal hidden behind the
  // other view while typing straight into the session drops to read-only —
  // one re-attach — and takes live keys back only when it is on screen again,
  // through the same freshness check the switch uses.
  var rwWhenShown = false;
  window.addEventListener('muster:view-hidden', function () {
    if (mode !== 'rw') return;
    rwWhenShown = true;
    mode = 'ro';
    applyMode();
    connect();
  });
  window.addEventListener('muster:view-shown', function () {
    if (!rwWhenShown) return;
    rwWhenShown = false;
    if (confirmPath && !fresh()) { openConfirm(''); return; }
    goInteractive();
  });

  // Leaving the page ends the stream now rather than when the socket times out.
  window.addEventListener('pagehide', function () { if (ws) { ws.onclose = null; ws.close(); } });

  if (confirmPath && remembered() && fresh()) mode = 'rw';
  applyMode();
  if (!unavailable) connect();
})();
`))
}

// mirrorTerminalTitle is the terminal page's title for a mirrored entry, drawn
// by the same mirrorEntryTitle as its card: the label with the raw session name
// beside it when rawName is set, else the title alone — either with its
// qualifier.
func mirrorTerminalTitle(title, rawName, qualifier string) g.Node {
	if rawName != "" {
		return mirrorEntryTitle(title, "", rawName, qualifier, "")
	}
	return mirrorEntryTitle("", title, "", qualifier, "")
}
