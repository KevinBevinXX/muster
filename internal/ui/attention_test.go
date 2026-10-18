package ui

import (
	"strings"
	"testing"
)

// TestTheAttentionLinkIsDrawnOnlyWithTheQueue: the sidebar draws the
// Attention link (with its live badge) only when the server holds the queue.
func TestTheAttentionLinkIsDrawnOnlyWithTheQueue(t *testing.T) {
	var on, off strings.Builder
	if err := RenderPage(&on, "tasks", Features{Attention: true}); err != nil {
		t.Fatal(err)
	}
	if err := RenderPage(&off, "tasks", Features{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on.String(), `id="sidebar-attention"`) || !strings.Contains(on.String(), `id="attention-badge"`) {
		t.Fatal("the queue is served and the sidebar has no Attention link")
	}
	if strings.Contains(off.String(), `sidebar-attention`) || strings.Contains(off.String(), `attention-badge`) {
		t.Fatal("no queue, yet the sidebar draws its link")
	}
}

// TestTheBadgeFragmentCannotRetriggerItself: the route's fragment replaces
// the slot's CONTENT and carries no htmx wiring — a fragment with a `load`
// trigger re-requests itself on every swap (measured: one request every
// ~25ms). The slot carries the wiring and swaps innerHTML. Hidden at zero.
func TestTheBadgeFragmentCannotRetriggerItself(t *testing.T) {
	zero, three := AttentionBadgeHTML(0), AttentionBadgeHTML(3)
	for _, b := range []string{zero, three} {
		if strings.Contains(b, "hx-") {
			t.Fatalf("the fragment carries htmx wiring: %s", b)
		}
	}
	slot := renderString(t, attentionBadgeSlot())
	for _, want := range []string{`hx-get="` + AttentionBadgePath + `"`, `hx-trigger="` + attentionBadgeTrigger + `"`, `hx-swap="innerHTML"`, `id="attention-badge-slot"`} {
		if !strings.Contains(slot, want) {
			t.Fatalf("the slot lacks %s: %s", want, slot)
		}
	}
	if !strings.Contains(zero, " hidden>") || strings.Contains(three, " hidden") || !strings.Contains(three, `>3</span>`) {
		t.Fatalf("zero %s / three %s", zero, three)
	}
	if !strings.Contains(attentionBadgeTrigger, "sse:attention_queue.changed") {
		t.Fatal("the badge does not listen for the queue's event")
	}
}
