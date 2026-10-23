package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/askq"
)

// THE QUESTION CARDS — Claude Code's AskUserQuestion in the rendered view.
//
// A question is on screen IN BOTH PLACES AT ONCE: the terminal's own picker,
// and here. Whichever is answered first wins; the other side shows it.
//
// TWO PLACES ON THIS PAGE DRAW A QUESTION, BECAUSE THE TRANSCRIPT CANNOT SHOW AN
// OPEN ONE. Measured (CLI 2.1.289): while the picker is on screen the call's
// tool_use record is not in the transcript yet — it is written with the
// answer. So:
//
//   - THE LIVE REGION (#mv-asks, above the composer) draws what the SERVER
//     knows (the window's `muster-mirror ask` hook registered it): every open
//     question as a form — one fieldset per question (header and text), radio
//     buttons for a single-select question and checkboxes for a multi-select
//     one (each option with its description), an "Other" choice with a text box
//     for the person's own answer, and ONE Submit for every question. A submit
//     is TYPED into the terminal's picker by the host; the card then shows
//     "typing", and how it ended once the CLI recorded it — answered here,
//     answered in the terminal (with the answers), dismissed, or that it
//     could not be typed in. The page polls it (MirrorAsksPath); a card stays a
//     few minutes after it ends, until the conversation draws the call itself.
//   - THE CONVERSATION draws the call from the transcript once it is there:
//     read-only, with the answers its result records (the toolUseResult) —
//     however it was answered.
//
// 🔴 THE PAGE NEVER DECIDES ANYTHING. Which state a card is in is the server's
// answer to the poll; a submit is refused server-side unless the question is
// still open, the host is -interactive and the sign-in is fresh, exactly like
// a Send.

// MirrorAskPath takes a card's answers (POST); MirrorAsksPath is the window's
// live region (GET).
const (
	MirrorAskPath  = "/mirror/view/ask"
	MirrorAsksPath = "/mirror/view/asks"
	// MirrorAskCancelPath dismisses a card's picker in the terminal with one
	// Escape (POST).
	MirrorAskCancelPath = "/mirror/view/ask/cancel"
)

// The states a card can be in, as data-ask-state carries them.
// AskStateHistory and AskStateWaiting are cards drawn from the transcript.
const (
	AskStatePending          = "pending"
	AskStateTyping           = "typing"
	AskStateFailed           = "failed"
	AskStateAnswered         = "answered"
	AskStateAnsweredTerminal = "answered-terminal"
	AskStateDeclined         = "declined"
	AskStateExpired          = "expired"
	AskStateSuperseded       = "superseded"
	AskStateHistory          = "history"
	AskStateWaiting          = "waiting"
)

// MirrorAsksSrc is the live region's URL for the window ref.
func MirrorAsksSrc(ref MirrorRef) string { return MirrorAsksPath + "?" + ref.query() }

// MirrorAskRowID is the element id the conversation gives the call toolID —
// a tool row's (toolRowID) — which the live region reads to know the
// conversation has drawn it.
func MirrorAskRowID(toolID string) string { return toolRowID(toolID) }

// askCardClass is the card's frame: the tool row's, a little wider.
const askCardClass = "w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-sm ring-1 ring-inset ring-line"

// askCardHead is the card's title line.
func askCardHead(label string) g.Node {
	return Div(Class("flex min-h-[44px] items-center gap-2 px-3 py-2 text-xs font-medium text-fg2"),
		Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("❓")),
		Span(Class("shrink-0 font-mono text-st-review-fg"), g.Text(askq.ToolName)),
		Span(Class("ml-auto shrink-0 text-muted"), g.Text(label)))
}

// askQuestionHead is one question's header and text.
func askQuestionHead(q askq.Question) g.Node {
	return g.Group{
		g.If(q.Header != "", Span(Class("text-[10px] uppercase tracking-wide text-muted"), g.Text(q.Header))),
		Span(Class("break-words font-medium text-fg"), g.Text(q.Question)),
	}
}

