package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheTerminalAttachesToCcdsSession pins muster's terminal attach to the
// tmux session and socket ccd actually uses: the same variables, with the
// same defaults, as ccd and as the image's entrypoint. The attach command is
// built in internal/agents; ccd is a separate binary, so nothing else connects
// the two, and a renamed default would leave every terminal attaching to a
// session that does not exist.
//
// ⚠ A RELATIONSHIP BETWEEN LITERALS, NOT BEHAVIOUR. That the command really
// attaches — read-only and interactive, on a named and on the default socket —
// is the tmuxit suite's TestTmuxTerminalAttach* against real tmux.
func TestTheTerminalAttachesToCcdsSession(t *testing.T) {
	tag, err := agents.NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := agents.TerminalCommand(agents.KindClaudeCode, true, tag)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmd, " ")
	for _, want := range []string{
		`"=${CCD_TMUX_TARGET:-` + defaultTmuxTarget + `}"`,
		// ccd's default socket is no -L at all; tmux's own default name is
		// "default", so -L default is the same server.
		`-L "${CCD_TMUX_SOCKET:-default}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the attach command does not contain %s:\n%s", want, script)
		}
	}
	entry, err := os.ReadFile(filepath.Join("..", "..", "images", "claude-code-agent", "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `CCD_TMUX_TARGET="${CCD_TMUX_TARGET:-` + defaultTmuxTarget + `}"`; !strings.Contains(string(entry), want) {
		t.Errorf("entrypoint.sh no longer defaults the session as %s", want)
	}
}
