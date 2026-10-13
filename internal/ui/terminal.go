package ui

import (
	"encoding/hex"
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/termsize"
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
const (
	xtermJS    = "/static/vendor/xterm.js"
	xtermCSS   = "/static/vendor/xterm.css"
	xtermFitJS = "/static/vendor/addon-fit.js"
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
	// Unavailable, when set, is why this deployment cannot attach a terminal;
	// the page says so instead of opening a socket that would be refused.
	Unavailable string
	// BackHref is where the back arrow goes ("" = the agent's own page). A
	// mirrored session has no agent page — its terminal is its page — so it goes
	// back to the list.
	BackHref string
	// Badge is a short label beside the title ("Mirrored · <host>"); "" = none.
	Badge string
	// RawName, when set, is shown small and muted after the title, and as its
	// tooltip (mirrorSessionTitle): a mirrored session titled by its host's
	// label ("mango:1") keeps its real tmux session name findable. "" = the
	// title alone.
	RawName string
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
}

// StepUpView is what the page needs to ask "Confirm it's you" before it
// attaches interactively.
//
// ⚠ THE PAGE'S VIEW OF FRESHNESS IS A CONVENIENCE, NOT THE GATE. It only
// decides whether to ask before trying; the server refuses a stale interactive
// attach (403) whatever the page believed, and the page then asks.
type StepUpView struct {
	// ConfirmPath is the same-origin route that takes the password again.
	ConfirmPath string
	// FreshFor is how many milliseconds the current sign-in stays fresh, as
	// the SERVER measured it when it rendered the page (0 = not fresh). A
	// duration rather than an instant, so the browser's clock does not matter.
	FreshFor int64
	// RememberKey names what an Interactive choice is remembered for, in this
	// browser session (sessionStorage): "mirror:<host>" — one choice per host.
	RememberKey string
}

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

// AgentTerminalPage is a full-height document: a header with the read-only /
// interactive switch, the terminal, and the quick-key bar.
//
// 🔴 IT IS ITS OWN DOCUMENT, NOT A PANEL OF THE AGENT PAGE. The agent page is
// hx-boost'ed and its chat script tears down and re-creates its socket on
// every boosted swap; a terminal living inside that lifecycle would be
// re-attached on every navigation. The links into this page opt out of boost,
// so it is always a real load with its own scripts.
//
// ⚠ READ-ONLY IS THE STATE THE PAGE OPENS IN, AND THE SERVER AGREES
// INDEPENDENTLY: the socket is opened with mode=ro until the switch is flipped,
// and the server attaches read-only for anything but mode=rw. (The one
// exception is a mirrored host whose Interactive choice this browser session
// remembers while its sign-in is still fresh — and the server re-checks that.)
//
// 🔴 A WIDER FAR SIDE IS SCROLLED, NOT SQUEEZED (internal/termsize). When the
// session's window is wider (or taller) than this page, the far side keeps its
// size and says so in-band; the page renders that size inside #term-scroll,
// which scrolls both ways and can be pinch-zoomed, and offers "Fit width".
func AgentTerminalPage(v AgentTerminalView) g.Node {
	title := displayOr(v.DisplayName, v.Name)
	back := v.BackHref
	if back == "" {
		back = "/agents/" + url.PathEscape(v.Name)
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Terminal · "+markdownPlain(title)+" · muster")),
				Link(Rel("stylesheet"), Href("/static/app.css")),
				Link(Rel("stylesheet"), Href(xtermCSS)),
				Script(Src(xtermJS)),
				Script(Src(xtermFitJS)),
				faroHead(),
			),
			Body(
				Class("flex h-dvh flex-col overflow-hidden bg-bg text-fg antialiased"),
				Header(
					Class("flex items-center gap-2 border-b border-line px-2 py-1.5"),
					A(
						ID("term-back"),
						Href(back),
						g.Attr("aria-label", "Back to the agent"),
						Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
						g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
					),
					Div(
						Class("min-w-0 flex-1"),
						H1(Class("truncate text-sm font-semibold"),
							g.If(v.RawName == "", g.Text(markdownPlain(title))),
							g.If(v.RawName != "", mirrorSessionTitle(markdownPlain(title), v.RawName, "")),
							g.If(v.Badge != "", Span(g.Attr("data-term-badge", ""), Class("ml-2"), chip("kind", v.Badge)))),
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
					// The switch. aria-checked is the INTERACTIVE state, so
					// "false" — read-only — is what the page opens with.
					Button(
						ID("term-mode"),
						Type("button"),
						g.Attr("role", "switch"),
						g.Attr("aria-checked", "false"),
						g.Attr("aria-label", "Interactive: let this page type into the session"),
						g.If(v.Unavailable != "", g.Attr("disabled", "")),
						// ⚠ The `hidden` CLASS, not the attribute: an author display
						// utility (inline-flex) beats the UA's [hidden] rule.
						g.If(v.ReadOnlyOnly, g.Attr("hidden", "")),
						g.If(v.InteractiveNote != "", g.Attr("title", v.InteractiveNote)),
						g.If(v.InteractiveNote != "", g.Attr("data-interactive-note", v.InteractiveNote)),
						Class(termModeDisplay(v.ReadOnlyOnly)+" press min-h-[44px] shrink-0 items-center gap-2 rounded-lg bg-s2 px-3 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition aria-checked:bg-danger aria-checked:text-on-accent disabled:opacity-40"),
						Span(ID("term-mode-label"), g.Text("Read-only")),
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
								Class("h-full w-full"),
							),
						),
					),
				),
				quickKeyBar(),
				g.If(v.StepUp != nil, confirmDialog()),
				agentTerminalScript(),
				pwaChrome(),
			),
		),
	)
}

