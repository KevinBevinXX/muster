package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// "CONFIRM IT'S YOU" — the one dialog and the one script behind every step-up
// prompt (internal/stepup): the mirrored Terminal's Interactive switch, and the
// message composer on both the Terminal and the Rendered view.
//
// 🔴 ONE COPY. The terminal page and the rendered view each used to carry their
// own submit handler for this dialog; a second way to confirm (a passkey) would
// have had to be written twice and kept in step. Both pages now render
// confirmDialog and include stepUpScript, and reach it through
// window.musterStepUp.
//
// ⚠ THE PAGE'S VIEW OF FRESHNESS IS A CONVENIENCE, NOT THE GATE. musterStepUp
// only decides whether to ask BEFORE trying; the server refuses a stale write
// (403) whatever the page believed, and the page then asks.

// StepUpView is what a page needs to ask "Confirm it's you" before a gated
// action.
type StepUpView struct {
	// ConfirmPath is the same-origin route that takes the password again.
	ConfirmPath string
	// PasskeyPath is the same-origin route PREFIX of the passkey confirm
	// (<prefix>/options, <prefix>/verify); "" when this server has no passkeys
	// (password mode, or GoTrue's passkeys off), and the dialog is then
	// password-only. Whether THIS account has a passkey is asked when the dialog
	// opens — never on page load, which would spend a GoTrue call on every view.
	PasskeyPath string
	// FreshFor is how many milliseconds the current sign-in stays fresh, as
	// the SERVER measured it when it rendered the page (0 = not fresh). A
	// duration rather than an instant, so the browser's clock does not matter.
	FreshFor int64
	// RememberKey names what an Interactive choice is remembered for, in this
	// browser session (sessionStorage): "mirror:<host>" — one choice per host.
	// "" on a page with no Interactive switch.
	RememberKey string
	// Window is the freshness window as a person reads it ("12 hours") —
	// stepup.WindowText, passed in so this package states no window of its own.
	Window string
}

// stepUpAttrs carries a StepUpView to the terminal script on #term; nothing for
// nil.
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

// StepUpWhy is the dialog's sentence about the gate, with the window in it.
func StepUpWhy(window string) string {
	return "Typing into this session needs a sign-in from the last " + window + "."
}

// The dialog's "how" line, in its two states. The server renders the password
// one; stepUpScript swaps in the passkey one once it knows the account has a
// passkey this browser can use.
const (
	stepUpHowPassword = "Enter your password to continue."
	stepUpHowPasskey  = "Confirm with your passkey, or use your password."
)

const (
	stepUpPrimary   = "bg-accent text-on-accent hover:bg-accent/90"
	stepUpSecondary = "bg-bg text-fg ring-1 ring-inset ring-edge"
)

// confirmDialog is "Confirm it's you": a passkey or the password again before a
// gated action. stepUpScript posts to the StepUpView's routes (same origin,
// credentials included); the server's answer replaces the session's cookies
// with a freshly signed-in one.
//
// The passkey button is drawn HIDDEN: it is shown, as the primary action, only
// once the server has said this account has a passkey and the browser can use
// one. The password field is always there — the fallback is never a tap away.
func confirmDialog(s *StepUpView) g.Node {
	if s == nil {
		return nil
	}
	return Dialog(
		ID("term-confirm"),
		g.Attr("aria-labelledby", "term-confirm-title"),
		g.Attr("data-confirm", s.ConfirmPath),
		g.Attr("data-passkey", s.PasskeyPath),
		g.Attr("data-fresh-for", strconv.FormatInt(s.FreshFor, 10)),
		g.Attr("data-how-password", stepUpHowPassword),
		g.Attr("data-how-passkey", stepUpHowPasskey),
		Class("w-[calc(100%-2rem)] max-w-sm rounded-2xl bg-s1 p-0 text-fg ring-1 ring-line backdrop:bg-black/60"),
		Form(
			ID("term-confirm-form"),
			Class("flex flex-col gap-4 px-5 py-6"),
			H2(ID("term-confirm-title"), Class("text-base font-semibold text-fg"), g.Text("Confirm it's you")),
			P(ID("term-confirm-why"), Class("text-sm text-fg2"),
				g.Text(StepUpWhy(s.Window)+" "), Span(ID("term-confirm-how"), g.Text(stepUpHowPassword))),
			Div(
				ID("term-confirm-passkey-box"),
				g.Attr("hidden", ""),
				Class("flex flex-col gap-3"),
				Button(ID("term-confirm-passkey"), Type("button"),
					Class("press min-h-[44px] rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
					g.Text("Confirm with passkey")),
				P(Class("text-center text-xs text-muted"), g.Text("or use your password")),
			),
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
					Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
					g.Text("Cancel")),
				Button(ID("term-confirm-submit"), Type("submit"),
					g.Attr("data-primary", stepUpPrimary),
					g.Attr("data-secondary", stepUpSecondary),
					Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
					g.Text("Confirm")),
			),
		),
	)
}

