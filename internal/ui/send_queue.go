package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SEND QUEUE in a mirrored window's Rendered view (internal/sendqueue): a
// "Queue for when idle" item behind Send's caret (composer_menu.go), and the
// window's queue under the status line — what is waiting, what
// it is waiting for, a Cancel for each message not yet being sent, and the
// last few outcomes.
//
// 🔴 THE QUEUE IS THE SERVER'S, NOT THE PAGE'S. Queue posts the message to
// SendQueuePath and the server sends it when the target has been idle long
// enough — with this page closed, or from another device. The list is the
// server's list (GET SendQueuePath), re-read every few seconds while anything
// is open and on every Queue, so a message queued or cancelled elsewhere
// shows up here too.
//
// IT WORKS WITHOUT SCRIPT, like Send: the caret's menu is a popover the
// browser opens itself, Queue in it is a submit button whose formaction is the
// queue route (the browser posts the same form there and is
// redirected back with a notice), the list is drawn into the page by the
// server, and each Cancel is a form of its own.

// SendQueuePath is the queue's route: POST queues a message, GET is a
// target's list (SendQueueList), drawn as a fragment.
const SendQueuePath = "/send-queue"

// SendQueueCancelPath is where message id's Cancel posts.
func SendQueueCancelPath(id int64) string {
	return SendQueuePath + "/" + strconv.FormatInt(id, 10) + "/cancel"
}

// SendQueueRetryPath is where message id's Retry posts.
func SendQueueRetryPath(id int64) string {
	return SendQueuePath + "/" + strconv.FormatInt(id, 10) + "/retry"
}

// SendQueueMayHaveSentText is what a failed message that may already have
// been sent says beside its Retry.
const SendQueueMayHaveSentText = "May already have been sent."

// ComposerQueue is a Rendered view composer's queue: the list's URL, and the
// list as it was when the page was drawn.
type ComposerQueue struct {
	// Src is the list's URL (SendQueuePath with the window's address).
	Src  string
	List SendQueueListView
	// Disabled, when set, draws Queue disabled and says why under the list —
	// a host whose muster-mirror does not report whether Claude Code is busy,
	// a window that is not Claude Code. The server refuses such a queue
	// regardless, and sends nothing to such a target.
	Disabled string
}

// SendQueueItem is one message of the list.
type SendQueueItem struct {
	ID int64
	// State is the sendqueue state: queued, sending, sent, failed, cancelled.
	State string
	// Preview is the start of the message (the owner's own text).
	Preview string
	// Detail is the outcome in words, for a finished one.
	Detail string
	// Cancellable: still queued.
	Cancellable bool
	// Answered is the asker whose question this message already answered when
	// it was queued, "" for none: its Cancel cannot take that answer back.
	Answered string
	// Retryable: it failed and has not been retried; drawn with a Retry.
	Retryable bool
	// MayHaveDelivered: muster stopped while sending it, so it may have
	// reached the window. Its Retry warns, and asks once before it queues it.
	MayHaveDelivered bool
}

// SendQueueAnsweredText is what a queued message that answered asker's
// question when it was queued says under its preview.
func SendQueueAnsweredText(asker string) string { return "Already answered " + asker + "'s question." }

// SendQueueListView is a target's queue.
type SendQueueListView struct {
	Items []SendQueueItem
	// Waiting is what the next message is waiting for (sendqueue.Decide's
	// reason), "" when nothing is queued or it is about to send.
	Waiting string
	// Error is why the list could not be read.
	Error string
}

// active reports whether anything is still to be sent.
func (v SendQueueListView) active() bool {
	for _, it := range v.Items {
		if it.State == "queued" || it.State == "sending" {
			return true
		}
	}
	return false
}

// sendQueueStateLabel is a state as it is shown.
func sendQueueStateLabel(state string) (label, class string) {
	switch state {
	case "queued":
		return "Queued", "bg-st-open-bg text-st-open-fg"
	case "sending":
		return "Sending", "bg-st-progress-bg text-st-progress-fg"
	case "sent":
		return "Sent", "bg-st-complete-bg text-st-complete-fg"
	case "failed":
		return "Failed", "bg-st-error-bg text-st-error-fg"
	case "cancelled":
		return "Cancelled", "bg-s2 text-muted"
	}
	return state, "bg-s2 text-muted"
}

