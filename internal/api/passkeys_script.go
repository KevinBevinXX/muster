package api

// The passkey pages' scripts. Inline, like every auth page's (the CSP allows
// inline script; see securityHeaders), and plain ES2017 so an installed PWA on
// an older iOS runs them.
//
// The shared helpers (mpk*: the JSON <-> ArrayBuffer conversion, the options
// prefetch) live in internal/ui (ui.PasskeyJS, ui.PasskeyPrefetchJS), because
// the "Confirm it's you" dialog there uses them too — one copy.

import "github.com/ZacxDev/muster/internal/ui"

// passkeySignInJS drives the sign-in page's "Sign in with a passkey" button.
const passkeySignInJS = ui.PasskeyPrefetchJS + `
(function () {
  var btn = document.getElementById('passkey-signin'), msg = document.getElementById('passkey-signin-status');
  if (!btn) { return; }
  if (!mpkSupported()) { btn.hidden = true; return; }
  var pre = mpkPrefetch('` + passkeySignInOptionsPath + `');
  btn.addEventListener('click', async function () {
    btn.disabled = true; mpkSay(msg, '');
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch('` + passkeySignInOptionsPath + `'); }
      var o = await pre.promise;
      pre = null;
      if (!o.ok) { mpkSay(msg, o.data.error || 'Passkey sign-in is unavailable.'); return; }
      var pk = o.data.options;
      pk.challenge = mpkB64ToBuf(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
      var cred = await navigator.credentials.get({ publicKey: pk });
      var next = (document.querySelector('input[name=next]') || {}).value || '/';
      var v = await mpkPost('` + passkeySignInVerifyPath + `', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred), next: next });
      if (!v.ok) { mpkSay(msg, v.data.error || 'That passkey was not accepted.'); return; }
      location.assign(v.data.redirect || '/');
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? 'Passkey sign-in was cancelled.' : 'Passkey sign-in failed on this device.');
    } finally {
      btn.disabled = false;
      if (!pre) { pre = mpkPrefetch('` + passkeySignInOptionsPath + `'); }
    }
  });
})();
`

// passkeyAddJS drives /account/passkeys: add a passkey. The registration
// options are prefetched on load; when the server answers reauth_required the
// password form is shown instead of the button, and a correct password
// prefetches again and brings the button back (the person taps it: creating a
// passkey needs its own gesture).
const passkeyAddJS = ui.PasskeyPrefetchJS + `
(function () {
  var btn = document.getElementById('passkey-add'), msg = document.getElementById('passkey-status'),
      form = document.getElementById('passkey-reauth'), pw = document.getElementById('passkey-password');
  if (!btn) { return; }
  if (!mpkSupported()) { btn.disabled = true; mpkSay(msg, 'This browser cannot create passkeys.'); return; }
  var path = '` + accountPasskeysPath + `/options', pre = null;
  function needReauth(r) { return r.status === 401 && r.data.error === '` + PasskeyReauthRequired + `'; }
  async function load() {
    pre = mpkPrefetch(path);
    var o = await pre.promise;
    if (needReauth(o)) { pre = null; btn.hidden = true; form.hidden = false; }
  }
  btn.addEventListener('click', async function () {
    btn.disabled = true; mpkSay(msg, '');
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
      var o = await pre.promise;
      pre = null;
      if (needReauth(o)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (!o.ok) { mpkSay(msg, o.data.error || 'A passkey cannot be added right now.'); return; }
      var pk = o.data.options;
      pk.challenge = mpkB64ToBuf(pk.challenge);
      pk.user.id = mpkB64ToBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
      var cred = await navigator.credentials.create({ publicKey: pk });
      var v = await mpkPost('` + accountPasskeysPath + `/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) });
      if (needReauth(v)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (!v.ok) { mpkSay(msg, v.data.error || 'That passkey could not be added.'); return; }
      location.assign('` + accountPasskeysPath + `');
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? 'Adding a passkey was cancelled.' : 'This device could not create a passkey.');
    } finally {
      btn.disabled = false;
      if (!pre && !btn.hidden) { pre = mpkPrefetch(path); }
    }
  });
  form.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    // The shared "confirm it's you" route (stepup.go): the session user's
    // password again, form-encoded, answered with a fresh session.
    var res = await fetch('` + stepUpConfirmPath + `', { method: 'POST', credentials: 'same-origin',
      headers: { 'Accept': 'application/json' }, body: new URLSearchParams({ password: pw.value }) });
    var r = { ok: res.ok, data: {} };
    try { r.data = await res.json(); } catch (e) {}
    pw.value = '';
    if (!r.ok) { mpkSay(msg, r.data.error || 'That password is not right.'); return; }
    form.hidden = true; btn.hidden = false;
    mpkSay(msg, 'Password confirmed. Now tap “Add a passkey on this device”.');
    pre = mpkPrefetch(path);
  });
  load();
})();
`