// StepUpFormView is the plain "Confirm it's you" page: the step-up for a send
// the BROWSER submitted (no script ran), which the dialog above cannot serve.
type StepUpFormView struct {
	// Action is the route the page posts to: it checks the password and, when
	// it is right, performs the send that was waiting.
	Action string
	// Fields are the pending send, carried as hidden inputs — its address, its
	// text, its nonce — so confirming completes it rather than asking for the
	// message again.
	Fields []ComposerField
	// BackHref is the view the send came from (Cancel, and where the result is
	// shown).
	BackHref string
	// Window is the freshness window as a person reads it ("12 hours").
	Window string
	// Error is why the last attempt was not accepted; "" on the first showing.
	Error string
}

// StepUpFormPage is a whole document with one form: the password again, and
// the pending send in hidden fields. No script is needed to use it — it exists
// for the page whose script did not run. (A passkey needs script; the dialog on
// a working page offers one.)
func StepUpFormPage(v StepUpFormView) g.Node {
	hidden := make([]g.Node, 0, len(v.Fields))
	for _, f := range v.Fields {
		hidden = append(hidden, Input(Type("hidden"), Name(f.Name), Value(f.Value)))
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Confirm it's you · muster")),
				Link(Rel("stylesheet"), Href("/static/app.css")),
				faroHead(),
			),
			Body(
				Class("flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased"),
				Form(
					ID("stepup-form"),
					Method("post"),
					Action(v.Action),
					Class("flex w-full max-w-sm flex-col gap-4 rounded-2xl bg-s1 px-5 py-6 ring-1 ring-line"),
					g.Group(hidden),
					H1(Class("text-base font-semibold text-fg"), g.Text("Confirm it's you")),
					P(Class("text-sm text-fg2"), g.Text(StepUpWhy(v.Window)+" "+stepUpHowPassword+" Your message is sent once you have.")),
					Label(For("stepup-password"), Class("text-sm font-medium text-fg"), g.Text("Password")),
					Input(
						ID("stepup-password"),
						Name("password"),
						Type("password"),
						g.Attr("autocomplete", "current-password"),
						g.Attr("required", ""),
						g.Attr("autofocus", ""),
						Class("min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
					),
					g.If(v.Error != "", P(ID("stepup-error"), g.Attr("role", "alert"),
						Class("rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"), g.Text(v.Error))),
					Div(
						Class("flex gap-2"),
						A(ID("stepup-cancel"), Href(v.BackHref),
							Class("press inline-flex min-h-[44px] flex-1 items-center justify-center rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
							g.Text("Cancel")),
						Button(ID("stepup-submit"), Type("submit"),
							Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
							g.Text("Confirm and send")),
					),
				),
				// Like every document: the worker and the update toast. The form
				// above needs neither.
				pwaChrome(),
			),
		),
	)
}

