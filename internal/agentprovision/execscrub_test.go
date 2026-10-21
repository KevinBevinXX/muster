package agentprovision

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
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

// plantVector is one way the agent's session can make git run a command of its
// choosing while muster's job scripts run git in its workspace.
type plantVector struct {
	name string
	// plant arranges the vector in the fixture: r is the job fixture, home the
	// exec's $HOME (the session-writable global config), script the planted
	// command's path.
	plant func(t *testing.T, r worktreetest.Repo, home, script string)
	// mayRun: git offers no way to neutralise this vector by name, so it RUNS —
	// and the exec's `env -u` prefix is the only thing between it and the turn
	// credentials. Every other vector must not run at all.
	mayRun bool
}

func gitIn(t *testing.T, dir string, args ...string) { t.Helper(); worktreetest.Git(t, dir, args...) }

var plantVectors = []plantVector{
	{name: "core.fsmonitor", plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		gitIn(t, r.Checkout, "config", "core.fsmonitor", s)
	}},
	{name: "post-checkout hook (.git/hooks)", plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		hook := filepath.Join(r.Checkout, ".git", "hooks", "post-checkout")
		mustCopyExec(t, s, hook)
	}},
	{name: "config-based hook (hook.<name>.command)", plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		gitIn(t, r.Checkout, "config", "hook.plant.command", s)
		gitIn(t, r.Checkout, "config", "hook.plant.event", "post-checkout")
	}},
	{name: "core.gitProxy (git:// origin, set-head --auto)", plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		gitIn(t, r.Checkout, "config", "core.gitProxy", s)
		gitIn(t, r.Checkout, "remote", "set-url", "origin", "git://127.0.0.1:9/acme/widgets")
		gitIn(t, r.Checkout, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	}},
	{name: "core.alternateRefsCommand (fetch with an alternate)", plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		alt := filepath.Join(r.Checkout, ".git", "objects", "info", "alternates")
		if err := os.WriteFile(alt, []byte(filepath.Join(r.Origin, "objects")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, r.Checkout, "config", "core.alternateRefsCommand", s)
	}},
	{name: "global uploadpack.packObjectsHook (local fetch)", plant: func(t *testing.T, _ worktreetest.Repo, home, s string) {
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
			[]byte("[uploadpack]\n\tpackObjectsHook = "+s+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "filter driver (smudge, .git/info/attributes)", mayRun: true, plant: func(t *testing.T, r worktreetest.Repo, _, s string) {
		attrs := filepath.Join(r.Checkout, ".git", "info", "attributes")
		if err := os.MkdirAll(filepath.Dir(attrs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(attrs, []byte("* filter=plant\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, r.Checkout, "config", "filter.plant.smudge", s)
	}},
}

func mustCopyExec(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// runJobScripts runs every job operation that runs git in the agent's
// workspace — prepare the job's worktree, remove an untracked result file,
// remove the worktree — ignoring their results (a plant may make one fail; what
// is asserted is what ran).
func runJobScripts(a *Adapter, r worktreetest.Repo) {
	_, _ = prepareJob(a, r, r.Commit)
	_, _ = a.RemoveUntrackedWorkspaceFile(context.Background(), workspaceAgent, "nothing-here.md")
	_ = a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID)
}

// 🔴 TestTheJobScriptsRunNoPlantedCommandAndShowNoneATurnCredential, one row per
// vector: the session plants a command in its workspace's git config (or its
// global config); muster's job scripts then run git there with the CONTAINER's
// environment (the local driver's env holds sentinel turn credentials, and
// $HOME is the plant's). Every vector git lets muster neutralise by name must
// not run at all; the one it does not (a filter driver) runs — asserted, so the
// credential check below is REACHED — and sees no turn credential, because the
// exec's `env -u` prefix removed them.
//
// THE CONTROL, PER VECTOR: the same plant, with both layers removed from the
// command the driver runs (the `env -u` prefix and the GIT_CONFIG_* export),
// RUNS and SEES the credentials — so every "did not run" below is the
// neutralisation, not a plant this git ignores. A vector this git cannot fire
// at all (its control does not run either) is reported as such and skipped.
func TestTheJobScriptsRunNoPlantedCommandAndShowNoneATurnCredential(t *testing.T) {
	out, _ := exec.Command("git", "--version").Output()
	t.Logf("%s", strings.TrimSpace(string(out)))
	reached := 0
	for _, v := range plantVectors {
		t.Run(v.name, func(t *testing.T) {
			outcome := func(unlayered bool) (ran bool, seen []string) {
				a, d := newWorkspaceAdapter(t)
				home := t.TempDir()
				d.env = append(containerEnv(), "HOME="+home)
				d.unlayered = unlayered
				r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
				dump := filepath.Join(t.TempDir(), "plant.env")
				script := filepath.Join(t.TempDir(), "plant.sh")
				// cat: a smudge filter must pass its input through; for every other
				// vector stdin is empty or ignored.
				if err := os.WriteFile(script, []byte("#!/bin/sh\nenv >> '"+dump+"'\ncat\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				v.plant(t, r, home, script)
				runJobScripts(a, r)
				b, err := os.ReadFile(dump)
				if err != nil {
					return false, nil
				}
				return true, sawTurnCredential(string(b))
			}
			cRan, cSeen := outcome(true)
			if !cRan {
				t.Logf("PLANT TABLE | %-52s | control: does not fire on this git | -", v.name)
				return
			}
			if len(cSeen) != len(containerTurnCredentials) {
				t.Fatalf("control: the unlayered plant ran but saw %v, not every credential; the env is not the container's", cSeen)
			}
			ran, seen := outcome(false)
			t.Logf("PLANT TABLE | %-52s | control: ran, saw %d | with both layers: ran=%v saw=%v", v.name, len(cSeen), ran, seen)
			if len(seen) > 0 {
				t.Errorf("the planted %s saw %v", v.name, seen)
			}
			switch {
			case v.mayRun && !ran:
				t.Errorf("instrument check: the %s did not run, so the credential check above was never reached", v.name)
			case v.mayRun:
				reached++
			case ran:
				t.Errorf("the planted %s RAN during muster's job scripts; git lets it be neutralised by name", v.name)
			}
		})
	}
	if reached == 0 {
		t.Error("instrument check: no vector that runs was observed, so no credential assertion was reached")
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
