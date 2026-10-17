package mirrorhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE COMPOSER MENU'S CATALOG, read on this host (the wire half is
// internal/mirror/catalog.go): for one window, the Claude Code commands and
// skills its pane can run, and the operator's agent-prompt snippets.
//
// WHERE EACH COMES FROM — the places Claude Code itself reads them:
//
//   - USER: <config>/commands/*.md (and one level of sub-directory, named
//     "<dir>:<name>") and <config>/skills/<name>/SKILL.md, where <config> is
//     $CLAUDE_CONFIG_DIR, else ~/.claude;
//   - PROJECT: the same two under <dir>/.claude for the window's working
//     directory and each directory above it up to the repository root (the
//     first one holding .git) — but never the home directory itself, whose
//     .claude IS the user's;
//   - PLUGIN: every plugin <config>/plugins/installed_plugins.json records as
//     installed for the user (or for one of those project directories) AND that
//     an enabledPlugins setting turns on (<config>/settings.json, or the
//     project's .claude/settings.json, or its settings(.local).json): its commands/*.md
//     and skills/*/SKILL.md, named "<plugin>:<name>" as Claude Code names them;
//   - SNIPPETS: SnippetsFile, a JSON array of {trigger, label, body} the
//     operator's dotfiles write (DefaultSnippetsFile says where and why).
//
// A name and a description come from the file's YAML front matter (`name:`,
// `description:`); a command with no description is described by its first
// line of text, as Claude Code does. A skill marked `user-invocable: false` is
// left out: Claude Code does not offer it in its `/` menu either.
//
// 🔴 CHEAP TO ASK AGAIN. Every parsed file is cached by path, keyed on its
// modification time and size, so a repeat request re-reads directories (a
// handful of getdents) and stats files, and parses only what changed. A file
// the operator edits is picked up on the next request — no restart, no watch.
//
// 🔴 NOTHING HERE FAILS THE REQUEST. A directory that does not exist or cannot
// be read, a file that does not parse, a snippets file that is missing: each
// contributes nothing, and the rest is still answered. Every count and size is
// bounded — per directory here, and for the whole answer by
// mirror.NormalizeCatalog.

// maxCatalogFiles bounds the entries read from one directory, and
// maxCatalogHead the bytes read from one file (front matter and a first line
// are at its head).
const (
	maxCatalogFiles = 512
	maxCatalogHead  = 64 << 10
	// maxSnippetsFile bounds the snippets file.
	maxSnippetsFile = 512 << 10
	// maxProjectDepth bounds the walk up from a window's working directory.
	maxProjectDepth = 16
)

// DefaultSnippetsFile is where muster-mirror reads the operator's snippets when
// MUSTER_MIRROR_SNIPPETS_FILE is unset: $XDG_CONFIG_HOME/muster-mirror/
// snippets.json, else ~/.config/muster-mirror/snippets.json.
//
// WHY THERE: it is muster-mirror's own config namespace, so nothing else reads
// it (an espanso match directory would be parsed by espanso; Claude Code's
// config directory moves with $CLAUDE_CONFIG_DIR); it is where a home-manager
// `xdg.configFile` writes, so the dotfiles that own the snippets place it
// without touching this service's unit; and because it is re-read by
// modification time, a dotfiles switch that changes it is picked up by the
// running agent with no restart.
func DefaultSnippetsFile() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "muster-mirror", "snippets.json")
}

// CatalogScanner reads catalogs. Its zero value reads the real locations; a
// test points each at a fixture. Safe for concurrent use.
type CatalogScanner struct {
	// Config is Claude Code's config directory ("" = $CLAUDE_CONFIG_DIR, else
	// ~/.claude).
	Config string
	// Home is the user's home directory, where the project walk stops ("" =
	// os.UserHomeDir).
	Home string
	// SnippetsFile is the snippets JSON ("" = DefaultSnippetsFile; "-" = none).
	SnippetsFile string

	mu    sync.Mutex
	files map[string]cachedEntry
	snip  cachedSnippets
}

// cachedEntry is one parsed command or skill file, valid while the file's
// modification time and size are these.
type cachedEntry struct {
	mod  time.Time
	size int64
	head entryHead
}

type cachedSnippets struct {
	path string
	mod  time.Time
	size int64
	list []mirror.Snippet
}

// entryHead is what a command or skill file says about itself.
type entryHead struct {
	name, description string
	// hidden: `user-invocable: false`.
	hidden bool
}

