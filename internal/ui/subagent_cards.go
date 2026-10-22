package ui

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE SUBAGENT CARDS of the rendered view (internal/mirror's subagents.go): at
// each Agent (or Task) call, a card in place of the plain tool row — the
// agent's description and type, running / done / failed, elapsed time and its
// latest step — and, when it is opened, the agent's own recent steps.
//
// 🔴 ONLY FOR A HOST THAT REPORTS SUBAGENTS (MirrorTranscript.Subagents, set
// from the stream's SubagentList). An older host's Agent call is the plain
// tool row it always was: no card, nothing broken.
//
// 🔴 THE STATE IS THE HOST'S, NEVER DERIVED HERE. The host applies the one
// status rule (internal/ccsubagents' State) and sends the result; a
// card with no summary yet says "starting", whatever its call's tool_result
// says — a background agent's result is only its launch.
//
// The card is a <details> whose <summary> (id <card>-s) is redrawn alone when a
// summary moves, so an opened card keeps its tail; the page's script fills the
// tail from MirrorSubagentPath while it is open and ticks the elapsed time.

// MirrorSubagentPath is a subagent's recent steps, as an HTML fragment
// (?host&session&window&agent).
const MirrorSubagentPath = "/mirror/view/subagent"

// SubagentPath is MirrorSubagentPath for this entry, before the page adds
// `agent`.
func (r MirrorRef) SubagentPath() string { return MirrorSubagentPath + "?" + r.query() }

// subagentCall is what the parent's Agent call says about its agent.
type subagentCall struct {
	description, agentType string
	result                 *ChatLine
}

// subagentCallOf reads an Agent call's description and agent type from its
// input — by name, from a decoded object. Input is capped at MaxInput, which
// can cut a long prompt's call to a document that no longer decodes: then the
// call's one-line target stands in for the description.
func subagentCallOf(ev cctranscript.Event) subagentCall {
	var in struct {
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal([]byte(ev.Input), &in)
	c := subagentCall{description: in.Description, agentType: in.SubagentType}
	if c.description == "" {
		c.description = ev.Target
	}
	return c
}

// The card states the page draws, beyond the host's three.
const subagentStarting = "starting"

// subagentStateLabel is a state as the card says it.
func subagentStateLabel(state string) (label, class string) {
	switch state {
	case mirror.SubagentDone:
		return "done", "bg-accent/15 text-accent ring-accent/40"
	case mirror.SubagentFailed:
		return "failed", "bg-danger/15 text-st-error-fg ring-danger/40"
	case mirror.SubagentRunning:
		return "running", "bg-s2 text-fg2 ring-line animate-pulse"
	}
	return "starting", "bg-s2 text-muted ring-line"
}

// subagentElapsed is d short: 9s, 3m 04s, 1h 02m.
func subagentElapsed(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m " + pad2(int(d.Seconds())%60) + "s"
	default:
		return strconv.Itoa(int(d.Hours())) + "h " + pad2(int(d.Minutes())%60) + "m"
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// subagentSummary is a card's <summary>: what is redrawn as the agent moves.
func subagentSummary(id string, c subagentCall, s *mirror.Subagent) g.Node {
	desc, typ, state, last := c.description, c.agentType, subagentStarting, ""
	var started, ended int64
	background := false
	if s != nil {
		if s.Description != "" {
			desc = s.Description
		}
		if s.Type != "" {
			typ = s.Type
		}
		state, last, started, background = s.State, s.Last, s.Started, s.Background
		if s.State != mirror.SubagentRunning {
			ended = s.Updated
		}
	}
	if desc == "" {
		desc = "Subagent"
	}
	label, cls := subagentStateLabel(state)
	var elapsed g.Node
	if started > 0 {
		text := "…"
		if ended >= started {
			text = subagentElapsed(ended - started)
		}
		elapsed = Span(g.Attr("data-subagent-elapsed", ""), g.Attr("data-started", strconv.FormatInt(started, 10)),
			g.If(ended >= started, g.Attr("data-ended", strconv.FormatInt(ended, 10))),
			Class("shrink-0 tabular-nums text-muted"), g.Text(text))
	}
	return Summary(
		ID(id+"-s"),
		Class("flex min-h-[44px] min-w-0 cursor-pointer select-none flex-col gap-1 px-3 py-2 text-fg2 hover:text-fg"),
		Div(Class("flex min-w-0 items-center gap-2"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("🤖")),
			Span(g.Attr("data-subagent-desc", ""), Class("min-w-0 truncate font-medium text-fg"), g.Text(desc)),
			Span(g.Attr("data-subagent-status", state), Class("ml-auto shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold ring-1 ring-inset "+cls),
				g.Text(label)),
		),
		Div(Class("flex min-w-0 items-center gap-2 text-[11px]"),
			g.If(typ != "", Span(g.Attr("data-subagent-type", ""), Class("shrink-0 font-mono text-st-review-fg"), g.Text(typ))),
			g.If(background, Span(g.Attr("data-subagent-background", ""), Class("shrink-0 text-muted"), g.Text("background"))),
			elapsed,
		),
		g.If(last != "", Div(g.Attr("data-subagent-last", ""), Class("min-w-0 truncate font-mono text-[11px] text-muted"), g.Text(last))),
	)
}

// subagentCard is a whole card: its summary, the tail the page fills while it
// is open, and the call's result once it has one.
func subagentCard(id string, c subagentCall, s *mirror.Subagent) g.Node {
	state, agent := subagentStarting, ""
	if s != nil {
		state, agent = s.State, s.Agent
	}
	var result g.Node
	if c.result != nil && strings.TrimSpace(c.result.Content) != "" {
		result = toolPre("result", c.result.Content)
	}
	return Details(
		ID(id),
		g.Attr("data-subagent-card", ""),
		g.Attr("data-subagent-state", state),
		g.If(agent != "", g.Attr("data-agent", agent)),
		Class("w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line"),
		subagentSummary(id, c, s),
		Div(Class("flex flex-col gap-1 px-3 pb-2 pt-0"),
			Div(g.Attr("data-subagent-tail", ""), Class("flex flex-col gap-1"),
				P(Class("text-muted"), g.Text("Loading its steps…"))),
			result,
		),
	)
}

// subagentCallOps draws an Agent call as its card: not a tool row, so it ends
// a run and is never grouped (like a question card).
func (t *MirrorTranscript) subagentCallOps(ev cctranscript.Event) []MirrorOp {
	if t.cards == nil {
		t.cards = map[string]subagentCall{}
	}
	id := toolRowID(ev.ToolID)
	_, seen := t.calls[ev.ToolID]
	t.track(ev.ToolID)
	t.calls[ev.ToolID] = ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool}
	c := subagentCallOf(ev)
	if old, ok := t.cards[ev.ToolID]; ok {
		c.result = old.result
	}
	t.cards[ev.ToolID] = c
	t.endRun()
	html := render(subagentCard(id, c, t.subOf(ev.ToolID)))
	if seen {
		return []MirrorOp{{Op: "replace", ID: id, HTML: html}}
	}
	return []MirrorOp{{Op: "append", ID: id, HTML: html}}
}

// subagentResultOps redraws a card with its call's result.
func (t *MirrorTranscript) subagentResultOps(ev cctranscript.Event) []MirrorOp {
	c := t.cards[ev.ToolID]
	c.result = &ChatLine{Kind: "tool_result", ToolID: ev.ToolID, ToolOK: ev.OK, Content: ev.Output}
	t.cards[ev.ToolID] = c
	id := toolRowID(ev.ToolID)
	return []MirrorOp{{Op: "replace", ID: id, HTML: render(subagentCard(id, c, t.subOf(ev.ToolID)))}}
}

func (t *MirrorTranscript) subOf(toolID string) *mirror.Subagent {
	if s, ok := t.subs[toolID]; ok {
		return &s
	}
	return nil
}

// SubagentOps is a subagent list (mirror.Msg.Subagents) as the page's DOM
// operations: each card whose summary changed is redrawn — its <summary> (a
// "patch": a page without that card draws nothing), and its state and agent
// attributes — and nothing else, so an opened card keeps its tail. A summary for a call this socket never drew is drawn only when the
// page may have it from an earlier socket (Resumed); otherwise it is kept for
// the card the call will draw.
func (t *MirrorTranscript) SubagentOps(list []mirror.Subagent) []MirrorOp {
	if t.subs == nil {
		t.subs = map[string]mirror.Subagent{}
	}
	var ops []MirrorOp
	for _, s := range mirror.NormalizeSubagents(list) {
		if old, ok := t.subs[s.ToolID]; ok && old == s {
			continue
		}
		t.subs[s.ToolID] = s
		c, drawn := t.cards[s.ToolID]
		if !drawn && !t.Resumed {
			continue
		}
		id := toolRowID(s.ToolID)
		ops = append(ops,
			MirrorOp{Op: "patch", ID: id + "-s", HTML: render(subagentSummary(id, c, &s))},
			MirrorOp{Op: "attr", ID: id, Name: "data-subagent-state", Value: s.State},
			MirrorOp{Op: "attr", ID: id, Name: "data-agent", Value: s.Agent},
		)
	}
	return ops
}

// SubagentTailView is one opened card's steps (MirrorSubagentPath).
type SubagentTailView struct {
	Tail mirror.SubagentTail
	// Note replaces the steps when they could not be read: why.
	Note string
}

// SubagentTailFragment is a card's steps, oldest first, as the fragment the
// page puts in its tail.
func SubagentTailFragment(v SubagentTailView) g.Node {
	if v.Note != "" {
		return P(g.Attr("data-subagent-note", ""), Class("text-muted"), g.Text(v.Note))
	}
	if len(v.Tail.Steps) == 0 {
		return P(g.Attr("data-subagent-note", ""), Class("text-muted"), g.Text("No steps yet."))
	}
	rows := make([]g.Node, 0, len(v.Tail.Steps)+1)
	if v.Tail.More {
		rows = append(rows, P(Class("text-muted"), g.Text("Earlier steps not shown.")))
	}
	for _, s := range v.Tail.Steps {
		rows = append(rows, Div(g.Attr("data-subagent-step", ""), Class("flex min-w-0 items-baseline gap-2 font-mono text-[11px]"),
			g.If(s.Tool != "", Span(Class("shrink-0 text-st-review-fg"), g.Text(s.Tool))),
			Span(Class("min-w-0 break-words text-fg2"), g.Text(s.Text)),
		))
	}
	return g.Group(rows)
}

// subagentCardScript is the cards' behaviour: an opened card's tail is read
// from the page's subagent path (data-subagent-src on the log) when it opens
// and every few seconds while it stays open, and every running card's elapsed
// time ticks. Every node it inserts is server-rendered HTML
// (SubagentTailFragment), whose text gomponents escaped.
func subagentCardScript() g.Node {
	return Script(g.Raw(`
(function () {
  var log = document.getElementById('mv-log');
  if (!log) return;
  var src = log.getAttribute('data-subagent-src');
  function fmt(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60), h = Math.floor(m / 60);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return h ? h + 'h ' + p(m % 60) + 'm' : m + 'm ' + p(s % 60) + 's';
  }
  function load(card) {
    var agent = card.getAttribute('data-agent');
    var tail = card.querySelector('[data-subagent-tail]');
    if (!tail) return;
    if (!agent || !src) { tail.innerHTML = '<p data-subagent-note class="text-muted">No steps yet.</p>'; return; }
    fetch(src + '&agent=' + encodeURIComponent(agent), { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) { var t = card.querySelector('[data-subagent-tail]'); if (t) t.innerHTML = html; })
      .catch(function () { var t = card.querySelector('[data-subagent-tail]'); if (t) t.innerHTML = '<p data-subagent-note class="text-muted">Couldn\'t load its steps.</p>'; });
  }
  document.addEventListener('toggle', function (e) {
    var c = e.target;
    if (c && c.hasAttribute && c.hasAttribute('data-subagent-card') && c.open) load(c);
  }, true);
  setInterval(function () {
    [].forEach.call(log.querySelectorAll('details[data-subagent-card][open]'), load);
  }, 3000);
  function tick() {
    var now = Date.now();
    [].forEach.call(log.querySelectorAll('[data-subagent-elapsed]'), function (el) {
      var st = +el.getAttribute('data-started'), en = el.getAttribute('data-ended');
      if (st > 0) el.textContent = fmt((en ? +en : now) - st);
    });
  }
  setInterval(tick, 1000);
  tick();
})();
`))
}
