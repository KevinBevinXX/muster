package ui

import (
	"encoding/json"
	"sort"
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
//
// CONTROLS. Nobody has to type `host:tower`: the bar has one <select> per
// facet field (facetFields: host, type, status, kind), whose options are
// derived SERVER-SIDE from the cards the list currently holds (agentsFacets,
// refreshed out-of-band with every list render), and the box is plain text.
//
// 🔴 THE CONTROLS ARE A VIEW OF THE SAME TOKEN STRING, NOT A SECOND FILTER. The
// script's ONE string (composeQ) is the selects' tokens followed by the box's
// text; that string — unchanged in grammar — is what the hide rules, the
// count, `?q=` and sessionStorage all read and write. Going the other way,
// split() hands the FIRST token of each facet field in a `?q=` to its select
// and leaves everything else (free words, `session:`, a second `host:`) in the
// box, so an old link and a typed token keep working, and `?q=host:x` shows x
// selected. Active selections are drawn as removable chips beside a "Clear all"
// and the "N of M" count.
//
// ⚠ A SELECTED VALUE STILL MATCHES AS A SUBSTRING, the grammar's one rule: host
// "mac" also shows host "macbook". Selecting exactly would need a new token
// operator, which this change deliberately does not add.

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

// facetFields are the fields the bar offers as controls, in the order it
// draws them. Each must be a registered field (TestEveryFacetIsARegisteredField).
var facetFields = []string{"host", "type", "status", "kind"}

// facetOption is one option of a facet's <select>: V is the value as written in
// the card's data-f-<field> (fieldValue), L what the option says.
type facetOption struct {
	V string `json:"v"`
	L string `json:"l"`
}

// agentsFacets is every facet field's options, derived from the cards the list
// holds now: each distinct value once, from the SAME cardFields the cards'
// attributes are written from (agentFields, mirrorFields), so an option can
// never name a value no card carries. Sorted by label, then value.
func agentsFacets(list []AgentCardView, mirrors []MirrorCardView) map[string][]facetOption {
	byKey := map[string]func(cardFields) string{}
	for _, fd := range cardFieldRegistry {
		byKey[fd.Key] = fd.Of
	}
	seen := map[string]map[string]string{}
	add := func(f cardFields) {
		for _, key := range facetFields {
			raw := strings.TrimSpace(byKey[key](f))
			v := fieldValue(raw)
			if v == "" {
				continue
			}
			if seen[key] == nil {
				seen[key] = map[string]string{}
			}
			if _, ok := seen[key][v]; !ok {
				seen[key][v] = facetLabel(key, raw)
			}
		}
	}
	for _, a := range list {
		add(agentFields(a))
	}
	for _, m := range mirrors {
		add(mirrorFields(m))
	}
	out := map[string][]facetOption{}
	for _, key := range facetFields {
		opts := []facetOption{}
		for v, l := range seen[key] {
			opts = append(opts, facetOption{V: v, L: l})
		}
		sort.Slice(opts, func(i, j int) bool {
			li, lj := strings.ToLower(opts[i].L), strings.ToLower(opts[j].L)
			if li != lj {
				return li < lj
			}
			return opts[i].V < opts[j].V
		})
		out[key] = opts
	}
	return out
}

// facetLabel is what an option says: the value as the card spells it, with the
// two card types named as a person would.
func facetLabel(key, raw string) string {
	if key == "type" {
		switch raw {
		case "agent":
			return "Agent"
		case "mirrored":
			return "Mirrored"
		}
	}
	return raw
}

// agentsFacetsID is the element that carries the options to the script.
const agentsFacetsID = "agents-facets"

// agentsFacetsNode carries the facets, as JSON in an attribute, to the bar's
// script. The shell draws it empty; every /ui/agents render sends it again
// OUT OF BAND (oob), because the bar sits OUTSIDE #agents-list and the list's
// own swap never reaches it — and #agents-list may hold only cards (the
// filter's `#agents-list > article` rule).
func agentsFacetsNode(f map[string][]facetOption, oob bool) g.Node {
	b, _ := json.Marshal(f)
	return Div(
		ID(agentsFacetsID),
		g.Attr("hidden", ""),
		g.Attr("data-facets", string(b)),
		g.If(oob, hx("hx-swap-oob", "true")),
	)
}

// AgentsListPartial is the /ui/agents response: the cards (AgentsCardsWithMirrors)
// and, out of band, the filter's options derived from those same cards.
func AgentsListPartial(list []AgentCardView, mirrors []MirrorCardView) g.Node {
	return g.Group{AgentsCardsWithMirrors(list, mirrors), agentsFacetsNode(agentsFacets(list, mirrors), true)}
}

// facetAny is a facet select's first option, which selects nothing.
var facetAny = map[string]string{"host": "Any host", "type": "Any type", "status": "Any status", "kind": "Any kind"}

// facetSelect is one facet's control. Its options beyond "Any …" are filled by
// the script from agentsFacetsNode. text-base: 16px, so iOS does not zoom; a
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
// one select per facet field, the active filters as removable chips with a
// "Clear all", and the "N of M" count. It sits in the Agents panel ABOVE
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
				Class("flex flex-wrap items-center gap-2"),
				// The script draws one removable chip per active filter here.
				Div(ID("agents-active"), Class("flex flex-wrap items-center gap-2")),
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
			agentsFacetsNode(agentsFacets(nil, nil), false),
		),
		P(ID("agents-nomatch"), g.Attr("hidden", ""), Class("mb-3 text-sm text-muted"), g.Text("Nothing matches that filter.")),
	}
}