// SendQueueList is the list's markup: what GET SendQueuePath answers, and
// what a composer draws inside its #mv-queue.
func SendQueueList(v SendQueueListView) g.Node {
	if v.Error != "" {
		return P(ID("mv-queue-error"), Class("px-3 pt-2 text-xs text-st-error-fg break-words"), g.Text(v.Error))
	}
	if len(v.Items) == 0 {
		// An empty group, never nil: the GET route renders this node itself,
		// and a nil node has no Render (the 2026-10-19 panic on every window
		// with nothing queued).
		return g.Group(nil)
	}
	open := 0
	rows := make([]g.Node, 0, len(v.Items))
	for _, it := range v.Items {
		if it.State == "queued" || it.State == "sending" {
			open++
		}
		label, tone := sendQueueStateLabel(it.State)
		rows = append(rows, Li(
			g.Attr("data-queue-id", strconv.FormatInt(it.ID, 10)),
			g.Attr("data-queue-state", it.State),
			Class("flex items-start gap-2 py-1"),
			Span(Class("mt-0.5 shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold "+tone), g.Text(label)),
			Div(Class("min-w-0 flex-1"),
				P(Class("line-clamp-2 text-xs text-fg [overflow-wrap:anywhere]"), g.Text(it.Preview)),
				g.If(it.Detail != "" && it.State != "queued" && it.State != "sending",
					P(Class("text-[11px] text-muted [overflow-wrap:anywhere]"), g.Text(it.Detail))),
				g.If(it.Answered != "", P(g.Attr("data-queue-answered", ""), Class("text-[11px] text-muted [overflow-wrap:anywhere]"),
					g.Text(SendQueueAnsweredText(it.Answered)))),
				g.If(it.Retryable && it.MayHaveDelivered, P(g.Attr("data-queue-may-have-sent", ""),
					Class("text-[11px] font-semibold text-st-error-fg [overflow-wrap:anywhere]"), g.Text(SendQueueMayHaveSentText))),
			),
			g.If(it.Retryable, sendQueueRetryForm(it)),
			g.If(it.Cancellable, Form(
				Method("post"),
				Action(SendQueueCancelPath(it.ID)),
				g.Attr("data-cancel", ""),
				Class("shrink-0"),
				Button(Type("submit"),
					g.Attr("aria-label", "Cancel this queued message"),
					Class("press min-h-[44px] rounded-lg px-3 text-xs font-semibold text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
					g.Text("Cancel")),
			)),
		))
	}
	return Div(
		ID("mv-queue-items"),
		g.If(v.active(), g.Attr("data-active", "")),
		Class("px-3 pt-2"),
		g.If(open > 0, P(ID("mv-queue-head"), Class("text-[11px] font-semibold text-fg2"),
			g.Text("Queued: "+strconv.Itoa(open)+" — sends one at a time, each when Claude Code has been idle a while"))),
		g.If(v.Waiting != "", P(ID("mv-queue-waiting"), Class("text-[11px] text-muted [overflow-wrap:anywhere]"),
			g.Text("Next: "+v.Waiting))),
		Ul(Class("max-h-40 overflow-y-auto"), g.Group(rows)),
	)
}

// sendQueueRetryForm is a failed message's Retry: a form of its own, so it
// works without script. One that may already have been sent carries
// data-confirm (the script asks before posting, and confirms in the post); the
// server asks again — a page of its own — when the post does not say so.
func sendQueueRetryForm(it SendQueueItem) g.Node {
	label := "Queue this failed message again"
	if it.MayHaveDelivered {
		label = "Queue this message again (it may already have been sent)"
	}
	return Form(
		Method("post"),
		Action(SendQueueRetryPath(it.ID)),
		g.Attr("data-retry", ""),
		g.If(it.MayHaveDelivered, g.Attr("data-confirm", SendQueueMayHaveSentText+" Queue it again anyway?")),
		Class("shrink-0"),
		Button(Type("submit"),
			g.Attr("aria-label", label),
			Class("press min-h-[44px] rounded-lg px-3 text-xs font-semibold text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
			g.Text("Retry")),
	)
}

// SendQueueRetryConfirmPage is a whole document asking once before a message
// that may already have been sent is queued again: the browser's own answer
// to a Retry posted without the confirm (no script ran). It posts to action
// with field=1.
func SendQueueRetryConfirmPage(action, field, question, back string) g.Node {
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Queue it again? · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				faroHead(),
			),
			Body(
				Class("flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased"),
				Form(
					ID("sq-retry-confirm"),
					Method("post"),
					Action(action),
					Class("flex w-full max-w-sm flex-col gap-4 rounded-2xl bg-s1 px-5 py-6 ring-1 ring-line"),
					Input(Type("hidden"), Name(field), Value("1")),
					H1(Class("text-base font-semibold text-fg"), g.Text("Queue it again?")),
					P(Class("text-sm text-fg2"), g.Text(question)),
					Div(
						Class("flex gap-2"),
						A(Href(back),
							Class("press inline-flex min-h-[44px] flex-1 items-center justify-center rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
							g.Text("Cancel")),
						Button(Type("submit"),
							Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
							g.Text("Queue it again")),
					),
				),
				pwaChrome(),
			),
		),
	)
}

// sendQueueButtonClass is Queue's look: the one item of Send's caret menu
// (composer_menu.go's sendSplit), a full-width 44px row.
const sendQueueButtonClass = "press flex min-h-[44px] w-full items-center rounded-lg px-3 text-left text-sm font-semibold text-fg transition hover:bg-s2 focus:bg-s2 focus:outline-none disabled:opacity-40"

// SendQueueLabel is Queue's text in the caret menu.
const SendQueueLabel = "Queue for when idle"