// askReadOnly draws qs with what was answered (answers, keyed by question
// text; nil for none yet): each question, its options, and the answer.
func askReadOnly(qs []askq.Question, answers map[string]string) g.Node {
	items := make([]g.Node, 0, len(qs))
	for _, q := range qs {
		opts := make([]g.Node, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, Li(Class("break-words text-fg2"), g.Text(o.Label),
				g.If(o.Description != "", Span(Class("text-muted"), g.Text(" — "+o.Description)))))
		}
		a, answered := answers[q.Question]
		items = append(items, Div(g.Attr("data-ask-question", ""), Class("flex flex-col gap-1"),
			askQuestionHead(q),
			g.If(q.MultiSelect, P(Class("text-[11px] text-muted"), g.Text("Choose any number"))),
			Ul(Class("list-disc pl-5 text-xs"), g.Group(opts)),
			g.If(answered, P(g.Attr("data-ask-answer", ""), Class("break-words text-xs text-fg"),
				Span(Class("font-semibold text-accent"), g.Text("Answer: ")), g.Text(a))),
		))
	}
	return Div(Class("flex flex-col gap-3"), g.Group(items))
}

// MirrorAskCall is a call the transcript holds with no result yet: its
// questions, read-only. Claude Code 2.1.289 writes the call together with its
// result (measured: neither is in the transcript while the picker shows), so
// this is drawn only for a transcript cut between the two.
func MirrorAskCall(rowID string, qs []askq.Question) g.Node {
	return Section(
		g.If(rowID != "", ID(rowID)),
		g.Attr("data-ask", ""),
		g.Attr("data-ask-state", AskStateWaiting),
		g.Attr("aria-label", "Claude is asking"),
		Class(askCardClass),
		askCardHead("asking…"),
		Div(Class("flex flex-col gap-3 px-3 pb-3"),
			askReadOnly(qs, nil),
			P(Class("text-xs text-muted"), g.Text("Waiting for the answer in the terminal.")),
		),
	)
}

// MirrorAskResult is a call drawn from its result: read-only, with the answers
// it was given (none, when the result carries none — the picker was dismissed —
// and note says what the result said instead).
func MirrorAskResult(rowID string, qs []askq.Question, answers map[string]string, note string) g.Node {
	label := "answered"
	if len(answers) == 0 {
		label = "not answered"
	}
	return Section(
		g.If(rowID != "", ID(rowID)),
		g.Attr("data-ask", ""),
		g.Attr("data-ask-state", AskStateHistory),
		g.Attr("aria-label", "Claude asked"),
		Class(askCardClass),
		askCardHead(label),
		Div(Class("flex flex-col gap-3 px-3 pb-3"),
			askReadOnly(qs, answers),
			g.If(note != "", P(Class("break-words text-xs text-muted"), g.Text(note))),
		),
	)
}

// MirrorAskLiveView is one question a window's session is asking (or asked
// lately), as the server knows it.
type MirrorAskLiveView struct {
	Ref    MirrorRef
	ToolID string
	// State is one of the AskState* of a live card.
	State     string
	Questions []askq.Question
	// Answers is the answers recorded (answered) or being typed (typing).
	Answers map[string]string
	// Note is why typing failed (failed).
	Note string
	// Unanswerable, when set, is why this question cannot be answered from
	// here (a read-only or older host): the card says it in place of a form.
	Unanswerable string
	// Error is a refused submit's reason, drawn above the form.
	Error string
	// Retry (failed) says the answer can be tried again from here: with
	// Chosen, that answer is resent as it is (a fixed retry: the picker holds
	// part of it, and muster continues it); without, the form is drawn again.
	Retry bool
	// Chosen is the answer muster typed part of (failed), one choice per
	// question, as the picker takes them.
	Chosen []askq.Choice
	// CanCancel says the card offers "Cancel question" (pending, failed): the
	// host is online, -interactive, and advertised it can dismiss a picker.
	// WindowName is the window as the confirm dialog names it.
	CanCancel  bool
	WindowName string
	// Sent is a dismissal's note, on the card the dismissal answers with
	// (not kept: the next poll's card is the server's, and the CLI's record
	// of the call ending flips it): the Esc was sent. SentShowing says the
	// picker was still on the terminal's screen after it: the card keeps its
	// form and Cancel under the note; otherwise the picker went, and the
	// card shows the question read-only. Either is data-ask-sent on the card,
	// which the page's merge compares with the state, so the next poll's
	// card replaces it.
	Sent        string
	SentShowing bool
}

