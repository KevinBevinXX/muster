package main

import (
	"strings"
	"testing"
)

// TestTheSuggestBootLineSaysItIsNotWiredYet: with suggestions on, the boot
// line names the model and says nothing generates until the chip row ships —
// never that sets are made at a turn end, which is not true yet. Off says off.
func TestTheSuggestBootLineSaysItIsNotWiredYet(t *testing.T) {
	on := suggestBootLine(config{SuggestModel: "example/model-x"})
	if !strings.Contains(on, "example/model-x") || !strings.Contains(on, "NOT GENERATED YET") || strings.Contains(on, "at a rendered view's turn end") {
		t.Fatalf("on: %q", on)
	}
	off := suggestBootLine(config{SuggestOff: true, SuggestModel: "example/model-x"})
	if !strings.Contains(off, "OFF (MUSTER_SUGGEST=off)") || strings.Contains(off, "example/model-x") {
		t.Fatalf("off: %q", off)
	}
}
