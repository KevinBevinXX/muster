package ui

import (
	"encoding/json"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE AGENTS LIST'S FILTER: a search box above the cards that narrows them as
// you type. With every tmux window a card of its own the list runs past a
// hundred entries; finding "Gold:8" should not mean scrolling for it.
//
// Each card carries what it can be found by in `data-filter` (filterHaystack):
// its title, its raw session name, its host, what runs in it, and — for an
// agent — its name, repository, kind and status. The box matches
// case-insensitive substrings; several words must ALL match.
//
// 🔴 THE HIDING IS A STYLESHEET RULE, NOT AN ATTRIBUTE ON EACH CARD — and that
// is what makes it survive the list's live refresh. #agents-list re-renders
// itself (an htmx morph) whenever a host nudges it, which with many windows is
// often. A filter that marked cards `hidden` would have its marks morphed away
// with each refresh and every card would flash back until a script re-applied
// them. Instead the script writes ONE rule per word,
//
//	#agents-list > article:not([data-filter*="word"]) { display: none }
//
// into a <style> in the document head. A refreshed card is hidden by the same
// rule the moment it exists; nothing has to notice the refresh, the input is
// outside the list and is never touched by it, and there is no per-card work
// at all. Only the "N of M" count is recomputed when the list settles — with
// the SAME attribute selector (agentsFilterScript's `sel`), so what is counted
// is what is shown.
//
// FIELD TOKENS. Besides the free-text haystack each card carries ONE attribute
// per field, `data-f-<field>` (cardFilterAttrs, over cardFieldRegistry), and the
// box understands a typed token for each:
//
//	word            [data-filter*="word"]       anywhere on the card (as before)
//	"two words"     [data-filter*="two words"]  a phrase
//	host:tower      [data-f-host*="tower"]      that field only, as a substring
//	host=tower      [data-f-host="tower"]       that field only, EXACTLY
//	kind:"claude code"                          a quoted value may hold spaces
//
// Terms are whitespace-separated and ALL must match. A `field:value` matches as
// a case-insensitive SUBSTRING of that field — the same rule as a free word, so
// `host:tow` behaves like typing `tow`, only narrower. A `field=value` matches
// the whole field, so `host=mac` does not also show host "macbook" (the host
// TABS below match the same way). A token whose prefix is
// NOT a registered field is free text, colon and all: a window called
// "Gold:8" is found by typing `gold:8`, and that must keep working. A known
// field with nothing after the colon (`host:` mid-typing) is no term yet, so the
// list does not blank while the value is being typed. The rule per term is
//
//	#agents-list > article:not([data-f-host*="tower"]) { display: none }
//
// so a field token survives a refresh for the same reason a word does. The
// field NAME in a selector comes only from the registry (a closed set of plain
// identifiers, pinned by a test); the VALUE is escaped.
//
// The filter's text is also the page's `?q=` (history.replaceState, so typing
// adds no history entries). On load a `?q=` wins over the session's remembered
// copy, so a shared link reproduces the filter.
//
// HOST TABS. Above the box, one tab per host the list holds — "All", then
// each host's name (dispatched pod agents are the host "cluster", podHost) —
// each with how many of its cards match the box. A tab is a FILTER OVER THE ONE
// LIST, not a panel of its own: choosing one adds one more hide rule,
//
//	#agents-list > article:not([data-f-host="tower"]) { display: none }
//
// EXACT (a host "mac" never shows "macbook"), ANDed with the box's terms the
// same way the box's terms AND with each other, so it survives a refresh for
// the same reason they do. The tabs are built by the script from the cards the
// list DRAWS now (an arrival held behind the "N new ↑" pill offers no tab until
// it is drawn) and rebuilt whenever the list re-renders, so a host that
// connects gets a tab and one that leaves loses it.
//
// The choice is the browser's, not the link's: it is kept in localStorage
// ("muster:tabs:agents-host", the prefix tabs.go uses) and is NOT part of ?q=.
// While the remembered host is not on the list (a restart, a laptop asleep)
// the list SHOWS All and keeps the choice, and is filtered to that host again
// when it is back; only the reader's own pick changes it. A link carrying its
// own ?q= opens on All. The "N new ↑" hold counts only cards the filter (tab
// AND box) shows, so a refresh that changes only hidden ones is applied at
// once and the other hosts' tabs and counts stay live.
//
// The tabs are tabs.go's WAI-ARIA tabs in markup, look (tabClass) and keys —
// role=tablist/tab, aria-selected on exactly one, a roving tabindex, Left/Right
// and Home/End move AND select — but NOT its script: tabs.go hides the panel
// each tab aria-controls, and every host tab controls the same list.
//
// The box keeps the whole token grammar above (`host:tow`, `kind=shell`, …);
// the four dropdowns (host, type, status, kind) that once wrote it for you are
// gone — the host is the tab, and the rest are a typed token.

// cardFields is what a card can be filtered by, one value per field. Empty
// fields write no attribute.
//
// 🔴 HOST FOR A DISPATCHED POD AGENT IS THE LITERAL "cluster" (podHost): muster
// has no cluster name to put there, and naming it after a machine would merge
// "my tmux on that machine" with "pods on that cluster" — the very cut host:
// exists to make. `ns:` is the finer cut for pods.
type cardFields struct {
	Host, Type, Name, Status, Kind, Repo, Account, Session, NS, Mode, State string
	// Titles are what the card is CALLED — an agent's display name and name, a
	// window's label, title and session — in the order the card shows them. They
	// rank a match (rankAttrs); they filter nothing.
	Titles []string
}

// podHost is a dispatched agent's host field.
const podHost = "cluster"

// cardField is one registered field: the token's name (and the attribute's,
// as data-f-<Key>) and where its value comes from.
//
// THIS IS THE ONE PLACE A FIELD IS DECLARED. The cards' attributes and the
// script's set of known fields (FIELDS) are both derived from it, so a field
// one side knows the other cannot miss. A later source of fields (plugin-declared
// ones) is more entries here, not another attribute builder.
type cardField struct {
	Key string
	Of  func(cardFields) string
}

var cardFieldRegistry = []cardField{
	{"host", func(f cardFields) string { return f.Host }},
	{"name", func(f cardFields) string { return f.Name }},
	{"status", func(f cardFields) string { return f.Status }},
	{"kind", func(f cardFields) string { return f.Kind }},
	{"repo", func(f cardFields) string { return f.Repo }},
	{"account", func(f cardFields) string { return f.Account }},
	{"session", func(f cardFields) string { return f.Session }},
	{"ns", func(f cardFields) string { return f.NS }},
	{"mode", func(f cardFields) string { return f.Mode }},
	{"state", func(f cardFields) string { return f.State }},
	{"type", func(f cardFields) string { return f.Type }},
}

// cardFieldKeysJSON is the registry's field names as a JS array literal.
func cardFieldKeysJSON() string {
	keys := make([]string, 0, len(cardFieldRegistry))
	for _, f := range cardFieldRegistry {
		keys = append(keys, f.Key)
	}
	b, _ := json.Marshal(keys)
	return string(b)
}

// fieldValue is a field as written into its attribute: lower-cased, its
// whitespace collapsed to single spaces — what the script does to a typed value.
func fieldValue(v string) string { return strings.Join(strings.Fields(strings.ToLower(v)), " ") }

// cardFilterAttrs is EVERY filter attribute of a card, for both card kinds:
// the free-text `data-filter` haystack (exactly filterHaystack of the parts),
// one `data-f-<field>` per non-empty field and, for a labelled field
// (labelledFields) whose value lower-casing changed, `data-l-<field>`: the
// value as the card spells it, which is what its host tab says ("Tower", not
// "tower").
func cardFilterAttrs(f cardFields, haystack ...string) g.Node {
	attrs := g.Group{g.Attr("data-filter", filterHaystack(haystack...))}
	for _, fd := range cardFieldRegistry {
		raw := strings.Join(strings.Fields(fd.Of(f)), " ")
		v := fieldValue(raw)
		if v == "" {
			continue
		}
		attrs = append(attrs, g.Attr("data-f-"+fd.Key, v))
		if raw != v && isLabelled(fd.Key) {
			attrs = append(attrs, g.Attr("data-l-"+fd.Key, raw))
		}
	}
	return append(attrs, rankAttrs(f.Titles)...)
}

// RANKING. While a free-text word is typed the shown cards are REORDERED, best
// match first:
//
//	0  a title STARTS with the word                 ("gold" → "Gold:8")
//	1  a WORD in a title starts with it             ("sweep" → "docs-sweeper")
//	2  the word is anywhere else on the card        (the match that shows it)
//
// and cards of one rank keep the list's own order (pins first, then activity),
// which is what clearing the box restores. Tiers 0 and 1 are window.musterRank
// over the card's titles — the rule every search picker uses
// (internal/searchrank), held to its shared table.
//
// 🔴 THE CARDS THEMSELVES MOVE (agentsFilterScript's reorder), so Tab and a
// screen reader follow the ranking. An earlier version reordered with CSS
// `order` rules, which survive a refresh but move only the boxes: the ranking
// was visual only. The list's refresh morphs it back into the server's order;
// the script re-ranks on htmx:afterSwap, in the same task, so no frame shows
// the unranked order.
//
// rankAttrs is the attribute the script ranks by: every title, newline-led
// (a newline cannot be typed into the box).
func rankAttrs(titles []string) []g.Node {
	var heads []string
	seen := map[string]bool{}
	for _, t := range titles {
		v := strings.Join(strings.Fields(t), " ")
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		heads = append(heads, v)
	}
	if len(heads) == 0 {
		return nil
	}
	return []g.Node{g.Attr("data-rank-title", "\n"+strings.Join(heads, "\n"))}
}

// agentsFilterStorageKey is where the filter's text is kept for the browser
// session, so it is still there after a visit to another tab or a card.
const agentsFilterStorageKey = "muster.agents.filter"

// filterHaystack is a card's `data-filter`: every non-empty part, lower-cased,
// joined by spaces.
func filterHaystack(parts ...string) string {
	var b []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			b = append(b, strings.ToLower(p))
		}
	}
	return strings.Join(b, " ")
}

