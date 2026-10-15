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
//	host:tower      [data-f-host*="tower"]      that field only
//	kind:"claude code"                          a quoted value may hold spaces
//
// Terms are whitespace-separated and ALL must match. A field value matches as a
// case-insensitive SUBSTRING of that field — the same rule as a free word, so
// `host:tow` behaves like typing `tow`, only narrower. A token whose prefix is
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

// cardFields is what a card can be filtered by, one value per field. Empty
// fields write no attribute.
//
// 🔴 HOST FOR A DISPATCHED POD AGENT IS THE LITERAL "cluster" (podHost): muster
// has no cluster name to put there, and naming it after a machine would merge
// "my tmux on that machine" with "pods on that cluster" — the very cut host:
// exists to make. `ns:` is the finer cut for pods.
type cardFields struct {
	Host, Type, Name, Status, Kind, Repo, Account, Session, NS, Mode, State string
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
// the free-text `data-filter` haystack (exactly filterHaystack of the parts)
// and one `data-f-<field>` per non-empty field.
func cardFilterAttrs(f cardFields, haystack ...string) g.Node {
	attrs := g.Group{g.Attr("data-filter", filterHaystack(haystack...))}
	for _, fd := range cardFieldRegistry {
		if v := fieldValue(fd.Of(f)); v != "" {
			attrs = append(attrs, g.Attr("data-f-"+fd.Key, v))
		}
	}
	return attrs
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

// agentsFilterBar is the search box, its clear button and the "N of M" count.
// It sits in the Agents panel ABOVE #agents-list, outside what a refresh
// re-renders.
func agentsFilterBar() g.Node {
	return g.Group{
		Div(
			ID("agents-filter"),
			Class("mb-3 flex items-center gap-2"),
			Div(
				Class("relative min-w-0 flex-1"),
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
					g.Attr("aria-label", "Filter agents and sessions"),
					g.Attr("aria-controls", "agents-list"),
					Placeholder("Filter: words, or host:… kind:… status:…"),
					// text-base: 16px. iOS zooms the page on focusing a smaller input.
					Class("min-h-[44px] w-full rounded-xl border-0 bg-s2 py-2.5 pl-3 pr-12 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
				// ⚠ No display utility on this button: it is shown and hidden with
				// the `hidden` ATTRIBUTE, which an author display class would beat.
				Button(
					ID("agents-search-clear"),
					Type("button"),
					g.Attr("hidden", ""),
					g.Attr("aria-label", "Clear the filter"),
					Class("press absolute right-0 top-0 h-11 w-11 rounded-xl text-xl leading-none text-muted transition hover:text-fg"),
					g.Text("×"),
				),
			),
			Span(ID("agents-count"), g.Attr("aria-live", "polite"), Class("shrink-0 text-xs tabular-nums text-muted")),
		),
		P(ID("agents-nomatch"), g.Attr("hidden", ""), Class("mb-3 text-sm text-muted"), g.Text("Nothing matches that filter.")),
	}
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
  // terms parses the box into what must ALL match: {attr, value} each, attr
  // being data-filter (free text) or data-f-<registered field>.
  function terms(v) {
    var out = [], s = String(v || '').toLowerCase(), m;
    var re = /([^\s:"]+):"([^"]*)"?|"([^"]*)"?|\S+/g;
    function free(t) { t = norm(t); if (t) out.push({ attr: 'data-filter', value: t }); }
    function field(f, t) { t = norm(t); if (t) out.push({ attr: 'data-f-' + f, value: t }); }
    while ((m = re.exec(s))) {
      if (m[1] !== undefined) {
        if (FIELDS.indexOf(m[1]) >= 0) field(m[1], m[2]); else free(m[1] + ':' + m[2]);
      } else if (m[3] !== undefined) {
        free(m[3]);
      } else {
        var i = m[0].indexOf(':');
        if (i > 0 && FIELDS.indexOf(m[0].slice(0, i)) >= 0) field(m[0].slice(0, i), m[0].slice(i + 1)); else free(m[0]);
      }
    }
    return out;
  }
  // sel is the attribute selector for ONE term, its value a CSS double-quoted
  // string. The rule that hides and the query that counts are both built from it.
  function sel(t) { return '[' + t.attr + '*="' + t.value.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"]'; }
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
  function apply() {
    var input = el('agents-search'), list = el('agents-list');
    if (!input || !list) return;
    var ts = terms(input.value);
    var style = el('agents-filter-style');
    if (!style) {
      style = document.createElement('style');
      style.id = 'agents-filter-style';
      document.head.appendChild(style);
    }
    // One rule per term: a card lacking ANY of them is hidden, so the terms AND.
    style.textContent = ts.map(function (t) {
      return '#agents-list > article:not(' + sel(t) + '){display:none !important}';
    }).join('\n');
    var all = list.querySelectorAll(':scope > article');
    var shown = ts.length ? list.querySelectorAll(':scope > article' + ts.map(sel).join('')) : all;
    var count = el('agents-count'), clear = el('agents-search-clear'), none = el('agents-nomatch');
    if (count) count.textContent = all.length ? shown.length + ' of ' + all.length : '';
    if (clear) clear.hidden = !input.value;
    if (none) none.hidden = !(ts.length && all.length && !shown.length);
    try { sessionStorage.setItem(KEY, input.value); } catch (e) {}
    syncURL(input.value);
  }
  // restore: a ?q= wins (a shared link); otherwise the session's copy fills an
  // empty box. The value is only written when it differs, so a list refresh
  // (which lands here via htmx:load) never moves the caret.
  function restore() {
    var input = el('agents-search');
    if (!input) return;
    var q = urlQ();
    if (q !== null) { if (input.value !== q) input.value = q; }
    else if (!input.value) { try { input.value = sessionStorage.getItem(KEY) || ''; } catch (e) {} }
    apply();
  }
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'agents-search') apply();
  });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('#agents-search-clear');
    if (!b) return;
    var input = el('agents-search');
    if (!input) return;
    input.value = '';
    apply();
    input.focus();
  });
  // The list re-renders itself; the rule above already hides what should be
  // hidden, so all that is left is the count.
  document.addEventListener('htmx:afterSettle', function (e) {
    if (e.target && e.target.id === 'agents-list') apply();
  });
  document.addEventListener('htmx:load', restore);
  // Back on the Agents tab (switchTab pushed a bare /agents): put the filter's
  // ?q= back on it at once — a replace, so the switch is still one entry.
  document.addEventListener('muster:tabshown', function (e) {
    var input = el('agents-search');
    if (input && e.detail && e.detail.tab === 'agents') syncURL(input.value);
  });
  // Back/forward onto an /agents entry: its ?q= is the filter it had.
  window.addEventListener('popstate', restore);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', restore); else restore();
})();
`))
}
