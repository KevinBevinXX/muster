package api

// The passkey pages' scripts. Inline, like every auth page's (the CSP allows
// inline script; see securityHeaders), and plain ES2017 so an installed PWA on
// an older iOS runs them.
//
// ⚠ THE JSON <-> ArrayBuffer CONVERSION IS DONE BY HAND, not with
// PublicKeyCredential.parseRequestOptionsFromJSON / credential.toJSON(): those
// are missing from browsers muster's users still run (Safari before 18.x), and
// a page that works only where they exist would fail exactly on the phones the
// passkeys are for. GoTrue's options use base64url for every binary field.

// passkeyJS: the shared helpers. mpk* names keep them out of any other page
// script's way.
const passkeyJS = `
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

// mpkPrefetch: the options are fetched BEFORE the tap and kept, so the tap's
// handler reaches navigator.credentials without a network round trip in
// between. WebKit grants WebAuthn only within a user gesture, and an await on
// fetch inside the click handler can outlive it (NotAllowedError on iOS). A
// prefetched challenge is single-use and GoTrue expires it after 5 minutes, so
// one older than mpkMaxAge, or one already spent, is fetched again.
const passkeyPrefetchJS = `
var mpkMaxAge = 240000;
function mpkPrefetch(path) {
  var p = { at: Date.now(), promise: mpkPost(path, {}) };
  return p;
}
function mpkFresh(p) { return p && (Date.now() - p.at) < mpkMaxAge; }
`

// passkeySignInJS drives the sign-in page's "Sign in with a passkey" button.
const passkeySignInJS = passkeyPrefetchJS + `
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
const passkeyAddJS = passkeyPrefetchJS + `
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