// PasskeyJS is the WebAuthn helpers every passkey script on every page shares:
// the sign-in and account pages (internal/api) and the confirm dialog here.
// mpk* names keep them out of any other page script's way.
//
// ⚠ THE JSON <-> ArrayBuffer CONVERSION IS DONE BY HAND, not with
// PublicKeyCredential.parseRequestOptionsFromJSON / credential.toJSON(): those
// are missing from browsers muster's users still run (Safari before 18.x), and
// a page that works only where they exist would fail exactly on the phones the
// passkeys are for. GoTrue's options use base64url for every binary field.
const PasskeyJS = `
function mpkB64ToBuf(s) {
  s = String(s).replace(/-/g, '+').replace(/_/g, '/');
  while (s.length % 4) { s += '='; }
  var bin = atob(s), out = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
  return out.buffer;
}
function mpkBufToB64(buf) {
  var b = new Uint8Array(buf), s = '';
  for (var i = 0; i < b.length; i++) { s += String.fromCharCode(b[i]); }
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function mpkCredJSON(cred) {
  var r = cred.response, out = {
    id: cred.id, rawId: mpkBufToB64(cred.rawId), type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response: { clientDataJSON: mpkBufToB64(r.clientDataJSON) }
  };
  if (r.attestationObject) {
    out.response.attestationObject = mpkBufToB64(r.attestationObject);
    if (r.getTransports) { out.response.transports = r.getTransports(); }
  } else {
    out.response.authenticatorData = mpkBufToB64(r.authenticatorData);
    out.response.signature = mpkBufToB64(r.signature);
    if (r.userHandle) { out.response.userHandle = mpkBufToB64(r.userHandle); }
  }
  return out;
}
async function mpkPost(path, body) {
  var res = await fetch(path, { method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
    body: JSON.stringify(body || {}) });
  var data = {};
  try { data = await res.json(); } catch (e) {}
  return { ok: res.ok, status: res.status, data: data };
}
function mpkSupported() {
  return !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext);
}
function mpkSay(el, msg) { if (el) { el.textContent = msg; } }
function mpkCancelled(e) { return e && (e.name === 'NotAllowedError' || e.name === 'AbortError'); }
`

// PasskeyPrefetchJS: WebAuthn options are fetched BEFORE the tap and kept, so
// the tap's handler reaches navigator.credentials without a network round trip
// in between. WebKit grants WebAuthn only within a user gesture, and an await
// on fetch inside the click handler can outlive it (NotAllowedError on iOS). A
// prefetched challenge is single-use and GoTrue expires it after 5 minutes, so
// one older than mpkMaxAge, or one already spent, is fetched again.
const PasskeyPrefetchJS = `
var mpkMaxAge = 240000;
function mpkPrefetch(path) {
  var p = { at: Date.now(), promise: mpkPost(path, {}) };
  return p;
}
function mpkFresh(p) { return p && (Date.now() - p.at) < mpkMaxAge; }
`

