package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

func runWith(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := run(args, func(k string) string { return env[k] }, &out, &errb)
	return rc, out.String(), errb.String()
}

// TestTheTokenNeverHasToBeAnArgument: there is no -token flag (argv is visible
// to every process on the host); the token comes from a file or the environment.
func TestTheTokenNeverHasToBeAnArgument(t *testing.T) {
	if rc, _, errs := runWith(t, nil, "-token", "mmt_x"); rc != 2 || !strings.Contains(errs, "flag provided but not defined: -token") {
		t.Fatalf("-token was accepted (rc %d): %s", rc, errs)
	}
}

// TestMissingURLOrTokenIsAUsageError, and a non-mirror token or a cleartext URL
// is refused before anything dials.
func TestMissingURLOrTokenIsAUsageError(t *testing.T) {
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example"}); rc != 2 || !strings.Contains(errs, "are required") {
		t.Fatalf("no token: rc %d %s", rc, errs)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	if err := os.WriteFile(f, []byte("hook-token-not-a-mirror-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example", envHost: "laptop"}, "-token-file", f); rc != 2 || !strings.Contains(errs, "not a mirror token") {
		t.Fatalf("hook token in the file: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "http://192.0.2.1:30306", envToken: "mmt_x", envHost: "laptop"}); rc != 2 || !strings.Contains(errs, "cleartext") {
		t.Fatalf("http without -allow-http: rc %d %s", rc, errs)
	}
}

// listBackend is a host's tmux for -list: fixed sessions and active windows.
type listBackend struct{}

func (listBackend) List(context.Context) ([]string, error) {
	return []string{"0", "infra-work", "scratch13", "scratch2"}, nil
}
func (listBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	return nil, errors.New("not in -list")
}
func (listBackend) ActiveWindows(context.Context) (map[string]string, error) {
	return map[string]string{"scratch13": "1", "scratch2": "8", "infra-work": "2", "0": "0"}, nil
}

func withListBackend(t *testing.T) {
	t.Helper()
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return listBackend{} }
	t.Cleanup(func() { e2eBackend = prev })
}

