package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/askq"
)

// THE QUESTION CARDS — Claude Code's AskUserQuestion in the rendered view.
//
// TWO PLACES DRAW A QUESTION, BECAUSE THE TRANSCRIPT CANNOT SHOW A HELD ONE.
// Measured (CLI 2.1.289): the call's tool_use record is written to the
// transcript only AFTER the PreToolUse hook returns — while `muster-mirror ask`
// holds the call, the transcript has nothing for the conversation to draw. So:
//
//   - THE LIVE REGION (#mv-asks, above the composer) draws what the SERVER
//     knows: every question the window's ask hook is holding, as a form — one
//     fieldset per question (header and text), radio buttons for a
//     single-select question and checkboxes for a multi-select one (each
//     option with its description), an "Other" choice with a text box for the
//     person's own answer, and ONE Submit for every question. The page polls it
//     (MirrorAsksPath); a card stays a few minutes after it is answered here or
//     goes back to the terminal, until the conversation draws the call itself.
//   - THE CONVERSATION draws the call from the transcript once it is there:
//     read-only, with the answers its result records (the toolUseResult) —
//     however it was answered, here or in the terminal's own picker.
//
// 🔴 THE PAGE NEVER DECIDES ANYTHING. Which state a card is in is the server's
// answer to the poll; a submit is refused server-side unless the question is
// still pending, the host is -interactive and the sign-in is fresh, exactly
// like a Send.

// MirrorAskPath takes a card's answers (POST); MirrorAsksPath is the window's
// live region (GET).
const (
	MirrorAskPath  = "/mirror/view/ask"
	MirrorAsksPath = "/mirror/view/asks"
)