// stepUpScript defines window.musterStepUp on a page that rendered
// confirmDialog (and leaves it undefined on one that did not):
//
//	fresh()        the page's belief that the sign-in is still fresh
//	stale()        the server just said otherwise
//	ask(why, done) open the dialog; done() runs after a successful confirm —
//	               the pending action (attach, or send the text again)
//
// 🔴 THE PASSKEY OPTIONS ARE FETCHED WHEN THE DIALOG OPENS, NOT ON PAGE LOAD.
// The sign-in page prefetches on load because signing in is what that page is
// for; these pages are opened all day by someone already fresh, and a challenge
// minted per page view would spend GoTrue's passkey rate budget on dialogs that
// never open. One options call per dialog open (plus one after a failed
// attempt, whose challenge is spent), and only when the account has a passkey:
// the server answers the same call with available:false, and no challenge, when
// it has none.
//
// The tap on "Confirm with passkey" then reaches navigator.credentials on an
// already-resolved promise — no network between the gesture and WebAuthn (see
// PasskeyPrefetchJS).
func stepUpScript() g.Node {
	return Script(g.Raw(PasskeyJS + PasskeyPrefetchJS + `
window.musterStepUp = (function () {
  var dlg = document.getElementById('term-confirm');
  if (!dlg) return null;
  var path = dlg.getAttribute('data-confirm');
  var pkBase = dlg.getAttribute('data-passkey') || '';
  var form = document.getElementById('term-confirm-form');
  var pw = document.getElementById('term-confirm-password');
  var err = document.getElementById('term-confirm-error');
  var submit = document.getElementById('term-confirm-submit');
  var how = document.getElementById('term-confirm-how');
  var pkBox = document.getElementById('term-confirm-passkey-box');
  var pkBtn = document.getElementById('term-confirm-passkey');
  var done = null, pre = null, freshUntil = 0;
  var ff = +dlg.getAttribute('data-fresh-for') || 0;
  if (ff > 0) freshUntil = performance.now() + ff;

  function fail(msg) { err.textContent = msg || ''; err.hidden = !msg; }
  function swap(el, from, to) {
    from.split(' ').forEach(function (c) { el.classList.remove(c); });
    to.split(' ').forEach(function (c) { el.classList.add(c); });
  }
  // offer shows (or hides) the passkey as the PRIMARY action; the password
  // button steps down to secondary beside it and back up without it.
  function offer(on) {
    pkBox.hidden = !on;
    dlg.setAttribute('data-passkey-offered', on ? '1' : '0');
    how.textContent = dlg.getAttribute(on ? 'data-how-passkey' : 'data-how-password');
    var pri = submit.getAttribute('data-primary'), sec = submit.getAttribute('data-secondary');
    if (on) { swap(submit, pri, sec); submit.textContent = 'Use password'; }
    else { swap(submit, sec, pri); submit.textContent = 'Confirm'; }
  }
  function prefetch() {
    var mine = pre = mpkPrefetch(pkBase + '/options');
    return mine.promise.then(function (o) {
      if (pre !== mine) return false;
      var ok = !!(o.ok && o.data && o.data.available && o.data.challenge_id);
      if (!ok) pre = null;
      return ok;
    }, function () { if (pre === mine) pre = null; return false; });
  }
  function finish(freshFor) {
    freshUntil = performance.now() + (+freshFor || 0);
    pw.value = '';
    var cb = done; done = null;
    dlg.close();
    if (cb) cb();
  }
  function ask(why, cb) {
    done = cb || null;
    fail(why);
    pw.value = '';
    offer(false);
    if (!dlg.open) dlg.showModal();
    pw.focus();
    if (!pkBase || !mpkSupported()) return;
    prefetch().then(function (ok) {
      if (!ok || !dlg.open) return;
      offer(true);
      pkBtn.focus();
    });
  }

  document.getElementById('term-confirm-cancel').addEventListener('click', function () { done = null; dlg.close(); });
  dlg.addEventListener('close', function () { pre = null; });
  form.addEventListener('submit', function (ev) {
    ev.preventDefault();
    submit.disabled = true;
    fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json' },
      body: new URLSearchParams({ password: pw.value })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; });
    }).then(function (res) {
      submit.disabled = false;
      if (!res.ok) {
        fail(res.j.error || 'That could not be confirmed.');
        pw.value = '';
        pw.focus();
        return;
      }
      finish(res.j.fresh_for);
    }, function () {
      submit.disabled = false;
      fail('muster could not be reached. Try again.');
    });
  });
  pkBtn.addEventListener('click', async function () {
    pkBtn.disabled = true;
    fail('');
    var spent = false;
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch(pkBase + '/options'); }
      var o = await pre.promise;
      pre = null; spent = true;
      if (!o.ok || !o.data.available || !o.data.challenge_id) {
        offer(false);
        fail(o.data.error || 'A passkey cannot be used right now. Use your password.');
        return;
      }
      var pk = o.data.options;
      pk.challenge = mpkB64ToBuf(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
      var cred = await navigator.credentials.get({ publicKey: pk });
      var v = await mpkPost(pkBase + '/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) });
      if (!v.ok) { fail(v.data.error || 'That passkey was not accepted.'); return; }
      spent = false;
      finish(v.data.fresh_for);
    } catch (e) {
      fail(mpkCancelled(e) ? 'The passkey was cancelled. Try again, or use your password.' : 'This device could not use a passkey. Use your password.');
    } finally {
      pkBtn.disabled = false;
      // The challenge is single-use: a failed attempt needs a new one before
      // the next tap (fetched now, so that tap still has its gesture).
      if (spent && dlg.open && !pkBox.hidden) {
        prefetch().then(function (ok) { if (!ok && dlg.open) offer(false); });
      }
    }
  });

  return {
    // A margin, so an action is not tried in the last seconds of freshness.
    fresh: function () { return performance.now() < freshUntil - 10000; },
    stale: function () { freshUntil = 0; },
    ask: ask
  };
})();
`))
}
