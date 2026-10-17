package mirrorhost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE CATALOG SCAN (catalog.go) against fixture trees: a Claude Code config
// directory, a home directory, a repository with a project .claude, plugins and
// a snippets file. Every name and description below is distinct from every
// other, so an entry read from the wrong place, or under the wrong name, shows.

// write writes body to path, creating its directories.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skill(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# body\n"
}

// fixture is a home with a config dir, a repository under it, and a scanner
// over them.
type fixture struct {
	homeDir, cfg, repo, sub string
	s                       *CatalogScanner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{homeDir: home, cfg: filepath.Join(home, ".claude"), repo: filepath.Join(home, "work", "repo")}
	f.sub = filepath.Join(f.repo, "pkg", "deep")
	if err := os.MkdirAll(filepath.Join(f.repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.s = &CatalogScanner{Config: f.cfg, Home: home, SnippetsFile: filepath.Join(home, "snippets.json")}
	return f
}

// byName is the catalog's commands by name.
func byName(c mirror.Catalog) map[string]mirror.CatalogEntry {
	out := map[string]mirror.CatalogEntry{}
	for _, e := range c.Commands {
		out[e.Name] = e
	}
	return out
}

func names(c mirror.Catalog) []string {
	var out []string
	for _, e := range c.Commands {
		out = append(out, e.Source+":"+e.Kind+":"+e.Name)
	}
	return out
}

// TestTheUserSourcesAreReadByTheirFrontMatter: the user's skills and commands
// (a sub-directory command named "<dir>:<name>"), each described by its front
// matter, or — a command with none — by its first line; a skill marked
// user-invocable: false is not offered.
func TestTheUserSourcesAreReadByTheirFrontMatter(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.cfg, "skills", "handoff-dir", "SKILL.md"), skill("handoff", "Write the handoff doc"))
	write(t, filepath.Join(f.cfg, "skills", "nameless", "SKILL.md"), "---\ndescription: \"A skill named by its \\\"directory\\\"\"\n---\n")
	write(t, filepath.Join(f.cfg, "skills", "hidden", "SKILL.md"), "---\nname: hidden\ndescription: never offered\nuser-invocable: false\n---\n")
	write(t, filepath.Join(f.cfg, "skills", "nofile", "README.md"), "not a skill")
	write(t, filepath.Join(f.cfg, "commands", "deploy.md"), "---\ndescription: 'Ship it, don''t wait'\n---\nbody")
	write(t, filepath.Join(f.cfg, "commands", "plain.md"), "\n\n# Plainly described by its first line\nmore")
	write(t, filepath.Join(f.cfg, "commands", "git", "commit.md"), "---\ndescription: >\n  Folded onto\n  one line\n---\n")
	write(t, filepath.Join(f.cfg, "commands", "notes.txt"), "not a command")

	c := f.s.Scan("")
	want := []string{"user:command:deploy", "user:command:git:commit", "user:skill:handoff", "user:skill:nameless", "user:command:plain"}
	if got := names(c); !slices.Equal(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	m := byName(c)
	for name, desc := range map[string]string{
		"handoff":    "Write the handoff doc",
		"nameless":   `A skill named by its "directory"`,
		"deploy":     "Ship it, don't wait",
		"plain":      "Plainly described by its first line",
		"git:commit": "Folded onto one line",
	} {
		if m[name].Description != desc {
			t.Errorf("%s: description %q, want %q", name, m[name].Description, desc)
		}
	}
}

// TestProjectSourcesAreFoundFromTheWindowsDirectory: the window's directory
// and each one above it up to the repository root contribute their .claude —
// a directory ABOVE the root does not, and neither does the home directory
// (whose .claude is the user's). A project's command shadows the user's of the
// same name.
func TestProjectSourcesAreFoundFromTheWindowsDirectory(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".claude", "commands", "build.md"), "---\ndescription: repo root build\n---\n")
	write(t, filepath.Join(f.sub, ".claude", "skills", "deeptool", "SKILL.md"), skill("deeptool", "nested project skill"))
	write(t, filepath.Join(f.homeDir, "work", ".claude", "commands", "above.md"), "---\ndescription: above the repository root\n---\n")
	write(t, filepath.Join(f.repo, ".claude", "commands", "shared.md"), "---\ndescription: the PROJECT's shared\n---\n")
	write(t, filepath.Join(f.cfg, "commands", "shared.md"), "---\ndescription: the USER's shared\n---\n")

	c := f.s.Scan(f.sub)
	m := byName(c)
	if m["build"].Source != mirror.SourceProject || m["deeptool"].Source != mirror.SourceProject {
		t.Fatalf("project entries missing or mis-sourced: %q", names(c))
	}
	if _, ok := m["above"]; ok {
		t.Fatalf("a .claude above the repository root was read: %q", names(c))
	}
	if m["shared"].Source != mirror.SourceProject || m["shared"].Description != "the PROJECT's shared" {
		t.Fatalf("shared = %+v, want the project's", m["shared"])
	}
	// No working directory: the user's alone.
	if m := byName(f.s.Scan("")); m["build"].Name != "" || m["shared"].Source != mirror.SourceUser {
		t.Fatalf("with no cwd: %q", names(f.s.Scan("")))
	}
	// A directory outside any repository walks up to, and stops BEFORE, home.
	loose := filepath.Join(f.homeDir, "loose", "x")
	write(t, filepath.Join(loose, ".claude", "commands", "loose.md"), "x")
	if got := f.s.projectDirs(loose); !slices.Equal(got, []string{loose, filepath.Dir(loose)}) {
		t.Fatalf("projectDirs(%s) = %q, want it and its parent, not home", loose, got)
	}
}