// askTerminalLink is the card's link to the window's terminal.
func askTerminalLink(v MirrorAskLiveView) g.Node {
	return A(Href(v.Ref.TerminalPath()), hx("hx-boost", "false"), Class("font-semibold text-accent underline"), g.Text("Open the terminal"))
}

// MirrorAskLive is one card of the live region.
func MirrorAskLive(v MirrorAskLiveView) g.Node {
	label := "Claude is asking"
	status := func(cls, text string, link bool) g.Node {
		return P(g.Attr("role", "status"), Class("text-xs "+cls), g.Text(text), g.If(link, g.Group{g.Text(" "), askTerminalLink(v)}))
	}
	var body g.Node
	switch v.State {
	case AskStatePending:
		if v.Unanswerable != "" {
			body = g.Group{askReadOnly(v.Questions, nil), status("text-muted", v.Unanswerable+" The terminal shows this question too.", true)}
		} else {
			body = askForm(v)
		}
	case AskStateTyping:
		label = "typing it in…"
		body = g.Group{askReadOnly(v.Questions, v.Answers), status("text-fg2", "Typing your answer into the terminal…", false)}
	case AskStateFailed:
		label = "answer in the terminal"
		body = g.Group{askReadOnly(v.Questions, nil), status("text-muted", v.Note, true)}
		switch {
		case v.Retry && v.Unanswerable == "" && v.Chosen != nil:
			// The picker holds part of THIS answer: Try again resends it as
			// it is — never another, which the picker would not take.
			label = "not typed in yet"
			body = g.Group{askReadOnly(v.Questions, askq.AnswersOf(v.Questions, v.Chosen)), status("text-muted", v.Note, true), askRetryForm(v)}
		case v.Retry && v.Unanswerable == "":
			label = "not typed in yet"
			body = g.Group{status("text-muted", v.Note, true), askForm(v)}
		}
	case AskStateAnswered:
		label = "answered here"
		body = g.Group{askReadOnly(v.Questions, v.Answers), status("text-accent", "Answered here — Claude has it.", false)}
	case AskStateAnsweredTerminal:
		label = "answered in the terminal"
		body = g.Group{askReadOnly(v.Questions, v.Answers), status("text-muted", "Answered in the terminal.", false)}
	case AskStateDeclined:
		label = "dismissed"
		body = g.Group{askReadOnly(v.Questions, nil), status("text-muted", "Dismissed in the terminal — not answered.", false)}
	case AskStateSuperseded:
		label = "replaced"
		body = g.Group{askReadOnly(v.Questions, nil), status("text-muted", "Claude asked a newer question in this session, so this one is no longer on the terminal's screen.", false)}
	default:
		label = "no answer seen"
		why := "No answer was seen for a long time; if it is still on the terminal's screen, answer it there."
		if v.Note != "" {
			why = v.Note
		}
		body = g.Group{askReadOnly(v.Questions, nil), status("text-muted", why, true)}
	}
	// An open question the terminal still shows can be dismissed from here.
	sent := ""
	if v.Sent != "" {
		note := P(g.Attr("role", "status"), g.Attr("data-ask-sent-note", ""), Class("text-xs text-fg2"), g.Text(v.Sent))
		if v.SentShowing {
			sent = "showing"
			body = g.Group{note, body}
		} else {
			sent = "gone"
			body = g.Group{askReadOnly(v.Questions, nil), note}
		}
	}
	if v.CanCancel && sent != "gone" && (v.State == AskStatePending || v.State == AskStateFailed) {
		body = g.Group{body, askCancelControl(v)}
	}
	return Section(
		g.Attr("data-ask-live", ""),
		g.Attr("data-ask-tool", v.ToolID),
		g.Attr("data-ask-row", MirrorAskRowID(v.ToolID)),
		g.Attr("data-ask-state", v.State),
		g.Attr("data-ask-sent", sent),
		g.Attr("aria-label", "Claude is asking"),
		Class(askCardClass),
		askCardHead(label),
		Div(Class("flex flex-col gap-3 px-3 pb-3"), body),
	)
}

