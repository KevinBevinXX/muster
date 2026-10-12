package ui

import (
	"encoding/hex"
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
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
// and the server attaches read-only for anything but mode=rw.
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
						H1(Class("truncate text-sm font-semibold"), g.Text(markdownPlain(title)),
							g.If(v.Badge != "", Span(g.Attr("data-term-badge", ""), Class("ml-2"), chip("kind", v.Badge)))),
						P(ID("term-status"), Class("truncate text-xs text-muted"), g.Attr("aria-live", "polite"), g.Text("Not connected")),
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
					Class("relative min-h-0 flex-1 overflow-hidden bg-black px-1 py-1"),
					Div(
						ID("term"),
						g.Attr("data-agent", v.Name),
						g.If(v.WSPath != "", g.Attr("data-ws", v.WSPath)),
						g.If(v.Unavailable != "", g.Attr("data-unavailable", "")),
						Class("h-full w-full"),
					),
				),
				quickKeyBar(),
				agentTerminalScript(),
				pwaChrome(),
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
// terminal.go for the wire.
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
  var statusEl = document.getElementById('term-status');
  var errEl = document.getElementById('term-error');
  var errText = document.getElementById('term-error-text');
  var reconnectBtn = document.getElementById('term-reconnect');
  var modeBtn = document.getElementById('term-mode');
  var modeLabel = document.getElementById('term-mode-label');
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
  function sendResize() {
    if (ws && ws.readyState === 1) {
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    }
  }
  function refit() {
    try { fit.fit(); } catch (e) {}
    sendResize();
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
    try { fit.fit(); } catch (e) {}
    var wsPath = host.getAttribute('data-ws');
    var base = wsPath ? wsPath + (wsPath.indexOf('?') < 0 ? '?' : '&')
      : '/agents/' + encodeURIComponent(agent) + '/terminal/ws?';
    var url = (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host +
      base + 'mode=' + mode + '&cols=' + term.cols + '&rows=' + term.rows;
    setStatus('Connecting…');
    var sock = new WebSocket(url);
    sock.binaryType = 'arraybuffer';
    var refused = false;
    sock.onmessage = function (ev) {
      if (typeof ev.data === 'string') {
        var m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        if (m.type === 'ready') {
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
      term.write(new Uint8Array(ev.data));
    };
    sock.onclose = function () {
      if (ws !== sock) return;
      setStatus('Disconnected');
      if (!refused) showError('The terminal connection closed.', true);
    };
    ws = sock;
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
    mode = mode === 'rw' ? 'ro' : 'rw';
    applyMode();
    connect();
  });
  reconnectBtn.addEventListener('click', connect);

  var pending = false;
  function scheduleFit() {
    if (pending) return;
    pending = true;
    requestAnimationFrame(function () { pending = false; refit(); });
  }
  if (typeof ResizeObserver !== 'undefined') { new ResizeObserver(scheduleFit).observe(host); }
  window.addEventListener('resize', scheduleFit);
  if (window.visualViewport) { window.visualViewport.addEventListener('resize', scheduleFit); }
  // Leaving the page ends the stream now rather than when the socket times out.
  window.addEventListener('pagehide', function () { if (ws) { ws.onclose = null; ws.close(); } });

  applyMode();
  if (!unavailable) connect();
})();
`))
}
