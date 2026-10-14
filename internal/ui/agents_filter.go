package ui

import (
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
// the SAME attribute selector (agentsFilterScript's `has`), so what is counted
// is what is shown.

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
					Placeholder("Filter: name, session, host, kind…"),
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
  function el(id) { return document.getElementById(id); }
  function words(v) { return String(v || '').toLowerCase().split(/\s+/).filter(Boolean); }
  // has is the attribute selector for ONE word, as a CSS double-quoted string.
  // The rule that hides and the query that counts are both built from it.
  function has(word) { return '[data-filter*="' + word.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"]'; }
  function apply() {
    var input = el('agents-search'), list = el('agents-list');
    if (!input || !list) return;
    var ws = words(input.value);
    var style = el('agents-filter-style');
    if (!style) {
      style = document.createElement('style');
      style.id = 'agents-filter-style';
      document.head.appendChild(style);
    }
    // One rule per word: a card lacking ANY of them is hidden, so the words AND.
    style.textContent = ws.map(function (w) {
      return '#agents-list > article:not(' + has(w) + '){display:none !important}';
    }).join('\n');
    var all = list.querySelectorAll(':scope > article');
    var shown = ws.length ? list.querySelectorAll(':scope > article' + ws.map(has).join('')) : all;
    var count = el('agents-count'), clear = el('agents-search-clear'), none = el('agents-nomatch');
    if (count) count.textContent = all.length ? shown.length + ' of ' + all.length : '';
    if (clear) clear.hidden = !input.value;
    if (none) none.hidden = !(ws.length && all.length && !shown.length);
    try { sessionStorage.setItem(KEY, input.value); } catch (e) {}
  }
  function restore() {
    var input = el('agents-search');
    if (!input) return;
    if (!input.value) { try { input.value = sessionStorage.getItem(KEY) || ''; } catch (e) {} }
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
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', restore); else restore();
})();
`))
}