func (s *CatalogScanner) config() string {
	if s.Config != "" {
		return s.Config
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

func (s *CatalogScanner) homeDir() string {
	if s.Home != "" {
		return s.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

func (s *CatalogScanner) snippetsFile() string {
	switch s.SnippetsFile {
	case "":
		return DefaultSnippetsFile()
	case "-":
		return ""
	}
	return s.SnippetsFile
}

// Scan is the catalog for a window whose pane runs in cwd ("" = unknown: the
// user's, the plugins' and the snippets alone), normalised for the wire.
func (s *CatalogScanner) Scan(cwd string) mirror.Catalog {
	var cmds []mirror.CatalogEntry
	projects := s.projectDirs(cwd)
	for _, p := range projects {
		root := filepath.Join(p, ".claude")
		cmds = append(cmds, s.commands(filepath.Join(root, "commands"), "", mirror.SourceProject)...)
		cmds = append(cmds, s.skills(filepath.Join(root, "skills"), "", mirror.SourceProject)...)
	}
	cfg := s.config()
	cmds = append(cmds, s.commands(filepath.Join(cfg, "commands"), "", mirror.SourceUser)...)
	cmds = append(cmds, s.skills(filepath.Join(cfg, "skills"), "", mirror.SourceUser)...)
	for _, pl := range s.plugins(cfg, projects) {
		cmds = append(cmds, s.commands(filepath.Join(pl.path, "commands"), pl.name, mirror.SourcePlugin)...)
		cmds = append(cmds, s.skills(filepath.Join(pl.path, "skills"), pl.name, mirror.SourcePlugin)...)
	}
	return mirror.NormalizeCatalog(mirror.Catalog{Commands: cmds, Snippets: s.snippets()})
}

// projectDirs is cwd and the directories above it whose .claude is a
// PROJECT's, nearest first: up to and including the first holding .git, never
// the home directory or the filesystem root, at most maxProjectDepth.
func (s *CatalogScanner) projectDirs(cwd string) []string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil
	}
	home := filepath.Clean(s.homeDir())
	var out []string
	d := filepath.Clean(cwd)
	for i := 0; i < maxProjectDepth; i++ {
		if d == home || d == filepath.Dir(d) {
			break
		}
		out = append(out, d)
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break
		}
		d = filepath.Dir(d)
	}
	return out
}

// commands is dir's *.md files — and those one directory down, named
// "<sub>:<name>" — as commands, each prefixed "<plugin>:" when plugin is set.
func (s *CatalogScanner) commands(dir, plugin, source string) []mirror.CatalogEntry {
	ents, err := readDirBounded(dir)
	if err != nil {
		return nil
	}
	var out []mirror.CatalogEntry
	add := func(path, name string) {
		h, ok := s.head(path, true)
		if !ok || h.hidden {
			return
		}
		if plugin != "" {
			name = plugin + ":" + name
		}
		out = append(out, mirror.CatalogEntry{Name: name, Description: h.description, Source: source, Kind: mirror.KindCommand})
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if isDir(p) {
			sub, err := readDirBounded(p)
			if err != nil {
				continue
			}
			for _, f := range sub {
				if n, ok := strings.CutSuffix(f.Name(), ".md"); ok && !isDir(filepath.Join(p, f.Name())) {
					add(filepath.Join(p, f.Name()), e.Name()+":"+n)
				}
			}
			continue
		}
		if n, ok := strings.CutSuffix(e.Name(), ".md"); ok {
			add(p, n)
		}
	}
	return out
}

// skills is every <dir>/<name>/SKILL.md as a skill, named by its front
// matter's `name` (else the directory), prefixed "<plugin>:" when plugin is set.
func (s *CatalogScanner) skills(dir, plugin, source string) []mirror.CatalogEntry {
	ents, err := readDirBounded(dir)
	if err != nil {
		return nil
	}
	var out []mirror.CatalogEntry
	for _, e := range ents {
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		h, ok := s.head(p, false)
		if !ok || h.hidden {
			continue
		}
		name := h.name
		if name == "" || !mirror.ValidCatalogName(name) {
			name = e.Name()
		}
		if plugin != "" {
			name = plugin + ":" + name
		}
		out = append(out, mirror.CatalogEntry{Name: name, Description: h.description, Source: source, Kind: mirror.KindSkill})
	}
	return out
}

// readDirBounded is os.ReadDir, at most maxCatalogFiles entries, dot-files
// skipped.
func readDirBounded(dir string) ([]os.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(maxCatalogFiles)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := ents[:0]
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e)
		}
	}
	return out, nil
}