// TestListShowsTheLabelAsALastColumn: -list prints what would be registered,
// and a labelled session's line ends in `label=<codename>:<window>`; the
// operator's own examples. Sessions with no codename print as before.
func TestListShowsTheLabelAsALastColumn(t *testing.T) {
	withListBackend(t)
	f := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(f, []byte(`{"scratch13":"mango","scratch2":"Gold"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envLabelsFile: f}, "-list")
	if rc != 0 {
		t.Fatalf("-list: rc %d: %s", rc, errs)
	}
	if want := "0\ninfra-work\nscratch13\tlabel=mango:1\nscratch2\tlabel=Gold:8\n"; out != want {
		t.Fatalf("-list printed %q, want %q", out, want)
	}
}

// TestABadLabelsFileIsNeverFatal: a missing or malformed labels file is logged
// and the host runs exactly as with none — -list still succeeds, unlabelled.
func TestABadLabelsFileIsNeverFatal(t *testing.T) {
	withListBackend(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`["not","an","object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(t.TempDir(), "absent.json"), bad} {
		rc, out, errs := runWith(t, nil, "-list", "-labels-file", f)
		if rc != 0 || out != "0\ninfra-work\nscratch13\nscratch2\n" {
			t.Errorf("%s: rc %d, out %q, want 0 and the raw list", f, rc, out)
		}
		if strings.Count(errs, "no session labels") != 1 {
			t.Errorf("%s: want the failure logged exactly once, got: %s", f, errs)
		}
	}
}

// windowsListBackend is a per-window host: one session, three windows — two
// running Claude Code (resolved two different ways), one a shell.
type windowsListBackend struct{ listBackend }

func (windowsListBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{
		{Session: "scratch13", ID: "@46", Index: 1, Active: true, Pane: "%46", Command: "claude",
			Claude: &mirrorhost.ClaudePane{Pane: "%46", Gen: "sess-b", Transcript: "/p/sess-b.jsonl", Busy: true, Source: mirrorhost.SourcePaneMap}},
		{Session: "scratch13", ID: "@47", Index: 2, Pane: "%47", Command: "claude",
			Claude: &mirrorhost.ClaudePane{Pane: "%47", Gen: "sess-c", Transcript: "/p/sess-c.jsonl", Source: mirrorhost.SourceSessions}},
		{Session: "scratch13", ID: "@48", Index: 3, Pane: "%48", Command: "zsh"},
	}, nil
}

// TestListClaudeIsOneRowPerClaudeWINDOW: two Claude Code windows of ONE session
// are two rows, each with its own window id, pane, session id, transcript and
// how it was resolved; the shell window is not listed. (It printed one row per
// SESSION — the active window's — which hid the window being debugged.)
func TestListClaudeIsOneRowPerClaudeWINDOW(t *testing.T) {
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return windowsListBackend{} }
	t.Cleanup(func() { e2eBackend = prev })
	f := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(f, []byte(`{"scratch13":"mango"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envLabelsFile: f}, "-list", "-claude")
	if rc != 0 {
		t.Fatalf("-list -claude: rc %d: %s", rc, errs)
	}
	want := "scratch13\t@46\t1\tclaude\t%46\tbusy\tsess-b\t/p/sess-b.jsonl\tpane-map\tlabel=mango:1\n" +
		"scratch13\t@47\t2\tclaude\t%47\tidle\tsess-c\t/p/sess-c.jsonl\tsessions\tlabel=mango:2\n"
	if out != want {
		t.Fatalf("-list -claude printed\n%q\nwant\n%q", out, want)
	}
}

// TestThePaneMapDirReachesTheTmuxBackend: the flag/env is not decoration — the
// backend the host runs reads the map from it.
func TestThePaneMapDirReachesTheTmuxBackend(t *testing.T) {
	b, ok := tmuxBackend("s", "", "/fx/ledger").(mirrorhost.Tmux)
	if !ok || b.Claude.PaneMapDir != "/fx/ledger" || b.Socket != "s" {
		t.Fatalf("tmuxBackend = %+v, want PaneMapDir /fx/ledger", b)
	}
	if b := tmuxBackend("", "", "").(mirrorhost.Tmux); b.Claude.PaneMapDir != "" {
		t.Fatalf("no pane map dir configured, yet the backend has %q", b.Claude.PaneMapDir)
	}
	// And run passes it: from the environment, and from the flag (which wins).
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	var got []string
	newTmuxBackend = func(s, sp, dir string) mirrorhost.Backend {
		got = append(got, dir)
		return listBackend{}
	}
	if rc, _, errs := runWith(t, map[string]string{envPaneMapDir: "/fx/from-env"}, "-list"); rc != 0 {
		t.Fatalf("-list: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, map[string]string{envPaneMapDir: "/fx/from-env"}, "-list", "-pane-map-dir", "/fx/from-flag"); rc != 0 {
		t.Fatalf("-list -pane-map-dir: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, nil, "-list"); rc != 0 {
		t.Fatalf("-list: rc %d %s", rc, errs)
	}
	if strings.Join(got, "|") != "/fx/from-env|/fx/from-flag|" {
		t.Fatalf("run passed pane map dirs %q, want env, flag, none", got)
	}
}

func TestVersion(t *testing.T) {
	if rc, out, _ := runWith(t, nil, "-version"); rc != 0 || !strings.Contains(out, "tunnel protocol 2") {
		t.Fatalf("-version: rc %d %q", rc, out)
	}
}

// TestCatalogPrintsWhatTheHostWouldReport: -catalog prints the composer menu's
// catalog for a pane in the current directory — the user's skills (through
// CLAUDE_CONFIG_DIR), and the snippets from -snippets-file / its variable —
// without dialling anything.
func TestCatalogPrintsWhatTheHostWouldReport(t *testing.T) {
	cfg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg, "skills", "printed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "skills", "printed", "SKILL.md"), []byte("---\nname: printed\ndescription: shown by -catalog\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snip := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(snip, []byte(`[{"trigger":":pc","label":"Printed snippet","body":"b"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, args := range [][]string{{"-catalog", "-snippets-file", snip}, {"-catalog"}} {
		env := map[string]string{}
		if len(args) == 1 {
			env[envSnippets] = snip
		}
		rc, out, errs := runWith(t, env, args...)
		if rc != 0 {
			t.Fatalf("%v: rc %d %s", args, rc, errs)
		}
		if !strings.Contains(out, "user\tskill\t/printed\tshown by -catalog\n") || !strings.Contains(out, "snippet\t:pc\tPrinted snippet\n") {
			t.Fatalf("%v printed:\n%s", args, out)
		}
	}
}