// stepUpAttrs carries a StepUpView to the script on #term; nothing for nil.
func stepUpAttrs(s *StepUpView) g.Node {
	if s == nil {
		return nil
	}
	return g.Group{
		g.Attr("data-step-up", s.ConfirmPath),
		g.Attr("data-fresh-for", strconv.FormatInt(s.FreshFor, 10)),
		g.Attr("data-remember", s.RememberKey),
	}
}

// confirmDialog is "Confirm it's you": the password again before an
// interactive attach. It posts to the StepUpView's ConfirmPath from the script
// (same origin, credentials included); the server's answer replaces the
// session's cookies with a freshly signed-in one.
func confirmDialog() g.Node {
	return Dialog(
		ID("term-confirm"),
		g.Attr("aria-labelledby", "term-confirm-title"),
		Class("w-[calc(100%-2rem)] max-w-sm rounded-2xl bg-s1 p-0 text-fg ring-1 ring-line backdrop:bg-black/60"),
		Form(
			ID("term-confirm-form"),
			Class("flex flex-col gap-4 px-5 py-6"),
			H2(ID("term-confirm-title"), Class("text-base font-semibold text-fg"), g.Text("Confirm it's you")),
			P(Class("text-sm text-fg2"), g.Text("Typing into this session needs a sign-in from the last 5 minutes. Enter your password to continue.")),
			Label(For("term-confirm-password"), Class("text-sm font-medium text-fg"), g.Text("Password")),
			Input(
				ID("term-confirm-password"),
				Name("password"),
				Type("password"),
				g.Attr("autocomplete", "current-password"),
				g.Attr("required", ""),
				Class("min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
			),
			P(ID("term-confirm-error"), g.Attr("role", "alert"), g.Attr("hidden", ""), Class("rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40")),
			Div(
				Class("flex gap-2"),
				Button(ID("term-confirm-cancel"), Type("button"),
					Class("press min-h-[44px] flex-1 rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge"),
					g.Text("Cancel")),
				Button(ID("term-confirm-submit"), Type("submit"),
					Class("press min-h-[44px] flex-1 rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90"),
					g.Text("Confirm")),
			),
		),
	)
}

// quickKeyBar renders QuickKeys as a two-row grid that fits a phone's width
// without scrolling. The buttons start DISABLED: the page opens read-only.
func quickKeyBar() g.Node {
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
		Class("grid grid-cols-6 gap-1 border-t border-line bg-bg px-2 pt-2 pb-[max(0.5rem,env(safe-area-inset-bottom))]"),
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
  var confirmPath = host.getAttribute('data-step-up');
  var dlg = document.getElementById('term-confirm');
  var freshUntil = 0;     // performance.now() deadline of the current sign-in's freshness
  var justConfirmed = false;
  var rememberKey = 'muster.interactive.' + (host.getAttribute('data-remember') || '');
  if (confirmPath) {
    var ff = +host.getAttribute('data-fresh-for') || 0;
    if (ff > 0) freshUntil = performance.now() + ff;
  }
  // A margin, so an attach is not tried in the last seconds of freshness.
  function fresh() { return performance.now() < freshUntil - 10000; }
  function remember(on) {
    if (!confirmPath) return;
    try { if (on) sessionStorage.setItem(rememberKey, '1'); else sessionStorage.removeItem(rememberKey); } catch (e) {}
  }
  function remembered() {
    try { return sessionStorage.getItem(rememberKey) === '1'; } catch (e) { return false; }
  }
  function openConfirm(why) {
    if (!dlg) return;
    var err = document.getElementById('term-confirm-error');
    err.hidden = !why;
    err.textContent = why || '';
    var pw = document.getElementById('term-confirm-password');
    pw.value = '';
    if (!dlg.open) dlg.showModal();
    pw.focus();
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
    modeLabel.textContent = rw ? 'Interactive' : 'Read-only';
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
          setStatus(m.mode === 'rw' ? 'Connected · interactive' : 'Connected · read-only');
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
      // opened is therefore taken as "not fresh any more": back to read-only,
      // and ask — unless the password was JUST confirmed, when asking again
      // would loop.
      if (!opened && mode === 'rw' && confirmPath) {
        var again = !justConfirmed;
        freshUntil = 0;
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

  if (dlg) {
    document.getElementById('term-confirm-cancel').addEventListener('click', function () { dlg.close(); });
    document.getElementById('term-confirm-form').addEventListener('submit', function (ev) {
      ev.preventDefault();
      var pw = document.getElementById('term-confirm-password');
      var err = document.getElementById('term-confirm-error');
      var btn = document.getElementById('term-confirm-submit');
      btn.disabled = true;
      fetch(confirmPath, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Accept': 'application/json' },
        body: new URLSearchParams({ password: pw.value })
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; });
      }).then(function (res) {
        btn.disabled = false;
        if (!res.ok) {
          err.textContent = res.j.error || 'That could not be confirmed.';
          err.hidden = false;
          pw.value = '';
          pw.focus();
          return;
        }
        freshUntil = performance.now() + (+res.j.fresh_for || 0);
        justConfirmed = true;
        pw.value = '';
        dlg.close();
        goInteractive();
      }, function () {
        btn.disabled = false;
        err.textContent = 'muster could not be reached. Try again.';
        err.hidden = false;
      });
    });
  }

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
  // Leaving the page ends the stream now rather than when the socket times out.
  window.addEventListener('pagehide', function () { if (ws) { ws.onclose = null; ws.close(); } });

  if (confirmPath && remembered() && fresh()) mode = 'rw';
  applyMode();
  if (!unavailable) connect();
})();
`))
}
