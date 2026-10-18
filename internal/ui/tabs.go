package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// IN-PAGE TABS: one tablist and its panels, switched without a request.
//
// 🔴 THE WAI-ARIA TABS PATTERN, WHOLE: role=tablist / tab / tabpanel, each tab
// aria-controls its panel and each panel aria-labelledby its tab,
// aria-selected on exactly one tab, a ROVING tabindex (the selected tab is 0,
// the rest -1, so Tab enters the list once and leaves it), and the arrow keys
// — Left/Right, Home/End — move AND select (automatic activation). It is not
// the sidebar's pattern: the sidebar switches pages and is links on purpose;
// this switches panels of ONE page.
//
// The choice PERSISTS per tabs instance, in localStorage under
// "muster:tabs:<key>" — every access in try/catch, because storage can be
// absent or throw (a private window, blocked site data) and the tabs must work
// without it: the server's first tab is then the default.
//
// The script is bound once per document (window.muTabs) by delegation, so tabs
// arriving in a later swap work with no re-binding.

// Tab is one tab and its panel.
type Tab struct {
	// ID is the tab's name within its tablist: what is stored, and what the
	// elements' ids end in. Plain [a-z0-9-].
	ID    string
	Label string
	Panel g.Node
}

// TabSet is a tablist and its panels, drawn apart so the list can sit in a
// header beside other controls. They are tied by ids (aria-controls /
// aria-labelledby), not by nesting.
type TabSet struct {
	List   g.Node
	Panels g.Node
}

// tabClass is a tab's look, shared by every tablist in the app (Tabs here, and
// the Agents list's host tabs, agentsHostTabs). The selected tab is underlined
// in the accent; every tab carries the border (transparent), so selecting one
// moves nothing. It is styled by aria-selected, so a script that sets the
// attribute restyles the tab with no class of its own.
const tabClass = "press inline-flex min-h-[44px] shrink-0 items-center whitespace-nowrap border-b-2 border-transparent px-3 text-sm font-medium text-muted transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-focus aria-selected:border-accent aria-selected:text-fg"

// Tabs is a tablist over tabs' panels, the first selected. id names the
// instance (element ids are id+"-tab-"+Tab.ID and id+"-panel-"+Tab.ID); key is
// what the choice is stored under ("" = not stored); label is the tablist's
// accessible name. Each panel is the scroll container of its own content.
func Tabs(id, key, label string, tabs ...Tab) TabSet {
	list := []g.Node{
		ID(id), g.Attr("role", "tablist"), g.Attr("aria-label", label), g.Attr("data-tabs", ""),
		g.If(key != "", g.Attr("data-tabs-key", key)),
		Class("flex min-w-0 flex-1 items-end gap-1"),
	}
	var panels []g.Node
	for i, t := range tabs {
		tabID, panelID := id+"-tab-"+t.ID, id+"-panel-"+t.ID
		tabIndex, selected := "-1", "false"
		if i == 0 {
			tabIndex, selected = "0", "true"
		}
		list = append(list, Button(
			Type("button"),
			ID(tabID),
			g.Attr("role", "tab"),
			g.Attr("data-tab", t.ID),
			g.Attr("aria-controls", panelID),
			g.Attr("aria-selected", selected),
			g.Attr("tabindex", tabIndex),
			Class(tabClass),
			g.Text(t.Label),
		))
		panels = append(panels, Div(
			ID(panelID),
			g.Attr("role", "tabpanel"),
			g.Attr("data-tab-panel", t.ID),
			g.Attr("aria-labelledby", tabID),
			g.Attr("tabindex", "0"),
			g.If(i != 0, g.Attr("hidden", "")),
			Class("min-h-0 flex-1 flex-col overflow-y-auto overscroll-contain focus-visible:outline-none [&:not([hidden])]:flex"),
			t.Panel,
		))
	}
	return TabSet{List: Div(list...), Panels: g.Group(panels)}
}

// tabsScript switches every [data-tabs] tablist's panels: a click, and the
// arrow keys, Home and End on a focused tab. Each switch is stored (when the
// list has a data-tabs-key), and the stored choice is restored on load and
// after every htmx swap.
func tabsScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muTabs) return;
  var PREFIX = 'muster:tabs:';
  function tabsOf(list) { return Array.prototype.slice.call(list.querySelectorAll('[role="tab"]')); }
  function load(key) { try { return window.localStorage.getItem(PREFIX + key); } catch (e) { return null; } }
  function save(key, v) { try { window.localStorage.setItem(PREFIX + key, v); } catch (e) {} }
  function select(list, tab, opts) {
    opts = opts || {};
    tabsOf(list).forEach(function (t) {
      var on = t === tab;
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      t.setAttribute('tabindex', on ? '0' : '-1');
      var p = document.getElementById(t.getAttribute('aria-controls'));
      if (p) p.hidden = !on;
    });
    if (opts.focus) tab.focus();
    var key = list.getAttribute('data-tabs-key');
    if (key && opts.persist) save(key, tab.getAttribute('data-tab'));
  }
  function restore(root) {
    var lists = (root || document).querySelectorAll('[data-tabs][data-tabs-key]');
    for (var i = 0; i < lists.length; i++) {
      var list = lists[i], v = load(list.getAttribute('data-tabs-key'));
      if (!v) continue;
      var tab = tabsOf(list).filter(function (t) { return t.getAttribute('data-tab') === v; })[0];
      if (tab && tab.getAttribute('aria-selected') !== 'true') select(list, tab, {});
    }
  }
  document.addEventListener('click', function (e) {
    var tab = e.target && e.target.closest && e.target.closest('[data-tabs] [role="tab"]');
    if (!tab) return;
    select(tab.closest('[data-tabs]'), tab, { persist: true });
  });
  document.addEventListener('keydown', function (e) {
    var tab = e.target && e.target.closest && e.target.closest('[data-tabs] [role="tab"]');
    if (!tab) return;
    var list = tab.closest('[data-tabs]'), ts = tabsOf(list), i = ts.indexOf(tab), next = -1;
    switch (e.key) {
      case 'ArrowRight': next = (i + 1) % ts.length; break;
      case 'ArrowLeft': next = (i - 1 + ts.length) % ts.length; break;
      case 'Home': next = 0; break;
      case 'End': next = ts.length - 1; break;
      default: return;
    }
    e.preventDefault();
    select(list, ts[next], { focus: true, persist: true });
  });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { restore(); });
  else restore();
  document.addEventListener('htmx:load', function (e) { restore(e.target); });
  window.muTabs = { select: select, restore: restore };
})();
`))
}