// sendQueueButton is the composer's Queue: the menu item behind Send's caret,
// and a submit with its own formaction, so the browser posts the composer's
// form (the window's address and the text) to the queue route when no script
// runs. Disabled (and saying why under the list) when the host cannot tell
// when Claude Code is idle.
func sendQueueButton(q *ComposerQueue, off bool) g.Node {
	if q == nil || off {
		return nil
	}
	return Button(
		ID("mv-queue-send"),
		Type("submit"),
		g.Attr("role", "menuitem"),
		g.Attr("formaction", SendQueuePath),
		g.If(q.Disabled != "", g.Attr("disabled", "")),
		g.Attr("aria-label", "Queue: send when Claude Code is idle"),
		g.If(q.Disabled == "", g.Attr("title", "Send when Claude Code has been idle for a while — even after this page is closed")),
		g.If(q.Disabled != "", g.Attr("title", q.Disabled)),
		Class(sendQueueButtonClass),
		g.Text(SendQueueLabel),
	)
}

// sendQueueBox is the queue's place in the composer. Its script
// (sendQueueScript) is drawn after the composer's form, with the composer's
// other scripts (mirrorComposer): a script inside the composer, before its
// form, lets the browser paint the composer half-parsed — one row high — and
// the row it then grows by is a layout shift.
func sendQueueBox(q *ComposerQueue, off bool) g.Node {
	if q == nil || off {
		return nil
	}
	return g.Group([]g.Node{
		Div(ID("mv-queue"), g.Attr("data-src", q.Src), g.Attr("aria-live", "polite"), SendQueueList(q.List)),
		g.If(q.Disabled != "", P(ID("mv-queue-off"), Class("px-3 pt-2 text-[11px] text-muted [overflow-wrap:anywhere]"),
			g.Text("Queue is off here: "+q.Disabled))),
	})
}

// sendQueueScript keeps #mv-queue current: re-read every 3 s while anything is
// open there, every 15 s otherwise, at once on a Queue (the composer's script
// dispatches mv-queued) and when the page is shown again; and it sends each
// Cancel without leaving the page.
func sendQueueScript() g.Node {
	return Script(g.Raw(`
(function () {
  var box = document.getElementById('mv-queue');
  if (!box) return;
  var src = box.getAttribute('data-src');
  var timer = null, loading = false;
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(load, box.querySelector('[data-active]') ? 3000 : 15000);
  }
  function load() {
    if (typeof fetch !== 'function') return;
    if (loading) return;
    if (document.hidden) { schedule(); return; }
    loading = true;
    fetch(src, { credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (t) { loading = false; if (t !== null) box.innerHTML = t; schedule(); },
            function () { loading = false; schedule(); });
  }
  function note(t) {
    var n = document.getElementById('mv-note');
    if (n && t) { n.textContent = t; n.hidden = false; n.removeAttribute('data-ok'); }
  }
  // Retry: queues a failed message again under this session's sign-in. One
  // that may already have been sent is asked about once (data-confirm), and the
  // post says it was; a stale sign-in asks "Confirm it's you" and retries.
  function retry(f, confirmed) {
    var b = f.querySelector('button');
    if (b) b.disabled = true;
    var body = new URLSearchParams();
    if (confirmed) body.append('confirm', '1');
    return fetch(f.getAttribute('action'), { method: 'POST', body: body, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (r.ok) return;
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && window.musterStepUp) {
          window.musterStepUp.stale();
          window.musterStepUp.ask('', function () { return retry(f, confirmed); });
          return;
        }
        return r.text().then(function (t) { note(t.trim() || ('Not queued again (HTTP ' + r.status + ')')); });
      })
      .then(load, load);
  }
  box.addEventListener('submit', function (e) {
    var f = e.target;
    if (f && f.hasAttribute && f.hasAttribute('data-retry') && typeof fetch === 'function' && typeof URLSearchParams === 'function') {
      e.preventDefault();
      var ask = f.getAttribute('data-confirm');
      if (ask && !window.confirm(ask)) return;
      retry(f, !!ask);
      return;
    }
    if (!f || !f.hasAttribute || !f.hasAttribute('data-cancel') || typeof fetch !== 'function') return;
    e.preventDefault();
    var b = f.querySelector('button');
    if (b) b.disabled = true;
    fetch(f.getAttribute('action'), { method: 'POST', credentials: 'same-origin',
      headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (r.status === 204) return;
        if (r.ok) return r.text().then(function (t) {
          // Cancelled, with a warning (an answer it already delivered).
          var n = document.getElementById('mv-note');
          if (n && t.trim()) { n.textContent = t.trim(); n.hidden = false; n.removeAttribute('data-ok'); }
        });
        return r.text().then(function (t) {
          var n = document.getElementById('mv-note');
          if (n) { n.textContent = t.trim() || ('Not cancelled (HTTP ' + r.status + ')'); n.hidden = false; n.removeAttribute('data-ok'); }
        });
      })
      .then(load, load);
  });
  document.addEventListener('mv-queued', load);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) load(); });
  schedule();
})();
`))
}
