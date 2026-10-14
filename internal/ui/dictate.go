package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// DICTATION: a microphone on every message box.
//
// Tap the mic, speak, tap it again: the recording is posted to TranscribePath
// (internal/api/transcribe.go), which hands it to the speech-to-text service,
// and the text that comes back is INSERTED INTO THE BOX AT THE CARET. It is
// never sent — the operator reads it and presses Send, which is the gated
// step.
//
// 🔴 ONE CONTROL AND ONE SCRIPT, on every message box: the shared composer
// (mirrorComposer — a mirrored session's Rendered and Terminal views and an
// agent's Terminal view) and the agent chat form (agentChatPane). Each draws
// dictateControl beside its own textarea; dictateScript is document-wide and
// finds the box through the button's data-dictate-input, so there is no second
// copy to drift.
//
// 🔴 PROGRESSIVE ENHANCEMENT. The button is drawn with the `hidden` CLASS and
// only the script shows it, so a page whose script did not run has no mic at
// all (and its form still sends). A server with no speech-to-text service
// draws neither the button nor the script (an empty path).
//
// 🔴 getUserMedia NEEDS A SECURE CONTEXT. Over https (the public host) or
// localhost it works; over a plain-http LAN address the browser does not offer
// the microphone at all, and the mic says so when tapped instead of failing
// silently.

// TranscribePath is where a recording is posted.
const TranscribePath = "/transcribe"

// DictateMaxSeconds is how long one recording may run; the page stops it there
// and transcribes what it has.
const DictateMaxSeconds = 120

// dictateIdleLabel and friends are the button's accessible names per state.
const (
	dictateIdleLabel = "Dictate: record a message"
	dictateStopLabel = "Stop recording"
	dictateBusyLabel = "Transcribing…"
)

// dictateMessages are the inline notes. Spelled once, here, and handed to the
// script, so a test can read the same sentence the page shows.
const (
	DictateInsecure    = "The microphone needs a secure (https) address. Open muster at its https address to dictate."
	DictateUnsupported = "This browser cannot record audio, so dictation is not available here."
	DictateDenied      = "Microphone access was blocked. Allow it for this site in the browser's settings, then tap the mic again."
	DictateNoMic       = "No microphone was found."
	DictateMicBusy     = "The microphone is in use by another app."
	DictateNothing     = "Nothing was heard in that recording. Try again, closer to the mic."
	DictateUnavailable = "Transcription is unavailable right now. Try again in a moment."
	DictateSignedOut   = "You are signed out. Reload the page to sign in again."
	DictateEmpty       = "Nothing was recorded."
)

// dictateButtonClass is the mic's look: a 44px square target, the composer's
// surface at rest, the danger colour while recording.
const dictateButtonClass = "group hidden press min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition disabled:opacity-60 data-[state=recording]:bg-danger data-[state=recording]:text-on-accent data-[state=recording]:ring-0"

// dictateControl is the mic button for the textarea inputID, reporting into
// the note noteID. Nothing when path is "" (no speech-to-text service).
func dictateControl(path, inputID, noteID, buttonID string) g.Node {
	if path == "" {
		return nil
	}
	return Button(
		ID(buttonID),
		Type("button"),
		g.Attr("data-dictate", path),
		g.Attr("data-dictate-input", inputID),
		g.Attr("data-dictate-note", noteID),
		g.Attr("data-dictate-max", strconv.Itoa(DictateMaxSeconds)),
		g.Attr("data-state", "idle"),
		g.Attr("aria-label", dictateIdleLabel),
		g.Attr("title", "Dictate"),
		Class(dictateButtonClass),
		// Three glyphs, one shown per state (group-data-[state=…]).
		g.Raw(`<svg class="h-5 w-5 group-data-[state=recording]:hidden group-data-[state=busy]:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v1a7 7 0 0 0 14 0v-1"/><path d="M12 18v4"/></svg>`),
		g.Raw(`<svg class="hidden h-4 w-4 group-data-[state=recording]:block" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="5" y="5" width="14" height="14" rx="2"/></svg>`),
		g.Raw(`<svg class="hidden h-5 w-5 animate-spin group-data-[state=busy]:block" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-6.2-8.6"/></svg>`),
	)
}

// dictateNote is the mic's status line: the recording timer, "Transcribing…",
// and why a recording was not transcribed. Hidden until it has something to
// say. Nothing when path is "".
func dictateNote(path, noteID, class string) g.Node {
	if path == "" {
		return nil
	}
	return P(ID(noteID), g.Attr("role", "status"), g.Attr("aria-live", "polite"), g.Attr("hidden", ""),
		Class("text-xs break-words text-fg2 "+class))
}

