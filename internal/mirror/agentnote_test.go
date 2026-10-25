package mirror_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnAgentNoteIsKeptForAWireRuntimeOnlyAndBounded: the host's line about
// an opencode window reaches the list (and the card) as it was written, with
// control characters made spaces; a window of no wire runtime keeps none; a
// note is cut to MaxAgentNote bytes on a rune boundary; and at most
// MaxAgentNoteWindows windows of a list carry one, in the list's order.
func TestAnAgentNoteIsKeptForAWireRuntimeOnlyAndBounded(t *testing.T) {
	long := strings.Repeat("é", mirror.MaxAgentNote) // 2 bytes a rune
	in := []mirror.Window{
		{Session: "a", ID: "@1", Runtime: mirror.RuntimeOpenCode, AgentNote: "opencode is not paired on this host:\nrun muster-mirror opencode-pair"},
		{Session: "a", ID: "@2", Command: "zsh", AgentNote: "a plain window has no note"},
		{Session: "a", ID: "@3", Claude: true, ClaudeSession: "abc", AgentNote: "nor has Claude Code"},
		{Session: "a", ID: "@4", Runtime: mirror.RuntimeOpenCode, AgentNote: long},
	}
	for i := 0; i < mirror.MaxAgentNoteWindows+3; i++ {
		in = append(in, mirror.Window{Session: "b", ID: "@" + strconv.Itoa(10+i), Runtime: mirror.RuntimeOpenCode, AgentNote: "note"})
	}
	got := mirror.NormalizeWindows(in, 0)
	if got[0].AgentNote != "opencode is not paired on this host: run muster-mirror opencode-pair" {
		t.Errorf("the opencode note: %q", got[0].AgentNote)
	}
	if got[1].AgentNote != "" || got[2].AgentNote != "" {
		t.Errorf("a window of no wire runtime kept its note: %q %q", got[1].AgentNote, got[2].AgentNote)
	}
	if n := len(got[3].AgentNote); n > mirror.MaxAgentNote || n < mirror.MaxAgentNote-1 || !strings.HasPrefix(long, got[3].AgentNote) {
		t.Errorf("a long note is %d bytes (bound %d): %q", n, mirror.MaxAgentNote, got[3].AgentNote)
	}
	notes := 0
	for _, w := range got {
		if w.AgentNote != "" {
			notes++
		}
	}
	if notes != mirror.MaxAgentNoteWindows {
		t.Errorf("%d windows carry a note, want %d", notes, mirror.MaxAgentNoteWindows)
	}
	// The SessionView carries it to the card.
	h := mirror.HostView{Host: "h", Online: true, Windows: got[:1]}
	if e := h.Entries(); len(e) != 1 || e[0].AgentNote != got[0].AgentNote {
		t.Errorf("entries %+v", e)
	}
}
