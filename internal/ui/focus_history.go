package ui

import (
	"io"
	"net/url"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/textplain"
)

// FOCUS HISTORY (internal/focus, migration 0022): every focus a session has
// had, newest first, scrolled back a page at a time. It is a tab beside "Your
// messages" on a mirrored session's rendered view (chatIndexSheet) and a
// dialog from the header of a dispatched agent's page (focusHistoryDialog).
//
// 🔴 PAGES ARE FETCHED BY focusHistoryScript, NOT BY htmx. The rendered view
// does not load htmx at all (it is its own document with its own socket), so
// one small loader serves both pages: the list ends in a sentinel row naming
// the next page's URL; when the sentinel scrolls into view (an
// IntersectionObserver) or its button is pressed, the page is fetched and the
// sentinel is replaced by it — rows, then the next sentinel, or the end mark.
// The fragment is server-rendered and escaped like every other view
// (RenderFocusHistoryPage); the script parses it with a <template>, never runs
// anything in it.

// FocusHistoryRoute is the history fragment's route.
const FocusHistoryRoute = "/ui/focus/history"

// FocusHistoryPath is one page of subject's ("agent:12", "mirror:<sid>")
// history: the newest page for cursor "", else the page after cursor.
func FocusHistoryPath(subject, cursor string) string {
	q := url.Values{"subject": {subject}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return FocusHistoryRoute + "?" + q.Encode()
}

// FocusHistoryEntry is one past focus as the list shows it.
type FocusHistoryEntry struct {
	Title       string
	Description string
	At          time.Time
}

// FocusHistoryPageView is one fetched page: its entries, newest first, and the
// next page's URL ("" = this page reaches the oldest). First is true for the
// newest page, so an empty one says so instead of ending a list.
type FocusHistoryPageView struct {
	Entries []FocusHistoryEntry
	Next    string
	First   bool
}

// RenderFocusHistoryPage writes one page as list rows: what the sentinel it
// was fetched for is replaced with.
func RenderFocusHistoryPage(w io.Writer, v FocusHistoryPageView) error {
	return focusHistoryRows(v).Render(w)
}

func focusHistoryRows(v FocusHistoryPageView) g.Node {
	if len(v.Entries) == 0 && v.First {
		return focusHistoryEmpty()
	}
	rows := make([]g.Node, 0, len(v.Entries)+1)
	for _, e := range v.Entries {
		rows = append(rows, Li(
			g.Attr("data-focus-history-item", ""),
			Class("flex flex-col gap-1 border-b border-line px-4 py-3"),
			Div(
				Class("flex items-start gap-3"),
				Span(g.Attr("data-focus-history-title", ""), Class("min-w-0 flex-1 break-words text-sm font-semibold text-fg"), g.Text(textplain.Title(e.Title))),
				Span(g.Attr("data-focus-history-at", ""), Class("inline-flex shrink-0 items-center gap-1 text-xs text-muted"), cardTime(e.At), g.Text("ago")),
			),
			g.If(textplain.Title(e.Description) != "", P(g.Attr("data-focus-history-description", ""), Class("break-words text-sm text-fg2"), g.Text(textplain.Title(e.Description)))),
		))
	}
	if v.Next != "" {
		rows = append(rows, focusHistoryMore(v.Next, "Load older"))
	} else {
		rows = append(rows, Li(g.Attr("data-focus-history-end", ""), Class("px-4 py-4 text-center text-xs text-muted"),
			g.Text("The start of this session's focus history.")))
	}
	return g.Group(rows)
}

// focusHistoryEmpty is the list of a session with no history yet.
func focusHistoryEmpty() g.Node {
	return Li(g.Attr("data-focus-history-empty", ""), Class("px-4 py-6 text-center text-sm text-muted"),
		g.Text("No focus history yet. A line is added here each time this session's focus changes."))
}

// focusHistoryMore is the sentinel row that loads href: by being scrolled to,
// or by its button.
func focusHistoryMore(href, label string) g.Node {
	return Li(
		g.Attr("data-focus-history-more", ""),
		g.Attr("data-href", href),
		Class("flex justify-center px-4 py-3"),
		Button(
			Type("button"),
			Class("press inline-flex min-h-[44px] items-center rounded-lg px-4 text-sm font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2 disabled:opacity-60"),
			g.Text(label),
		),
	)
}

// focusHistoryList is the history list for path (FocusHistoryPath of the
// newest page), loaded when it is first shown; with no path (a mirrored window
// whose host reports no Claude session) it is the empty state at once.
func focusHistoryList(path string) g.Node {
	var first g.Node = focusHistoryEmpty()
	if path != "" {
		first = focusHistoryMore(path, "Load focus history")
	}
	return Ol(g.Attr("data-focus-history-list", ""), g.Attr("aria-label", "Focus history, newest first"), Class("py-1"), first)
}

// focusHistoryButton is the agent page's header control that opens
// focusHistoryDialog.
func focusHistoryButton() g.Node {
	return Button(
		Type("button"),
		ID("focus-history-open"),
		g.Attr("data-focus-history-open", "focus-history"),
		g.Attr("aria-label", "Focus history"),
		g.Attr("title", "Focus history"),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", "focus-history"),
		g.Attr("aria-expanded", "false"),
		Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="5"/><circle cx="12" cy="12" r="1"/></svg>`),
	)
}

// focusHistoryDialog is the agent page's focus history: a native modal
// <dialog> (the browser traps focus in it and closes it on Escape), a bottom
// sheet on a phone and centred from sm up, of a FIXED height so a page
// arriving grows the list inside it and moves nothing else.
//
// ⚠ NOT TABS, AND THAT IS THE CHOICE RATHER THAN AN OMISSION: the agent page
// has no "Your messages" sheet to put a tab beside (its chat is the page
// itself, with its own session drawer), so a tablist here would hold one tab.
// The list, its pages and its loader are the rendered view's, shared.
func focusHistoryDialog(path string) g.Node {
	return g.El("dialog",
		ID("focus-history"),
		g.Attr("aria-labelledby", "focus-history-title"),
		g.Attr("data-focus-history-dialog", ""),
		Class("mx-auto mb-0 mt-auto h-[70dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/60 sm:m-auto sm:rounded-2xl [&[open]]:flex"),
		Div(
			Class("flex items-center gap-2 border-b border-line px-4 py-2"),
			H2(ID("focus-history-title"), Class("flex-1 text-sm font-semibold text-fg"), g.Text("Focus history")),
			Button(
				Type("button"),
				g.Attr("data-focus-history-close", ""),
				g.Attr("aria-label", "Close"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
				g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
			),
		),
		Div(ID("focus-history-scroll"), Class("min-h-0 flex-1 overflow-y-auto overscroll-contain"), focusHistoryList(path)),
	)
}

// focusHistoryScript loads history pages (see the file's header) and opens and
// closes focusHistoryDialog. Bound once per document (window.muFocusHistory) by
// delegation; the agent page is boosted, so it re-runs on a swap and must not
// bind twice.
func focusHistoryScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muFocusHistory) return;
  var io = typeof IntersectionObserver === 'function'
    ? new IntersectionObserver(function (es) { es.forEach(function (e) { if (e.isIntersecting) load(e.target); }); })
    : null;
  function scan(root) {
    var ls = (root && root.querySelectorAll ? root : document).querySelectorAll('[data-focus-history-more]:not([data-observed])');
    for (var i = 0; i < ls.length; i++) { ls[i].setAttribute('data-observed', ''); if (io) io.observe(ls[i]); }
  }
  function load(li) {
    if (!li.isConnected || li.hasAttribute('data-loading')) return;
    li.setAttribute('data-loading', '');
    var btn = li.querySelector('button');
    if (btn) { btn.disabled = true; btn.textContent = 'Loading…'; }
    fetch(li.getAttribute('data-href'), { credentials: 'same-origin', headers: { Accept: 'text/html' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.text(); })
      .then(function (html) {
        if (io) io.unobserve(li);
        var tpl = document.createElement('template');
        tpl.innerHTML = html;
        var list = li.parentNode;
        li.replaceWith(tpl.content);
        scan(list);
      })
      .catch(function () {
        li.removeAttribute('data-loading');
        if (btn) { btn.disabled = false; btn.textContent = 'Could not load. Try again'; }
      });
  }
  function dialogFor(b) { return document.getElementById(b.getAttribute('data-focus-history-open')); }
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var more = t.closest('[data-focus-history-more]');
    if (more) { load(more); return; }
    var open = t.closest('[data-focus-history-open]');
    if (open) {
      var d = dialogFor(open);
      if (d && !d.open) { d.showModal(); open.setAttribute('aria-expanded', 'true'); scan(d); }
      return;
    }
    var d2 = t.closest('[data-focus-history-dialog]');
    // A tap on the close button, or on the backdrop (the dialog element itself).
    if (d2 && (t.closest('[data-focus-history-close]') || t === d2)) d2.close();
  });
  document.addEventListener('close', function (e) {
    var d = e.target;
    if (!d || !d.matches || !d.matches('[data-focus-history-dialog]')) return;
    var bs = document.querySelectorAll('[data-focus-history-open="' + d.id + '"]');
    for (var i = 0; i < bs.length; i++) bs[i].setAttribute('aria-expanded', 'false');
  }, true);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { scan(); });
  else scan();
  document.addEventListener('htmx:load', function (e) { scan(e.target); });
  window.muFocusHistory = { scan: scan, load: load };
})();
`))
}
