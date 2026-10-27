package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// TOOL-CALL RUNS COLLAPSE INTO ONE ROW. Two or more tool rows with nothing
// between them — no assistant text, no notice, no prompt — are drawn as ONE
// collapsed "N tool calls" row (toolGroup). Opening it shows the individual rows
// (toolChip), each still expanding to its own args and result. A single call
// stays a plain row, and text stays exactly where it was in the sequence: a run
// ends at the first thing that is not a tool row.
//
// THE RULE IS STATED ONCE AND APPLIED BY THREE RENDERERS:
//
//   - groupToolRuns — a persisted transcript (chatLogInner: the agent chat and
//     the agent panel), server-rendered;
//   - MirrorTranscript.Ops — the mirrored view, server-rendered but incremental:
//     its runState wraps the previous row when a second call arrives and adds to
//     the group after that (mirror_view.go);
//   - toolGroupScript — the agent chat's LIVE stream, built in the browser.
//
// 🔴 THE MARKUP IS GO'S IN ALL THREE. The live stream's group shell is this
// file's toolGroup rendered to a string and handed to the script
// (toolGroupShellJSON), so the browser builds the very element the server
// draws rather than a JavaScript copy of it that can drift. What the script owns
// is only the rule's two moves — adopt a row into the run before it, and
// recount — and tool-rows.spec.ts renders one sequence both ways (live, then the
// persisted reload) and compares the result.
//
// ⚠ ONE KNOWN DIFFERENCE, AND IT IS THE STREAM'S, NOT THE RULE'S: the live stream
// draws a turn's reasoning (a "thinking" row) and a persisted transcript does
// not store it. A call, then thinking, then a call is two runs live and one run
// after the reload replaces the stream with the persisted transcript.

// toolGroupAnimMS is how long a group's height takes to open or close.
const toolGroupAnimMS = "220"

// Tool-row states, as data-tool-status spells them on a row and on a group.
const (
	toolStateRunning = "running"
	toolStateOK      = "ok"
	toolStateError   = "error"
)

// toolGroupState is a group's state from its rows': a failure shows (a closed
// group must not hide one), then a call still running, else every call
// succeeded.
func toolGroupState(states []string) string {
	running := false
	for _, s := range states {
		switch s {
		case toolStateError:
			return toolStateError
		case toolStateRunning:
			running = true
		}
	}
	if running {
		return toolStateRunning
	}
	return toolStateOK
}

// toolGroupLabel is a group's summary text.
func toolGroupLabel(n int) string { return strconv.Itoa(n) + " tool calls" }

// toolGroupStatus is the status glyph a group's summary carries, drawn the
// rows' way (toolChip).
func toolGroupStatus(state string) g.Node {
	glyph, class := "…", "ml-auto shrink-0 text-muted"
	switch state {
	case toolStateOK:
		glyph, class = "✓", "ml-auto shrink-0 font-semibold text-accent"
	case toolStateError:
		glyph, class = "✗", "ml-auto shrink-0 font-semibold text-st-error-fg"
	}
	return Span(g.Attr("data-tool-group-status", ""), g.Attr("aria-label", "status: "+state), Class(class), g.Text(glyph))
}

// toolGroupSummary is a group's one line: 🔧, "N tool calls", a chevron, and
// the group's state. id (may be empty) is the group's; the summary is id+"-s",
// so the mirrored view can redraw it alone as the count ticks up.
func toolGroupSummary(id string, n int, state string) g.Node {
	return Summary(
		g.If(id != "", ID(id+"-s")),
		// min-h-[44px]: a thumb target, like every row's summary.
		Class("flex min-h-[44px] min-w-0 cursor-pointer select-none items-center gap-2 px-3 py-2 font-medium text-fg2 hover:text-fg"),
		Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("🔧")),
		Span(g.Attr("data-tool-group-count", strconv.Itoa(n)), Class("min-w-0 truncate"), g.Text(toolGroupLabel(n))),
		Span(g.Attr("data-tool-group-chevron", ""), g.Attr("aria-hidden", "true"), Class("shrink-0 text-muted"), g.Text("›")),
		toolGroupStatus(state),
	)
}

// toolGroup is the collapsed row around a run of tool rows. id may be empty
// (a persisted transcript needs none); the body is id+"-b".
func toolGroup(id string, states []string, rows ...g.Node) g.Node {
	state := toolGroupState(states)
	return Details(
		g.If(id != "", ID(id)),
		g.Attr("data-tool-group", ""),
		g.Attr("data-tool-status", state),
		Class("w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line"),
		toolGroupSummary(id, len(states), state),
		Div(
			g.If(id != "", ID(id+"-b")),
			g.Attr("data-tool-group-body", ""),
			// The rows inside take the group's width rather than 92% of it
			// (web/css/input.css).
			Class("flex flex-col gap-1.5 px-2 pb-2"),
			g.Group(rows),
		),
	)
}

// toolGroupID is a mirrored run's group id: the run's FIRST call id, hashed
// (toolRowID's reasoning — it is the host's text).
func toolGroupID(firstToolID string) string {
	sum := sha256.Sum256([]byte("group:" + firstToolID))
	return "mg-" + hex.EncodeToString(sum[:8])
}

// logItem is one thing a transcript draws, in order: a node, and — when it is a
// tool row — the row's state ("" for anything else).
type logItem struct {
	node      g.Node
	toolState string
}

