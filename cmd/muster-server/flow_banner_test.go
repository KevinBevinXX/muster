package main

import (
	"strings"
	"testing"
)

// TestAnUnsetAgentDenyWordListIsALoudLine: the agent door's deployment word
// list fails OPEN when unset (only the generic patterns screen agent text), so
// the unset state is a WARNING naming the variable and what is not screened —
// and the set state says how many names are in force.
func TestAnUnsetAgentDenyWordListIsALoudLine(t *testing.T) {
	onOut, offOut := bannerBothDirections(t)
	off := lineNaming(offOut, envFlowAgentDenyWords)
	for _, want := range []string{"WARNING", "UNSET", "GENERIC patterns only"} {
		if !strings.Contains(off, want) {
			t.Errorf("the unset line lacks %q: %q", want, off)
		}
	}
	if on := lineNaming(onOut, envFlowAgentDenyWords); !strings.Contains(on, "1 deployment name(s)") || strings.Contains(on, "WARNING") {
		t.Errorf("the set line = %q; want the count of names and no warning", on)
	}
}
