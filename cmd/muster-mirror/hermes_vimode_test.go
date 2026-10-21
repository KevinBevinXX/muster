package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestListHermesSaysWhetherViModeBlocksSending: -list -hermes prints the
// vi-mode line from config.yaml's display section, both values, and never
// any other key of the file (listHermes fails on the inline secret).
//
// 🔴 The fake backend is not optional: without it -list -hermes lists the
// windows through the REAL tmux — the operator's own server where tmux
// exists, and an exec failure in the nix sandbox, which has none.
func TestListHermesSaysWhetherViModeBlocksSending(t *testing.T) {
	withBackend(t, hermesListBackend{})
	for _, c := range []struct{ display, want string }{
		{"display:\n  vim_mode: true\n", "vi-mode\ton (display.vim_mode): muster never sends to this Hermes, -hermes-send or not\n"},
		{"display:\n  vim_mode: false\n", "vi-mode\toff\n"},
		{"", "vi-mode\toff\n"},
	} {
		home := t.TempDir()
		hermesConfig(t, home, binX)
		f, err := os.OpenFile(filepath.Join(home, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0)
		must(t, err)
		_, _ = f.WriteString(c.display)
		_ = f.Close()
		if out := listHermes(t, home, nil); !strings.Contains(out, c.want) {
			t.Errorf("display %q: -list -hermes printed\n%s\nwant a line %q", c.display, out, c.want)
		}
	}
}
