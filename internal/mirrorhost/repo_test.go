package mirrorhost

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// repoTree is a directory tree for the repo-name tests: a repository with a
// subdirectory, a git WORKTREE (whose .git is a file), and a directory outside
// any repository. Every name is distinct, so a lookup that answered the wrong
// level cannot pass.
func repoTree(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"src/fixture-repo/.git", "src/fixture-repo/cmd/tool", "src/plain-dir/inner", "wt/fixture-worktree"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "wt/fixture-worktree/.git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRepoNameIsTheGitTopLevelElseTheCwdsOwnName: the four cases the chip
// names — in a repository, in a subdirectory of one, outside git, and a deleted
// cwd — plus a worktree and the inputs that name nothing.
func TestRepoNameIsTheGitTopLevelElseTheCwdsOwnName(t *testing.T) {
	root := repoTree(t)
	for _, c := range []struct{ cwd, want string }{
		{filepath.Join(root, "src/fixture-repo"), "fixture-repo"},
		{filepath.Join(root, "src/fixture-repo/cmd/tool"), "fixture-repo"},
		{filepath.Join(root, "src/fixture-repo/cmd/tool") + "/", "fixture-repo"},
		{filepath.Join(root, "wt/fixture-worktree"), "fixture-worktree"},
		// Outside git: the cwd's own name, not its parent's.
		{filepath.Join(root, "src/plain-dir/inner"), "inner"},
		// A deleted cwd, as Linux's /proc link spells it: inside a repository it
		// still names the repository; outside one, its own name.
		{filepath.Join(root, "src/fixture-repo/cmd/gone") + deletedSuffix, "fixture-repo"},
		{filepath.Join(root, "src/plain-dir/gone-too") + deletedSuffix, "gone-too"},
		{"", ""},
		{"relative/dir", ""},
		{"/", ""},
	} {
		if got := repoName(c.cwd, os.Stat); got != c.want {
			t.Errorf("repoName(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
	// A name that is not one (control bytes) is cleaned; one past MaxRepoName is cut.
	long := filepath.Join(root, strings.Repeat("x", mirror.MaxRepoName+20))
	if err := os.Mkdir(long, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := repoName(long, os.Stat); len(got) > mirror.MaxRepoName || !strings.HasPrefix(got, "xxxx") {
		t.Errorf("a %d-byte directory name came out as %q (%d bytes), want at most %d", mirror.MaxRepoName+20, got, len(got), mirror.MaxRepoName)
	}
}

// TestTheRepoLookupIsCachedPerCwd: eleven windows (ten Claude, one shell) in
// two working directories, read twice. The first read stats each directory's levels ONCE
// (two cwds, not ten windows); the second stats NOTHING; past repoCacheTTL the
// cwds are looked up again. No git process exists to count: the lookup is
// stats alone.
func TestTheRepoLookupIsCachedPerCwd(t *testing.T) {
	root := repoTree(t)
	inRepo, outside := filepath.Join(root, "src/fixture-repo/cmd/tool"), filepath.Join(root, "src/plain-dir/inner")
	stats := 0
	now := time.Unix(1791903000, 0)
	r := &repoNamer{stat: func(p string) (os.FileInfo, error) { stats++; return os.Stat(p) }, now: func() time.Time { return now }}
	var windows []WindowInfo
	for i := 0; i < 10; i++ {
		cwd := inRepo
		if i%2 == 1 {
			cwd = outside
		}
		windows = append(windows, WindowInfo{Session: "s", ID: "@" + strconv.Itoa(i+1), Claude: &ClaudePane{PID: 1}, Cwd: cwd})
	}
	// A plain shell window is named too (every window has a chip), from the
	// same cache.
	windows = append(windows, WindowInfo{Session: "s", ID: "@99", Cwd: inRepo})

	first := r.windowRepos(windows)
	// The repo cwd stops at its 3rd level (tool, cmd, fixture-repo); the outside
	// cwd walks every level to the root.
	levels := strings.Count(filepath.Clean(outside), "/") + 1
	if want := 3 + levels; stats != want {
		t.Fatalf("the first read made %d stats, want %d (each cwd's levels once, not once per window)", stats, want)
	}
	if len(first) != 11 || first["@1"] != "fixture-repo" || first["@2"] != "inner" || first["@99"] != "fixture-repo" {
		t.Fatalf("windowRepos = %v", first)
	}
	stats = 0
	for tick := 0; tick < 5; tick++ {
		if again := r.windowRepos(windows); !maps.Equal(again, first) {
			t.Fatalf("tick %d answered %v, want %v", tick, again, first)
		}
	}
	if stats != 0 {
		t.Fatalf("five more reads of the same cwds made %d stats, want 0 (cached)", stats)
	}
	now = now.Add(repoCacheTTL)
	r.windowRepos(windows)
	if stats == 0 {
		t.Fatal("past repoCacheTTL nothing was looked up again: a `git init` would never show")
	}
}

// TestAPerWindowHostSendsItsReposAfterItsWindows: the host's repos message —
// after the hello that listed the windows, one entry per window with a cwd (a
// shell's as much as Claude Code's), sent again only when a name CHANGES.
func TestAPerWindowHostSendsItsReposAfterItsWindows(t *testing.T) {
	root := repoTree(t)
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: []WindowInfo{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Pane: "%21", PanePID: 1, Command: "claude",
			Claude: &ClaudePane{Pane: "%21", PID: 9, Gen: "sess-a"}, Cwd: filepath.Join(root, "src/fixture-repo/cmd")},
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 2, Command: "zsh", Cwd: filepath.Join(root, "src/plain-dir")},
	}}
	d.run(t, b, nil, false)
	waitUntil(t, "the first repos message", func() bool { return len(d.of(mirror.TypeRepos)) == 1 })
	if got := d.of(mirror.TypeRepos)[0].Repos; !maps.Equal(got, map[string]string{"@7": "fixture-repo", "@4": "plain-dir"}) {
		t.Fatalf("repos = %v, want the Claude window's AND the shell's", got)
	}
	d.mu.Lock()
	if d.msgs[0].Type != mirror.TypeHello || d.msgs[0].Windows == nil {
		d.mu.Unlock()
		t.Fatalf("the first message was %q, want the hello listing the windows", d.msgs[0].Type)
	}
	d.mu.Unlock()
	// Many refreshes with nothing changed: no second repos message.
	time.Sleep(150 * time.Millisecond)
	if n := len(d.of(mirror.TypeRepos)); n != 1 {
		t.Fatalf("%d repos messages with nothing changed, want 1", n)
	}
	// The window's Claude moves to another directory: the map is sent again.
	b.mu.Lock()
	w := slices.Clone(b.windows)
	b.mu.Unlock()
	w[0].Cwd = filepath.Join(root, "wt/fixture-worktree")
	b.set(w...)
	waitUntil(t, "the second repos message", func() bool { return len(d.of(mirror.TypeRepos)) == 2 })
	if got := d.of(mirror.TypeRepos)[1].Repos; !maps.Equal(got, map[string]string{"@7": "fixture-worktree", "@4": "plain-dir"}) {
		t.Fatalf("repos after the move = %v", got)
	}
}

// TestTheLargestReposMessageFitsOneFrame is the size behind sending the names
// on a message of their own: MaxWindows entries — EVERY window named, shells
// as much as Claude Code windows — the longest window id, a name
// at MaxRepoName of bytes JSON writes six-fold. It must fit one frame. The
// window list could not have carried it: its own worst case
// (TestTheLargestWindowListFitsOneFrame) plus even the field's NAME per window
// leaves no room for a name of any useful length — that arithmetic is checked
// here too, as the control that the separate message is needed.
func TestTheLargestReposMessageFitsOneFrame(t *testing.T) {
	var windows []mirror.Window
	repos := map[string]string{}
	for i := 0; i < mirror.MaxWindows; i++ {
		id := "@" + strconv.Itoa(9_999_999_999-i)
		windows = append(windows, mirror.Window{Session: "s", ID: id, Index: i})
		repos[id] = strings.Repeat("<", mirror.MaxRepoName)
	}
	kept := mirror.NormalizeRepos(repos, mirror.NormalizeWindows(windows, 0))
	if len(kept) != mirror.MaxWindows {
		t.Fatalf("instrument check: %d names kept, want %d", len(kept), mirror.MaxWindows)
	}
	for _, v := range kept {
		if len(v) != mirror.MaxRepoName {
			t.Fatalf("instrument check: a name was cut to %d bytes, so this is not the worst case", len(v))
		}
	}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypeRepos, Repos: kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest repos message is %d bytes, over MaxFrame (%d)", len(b), mirror.MaxFrame)
	}
	// The window list's measured worst case (TestTheLargestWindowListFitsOneFrame's
	// log): inline, the per-window cost of the same name would overrun it.
	const windowListWorst = 1_030_215
	perName := len(`,"repo":""`) + 6*mirror.MaxRepoName
	if windowListWorst+mirror.MaxWindows*perName <= mirror.MaxFrame {
		t.Fatalf("instrument check: a name per window would have fitted the window list — the separate message is not needed")
	}
	t.Logf("largest repos message: %d names in %d bytes (%.0f%% of MaxFrame); inline it would have made the window list %d bytes",
		len(kept), len(b), 100*float64(len(b))/float64(mirror.MaxFrame), windowListWorst+mirror.MaxWindows*perName)
}

// TestTmuxReadsEveryWindowsWorkingDirectory: the real backend's listing gives
// EVERY window its working directory — a plain shell's from its pane's root
// process (/proc/<pane pid>/cwd), with no Claude Code anywhere — and a window
// whose process has no readable cwd gets none. The listing is one tmux call
// (a script here); the cwds are readlinks of the fake /proc.
func TestTmuxReadsEveryWindowsWorkingDirectory(t *testing.T) {
	root := repoTree(t)
	proc := newFakeProc(t)
	proc.add(300, 1, "zsh", filepath.Join(root, "src/fixture-repo/cmd/tool"), "-zsh")
	proc.add(400, 1, "vim", "", "vim") // no cwd link
	bin := filepath.Join(t.TempDir(), "tmux")
	out := "1\t@3\t1\t1\t%3\t300\t1791903000\t1791903000\tzsh\t/dev/pts/3\t\t\tshells\n" +
		"1\t@4\t2\t0\t%4\t400\t1791903000\t1791903000\tvim\t/dev/pts/4\t\t\tshells\n"
	listing := filepath.Join(t.TempDir(), "listing")
	must(t, os.WriteFile(listing, []byte(out), 0o644))
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\ncat '"+listing+"'\n"), 0o755))
	ws, err := Tmux{Bin: bin, Claude: ClaudeLocator{Proc: proc.root, Home: t.TempDir()}}.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Claude != nil || ws[1].Claude != nil {
		t.Fatalf("instrument check: want two plain windows, got %+v", ws)
	}
	if ws[0].Cwd != filepath.Join(root, "src/fixture-repo/cmd/tool") || ws[1].Cwd != "" {
		t.Fatalf("cwds = %q, %q; want the shell's, and none for the unreadable one", ws[0].Cwd, ws[1].Cwd)
	}
	if got := (&repoNamer{}).windowRepos(ws); !maps.Equal(got, map[string]string{"@3": "fixture-repo"}) {
		t.Fatalf("windowRepos = %v, want the shell window named by its repository", got)
	}
}

// TestAClaudeWindowIsNamedByItsClaudeProcess: a Claude Code window's cwd is its
// Claude process's (where the session works), not the shell's around it.
func TestAClaudeWindowIsNamedByItsClaudeProcess(t *testing.T) {
	if pid := windowCwdPID(WindowInfo{PanePID: 10, Claude: &ClaudePane{PID: 11}}); pid != 11 {
		t.Fatalf("a Claude window reads pid %d's cwd, want its Claude process 11", pid)
	}
	if pid := windowCwdPID(WindowInfo{PanePID: 10}); pid != 10 {
		t.Fatalf("a shell window reads pid %d's cwd, want its pane's 10", pid)
	}
}
