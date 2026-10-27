package ui

import (
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SEND QUEUE in a mirrored window's Rendered view (internal/sendqueue): a
// "Queue for when idle" item behind Send's caret (composer_menu.go), and the
// window's queue under the status line — what is waiting, what
// it is waiting for, a ⋯ menu for each message not yet being sent (Send now,
// Copy message, Cancel: sendQueueRowMenu), and the last few outcomes, each
// with a Delete.
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
// server, each row's ⋯ menu is a popover too, and each Send now, Cancel, Retry
// and Delete is a form of its own. (Copy message needs script, and is not
// offered without it.)

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

// SendQueueDeletePath is where message id's Delete posts.
func SendQueueDeletePath(id int64) string {
	return SendQueuePath + "/" + strconv.FormatInt(id, 10) + "/delete"
}

// SendQueueSendNowPath is where message id's Send now posts.
func SendQueueSendNowPath(id int64) string {
	return SendQueuePath + "/" + strconv.FormatInt(id, 10) + "/send-now"
}

// SendQueueEditPath is where message id's Edit posts: it HOLDS the message
// (the dispatcher sends nothing of its window while it is held), and then the
// row is drawn as its edit form (sendQueueEditRow).
func SendQueueEditPath(id int64) string {
	return SendQueuePath + "/" + strconv.FormatInt(id, 10) + "/edit"
}

// SendQueueEditSavePath is where an edit's Save posts (hold, text).
func SendQueueEditSavePath(id int64) string { return SendQueueEditPath(id) + "/save" }

// SendQueueEditCancelPath is where an edit's Cancel posts (hold).
func SendQueueEditCancelPath(id int64) string { return SendQueueEditPath(id) + "/cancel" }

// SendQueueEditParam is the query parameter that draws a held row open as its
// edit form, on the view (a no-script Edit's redirect) and on the list (the
// script's re-read after an Edit).
const SendQueueEditParam = "edit"

// SendQueueHoldParam is the hold an Edit gave this device: the form is drawn
// only under it (a held row named under any other is being edited elsewhere).
const SendQueueHoldParam = "hold"

// SendQueueDeleteLabel is a finished message's Delete, as a screen reader
// names it.
const SendQueueDeleteLabel = "Delete this message"

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
	// Body is the whole message, for a queued one's Copy message; "" for the
	// others, which have no Copy.
	Body string
	// Detail is the outcome in words, for a finished one.
	Detail string
	// Cancellable: still queued.
	Cancellable bool
	// Answered is the asker whose question this message already answered when
	// it was queued, "" for none: its Cancel cannot take that answer back.
	Answered string
	// Retryable: it failed and has not been retried; drawn with a Retry.
	Retryable bool
	// Deletable: it is finished (sent, failed, cancelled), so its owner may
	// remove it from the list; drawn with a Delete. Never a queued message
	// (that is Cancel) or one being sent.
	Deletable bool
	// MayHaveDelivered: muster stopped while sending it, so it may have
	// reached the window. Its Retry warns, and asks once before it queues it.
	MayHaveDelivered bool
	// Editable: queued, and a person's own (a routed message's text is the
	// agent's); its menu has Edit.
	Editable bool
	// Held: held for an edit right now (here or on another device); nothing of
	// the window's queue is sent until it is saved, cancelled or runs out.
	Held bool
	// HoldLapsed: an edit was opened and not saved in time; it kept its text.
	HoldLapsed bool
	// Edited: its text was changed by a saved edit.
	Edited bool
	// HeldElsewhere: this page asked for the row open, but its hold is not the
	// one this page's Edit was given — another device took the edit over.
	HeldElsewhere bool
	// Editing: draw the row as its edit form (sendQueueEditRow), under
	// HoldToken, the hold its Save and Cancel present. HoldLeftMS is how long
	// the hold has left, which the list's script counts down.
	Editing    bool
	HoldToken  string
	HoldLeftMS int64
}

// What a held, a lapsed and an edited message say under their preview.
const (
	SendQueueHeldText          = "Being edited: held, and nothing queued for this window is sent until the edit is saved or cancelled."
	SendQueueHeldElsewhereText = "Being edited on another device: held, and nothing queued for this window is sent until that edit is saved or cancelled. Edit it here to take it over."
	SendQueueLapsedText        = "An edit was not saved in time, so its hold ran out and the message kept its original text."
	SendQueueEditedText        = "Edited."
)

// What the list's script says when an edit is saved, and cancelled.
const (
	SendQueueSavedNote         = "Saved."
	SendQueueEditCancelledNote = "Edit cancelled: the message is unchanged."
)

