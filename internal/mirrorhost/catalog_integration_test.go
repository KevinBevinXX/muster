//go:build tmuxit

package mirrorhost

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTmuxWindowCwdIsThePanesWorkingDirectory: against REAL tmux on a private
// socket, WindowCwd answers the working directory of the window's pane — each
// window its own, the active one for window "" — which is what the composer
// menu's PROJECT commands are found from. Two windows in two distinct
// directories, so an answer read from the wrong window shows.
func TestTmuxWindowCwdIsThePanesWorkingDirectory(t *testing.T) {
	r := newTmuxRig(t)
	a, b := t.TempDir(), t.TempDir()
	// A symlink-free, absolute path is what /proc reports.
	a, _ = filepath.EvalSymlinks(a)
	b, _ = filepath.EvalSymlinks(b)
	r.run("new-session", "-d", "-s", "cwd", "-x", "80", "-y", "24", "-c", a, "sh", "-c", "exec cat")
	other := strings.TrimSpace(r.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=cwd:", "-c", b, "sh", "-c", "exec cat"))
	first := strings.TrimSpace(r.run("display-message", "-p", "-t", "=cwd:0", "#{window_id}"))
	tm := Tmux{Socket: r.sock}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range []struct{ window, want string }{{first, a}, {other, b}, {"", a}} {
		got, err := tm.WindowCwd(ctx, "cwd", c.window)
		if err != nil {
			t.Fatalf("WindowCwd(%q): %v", c.window, err)
		}
		if got != c.want {
			t.Fatalf("WindowCwd(%q) = %q, want %q", c.window, got, c.want)
		}
	}
	if _, err := tm.WindowCwd(ctx, "cwd", "@999999"); err == nil {
		t.Fatal("a window that does not exist answered a directory")
	}
}