// dictateScript wires every mic on the page. Nothing when path is "".
//
// It is safe to run more than once (a boosted navigation re-runs a swapped
// body's scripts, and the chief panel re-renders its chat): the
// document-lifetime listeners bind once (window.__musterDictate), and each run
// only reveals the buttons now on the page.
//
// 🔴 ONE RECORDING AT A TIME, AND IT IS RELEASED WHEN ITS PAGE GOES. The
// microphone is held from the first tap to the second; a navigation away (a
// boosted swap of the body, or the page being hidden) stops the recorder and
// the tracks and throws the audio away, so the browser's "recording"
// indicator never outlives the box it was for.
func dictateScript(path string) g.Node {
	if path == "" {
		return nil
	}
	msgs := map[string]string{
		"insecure": DictateInsecure, "unsupported": DictateUnsupported, "denied": DictateDenied,
		"nomic": DictateNoMic, "micbusy": DictateMicBusy, "nothing": DictateNothing,
		"unavailable": DictateUnavailable, "signedout": DictateSignedOut, "empty": DictateEmpty,
		"idle": dictateIdleLabel, "stop": dictateStopLabel, "busy": dictateBusyLabel,
	}
	js := "{"
	first := true
	for _, k := range []string{"insecure", "unsupported", "denied", "nomic", "micbusy", "nothing", "unavailable", "signedout", "empty", "idle", "stop", "busy"} {
		if !first {
			js += ","
		}
		first = false
		js += strconv.Quote(k) + ":" + strconv.Quote(msgs[k])
	}
	js += "}"
	return Script(g.Raw(`
(function () {
  var M = ` + js + `;
  function reveal(root) {
    var bs = (root && root.querySelectorAll ? root : document).querySelectorAll('[data-dictate]');
    for (var i = 0; i < bs.length; i++) { bs[i].classList.remove('hidden'); bs[i].classList.add('inline-flex'); }
  }
  reveal(document);
  if (window.__musterDictate) return;
  window.__musterDictate = true;

  // Opus in WebM first (Chrome, Firefox, Android), then MP4 (Safari, iOS).
  var TYPES = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/ogg;codecs=opus'];
  function pickType() {
    if (typeof MediaRecorder === 'undefined' || typeof MediaRecorder.isTypeSupported !== 'function') return null;
    for (var i = 0; i < TYPES.length; i++) if (MediaRecorder.isTypeSupported(TYPES[i])) return TYPES[i];
    return '';
  }
  function parts(btn) {
    return { btn: btn, input: document.getElementById(btn.getAttribute('data-dictate-input')),
      note: document.getElementById(btn.getAttribute('data-dictate-note')) };
  }
  function say(p, msg) { if (!p.note) return; p.note.textContent = msg || ''; p.note.hidden = !msg; }
  function state(p, s) {
    p.btn.setAttribute('data-state', s);
    p.btn.setAttribute('aria-label', s === 'recording' ? M.stop : s === 'busy' ? M.busy : M.idle);
    p.btn.setAttribute('aria-pressed', s === 'recording' ? 'true' : 'false');
    p.btn.disabled = s === 'busy';
  }
  function clock(s) { s = Math.max(0, Math.floor(s)); return Math.floor(s / 60) + ':' + (s % 60 < 10 ? '0' : '') + (s % 60); }

  // insert puts text into the box at its caret (replacing a selection), with a
  // space on either side where the neighbouring text needs one, and leaves the
  // caret after it, focused.
  function insert(input, text) {
    var v = input.value, a = input.selectionStart, b = input.selectionEnd;
    if (typeof a !== 'number' || typeof b !== 'number') { a = b = v.length; }
    var before = v.slice(0, a), after = v.slice(b);
    if (before && !/\s$/.test(before)) text = ' ' + text;
    if (after && !/^\s/.test(after)) text = text + ' ';
    input.value = before + text + after;
    var at = before.length + text.length;
    try { input.focus({ preventScroll: true }); } catch (e) { input.focus(); }
    try { input.setSelectionRange(at, at); } catch (e) {}
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }

  var active = null; // { p, rec, stream, chunks, t0, tick, abandoned }

  function release(s) {
    if (s.tick) { clearInterval(s.tick); s.tick = null; }
    if (s.stream) { var ts = s.stream.getTracks(); for (var i = 0; i < ts.length; i++) { try { ts[i].stop(); } catch (e) {} } }
  }
  function abandon() {
    if (!active) return;
    var s = active; active = null; s.abandoned = true;
    try { if (s.rec && s.rec.state !== 'inactive') s.rec.stop(); } catch (e) {}
    release(s);
    if (s.p.btn.isConnected) { state(s.p, 'idle'); say(s.p, ''); }
  }

  function upload(p, blob) {
    state(p, 'busy');
    say(p, M.busy);
    var ctl = typeof AbortController === 'function' ? new AbortController() : null;
    var timer = ctl ? setTimeout(function () { ctl.abort(); }, 75000) : null;
    fetch(p.btn.getAttribute('data-dictate'), {
      method: 'POST', body: blob, credentials: 'same-origin', signal: ctl ? ctl.signal : undefined,
      headers: { 'Content-Type': blob.type || 'application/octet-stream', 'X-Requested-With': 'XMLHttpRequest' }
    }).then(function (r) {
      var plain = (r.headers.get('Content-Type') || '').toLowerCase().indexOf('text/plain') === 0;
      return r.text().then(function (t) {
        if (r.ok && plain) {
          t = t.trim();
          if (!t) { say(p, M.nothing); return; }
          if (!p.input || !p.input.isConnected) { say(p, ''); return; }
          say(p, '');
          insert(p.input, t);
          return;
        }
        if (r.status === 401) { say(p, M.signedout); return; }
        // Only muster's own short sentences are shown; anything else (an edge's
        // page, a proxy's error) is not words for this box.
        if (plain && t.trim() && t.length < 400 && !/<[a-z!\/]/i.test(t)) { say(p, t.trim()); return; }
        say(p, M.unavailable + ' (HTTP ' + r.status + ')');
      });
    }, function () {
      say(p, M.unavailable);
    }).then(function () {
      if (timer) clearTimeout(timer);
      state(p, 'idle');
    });
  }

  function start(p) {
    if (!window.isSecureContext) { say(p, M.insecure); return; }
    var type = pickType();
    if (!navigator.mediaDevices || typeof navigator.mediaDevices.getUserMedia !== 'function' || type === null) { say(p, M.unsupported); return; }
    var s = { p: p, chunks: [], abandoned: false, opening: true };
    active = s;
    say(p, '');
    navigator.mediaDevices.getUserMedia({ audio: true }).then(function (stream) {
      s.stream = stream;
      s.opening = false;
      if (s.abandoned || active !== s) { release(s); return; }
      var opts = { audioBitsPerSecond: 64000 };
      if (type) opts.mimeType = type;
      var rec;
      try { rec = new MediaRecorder(stream, opts); } catch (e) {
        try { rec = new MediaRecorder(stream); } catch (e2) { active = null; release(s); say(p, M.unsupported); return; }
      }
      s.rec = rec;
      rec.addEventListener('dataavailable', function (e) { if (e.data && e.data.size > 0) s.chunks.push(e.data); });
      rec.addEventListener('stop', function () {
        release(s);
        if (active === s) active = null;
        if (s.abandoned) return;
        var blob = new Blob(s.chunks, { type: rec.mimeType || type || 'application/octet-stream' });
        if (!blob.size) { state(p, 'idle'); say(p, M.empty); return; }
        upload(p, blob);
      });
      var max = Number(p.btn.getAttribute('data-dictate-max')) || 120;
      s.t0 = Date.now();
      var show = function () {
        var el = (Date.now() - s.t0) / 1000;
        say(p, '● Recording ' + clock(el) + ' / ' + clock(max) + ' — tap the mic again to stop');
        if (el >= max && rec.state === 'recording') rec.stop();
      };
      rec.start(1000);
      state(p, 'recording');
      show();
      s.tick = setInterval(show, 250);
    }, function (err) {
      if (active === s) active = null;
      var n = err && err.name;
      say(p, n === 'NotAllowedError' || n === 'SecurityError' ? M.denied
        : n === 'NotFoundError' || n === 'OverconstrainedError' ? M.nomic
        : n === 'NotReadableError' || n === 'AbortError' ? M.micbusy : M.unsupported);
      state(p, 'idle');
    });
  }

  // Pressing the mic does not take focus from the box: the caret stays where
  // the text will go, and on a phone the on-screen keyboard stays up rather
  // than closing (and resizing the page) for every recording.
  document.addEventListener('mousedown', function (e) {
    if (e.target && e.target.closest && e.target.closest('[data-dictate]')) e.preventDefault();
  });
  document.addEventListener('click', function (e) {
    var btn = e.target && e.target.closest ? e.target.closest('[data-dictate]') : null;
    if (!btn || btn.disabled) return;
    e.preventDefault();
    var p = parts(btn);
    if (active) {
      if (active.p.btn !== btn) { abandon(); start(p); return; }
      if (active.opening) return;
      try { active.rec.stop(); } catch (err) { abandon(); }
      return;
    }
    start(p);
  });
  // A boosted navigation swaps the body away: the recording goes with it.
  document.addEventListener('htmx:beforeSwap', function (e) {
    if (active && e.detail && e.detail.target && e.detail.target.contains(active.p.btn)) abandon();
  });
  document.addEventListener('htmx:load', function (e) { reveal(e.target); });
  window.addEventListener('pagehide', abandon);
})();
`))
}