// TestPluginsAreOfferedWhenInstalledAndEnabled: a plugin installed for the
// user AND enabled is offered, namespaced; one installed but not enabled is
// not; one installed for a project is offered only in that project.
func TestPluginsAreOfferedWhenInstalledAndEnabled(t *testing.T) {
	f := newFixture(t)
	pdir := func(n string) string { return filepath.Join(f.cfg, "plugins", "cache", "mkt", n, "1.0.0") }
	for _, n := range []string{"stripe", "cloud", "local"} {
		write(t, filepath.Join(pdir(n), "skills", n+"-skill", "SKILL.md"), skill(n+"-skill", n+" plugin skill"))
		write(t, filepath.Join(pdir(n), "commands", n+"-cmd.md"), "---\ndescription: "+n+" plugin command\n---\n")
	}
	ip := map[string]any{"version": 2, "plugins": map[string]any{
		"stripe@mkt": []map[string]string{{"scope": "user", "installPath": pdir("stripe")}},
		"cloud@mkt":  []map[string]string{{"scope": "user", "installPath": pdir("cloud")}},
		"local@mkt":  []map[string]string{{"scope": "project", "projectPath": f.repo, "installPath": pdir("local")}},
	}}
	b, _ := json.Marshal(ip)
	write(t, filepath.Join(f.cfg, "plugins", "installed_plugins.json"), string(b))
	write(t, filepath.Join(f.cfg, "settings.json"), `{"enabledPlugins": {"stripe@mkt": true, "cloud@mkt": false}}`)
	write(t, filepath.Join(f.repo, ".claude", "settings.json"), `{"enabledPlugins": {"local@mkt": true}}`)

	m := byName(f.s.Scan(f.sub))
	for _, n := range []string{"stripe:stripe-skill", "stripe:stripe-cmd", "local:local-skill", "local:local-cmd"} {
		if m[n].Source != mirror.SourcePlugin {
			t.Errorf("%s: %+v, want a plugin entry", n, m[n])
		}
	}
	if m["stripe:stripe-skill"].Description != "stripe plugin skill" {
		t.Errorf("stripe skill description %q", m["stripe:stripe-skill"].Description)
	}
	for n := range m {
		if strings.HasPrefix(n, "cloud:") {
			t.Errorf("%s offered: the plugin is installed but not enabled", n)
		}
	}
	// Outside the project that installed it, the project's plugin is not offered.
	if m := byName(f.s.Scan("")); m["local:local-skill"].Name != "" || m["stripe:stripe-skill"].Name == "" {
		t.Fatalf("with no cwd: local offered=%v, stripe offered=%v", m["local:local-skill"].Name != "", m["stripe:stripe-skill"].Name != "")
	}
}