// SendQueueEditHoldMinutes is how long an edit holds its message, in minutes
// (sendqueue.EditHold; a test in internal/api holds the two together).
const SendQueueEditHoldMinutes = 10

// SendQueueEditAnsweredText is what the edit form of a message that answered
// asker's question when it was queued says before anything is typed: the
// answer stands, whatever the edit.
func SendQueueEditAnsweredText(asker string) string {
	return "This message already answered " + asker + "'s question when it was queued, and that answer was delivered: a Save changes what is pasted into the session, not the answer."
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
	// Held: the queue is held on how the window's latest turn failed — a usage
	// limit, or the circuit breaker (sendqueue.Decision.Held). Waiting says
	// why; Send now still sends one.
	Held bool
	// ResumeAt is when a held queue resumes — a usage limit's reset, the
	// breaker's next retry — zero when it names none. Drawn after Waiting in
	// the READER's zone (a <time data-clock> the list's script formats); the
	// server, which does not know that zone, draws it in UTC, saying so.
	ResumeAt time.Time
	// Now is the server's clock when the view was drawn: whether ResumeAt is
	// today decides whether its date is drawn (utcClock).
	Now time.Time
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
	case "editing":
		return "Editing", "bg-st-review-bg text-st-review-fg"
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
		if it.Editing {
			rows = append(rows, sendQueueEditRow(it))
			continue
		}
		shown := it.State
		if it.Held {
			shown = "editing"
		}
		label, tone := sendQueueStateLabel(shown)
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
				g.If(it.Held && !it.HeldElsewhere, P(g.Attr("data-queue-held", ""), Class("text-[11px] text-muted [overflow-wrap:anywhere]"), g.Text(SendQueueHeldText))),
				g.If(it.HeldElsewhere, P(g.Attr("data-queue-held", ""), g.Attr("data-queue-held-elsewhere", ""),
					Class("text-[11px] font-semibold text-st-warning-fg [overflow-wrap:anywhere]"), g.Text(SendQueueHeldElsewhereText))),
				g.If(it.HoldLapsed && !it.Held, P(g.Attr("data-queue-lapsed", ""), Class("text-[11px] font-semibold text-st-warning-fg [overflow-wrap:anywhere]"),
					g.Text(SendQueueLapsedText))),
				g.If(it.Edited && it.State == "queued" && !it.Held && !it.HoldLapsed,
					P(g.Attr("data-queue-edited", ""), Class("text-[11px] text-muted"), g.Text(SendQueueEditedText))),
			),
			g.If(it.Retryable, sendQueueRetryForm(it)),
			g.If(it.Cancellable, sendQueueRowMenu(it)),
			g.If(it.Deletable, Form(
				Method("post"),
				Action(SendQueueDeletePath(it.ID)),
				g.Attr("data-delete", ""),
				Class("shrink-0"),
				Button(Type("submit"),
					g.Attr("aria-label", SendQueueDeleteLabel),
					Class("press min-h-[44px] rounded-lg px-3 text-xs font-semibold text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
					g.Text("Delete")),
			)),
		))
	}
	return Div(
		ID("mv-queue-items"),
		g.If(v.active(), g.Attr("data-active", "")),
		Class("px-3 pt-2"),
		g.If(open > 0, P(ID("mv-queue-head"), Class("text-[11px] font-semibold text-fg2"),
			g.Text("Queued: "+strconv.Itoa(open)+" — sends one at a time, each when Claude Code has been idle a while"))),
		g.If(v.Waiting != "", sendQueueWaiting(v)),
		g.If(v.Waiting != "", sendQueueWaitingFull()),
		Ul(Class("max-h-40 overflow-y-auto"), g.Group(rows)),
	)
}