// groupToolRuns is the rule over a whole transcript: each run of two or more
// consecutive tool rows becomes one toolGroup; a single row and everything else
// pass through in place.
func groupToolRuns(items []logItem) []g.Node {
	out := make([]g.Node, 0, len(items))
	for i := 0; i < len(items); {
		if items[i].toolState == "" {
			out = append(out, items[i].node)
			i++
			continue
		}
		j := i
		for j < len(items) && items[j].toolState != "" {
			j++
		}
		if j-i == 1 {
			out = append(out, items[i].node)
		} else {
			rows := make([]g.Node, 0, j-i)
			states := make([]string, 0, j-i)
			for _, it := range items[i:j] {
				rows = append(rows, it.node)
				states = append(states, it.toolState)
			}
			out = append(out, toolGroup("", states, rows...))
		}
		i = j
	}
	return out
}

// toolGroupShellJSON is an EMPTY group, rendered, as a JS string literal: the
// live stream's template (toolGroupScript fills in its rows and count).
func toolGroupShellJSON() string {
	b, _ := json.Marshal(render(toolGroup("", nil)))
	return string(b)
}

// toolGroupScript is the live stream's half of the rule:
// window.musterToolGroups.adopt(row) after a row is inserted, and .refresh(el)
// after a row's state changes. Bound once per document.
//
//   - adopt: the element before the row is a group → the row joins it; is a
//     row → a group (toolGroupShellJSON) takes its place and both rows go in;
//     anything else → the row stays a plain row.
//   - refresh: the group's count, label and state, recomputed from its rows —
//     which is what makes the count tick up while a run is still streaming.
func toolGroupScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.musterToolGroups) return;
  var SHELL = ` + toolGroupShellJSON() + `;
  var GLYPH = { running: ['…', 'ml-auto shrink-0 text-muted'], ok: ['✓', 'ml-auto shrink-0 font-semibold text-accent'], error: ['✗', 'ml-auto shrink-0 font-semibold text-st-error-fg'] };
  function has(el, a) { return !!(el && el.hasAttribute && el.hasAttribute(a)); }
  function body(gr) { return gr.querySelector(':scope > [data-tool-group-body]'); }
  function groupOf(row) { var b = row && row.parentElement; return has(b, 'data-tool-group-body') ? b.parentElement : null; }
  function state(sts) {
    var running = false;
    for (var i = 0; i < sts.length; i++) { if (sts[i] === 'error') return 'error'; if (sts[i] === 'running') running = true; }
    return running ? 'running' : 'ok';
  }
  function refresh(el) {
    var gr = has(el, 'data-tool-group') ? el : groupOf(el);
    if (!gr) return;
    var rows = [].filter.call(body(gr).children, function (c) { return has(c, 'data-tool-row'); });
    var st = state(rows.map(function (r) { return r.getAttribute('data-tool-status'); }));
    gr.setAttribute('data-tool-status', st);
    var cnt = gr.querySelector(':scope > summary [data-tool-group-count]');
    cnt.setAttribute('data-tool-group-count', String(rows.length));
    cnt.textContent = rows.length + ' tool calls';
    var s = gr.querySelector(':scope > summary [data-tool-group-status]');
    s.setAttribute('aria-label', 'status: ' + st);
    s.textContent = GLYPH[st][0];
    s.className = GLYPH[st][1];
  }
  function adopt(row) {
    var prev = row && row.previousElementSibling;
    if (has(prev, 'data-tool-group')) { body(prev).appendChild(row); refresh(prev); return prev; }
    if (!has(prev, 'data-tool-row')) return null;
    var t = document.createElement('template');
    t.innerHTML = SHELL;
    var gr = t.content.firstElementChild;
    prev.parentNode.insertBefore(gr, prev);
    body(gr).appendChild(prev);
    body(gr).appendChild(row);
    refresh(gr);
    return gr;
  }
  window.musterToolGroups = { adopt: adopt, refresh: refresh };

  // THE GROUP'S HEIGHT ANIMATES, OPENING AND CLOSING. A <details> snaps: its
  // box jumps to the open height and everything below it with it. So a tap on
  // a group's summary is taken over — the box is held at its current height
  // and animated to the other one (the Web Animations API, which every browser
  // we serve has; CSS cannot animate a <details> closed, its content is gone the
  // instant [open] is), and only then is [open] removed on a close. Reduced
  // motion leaves the native snap. A second tap mid-way reverses from where the
  // box is. Delegated on document, so every group — persisted, live, mirrored —
  // is covered, including ones drawn after this ran.
  var GROUP_MS = ` + toolGroupAnimMS + `;
  function still() { return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches; }
  document.addEventListener('click', function (e) {
    var s = e.target && e.target.closest ? e.target.closest('summary') : null;
    var d = s && s.parentElement;
    if (!has(d, 'data-tool-group') || still() || typeof d.animate !== 'function') return;
    e.preventDefault();
    var from = d.getBoundingClientRect().height;
    if (d.__groupAnim) { d.__groupAnim.cancel(); d.__groupAnim = null; }
    var opening = !(d.open && !d.__closing);
    d.__closing = !opening;
    d.open = true;
    var to = opening ? d.getBoundingClientRect().height : s.getBoundingClientRect().height;
    d.style.overflow = 'hidden';
    var a = d.animate([{ height: from + 'px' }, { height: to + 'px' }], { duration: GROUP_MS, easing: 'cubic-bezier(0.2, 0.8, 0.2, 1)' });
    d.__groupAnim = a;
    a.onfinish = function () {
      if (d.__groupAnim !== a) return;
      d.__groupAnim = null;
      d.style.overflow = '';
      if (d.__closing) { d.__closing = false; d.open = false; }
    };
  });
})();
`))
}
