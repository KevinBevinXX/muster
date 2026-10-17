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
// the whole field: it is what a select writes (below), so choosing host "mac"
// does not also show host "macbook". A token whose prefix is
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
// CONTROLS. Nobody has to type `host=tower`: the bar has one <select> per
// facet field (facetFields: host, type, status, kind) and the box is plain
// text. The script fills each select's options from the cards the list holds
// now — the distinct `data-f-<field>` values, labelled by `data-l-<field>`
// where the value had to be lower-cased — and refills them whenever the list
// re-renders.
//
// 🔴 THE CONTROLS ARE A VIEW OF THE SAME TOKEN STRING, NOT A SECOND FILTER. The
// script's ONE string (composeQ) is the selects' `field=value` tokens followed
// by the box's text; that string is what the hide rules, the count, `?q=` and
// sessionStorage all read and write. Going the other way — for a link, the
// session's copy or a history entry ONLY, never for a list refresh and never
// while the box has focus (place / settled in the script) — split() hands a
// `field=value` token to its select when that select is still empty and has an
// option of exactly that value; everything else (free words, a typed
// `host:tow`, `session:`, a value no card has) stays in the box, where it
// filters just the same. So an old link and a typed token keep working, and a
// `host=x` typed by hand stays where it was typed.
//
// A select's value is spelled `field=value` when it is bare-safe, else
// `field="value"` with `\"` and `\\` escaped; inside quotes the grammar reads
// those two escapes back, so any value a card carries round-trips through `?q=`.

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
// one `data-f-<field>` per non-empty field and, for a facet field whose value
// lower-casing changed, `data-l-<field>`: the value as the card spells it,
// which is what the field's select option says ("Claude Code", not "claude
// code").
func cardFilterAttrs(f cardFields, haystack ...string) g.Node {
	attrs := g.Group{g.Attr("data-filter", filterHaystack(haystack...))}
	for _, fd := range cardFieldRegistry {
		raw := strings.Join(strings.Fields(fd.Of(f)), " ")
		v := fieldValue(raw)
		if v == "" {
			continue
		}
		attrs = append(attrs, g.Attr("data-f-"+fd.Key, v))
		if raw != v && isFacet(fd.Key) {
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

// facetFields are the fields the bar offers as controls, in the order it
// draws them. Each must be a registered field (TestEveryFacetIsARegisteredField).
var facetFields = []string{"host", "type", "status", "kind"}

// isFacet reports whether key is one of facetFields.
func isFacet(key string) bool {
	for _, f := range facetFields {
		if f == key {
			return true
		}
	}
	return false
}

// facetAny is a facet select's first option, which selects nothing.
var facetAny = map[string]string{"host": "Any host", "type": "Any type", "status": "Any status", "kind": "Any kind"}

// facetSelect is one facet's control. Its options beyond "Any …" are filled by
// the script from the cards (syncOptions). text-base: 16px, so iOS does not zoom; a
// 44px target. ⚠ data-[active] (set by the script while a value is chosen) is
// the selected look; no display utility fights anything here.
func facetSelect(key string) g.Node {
	return Select(
		ID("agents-f-"+key),
		g.Attr("data-filter-field", key),
		g.Attr("aria-label", "Filter by "+key),
		g.Attr("aria-controls", "agents-list"),
		Class("min-h-[44px] w-full min-w-0 rounded-xl border-0 bg-s2 px-3 py-2 text-base text-fg ring-1 ring-inset ring-line focus:outline-none focus:ring-2 focus:ring-focus data-[active]:font-semibold data-[active]:ring-2 data-[active]:ring-accent"),
		Option(Value(""), g.Text(facetAny[key])),
	)
}

// agentsFilterBar is the filter: a plain search box (and its clear button),
// one select per facet field, a "Clear all", and the "N of M" count. It sits in the Agents panel ABOVE
// #agents-list, outside what a refresh re-renders.
func agentsFilterBar() g.Node {
	return g.Group{
		Div(
			ID("agents-filter"),
			Class("mb-3 flex flex-col gap-2"),
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
			// Two columns at a phone's width, four from sm up: each a full-width
			// 44px target.
			Div(
				ID("agents-filter-controls"),
				Class("grid grid-cols-2 gap-2 sm:grid-cols-4"),
				g.Group(g.Map(facetFields, facetSelect)),
			),
			Div(
				// min-h-5: the row is one line of the count's text-sm tall BEFORE the
				// script writes the count. Empty, it was 0px, and the count's first
				// write pushed the list 20px down — a layout shift on every Agents
				// load (e2e/tests/cls.spec.ts).
				Class("flex min-h-5 flex-wrap items-center gap-2"),
				// ⚠ No display utility, for the same reason as the box's ×.
				Button(
					ID("agents-filter-clear"),
					Type("button"),
					g.Attr("hidden", ""),
					Class("press min-h-[44px] rounded-xl px-3 text-sm font-medium text-accent transition hover:bg-s2"),
					g.Text("Clear all"),
				),
				Span(ID("agents-count"), g.Attr("aria-live", "polite"), Class("ml-auto shrink-0 text-sm font-medium tabular-nums text-fg2")),
			),
		),
		P(ID("agents-nomatch"), g.Attr("hidden", ""), Class("mb-3 text-sm text-muted"), g.Text("Nothing matches that filter.")),
	}
}

// facetFieldsJSON is facetFields as a JS array literal.
func facetFieldsJSON() string {
	b, _ := json.Marshal(facetFields)
	return string(b)
}

// agentsFilterScript applies the filter. Bound once per document, like every
// other shell script (htmx re-executes in-body scripts on a boosted swap), and
// it looks its elements up at use time because a boosted swap replaces them.
func agentsFilterScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterAgentsFilter) return;
  window.__musterAgentsFilter = true;
  var KEY = '` + agentsFilterStorageKey + `';
  // The registered fields (cardFieldRegistry). A token naming anything else is free text.
  var FIELDS = ` + cardFieldKeysJSON() + `;
  function el(id) { return document.getElementById(id); }
  function norm(v) { return String(v || '').replace(/\s+/g, ' ').trim(); }
  // tokens is a string in the grammar, term by term and in order: {raw, f, exact,
  // v} each — f a registered field (lower-cased) or null for free text, exact
  // true for field=value, v the value as written. Inside double quotes a
  // backslash escapes a double quote or a backslash (\" and \\, what tok
  // writes); any other backslash is itself. Outside quotes nothing is escaped.
  function unq(v) { return v.replace(/\\(["\\])/g, '$1'); }
  function tokens(s) {
    var out = [], m, re = /([^\s:="]+)([:=])"((?:[^"\\]|\\.)*)"?|"((?:[^"\\]|\\.)*)"?|\S+/g;
    s = String(s || '');
    while ((m = re.exec(s))) {
      var f = null, sep = '', v = m[0];
      if (m[1] !== undefined) { f = m[1]; sep = m[2]; v = unq(m[3]); }
      else if (m[4] !== undefined) { v = unq(m[4]); }
      else {
        var i = m[0].search(/[:=]/);
        if (i > 0) { f = m[0].slice(0, i); sep = m[0][i]; v = m[0].slice(i + 1); }
      }
      if (f !== null && FIELDS.indexOf(f.toLowerCase()) < 0) { v = m[1] !== undefined ? f + sep + v : m[0]; f = null; }
      out.push({ raw: m[0], f: f === null ? null : f.toLowerCase(), exact: f !== null && sep === '=', v: v });
    }
    return out;
  }
  // terms parses the filter into what must ALL match: {attr, value, exact}
  // each, attr being data-filter (free text) or data-f-<registered field>. A
  // field with nothing after its separator (host: mid-typing) is no term yet.
  function terms(q) {
    var out = [];
    tokens(String(q || '').toLowerCase()).forEach(function (t) {
      var v = norm(t.v);
      if (v) out.push(t.f ? { attr: 'data-f-' + t.f, value: v, exact: t.exact } : { attr: 'data-filter', value: v });
    });
    return out;
  }
  // sel is the attribute selector for ONE term, its value a CSS double-quoted
  // string: [a="v"] for an exact term, [a*="v"] for every other. The rule that
  // hides and the query that counts are both built from it.
  function sel(t) { return '[' + t.attr + (t.exact ? '=' : '*=') + '"' + t.value.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"]'; }
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
      return { c: c, h: c.hasAttribute('data-held') ? 1 : 0, r: w ? cardRank(c, w) : 0, i: +c.getAttribute('data-list-i') };
    });
    keyed.sort(function (a, b) { return a.h - b.h || a.r - b.r || a.i - b.i; });
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
  // The facet fields the bar has a <select> for (facetFields), id agents-f-<field>.
  var CONTROLS = ` + facetFieldsJSON() + `;
  function control(f) { return el('agents-f-' + f); }
  function hasOption(c, v) { return !!v && [].some.call(c.options, function (o) { return o.value === v; }); }
  // tok spells one select's value as an EXACT token that tokens() reads back
  // as the same value: bare when it holds no space, double quote or backslash,
  // else quoted with \" and \\ escaped (so a kind such as say"hi survives ?q=).
  function tok(f, v) {
    v = norm(v);
    return /[\s"\\]/.test(v) ? f + '="' + v.replace(/["\\]/g, '\\$&') + '"' : f + '=' + v;
  }
  // composeQ is the filter's ONE token string: the selects' tokens, then the
  // box's text as typed. Everything below reads and writes this.
  function composeQ() {
    var parts = [];
    CONTROLS.forEach(function (f) { var c = control(f); if (c && c.value) parts.push(tok(f, c.value)); });
    var input = el('agents-search'), box = input ? input.value : '';
    if (!parts.length) return box;
    if (box.trim()) parts.push(box);
    return parts.join(' ');
  }
  // split is a token string as the bar holds it. A facet field's field=value
  // token goes to that field's select when it is the field's ONLY exact token
  // and the select has an option of exactly that value; every other token — a
  // free word, a typed host:tow, session:, a value no card has — stays in the
  // box, in order, and filters from there just the same. With nothing taken
  // the box gets the string untouched.
  function split(q) {
    var s = String(q || ''), ts = tokens(s), n = {}, fields = {}, text = [], any = false;
    ts.forEach(function (t) { if (t.exact) n[t.f] = (n[t.f] || 0) + 1; });
    ts.forEach(function (t) {
      var v = norm(t.v).toLowerCase(), c = t.exact && n[t.f] === 1 && CONTROLS.indexOf(t.f) >= 0 ? control(t.f) : null;
      if (c && hasOption(c, v)) { fields[t.f] = v; any = true; return; }
      text.push(t.raw);
    });
    return { fields: fields, text: any ? text.join(' ') : s };
  }
  function distribute(q) {
    var p = split(q);
    CONTROLS.forEach(function (f) { var c = control(f); if (c) c.value = p.fields[f] || ''; });
    var input = el('agents-search');
    if (input && input.value !== p.text) input.value = p.text;
  }
  // syncOptions fills each select from the cards the list holds now: every
  // distinct data-f-<field> once, saying its data-l-<field> when it has one,
  // sorted by what it says. A select remembers what it was built from, so an
  // unchanged list rebuilds nothing. A chosen value no card carries any more is
  // NOT kept as an option of its own: its token moves to the box, so the filter
  // that was asked for is still applied, and visibly.
  function syncOptions() {
    var list = el('agents-list');
    if (!list) return;
    var moved = [];
    CONTROLS.forEach(function (f) {
      var c = control(f);
      if (!c) return;
      var seen = Object.create(null), opts = [];
      // Only DRAWN cards offer a value: an arrival held behind the pill is not
      // on screen, so choosing a kind only it has would show nothing at all.
      // (It is offered once the tap draws it — applyLive re-runs apply.)
      [].forEach.call(list.querySelectorAll(':scope > article[data-f-' + f + ']:not([data-held])'), function (a) {
        var v = a.getAttribute('data-f-' + f);
        if (seen[v]) return;
        seen[v] = true;
        opts.push({ v: v, l: a.getAttribute('data-l-' + f) || v });
      });
      opts.sort(function (x, y) {
        var a = x.l.toLowerCase(), b = y.l.toLowerCase();
        if (a !== b) return a < b ? -1 : 1;
        return x.v < y.v ? -1 : x.v > y.v ? 1 : 0;
      });
      var sig = JSON.stringify(opts);
      if (c.getAttribute('data-options-of') === sig) return;
      var cur = c.value;
      while (c.options.length > 1) c.remove(1);
      opts.forEach(function (o) {
        var op = document.createElement('option');
        op.value = o.v;
        op.textContent = o.l;
        c.appendChild(op);
      });
      c.setAttribute('data-options-of', sig);
      if (cur && !seen[cur]) moved.push(tok(f, cur));
      c.value = cur && seen[cur] ? cur : '';
    });
    var input = el('agents-search');
    if (!moved.length || !input) return;
    // The moved tokens go in FRONT (where composeQ had them), so a caret in
    // the box keeps its place in what was typed.
    var lead = moved.join(' ') + (input.value.trim() ? ' ' : ''), typing = focused();
    var a = input.selectionStart, b = input.selectionEnd;
    input.value = lead + (input.value.trim() ? input.value : '');
    if (typing && a !== null) input.setSelectionRange(a + lead.length, b + lead.length);
  }
  // apply(held): held is true for a background refresh the hold kept back.
  // That refresh is NOT re-ranked: its markup came in the drawn order
  // (keepDrawnOrder), and re-ranking it would move a drawn card whose rank
  // the refresh changed (another client renamed it while a filter is typed).
  // The rank is applied with everything else on the tap (applyLive), and on
  // the reader's own typing.
  function apply(held) {
    var input = el('agents-search'), list = el('agents-list');
    if (!input || !list) return;
    syncOptions();
    var q = composeQ();
    var ts = terms(q);
    var style = el('agents-filter-style');
    if (!style) {
      style = document.createElement('style');
      style.id = 'agents-filter-style';
      document.head.appendChild(style);
    }
    // One rule per term: a card lacking ANY of them is hidden, so the terms AND.
    // Then the rank (reorder): by the FIRST free-text word, best match first.
    style.textContent = ['#agents-list > article[data-held]{display:none !important}'].concat(ts.map(function (t) {
      return '#agents-list > article:not(' + sel(t) + '){display:none !important}';
    })).join('\n');
    if (!held) reorder(ts);
    var all = list.querySelectorAll(':scope > article:not([data-held])');
    var shown = ts.length ? list.querySelectorAll(':scope > article:not([data-held])' + ts.map(sel).join('')) : all;
    var count = el('agents-count'), clear = el('agents-search-clear'), none = el('agents-nomatch'), clearAll = el('agents-filter-clear');
    if (count) count.textContent = all.length ? shown.length + ' of ' + all.length : '';
    if (clear) clear.hidden = !input.value;
    if (clearAll) clearAll.hidden = !q.trim();
    if (none) none.hidden = !(ts.length && all.length && !shown.length);
    CONTROLS.forEach(function (f) { var c = control(f); if (c) c.toggleAttribute('data-active', !!c.value); });
    try { sessionStorage.setItem(KEY, q); } catch (e) {}
    syncURL(q);
  }
  // 🔴 WHEN A TOKEN MOVES FROM THE BOX TO ITS SELECT — and when it never does.
  // ?q= is rewritten on every keystroke, so it is not a link's filter but the
  // one being typed: re-splitting it on a list refresh took a hand-typed
  // host=macbook out of the box mid-word (the caret jumped, and what was typed
  // next landed in a different string), and with host "mac" selected and
  // host=macbook typed it cleared the select. So a refresh NEVER re-splits.
  // Tokens move only:
  //   - place(): when a bar is first seen (page load, or a swap that brought a
  //     new one) and on back/forward — the filter of a link, the session's copy
  //     or a history entry;
  //   - settled(): the token place() left in the box because its select had no
  //     such option YET (the options arrive with the list, after a link was
  //     read), once, at the first list render after which that select has
  //     options — and not while the box has focus (it waits; typing in the box
  //     cancels it, the text being the user's now).
  // place() itself moves nothing while the box has focus: it puts the whole
  // string in the box, which filters the same.
  var pending = Object.create(null);
  function focused() { var i = el('agents-search'); return !!i && document.activeElement === i; }
  function place(q) {
    var input = el('agents-search');
    if (!input) return;
    pending = Object.create(null);
    if (focused()) {
      CONTROLS.forEach(function (f) { var c = control(f); if (c) c.value = ''; });
      if (input.value !== q) input.value = q;
    } else distribute(q);
    tokens(input.value).forEach(function (t) { if (t.exact && CONTROLS.indexOf(t.f) >= 0) pending[t.f] = true; });
  }
  // takeFromBox moves field f's exact token from the box to f's select, by the
  // same rule as split(): the box's only exact token of f, f's select empty,
  // and an option of exactly that value.
  function takeFromBox(f) {
    var input = el('agents-search'), c = control(f);
    if (!input || !c || c.value) return;
    var ts = tokens(input.value), mine = ts.filter(function (t) { return t.exact && t.f === f; });
    if (mine.length !== 1) return;
    var v = norm(mine[0].v).toLowerCase();
    if (!hasOption(c, v)) return;
    c.value = v;
    input.value = ts.filter(function (t) { return t !== mine[0]; }).map(function (t) { return t.raw; }).join(' ');
  }
  // boot: a bar not seen before (or one a swap emptied while a filter is
  // remembered) is given its filter — a ?q= wins (a shared link), else the
  // session's copy. Any other call is a no-op.
  function boot() {
    var input = el('agents-search');
    if (!input) return;
    var q = urlQ();
    if (q === null) { try { q = sessionStorage.getItem(KEY) || ''; } catch (e) { q = ''; } }
    if (input.__musterFilterBar && (composeQ() || !q)) return;
    input.__musterFilterBar = true;
    syncOptions();
    place(q);
    apply();
  }
  // settled: the list rendered. The options and the count follow it; a pending
  // token may now find its option. Nothing else in the bar is touched.
  function settled() {
    boot();
    syncOptions();
    if (!focused()) {
      CONTROLS.forEach(function (f) {
        var c = control(f);
        if (!pending[f] || !c || c.options.length < 2) return;
        delete pending[f];
        takeFromBox(f);
      });
    }
    apply(swapHeld);
  }
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'agents-search') { pending = Object.create(null); apply(); }
  });
  // A select reports a choice with change (and input, in some browsers): both
  // apply, which is idempotent.
  function onSelect(e) {
    if (e.target && e.target.hasAttribute && e.target.hasAttribute('data-filter-field')) apply();
  }
  document.addEventListener('change', onSelect);
  document.addEventListener('input', onSelect);
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
    if (t.closest('#agents-filter-clear')) {
      CONTROLS.forEach(function (f) { var c = control(f); if (c) c.value = ''; });
      if (input) input.value = '';
      apply();
    }
  });
  // The list re-renders itself; the rule above already hides what should be
  // hidden, so all that is left is the count and the options — and a link's
  // token that was waiting in the box for its option (settled).
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
    return type.indexOf('sse:') === 0 || type === 'muster:resync';
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
  function forget() { heldOrder = null; liveOrder = null; pillCount = 0; }
  document.addEventListener('htmx:historyRestore', forget);
  document.addEventListener('htmx:beforeSwap', function (e) { if (e.target === document.body) forget(); });
  function stamp(list, order) {
    var at = {};
    order.forEach(function (id, i) { at[id] = i; });
    cardsOf(list).forEach(function (c) {
      if (c.id in at) { c.setAttribute('data-list-i', String(at[c.id])); c.removeAttribute('data-held'); }
    });
  }
  function applyLive() {
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
  function decide(e, live) {
    if (!heldOrder || !background(e) || !heldOrder.length) return true;
    // What applying would move: every drawn card from the first place the
    // held and the live order differ. An arrival at the END moves nothing; a
    // change wholly below the screen (or on a hidden tab) moves nothing in
    // view. Either is applied at once; anything else is held.
    var p = 0;
    while (p < heldOrder.length && heldOrder[p] === live[p]) p++;
    return !heldOrder.slice(p).some(inView);
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
    var apply_ = p ? p.apply : decide(e, liveOrder);
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
    // What waits: each arrival, and each card the live order puts HIGHER than
    // it is drawn (one card overtaking three is one change, not four).
    var liveKnown = liveOrder.filter(function (id) { return known[id]; });
    var risen = 0;
    liveKnown.forEach(function (id, i) { if (i < heldOrder.indexOf(id)) risen++; });
    showPill(arrived.length + risen);
    return true;
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
    var live = [].filter.call(tpl.content.children, function (c) { return c.tagName === 'ARTICLE' && c.id; })
      .map(function (c) { return c.id; });
    swapPlan = { live: live, apply: decide(e, live) };
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
    if (input && e.detail && e.detail.tab === 'agents') syncURL(composeQ());
  });
  // Back/forward onto an /agents entry: its ?q= is the filter it had.
  // (An entry with no ?q= leaves the bar as it is, and apply writes it back.)
  window.addEventListener('popstate', function () {
    // The shell's own popstate handler shows the tab; after it, the pill follows.
    setTimeout(syncPill, 0);
    var q = urlQ();
    if (q !== null && el('agents-search')) { syncOptions(); place(q); }
    apply();
  });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
})();
`))
}
