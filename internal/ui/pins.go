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

// agentPinMenuItem is the ⋯ menu's "Pin to top". A pinned agent is drawn as a
// chip, not a card (pinned_chips.go), and is unpinned from the chip's × — so
// the menu offers this item only for an unpinned agent (agentMenu).
func agentPinMenuItem(a AgentCardView) g.Node {
	return pinForm(false, [][2]string{{"kind", "agent"}, {"agent_id", strconv.FormatInt(a.ID, 10)}},
		Button(
			Type("submit"),
			g.Attr("data-pin-toggle", ""),
			Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-sm font-medium transition hover:bg-s2 disabled:opacity-60 text-fg"),
			hx("hx-disabled-elt", "this"),
			g.Text("Pin to top"),
		))
}

// mirrorPinToggle is a mirrored card's pin button: a mirrored card has no ⋯
// menu (there is nothing to start, stop or delete), so the toggle is the
// card's one control. It only ever PINS: a pinned window is drawn as a chip,
// not a card (pinned_chips.go), and is unpinned from the chip's ×.
func mirrorPinToggle(m MirrorCardView) g.Node {
	return pinForm(false, [][2]string{{"kind", "mirror"}, {"host", m.Host}, {"session", m.Session}, {"window", m.Window}},
		Button(
			Type("submit"),
			g.Attr("data-pin-toggle", ""),
			g.Attr("aria-label", "Pin to top"),
			g.Attr("title", "Pin to top"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-xl text-muted transition hover:bg-s2 hover:text-fg"),
			hx("hx-disabled-elt", "this"),
			g.Raw(pinIcon),
		))
}
