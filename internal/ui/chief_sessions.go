package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (internal/api/session_runtime.go):
// the panel's thread list gains a "New session" picker — a runtime and a model,
// fixed for the session's life — each thread row names its runtime and model,
// and one thread is marked the DEFAULT (where plugin messages and `chief ask`
// go), with a control on every other row to make it so.

const (
	// sessionPickerAttr marks the picker; newSessionAttr each of its forms,
	// valued with the runtime it starts.
	sessionPickerAttr = "data-session-picker"
	newSessionAttr    = "data-new-session"
	// sessionRuntimeAttr marks a row's runtime line, valued with the runtime.
	sessionRuntimeAttr = "data-session-runtime"
	// sessionDefaultAttr marks the default thread's badge; makeDefaultAttr the
	// control that makes a thread the default.
	sessionDefaultAttr = "data-session-default"
	makeDefaultAttr    = "data-make-default"
	// sessionModelInputID / sessionModelListID are the OpenRouter model
	// combobox's own ids: the agent page's header has one of its own
	// (modelFieldForDisplay's "model-input"), and the panel can be open on it.
	sessionModelInputID = "chief-session-model-input"
	sessionModelListID  = "chief-session-model-list"
)

// SessionRuntimeView names a runtime the picker offers.
type SessionRuntimeView struct {
	Runtime string // "claude-code" | "gateway"
	Label   string
}

// sessionRuntimeLine is a thread row's runtime and model, for an explicit
// session; nothing for a session on the chief's own runtime.
func sessionRuntimeLine(sess SessionView) g.Node {
	if sess.Runtime == "" {
		return nil
	}
	text := sess.RuntimeLabel
	if m := strings.TrimPrefix(sess.Model, "openrouter/"); m != "" {
		text += " · " + m
	}
	return Span(
		g.Attr(sessionRuntimeAttr, sess.Runtime),
		Class("text-[11px] leading-snug text-muted"),
		g.Text(text),
	)
}

// sessionDefaultBadge marks the default thread.
func sessionDefaultBadge(sess SessionView) g.Node {
	if !sess.IsDefault {
		return nil
	}
	return Span(
		g.Attr(sessionDefaultAttr, ""),
		g.Attr("title", "Plugin messages and chief ask go to this thread"),
		Class("shrink-0 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-accent ring-1 ring-inset ring-accent/50"),
		g.Text("Default"),
	)
}

// makeDefaultControl makes a thread the default, re-rendering the panel on it.
// Nothing for the default thread itself, or where defaults are not offered.
func makeDefaultControl(sess SessionView) g.Node {
	if sess.IsDefault || sess.DefaultPath == "" {
		return nil
	}
	return Form(
		hx("hx-post", sess.DefaultPath+"?"+ChatSurfaceQueryParam+"="+string(ChatSurfaceChiefPanel)),
		hx("hx-target", "#"+chiefPanelBodyID),
		hx("hx-swap", "innerHTML"),
		Class("contents"),
		Button(
			Type("submit"),
			g.Attr(makeDefaultAttr, ""),
			g.Attr("title", "Send plugin messages and chief ask to this thread"),
			Class("press shrink-0 rounded-lg px-2 text-[11px] text-muted ring-1 ring-inset ring-line transition hover:bg-s2 hover:text-fg"),
			g.Text("Make default"),
		),
	)
}

// chiefNewSessionPicker opens a session on a runtime and a model: one compact
// form per offered runtime, each posting runtime + model to the session-create
// route, whose panel answer re-renders the panel on the new session.
func chiefNewSessionPicker(agentID int64, runtimes []SessionRuntimeView, claudeModels []string) g.Node {
	if len(runtimes) == 0 {
		return nil
	}
	forms := make([]g.Node, 0, len(runtimes))
	for _, rt := range runtimes {
		var model g.Node
		switch rt.Runtime {
		case "claude-code":
			opts := make([]g.Node, 0, len(claudeModels))
			for _, m := range claudeModels {
				opts = append(opts, Option(Value(m), g.Text(m)))
			}
			model = Select(
				Name("model"),
				g.Attr("aria-label", rt.Label+" model"),
				Class("min-h-[40px] min-w-0 flex-1 rounded-lg border-0 bg-bg px-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
				g.Group(opts),
			)
		default:
			model = Div(Class("min-w-0 flex-1"), sessionModelCombobox(rt.Label+" model"))
		}
		forms = append(forms, Form(
			g.Attr(newSessionAttr, rt.Runtime),
			hx("hx-post", agentSessionsPath(agentID)+"?"+ChatSurfaceQueryParam+"="+string(ChatSurfaceChiefPanel)),
			hx("hx-target", "#"+chiefPanelBodyID),
			hx("hx-swap", "innerHTML"),
			Class("flex items-center gap-2"),
			Input(Type("hidden"), Name("runtime"), Value(rt.Runtime)),
			Span(Class("w-20 shrink-0 text-xs text-fg2"), g.Text(rt.Label)),
			model,
			Button(
				Type("submit"),
				Class("press min-h-[40px] shrink-0 rounded-lg bg-accent px-3 text-xs font-semibold text-on-accent transition hover:bg-accent/90"),
				g.Text("Start"),
			),
		))
	}
	return Div(
		g.Attr(sessionPickerAttr, ""),
		Class("flex flex-none flex-col gap-2 rounded-lg bg-s1 p-2 ring-1 ring-inset ring-line"),
		P(Class("text-xs font-semibold text-fg"), g.Text("New session")),
		g.Group(forms),
	)
}

// sessionModelCombobox is the OpenRouter model search for a new session: the
// shared combobox component (data-combobox, appScript) with the panel's own
// ids, seeded with the curated models — minus "the cluster default", which is
// not a model a session can be pinned to.
func sessionModelCombobox(label string) g.Node {
	var seeded []g.Node
	for _, m := range curatedModels() {
		if m.Value == "" {
			continue
		}
		seeded = append(seeded, seededComboOption(ComboOption{Value: m.Value, Label: m.Label}))
	}
	return Div(
		g.Attr("data-combobox", ""),
		g.Attr("data-combobox-remote", "/api/openrouter/models"),
		Class("relative"),
		Input(Type("hidden"), Name("model"), g.Attr("data-combobox-hidden", "")),
		Input(
			Type("text"),
			ID(sessionModelInputID),
			g.Attr("data-combobox-input", ""),
			g.Attr("role", "combobox"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", sessionModelListID),
			g.Attr("aria-label", label),
			g.Attr("autocomplete", "off"),
			Placeholder("Search OpenRouter models…"),
			Class("min-h-[40px] w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Ul(
			ID(sessionModelListID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			Class("hidden absolute z-30 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			g.Group(seeded),
		),
	)
}