// MirrorAsksLive is the live region's content: one card per view.
func MirrorAsksLive(vs []MirrorAskLiveView) g.Node {
	out := make([]g.Node, 0, len(vs))
	for _, v := range vs {
		out = append(out, MirrorAskLive(v))
	}
	return g.Group(out)
}

// mirrorAsksRegion is the page's live region: empty and hidden until the poll
// (askScript) fills it.
//
// 🔴 IT SHRINKS INTO THE ROOM ABOVE THE COMPOSER (min-h-0, never shrink-0). The
// composer is pinned to the bottom of the screen and the column ends above it
// (pinnedShellClass), so the room this region has is what the composer leaves —
// and a queue drawn in the composer makes that much less. A region that
// refused to shrink kept its full height and ran on under the pinned composer,
// drawing the question's Submit under the queued messages, where it could not
// be tapped. Shrunk, it scrolls within itself, and its Submit is in reach.
func mirrorAsksRegion(ref MirrorRef) g.Node {
	return Section(
		ID("mv-asks"),
		g.Attr("data-src", MirrorAsksSrc(ref)),
		g.Attr("aria-label", "Questions from Claude"),
		g.Attr("aria-live", "polite"),
		g.Attr("hidden", ""),
		Class("flex max-h-[70vh] min-h-0 flex-col gap-2 overflow-y-auto border-t border-line px-3 py-2"),
	)
}