// labelledFields are the fields whose value, where lower-casing changed it,
// is written as it was spelled too (data-l-<field>): what a host TAB says
// ("Tower", not "tower"). Each must be a registered field
// (TestEveryLabelledFieldIsARegisteredField).
var labelledFields = []string{"host"}

// isLabelled reports whether key is one of labelledFields.
func isLabelled(key string) bool {
	for _, f := range labelledFields {
		if f == key {
			return true
		}
	}
	return false
}

// agentsHostTabsStorageKey is where the chosen host tab is kept (localStorage,
// under tabs.go's prefix).
const agentsHostTabsStorageKey = "muster:tabs:agents-host"

// agentsHostTabs is the host tab strip, drawn with its one fixed tab, "All".
// The script adds a tab per host by CLONING that one (so the markup and look
// live here once), and keeps the counts. It is one 44px row whatever it holds,
// so its tabs arriving shift nothing below it; at a phone's width it scrolls
// sideways inside itself, never the page.
func agentsHostTabs() g.Node {
	return Div(
		ID("agents-hosts"),
		g.Attr("role", "tablist"),
		g.Attr("aria-label", "Hosts"),
		Class("flex min-h-[44px] min-w-0 max-w-full flex-nowrap items-end gap-1 overflow-x-auto overflow-y-hidden overscroll-x-contain border-b border-line [scrollbar-width:none]"),
		Button(
			Type("button"),
			ID("agents-host-all"),
			g.Attr("role", "tab"),
			g.Attr("data-host", ""),
			g.Attr("aria-controls", "agents-list"),
			g.Attr("aria-selected", "true"),
			g.Attr("tabindex", "0"),
			Class(tabClass),
			Span(g.Attr("data-tab-label", ""), g.Text("All")),
			Span(g.Attr("data-tab-count", ""), Class("ml-1.5 min-w-[1.25rem] rounded-full bg-s2 px-1.5 text-center text-xs tabular-nums text-fg2")),
		),
	)
}

