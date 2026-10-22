package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// agentsRulesJS defines window.__musterAgentsRules — the filter's grammar, the
// chips' table and the hide rules — once per window. It is emitted in BOTH the
// head script and agentsFilterScript, from this one constant: a boosted
// navigation into the Agents tab from a document without the shell's head (an
// agent's page) runs the body script with no head script ever having run.
var agentsRulesJS = `
if (!window.__musterAgentsRules) (function () {
    var FIELDS = ` + cardFieldKeysJSON() + `;
    var CHIP_SEL = ` + chipSelectorsJSON() + `;
    var CHIP_IDS = ` + chipIDsJSON() + `;
    function norm(v) { return String(v || '').replace(/\s+/g, ' ').trim(); }
    // tokens is a string in the grammar, term by term and in order: {raw, f, exact,
    // v} each — f a registered field (lower-cased) or null for free text, exact
    // true for field=value, v the value as written. Inside double quotes a
    // backslash escapes a double quote or a backslash (\" and \\); any other
    // backslash is itself. Outside quotes nothing is escaped.
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
    // chipsOf is a stored chip list, cleaned to the known chips.
    function chipsOf(raw) {
      try { var v = JSON.parse(raw || '[]'); return Array.isArray(v) ? v.filter(function (c) { return CHIP_IDS.indexOf(c) >= 0; }) : []; } catch (e) { return []; }
    }
    // chipSel is the chips' compound selector for a selection ('' for none).
    function chipSel(cs) { return CHIP_SEL[CHIP_IDS.filter(function (c) { return cs.indexOf(c) >= 0; }).join(',')] || ''; }
    // rules is the hide rules for a filter: the box's terms ts, the shown host
    // (exact; '' = All) and the chips' selector cs. hostHas: the host rule
    // applies only to a list holding one of its cards (the first paint, where
    // the script has not looked yet).
    function rules(ts, host, cs, hostHas) {
      var r = ['#agents-list > article[data-held]{display:none !important}'];
      ts.forEach(function (t) { r.push('#agents-list > article:not(' + sel(t) + '){display:none !important}'); });
      if (host) {
        var h = sel({ attr: 'data-f-host', value: host, exact: true });
        r.push((hostHas ? '#agents-list:has(> article' + h + ')' : '#agents-list') + ' > article:not(' + h + '){display:none !important}');
      }
      if (cs) r.push('#agents-list > article:not(' + cs + '){display:none !important}');
      return r.join('\n');
    }
    window.__musterAgentsRules = { tokens: tokens, terms: terms, sel: sel, norm: norm, chipsOf: chipsOf, chipSel: chipSel, rules: rules, CHIP_IDS: CHIP_IDS };
})();
`

// THE LIST'S FILTER STATE, APPLIED BEFORE THE FIRST PAINT.
//
// The Agents tab's document carries its cards (agents_list_ssr.go). What the
// reader had the list narrowed to — the search box's text (sessionStorage, or
// the link's ?q=), the host tab (localStorage) and the quick-filter chips
// (localStorage) — lives in THIS browser, not on the server. agentsFilterScript
// applies it, but it is at the end of <body>: a browser may paint the list
// before it runs (a long document is parsed and painted in pieces), and the
// list would then show unfiltered and shrink to the filtered set — every card
// below the first hidden one jumping up.
//
// So this script, in <head>, writes the SAME hide rules into the SAME <style>
// (#agents-filter-style) before <body> exists. agentsFilterScript later
// rewrites that element with its own, equal, rules; the cards never move.
//
// 🔴 ONE GRAMMAR. The filter's parser (tokens/terms/sel), the chips' table and
// the hide rules are defined once, in agentsRulesJS (window.__musterAgentsRules),
// and both scripts use them — the head and the body cannot disagree about what
// a filter hides.
//
// ⚠ WHY NOT A COOKIE (so the server could render the filtered list itself).
// The state is per-browser by design (the tab per device, the search per
// browser tab), and the body script already owns it; a cookie would be a second
// copy of it on every request, and the server would then have to implement the
// whole grammar a second time — in Go — to agree with the script. Every card
// is still rendered; this only hides, as the script always has.
//
// ⚠ THE HOST TAB'S FALLBACK WITHOUT THE DOM. The script shows All while the
// remembered host has no card on the list (syncHosts); here, before the list
// exists, that is the rule's `:has()` — the host's rule applies only to a list
// that HAS one of that host's cards.
//
// ⚠ NOT APPLIED BEFORE PAINT: the search's RANKING (the cards moved best match
// first, agentsFilterScript's reorder) — it moves DOM nodes, which need the
// list. It runs when the body script boots.
func agentsFilterHead() g.Node {
	return Script(g.Raw(agentsRulesJS + `
(function () {
  var R = window.__musterAgentsRules;

  // The first paint: the state as agentsFilterScript's boot reads it.
  try {
    var onAgents = location.pathname === '/agents';
    var urlQ = null;
    if (onAgents) { try { urlQ = new URL(location.href).searchParams.get('q'); } catch (e) {} }
    var mine = null;
    try { mine = sessionStorage.getItem('` + agentsFilterStorageKey + `'); } catch (e) {}
    var q = urlQ !== null ? urlQ : (mine || '');
    // A link carrying its own ?q= starts on All (boot's linkAll).
    var host = '';
    if (!(urlQ && urlQ !== mine)) { try { host = localStorage.getItem('` + agentsHostTabsStorageKey + `') || ''; } catch (e) {} }
    var cs = '';
    try { cs = R.chipSel(R.chipsOf(localStorage.getItem('` + agentsChipsStorageKey + `'))); } catch (e) {}
    var css = R.rules(R.terms(q), host, cs, true);
    var st = document.createElement('style');
    st.id = 'agents-filter-style';
    st.textContent = css;
    document.head.appendChild(st);
  } catch (e) {}
})();
`))
}
