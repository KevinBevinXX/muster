package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// The pin routes (internal/api pins.go). The toggles are forms with hx-post:
// htmx posts them in place and the answer is the re-rendered list. They also
// carry method/action, but that is not a no-JavaScript path — #agents-list and
// its cards are loaded only by htmx, so without JavaScript no toggle is drawn.
const (
	PinPath   = "/agents/pins"
	UnpinPath = "/agents/pins/delete"
)

// pinIcon is a push-pin, drawn in currentColor.
const pinIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 17v5"/><path d="M9 3h6l-1 6 3 3v2H7v-2l3-3z"/></svg>`

// pinnedGlyph is the "pinned" mark beside a pinned card's title, on both card
// kinds.
func pinnedGlyph() g.Node {
	return Span(
		g.Attr("data-pinned-glyph", ""),
		g.Attr("role", "img"),
		g.Attr("aria-label", "Pinned"),
		g.Attr("title", "Pinned to the top of your list"),
		Class("inline-flex items-center text-accent"),
		g.Raw(pinIcon),
	)
}

// pinForm is the form one pin toggle submits: to PinPath when the entry is not
// pinned, to UnpinPath when it is, with the subject as hidden fields. It
// carries its own hx-post/target/swap rather than inheriting #agents-list's.
func pinForm(pinned bool, fields [][2]string, button g.Node) g.Node {
	action := PinPath
	if pinned {
		action = UnpinPath
	}
	nodes := []g.Node{
		Method("post"),
		Action(action),
		g.Attr("data-pin-form", ""),
		hx("hx-post", action),
		hx("hx-target", "#agents-list"),
		hx("hx-swap", "morph:innerHTML"),
	}
	for _, f := range fields {
		nodes = append(nodes, Input(Type("hidden"), Name(f[0]), Value(f[1])))
	}
	return Form(append(nodes, button)...)
}

// agentPinMenuItem is the ⋯ menu's "Pin to top" / "Unpin".
func agentPinMenuItem(a AgentCardView) g.Node {
	label := "Pin to top"
	if a.Pinned {
		label = "Unpin"
	}
	return pinForm(a.Pinned, [][2]string{{"kind", "agent"}, {"agent_id", strconv.FormatInt(a.ID, 10)}},
		Button(
			Type("submit"),
			g.Attr("data-pin-toggle", ""),
			Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-sm font-medium transition hover:bg-s2 disabled:opacity-60 text-fg"),
			hx("hx-disabled-elt", "this"),
			g.Text(label),
		))
}

// mirrorPinToggle is a mirrored card's pin button: a mirrored card has no ⋯
// menu (there is nothing to start, stop or delete), so the toggle is the
// card's one control, pressed while pinned.
func mirrorPinToggle(m MirrorCardView) g.Node {
	label := "Pin to top"
	if m.Pinned {
		label = "Unpin"
	}
	return pinForm(m.Pinned, [][2]string{{"kind", "mirror"}, {"host", m.Host}, {"session", m.Session}, {"window", m.Window}},
		Button(
			Type("submit"),
			g.Attr("data-pin-toggle", ""),
			g.Attr("aria-label", label),
			g.Attr("title", label),
			g.Attr("aria-pressed", strconv.FormatBool(m.Pinned)),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-xl text-muted transition hover:bg-s2 hover:text-fg aria-pressed:bg-accent aria-pressed:text-on-accent"),
			hx("hx-disabled-elt", "this"),
			g.Raw(pinIcon),
		))
}