// sendQueueWaiting is the line saying what the next message waits for. It
// RESERVES two lines whatever it says, so the queue going from "working" (one
// line) to held on a usage limit (two, at a phone's width) does not grow the
// composer — which is anchored to the bottom, so a line more moved its top: a
// layout shift of 0.005–0.006 measured without the reservation (two runs), 0 with it. The turn
// that hit the limit is not the reader's input, so any shift counts against
// the page (e2e send-queue: "going from working to held … moves nothing").
//
// Held, it says so in the queue's own words (Waiting: "Held — usage limit,
// resumes") and then the time it resumes, if any, in the READER's zone — "10:30
// PM" — through a <time data-clock> the list's script formats. Without script
// the server, which cannot know that zone, draws the time in UTC and says so.
//
// The line is drawn only while something is queued (Waiting is set only for a
// head), so an idle composer with an empty queue reserves nothing.
func sendQueueWaiting(v SendQueueListView) g.Node {
	// At most two lines, whatever the width (a 320px phone included): the
	// line is clamped, and its whole text is its title.
	cls := "min-h-[2lh] line-clamp-2 text-[11px] [overflow-wrap:anywhere]"
	// A tap (or Enter) shows the whole line in a popover (sendQueueWaitingFull):
	// it opens in the top layer, so nothing on the page moves.
	tap := g.Group([]g.Node{g.Attr("tabindex", "0"), g.Attr("role", "button"), g.Attr("aria-controls", SendQueueWaitingFullID),
		g.Attr("aria-label", "Show the whole line")})
	if !v.Held {
		text := "Next: " + v.Waiting
		return P(ID("mv-queue-waiting"), g.Attr("title", text), tap, Class(cls+" cursor-pointer text-muted"), g.Text(text))
	}
	text := v.Waiting
	if !v.ResumeAt.IsZero() {
		text += " " + utcClock(v.ResumeAt, v.Now)
	}
	return P(ID("mv-queue-waiting"), g.Attr("data-queue-hold", ""), g.Attr("title", text), tap, Class(cls+" cursor-pointer font-semibold text-st-warning-fg"),
		g.Text(v.Waiting),
		g.If(!v.ResumeAt.IsZero(), g.Group([]g.Node{g.Text(" "),
			g.El("time", g.Attr("data-clock", strconv.FormatInt(v.ResumeAt.UnixMilli(), 10)),
				g.Attr("datetime", v.ResumeAt.UTC().Format(time.RFC3339)), g.Text(utcClock(v.ResumeAt, v.Now)))})))
}

// SendQueueWaitingFullID is the popover that shows the waiting line whole.
const SendQueueWaitingFullID = "mv-queue-waiting-full"

// sendQueueWaitingFull is the waiting line's popover: empty until the list's
// script fills it with the line's whole text, as drawn (its time in the
// reader's zone), and opens it. A popover is in the top layer: opening it
// moves nothing on the page — the line itself stays clamped to two lines.
func sendQueueWaitingFull() g.Node {
	return Div(ID(SendQueueWaitingFullID), g.Attr("popover", ""), g.Attr("role", "status"),
		Class("m-auto max-w-[min(90vw,28rem)] rounded-xl border-0 bg-s1 p-3 text-xs text-fg shadow-lg ring-1 ring-line [overflow-wrap:anywhere]"))
}

// utcClock is t as the server draws it for a reader whose zone it does not
// know: in UTC, saying so — "22:30 UTC" when it falls on now's UTC date,
// "Oct 27 22:30 UTC" when not (a weekly limit's reset is days away), and
// with the date whenever now is unknown. The list's script redraws it in the
// reader's own zone, the same way.
func utcClock(t, now time.Time) string {
	t = t.UTC()
	if !now.IsZero() && t.Format("2006-01-02") == now.UTC().Format("2006-01-02") {
		return t.Format("15:04") + " UTC"
	}
	return t.Format("Jan 2 15:04") + " UTC"
}

// The labels of a queued message's ⋯ menu.
const (
	SendQueueMenuLabel    = "More actions for this queued message"
	SendQueueSendNowLabel = "Send now"
	SendQueueEditLabel    = "Edit"
	SendQueueCopyLabel    = "Copy message"
	SendQueueCancelLabel  = "Cancel this queued message"
)

// The edit form's labels.
const (
	SendQueueEditTextLabel   = "Edit this queued message"
	SendQueueEditSaveLabel   = "Save"
	SendQueueEditCancelLabel = "Cancel editing; keep the message as it was"
)

