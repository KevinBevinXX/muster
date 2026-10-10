package agents

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fixtureTag is a well-formed tag for the command tests.
const fixtureTag TerminalTag = "MUSTER_TERMINAL_ID=0123456789abcdef0123456789abcdef"

// TestTerminalCommandIsOnlyForClaudeCode: the gateway kind (and the unset kind,
// which IS the gateway kind) has no session to attach to and is refused by
// name; a claude-code agent gets the attach command.
func TestTerminalCommandIsOnlyForClaudeCode(t *testing.T) {
	for _, kind := range []string{"", KindGateway} {
		if _, err := TerminalCommand(kind, true, fixtureTag); !errors.Is(err, ErrNoTerminal) {
			t.Errorf("kind %q: want ErrNoTerminal, got %v", kind, err)
		}
		if err := CheckTerminal(kind); !errors.Is(err, ErrNoTerminal) {
			t.Errorf("CheckTerminal(%q): want ErrNoTerminal, got %v", kind, err)
		}
	}
	if err := CheckTerminal(KindClaudeCode); err != nil {
		t.Errorf("CheckTerminal(claude-code): %v", err)
	}
	cmd, err := TerminalCommand(KindClaudeCode, true, fixtureTag)
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
	ro, _ := TerminalCommand(KindClaudeCode, true, fixtureTag)
	if ro[len(ro)-1] != TerminalReadOnlyFlag {
		t.Errorf("read-only command %q does not end in %q", ro, TerminalReadOnlyFlag)
	}
	rw, _ := TerminalCommand(KindClaudeCode, false, fixtureTag)
	if slices.Contains(rw, TerminalReadOnlyFlag) {
		t.Errorf("interactive command %q carries %q", rw, TerminalReadOnlyFlag)
	}
	if len(ro) != len(rw)+1 || !slices.Equal(ro[:len(rw)], rw) {
		t.Errorf("read-only must be the interactive command plus the flag: %q vs %q", ro, rw)
	}
}

// TestTheTerminalTagIsTheScriptsFirstArgument: the tag is argv[4] — $1 of the
// script (argv[3] is $0), and the element the release matches as a WHOLE
// entry of /proc/<pid>/cmdline. Pinned exactly, for both modes.
func TestTheTerminalTagIsTheScriptsFirstArgument(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		cmd, err := TerminalCommand(KindClaudeCode, readOnly, fixtureTag)
		if err != nil {
			t.Fatal(err)
		}
		if cmd[3] != "muster-terminal" || cmd[4] != string(fixtureTag) {
			t.Errorf("read-only=%v: argv %q does not carry $0 then the tag", readOnly, cmd)
		}
	}
}

// TestTerminalCommandRefusesAMalformedTag: an attach whose tag the release
// could not match is an attach nothing can end, so it is refused rather than
// started. Each case differs from a valid tag in exactly one way.
func TestTerminalCommandRefusesAMalformedTag(t *testing.T) {
	for _, bad := range []TerminalTag{
		"",
		"0123456789abcdef0123456789abcdef", // no variable
		"MUSTER_TERMINAL_ID=",              // no value
		"MUSTER_TERMINAL_ID=0123456789abcdef0123456789abcde",    // 31 hex
		"MUSTER_TERMINAL_ID=0123456789abcdef0123456789abcdef0",  // 33 hex
		"MUSTER_TERMINAL_ID=0123456789ABCDEF0123456789abcdef",   // upper case
		"OTHER_VARIABLE_ID=0123456789abcdef0123456789abcdef",    // wrong variable
		"MUSTER_TERMINAL_ID=0123456789abcdef0123456789abcdef\n", // trailing newline
	} {
		if _, err := TerminalCommand(KindClaudeCode, false, bad); err == nil {
			t.Errorf("tag %q was accepted", bad)
		}
	}
	if _, err := TerminalCommand(KindClaudeCode, false, fixtureTag); err != nil {
		t.Errorf("control: the well-formed tag was refused: %v", err)
	}
}

// TestNewTerminalTagIsWellFormedAndFresh: a minted tag passes the command's own
// check, and two are never the same.
func TestNewTerminalTagIsWellFormedAndFresh(t *testing.T) {
	a, err := NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two minted tags are equal: %q", a)
	}
	for _, tag := range []TerminalTag{a, b} {
		if _, err := TerminalCommand(KindClaudeCode, false, tag); err != nil {
			t.Errorf("minted tag %q is refused: %v", tag, err)
		}
	}
}

// TestTheReleaseInputIsTheTagAsOneLine: the script reads exactly one line.
func TestTheReleaseInputIsTheTagAsOneLine(t *testing.T) {
	if got := TerminalReleaseInput(fixtureTag); got != string(fixtureTag)+"\n" {
		t.Fatalf("release input %q", got)
	}
	if got := strings.Join(TerminalReleaseCommand()[:2], " "); got != "sh -c" {
		t.Fatalf("release command starts %q, want sh -c", got)
	}
}