// facetFieldsJSON is facetFields as a JS array literal.
func facetFieldsJSON() string {
	b, _ := json.Marshal(facetFields)
	return string(b)
}

// filterChipClass is a removable active-filter chip, built by the script.
const filterChipClass = "press inline-flex min-h-[44px] max-w-full items-center gap-1.5 rounded-full bg-accent/15 px-3 text-sm font-medium text-fg ring-1 ring-inset ring-accent/40 transition hover:bg-accent/25"

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
  // The facet fields the bar has a <select> for (facetFields), id agents-f-<field>.
  var CONTROLS = ` + facetFieldsJSON() + `;
  var CHIP = '` + filterChipClass + `';
  function control(f) { return el('agents-f-' + f); }
  // tok spells one select's value as a token: quoted when it holds a space. A
  // double quote cannot be spelled in the grammar, so it is dropped.
  function tok(f, v) { v = norm(v).replace(/"/g, ''); return /\s/.test(v) ? f + ':"' + v + '"' : f + ':' + v; }
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
  // split is a token string as the bar holds it: the FIRST token of each facet
  // field goes to that field's select; every other token — a free word, a
  // non-facet field like session:, a second host: — stays in the box, in
  // order. With no facet token at all the box gets the string untouched.
  function split(q) {
    var s = String(q || ''), fields = {}, text = [], m, any = false;
    var re = /([^\s:"]+):"([^"]*)"?|"([^"]*)"?|\S+/g;
    while ((m = re.exec(s))) {
      var f = null, v = null;
      if (m[1] !== undefined) { f = m[1].toLowerCase(); v = m[2]; }
      else if (m[3] === undefined) {
        var i = m[0].indexOf(':');
        if (i > 0) { f = m[0].slice(0, i).toLowerCase(); v = m[0].slice(i + 1); }
      }
      if (f !== null && CONTROLS.indexOf(f) >= 0 && norm(v) && !(f in fields)) {
        fields[f] = norm(v).toLowerCase();
        any = true;
        continue;
      }
      text.push(m[0]);
    }
    return { fields: fields, text: any ? text.join(' ') : s };
  }
  // setControl selects v in c. A value no card offers (a partial host:tow from
  // a link, a host that is not listed now) is added as an option of its own, so
  // the link still shows what it asks for.
  function setControl(c, v) {
    if (v && ![].some.call(c.options, function (o) { return o.value === v; })) {
      var o = document.createElement('option');
      o.value = v;
      o.textContent = v;
      c.appendChild(o);
    }
    c.value = v || '';
  }
  function distribute(q) {
    var p = split(q);
    CONTROLS.forEach(function (f) { var c = control(f); if (c) setControl(c, p.fields[f] || ''); });
    var input = el('agents-search');
    if (input && input.value !== p.text) input.value = p.text;
  }
  // syncFacets fills each select's options from the server's facets
  // (agentsFacetsNode, sent out of band with every list render), keeping what
  // is selected. A select remembers which facets it was built from, so an
  // unchanged list rebuilds nothing.
  function syncFacets() {
    var d = el('` + agentsFacetsID + `');
    if (!d) return;
    var raw = d.getAttribute('data-facets') || '{}', fx = null;
    CONTROLS.forEach(function (f) {
      var c = control(f);
      if (!c || c.getAttribute('data-facets-of') === raw) return;
      if (fx === null) { try { fx = JSON.parse(raw) || {}; } catch (e) { fx = {}; } }
      var cur = c.value;
      while (c.options.length > 1) c.remove(1);
      (fx[f] || []).forEach(function (o) {
        var op = document.createElement('option');
        op.value = o.v;
        op.textContent = o.l;
        c.appendChild(op);
      });
      setControl(c, cur);
      c.setAttribute('data-facets-of', raw);
    });
  }
  // renderActive draws one removable chip per active filter — each chosen
  // select, and the box's text — and shows "Clear all" while any is active.
  // Rebuilt only when what it shows changes.
  function renderActive() {
    var box = el('agents-active');
    if (!box) return;
    var want = [];
    CONTROLS.forEach(function (f) {
      var c = control(f);
      if (c && c.value) want.push({ key: f, label: f + ': ' + (c.selectedIndex >= 0 ? c.options[c.selectedIndex].textContent : c.value) });
    });
    var input = el('agents-search');
    if (input && input.value.trim()) want.push({ key: 'text', label: '“' + input.value.trim() + '”' });
    var sig = JSON.stringify(want);
    if (box.getAttribute('data-sig') !== sig) {
      box.setAttribute('data-sig', sig);
      box.textContent = '';
      want.forEach(function (w) {
        var b = document.createElement('button');
        b.type = 'button';
        b.className = CHIP;
        b.setAttribute('data-filter-chip', w.key);
        b.setAttribute('aria-label', 'Remove filter ' + w.label);
        var t = document.createElement('span');
        t.className = 'truncate';
        t.textContent = w.label;
        var x = document.createElement('span');
        x.setAttribute('aria-hidden', 'true');
        x.className = 'text-base leading-none text-muted';
        x.textContent = '×';
        b.appendChild(t);
        b.appendChild(x);
        box.appendChild(b);
      });
    }
    var all = el('agents-filter-clear');
    if (all) all.hidden = !want.length;
  }
  function apply() {
    var input = el('agents-search'), list = el('agents-list');
    if (!input || !list) return;
    syncFacets();
    var q = composeQ();
    var ts = terms(q);
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
    CONTROLS.forEach(function (f) { var c = control(f); if (c) c.toggleAttribute('data-active', !!c.value); });
    renderActive();
    try { sessionStorage.setItem(KEY, q); } catch (e) {}
    syncURL(q);
  }
  // restore: a ?q= wins (a shared link); otherwise the session's copy fills an
  // empty filter. It is only distributed when it differs from what the bar
  // already holds, so a list refresh (which lands here via htmx:load) never
  // moves the caret.
  function restore() {
    var input = el('agents-search');
    if (!input) return;
    syncFacets();
    var q = urlQ();
    if (q !== null) { if (composeQ() !== q) distribute(q); }
    else if (!composeQ()) { try { distribute(sessionStorage.getItem(KEY) || ''); } catch (e) {} }
    apply();
  }
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'agents-search') apply();
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
    var chip = t.closest('[data-filter-chip]');
    if (chip) {
      var key = chip.getAttribute('data-filter-chip');
      if (key === 'text') { if (input) input.value = ''; }
      else { var c = control(key); if (c) c.value = ''; }
      apply();
      return;
    }
    if (t.closest('#agents-filter-clear')) {
      CONTROLS.forEach(function (f) { var c = control(f); if (c) c.value = ''; });
      if (input) input.value = '';
      apply();
    }
  });
  // The list re-renders itself; the rule above already hides what should be
  // hidden, so all that is left is the count — and the options, which arrive
  // out of band with the same render.
  document.addEventListener('htmx:afterSettle', function (e) {
    if (e.target && e.target.id === 'agents-list') apply();
  });
  document.addEventListener('htmx:oobAfterSwap', function (e) {
    if (e.target && e.target.id === '` + agentsFacetsID + `') apply();
  });
  document.addEventListener('htmx:load', restore);
  // Back on the Agents tab (switchTab pushed a bare /agents): put the filter's
  // ?q= back on it at once — a replace, so the switch is still one entry.
  document.addEventListener('muster:tabshown', function (e) {
    var input = el('agents-search');
    if (input && e.detail && e.detail.tab === 'agents') syncURL(composeQ());
  });
  // Back/forward onto an /agents entry: its ?q= is the filter it had.
  window.addEventListener('popstate', restore);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', restore); else restore();
})();
`))
}
