package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE COMPOSER of a mirrored session: a text box and Send, at the
// bottom of BOTH of its views.
//
//   - Rendered view (a pane running Claude Code): posts to MirrorSendPath, and
//     the host pastes only into a pane that still runs Claude Code.
//   - Terminal view (any session of an -interactive host): posts to
//     MirrorTerminalSendPath, and the host pastes into the session's active
//     pane whatever runs there — the shell or TUI on screen above it.
//
// 🔴 ONE FORM, ONE SCRIPT, ONE HANDLER BEHIND BOTH ROUTES (internal/api's
// mirrorSend). The two views differ in a route and a placeholder, nothing else:
// the same markup (mirrorComposer), the same script (mirrorComposerScript), the
// same gates. A fix to one is a fix to both because there is no second copy.
//
// 🔴 IT IS NOT THE INTERACTIVE SWITCH. The switch turns the page's terminal
// into a live keyboard; the composer is one discrete, gated paste per Send and
// works with the terminal read-only. Both need the host's -interactive and a
// fresh sign-in — the server checks each send, whatever the page shows.
//
// PHONE ERGONOMICS, which the browser suite measures at 390×844: the input is
// 16px (text-base) — iOS zooms the page on focusing anything smaller and does
// not zoom back; both controls are 44px touch targets; the form is the last
// thing in an h-dvh column and carries the safe-area inset, so with
// interactive-widget=resizes-content it sits on top of the on-screen keyboard
// rather than under it.

// MirrorSendPath is where the rendered view's composer posts a message: into
// the session's Claude Code pane, and only that.
const MirrorSendPath = "/mirror/view/send"

// MirrorTerminalSendPath is where the Terminal view's composer posts a message:
// into the session's active pane, whatever runs in it.
const MirrorTerminalSendPath = "/mirror/terminal/send"

// MirrorComposerView is one composer.
type MirrorComposerView struct {
	// SendPath is the route the form posts to (one of the two above).
	SendPath string
	Host     string
	Session  string
	// Window is the tmux window id the message is for; "" = the session's
	// active window (a per-session host).
	Window string
	// Placeholder and Aria name what the text reaches ("Message Claude…").
	Placeholder string
	Aria        string
	// Disabled, when set, draws the composer disabled and is the reason shown
	// under it — a read-only host, or a muster-mirror too old to be sent to.
	// The server refuses the send regardless.
	Disabled string
	// DisabledPlaceholder is the input's placeholder while Disabled.
	DisabledPlaceholder string
}

// The composer's own messages. The script shows them in #mv-note.
const (
	// MirrorComposeEmpty answers a Send with nothing typed: said, not swallowed.
	MirrorComposeEmpty = "Type a message first."
	mirrorComposeDown  = "Not sent: muster could not be reached."
)

// mirrorComposer is the composer's markup: a status line, then the input row.
func mirrorComposer(v MirrorComposerView) g.Node {
	off := v.Disabled != ""
	placeholder := v.Placeholder
	if off {
		placeholder = v.DisabledPlaceholder
	}
	return Div(
		ID("mv-compose"),
		Class("flex-none border-t border-line bg-bg"),
		// The send's outcome, inline: why it was not sent, or that there was
		// nothing to send. Empty and hidden otherwise.
		P(ID("mv-note"), g.Attr("role", "status"), g.Attr("aria-live", "polite"), g.Attr("hidden", ""),
			Class("px-3 pt-2 text-xs text-st-error-fg break-words")),
		Form(
			ID("mv-form"),
			g.Attr("data-send", v.SendPath),
			g.Attr("data-host", v.Host),
			g.Attr("data-session", v.Session),
			g.Attr("data-window", v.Window),
			g.Attr("data-empty", MirrorComposeEmpty),
			g.Attr("data-down", mirrorComposeDown),
			g.Attr("novalidate", ""),
			Class("flex items-end gap-2 px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-2"),
			Textarea(
				ID("mv-input"),
				Name("text"),
				Rows("1"),
				g.Attr("aria-label", v.Aria),
				g.Attr("enterkeyhint", "send"),
				g.If(off, g.Attr("disabled", "")),
				g.If(off, g.Attr("title", v.Disabled)),
				Placeholder(placeholder),
				Class("min-h-[44px] min-w-0 flex-1 resize-none rounded-xl border-0 bg-s2 px-3 py-2.5 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-50"),
			),
			Button(
				ID("mv-send"),
				Type("submit"),
				g.If(off, g.Attr("disabled", "")),
				g.Attr("aria-label", "Send"),
				Class("press min-h-[44px] shrink-0 rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition disabled:opacity-40"),
				g.Text("Send"),
			),
		),
		g.If(off, P(ID("mv-readonly"), Class("px-3 pb-2 text-[11px] text-muted"), g.Text(v.Disabled))),
	)
}

// mirrorComposerScript posts the composer's text.
//
// A 403 carrying X-Muster-Step-Up means the sign-in is not fresh: "Confirm it's
// you" is asked (window.musterStepUp, stepup_confirm.go) and on success the
// SAME text — still in the box, never cleared on a refusal — is sent again. The
// page never decides freshness; the server does.
//
// What a person sees: sent → the box clears (and the view above shows the text
// land); not sent → the reason, inline, with the text kept; nothing typed →
// MirrorComposeEmpty rather than a Send that silently does nothing.
func mirrorComposerScript() g.Node {
	return Script(g.Raw(`
(function () {
  var form = document.getElementById('mv-form');
  if (!form) return;
  var input = document.getElementById('mv-input');
  var send = document.getElementById('mv-send');
  var note = document.getElementById('mv-note');
  function say(msg) { note.textContent = msg || ''; note.hidden = !msg; }
  function post() {
    var body = new URLSearchParams();
    body.set('host', form.getAttribute('data-host'));
    body.set('session', form.getAttribute('data-session'));
    var win = form.getAttribute('data-window');
    if (win) body.set('window', win);
    body.set('text', input.value);
    send.disabled = true;
    return fetch(form.getAttribute('data-send'), { method: 'POST', body: body, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' } })
      .then(function (r) {
        if (r.ok) { input.value = ''; say(''); return; }
        var stepUp = window.musterStepUp;
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && stepUp) {
          stepUp.stale();
          stepUp.ask('', post);
          return;
        }
        return r.text().then(function (t) { say(t.trim() || ('Not sent (HTTP ' + r.status + ')')); });
      })
      .catch(function () { say(form.getAttribute('data-down')); })
      .then(function () { send.disabled = false; });
  }
  form.addEventListener('submit', function (e) {
    e.preventDefault();
    if (input.disabled) return;
    if (!input.value.trim()) { say(form.getAttribute('data-empty')); input.focus(); return; }
    say('');
    post();
  });
  input.addEventListener('input', function () { if (input.value.trim()) say(''); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); form.requestSubmit(); }
  });
})();
`))
}