// sendQueueEditRow is a HELD message drawn open for editing, in place of its
// row: its whole text in a box, Save and Cancel. It is a real form (Save posts
// to SendQueueEditSavePath; Cancel is a submit whose formaction is
// SendQueueEditCancelPath), so it works without script, and it carries the
// hold it was opened under — a Save under any other hold is refused.
//
// The row says, while it is open, that the message and everything queued after
// it are held, and for how long at most; the list's script counts the hold down
// (data-hold-left-ms) and says when it has run out.
func sendQueueEditRow(it SendQueueItem) g.Node {
	id := strconv.FormatInt(it.ID, 10)
	label, tone := sendQueueStateLabel("editing")
	return Li(
		g.Attr("data-queue-id", id),
		g.Attr("data-queue-state", it.State),
		g.Attr("data-queue-editing", ""),
		Class("py-1"),
		Form(
			ID("mv-qe-"+id),
			Method("post"),
			Action(SendQueueEditSavePath(it.ID)),
			g.Attr("data-queue-edit-form", ""),
			g.Attr("data-hold-left-ms", strconv.FormatInt(max(it.HoldLeftMS, 0), 10)),
			g.Attr("novalidate", ""),
			Class("flex flex-col gap-2"),
			Input(Type("hidden"), Name("hold"), Value(it.HoldToken)),
			Div(Class("flex items-center gap-2"),
				Span(Class("shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold "+tone), g.Text(label)),
				P(g.Attr("data-queue-edit-note", ""), g.Attr("role", "status"), Class("min-w-0 flex-1 text-[11px] text-muted [overflow-wrap:anywhere]"),
					g.Text("Held while you edit: nothing queued for this window is sent until you Save or Cancel. "+
						"Left open, the hold runs out after "+strconv.Itoa(SendQueueEditHoldMinutes)+" minutes and the message keeps its original text.")),
			),
			g.If(it.Answered != "", P(g.Attr("data-queue-edit-answered", ""),
				Class("text-[11px] font-semibold text-st-warning-fg [overflow-wrap:anywhere]"), g.Text(SendQueueEditAnsweredText(it.Answered)))),
			Textarea(
				ID("mv-qe-text-"+id),
				Name("text"),
				Rows("3"),
				g.Attr("aria-label", SendQueueEditTextLabel),
				// 16px, like every message box: iOS zooms on a smaller one.
				Class("min-h-[44px] w-full resize-y rounded-xl border-0 bg-s2 px-3 py-2 text-base text-fg ring-1 ring-inset ring-line focus:outline-none focus:ring-2 focus:ring-focus"),
				g.Text(it.Body),
			),
			Div(Class("flex justify-end gap-2"),
				Button(Type("submit"), g.Attr("formaction", SendQueueEditCancelPath(it.ID)), g.Attr("data-queue-edit-cancel", ""),
					g.Attr("aria-label", SendQueueEditCancelLabel),
					Class("press min-h-[44px] rounded-lg px-3 text-sm font-semibold text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
					g.Text("Cancel")),
				Button(Type("submit"), g.Attr("data-queue-edit-save", ""),
					Class("press min-h-[44px] rounded-lg bg-accent px-4 text-sm font-semibold text-on-accent transition disabled:opacity-40"),
					g.Text(SendQueueEditSaveLabel)),
			),
		),
	)
}

// sendQueueMenuItemClass is an item of a queued message's ⋯ menu: a full-width
// 44px row, like the caret menu's Queue.
const sendQueueMenuItemClass = "press flex min-h-[44px] w-full items-center rounded-lg px-3 text-left text-sm font-semibold transition hover:bg-s2 focus:bg-s2 focus:outline-none disabled:opacity-40"

// sendQueueRowMenu is a QUEUED message's ⋯ menu: Send now, Edit (a person's
// own message only: never a routed one), Copy message and Cancel.
//
// IT WORKS WITHOUT SCRIPT, like the caret's menu: it is a POPOVER the ⋯ button
// opens itself (popovertarget), which closes on Escape and on a tap outside
// with no script at all, and Send now and Cancel are each a real form. Copy
// message needs the clipboard, so it is drawn hidden and the list's script
// shows it; the whole message rides in the row (a template), because the
// preview is cut. sendQueueScript adds the menu-button keyboard pattern,
// places the menu beside its button and keeps aria-expanded true.
//
// 🔴 OUT OF FLOW: the menu is in the top layer, so opening it moves nothing on
// the page (no layout shift), and the list's poll does not redraw the rows
// while one is open.
func sendQueueRowMenu(it SendQueueItem) g.Node {
	id := strconv.FormatInt(it.ID, 10)
	menu := "mv-qm-" + id
	return Div(
		g.Attr("data-queue-menu", ""),
		Class("shrink-0"),
		Button(
			Type("button"),
			ID(menu+"-btn"),
			g.Attr("popovertarget", menu),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", menu),
			g.Attr("aria-label", SendQueueMenuLabel),
			g.Attr("title", SendQueueMenuLabel),
			Class("press inline-flex min-h-[44px] min-w-[44px] items-center justify-center rounded-lg text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><circle cx="4" cy="10" r="1.75"/><circle cx="10" cy="10" r="1.75"/><circle cx="16" cy="10" r="1.75"/></svg>`),
		),
		Div(
			ID(menu),
			g.Attr("popover", ""),
			g.Attr("role", "menu"),
			g.Attr("aria-label", "Queued message"),
			Class("m-0 min-w-[12rem] rounded-xl border-0 bg-s1 p-1 text-fg shadow-lg ring-1 ring-line"),
			Form(Method("post"), Action(SendQueueSendNowPath(it.ID)), g.Attr("data-send-now", ""),
				Button(Type("submit"), g.Attr("role", "menuitem"), g.Attr("title", "Send it now, without waiting for Claude Code to be idle"),
					Class(sendQueueMenuItemClass+" text-fg"), g.Text(SendQueueSendNowLabel))),
			// Edit HOLDS the message first (the server), and only then is its text
			// drawn for editing: a form, so it works without script.
			g.If(it.Editable, Form(Method("post"), Action(SendQueueEditPath(it.ID)), g.Attr("data-edit", ""),
				Button(Type("submit"), g.Attr("role", "menuitem"),
					g.Attr("title", "Change its text; it is held (not sent) while you edit, and keeps its place"),
					Class(sendQueueMenuItemClass+" text-fg"), g.Text(SendQueueEditLabel)))),
			Button(Type("button"), g.Attr("role", "menuitem"), g.Attr("data-queue-copy", ""), g.Attr("hidden", ""),
				Class(sendQueueMenuItemClass+" text-fg"), g.Text(SendQueueCopyLabel)),
			Form(Method("post"), Action(SendQueueCancelPath(it.ID)), g.Attr("data-cancel", ""),
				Button(Type("submit"), g.Attr("role", "menuitem"), g.Attr("aria-label", SendQueueCancelLabel),
					Class(sendQueueMenuItemClass+" text-st-error-fg"), g.Text("Cancel"))),
		),
		// The whole message, for Copy message: inert, never rendered.
		Template(g.Attr("data-queue-body", ""), g.Text(it.Body)),
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
// dispatches mv-queued) and when the page is shown again — never while a row's
// menu is open; it sends each Send now, Cancel, Retry and Delete without
// leaving the page; it copies a queued message; and it drives the rows' ⋯
// menus (sendQueueRowMenu).
func sendQueueScript() g.Node {
	return Script(g.Raw(`
(function () {
  var box = document.getElementById('mv-queue');
  if (!box) return;
  var src = box.getAttribute('data-src');
  var timer = null, loading = false;
  // gen moves when the page removes a row itself (Delete). A load that was
  // already in flight then read the list BEFORE the removal, so its answer is
  // dropped and the list read again: drawn, it would put the deleted row back.
  var gen = 0;
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(load, box.querySelector('[data-active]') ? 3000 : 15000);
  }
  // menuOpen: a row's ⋯ menu is open. The list is not redrawn under it (a
  // redraw would close it, and drop the focus inside it); the next load after
  // it closes catches up.
  function menuOpen() {
    var ms = box.querySelectorAll('[data-queue-menu] [popover], #mv-queue-waiting-full');
    for (var i = 0; i < ms.length; i++) {
      try { if (ms[i].matches(':popover-open')) return true; } catch (e) { return false; }
    }
    return false;
  }
  // copyable: Copy message is shown only where the page can write the
  // clipboard (it is drawn hidden: without script there is no Copy).
  function copyable() {
    var cs = box.querySelectorAll('[data-queue-copy]');
    for (var i = 0; i < cs.length; i++) cs[i].hidden = false;
  }
  // clocks draws each <time data-clock> (a held queue's "resumes 10:30 PM")
  // in the reader's own local time; the server's text stays if it cannot.
  // A time that is not today carries its date ("Oct 27, 10:00 PM": a weekly
  // limit's reset is days away). The line's title follows what it now says.
  function clocks() {
    var ts = box.querySelectorAll('time[data-clock]');
    for (var i = 0; i < ts.length; i++) {
      var ms = parseInt(ts[i].getAttribute('data-clock'), 10);
      try {
        if (!ms) continue;
        var d = new Date(ms);
        ts[i].textContent = d.toDateString() === new Date().toDateString()
          ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
          : d.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
        var p = ts[i].closest('[title]');
        if (p) p.setAttribute('title', p.textContent);
      } catch (e) {}
    }
  }
  // drawn is what follows every draw of the list.
  function drawn() { copyable(); clocks(); }
  // editing: a row is open for editing (sendQueueEditRow). The list is not
  // redrawn under it either — that would throw away what is being typed; the
  // edit's own Save, Cancel or running out redraws it (show).
  function editing() { return box.querySelector('[data-queue-edit-form]'); }
  function load() {
    if (typeof fetch !== 'function') return;
    if (loading) return;
    if (document.hidden || menuOpen() || editing()) { schedule(); return; }
    loading = true;
    var g = gen;
    fetch(src, { credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (t) {
        loading = false;
        if (g !== gen) { load(); return; }
        if (t !== null && !menuOpen() && !editing()) { box.innerHTML = t; drawn(); }
        schedule();
      }, function () { loading = false; schedule(); });
  }
  // show re-reads the list and draws it whatever is open — with row id open
  // for editing when id is given (the server draws it open only while it is
  // held) — then arms the open edit's hold timer.
  var holdTimer = null;
  // hold is the hold THIS page's Edit was given: the server draws the form
  // only under it.
  function show(id, hold) {
    if (typeof fetch !== 'function') return Promise.resolve();
    var u = src + (id ? '&` + SendQueueEditParam + `=' + encodeURIComponent(id) + '&` + SendQueueHoldParam + `=' + encodeURIComponent(hold || '') : '');
    return fetch(u, { credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (t) {
        if (t === null) return;
        box.innerHTML = t;
        drawn();
        armHold();
        var ta = id ? box.querySelector('[data-queue-edit-form] textarea') : null;
        if (ta) { ta.focus(); try { ta.setSelectionRange(ta.value.length, ta.value.length); } catch (e) {} }
        schedule();
      }, function () { schedule(); });
  }
  // keepText puts an edit that could not be saved into the message box, when
  // the box is empty, so nothing typed is thrown away; it says where it went.
  function keepText(f) {
    var ta = f && f.querySelector('textarea');
    var input = document.getElementById('mv-input');
    if (!ta || !input || input.value.trim() || !ta.value.trim()) return '';
    input.value = ta.value;
    try { input.dispatchEvent(new Event('input', { bubbles: true })); } catch (e) {}
    return ' Your edited text is in the message box.';
  }
  // armHold counts the open edit's hold down (data-hold-left-ms, the server's
  // reading): when it runs out the server has already let the message go back
  // to the queue with its original text, so the row says so and is redrawn.
  function armHold() {
    clearTimeout(holdTimer);
    var f = editing();
    if (!f) return;
    var left = parseInt(f.getAttribute('data-hold-left-ms') || '0', 10);
    holdTimer = setTimeout(function () {
      var open = editing();
      if (!open) return;
      note(` + strconv.Quote(SendQueueLapsedText) + ` + keepText(open));
      show();
    }, Math.max(0, left) + 250);
  }
  function note(t) {
    var n = document.getElementById('mv-note');
    if (n && t) { n.textContent = t; n.hidden = false; n.removeAttribute('data-ok'); }
  }
  function noteOK(t) {
    var n = document.getElementById('mv-note');
    if (n && t) { n.textContent = t; n.hidden = false; n.setAttribute('data-ok', ''); }
  }

  // THE ROW MENUS (sendQueueRowMenu): the popover opens, light-dismisses and
  // closes on Escape by itself; this adds the menu-button pattern. Delegated,
  // because the rows are redrawn: toggle does not bubble, so it is taken in
  // the capture phase.
  function menuOf(btn) { return btn && document.getElementById(btn.getAttribute('aria-controls')); }
  function items(menu) {
    return Array.prototype.slice.call(menu.querySelectorAll('[role=menuitem]')).filter(function (b) { return !b.hidden; });
  }
  function focusItem(menu, i) {
    var it = items(menu); if (!it.length) return;
    it[(i + it.length) % it.length].focus();
  }
  // place puts the menu under its button, right-aligned to it — or above it
  // when there is no room below. Done BEFORE it is shown (beforetoggle): a
  // popover is painted in the middle of the screen first otherwise.
  function place(menu) {
    var btn = document.getElementById(menu.id + '-btn');
    if (!btn) return;
    var r = btn.getBoundingClientRect();
    var vv = window.visualViewport;
    var bottom = vv ? vv.height + vv.offsetTop : window.innerHeight;
    menu.style.position = 'fixed';
    menu.style.inset = 'auto';
    menu.style.margin = '0';
    menu.style.right = Math.max(8, Math.round(document.documentElement.clientWidth - r.right)) + 'px';
    var h = menu.querySelectorAll('[role=menuitem]').length * 44 + 16;
    if (r.bottom + 6 + h <= bottom) menu.style.top = Math.round(r.bottom + 6) + 'px';
    else menu.style.bottom = Math.max(8, Math.round(window.innerHeight - r.top + 6)) + 'px';
  }
  var focusFirst = false;
  box.addEventListener('beforetoggle', function (e) {
    var m = e.target;
    if (m && m.hasAttribute && m.hasAttribute('popover') && e.newState === 'open') { copyable(); place(m); }
  }, true);
  box.addEventListener('toggle', function (e) {
    var m = e.target;
    if (!m || !m.hasAttribute || !m.hasAttribute('popover')) return;
    var open = e.newState ? e.newState === 'open' : m.matches(':popover-open');
    var btn = document.getElementById(m.id + '-btn');
    if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open && focusFirst) focusItem(m, 0);
    focusFirst = false;
    if (!open) schedule();
  }, true);
  box.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest('[aria-haspopup=menu]') : null;
    if (b) focusFirst = true;
    var w = e.target && e.target.closest ? e.target.closest('#mv-queue-waiting') : null;
    if (w) showWaiting(w);
  });
  // showWaiting opens the waiting line, whole, in its popover.
  function showWaiting(w) {
    var full = document.getElementById('mv-queue-waiting-full');
    if (!full || typeof full.showPopover !== 'function') return;
    full.textContent = w.textContent;
    try { full.showPopover(); } catch (e) {}
  }
  box.addEventListener('keydown', function (e) {
    if ((e.key === 'Enter' || e.key === ' ') && e.target && e.target.id === 'mv-queue-waiting') {
      e.preventDefault();
      showWaiting(e.target);
    }
  });
  box.addEventListener('keydown', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var btn = t.closest('[aria-haspopup=menu]');
    if (btn && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      var m0 = menuOf(btn);
      if (!m0 || typeof m0.showPopover !== 'function') return;
      e.preventDefault();
      focusFirst = true;
      if (!m0.matches(':popover-open')) m0.showPopover(); else focusItem(m0, e.key === 'ArrowUp' ? -1 : 0);
      return;
    }
    var m = t.closest('[data-queue-menu] [popover]');
    if (!m) return;
    var it = items(m), i = it.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); focusItem(m, i + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); focusItem(m, i - 1); }
    else if (e.key === 'Home') { e.preventDefault(); focusItem(m, 0); }
    else if (e.key === 'End') { e.preventDefault(); focusItem(m, -1); }
    else if (e.key === 'Escape') {
      e.preventDefault();
      try { m.hidePopover(); } catch (err) {}
      var ob = document.getElementById(m.id + '-btn');
      if (ob) ob.focus();
    }
    else if (e.key === 'Tab') { try { m.hidePopover(); } catch (err) {} }
  });
  function closeMenu(el) {
    var m = el && el.closest ? el.closest('[data-queue-menu] [popover]') : null;
    if (m) { try { m.hidePopover(); } catch (err) {} }
  }

  // Copy message: the WHOLE message (the row's template; the preview is cut),
  // to the clipboard, then a brief "Copied." Where the async clipboard is
  // refused (no secure context), a selected textarea and execCommand.
  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (ok, fail) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed'; ta.style.top = '0'; ta.style.left = '0'; ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      var done = false;
      try { done = document.execCommand('copy'); } catch (err) {}
      document.body.removeChild(ta);
      if (done) ok(); else fail(new Error('copy refused'));
    });
  }
  box.addEventListener('click', function (e) {
    var c = e.target && e.target.closest ? e.target.closest('[data-queue-copy]') : null;
    if (!c) return;
    var row = c.closest('[data-queue-id]');
    var tpl = row ? row.querySelector('template[data-queue-body]') : null;
    var text = tpl ? (tpl.content ? tpl.content.textContent : tpl.textContent) : '';
    closeMenu(c);
    copyText(text).then(function () { noteOK('Copied.'); }, function () { note('Not copied: this browser did not allow it.'); });
  });

  // Send now: sends a queued message at once, under this session's sign-in; a
  // stale one asks "Confirm it's you" and sends.
  function sendNow(f) {
    var b = f.querySelector('button');
    if (b) b.disabled = true;
    return fetch(f.getAttribute('action'), { method: 'POST', credentials: 'same-origin',
      headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (r.ok) { noteOK('Sent.'); return; }
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && window.musterStepUp) {
          window.musterStepUp.stale();
          window.musterStepUp.ask('', function () { return sendNow(f); });
          return;
        }
        if (b) b.disabled = false;
        return r.text().then(function (t) { note(t.trim() || ('Not sent (HTTP ' + r.status + ')')); });
      })
      .then(load, load);
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
  // Edit: HOLD the message (the server), then draw it open. A stale sign-in
  // asks "Confirm it's you" and edits.
  function edit(f) {
    var b = f.querySelector('button');
    if (b) b.disabled = true;
    var id = (f.closest('[data-queue-id]') || {}).getAttribute ? f.closest('[data-queue-id]').getAttribute('data-queue-id') : '';
    return fetch(f.getAttribute('action'), { method: 'POST', credentials: 'same-origin',
      headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        // The answer is the hold this Edit was given.
        if (r.ok) return r.text().then(function (hold) { return show(id, hold.trim()); });
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && window.musterStepUp) {
          window.musterStepUp.stale();
          window.musterStepUp.ask('', function () { return edit(f); });
          return;
        }
        if (b) b.disabled = false;
        return r.text().then(function (t) { note(t.trim() || ('Not editable (HTTP ' + r.status + ')')); show(); });
      }, function () { if (b) b.disabled = false; });
  }
  // Save or Cancel of an open edit. Save needs a fresh sign-in, like a queue
  // post; a hold that ran out is refused, and says so.
  var editSub = null;
  function finishEdit(f, cancel) {
    var bs = f.querySelectorAll('button');
    for (var i = 0; i < bs.length; i++) bs[i].disabled = true;
    var body = new URLSearchParams();
    body.append('hold', (f.querySelector('input[name=hold]') || {}).value || '');
    if (!cancel) body.append('text', (f.querySelector('textarea') || {}).value || '');
    var to = cancel ? f.querySelector('[data-queue-edit-cancel]').getAttribute('formaction') : f.getAttribute('action');
    return fetch(to, { method: 'POST', body: body, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (r.ok) {
          clearTimeout(holdTimer);
          // 200 with words: saved, with a warning (an answer it already gave).
          if (r.status === 200) return r.text().then(function (t) { if (t.trim()) note(t.trim()); else noteOK(` + strconv.Quote(SendQueueSavedNote) + `); return show(); });
          noteOK(cancel ? ` + strconv.Quote(SendQueueEditCancelledNote) + ` : ` + strconv.Quote(SendQueueSavedNote) + `);
          return show();
        }
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && window.musterStepUp) {
          for (var j = 0; j < bs.length; j++) bs[j].disabled = false;
          window.musterStepUp.stale();
          window.musterStepUp.ask('', function () { return finishEdit(f, cancel); });
          return;
        }
        return r.text().then(function (t) {
          if (r.status === 400) {
            // Something the person can fix (an empty text): the edit stays open.
            for (var k = 0; k < bs.length; k++) bs[k].disabled = false;
            note(t.trim());
            return;
          }
          clearTimeout(holdTimer);
          note((t.trim() || ('Not saved (HTTP ' + r.status + ')')) + keepText(f));
          return show();
        });
      }, function () { for (var j = 0; j < bs.length; j++) bs[j].disabled = false; note('Not saved: you are offline.'); });
  }
  box.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest('[data-queue-edit-form] button') : null;
    if (b) editSub = b;
  });
  box.addEventListener('submit', function (e) {
    var f = e.target;
    // An item of a row's menu: the menu has done its job.
    closeMenu(f);
    if (f && f.hasAttribute && f.hasAttribute('data-edit') && typeof fetch === 'function') {
      e.preventDefault();
      edit(f);
      return;
    }
    if (f && f.hasAttribute && f.hasAttribute('data-queue-edit-form') && typeof fetch === 'function' && typeof URLSearchParams === 'function') {
      e.preventDefault();
      var sub = ('submitter' in e && e.submitter) ? e.submitter : editSub;
      editSub = null;
      finishEdit(f, !!(sub && sub.hasAttribute('data-queue-edit-cancel')));
      return;
    }
    if (f && f.hasAttribute && f.hasAttribute('data-send-now') && typeof fetch === 'function') {
      e.preventDefault();
      sendNow(f);
      return;
    }
    if (f && f.hasAttribute && f.hasAttribute('data-retry') && typeof fetch === 'function' && typeof URLSearchParams === 'function') {
      e.preventDefault();
      var ask = f.getAttribute('data-confirm');
      if (ask && !window.confirm(ask)) return;
      retry(f, !!ask);
      return;
    }
    // Delete: a finished message leaves the list at once, without leaving the
    // page; the list is then re-read, so every device's copy agrees.
    if (f && f.hasAttribute && f.hasAttribute('data-delete') && typeof fetch === 'function') {
      e.preventDefault();
      var db = f.querySelector('button');
      if (db) db.disabled = true;
      fetch(f.getAttribute('action'), { method: 'POST', credentials: 'same-origin',
        headers: { 'X-Requested-With': 'XMLHttpRequest' } })
        .then(function (r) {
          if (r.ok || r.status === 404) {
            // 404: already gone (deleted from another device).
            var li = f.closest ? f.closest('[data-queue-id]') : null;
            if (li && li.parentNode) li.parentNode.removeChild(li);
            gen++;
            return;
          }
          if (db) db.disabled = false;
          return r.text().then(function (t) { note(t.trim() || ('Not deleted (HTTP ' + r.status + ')')); });
        })
        .then(load, load);
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
  drawn();
  // A page drawn with a row open for editing (a no-script Edit came back here,
  // and script runs after all): count its hold down too.
  armHold();
  schedule();
})();
`))
}
