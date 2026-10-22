package mirror

import (
	"encoding/json"
	"testing"
)

// TestTheSubagentNamesTheWireReExportsAreUnchanged: the subagent names and
// bounds internal/mirror now re-exports from internal/ccsubagents keep the
// values and the encoding the wire had when they were defined here — literal
// values, not the leaf's, so a change in the leaf cannot carry the wire with
// it unnoticed.
func TestTheSubagentNamesTheWireReExportsAreUnchanged(t *testing.T) {
	if MaxSubagents != 64 {
		t.Errorf("MaxSubagents = %d, want 64", MaxSubagents)
	}
	if BackfillWindow != 4<<20 {
		t.Errorf("BackfillWindow = %d, want %d", BackfillWindow, 4<<20)
	}
	if SubagentRunning != "running" || SubagentDone != "done" || SubagentFailed != "failed" {
		t.Errorf("states = %q %q %q, want running done failed", SubagentRunning, SubagentDone, SubagentFailed)
	}
	for name, want := range map[string]bool{"Agent": true, "Task": true, "Bash": false, "agent": false, "": false} {
		if got := IsSubagentTool(name); got != want {
			t.Errorf("IsSubagentTool(%q) = %v, want %v", name, got, want)
		}
	}
	long := "a"
	for len(long) < 64 {
		long += "b"
	}
	for id, want := range map[string]bool{"a3f09c": true, "toolu_01AbC-x": true, long: true, long + "c": false,
		"": false, "_a": false, "-a": false, "a/b": false, "a.b": false, "..": false} {
		if got := ValidSubagentID(id); got != want {
			t.Errorf("ValidSubagentID(%q) = %v, want %v", id, got, want)
		}
	}
	b, _ := json.Marshal(SubagentStep{Tool: "Bash", Text: "ls", At: 5})
	if string(b) != `{"tool":"Bash","text":"ls","at":5}` {
		t.Errorf("a step encodes as %s", b)
	}
	b, _ = json.Marshal(SubagentStep{Text: "done"})
	if string(b) != `{"text":"done"}` {
		t.Errorf("a text step encodes as %s", b)
	}
}
