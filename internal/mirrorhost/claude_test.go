package mirrorhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// fakeProc is a /proc tree: <pid>/stat, <pid>/cmdline and <pid>/cwd, enough for
// ClaudeLocator's walk.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc { return &fakeProc{t: t, root: t.TempDir()} }

func (f *fakeProc) add(pid, ppid int, comm, cwd string, argv ...string) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, ppid)
	must(f.t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
	must(f.t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644))
	if cwd != "" {
		must(f.t, os.Symlink(cwd, filepath.Join(dir, "cwd")))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeHome is a ~/.claude with sessions/ and projects/.
type fakeHome struct {
	t    *testing.T
	root string
}

func newFakeHome(t *testing.T) *fakeHome { return &fakeHome{t: t, root: t.TempDir()} }

func (h *fakeHome) session(pid int, id, cwd, status string) {
	must(h.t, os.MkdirAll(filepath.Join(h.root, "sessions"), 0o755))
	must(h.t, os.WriteFile(filepath.Join(h.root, "sessions", fmt.Sprintf("%d.json", pid)),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"status":%q}`, pid, id, cwd, status)), 0o644))
}

func (h *fakeHome) transcript(cwd, id string, mtime time.Time) string {
	dir := filepath.Join(h.root, "projects", ProjectSlug(cwd))
	must(h.t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, id+".jsonl")
	must(h.t, os.WriteFile(p, []byte("{}\n"), 0o644))
	must(h.t, os.Chtimes(p, mtime, mtime))
	return p
}

func TestProjectSlugIsClaudeCodesDirName(t *testing.T) {
	// The rule was checked against a real ~/.claude/projects on a dev host
	// (17 live sessions resolved by `muster-mirror -list -claude`).
	for in, want := range map[string]string{
		"/work/space/my-repo": "-work-space-my-repo",
		"/srv/my.repo_v2":     "-srv-my-repo-v2",
	} {
		if got := ProjectSlug(in); got != want {
			t.Errorf("ProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocateWalksThePaneTreeToClaudeAndReadsItsSessionFile(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/work/repo"
	// pane 100: zsh → nix develop → bash → claude (pid 400, NixOS wrapper name)
	proc.add(100, 1, "zsh", cwd, "-zsh")
	proc.add(200, 100, "nix", cwd, "nix", "develop")
	proc.add(300, 200, "bash", cwd, "bash")
	proc.add(400, 300, ".claude-wrapped", cwd, "/nix/store/x-claude-code/bin/claude", "--continue")
	proc.add(401, 400, "bash", cwd, "bash", "-c", "go test") // a tool the CLI runs
	// pane 500: a plain shell, no claude below it
	proc.add(500, 1, "zsh", cwd, "-zsh")
	proc.add(501, 500, "vim", cwd, "vim", "claude.md")
	// pane 600: another claude in the SAME cwd, an npm install
	proc.add(600, 1, "node", cwd, "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")

	home.session(400, "sess-a", cwd, "busy")
	home.session(600, "sess-b", cwd, "idle")
	now := time.Now()
	pa := home.transcript(cwd, "sess-a", now.Add(-time.Hour)) // OLDER, but it is 400's
	pb := home.transcript(cwd, "sess-b", now)

	got := ClaudeLocator{Proc: proc.root, Home: home.root}.Locate([]pane{
		{Session: "work", ID: "%1", PID: 100},
		{Session: "shell", ID: "%2", PID: 500},
		{Session: "other", ID: "%3", PID: 600},
	})
	if len(got) != 2 {
		t.Fatalf("located %+v, want work and other only", got)
	}
	if w := got["work"]; w.Pane != "%1" || w.PID != 400 || w.Transcript != pa || w.Gen != "sess-a" || !w.Busy || w.Status != mirror.StatusBusy {
		t.Fatalf("work = %+v, want pane %%1 pid 400 %s sess-a busy", w, pa)
	}
	if o := got["other"]; o.PID != 600 || o.Transcript != pb || o.Gen != "sess-b" || o.Busy || o.Status != mirror.StatusIdle {
		t.Fatalf("other = %+v, want pid 600 %s sess-b idle", o, pb)
	}
	if _, ok := got["shell"]; ok {
		t.Fatal("a pane with no Claude Code was located")
	}
}

// With no sessions file (an older CLI) the newest transcript in the process's
// cwd is taken; a sessions file that names a transcript not written yet
// resolves to NOTHING rather than to another session's file.
func TestLocateFallbackAndAFreshSession(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/w"
	proc.add(10, 1, "claude", cwd, "claude")
	now := time.Now()
	home.transcript(cwd, "older", now.Add(-time.Minute))
	newest := home.transcript(cwd, "newest", now)
	loc := ClaudeLocator{Proc: proc.root, Home: home.root}
	got := loc.Locate([]pane{{Session: "s", ID: "%0", PID: 10}})
	if p := got["s"]; p.Transcript != newest || p.Gen != "newest" {
		t.Fatalf("fallback = %+v, want the newest transcript", p)
	}
	// A guessed transcript has no sessions file to read a status from: unknown,
	// never idle (the send queue sends nothing to an unknown pane).
	if p := got["s"]; p.Status != "" || p.Busy {
		t.Fatalf("a guessed pane reports status %q (busy %v); want unknown", p.Status, p.Busy)
	}
	home.session(10, "not-written-yet", cwd, "idle")
	if p, ok := loc.Locate([]pane{{Session: "s", ID: "%0", PID: 10}})["s"]; ok {
		t.Fatalf("a session with no transcript yet resolved to %+v", p)
	}
	// A sessionId that is not an id is never joined into a path.
	home.session(10, "../../etc/passwd", cwd, "idle")
	if p := loc.Locate([]pane{{Session: "s", ID: "%0", PID: 10}})["s"]; p.Gen != "newest" {
		t.Fatalf("a hostile sessionId was used: %+v", p)
	}
}

// The sessions file names the id; when its cwd's slug dir does not hold it
// (the cwd moved), the id is found under any project dir.
func TestLocateFindsTheSessionUnderAnotherProjectDir(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	proc.add(10, 1, "claude", "/now/here", "claude")
	home.session(10, "sid", "/now/here", "idle")
	p := home.transcript("/where/it/started", "sid", time.Now())
	if got := (ClaudeLocator{Proc: proc.root, Home: home.root}).Locate([]pane{{Session: "s", ID: "%0", PID: 10}})["s"]; got.Transcript != p {
		t.Fatalf("got %+v, want %s", got, p)
	}
}

func TestParseActivePanesTakesEachSessionsActivePane(t *testing.T) {
	out := "10\t%1\t111\tmain\n" + // active window, inactive pane
		"11\t%2\t222\tmain\n" + // the active pane
		"11\t%3\t333\tmy session: x\n" + // a name with spaces and a colon
		"11\t%4\t444\tunlisted\n" +
		"01\t%5\t555\tmain\n" + // inactive window
		"garbage\n"
	got := parseActivePanes(out, map[string]bool{"main": true, "my session: x": true})
	want := []pane{{Session: "main", ID: "%2", PID: 222}, {Session: "my session: x", ID: "%3", PID: 333}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

// TestThePaneStatusIsClaudeCodesOwnAndAnythingElseIsUnknown: a pane's Status is
// what the CLI's sessions file says — busy, idle or waiting, each kept as such;
// "shell" (idle with a background shell running, Claude Code 2.1.289) as IDLE —
// and a status this side does not know is never idle: a short word is kept so
// muster can name it, anything else is UNKNOWN (""). The send queue would
// otherwise paste into a session it cannot read — or, for "shell", hold a free
// prompt for as long as the background shell runs.
func TestThePaneStatusIsClaudeCodesOwnAndAnythingElseIsUnknown(t *testing.T) {
	for file, want := range map[string]string{"busy": mirror.StatusBusy, "idle": mirror.StatusIdle,
		"waiting": mirror.StatusWaiting, "shell": mirror.StatusIdle, "compacting": "compacting",
		"Not A Status!": "", "": ""} {
		proc, home := newFakeProc(t), newFakeHome(t)
		const cwd = "/w"
		proc.add(10, 1, "claude", cwd, "claude")
		home.session(10, "sess", cwd, file)
		home.transcript(cwd, "sess", time.Now())
		got, ok := ClaudeLocator{Proc: proc.root, Home: home.root}.Locate([]pane{{Session: "s", ID: "%0", PID: 10}})["s"]
		if !ok || got.Gen != "sess" {
			t.Fatalf("status %q: the pane did not resolve to its session: %+v", file, got)
		}
		if got.Status != want || got.Busy != (want == mirror.StatusBusy) {
			t.Errorf("sessions file status %q → Status %q, Busy %v; want %q", file, got.Status, got.Busy, want)
		}
	}
}
