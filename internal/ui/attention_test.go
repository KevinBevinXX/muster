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

// TestTheBadgeFragmentKeepsListening: the fragment the badge route answers
// replaces the badge (outerHTML), so it must carry the same refresh wiring —
// a fragment without it would leave a badge that never moves again. Hidden
// at zero, the count shown otherwise.
func TestTheBadgeFragmentKeepsListening(t *testing.T) {
	zero, three := AttentionBadgeHTML(0), AttentionBadgeHTML(3)
	for _, b := range []string{zero, three} {
		for _, want := range []string{`hx-get="` + AttentionBadgePath + `"`, `hx-trigger="` + attentionBadgeTrigger + `"`, `hx-swap="outerHTML"`, `id="attention-badge"`} {
			if !strings.Contains(b, want) {
				t.Fatalf("%s lacks %s", b, want)
			}
		}
	}
	if !strings.Contains(zero, " hidden>") || strings.Contains(three, " hidden") || !strings.Contains(three, `>3</span>`) {
		t.Fatalf("zero %s / three %s", zero, three)
	}
	if !strings.Contains(attentionBadgeTrigger, "sse:attention_queue.changed") {
		t.Fatal("the badge does not listen for the queue's event")
	}
}
