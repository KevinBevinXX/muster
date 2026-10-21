package agentprovision

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

// containerTurnCredentials is what a claude-code container's environment holds
// that drives a turn API (ccd's, or the gateway sidecar's), as sentinel values:
// every exec muster runs into the container inherits that environment unless
// muster strips it.
var containerTurnCredentials = map[string]string{
	agentspec.EnvGatewayBearer:            "sentinel-turn-bearer-4c1e9a",
	agentspec.EnvCCDGatewayUpstreamBearer: "sentinel-sidecar-bearer-77b0d2",
	agentspec.EnvGatewayToken:             "sentinel-hooks-token-1fa83e",
}

func containerEnv() []string {
	out := []string{}
	for k, v := range containerTurnCredentials {
		out = append(out, k+"="+v)
	}
	return out
}

// sawTurnCredential names every turn credential (by name or by value) in an
// `env` dump.
func sawTurnCredential(dump string) []string {
	var seen []string
	for name, v := range containerTurnCredentials {
		if strings.Contains(dump, name+"=") || strings.Contains(dump, v) {
			seen = append(seen, name)
		}
	}
	return seen
}

// plantScript writes an executable shell script at path that appends its
// environment to dump.
func plantScript(t *testing.T, path, dump string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nenv >> '"+dump+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 🔴 TestAJobCheckoutRunsNoPlantedCommandAndShowsItNoTurnCredential: a tool
// call in the agent's session can write its workspace's .git — a
// core.fsmonitor command, a post-checkout hook — and the next job checkout
// (muster's exec: git status, git checkout) would run it with the CONTAINER's
// environment, turn credentials included. With the container's credentials set
// on the exec (the local driver's env), neither planted command runs during the
// checkout, and nothing the checkout's processes could see carries them.
//
// The control: the same plants DO fire, and DO see the credentials, when git
// runs them directly in that environment — so "nothing ran" below is the
// neutralisation, not a plant this git ignores.
func TestAJobCheckoutRunsNoPlantedCommandAndShowsItNoTurnCredential(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	d.env = containerEnv()
	git, repo, first, second := newCheckoutRepo(t, d)
	dumps := t.TempDir()
	fsmon, hook := filepath.Join(dumps, "fsmonitor.env"), filepath.Join(dumps, "post-checkout.env")
	plantScript(t, filepath.Join(dumps, "fsmonitor.sh"), fsmon)
	plantScript(t, filepath.Join(repo, ".git", "hooks", "post-checkout"), hook)
	git("config", "core.fsmonitor", filepath.Join(dumps, "fsmonitor.sh"))

	// Control: the plants fire, with the container's environment.
	for _, args := range [][]string{{"status", "--porcelain"}, {"checkout", "--quiet", "--detach", first}, {"checkout", "--quiet", "--detach", second}} {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), d.env...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("control: git %v: %v: %s", args, err, out)
		}
	}
	for _, f := range []string{fsmon, hook} {
		b, err := os.ReadFile(f)
		if err != nil || len(sawTurnCredential(string(b))) != len(containerTurnCredentials) {
			t.Fatalf("control: %s did not fire with the container's credentials (%v); the test would prove nothing",
				filepath.Base(f), err)
		}
		_ = os.Remove(f)
	}

	if err := a.CheckoutCommit(context.Background(), workspaceAgent, first); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if head := git("rev-parse", "HEAD"); head != first {
		t.Fatalf("control: HEAD = %s, want %s (the checkout did not move)", head, first)
	}
	for _, f := range []string{fsmon, hook} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // did not run: neutralised
		}
		t.Errorf("the planted %s RAN during the job checkout", strings.TrimSuffix(filepath.Base(f), ".env"))
		if seen := sawTurnCredential(string(b)); len(seen) > 0 {
			t.Errorf("the planted %s saw %v", strings.TrimSuffix(filepath.Base(f), ".env"), seen)
		}
	}
}

// 🔴 TestATerminalAttachHoldsNoTurnCredential: the terminal exec's process —
// the `tmux attach` client that stays alive, dumpable, for as long as the
// terminal is open, readable by any same-uid tool call at /proc/<pid>/environ —
// is started with none of the container's turn credentials. A fake `tmux` on
// PATH stands in for the real one (never the operator's): it records its
// environment and exits.
func TestATerminalAttachHoldsNoTurnCredential(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	bin, dumps := t.TempDir(), t.TempDir()
	dump := filepath.Join(dumps, "tmux.env")
	if err := os.WriteFile(filepath.Join(bin, "tmux"),
		[]byte("#!/bin/sh\necho \"ARGS: $*\" >> '"+dump+"'\nenv >> '"+dump+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.env = append(containerEnv(), "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+t.TempDir())

	if err := a.AttachTerminal(context.Background(), workspaceAgent, false, strings.NewReader(""), io.Discard, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	b, err := os.ReadFile(dump)
	if err != nil || !strings.Contains(string(b), "attach-session") || !strings.Contains(string(b), "MUSTER_TERMINAL_ID=") {
		t.Fatalf("instrument check: the fake tmux never ran as the attach client (%v):\n%s", err, b)
	}
	if seen := sawTurnCredential(string(b)); len(seen) > 0 {
		t.Errorf("the terminal's tmux processes hold %v", seen)
	}
}
