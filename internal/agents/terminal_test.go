package agents

import (
	"errors"
	"slices"
	"testing"
)

// TestTerminalCommandIsOnlyForClaudeCode: the gateway kind (and the unset kind,
// which IS the gateway kind) has no session to attach to and is refused by
// name; a claude-code agent gets the attach command.
func TestTerminalCommandIsOnlyForClaudeCode(t *testing.T) {
	for _, kind := range []string{"", KindGateway} {
		if _, err := TerminalCommand(kind, true); !errors.Is(err, ErrNoTerminal) {
			t.Errorf("kind %q: want ErrNoTerminal, got %v", kind, err)
		}
	}
	cmd, err := TerminalCommand(KindClaudeCode, true)
	if err != nil {
		t.Fatalf("claude-code: %v", err)
	}
	if len(cmd) < 3 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != terminalAttachScript {
		t.Fatalf("claude-code command %q is not the attach script", cmd)
	}
}

// TestTerminalReadOnlyIsTheArgumentAndOnlyTheArgument: read-only puts tmux's
// -r last; interactive carries no -r ANYWHERE in argv (a stray one would make
// the "interactive" toggle silently read-only, and its absence in the
// read-only case would make the default writable).
func TestTerminalReadOnlyIsTheArgumentAndOnlyTheArgument(t *testing.T) {
	ro, _ := TerminalCommand(KindClaudeCode, true)
	if ro[len(ro)-1] != TerminalReadOnlyFlag {
		t.Errorf("read-only command %q does not end in %q", ro, TerminalReadOnlyFlag)
	}
	rw, _ := TerminalCommand(KindClaudeCode, false)
	if slices.Contains(rw, TerminalReadOnlyFlag) {
		t.Errorf("interactive command %q carries %q", rw, TerminalReadOnlyFlag)
	}
	if len(ro) != len(rw)+1 || !slices.Equal(ro[:len(rw)], rw) {
		t.Errorf("read-only must be the interactive command plus the flag: %q vs %q", ro, rw)
	}
}