// agentsFilterBar is the filter: the host tabs, a plain search box (and its
// clear button), and the "N of M" count. It sits in the Agents panel ABOVE
// #agents-list, outside what a refresh re-renders.
func agentsFilterBar() g.Node {
	return g.Group{
		Div(
			ID("agents-filter"),
			Class("mb-3 flex min-w-0 flex-col gap-2"),
			agentsHostTabs(),
			Div(
				Class("relative min-w-0"),
				Input(
					ID("agents-search"),
					// text, not search: a search input draws the browser's own
					// clear control beside ours.
					Type("text"),
					g.Attr("role", "searchbox"),
					g.Attr("inputmode", "search"),
					g.Attr("enterkeyhint", "search"),
					g.Attr("autocomplete", "off"),
					g.Attr("autocapitalize", "off"),
					g.Attr("autocorrect", "off"),
					g.Attr("spellcheck", "false"),
					g.Attr("aria-label", "Search agents and sessions by name"),
					g.Attr("aria-controls", "agents-list"),
					Placeholder("Search by name"),
					// text-base: 16px. iOS zooms the page on focusing a smaller input.
					Class("min-h-[44px] w-full rounded-xl border-0 bg-s2 py-2.5 pl-3 pr-12 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
				// ⚠ No display utility on this button: it is shown and hidden with
				// the `hidden` ATTRIBUTE, which an author display class would beat.
				Button(
					ID("agents-search-clear"),
					Type("button"),
					g.Attr("hidden", ""),
					g.Attr("aria-label", "Clear the search"),
					Class("press absolute right-0 top-0 h-11 w-11 rounded-xl text-xl leading-none text-muted transition hover:text-fg"),
					g.Text("×"),
				),
			),
			// The preset chips (list_signals.go): waiting on an ask / a question,
			// idle past a threshold. Drawn here, with the box, so their row is
			// part of the first paint and never shifts the list.
			agentsFilterChips(),
			Div(
				// min-h-5: the row is one line of the count's text-sm tall BEFORE the
				// script writes the count. Empty, it was 0px, and the count's first
				// write pushed the list 20px down — a layout shift on every Agents
				// load (e2e/tests/cls.spec.ts).
				Class("flex min-h-5 flex-wrap items-center gap-2"),
				Span(ID("agents-count"), g.Attr("aria-live", "polite"), Class("ml-auto shrink-0 text-sm font-medium tabular-nums text-fg2")),
			),
		),
		P(ID("agents-nomatch"), g.Attr("hidden", ""), Class("mb-3 text-sm text-muted"), g.Text("Nothing matches that filter.")),
	}
}

// agentsFilterScript applies the filter. Bound once per document, like every
// other shell script (htmx re-executes in-body scripts on a boosted swap), and
// it looks its elements up at use time because a boosted swap replaces them.
func agentsFilterScript() g.Node {
	return Script(g.Raw(agentsRulesJS + `
(function () {
  if (window.__musterAgentsFilter) return;
  window.__musterAgentsFilter = true;
  var KEY = '` + agentsFilterStorageKey + `';
  // THE GRAMMAR, THE CHIPS' TABLE AND THE HIDE RULES ARE agentsRulesJS's
  // (agents_filter_head.go; emitted just above this IIFE and in the shell's
  // head, defined once per window): the parser (tokens, terms, sel), the
  // chips' selectors — computed in Go (chipSelectorsJSON), so this only looks
  // them up — and the rules. The head applies the stored filter before the
  // first paint with them; this script keeps it applied. One definition, so
  // the two cannot disagree.
  var R = window.__musterAgentsRules;
  function terms(q) { return R.terms(q); }
  function sel(t) { return R.sel(t); }
  // The chosen chips are remembered per device (localStorage), in try/catch:
  // storage can be unavailable.
  var CHIPS_KEY = '` + agentsChipsStorageKey + `';
  var chips = null;
  function chipsChosen() {
    if (chips === null) {
      chips = [];
      try { chips = R.chipsOf(localStorage.getItem(CHIPS_KEY)); } catch (e) { chips = []; }
    }
    return chips;
  }
  function chipSel() { return R.chipSel(chipsChosen()); }
  // toggleChip: a wait chip toggles; an idle chip toggles and clears the other
  // idle chips (they are nested thresholds: one at a time).
  function toggleChip(b) {
    var id = b.getAttribute('data-chip'), cs = chipsChosen().slice(), on = cs.indexOf(id) >= 0;
    if (b.getAttribute('data-chip-group') === 'idle') {
      cs = cs.filter(function (c) { return c.indexOf('idle-') !== 0; });
    } else {
      cs = cs.filter(function (c) { return c !== id; });
    }
    if (!on) cs.push(id);
    chips = cs;
    try { if (cs.length) localStorage.setItem(CHIPS_KEY, JSON.stringify(cs)); else localStorage.removeItem(CHIPS_KEY); } catch (e) {}
  }
  function syncChips() {
    var cs = chipsChosen();
    [].forEach.call(document.querySelectorAll('#agents-chips [data-chip]'), function (b) {
      var p = String(cs.indexOf(b.getAttribute('data-chip')) >= 0);
      if (b.getAttribute('aria-pressed') !== p) b.setAttribute('aria-pressed', p);
    });
    // Clear is offered only while a chip is on: a remembered chip cannot
    // hide cards without a visible way out.
    var clr = el('agents-chips-clear');
    if (clr && clr.hidden !== !cs.length) clr.hidden = !cs.length;
  }
  function clearChips() {
    chips = [];
    try { localStorage.removeItem(CHIPS_KEY); } catch (e) {}
  }
  // RECENCY AS TIME PASSES (list_signals.go): a card's data-idle and its
  // dot's tint are derived from its data-active-ts, and the server's copy is
  // only true when it rendered. restamp re-derives both against this clock,
  // on every apply and every 30s, with the same tables the server used.
  //
  // 🔴 TIME IS NOT INPUT. A card crossing an idle threshold while an idle chip
  // (or any filter) is on would appear or vanish by itself and move every card
  // below it. So unless the restamp is the reader's own (live: typing, a tap,
  // the pill), a card whose VISIBILITY its new data-idle would change keeps its
  // old one: it is put in pendingIdle, counted on the "N new ↑" pill
  // (recountPill), and applied on the tap (applyLive). A change that shows or
  // hides nothing is written at once.
  var SCALE = ` + recencyScaleJSON() + `;
  var IDLE = ` + idleThresholdsJSON() + `;
  var pendingIdle = {};
  function setIdle(a, v) { if (v) a.setAttribute('data-idle', v); else a.removeAttribute('data-idle'); }
  // idleWouldMove: whether giving card a the data-idle v would show or hide it
  // under the filter as last applied (curSel). It leaves a as it found it.
  function idleWouldMove(a, v) {
    if (!curSel) return false;
    var was = a.getAttribute('data-idle') || '';
    if (was === v) return false;
    var before = a.matches(curSel);
    setIdle(a, v);
    var after = a.matches(curSel);
    setIdle(a, was);
    return before !== after;
  }
  function idleOf(a, now) {
    var d = now - (+a.getAttribute('data-active-ts')), ts = [];
    IDLE.forEach(function (t) { if (d > t.ms) ts.push(t.t); });
    return ts.join(' ');
  }
  // pendingIds is every card whose idle tokens or Idle section wait for the
  // tap (pendingIdle, pendingSect), as a set; gone cards are forgotten.
  function pendingIds() {
    var out = {};
    [pendingIdle, pendingSect].forEach(function (m) {
      Object.keys(m).forEach(function (id) { if (document.getElementById(id)) out[id] = true; else delete m[id]; });
    });
    return out;
  }
  function restamp(list, live) {
    var now = Date.now();
    if (live) pendingIdle = {};
    [].forEach.call(list.querySelectorAll(':scope > article[data-active-ts]'), function (a) {
      var v = idleOf(a, now);
      if ((a.getAttribute('data-idle') || '') === v) { delete pendingIdle[a.id]; return; }
      if (!live && idleWouldMove(a, v)) { pendingIdle[a.id] = v; return; }
      delete pendingIdle[a.id];
      setIdle(a, v);
    });
    [].forEach.call(list.querySelectorAll('[data-recency-ts]'), function (dot) {
      var d = now - (+dot.getAttribute('data-recency-ts')), b = SCALE.length - 1;
      for (var i = 0; i < SCALE.length; i++) { if (!SCALE[i].u || d < SCALE[i].u) { b = i; break; } }
      if (dot.getAttribute('data-recency') !== String(b)) {
        dot.setAttribute('data-recency', String(b));
        dot.className = SCALE[b].c;
        dot.setAttribute('title', SCALE[b].n);
      }
    });
  }
  function el(id) { return document.getElementById(id); }
  // THE IDLE SECTION (idle_section.go). Which section a card is in is the
  // server's data-idle-section; where the section is drawn, and whether it is
  // collapsed, is the stylesheet's (web/css/input.css) — keyed on <html>'s
  // data-agents-idle-open (this viewer's choice, localStorage) and
  // data-agents-searching (a term in the box draws it open).
  var IDLE_OPEN_KEY = '` + agentsIdleOpenKey + `';
  // pendingSect: id -> the section the server says, for a card kept in its
  // drawn section because moving it would move what the reader sees.
  var pendingSect = {};
  function inSect(a) { return a.hasAttribute('data-idle-section'); }
  function sectOpen() {
    var r = document.documentElement;
    return r.hasAttribute('data-agents-idle-open') || r.hasAttribute('data-agents-searching');
  }
  // sectWouldMove: whether giving drawn card d the section of c (the refresh's
  // copy) would move anything in view — d has a box now (it would leave its
  // place), or c would get one (it would appear in its new place).
  function sectWouldMove(d, c) {
    if (d.getClientRects().length) return true;
    if (curSel && !c.matches(curSel)) return false;
    return !inSect(c) || sectOpen();
  }
  function toggleIdleOpen() {
    var r = document.documentElement, open = !r.hasAttribute('data-agents-idle-open');
    r.toggleAttribute('data-agents-idle-open', open);
    try { if (open) localStorage.setItem(IDLE_OPEN_KEY, '1'); else localStorage.removeItem(IDLE_OPEN_KEY); } catch (e) {}
  }
  // syncIdleHead keeps the header true: its count is the section's cards the
  // filter shows (drawn ones; a held arrival is not), it is not drawn while
  // that is none, aria-expanded is the state in force, and it sits right
  // before the section's first card in the DOM (a refresh or a rank can leave
  // it elsewhere; the stylesheet draws it in place regardless).
  function syncIdleHead(list) {
    var head = el('agents-idle-head');
    if (!head || head.parentNode !== list) return;
    var n = list.querySelectorAll(':scope > article[data-idle-section]:not([data-held])' + curSel).length;
    var c = head.querySelector('[data-idle-count]'), txt = '(' + n + ')';
    if (c && c.textContent !== txt) c.textContent = txt;
    var disp = n ? '' : 'none';
    if (head.style.display !== disp) head.style.display = disp;
    var b = el('agents-idle-toggle'), ex = String(sectOpen());
    if (b && b.getAttribute('aria-expanded') !== ex) b.setAttribute('aria-expanded', ex);
    var first = list.querySelector(':scope > article[data-idle-section]');
    if (first && head.nextElementSibling !== first) {
      var focused = document.activeElement;
      if (typeof list.moveBefore === 'function') list.moveBefore(head, first); else list.insertBefore(head, first);
      if (focused && focused !== document.activeElement && focused.isConnected) focused.focus({ preventScroll: true });
    }
  }
  // reorder MOVES the cards into rank order for the first free-text term — the
  // DOM itself, so Tab and a screen reader follow the ranking, not only the
  // eye. A card's rank is the best window.musterRank (the pickers' rule,
  // internal/searchrank) over its titles (data-rank-title): 0 a title starts
  // with the word, 1 a word in one does, else 2 — the card is shown, so it
  // matches the word somewhere. Equal ranks keep the list's own order
  // (data-list-i, taken from the server's order), and with no free-text term
  // that order comes back.
  //
  // 🔴 IT RUNS ON htmx:afterSwap TOO, BEFORE THE BROWSER PAINTS. A refresh
  // morphs the list back into the server's order (and drops data-list-i, which
  // the server does not send, so it is retaken from that fresh order); re-ranking
  // in the same task means no frame ever shows the unranked order. It moves
  // nothing outside #agents-list and calls no focus(), so typing in the box is
  // never interrupted.
  function rankWord(ts) {
    for (var i = 0; i < ts.length; i++) if (ts[i].attr === 'data-filter') return ts[i].value;
    return '';
  }
  function cardRank(card, w) {
    var best = 2;
    (card.getAttribute('data-rank-title') || '').split('\n').forEach(function (t) {
      if (!t || !window.musterRank) return;
      var r = window.musterRank(t, w);
      if (r === 0 || r === 1) best = Math.min(best, r);
    });
    return best;
  }
  function reorder(ts) {
    var list = el('agents-list');
    if (!list) return;
    var cards = [].slice.call(list.querySelectorAll(':scope > article'));
    if (cards.some(function (c) { return !c.hasAttribute('data-list-i'); })) {
      // Fresh from the server (first load or a morph): this IS its order.
      cards.forEach(function (c, i) { c.setAttribute('data-list-i', String(i)); });
    }
    var w = rankWord(ts);
    // A HELD arrival sorts LAST, whatever its rank: it is hidden, and ranking
    // it above a drawn card would move every drawn card behind it (with a
    // filter typed, a held arrival that matches it best did exactly that).
    var keyed = cards.map(function (c) {
      // s: the Idle section's cards after the rest, so the DOM (Tab, a screen
      // reader) follows the sections the stylesheet draws.
      return { c: c, h: c.hasAttribute('data-held') ? 1 : 0, s: inSect(c) ? 1 : 0, r: w ? cardRank(c, w) : 0, i: +c.getAttribute('data-list-i') };
    });
    keyed.sort(function (a, b) { return a.h - b.h || a.s - b.s || a.r - b.r || a.i - b.i; });
    arrange(list, cards, keyed.map(function (k) { return k.c; }));
  }
  // arrange puts cards (in DOM order now) into order (the same nodes), and
  // touches nothing when they already are. When anything is out of place it
  // re-appends them all, in order: by moveBefore where the browser has it
  // (focus and an open popover survive the move), else by appendChild — which
  // DETACHES each card, so a focused one blurs (it is handed focus back below)
  // and an open popover inside one closes. A held background refresh does not
  // come here: its markup is put in the drawn order before the morph
  // (keepDrawnOrder), and a held swap is not re-ranked (apply(held), so a
  // rename elsewhere that changes a drawn card's rank moves nothing until the
  // tap). When reorder does run — the reader typing, the tap — held arrivals
  // sort last, so they move nothing. ⚠ What a held refresh CAN still change in
  // view: a drawn card whose new text stops (or starts) matching the typed
  // filter is hidden (or shown) at once — content, not order, and still open.
  function arrange(list, cards, order) {
    if (!order.some(function (c, i) { return cards[i] !== c; })) return;
    var focused = document.activeElement;
    var move = typeof list.moveBefore === 'function'
      ? function (c) { list.moveBefore(c, null); }
      : function (c) { list.appendChild(c); };
    order.forEach(move);
    if (focused && focused !== document.activeElement && focused.isConnected && typeof focused.focus === 'function') {
      focused.focus({ preventScroll: true });
    }
  }
  function onAgents() { return location.pathname === '/agents'; }
  // syncURL writes the text into ?q= — replaceState, so typing is not history.
  function syncURL(v) {
    if (!onAgents()) return;
    var u = new URL(location.href);
    if (v.trim()) u.searchParams.set('q', v); else u.searchParams.delete('q');
    var next = u.pathname + u.search + u.hash;
    if (next !== location.pathname + location.search + location.hash) history.replaceState(history.state, '', next);
  }
  function urlQ() {
    if (!onAgents()) return null;
    try { return new URL(location.href).searchParams.get('q'); } catch (e) { return null; }
  }
  // THE HOST TABS (agentsHostTabs). chosenHost is the tab the reader chose:
  // read from localStorage once, written ONLY when the reader picks a tab, ''
  // for All. shownHost is the host the list is filtered to now. linkAll is set
  // for a page opened from a link carrying its own ?q= (boot): that load starts
  // on All, so the link shows what it was shared to show, and the stored
  // choice is left as it is.
  var HOST_KEY = '` + agentsHostTabsStorageKey + `';
  var chosenHost = null, shownHost = '', linkAll = false;
  function hostChosen() {
    if (chosenHost === null) { try { chosenHost = localStorage.getItem(HOST_KEY) || ''; } catch (e) { chosenHost = ''; } }
    return chosenHost;
  }
  function chooseHost(h) {
    chosenHost = h;
    linkAll = false;
    try { if (h) localStorage.setItem(HOST_KEY, h); else localStorage.removeItem(HOST_KEY); } catch (e) {}
  }
  function hostTabs(s) { return [].slice.call(s.querySelectorAll('[role="tab"]')); }
  // revealTab scrolls the strip — only the strip, never the page — so tab t
  // is inside it (a phone's strip scrolls sideways).
  function revealTab(s, t) {
    var sr = s.getBoundingClientRect(), tr = t.getBoundingClientRect();
    if (tr.left < sr.left) s.scrollLeft += tr.left - sr.left;
    else if (tr.right > sr.right) s.scrollLeft += tr.right - sr.right;
  }
  // syncHosts makes the strip say what the list DRAWS now — a tab per distinct
  // data-f-host of a drawn card (an arrival held behind the pill is not drawn,
  // so it offers no tab yet), labelled by its data-l-host, sorted by label —
  // and returns the host the list is filtered to ('' = All). Each tab's count
  // is how many drawn cards of that host the box's terms (ts) show; All's is
  // all of them.
  //
  // A tab that stays is the SAME node (reused by its data-host); when the
  // order changes they are put in order by arrange — moveBefore where the
  // browser has it, and a focused tab is handed its focus back. A new one is
  // a CLONE of the server's All tab: one markup, one look (tabClass).
  //
  // 🔴 THE CHOICE IS KEPT, THE VIEW FALLS BACK. A remembered host the list
  // does not draw now (a restart, a laptop asleep) is not drawn as an empty
  // tab: the list SHOWS All — and keeps the choice, so when that host is back
  // the list is filtered to it again. Only the reader's own pick changes it.
  //
  // 🔴 A HELD REFRESH NEVER SWITCHES THE VIEW, and needs no branch here to
  // keep it so: the hold counts only what the filter shows (decide), so a
  // refresh is held only when a SHOWN card that stays on the list would move —
  // a card of the shown host, still drawn, so the shown host stays seen. And a
  // remembered host coming back arrives as held cards, which are not drawn, so
  // it is restored only when the refresh is applied (the tap, or the next
  // applied one).
  function syncHosts(list, ts) {
    var s = el('agents-hosts');
    if (!s) return '';
    var all = s.querySelector('[data-host=""]');
    var drawn = list.querySelectorAll(':scope > article:not([data-held])');
    var seen = Object.create(null), hosts = [], icons = Object.create(null);
    [].forEach.call(drawn, function (a) {
      var v = a.getAttribute('data-f-host');
      if (!v || seen[v]) return;
      seen[v] = true;
      // The host's icon (host_icons.go), as its first drawn card draws it: the
      // tab leads with the same one.
      var ic = a.querySelector('[data-host-icon]');
      icons[v] = ic;
      hosts.push({ v: v, l: a.getAttribute('data-l-host') || v, i: ic ? ic.getAttribute('data-host-icon') : '' });
    });
    var h = linkAll ? '' : hostChosen();
    var on = h && seen[h] ? h : '';
    hosts.sort(function (x, y) {
      var a = x.l.toLowerCase(), b = y.l.toLowerCase();
      if (a !== b) return a < b ? -1 : 1;
      return x.v < y.v ? -1 : x.v > y.v ? 1 : 0;
    });
    var sig = JSON.stringify(hosts);
    if (all && s.getAttribute('data-hosts-of') !== sig) {
      var have = {};
      hostTabs(s).forEach(function (t) { if (t !== all) have[t.getAttribute('data-host')] = t; });
      var want = [all].concat(hosts.map(function (h) {
        var t = have[h.v];
        delete have[h.v];
        if (!t) {
          t = all.cloneNode(true);
          t.removeAttribute('id');
          t.setAttribute('data-host', h.v);
          s.appendChild(t);
        }
        var lab = t.querySelector('[data-tab-label]');
        if (lab && lab.textContent !== h.l) lab.textContent = h.l;
        var had = t.querySelector('[data-host-icon]');
        if ((had ? had.getAttribute('data-host-icon') : '') !== h.i) {
          if (had) had.remove();
          if (icons[h.v] && lab) {
            var ic = icons[h.v].cloneNode(true);
            // The count's spacing (0.375rem), as a style: no class
            // the stylesheet does not already carry.
            ic.style.marginRight = '0.375rem';
            // The tab already names the host in its label: the icon is
            // decoration there, not a second name read out.
            ic.removeAttribute('role');
            ic.removeAttribute('aria-label');
            ic.setAttribute('aria-hidden', 'true');
            t.insertBefore(ic, lab);
          }
        }
        return t;
      }));
      Object.keys(have).forEach(function (k) { have[k].remove(); });
      arrange(s, hostTabs(s), want);
      s.setAttribute('data-hosts-of', sig);
    }
    var n = Object.create(null), total = 0;
    [].forEach.call(list.querySelectorAll(':scope > article:not([data-held])' + ts.map(sel).join('') + chipSel()), function (a) {
      var v = a.getAttribute('data-f-host') || '';
      n[v] = (n[v] || 0) + 1;
      total++;
    });
    var picked = null;
    hostTabs(s).forEach(function (t) {
      var v = t.getAttribute('data-host') || '', sel_ = v === on;
      if (t.getAttribute('aria-selected') !== String(sel_)) t.setAttribute('aria-selected', String(sel_));
      t.setAttribute('tabindex', sel_ ? '0' : '-1');
      if (sel_) picked = t;
      var c = t.querySelector('[data-tab-count]'), k = String(v ? n[v] || 0 : total);
      if (c && c.textContent !== k) c.textContent = k;
    });
    if (picked && on !== shownHost) revealTab(s, picked);
    shownHost = on;
    return on;
  }
  // curSel is the selector of everything the filter asks (tab AND box), as of
  // the last apply: what the hold reads to tell a change the reader can see
  // from one they cannot (shows).
  var curSel = '';
  function shows(el_) { return !!el_ && (!curSel || el_.matches(curSel)); }
  // apply(held): held is true for a background refresh the hold kept back.
  // That refresh is NOT re-ranked: its markup came in the drawn order
  // (keepDrawnOrder), and re-ranking it would move a drawn card whose rank
  // the refresh changed (another client renamed it while a filter is typed).
  // The rank is applied with everything else on the tap (applyLive), and on
  // the reader's own typing.
  // apply(held): held is undefined for the READER's own change (typing, a
  // tap, a load) — which applies everything, held recency included — and a
  // boolean for a refresh or the clock (see restamp).
  function apply(held) {
    var input = el('agents-search'), list = el('agents-list');
    if (!input || !list) return;
    var q = input.value;
    var ts = terms(q);
    restamp(list, held === undefined);
    syncChips();
    // The chips are one more compound term, ANDed with the box's.
    var cs = chipSel();
    // The host tab is one more EXACT term, ANDed with the box's.
    var host = syncHosts(list, ts);
    var all_ = host ? ts.concat([{ attr: 'data-f-host', value: host, exact: true }]) : ts;
    curSel = all_.map(sel).join('') + cs;
    // A term in the box draws the Idle section open (a search finds its
    // entries); the reader's open/closed choice is left as it was.
    document.documentElement.toggleAttribute('data-agents-searching', ts.length > 0);
    syncIdleHead(list);
    var style = el('agents-filter-style');
    if (!style) {
      style = document.createElement('style');
      style.id = 'agents-filter-style';
      document.head.appendChild(style);
    }
    // One rule per term: a card lacking ANY of them is hidden, so the terms AND
    // (R.rules — the head script's, which wrote this element's first copy).
    // Then the rank (reorder): by the FIRST free-text word, best match first.
    var text = R.rules(ts, host, cs, false);
    if (style.textContent !== text) style.textContent = text;
    if (!held) reorder(ts);
    var all = list.querySelectorAll(':scope > article:not([data-held])');
    var shown = curSel ? list.querySelectorAll(':scope > article:not([data-held])' + curSel) : all;
    var count = el('agents-count'), clear = el('agents-search-clear'), none = el('agents-nomatch');
    if (count) count.textContent = all.length ? shown.length + ' of ' + all.length : '';
    if (clear) clear.hidden = !input.value;
    if (none) none.hidden = !(curSel && all.length && !shown.length);
    try { sessionStorage.setItem(KEY, q); } catch (e) {}
    syncURL(q);
    recountPill();
  }
  // place puts a filter in the box: a link's ?q=, the session's copy, or a
  // history entry's. A list refresh never calls it — ?q= follows every
  // keystroke, so re-reading it on a refresh would rewrite what is being typed.
  function place(q) {
    var input = el('agents-search');
    if (input && input.value !== q) input.value = q;
  }
  // boot: a bar not seen before (or one a swap emptied while a filter is
  // remembered) is given its filter — a ?q= wins (a shared link), else the
  // session's copy. Any other call is a no-op.
  function boot() {
    var input = el('agents-search');
    if (!input) return;
    adopt();
    var q = urlQ(), mine = null;
    try { mine = sessionStorage.getItem(KEY); } catch (e) {}
    if (q === null) q = mine || '';
    if (input.__musterFilterBar && (input.value || !q)) return;
    input.__musterFilterBar = true;
    // A ?q= this tab did not write (a shared link: this session's copy says
    // otherwise) starts on All. A reload of this tab's own filter is not a link.
    linkAll = !!urlQ() && urlQ() !== mine;
    place(q);
    apply();
  }
  // settled: the list rendered. The tabs and the counts follow it; nothing in
  // the box is touched.
  function settled() {
    boot();
    apply(swapHeld);
  }
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'agents-search') apply();
  });
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target : null;
    if (!t) return;
    var input = el('agents-search');
    if (t.closest('#agents-search-clear')) {
      if (!input) return;
      input.value = '';
      apply();
      input.focus();
      return;
    }
    var tab = t.closest('#agents-hosts [role="tab"]');
    if (tab) { chooseHost(tab.getAttribute('data-host') || ''); apply(); return; }
    var chip = t.closest('#agents-chips [data-chip]');
    if (chip) { toggleChip(chip); apply(); return; }
    if (t.closest('#agents-chips-clear')) { clearChips(); apply(); return; }
    if (t.closest('#agents-idle-toggle')) { toggleIdleOpen(); apply(); }
  });
  // The host tabs' keys, as tabs.go's: Left/Right (wrapping), Home and End
  // move to a tab AND choose it.
  document.addEventListener('keydown', function (e) {
    var tab = e.target && e.target.closest && e.target.closest('#agents-hosts [role="tab"]');
    if (!tab) return;
    var ts = hostTabs(tab.parentNode), i = ts.indexOf(tab), next = -1;
    switch (e.key) {
      case 'ArrowRight': next = (i + 1) % ts.length; break;
      case 'ArrowLeft': next = (i - 1 + ts.length) % ts.length; break;
      case 'Home': next = 0; break;
      case 'End': next = ts.length - 1; break;
      default: return;
    }
    e.preventDefault();
    chooseHost(ts[next].getAttribute('data-host') || '');
    apply();
    ts[next].focus();
  });
  // The list re-renders itself; the rule above already hides what should be
  // hidden, so all that is left is the count and the host tabs.
  // A refresh morphed the list into the server's order: rank it again before
  // anything is painted (see reorder).
  // HOLD, AND SHOW "N new ↑" (Zach's call; applying ONLY on a tap, even at the
  // top, was this change's own resolution, since confirmed by him). A list that
  // re-renders itself in the background — an SSE nudge (sse:agent.changed / sse:chat.reply) or a
  // resync — must not move a card the reader can see: the list is ordered by
  // activity, so an arriving agent is drawn on top and pushes every card down
  // (with no input to excuse it — e2e/tests/cls.spec.ts measures that shift on
  // every run, as the positive control of its arrival case).
  // So a background refresh is HELD: the cards keep the order they had, a card
  // that arrived is kept out of view (data-held), and the pill counts what is
  // waiting — arrivals plus cards the live order puts higher. Tapping it applies the
  // live order (a tap is input, so what it moves is the page answering it).
  // Status dots and labels are NOT held: the morph has already updated them in
  // place; only order and insertion wait.
  //
  // 🔴 HELD EVEN AT THE TOP: applying without a tap moves the cards in view
  // whatever the scroll position — scrolling back up is not input either — so
  // the only shift-free apply of a change in view is the reader's own tap. A
  // change that moves nothing in view (an arrival at the end, a reorder wholly
  // below the screen, a list on a hidden tab) is applied at once.
  //
  // A refresh the reader caused (agents:changed, after their own rename or pin)
  // and the first load apply at once. A filter's rank still orders the cards;
  // ties fall back to the HELD order (data-list-i), so the two never fight.
  var heldOrder = null, liveOrder = null;
  function cardsOf(list) { return [].slice.call(list.querySelectorAll(':scope > article')); }
  function background(e) {
    var cfg = e.detail && e.detail.requestConfig, t = cfg && cfg.triggeringEvent;
    var type = t && t.type || '';
    return type.indexOf('sse:') === 0 || type === 'muster:resync' || type === '` + AgentsAwaitingEvent + `';
  }
  // 🔴 THE PILL IS LOOKED UP, NEVER CACHED. A boosted navigation swaps the
  // body and an htmx history restore puts back a SNAPSHOT of it, so a node
  // kept from an earlier visit is detached (it was updated, unseen, while the
  // arrival sat hidden with nothing to tap) — and a restored copy is a clone
  // that carries no listeners. So the live one is found by id each time, made
  // if missing, and its tap is handled by delegation on document.
  function pillEl() {
    var p = el('agents-new-pill');
    if (!p) {
      p = document.createElement('button');
      p.type = 'button';
      p.id = 'agents-new-pill';
      p.setAttribute('aria-live', 'polite');
      p.className = 'press fixed left-1/2 top-20 z-30 inline-flex min-h-[44px] -translate-x-1/2 items-center gap-1 rounded-full bg-accent px-4 text-sm font-semibold text-on-accent shadow-lg shadow-black/30';
      document.body.appendChild(p);
    }
    return p;
  }
  function onPill(e) { return e.target && e.target.closest && e.target.closest('#agents-new-pill'); }
  // A tap must not take focus: the box keeps its caret (and the phone its
  // keyboard), a focused card stays focused.
  document.addEventListener('mousedown', function (e) { if (onPill(e)) e.preventDefault(); });
  document.addEventListener('click', function (e) { if (onPill(e)) applyLive(); });
  // The pill is fixed on <body>, so it would float over whatever tab is shown:
  // it is drawn only while the Agents panel is, and comes back with its count
  // when the tab does (muster:tabshown, popstate).
  var pillCount = 0;
  function agentsShown() {
    var panel = el('panel-agents');
    return !!panel && !panel.classList.contains('hidden');
  }
  function syncPill() {
    if (!pillCount && !el('agents-new-pill')) return;
    var pill = pillEl();
    pill.textContent = pillCount + ' new ↑';
    pill.setAttribute('data-count', String(pillCount));
    pill.hidden = pillCount === 0 || !agentsShown();
  }
  function showPill(n) {
    pillCount = n;
    syncPill();
  }
  // A new body (a boosted navigation, or Back restoring one) holds a list this
  // state was not computed for: start over — its first load applies at once.
  function forget() { heldOrder = null; liveOrder = null; pillCount = 0; pendingSect = {}; }
  // adopt: a list the DOCUMENT drew (server-rendered, agents_list_ssr.go — or a
  // history restore's snapshot) is the drawn order, exactly as a first load's
  // swap would have made it (hold). Without it the first background refresh
  // found no held order and was applied at once, moving whatever it moved.
  function adopt() {
    if (heldOrder) return;
    var list = el('agents-list');
    if (!list) return;
    var ids = cardsOf(list).map(function (c) { return c.id; });
    if (!ids.length) return;
    heldOrder = ids.slice();
    liveOrder = ids.slice();
    stamp(list, heldOrder);
  }
  document.addEventListener('htmx:historyRestore', function () { forget(); adopt(); });
  document.addEventListener('htmx:beforeSwap', function (e) { if (e.target === document.body) forget(); });
  function stamp(list, order) {
    var at = {};
    order.forEach(function (id, i) { at[id] = i; });
    cardsOf(list).forEach(function (c) {
      if (c.id in at) { c.setAttribute('data-list-i', String(at[c.id])); c.removeAttribute('data-held'); }
    });
  }
  function applyLive() {
    pendingIdle = {};
    Object.keys(pendingSect).forEach(function (id) {
      var c = document.getElementById(id);
      if (c) c.toggleAttribute('data-idle-section', pendingSect[id]);
    });
    pendingSect = {};
    var list = el('agents-list');
    if (list && liveOrder) {
      heldOrder = liveOrder.slice();
      stamp(list, heldOrder);
    }
    showPill(0);
    apply();
    if (list && list.getBoundingClientRect().top < 0) list.scrollIntoView({ block: 'start' });
  }
  // seen is where each card was ON SCREEN just before a swap (beforeSwap), so
  // hold can tell whether applying the new order would move one in view.
  var seen = {};
  function inView(id) {
    var r = seen[id];
    return !!r && r.h > 0 && r.bottom > 0 && r.top < window.innerHeight;
  }
  // decide answers, for a swap of the list whose server order is live, whether
  // it is applied at once (true) or held.
  //
  // 🔴 ONLY WHAT THE FILTER SHOWS COUNTS. byId maps a live card's id to its
  // element in the refresh (shows reads the filter — tab AND box — off it). A
  // card the filter hides is neither in view nor about to be: an arrival on
  // another host's tab, or a re-order among hidden cards, moves nothing the
  // reader can see, so a refresh that changes only those is applied at once —
  // and the hidden hosts' tabs and counts follow it live. A card that is GONE
  // is not a reason to hold either: holding cannot keep it, it leaves the
  // screen whichever way the refresh goes.
  //
  // 🔴 A REFRESH THAT WOULD SWITCH THE SHOWN TAB IS HELD, wherever its cards
  // land. The remembered host coming back (wouldShow) turns All into that
  // host's tab: every card the reader was looking at but that host's would go,
  // with no input — even when its own cards land off screen or under the
  // search, which the order check below cannot see. Held, its cards are
  // arrivals (not drawn, so the tab stays All), the pill offers it
  // (recountPill), and the tap applies it.
  function decide(e, live, byId) {
    if (!heldOrder || !background(e) || !heldOrder.length) return true;
    var w = wouldShow(live, byId);
    if (w && w !== shownHost) return false;
    var inLive = {};
    live.forEach(function (id) { inLive[id] = true; });
    var h = heldOrder.filter(function (id) { return inLive[id] && shows(byId(id)); });
    var l = live.filter(function (id) { return shows(byId(id)); });
    // What applying would move: every SHOWN card from the first place the
    // held and the live order differ. An arrival at the END moves nothing; a
    // change wholly below the screen (or on a hidden tab) moves nothing in
    // view. Either is applied at once; anything else is held.
    var p = 0;
    while (p < h.length && h[p] === l[p]) p++;
    return !h.slice(p).some(inView);
  }
  function domById(id) { return document.getElementById(id); }
  // wouldShow is the host the list would be filtered to if the refresh with
  // these cards were drawn whole — syncHosts's rule — so decide can hold one
  // that would switch it. A shown host whose cards are all GONE is not held:
  // they leave the screen whichever way the refresh goes (see decide), and the
  // list shows All.
  function wouldShow(live, byId) {
    var h = linkAll ? '' : hostChosen();
    if (!h) return '';
    return live.some(function (id) { var c = byId(id); return !!c && c.getAttribute('data-f-host') === h; }) ? h : '';
  }
  // swapPlan is beforeSwap's reading of the swap afterSwap finishes: the server's
  // (live) order and the decision.
  var swapPlan = null;
  // swapHeld: whether the list's last swap was held — settled() re-applies the
  // filter after it and must not re-rank it either (see apply).
  var swapHeld = false;
  function hold(e) {
    var list = e.target;
    var p = swapPlan;
    swapPlan = null;
    liveOrder = p ? p.live : cardsOf(list).map(function (c) { return c.id; });
    var apply_ = p ? p.apply : decide(e, liveOrder, domById);
    if (apply_) {
      heldOrder = liveOrder.slice();
      stamp(list, heldOrder);
      showPill(0);
      return false;
    }
    var known = {};
    heldOrder.forEach(function (id) { known[id] = true; });
    // The held order, without cards that are gone; arrivals after it, held.
    heldOrder = heldOrder.filter(function (id) { return liveOrder.indexOf(id) >= 0; });
    var arrived = liveOrder.filter(function (id) { return !known[id]; });
    stamp(list, heldOrder);
    arrived.forEach(function (id, i) {
      var c = document.getElementById(id);
      if (c) { c.setAttribute('data-held', ''); c.setAttribute('data-list-i', String(heldOrder.length + i)); }
    });
    // The pill's count is apply's (recountPill), which follows right after.
    return true;
  }
  // recountPill is what WAITS for the reader, against the filter as it is
  // NOW — so changing the tab or the search during a hold recounts it: each
  // held arrival, and each card the live order puts HIGHER than it is drawn
  // (one card overtaking three is one change, not four), of the cards the
  // filter SHOWS only (decide); what it hides is not waiting. A held switch of
  // the shown tab (wouldShow) waits too: at least one.
  function recountPill() {
    var list = el('agents-list');
    if (!list) return;
    // Cards whose crossing an idle threshold is held (restamp), or whose move
    // in or out of the Idle section is (pendingSect), wait too — each once,
    // however many of those it is waiting on.
    var pend = pendingIds();
    if (!heldOrder || !liveOrder) { showPill(Object.keys(pend).length); return; }
    var counted = {};
    var shown = function (id) { return shows(domById(id)); };
    var arrived = cardsOf(list).filter(function (c) { return c.hasAttribute('data-held') && shows(c); });
    arrived.forEach(function (c) { counted[c.id] = true; });
    arrived = arrived.length;
    var inHeld = {};
    heldOrder.forEach(function (id) { inHeld[id] = true; });
    var heldShown = heldOrder.filter(shown);
    var liveKnown = liveOrder.filter(function (id) { return inHeld[id] && shown(id); });
    var risen = 0;
    liveKnown.forEach(function (id, i) { if (i < heldShown.indexOf(id)) { risen++; counted[id] = true; } });
    var n = arrived + risen, w = wouldShow(liveOrder, domById);
    var extra = Object.keys(pend).filter(function (id) { return !counted[id]; }).length;
    showPill((w && w !== shownHost && !n ? 1 : n) + extra);
  }
  // keepDrawnOrder rewrites a held refresh's markup so its cards come in the
  // order they are DRAWN (cards it no longer has dropped, arrivals after), and
  // idiomorph then morphs every card in place: nothing is moved, so nothing is
  // detached — an open popover stays open, a focused element is not blurred,
  // in every browser. (Morphed into the live order and put back, every card
  // moved on every refresh while the pill was up.)
  function keepDrawnOrder(e, drawn) {
    var tpl = document.createElement('template');
    tpl.innerHTML = e.detail.serverResponse;
    var arts = [].filter.call(tpl.content.children, function (c) { return c.tagName === 'ARTICLE' && c.id; });
    if (!arts.length) return;
    var byId = {};
    arts.forEach(function (a) { byId[a.id] = a; });
    var order = drawn.filter(function (id) { return byId[id]; });
    var kept = {};
    order.forEach(function (id) { kept[id] = true; });
    arts.forEach(function (a) { if (!kept[a.id]) order.push(a.id); });
    if (!order.some(function (id, i) { return arts[i].id !== id; })) return;
    var mark = document.createComment('');
    arts[0].parentNode.insertBefore(mark, arts[0]);
    arts.forEach(function (a) { a.remove(); });
    order.forEach(function (id) { mark.parentNode.insertBefore(byId[id], mark); });
    mark.remove();
    e.detail.serverResponse = tpl.innerHTML;
  }
  document.addEventListener('htmx:beforeSwap', function (e) {
    if (!e.target || e.target.id !== 'agents-list') return;
    seen = {};
    var drawn = cardsOf(e.target);
    drawn.forEach(function (c) {
      var r = c.getBoundingClientRect();
      seen[c.id] = { top: r.top, bottom: r.bottom, h: r.height };
    });
    swapPlan = null;
    if (typeof e.detail.serverResponse !== 'string') return;
    var tpl = document.createElement('template');
    tpl.innerHTML = e.detail.serverResponse;
    var arts = [].filter.call(tpl.content.children, function (c) { return c.tagName === 'ARTICLE' && c.id; });
    var byId = {};
    arts.forEach(function (c) { byId[c.id] = c; });
    var live = arts.map(function (c) { return c.id; });
    // A background refresh carries the server's data-idle: where that would
    // show or hide a drawn card, the drawn value is kept and the change held,
    // exactly as the clock's is (restamp).
    //
    // So does its data-idle-section (the Idle section, idle_section.go): a
    // card that would ENTER or LEAVE the section while it is drawn — or would
    // become drawn — keeps the drawn section and the move waits for the tap
    // (pendingSect): moving it would move every card between its two places.
    if (background(e)) {
      var changed = false;
      arts.forEach(function (c) {
        var d = document.getElementById(c.id);
        if (!d || d.parentNode !== e.target) { delete pendingSect[c.id]; return; }
        var drawnIdle = d.getAttribute('data-idle') || '', srv = c.getAttribute('data-idle') || '';
        if (curSel && drawnIdle !== srv) {
          setIdle(c, drawnIdle);
          if (idleWouldMove(c, srv)) { pendingIdle[c.id] = srv; changed = true; } else setIdle(c, srv);
        }
        var ds = inSect(d), ss = inSect(c);
        if (ds === ss) { delete pendingSect[c.id]; return; }
        if (sectWouldMove(d, c)) { c.toggleAttribute('data-idle-section', ds); pendingSect[c.id] = ss; changed = true; } else delete pendingSect[c.id];
      });
      if (changed) e.detail.serverResponse = tpl.innerHTML;
    } else {
      // The reader's own refresh applies everything, the section included.
      pendingSect = {};
    }
    swapPlan = { live: live, apply: decide(e, live, function (id) { return byId[id]; }) };
    if (!swapPlan.apply) keepDrawnOrder(e, drawn.map(function (c) { return c.id; }));
  });
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'agents-list') { swapHeld = hold(e); apply(swapHeld); }
  });
  document.addEventListener('htmx:afterSettle', function (e) {
    if (e.target && e.target.id === 'agents-list') settled();
  });
  document.addEventListener('htmx:load', boot);
  // Back on the Agents tab (switchTab pushed a bare /agents): put the filter's
  // ?q= back on it at once — a replace, so the switch is still one entry.
  document.addEventListener('muster:tabshown', function (e) {
    syncPill();
    var input = el('agents-search');
    if (input && e.detail && e.detail.tab === 'agents') syncURL(input.value);
  });
  // Back/forward onto an /agents entry: its ?q= is the filter it had.
  // (An entry with no ?q= leaves the bar as it is, and apply writes it back.)
  window.addEventListener('popstate', function () {
    // The shell's own popstate handler shows the tab; after it, the pill follows.
    setTimeout(syncPill, 0);
    var q = urlQ();
    if (q !== null) place(q);
    apply();
  });
  // Time passes with nothing re-rendered: re-derive the idle tokens and the
  // dots, and the filter with them (a card can cross "idle >15m" while the
  // list sits open). A held list is not re-ranked (apply(swapHeld)).
  setInterval(function () { if (el('agents-list') && el('agents-search')) apply(swapHeld); }, 30000);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
})();
`))
}
