package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// TestTheColourOptionIsFixedAndNothingIsSplicedIn: the tmux listing reads the
// identity colour from `@muster-color` and nothing else — observed in the
// format it passes to tmux (a script here, never the operator's server). No
// environment variable names another option, and there is no flag for one:
// no value from outside the binary is ever spliced into a tmux format.
func TestTheColourOptionIsFixedAndNothingIsSplicedIn(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> '"+args+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	newTmuxBackend = func(s, sp, dir string, h *mirrorhost.HermesSource) mirrorhost.Backend {
		return mirrorhost.Tmux{Bin: bin, Claude: mirrorhost.ClaudeLocator{Proc: t.TempDir(), Home: t.TempDir()}}
	}
	if rc, _, errs := runWith(t, map[string]string{"MUSTER_MIRROR_COLOR_OPTION": "@from-env"}, "-list", "-windows"); rc != 0 {
		t.Fatalf("-list -windows: rc %d %s", rc, errs)
	}
	b, _ := os.ReadFile(args)
	if got := string(b); !strings.Contains(got, ";E:@muster-color}") || !strings.Contains(got, "/:@muster-color}") || strings.Contains(got, "from-env") {
		t.Fatalf("the listing's format %q: want @muster-color, and nothing from the environment", got)
	}
	if rc, _, errs := runWith(t, nil, "-list", "-windows", "-color-option", "@from-flag"); rc != 2 || !strings.Contains(errs, "flag provided but not defined: -color-option") {
		t.Fatalf("-color-option: rc %d %q, want 2 and an unknown flag", rc, errs)
	}
}