// isDir follows a symlink (home-manager links skill directories in).
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// head is path's front matter, from the cache when the file has not changed.
// firstLine: a file with no description is described by its first line of
// text (a command); a skill without one has none.
func (s *CatalogScanner) head(path string, firstLine bool) (entryHead, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return entryHead{}, false
	}
	s.mu.Lock()
	c, ok := s.files[path]
	s.mu.Unlock()
	if ok && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.head, true
	}
	f, err := os.Open(path)
	if err != nil {
		return entryHead{}, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCatalogHead))
	_ = f.Close()
	if err != nil {
		return entryHead{}, false
	}
	h := parseHead(b, firstLine)
	s.mu.Lock()
	if s.files == nil {
		s.files = map[string]cachedEntry{}
	}
	s.files[path] = cachedEntry{mod: st.ModTime(), size: st.Size(), head: h}
	s.mu.Unlock()
	return h, true
}

// parseHead reads a markdown file's YAML front matter — the subset command and
// skill files use: `key: value` with plain, "double-quoted" or 'single-quoted'
// scalars, and `>` / `|` block scalars — for name, description and
// user-invocable. Anything it does not understand is ignored, never an error.
func parseHead(b []byte, firstLine bool) entryHead {
	var h entryHead
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxCatalogHead)
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	body := lines
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				end = i
				break
			}
		}
		if end > 0 {
			fm := lines[1:end]
			body = lines[end+1:]
			for i := 0; i < len(fm); i++ {
				key, val, ok := strings.Cut(fm[i], ":")
				if !ok || strings.HasPrefix(fm[i], " ") || strings.HasPrefix(fm[i], "\t") {
					continue
				}
				key, val = strings.TrimSpace(key), strings.TrimSpace(val)
				if strings.HasPrefix(val, ">") || strings.HasPrefix(val, "|") {
					folded := strings.HasPrefix(val, ">")
					var parts []string
					for i+1 < len(fm) && (strings.HasPrefix(fm[i+1], " ") || strings.HasPrefix(fm[i+1], "\t") || strings.TrimSpace(fm[i+1]) == "") {
						i++
						parts = append(parts, strings.TrimSpace(fm[i]))
					}
					sep := "\n"
					if folded {
						sep = " "
					}
					val = strings.TrimSpace(strings.Join(parts, sep))
				} else {
					val = unquoteYAML(val)
				}
				switch key {
				case "name":
					h.name = val
				case "description":
					h.description = val
				case "user-invocable":
					h.hidden = strings.EqualFold(val, "false")
				}
			}
		}
	}
	if h.description == "" && firstLine {
		for _, l := range body {
			if t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#")); t != "" {
				h.description = t
				break
			}
		}
	}
	return h
}

// unquoteYAML is a plain or quoted YAML scalar's value; a trailing comment on a
// plain scalar is dropped.
func unquoteYAML(v string) string {
	switch {
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return v[1 : len(v)-1]
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// plugin is one enabled, installed plugin: its name and where it is installed.
type plugin struct{ name, path string }

// installedPlugins is installed_plugins.json, the fields read.
type installedPlugins struct {
	Plugins map[string][]struct {
		Scope       string `json:"scope"`
		ProjectPath string `json:"projectPath"`
		InstallPath string `json:"installPath"`
	} `json:"plugins"`
}

// plugins is every plugin installed for the user (or for one of projects) and
// turned on by an enabledPlugins setting, sorted by name.
func (s *CatalogScanner) plugins(cfg string, projects []string) []plugin {
	b, err := readBounded(filepath.Join(cfg, "plugins", "installed_plugins.json"), maxSnippetsFile)
	if err != nil {
		return nil
	}
	var ip installedPlugins
	if json.Unmarshal(b, &ip) != nil {
		return nil
	}
	enabled := map[string]bool{}
	settings := []string{filepath.Join(cfg, "settings.json")}
	for _, p := range projects {
		settings = append(settings, filepath.Join(p, ".claude", "settings.json"), filepath.Join(p, ".claude", "settings"+".local.json"))
	}
	for _, f := range settings {
		b, err := readBounded(f, maxSnippetsFile)
		if err != nil {
			continue
		}
		var st struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		for k, v := range st.EnabledPlugins {
			if v {
				enabled[k] = true
			}
		}
	}
	inProject := map[string]bool{}
	for _, p := range projects {
		inProject[filepath.Clean(p)] = true
	}
	var out []plugin
	for key, recs := range ip.Plugins {
		if !enabled[key] {
			continue
		}
		name, _, _ := strings.Cut(key, "@")
		if !mirror.ValidCatalogName(name) || strings.Contains(name, ":") {
			continue
		}
		for _, r := range recs {
			if r.InstallPath == "" || !filepath.IsAbs(r.InstallPath) {
				continue
			}
			if r.Scope == "user" || (r.ProjectPath != "" && inProject[filepath.Clean(r.ProjectPath)]) {
				out = append(out, plugin{name: name, path: r.InstallPath})
				break
			}
		}
	}
	sortPlugins(out)
	return out
}

func sortPlugins(p []plugin) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].name < p[j-1].name; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// readBounded reads at most max bytes of path, refusing a larger file.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too large")
	}
	return b, nil
}