// The states a card can be in, as data-ask-state carries them.
// AskStateHistory is a card drawn from the transcript.
const (
	AskStatePending   = "pending"
	AskStateAnswered  = "answered"
	AskStateWithdrawn = "withdrawn"
	AskStateHistory   = "history"
	AskStateWaiting   = "waiting"
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
// questions, read-only. A call reaches the transcript only once no hook holds
// it — the terminal's picker is showing it.
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

// MirrorAskLiveView is one question set the window's ask hook asked, as the
// server knows it.
type MirrorAskLiveView struct {
	Ref    MirrorRef
	ToolID string
	// State is AskStatePending, AskStateAnswered or AskStateWithdrawn.
	State     string
	Questions []askq.Question
	Answers   map[string]string
	// Error is a refused submit's reason, drawn above the form.
	Error string
}

// MirrorAskLive is one card of the live region.
func MirrorAskLive(v MirrorAskLiveView) g.Node {
	label := "Claude is asking"
	var body g.Node
	switch v.State {
	case AskStatePending:
		body = askForm(v)
	case AskStateAnswered:
		label = "answered here"
		body = g.Group{askReadOnly(v.Questions, v.Answers),
			P(g.Attr("role", "status"), Class("text-xs text-accent"), g.Text("Answered here — sent to Claude."))}
	default:
		label = "back in the terminal"
		body = g.Group{askReadOnly(v.Questions, nil),
			P(g.Attr("role", "status"), Class("text-xs text-muted"),
				g.Text("Nobody answered here in time, so the question went back to the terminal — answer it there. "),
				A(Href(v.Ref.TerminalPath()), hx("hx-boost", "false"), Class("font-semibold text-accent underline"), g.Text("Open the terminal")))}
	}
	return Section(
		g.Attr("data-ask-live", ""),
		g.Attr("data-ask-tool", v.ToolID),
		g.Attr("data-ask-row", MirrorAskRowID(v.ToolID)),
		g.Attr("data-ask-state", v.State),
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
func mirrorAsksRegion(ref MirrorRef) g.Node {
	return Section(
		ID("mv-asks"),
		g.Attr("data-src", MirrorAsksSrc(ref)),
		g.Attr("aria-label", "Questions from Claude"),
		g.Attr("aria-live", "polite"),
		g.Attr("hidden", ""),
		Class("flex max-h-[70vh] shrink-0 flex-col gap-2 overflow-y-auto border-t border-line px-3 py-2"),
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
				Input(Type(kind), ID(id), Name(name), Value(o.Label), Class("mt-0.5 h-5 w-5 shrink-0 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing)),
				Span(Class("flex min-w-0 flex-col"),
					Span(Class("break-words text-fg"), g.Text(o.Label)),
					g.If(o.Description != "", Span(Class("break-words text-xs text-muted"), g.Text(o.Description))))))
		}
		otherID := base + "-" + idx + "-other"
		opts = append(opts, Div(Class("flex flex-col gap-1 rounded-lg px-2 py-2"),
			Label(For(otherID), Class("flex min-h-[44px] cursor-pointer items-center gap-2"),
				Input(Type(kind), ID(otherID), Name(name), Value(askq.OtherValue), Class("h-5 w-5 shrink-0 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing)),
				Span(Class("text-fg"), g.Text("Other"))),
			Textarea(Name("o"+idx), Rows("2"), MaxLength(strconv.Itoa(askq.MaxFreeText)),
				g.Attr("data-ask-other-text", ""), g.Attr("data-ask-other-for", otherID),
				g.Attr("aria-label", "Your own answer: "+q.Question),
				g.Attr("placeholder", "Type your own answer"),
				Class("w-full min-w-0 resize-y rounded-lg bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line"+focusRing)),
		))
		sets = append(sets, FieldSet(g.Attr("data-ask-question", ""), Class("flex min-w-0 flex-col gap-1"),
			Legend(Class("flex flex-col gap-1"), askQuestionHead(q)),
			g.If(q.MultiSelect, P(Class("text-[11px] text-muted"), g.Text("Choose any number"))),
			g.Group(opts)))
	}
	hidden := func(n, val string) g.Node { return Input(Type("hidden"), Name(n), Value(val)) }
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
			Button(Type("submit"), g.Attr("data-ask-submit", ""), Class(composerSendClass), g.Text("Submit answers")),
			Span(Class("text-xs text-muted"), g.Text("Claude is waiting — the terminal shows no picker while it waits here."))),
	)
}

// askScript polls the window's live region and sends a form's answers.
//
// 🔴 A PENDING CARD IS NEVER REDRAWN WHILE IT STAYS PENDING: a poll that lists
// it as pending again leaves it alone, so what a person has chosen or typed is
// not thrown away. Any other state (answered on another device, gone back to
// the terminal) replaces it — it cannot be answered here any more — and a card
// whose call the conversation has drawn (an element with its data-ask-row id
// in #mv-log) is removed: the conversation shows it from then on.
func askScript() g.Node {
	return Script(g.Raw(`
(function () {
  var region = document.getElementById('mv-asks');
  if (!region) return;
  var log = document.getElementById('mv-log');
  var busy = false;
  function frag(html) { var t = document.createElement('template'); t.innerHTML = html; return t.content; }
  function drawn(row) { return !!(row && log && log.querySelector('[id="' + row + '"]')); }
  function tidy() {
    region.querySelectorAll('[data-ask-live]').forEach(function (c) { if (drawn(c.getAttribute('data-ask-row'))) c.remove(); });
    region.hidden = !region.querySelector('[data-ask-live]');
  }
  function merge(html) {
    var got = frag(html), seen = {};
    Array.prototype.slice.call(got.querySelectorAll('[data-ask-live]')).forEach(function (card) {
      var tool = card.getAttribute('data-ask-tool');
      seen[tool] = true;
      var cur = region.querySelector('[data-ask-live][data-ask-tool="' + CSS.escape(tool) + '"]');
      if (drawn(card.getAttribute('data-ask-row'))) { if (cur) cur.remove(); return; }
      if (!cur) { region.appendChild(card); return; }
      if (cur.getAttribute('data-ask-state') === card.getAttribute('data-ask-state')) return;
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
      .then(function () { busy = false; });
  }
  setInterval(poll, 2000);
  document.addEventListener('visibilitychange', poll);
  poll();
  if (log && typeof MutationObserver !== 'undefined') new MutationObserver(tidy).observe(log, { childList: true, subtree: true });
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
    var form = e.target && e.target.closest && e.target.closest('form[data-ask-form]');
    if (!form || typeof fetch !== 'function') return;
    e.preventDefault();
    send(form);
  });
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
