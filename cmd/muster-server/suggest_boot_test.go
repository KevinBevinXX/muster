package main

import (
	"strings"
	"testing"
)

// TestTheSuggestBootLineSaysWhenSetsAreMade: with suggestions on, the boot
// line names the model and says a set is made at a Rendered view's turn end
// (P2 wired it) — not that nothing generates. Off says off.
func TestTheSuggestBootLineSaysWhenSetsAreMade(t *testing.T) {
	on := suggestBootLine(config{SuggestModel: "example/model-x"})
	if !strings.Contains(on, "ON with example/model-x") || !strings.Contains(on, "turn end while its Rendered view is open") || strings.Contains(on, "NOT GENERATED") {
		t.Fatalf("on: %q", on)
	}
	off := suggestBootLine(config{SuggestOff: true, SuggestModel: "example/model-x"})
	if !strings.Contains(off, "OFF (MUSTER_SUGGEST=off)") || strings.Contains(off, "example/model-x") {
		t.Fatalf("off: %q", off)
	}
}