// snippets is the snippets file's entries, from the cache when it has not
// changed; none when it is missing, too large or not a JSON array of
// {trigger, label, body}.
func (s *CatalogScanner) snippets() []mirror.Snippet {
	path := s.snippetsFile()
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	s.mu.Lock()
	c := s.snip
	s.mu.Unlock()
	if c.path == path && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.list
	}
	b, err := readBounded(path, maxSnippetsFile)
	if err != nil {
		return nil
	}
	var list []mirror.Snippet
	if json.Unmarshal(b, &list) != nil {
		list = nil
	}
	s.mu.Lock()
	s.snip = cachedSnippets{path: path, mod: st.ModTime(), size: st.Size(), list: list}
	s.mu.Unlock()
	return list
}

// CwdResolver is a Backend that can say where a window's pane runs: what the
// catalog's PROJECT commands and skills are found from. Tmux is one; a backend
// that is not gets the user's, the plugins' and the snippets alone.
type CwdResolver interface {
	// WindowCwd is the working directory of window's active pane in session —
	// of its Claude Code process when the pane runs one — or an error.
	// window == "" is the session's active window.
	WindowCwd(ctx context.Context, session, window string) (string, error)
}

// CwdResolverOf is b's CwdResolver, looking through wrappers.
func CwdResolverOf(b Backend) (CwdResolver, bool) { return capabilityOf[CwdResolver](b) }

// WindowCwd implements CwdResolver: the window's Claude Code process's cwd
// when its pane runs one (where Claude Code itself looks for the project's
// .claude), else the pane's root process's.
func (t Tmux) WindowCwd(ctx context.Context, session, window string) (string, error) {
	all, err := t.Windows(ctx)
	if err != nil {
		return "", err
	}
	var w WindowInfo
	found := false
	for _, x := range all {
		if x.Session == session && ((window == "" && x.Active) || x.ID == window) {
			w, found = x, true
			break
		}
	}
	if !found {
		return "", errors.New("no such window")
	}
	pid := w.PanePID
	if w.Claude != nil && w.Claude.PID > 0 {
		pid = w.Claude.PID
	}
	return os.Readlink(filepath.Join(t.Claude.proc(), strconv.Itoa(pid), "cwd"))
}

// catalog answers muster's catalog request m: the catalog of the window it
// names, or why not.
//
// 🔴 ONLY A SESSION — AND A WINDOW — THIS HOST LISTED, like every request. No
// -interactive is needed: it reads names and descriptions for this host's own
// owner and types nothing.
func (h *hostSide) catalog(m mirror.Msg) {
	reply := func(c *mirror.Catalog, err error) {
		out := mirror.Msg{Type: mirror.TypeCataloged, ID: m.ID, Catalog: c}
		if err != nil {
			out.Error = err.Error()
			h.c.log.Printf("muster-mirror: catalog of %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		}
		_ = h.send(out)
	}
	if err := h.allowed(m); err != nil {
		reply(nil, err)
		return
	}
	cwd := ""
	if cr, ok := CwdResolverOf(h.c.cfg.Backend); ok {
		ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
		if d, err := cr.WindowCwd(ctx, m.Session, m.Window); err == nil {
			cwd = d
		}
		cancel()
	}
	c := h.c.catalog.Scan(cwd)
	reply(&c, nil)
}