// TestTheConfigDirectoryFollowsCLAUDE_CONFIG_DIR: an unset Config is the
// variable's directory, not ~/.claude.
func TestTheConfigDirectoryFollowsCLAUDE_CONFIG_DIR(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "skills", "from-env", "SKILL.md"), skill("from-env", "found through the variable"))
	t.Setenv("CLAUDE_CONFIG_DIR", d)
	s := &CatalogScanner{Home: t.TempDir(), SnippetsFile: "-"}
	if m := byName(s.Scan("")); m["from-env"].Source != mirror.SourceUser {
		t.Fatalf("CLAUDE_CONFIG_DIR not read: %q", names(s.Scan("")))
	}
}

// TestSnippetsAreReadAndReloadedWhenTheFileChanges: the file's entries, none
// for a missing or invalid file, and a change picked up on the next scan.
func TestSnippetsAreReadAndReloadedWhenTheFileChanges(t *testing.T) {
	f := newFixture(t)
	if c := f.s.Scan(""); c.Snippets != nil {
		t.Fatalf("no file: %+v", c.Snippets)
	}
	write(t, f.s.SnippetsFile, `[{"trigger":":eos","label":"End of session","body":"reflect, then write the handoff"},{"trigger":"nocolon","body":"dropped"}]`)
	c := f.s.Scan("")
	if len(c.Snippets) != 1 || c.Snippets[0] != (mirror.Snippet{Trigger: ":eos", Label: "End of session", Body: "reflect, then write the handoff"}) {
		t.Fatalf("snippets = %+v", c.Snippets)
	}
	write(t, f.s.SnippetsFile, `[{"trigger":":kickoff","label":"Kickoff","body":"give the kickoff message"}]`)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(f.s.SnippetsFile, future, future)
	if c := f.s.Scan(""); len(c.Snippets) != 1 || c.Snippets[0].Trigger != ":kickoff" {
		t.Fatalf("after a change: %+v", c.Snippets)
	}
	write(t, f.s.SnippetsFile, `{"not": "an array"}`)
	_ = os.Chtimes(f.s.SnippetsFile, future.Add(time.Second), future.Add(time.Second))
	if c := f.s.Scan(""); c.Snippets != nil {
		t.Fatalf("an invalid file: %+v", c.Snippets)
	}
}

// TestAFileIsParsedOnceWhileItsTimeAndSizeStand: the cache. A same-size
// rewrite with the old modification time is NOT re-read (the cache answered);
// moving the time is.
func TestAFileIsParsedOnceWhileItsTimeAndSizeStand(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.cfg, "commands", "c.md")
	write(t, p, "---\ndescription: AAAA\n---\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	_ = os.Chtimes(p, old, old)
	if d := byName(f.s.Scan(""))["c"].Description; d != "AAAA" {
		t.Fatalf("first scan: %q", d)
	}
	write(t, p, "---\ndescription: BBBB\n---\n")
	_ = os.Chtimes(p, old, old)
	if d := byName(f.s.Scan(""))["c"].Description; d != "AAAA" {
		t.Fatalf("an unchanged time and size was re-parsed (got %q): the cache is not consulted", d)
	}
	_ = os.Chtimes(p, old.Add(time.Minute), old.Add(time.Minute))
	if d := byName(f.s.Scan(""))["c"].Description; d != "BBBB" {
		t.Fatalf("a changed time was not re-read: %q", d)
	}
}