// askForm is a pending question set's form: one fieldset per question and
// one Submit.
func askForm(v MirrorAskLiveView) g.Node {
	base := "ask" + MirrorAskRowID(v.ToolID)[2:]
	sets := make([]g.Node, 0, len(v.Questions))
	for i, q := range v.Questions {
		idx := strconv.Itoa(i)
		kind := "radio"
		if q.MultiSelect {
			kind = "checkbox"
		}
		name := "a" + idx
		opts := make([]g.Node, 0, len(q.Options)+1)
		for j, o := range q.Options {
			id := base + "-" + idx + "-" + strconv.Itoa(j)
			opts = append(opts, Label(For(id), Class("flex min-h-[44px] cursor-pointer items-start gap-2 rounded-lg px-2 py-2 hover:bg-s2"),
				Input(Type(kind), ID(id), Name(name), Value(o.Label),
					Class("mt-0.5 h-5 w-5 shrink-0 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing)),
				Span(Class("flex min-w-0 flex-col"),
					Span(Class("break-words text-fg"), g.Text(o.Label)),
					g.If(o.Description != "", Span(Class("break-words text-xs text-muted"), g.Text(o.Description))),
					g.If(o.Preview != "", Pre(g.Attr("data-ask-preview", ""),
						Class("mt-1 whitespace-pre-wrap break-words rounded-lg bg-s2 px-2 py-1 font-mono text-[11px] text-fg2"), g.Text(o.Preview))))))
		}
		// A question the terminal draws with previews has no row for an
		// answer of one's own (askq.Question.Previews): no "Other" here either.
		if !q.Previews() {
			otherID := base + "-" + idx + "-other"
			opts = append(opts, Div(Class("flex flex-col gap-1 rounded-lg px-2 py-2"),
				Label(For(otherID), Class("flex min-h-[44px] cursor-pointer items-center gap-2"),
					Input(Type(kind), ID(otherID), Name(name), Value(askq.OtherValue),
						Class("h-5 w-5 shrink-0 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing)),
					Span(Class("text-fg"), g.Text("Other"))),
				Textarea(Name("o"+idx), Rows("2"), MaxLength(strconv.Itoa(askq.MaxFreeText)),
					g.Attr("data-ask-other-text", ""), g.Attr("data-ask-other-for", otherID),
					g.Attr("aria-label", "Your own answer: "+q.Question),
					g.Attr("placeholder", "Type your own answer"),
					Class("w-full min-w-0 resize-y rounded-lg bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line"+focusRing)),
			))
		}
		sets = append(sets, FieldSet(g.Attr("data-ask-question", ""), Class("flex min-w-0 flex-col gap-1"),
			Legend(Class("flex flex-col gap-1"), askQuestionHead(q)),
			g.If(q.MultiSelect, P(Class("text-[11px] text-muted"), g.Text("Choose any number"))),
			g.Group(opts)))
	}
	hidden := func(n, val string) g.Node { return Input(Type("hidden"), Name(n), Value(val)) }
	submit := "Submit answers"
	if v.Retry {
		submit = "Try again"
	}
	return Form(
		g.Attr("data-ask-form", ""),
		Method("post"),
		Action(MirrorAskPath),
		Class("flex flex-col gap-4"),
		hidden("host", v.Ref.Host), hidden("session", v.Ref.Session),
		g.If(v.Ref.Window != "", hidden("window", v.Ref.Window)),
		hidden("tool", v.ToolID),
		P(g.Attr("data-ask-error", ""), g.Attr("role", "alert"), g.If(v.Error == "", g.Attr("hidden", "")),
			Class("break-words rounded-lg bg-danger/15 px-3 py-2 text-xs text-fg"), g.Text(v.Error)),
		g.Group(sets),
		Div(Class("flex flex-wrap items-center gap-2"),
			Button(Type("submit"), g.Attr("data-ask-submit", ""), Class(composerSendClass), g.Text(submit)),
			Span(Class("text-xs text-muted"), g.Text("The terminal shows this question too — whichever is answered first counts."))),
	)
}

// askRetryForm is a fixed retry: Chosen, as hidden fields, and "Try again" —
// the same answer, posted as a form would post it (askq.FromForm).
func askRetryForm(v MirrorAskLiveView) g.Node {
	hidden := func(n, val string) g.Node { return Input(Type("hidden"), Name(n), Value(val)) }
	var fields []g.Node
	for i, q := range v.Questions {
		if i >= len(v.Chosen) {
			break
		}
		idx := strconv.Itoa(i)
		for _, p := range v.Chosen[i].Picks {
			if p >= 0 && p < len(q.Options) {
				fields = append(fields, hidden("a"+idx, q.Options[p].Label))
			}
		}
		if v.Chosen[i].Other != "" {
			fields = append(fields, hidden("a"+idx, askq.OtherValue), hidden("o"+idx, v.Chosen[i].Other))
		}
	}
	return Form(g.Attr("data-ask-form", ""), g.Attr("data-ask-retry", ""), Method("post"), Action(MirrorAskPath), Class("flex flex-col gap-2"),
		hidden("host", v.Ref.Host), hidden("session", v.Ref.Session),
		g.If(v.Ref.Window != "", hidden("window", v.Ref.Window)),
		hidden("tool", v.ToolID), g.Group(fields),
		P(g.Attr("data-ask-error", ""), g.Attr("role", "alert"), g.Attr("hidden", ""),
			Class("break-words rounded-lg bg-danger/15 px-3 py-2 text-xs text-fg")),
		Div(Class("flex flex-wrap items-center gap-2"),
			Button(Type("submit"), g.Attr("data-ask-submit", ""), Class(composerSendClass), g.Text("Try again"))))
}

// askCancelControl is a card's "Cancel question": it opens a dialog that
// names the window, and only the dialog's "Send Esc" asks muster to dismiss
// the picker (MirrorAskCancelPath). A DOM dialog, never window.confirm() —
// native dialogs are suppressed in an installed standalone PWA.
func askCancelControl(v MirrorAskLiveView) g.Node {
	base := "askc" + MirrorAskRowID(v.ToolID)[2:]
	hidden := func(n, val string) g.Node { return Input(Type("hidden"), Name(n), Value(val)) }
	name := v.WindowName
	if name == "" {
		name = "this window"
	}
	return Div(g.Attr("data-ask-cancel-control", ""), Class("flex flex-col gap-1"),
		Div(Class("flex flex-wrap items-center gap-2"),
			Button(Type("button"), g.Attr("data-ask-cancel", ""), g.Attr("aria-haspopup", "dialog"),
				Class("press inline-flex min-h-[44px] items-center rounded-lg px-3 text-xs font-medium text-fg2 ring-1 ring-inset ring-edge hover:bg-s2"+focusRing),
				g.Text("Cancel question"))),
		P(g.Attr("data-ask-cancel-note", ""), g.Attr("role", "alert"), g.Attr("hidden", ""),
			Class("break-words rounded-lg bg-danger/15 px-3 py-2 text-xs text-fg")),
		Dialog(g.Attr("data-ask-cancel-dialog", ""), g.Attr("aria-labelledby", base+"-title"),
			Class("m-auto w-[calc(100%-2rem)] max-w-sm rounded-2xl bg-s1 p-4 text-sm text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/60"),
			Form(g.Attr("data-ask-cancel-form", ""), Method("post"), Action(MirrorAskCancelPath), Class("flex flex-col gap-3"),
				hidden("host", v.Ref.Host), hidden("session", v.Ref.Session),
				g.If(v.Ref.Window != "", hidden("window", v.Ref.Window)),
				hidden("tool", v.ToolID),
				P(ID(base+"-title"), Class("break-words font-medium"), g.Text("Send Esc to "+name+"? The question is dismissed.")),
				Div(Class("flex flex-wrap justify-end gap-2"),
					Button(Type("button"), g.Attr("data-ask-cancel-keep", ""),
						Class("press inline-flex min-h-[44px] items-center rounded-lg px-3 text-sm text-fg2 ring-1 ring-inset ring-edge hover:bg-s2"+focusRing),
						g.Text("Keep it")),
					Button(Type("submit"), g.Attr("data-ask-cancel-confirm", ""),
						Class("press inline-flex min-h-[44px] items-center rounded-lg bg-danger px-3 text-sm font-semibold text-on-danger hover:bg-danger/90"+focusRing),
						g.Text("Send Esc"))))))
}

// askScript polls the window's live region and sends a form's answers.
//
// 🔴 A PENDING CARD IS NEVER REDRAWN WHILE IT STAYS PENDING: a poll that lists
// it as pending again leaves it alone, so what a person has chosen or typed is
// not thrown away. Any other state (typing, answered here or in the terminal,
// dismissed) replaces it; a card the server no longer lists (a few minutes
// after it ended) is removed.
//
// It polls when the page's socket says the window's questions changed (a
// message of type "asks"), so a question shows the moment its hook registers
// it — and every few seconds besides, for a socket that is down.
func askScript() g.Node {
	return Script(g.Raw(`
(function () {
  var region = document.getElementById('mv-asks');
  if (!region) return;
  var busy = false, again = false;
  function frag(html) { var t = document.createElement('template'); t.innerHTML = html; return t.content; }
  function tidy() { region.hidden = !region.querySelector('[data-ask-live]'); }
  function merge(html) {
    var got = frag(html), seen = {};
    Array.prototype.slice.call(got.querySelectorAll('[data-ask-live]')).forEach(function (card) {
      var tool = card.getAttribute('data-ask-tool');
      seen[tool] = true;
      var cur = region.querySelector('[data-ask-live][data-ask-tool="' + CSS.escape(tool) + '"]');
      if (!cur) { region.appendChild(card); return; }
      // The same state, and no dismissal's note on either: left alone (what
      // a person chose stays). A note (data-ask-sent) is the dismissal's
      // answer only, so the next poll's card replaces it.
      if (cur.getAttribute('data-ask-state') === card.getAttribute('data-ask-state') &&
          (cur.getAttribute('data-ask-sent') || '') === (card.getAttribute('data-ask-sent') || '')) return;
      cur.replaceWith(card);
    });
    region.querySelectorAll('[data-ask-live]').forEach(function (c) { if (!seen[c.getAttribute('data-ask-tool')]) c.remove(); });
    tidy();
  }
  function poll() {
    if (busy || document.hidden) return;
    busy = true;
    fetch(region.getAttribute('data-src'), { credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) { return r.ok ? r.text().then(merge) : null; })
      .catch(function () {})
      .then(function () { busy = false; if (again) { again = false; poll(); } });
  }
  // The socket says when the window's questions change (type "asks"); the
  // interval is the fallback for a socket that is down.
  window.musterAskPoll = function () { if (busy) { again = true; return; } poll(); };
  setInterval(poll, 3000);
  document.addEventListener('visibilitychange', poll);
  poll();
  function send(form) {
    var card = form.closest('[data-ask-live]');
    var err = form.querySelector('[data-ask-error]');
    var btn = form.querySelector('[data-ask-submit]');
    function say(msg) { if (!err) return; err.textContent = msg || ''; err.hidden = !msg; }
    say('');
    if (btn) btn.disabled = true;
    var body = new URLSearchParams(new FormData(form));
    fetch(form.getAttribute('action'), { method: 'POST', body: body, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (r.status === 403 && r.headers.get('X-Muster-Step-Up') && window.musterStepUp) {
          window.musterStepUp.stale();
          window.musterStepUp.ask('', function () { return send(form); });
          return;
        }
        return r.text().then(function (t) {
          var got = r.headers.get('X-Ask-State') ? frag(t).querySelector('[data-ask-live]') : null;
          if (got && card) { card.replaceWith(got); tidy(); return; }
          if (!r.ok) say(t.trim() || ('Not sent (HTTP ' + r.status + ')'));
        });
      }, function () { say(navigator.onLine === false ? 'You are offline — the answers were not sent.' : 'The answers were not sent.'); })
      .then(function () { if (btn && document.body.contains(btn)) btn.disabled = false; });
  }
  document.addEventListener('submit', function (e) {
    var cf = e.target && e.target.closest && e.target.closest('form[data-ask-cancel-form]');
    if (cf && typeof fetch === 'function') { e.preventDefault(); cancelAsk(cf); return; }
    var form = e.target && e.target.closest && e.target.closest('form[data-ask-form]');
    if (!form || typeof fetch !== 'function') return;
    e.preventDefault();
    send(form);
  });
  // "Cancel question" opens its dialog; "Keep it" closes it, sending nothing.
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target : null;
    if (!t) return;
    var open = t.closest('[data-ask-cancel]');
    if (open) {
      var d = open.closest('[data-ask-cancel-control]').querySelector('[data-ask-cancel-dialog]');
      if (d && !d.open) d.showModal();
      return;
    }
    var keep = t.closest('[data-ask-cancel-keep]');
    if (keep) { var kd = keep.closest('dialog'); if (kd) kd.close(); }
  });
  function cancelAsk(form) {
    var card = form.closest('[data-ask-live]');
    var ctl = form.closest('[data-ask-cancel-control]');
    var note = ctl && ctl.querySelector('[data-ask-cancel-note]');
    var dlg = form.closest('dialog');
    var btn = form.querySelector('[data-ask-cancel-confirm]');
    function say(msg) { if (!note) return; note.textContent = msg || ''; note.hidden = !msg; }
    say('');
    if (btn) btn.disabled = true;
    fetch(form.getAttribute('action'), { method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        return r.text().then(function (t) {
          if (dlg && dlg.open) dlg.close();
          var got = r.headers.get('X-Ask-State') ? frag(t).querySelector('[data-ask-live]') : null;
          if (got && card) { card.replaceWith(got); tidy(); return; }
          if (!r.ok) say(t.trim() || ('Not sent (HTTP ' + r.status + ')'));
        });
      }, function () { if (dlg && dlg.open) dlg.close(); say('Not sent — check your connection.'); })
      .then(function () { if (btn && document.body.contains(btn)) btn.disabled = false; });
  }
  // Typing an answer of one's own chooses "Other".
  document.addEventListener('input', function (e) {
    var t = e.target;
    if (!t || !t.matches || !t.matches('[data-ask-other-text]')) return;
    var box = document.getElementById(t.getAttribute('data-ask-other-for'));
    if (box && t.value.trim()) box.checked = true;
  });
})();
`))
}
