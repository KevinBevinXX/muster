package ui

import (
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE COMPOSER: a text box and Send, at the bottom of every view that
// can be sent a message.
//
//   - A mirrored session's Rendered view (a pane running Claude Code): posts to
//     MirrorSendPath, and the host pastes only into a pane that still runs
//     Claude Code.
//   - A mirrored session's Terminal view (any window of an -interactive host):
//     posts to MirrorTerminalSendPath, and the host pastes into the window's
//     active pane whatever runs there — the shell or TUI on screen above it.
//   - A dispatched agent's Terminal view: posts to AgentSendPath, which hands
//     the text to the agent as a chat turn (the agent's own chat page keeps its
//     streaming form, in the same clothes: composerInputClass / composerSendClass).
//
// 🔴 ONE FORM AND ONE SCRIPT (mirrorComposer, mirrorComposerScript). The views
// differ in a route, the hidden fields that address what is sent to, and a
// placeholder; a fix to one is a fix to all because there is no second copy.
//
// 🔴 IT IS A REAL FORM, AND THE SCRIPT IS AN ENHANCEMENT. `method=post` and
// `action` are on the element and the address is in hidden inputs, so a page
// whose script did not run — or threw — still sends: the browser submits the
// form, and the server answers that NAVIGATION with a redirect back to the same
// view carrying a notice (ComposerView.Notice), or with a plain "Confirm it's
// you" page when the sign-in is stale (StepUpFormPage). The script, when it
// runs, does the same send without leaving the page — and hands over to the
// browser's own submit whenever its way fails. Send is never a silent no-op.
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

// ComposerView is one composer.
type ComposerView struct {
	// SendPath is the route the form posts to.
	SendPath string
	// Host, Session and Window address a mirrored entry (hidden fields `host`,
	// `session`, `window`); Window is "" for the session's active window. All
	// empty for a composer that is not a mirrored entry's.
	Host    string
	Session string
	Window  string
	// Extra are further hidden fields, in order: what else the route needs to
	// know where the message goes and where to come back to.
	Extra []ComposerField
	// Placeholder and Aria name what the text reaches ("Message Claude…").
	Placeholder string
	Aria        string
	// Disabled, when set, draws the composer disabled and is the reason shown
	// under it — a read-only host, a muster-mirror too old to be sent to, an
	// agent that is not running. The server refuses the send regardless.
	Disabled string
	// DisabledPlaceholder is the input's placeholder while Disabled.
	DisabledPlaceholder string
	// Notice, when set, is drawn in the status line when the page loads: the
	// outcome of a send the BROWSER submitted (no script), which came back here
	// as a redirect. NoticeOK says it is good news (it is styled as such).
	Notice   string
	NoticeOK bool
	// Nonce names ONE message for the server (hidden field `nonce`): a send
	// repeated with the same nonce is delivered once. The server mints one per
	// page; the script replaces it after each message that was sent.
	//
	// 🔴 IT IS WHAT MAKES "TRY AGAIN ANOTHER WAY" SAFE. When a send gets no
	// answer the script hands the form to the browser, which posts it again —
	// and the first post may have been DELIVERED with only its answer lost. A
	// prompt typed into an agent twice is run twice. With the nonce the second
	// post is recognised and answered as sent, without being pasted. "" = no
	// such protection (the server then delivers every post).
	Nonce string
	// Dictate is where the microphone posts a recording (TranscribePath), or ""
	// for no microphone — the server has no speech-to-text service. A disabled
	// composer draws none either way: there is nothing its text could be sent
	// to. See dictate.go.
	Dictate string
}

// MirrorComposerView is ComposerView under the name it had while only mirrored
// sessions had one.
type MirrorComposerView = ComposerView

// ComposerField is one hidden field of a composer.
type ComposerField struct{ Name, Value string }

// fields is every hidden field the form carries, in order.
func (v ComposerView) fields() []ComposerField {
	var out []ComposerField
	for _, f := range []ComposerField{{"host", v.Host}, {"session", v.Session}, {"window", v.Window}} {
		if f.Value != "" {
			out = append(out, f)
		}
	}
	return append(out, v.Extra...)
}

// action is the form's action: the send route with the address in its QUERY as
// well as in the hidden fields.
//
// ⚠ THE QUERY IS FOR A REQUEST THAT COMES BACK AS A GET. Behind a forward-auth
// edge a POST with an expired edge session is answered with a redirect to the
// sign-in portal, which returns the browser to this URL with GET and no body.
// The GET route reads the address from here and sends the browser back to the
// view it came from, saying the message was not sent (ComposerNoticeAgain). The
// POST itself reads its fields from the BODY only.
func (v ComposerView) action() string {
	q := url.Values{}
	for _, f := range v.fields() {
		q.Set(f.Name, f.Value)
	}
	if len(q) == 0 {
		return v.SendPath
	}
	return v.SendPath + "?" + q.Encode()
}

// The composer's own messages. The script shows them in #mv-note.
const (
	// MirrorComposeEmpty answers a Send with nothing typed: said, not swallowed.
	MirrorComposeEmpty = "Type a message first."
	mirrorComposeDown  = "Not sent: you are offline."
)

// composerInputClass and composerSendClass are the composer's look: a 16px
// input (iOS zooms the page on focusing anything smaller) and 44px targets.
// The agent chat's own form wears the same two, so every message box in the
// app is one design.
const (
	composerInputClass = "min-h-[44px] min-w-0 flex-1 resize-none rounded-xl border-0 bg-s2 px-3 py-2.5 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-50"
	composerSendClass  = "press min-h-[44px] shrink-0 rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition disabled:opacity-40"
)

// mirrorComposer is the composer's markup: a status line, then the input row.
func mirrorComposer(v ComposerView) g.Node {
	off := v.Disabled != ""
	dictate := v.Dictate
	if off {
		dictate = ""
	}
	placeholder := v.Placeholder
	if off {
		placeholder = v.DisabledPlaceholder
	}
	noteClass := "px-3 pt-2 text-xs break-words "
	if v.NoticeOK {
		noteClass += "text-fg2"
	} else {
		noteClass += "text-st-error-fg"
	}
	hidden := make([]g.Node, 0, 4)
	for _, f := range v.fields() {
		hidden = append(hidden, Input(Type("hidden"), Name(f.Name), Value(f.Value)))
	}
	return Div(
		ID("mv-compose"),
		Class("flex-none border-t border-line bg-bg"),
		// The send's outcome, inline: why it was not sent, or that there was
		// nothing to send. Empty and hidden otherwise — unless the page was
		// reached by the redirect that answers a send the browser submitted,
		// when the server draws the outcome here itself.
		P(ID("mv-note"), g.Attr("role", "status"), g.Attr("aria-live", "polite"),
			g.If(v.Notice == "", g.Attr("hidden", "")),
			g.If(v.Notice != "" && v.NoticeOK, g.Attr("data-ok", "")),
			Class(noteClass), g.Text(v.Notice)),
		// The microphone's own status line (dictate.go): kept apart from the
		// send's, so a recording's timer and a send's outcome never overwrite
		// each other.
		dictateNote(dictate, "mv-dictate-note", "px-3 pt-2"),
		Form(
			ID("mv-form"),
			// 🔴 method + action: without the script this is still a form the
			// browser can submit. The script reads the route from `action`.
			Method("post"),
			Action(v.action()),
			g.Attr("data-empty", MirrorComposeEmpty),
			g.Attr("data-down", mirrorComposeDown),
			g.Attr("novalidate", ""),
			// The page may be hx-boost'ed (an agent's): a boosted form would be
			// submitted by htmx as well as by the script.
			hx("hx-boost", "false"),
			Class("flex items-end gap-2 px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-2"),
			g.Group(hidden),
			g.If(v.Nonce != "", Input(ID("mv-nonce"), Type("hidden"), Name("nonce"), Value(v.Nonce))),
			Textarea(
				ID("mv-input"),
				Name("text"),
				Rows("1"),
				g.Attr("aria-label", v.Aria),
				g.Attr("enterkeyhint", "send"),
				g.If(off, g.Attr("disabled", "")),
				g.If(off, g.Attr("title", v.Disabled)),
				Placeholder(placeholder),
				Class(composerInputClass),
			),
			dictateControl(dictate, "mv-input", "mv-dictate-note", "mv-dictate"),
			Button(
				ID("mv-send"),
				Type("submit"),
				g.If(off, g.Attr("disabled", "")),
				g.Attr("aria-label", "Send"),
				Class(composerSendClass),
				g.Text("Send"),
			),
		),
		g.If(off, P(ID("mv-readonly"), Class("px-3 pb-2 text-[11px] text-muted"), g.Text(v.Disabled))),
		dictateScript(dictate),
	)
}

// mirrorComposerScript sends the composer's text without leaving the page.
//
// A 403 carrying X-Muster-Step-Up means the sign-in is not fresh: "Confirm it's
// you" is asked (window.musterStepUp, stepup_confirm.go) and on success the
// SAME text — still in the box, never cleared on a refusal — is sent again. The
// page never decides freshness; the server does.
//
// What a person sees: sent → the box clears (and the view above shows the text
// land); not sent → the reason, inline, with the text kept; nothing typed →
// MirrorComposeEmpty rather than a Send that silently does nothing.
//
// 🔴 WHEN THIS SCRIPT'S WAY FAILS, THE BROWSER'S TAKES OVER (native()). Three
// cases, each of which used to end in nothing visible or in a guess:
//
//   - the request got NO ANSWER while the browser believes it is online — a
//     sign-in redirect at an edge that a fetch cannot follow, a proxy's own
//     error. The browser's own form submission CAN follow a redirect, and shows
//     whatever the edge has to say;
//   - a stale sign-in on a page with no confirm dialog to ask through;
//   - anything this script throws.
//
// Offline is the one failure it keeps for itself: it says so and keeps the
// text, because the browser's answer to a submit with no network is an error
// page in place of the conversation.
func mirrorComposerScript() g.Node {
	return Script(g.Raw(`
(function () {
  var form = document.getElementById('mv-form');
  if (!form) return;
  var input = document.getElementById('mv-input');
  var send = document.getElementById('mv-send');
  var note = document.getElementById('mv-note');
  var nonce = document.getElementById('mv-nonce');
  function say(msg) { note.textContent = msg || ''; note.hidden = !msg; note.removeAttribute('data-ok'); }
  // A message that was sent is done with its nonce: the next one is a new
  // message, and must not be taken for a repeat of this one.
  function renew() {
    if (!nonce) return;
    // With no way to mint one the field is EMPTIED: no nonce is no protection,
    // and a reused one would have the next message taken for this one.
    nonce.value = '';
    if (!window.crypto || !crypto.getRandomValues) return;
    var b = new Uint8Array(16), s = '';
    crypto.getRandomValues(b);
    for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? '0' : '') + b[i].toString(16);
    nonce.value = s;
  }
  // A notice in the address has been shown (the server drew it): take it out,
  // so reloading the page does not repeat it.
  try {
    var here = new URL(location.href);
    if (here.searchParams.has('notice')) {
      here.searchParams.delete('notice');
      history.replaceState(history.state, '', here.pathname + here.search + here.hash);
    }
  } catch (e) {}
  // native hands the send to the browser: an ordinary form submission, which
  // the server answers with a redirect back to this view.
  function native() { HTMLFormElement.prototype.submit.call(form); }
  function body() {
    var b = new URLSearchParams();
    for (var i = 0; i < form.elements.length; i++) {
      var el = form.elements[i];
      if (el.name && !el.disabled) b.append(el.name, el.value);
    }
    return b;
  }
  function post() {
    send.disabled = true;
    // What is being sent: the box is emptied afterwards only if it still holds
    // this, so text typed while the request was out is not thrown away.
    var sent = input.value;
    var req;
    try {
      req = fetch(form.getAttribute('action'), { method: 'POST', body: body(), credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } });
    } catch (e) { native(); return; }
    return req.then(function (r) {
        if (r.ok) { if (input.value === sent) input.value = ''; say(''); renew(); return; }
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up')) {
          var stepUp = window.musterStepUp;
          if (!stepUp) { native(); return; }
          stepUp.stale();
          stepUp.ask('', post);
          return;
        }
        return r.text().then(function (t) { say(t.trim() || ('Not sent (HTTP ' + r.status + ')')); });
      }, function () {
        if (navigator.onLine === false) { say(form.getAttribute('data-down')); return; }
        native();
      })
      .then(function () { send.disabled = false; }, function () { native(); });
  }
  form.addEventListener('submit', function (e) {
    // From here on a failure of THIS handler leaves the event alone, and the
    // browser submits the form itself.
    if (input.disabled) { e.preventDefault(); return; }
    if (!input.value.trim()) { e.preventDefault(); say(form.getAttribute('data-empty')); input.focus(); return; }
    if (typeof fetch !== 'function' || typeof URLSearchParams !== 'function') return;
    e.preventDefault();
    say('');
    try { post(); } catch (err) { native(); }
  });
  input.addEventListener('input', function () { if (input.value.trim()) say(''); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      // requestSubmit is missing from older Safari; a click on Send is the same
      // submit, through the same handler.
      if (form.requestSubmit) form.requestSubmit(); else send.click();
    }
  });
})();
`))
}
