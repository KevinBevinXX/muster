package ui

import (
	"encoding/json"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// MESSAGE DRAFTS, PER DEVICE: what is typed into a message box is kept in this
// browser's localStorage as it is typed (debounced), restored when the same box
// is drawn again — a navigation away and back, a reload — and cleared once it
// is sent or queued.
//
// Every message box that sends somewhere has one (ComposerView.DraftKey, and
// the agent chat's #chat-input), each keyed by the signed-in user first:
//
//   - a mirrored window's Rendered view (#mv-input): MirrorDraftKey(user, "view", …)
//   - a mirrored window's Terminal view (#mv-input): MirrorDraftKey(user, "terminal", …)
//   - a dispatched agent's chat, and the chief panel that draws the same pane
//     (#chat-input): AgentChatDraftKey(user, agent, chat session)
//   - a dispatched agent's Terminal view (#mv-input): AgentTerminalDraftKey(user, agent)
//
// 🔴 THE KEY IS THE ADDRESS THE BOX SENDS TO, EVERY PART OF IT, AND IT IS MADE
// HERE, ONCE. A draft is restored only into a box whose key is the same string,
// so a draft typed for one window is never offered in another window, another
// session, another host — or the other view of the same window, whose Send
// reaches a different pane. The parts are a JSON array, so no value can be
// spelled to look like another's (a host named "a|b" is not host "a", session
// "b").
//
// 🔴 THE USER IS THE FIRST PART OF EVERY KEY (v2). localStorage belongs to the
// origin, not to an account, so two accounts signed in one after the other in
// the same browser share the store: without the user in the key, one account's
// unsent text was offered to the other on the same address. A v1 key (no user)
// is DROPPED by the script on its first run, never migrated: whose it was is
// unknowable. A draft never leaves the browser; the server never sees it.
//
// Bounds: at most DraftMaxChars characters are kept (a longer text is not
// stored, and an older draft of it is removed rather than restored stale), at
// most DraftMaxCount drafts (the oldest go first), and a draft not touched for
// DraftTTLDays is removed. Every storage call is wrapped: a browser that refuses
// storage (a private window, a full quota) just has no drafts.

// DraftKeyPrefix starts every draft's localStorage key.
const DraftKeyPrefix = "muster.draft.v2:"

// draftKeyLegacyPrefix is v1's prefix (keys with no user), dropped on sight.
const draftKeyLegacyPrefix = "muster.draft.v1:"

// The drafts' bounds.
const (
	// DraftMaxChars is mirror.MaxPaste (16 KiB) in characters: a longer text
	// cannot be sent anyway.
	DraftMaxChars = 16384
	DraftMaxCount = 50
	DraftTTLDays  = 7
)

// draftKey is the storage key for user's box at an address: the prefix, then
// the user and the address's parts as one JSON array.
func draftKey(user int64, parts ...string) string {
	all := make([]any, 0, len(parts)+1)
	all = append(all, user)
	for _, p := range parts {
		all = append(all, p)
	}
	b, err := json.Marshal(all)
	if err != nil {
		return ""
	}
	return DraftKeyPrefix + string(b)
}

// MirrorDraftKey is user's draft key of a mirrored window's message box: view is
// "view" (Rendered) or "terminal", then the host, the tmux session and the
// window id ("" for a per-session host's session).
func MirrorDraftKey(user int64, view, host, session, window string) string {
	return draftKey(user, "mirror", view, host, session, window)
}

// AgentChatDraftKey is the draft key of a dispatched agent's chat box, in
// chat session id.
func AgentChatDraftKey(user int64, agent string, session int64) string {
	return draftKey(user, "agent", agent, strconv.FormatInt(session, 10))
}

// AgentTerminalDraftKey is the draft key of a dispatched agent's Terminal view
// message box (it sends to the agent's current chat session).
func AgentTerminalDraftKey(user int64, agent string) string {
	return draftKey(user, "agent-terminal", agent)
}

// draftAttrs are a message box's draft attributes: its key, and the id of the
// status line a send the browser submitted comes back to (where an OK notice
// says it was delivered). Nothing for a box with no key.
func draftAttrs(key, note string) g.Node {
	if key == "" {
		return nil
	}
	return g.Group([]g.Node{g.Attr("data-draft-key", key), g.Attr("data-draft-note", note)})
}

// composerDraftScript keeps the drafts. Like composerFitScript it ships beside
// each box (the chief panel swaps the chat pane into another page) and defines
// window.musterDraft once; every copy then binds the boxes it finds that are
// not bound yet, so a box swapped in later is covered.
//
// 🔴 IT RUNS RIGHT AFTER THE BOX'S FORM, BEFORE THE PAGE PAINTS IT EMPTY: a
// restored text sizes the box (composer_fit.go) in the same task that draws
// it, so restoring is not a layout shift (e2e/tests/cls.spec.ts measures it).
//
// A send clears the draft through window.musterDraft.sent(box), which the
// senders (mirrorComposerScript, agentChatScript) call once the server — or
// the socket — has taken the message; whatever is left in the box then is
// kept. A send handed to the BROWSER (its own form submission) is marked
// pending in sessionStorage; the page it comes back to clears the draft only
// when it says the message went (an OK notice) and the draft is still that
// text.
func composerDraftScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (!window.musterDraft) {
    var PREFIX = ` + strconv.Quote(DraftKeyPrefix) + `, LEGACY = ` + strconv.Quote(draftKeyLegacyPrefix) + `;
    var MAX = ` + strconv.Itoa(DraftMaxChars) + `, MAXN = ` + strconv.Itoa(DraftMaxCount) + `;
    var TTL = ` + strconv.Itoa(DraftTTLDays) + ` * 24 * 3600 * 1000;
    var PENDING = 'muster.draft.pending';
    var DEBOUNCE = 400;
    var timers = {};
    function ls() { try { return window.localStorage || null; } catch (e) { return null; } }
    function read(key) {
      var s = ls(); if (!s) return null;
      try {
        var d = JSON.parse(s.getItem(key) || 'null');
        if (!d || typeof d.t !== 'string' || typeof d.at !== 'number') return null;
        if (Date.now() - d.at > TTL) { s.removeItem(key); return null; }
        return d;
      } catch (e) { return null; }
    }
    function remove(key) { var s = ls(); if (!s) return; try { s.removeItem(key); } catch (e) {} }
    // sweep removes stale and unreadable drafts, and the oldest past MAXN.
    function sweep() {
      var s = ls(); if (!s) return;
      try {
        var all = [];
        for (var i = 0; i < s.length; i++) {
          var k = s.key(i);
          if (k && k.indexOf(PREFIX) === 0) all.push(k);
          else if (k && k.indexOf(LEGACY) === 0) all.push(k);
        }
        var live = [];
        all.forEach(function (k) {
          var d = null;
          try { d = JSON.parse(s.getItem(k) || 'null'); } catch (e) {}
          // A v1 draft (no user in its key) is dropped: whose it was is unknowable.
          if (k.indexOf(LEGACY) === 0 || !d || typeof d.t !== 'string' || typeof d.at !== 'number' || Date.now() - d.at > TTL) s.removeItem(k);
          else live.push({ k: k, at: d.at });
        });
        live.sort(function (a, b) { return a.at - b.at; });
        while (live.length > MAXN) s.removeItem(live.shift().k);
      } catch (e) {}
    }
    function keyOf(el) { return el && el.getAttribute ? el.getAttribute('data-draft-key') : null; }
    // save stores el's text now: an empty box removes its draft, and a text
    // past MAX removes it too (it could not be sent; an older shorter draft
    // must not come back in its place).
    function save(el) {
      var key = keyOf(el); if (!key) return;
      clearTimeout(timers[key]); delete timers[key];
      var t = el.value || '';
      if (!t.trim() || t.length > MAX) { remove(key); return; }
      var s = ls(); if (!s) return;
      try { s.setItem(key, JSON.stringify({ t: t, at: Date.now() })); } catch (e) { return; }
      sweep();
    }
    function later(el) {
      var key = keyOf(el); if (!key) return;
      clearTimeout(timers[key]);
      timers[key] = setTimeout(function () { save(el); }, DEBOUNCE);
    }
    // flush saves every box with a save waiting: the page is going away.
    function flush() {
      var els = document.querySelectorAll('textarea[data-draft-bound]');
      for (var i = 0; i < els.length; i++) { if (timers[keyOf(els[i])]) save(els[i]); }
    }
    // pendingFor is the send that box key handed to the browser, if there is
    // one and it is recent; it is consumed by the box it names (or dropped
    // once stale).
    function pendingFor(key) {
      try {
        var p = JSON.parse(window.sessionStorage.getItem(PENDING) || 'null');
        if (!p || p.k !== key) {
          if (p && !(Date.now() - p.at < 120000)) window.sessionStorage.removeItem(PENDING);
          return null;
        }
        window.sessionStorage.removeItem(PENDING);
        return Date.now() - p.at < 120000 ? p : null;
      } catch (e) { return null; }
    }
    function bind(el) {
      if (el.hasAttribute('data-draft-bound')) return;
      el.setAttribute('data-draft-bound', '');
      var key = keyOf(el);
      // A send this box handed to the browser: cleared if it went.
      var p = pendingFor(key);
      if (p) {
        var note = document.getElementById(el.getAttribute('data-draft-note') || '');
        var d0 = read(key);
        if (note && note.hasAttribute('data-ok') && !note.hidden && d0 && d0.t === p.t) remove(key);
      }
      if (!el.value) {
        var d = read(key);
        if (d && d.t) {
          el.value = d.t;
          el.setAttribute('data-draft-restored', '');
          if (window.musterComposerFit) window.musterComposerFit(el);
        }
      }
      el.addEventListener('input', function () { later(el); });
      el.addEventListener('focusout', function () { if (timers[key]) save(el); });
    }
    window.musterDraft = {
      scan: function () {
        sweep();
        var els = document.querySelectorAll('textarea[data-draft-key]');
        for (var i = 0; i < els.length; i++) bind(els[i]);
      },
      // sent: el's message was taken. What is left in the box (typed while it
      // was going) is kept; an empty box has no draft.
      sent: function (el) { if (keyOf(el)) save(el); },
      // pending: el's message is being handed to the browser's own submit.
      pending: function (el) {
        var key = keyOf(el); if (!key) return;
        try { window.sessionStorage.setItem(PENDING, JSON.stringify({ k: key, t: el.value, at: Date.now() })); } catch (e) {}
      },
      flush: flush
    };
    window.addEventListener('pagehide', flush);
    document.addEventListener('visibilitychange', function () { if (document.hidden) flush(); });
    // A boosted navigation fires no pagehide.
    document.addEventListener('htmx:beforeRequest', flush);
  }
  window.musterDraft.scan();
})();
`))
}