// TestAnUnreadableDirectoryContributesNothingAndTheRestIsAnswered.
func TestAnUnreadableDirectoryContributesNothingAndTheRestIsAnswered(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("running as root: a mode-000 directory is still readable, so this case cannot be built")
	}
	f := newFixture(t)
	write(t, filepath.Join(f.cfg, "skills", "ok", "SKILL.md"), skill("ok", "still offered"))
	locked := filepath.Join(f.cfg, "commands")
	write(t, filepath.Join(locked, "secret.md"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	c := f.s.Scan("")
	if got := names(c); !slices.Equal(got, []string{"user:skill:ok"}) {
		t.Fatalf("commands = %q, want the readable skill alone", got)
	}
}

// TestTheScanIsBounded: a directory's entries, a description's length and the
// file head read are all capped.
func TestTheScanIsBounded(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.cfg, "commands")
	for i := 0; i < maxCatalogFiles+40; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("c%04d.md", i)), "x")
	}
	long := strings.Repeat("é", 3*mirror.MaxCatalogDescription)
	write(t, filepath.Join(f.cfg, "skills", "long", "SKILL.md"), skill("long", long))
	// A description past the head the scan reads is not seen at all.
	write(t, filepath.Join(f.cfg, "skills", "far", "SKILL.md"), "---\nname: far\n"+strings.Repeat("x: y\n", maxCatalogHead/5)+"description: beyond the head\n---\n")
	// The directory cap, read below the wire's own count cap (which
	// mirror.NormalizeCatalog applies, and its tests measure).
	if n := len(f.s.commands(dir, "", mirror.SourceUser)); n != maxCatalogFiles {
		t.Fatalf("%d commands read from a directory of %d, want the cap %d", n, maxCatalogFiles+40, maxCatalogFiles)
	}
	m := map[string]mirror.CatalogEntry{}
	for _, e := range f.s.skills(filepath.Join(f.cfg, "skills"), "", mirror.SourceUser) {
		m[e.Name] = mirror.NormalizeCatalog(mirror.Catalog{Commands: []mirror.CatalogEntry{e}}).Commands[0]
	}
	if d := m["long"].Description; len([]rune(d)) != mirror.MaxCatalogDescription || !strings.HasSuffix(d, "…") {
		t.Fatalf("a long description is %d runes, want %d ending in an ellipsis", len([]rune(d)), mirror.MaxCatalogDescription)
	}
	if m["far"].Description != "" {
		t.Fatalf("a description past maxCatalogHead was read: %q", m["far"].Description)
	}
}

// TestDefaultSnippetsFileFollowsXDG: $XDG_CONFIG_HOME first, else
// ~/.config.
func TestDefaultSnippetsFileFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	if got := DefaultSnippetsFile(); got != "/xdg/conf/muster-mirror/snippets.json" {
		t.Fatalf("with XDG_CONFIG_HOME: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/fixture-users/fixture-user")
	if got := DefaultSnippetsFile(); got != "/fixture-users/fixture-user/.config/muster-mirror/snippets.json" {
		t.Fatalf("without: %q", got)
	}
}

// TestParseHeadReadsTheFrontMatterSubset: the scalar forms, a literal block,
// a trailing comment, and a file whose front matter never closes (no front
// matter at all: its first line describes a command).
func TestParseHeadReadsTheFrontMatterSubset(t *testing.T) {
	for _, c := range []struct {
		in, name, desc string
		first, hidden  bool
	}{
		{in: "---\nname: a\ndescription: plain # a comment\n---\n", name: "a", desc: "plain"},
		{in: "---\nname: \"q\"\ndescription: \"tab\\tand \\\"quotes\\\"\"\n---\n", name: "q", desc: "tab\tand \"quotes\""},
		{in: "---\ndescription: |\n  line one\n  line two\nname: lit\n---\n", name: "lit", desc: "line one\nline two"},
		{in: "---\nname: never closed\ndescription: x\n", desc: "---", first: true},
		{in: "---\r\nname: crlf\r\ndescription: windows\r\n---\r\n", name: "crlf", desc: "windows"},
		{in: "---\nname: h\nuser-invocable: False\n---\n", name: "h", hidden: true},
	} {
		h := parseHead([]byte(c.in), c.first)
		if h.name != c.name || h.description != c.desc || h.hidden != c.hidden {
			t.Errorf("parseHead(%q) = %+v, want name=%q desc=%q hidden=%v", c.in, h, c.name, c.desc, c.hidden)
		}
	}
}
