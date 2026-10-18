package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// THE COMPOSER'S MENU CATALOG: what a mirrored window's message bar offers when
// `/` or `:` is typed — the Claude Code commands and skills that window's pane
// can run (the host's user-level, plugin and PROJECT `.claude/` sources), and
// the operator's prewritten agent-prompt snippets.
//
// 🔴 IT IS A REQUEST, NOT A FIELD OF THE WINDOW LIST. The window list travels in
// ONE message whose worst case already measures 97% of MaxFrame
// (TestTheLargestWindowListFitsOneFrame); a command list per window does not
// fit beside it, and it would be re-sent on every refresh for a menu almost
// nobody opens. So muster ASKS for one window's catalog when a composer needs it
// (catalog), and the host answers that request alone (cataloged): one message,
// bounded by MaxCatalogBytes ENCODED, far inside MaxFrame.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like Msg.PaneScroll:
//   - a host ADVERTISES it (Msg.CatalogReport in its hello). An older host never
//     does, so muster never asks it — the composer offers Claude Code's
//     built-in commands alone (BuiltinCommands) and says why;
//   - an older muster ignores the hello field and never sends a catalog request.
//     (An older host that somehow got one would ignore the unknown type — its
//     read loop has no default case — and muster's wait would time out into the
//     same built-ins-only answer.)
//
// 🔴 EVERY STRING IN IT IS HOST FILE CONTENT. A skill's description is whatever
// its SKILL.md says, so NormalizeCatalog — applied by BOTH halves — drops
// control characters, bounds every field, and caps the whole thing by encoded
// size; and the page draws every one of them as TEXT (textContent), never as
// markup.

const (
	// TypeCatalog (muster → host): send the catalog of Session/Window, by ID.
	TypeCatalog = "catalog"
	// TypeCataloged (host → muster): the answer to catalog ID — Msg.Catalog, or
	// Msg.Error.
	TypeCataloged = "cataloged"
)

// The catalog's bounds. Each is a property of the wire, so both halves read
// them here.
const (
	// MaxCatalogCommands and MaxCatalogSnippets bound the entry counts.
	MaxCatalogCommands = 512
	MaxCatalogSnippets = 128
	// MaxCatalogName bounds a command's name (without its leading `/`), bytes.
	MaxCatalogName = 96
	// MaxCatalogDescription bounds a description and a snippet's label, in
	// RUNES, cut on a rune boundary with an ellipsis.
	MaxCatalogDescription = 200
	// MaxSnippetTrigger bounds a snippet's trigger (":eos"), bytes.
	MaxSnippetTrigger = 32
	// MaxSnippetBody bounds a snippet's text, bytes.
	MaxSnippetBody = 4096
	// MaxCatalogBytes bounds a whole catalog ENCODED. A count is not a size —
	// JSON writes `<` as <, six bytes — so the bound that keeps one answer
	// inside MaxFrame is this one: entries past it are dropped (Truncated) —
	// the snippets kept within half of it, the commands within the rest, each
	// in order. TestTheLargestCatalogFitsOneFrame measures it.
	MaxCatalogBytes = 256 << 10
)

// The sources of a command (CatalogEntry.Source), in the order a NAME is
// claimed when two define it: a project's own command shadows the user's of the
// same name, the user's shadows a plugin's, and any of them a built-in.
const (
	SourceProject = "project"
	SourceUser    = "user"
	SourcePlugin  = "plugin"
	SourceBuiltin = "builtin"
)

// The kinds of a command (CatalogEntry.Kind).
const (
	KindCommand = "command"
	KindSkill   = "skill"
)

// CatalogEntry is one slash command or skill: typed as `/<Name>`.
type CatalogEntry struct {
	// Name is what follows the `/`: "compact", "handoff", "stripe:test-cards".
	Name string `json:"name"`
	// Description is what the entry says it does; "" for none.
	Description string `json:"description,omitempty"`
	// Source is SourceProject, SourceUser, SourcePlugin or SourceBuiltin.
	Source string `json:"source"`
	// Kind is KindCommand or KindSkill.
	Kind string `json:"kind"`
}

// Snippet is one prewritten message (the operator's agent-prompt snippets):
// typed as its Trigger after `:`, inserted as Body.
type Snippet struct {
	Trigger string `json:"trigger"`
	Label   string `json:"label,omitempty"`
	Body    string `json:"body"`
}

// Catalog is one window's menu: its commands and the host's snippets.
type Catalog struct {
	Commands []CatalogEntry `json:"commands,omitempty"`
	Snippets []Snippet      `json:"snippets,omitempty"`
	// Truncated: something was dropped to stay within the bounds.
	Truncated bool `json:"truncated,omitempty"`
}

// catalogNamePattern is a command name the wire carries: what Claude Code
// itself accepts as a command or skill name (letters, digits, `-`, `_`, `.`),
// optionally namespaced by `:` (a plugin's "stripe:test-cards", a command in a
// sub-directory "git:commit").
var catalogNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(:[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// ValidCatalogName reports whether name is a command name the wire carries.
func ValidCatalogName(name string) bool {
	return len(name) <= MaxCatalogName && catalogNamePattern.MatchString(name)
}

// snippetTriggerPattern is a snippet trigger: a `:` and a word — what espanso's
// own triggers look like, and what the menu filters on after the `:`.
var snippetTriggerPattern = regexp.MustCompile(`^:[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidSnippetTrigger reports whether trigger is one the wire carries.
func ValidSnippetTrigger(trigger string) bool {
	return len(trigger) <= MaxSnippetTrigger && snippetTriggerPattern.MatchString(trigger)
}

// CleanText is s as one line of display text: invalid UTF-8 replaced, every
// control character (C0, DEL, C1) and the bidirectional overrides that can
// make a description read differently from what it says turned into a space,
// runs of white space collapsed, trimmed, and cut to max runes with "…".
func CleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	space := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiControl(r) || r == ' ' || r == '\u00a0' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) <= max {
		return out
	}
	rs := []rune(out)
	return strings.TrimSpace(string(rs[:max-1])) + "…"
}

// isBidiControl reports the Unicode bidirectional formatting characters
// (embeddings, overrides, isolates and the marks).
func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}

// cleanBody is a snippet's text as it is inserted: valid UTF-8, CRLF and CR
// folded to LF, every other control character but tab and newline dropped —
// what SanitizePaste would refuse once the message is sent — and at most
// MaxSnippetBody bytes ("" when that leaves nothing).
func cleanBody(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiControl(r) {
			return -1
		}
		return r
	}, s)
	if strings.TrimSpace(s) == "" || len(s) > MaxSnippetBody {
		return ""
	}
	return s
}

// sourceRank orders sources by who claims a name first.
func sourceRank(s string) int {
	switch s {
	case SourceProject:
		return 0
	case SourceUser:
		return 1
	case SourcePlugin:
		return 2
	case SourceBuiltin:
		return 3
	}
	return -1
}

// NormalizeCatalog is c as the tunnel carries it. Both halves apply it: the host
// to what it read, muster to what it got.
//
//   - a command with an invalid name, an unknown source or kind is dropped; a
//     name defined twice is kept ONCE, by the first source in project, user,
//     plugin, built-in order (and within one source the first as given);
//   - a snippet with an invalid trigger, or a body that cleans to nothing, is
//     dropped; a trigger defined twice is kept once (the first);
//   - every description and label is CleanText'd to MaxCatalogDescription;
//   - commands are sorted by name, snippets kept in the order given (the
//     operator's own order), and both counts are capped;
//   - finally the whole is cut to MaxCatalogBytes ENCODED: the snippets kept
//     within half of it, the commands within what is left; entries past the
//     budget are dropped.
//
// Truncated says anything was dropped for a bound (not for being invalid).
func NormalizeCatalog(c Catalog) Catalog {
	out := Catalog{Truncated: c.Truncated}
	cmds := make([]CatalogEntry, 0, len(c.Commands))
	for _, e := range c.Commands {
		if !ValidCatalogName(e.Name) || sourceRank(e.Source) < 0 || (e.Kind != KindCommand && e.Kind != KindSkill) {
			continue
		}
		e.Description = CleanText(e.Description, MaxCatalogDescription)
		cmds = append(cmds, e)
	}
	sort.SliceStable(cmds, func(i, j int) bool { return sourceRank(cmds[i].Source) < sourceRank(cmds[j].Source) })
	seen := map[string]bool{}
	for _, e := range cmds {
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		out.Commands = append(out.Commands, e)
	}
	sort.SliceStable(out.Commands, func(i, j int) bool { return out.Commands[i].Name < out.Commands[j].Name })
	if len(out.Commands) > MaxCatalogCommands {
		out.Commands, out.Truncated = out.Commands[:MaxCatalogCommands], true
	}
	seenT := map[string]bool{}
	for _, s := range c.Snippets {
		if !ValidSnippetTrigger(s.Trigger) || seenT[s.Trigger] {
			continue
		}
		if s.Body = cleanBody(s.Body); s.Body == "" {
			continue
		}
		seenT[s.Trigger] = true
		s.Label = CleanText(s.Label, MaxCatalogDescription)
		out.Snippets = append(out.Snippets, s)
	}
	if len(out.Snippets) > MaxCatalogSnippets {
		out.Snippets, out.Truncated = out.Snippets[:MaxCatalogSnippets], true
	}
	// The byte budget: the size of each entry as it is encoded, plus a comma.
	// Snippets are counted FIRST, within half of it — they are the operator's
	// own few, and a host with hundreds of plugin skills must not push them
	// out — and commands get what is left.
	size := 64
	keepS := 0
	for _, s := range out.Snippets {
		b, _ := json.Marshal(s)
		if size+len(b)+1 > MaxCatalogBytes/2 {
			break
		}
		size += len(b) + 1
		keepS++
	}
	keepC := 0
	for _, e := range out.Commands {
		b, _ := json.Marshal(e)
		if size+len(b)+1 > MaxCatalogBytes {
			break
		}
		size += len(b) + 1
		keepC++
	}
	if keepC < len(out.Commands) || keepS < len(out.Snippets) {
		out.Truncated = true
		out.Commands, out.Snippets = out.Commands[:keepC], out.Snippets[:keepS]
	}
	if len(out.Commands) == 0 {
		out.Commands = nil
	}
	if len(out.Snippets) == 0 {
		out.Snippets = nil
	}
	return out
}

// MergeBuiltins is the menu muster offers for a window: the host's catalog with
// Claude Code's built-in commands added (a host-defined name shadows a built-in,
// as in Claude Code), normalised.
func MergeBuiltins(c Catalog) Catalog {
	all := Catalog{Commands: append(append([]CatalogEntry(nil), c.Commands...), BuiltinCommands...), Snippets: c.Snippets, Truncated: c.Truncated}
	return NormalizeCatalog(all)
}

// BuiltinCommands are the Claude Code built-in commands the composer offers on
// every window, whatever its host reports — the whole menu for a host whose
// muster-mirror predates the catalog. Names and descriptions as Claude Code
// 2.1.289's own `/` menu lists them (/init and /security-review are its bundled
// skills, listed in the same menu).
//
// ⚠ DELIBERATELY NOT ALL OF THEM. Left out: what ends or signs out the session
// in the pane (/exit, /logout, /login), and what only makes sense at the
// machine's own terminal or account page (/terminal-setup, /ide, /chrome,
// /keybindings, /mobile, /install-github-app, /install-slack-app, /upgrade,
// /passes, /stickers, /radio). Anything typed by hand is still sent as typed.
var BuiltinCommands = []CatalogEntry{
	builtin("add-dir", "Add a new working directory"),
	builtin("background", "Send this session to the background and free the terminal"),
	builtin("branch", "Create a branch of the current conversation at this point"),
	builtin("btw", "Ask a quick side question without interrupting the main conversation"),
	builtin("cd", "Move this session to a new working directory"),
	builtin("clear", "Start a new session with empty context; previous session stays on disk (resumable with /resume)"),
	builtin("compact", "Free up context by summarizing the conversation so far"),
	builtin("config", "Open settings"),
	builtin("context", "Visualize current context usage as a colored grid"),
	builtin("copy", "Copy Claude's last response to clipboard (or /copy N for the Nth-latest)"),
	builtin("diff", "Toggle the diff panel showing uncommitted changes"),
	builtin("effort", "Set effort level for model usage"),
	builtin("fast", "Toggle fast mode"),
	builtin("export", "Export the current conversation to a file or clipboard"),
	builtin("fork", "Copy this conversation into a new background session and keep working here"),
	builtin("goal", "Set a goal Claude checks before stopping"),
	builtin("help", "Show help and available commands"),
	builtin("hooks", "View hook configurations for tool events"),
	builtin("init", "Initialize a new CLAUDE.md file with codebase documentation"),
	builtin("mcp", "Manage MCP servers"),
	builtin("memory", "Edit CLAUDE.md files and memory settings"),
	builtin("model", "Set the AI model for Claude Code"),
	builtin("output-style", "List output styles or switch to one"),
	builtin("permissions", "Manage allow and deny tool permission rules"),
	builtin("plan", "Enable plan mode or view the current session plan"),
	builtin("plugin", "Manage Claude Code plugins"),
	builtin("recap", "Generate a one-line session recap now"),
	builtin("release-notes", "View release notes"),
	builtin("reload-plugins", "Activate pending plugin changes in the current session"),
	builtin("reload-skills", "Pick up skills added or changed on disk during this session"),
	builtin("rename", "Rename the current conversation"),
	builtin("resume", "Resume a previous conversation"),
	builtin("rewind", "Restore the code and/or conversation to a previous point"),
	builtin("security-review", "Complete a security review of the pending changes on the current branch"),
	builtin("skills", "List available skills"),
	builtin("subtask", "Send a subagent off with your full context; its result comes back here"),
	builtin("status", "Show Claude Code status including version, model, account, API connectivity, and tool statuses"),
	builtin("tasks", "View and manage everything running in the background"),
	builtin("usage", "Show session cost, plan usage, and activity stats"),
}

func builtin(name, desc string) CatalogEntry {
	return CatalogEntry{Name: name, Description: desc, Source: SourceBuiltin, Kind: KindCommand}
}

// The catalog refusals.
var (
	// ErrCatalogUnsupported: the host did not advertise a catalog (an older
	// muster-mirror). The composer then offers the built-ins alone.
	ErrCatalogUnsupported = errors.New("mirror: this host's muster-mirror does not report its commands, skills or snippets; upgrade it")
	// ErrCatalogRefused wraps the host's own reason for not answering.
	ErrCatalogRefused = errors.New("mirror: the host did not send its catalog")
	// ErrCatalogTimeout: the host did not answer in time.
	ErrCatalogTimeout = errors.New("mirror: the host did not send its catalog in time")
)

// catalogTimeout bounds how long a catalog request waits for the host. A
// catalog is a few directory reads, cached by mtime on the host; one that takes
// longer than this is stuck, and the composer offers the built-ins meanwhile.
const catalogTimeout = 3 * time.Second

// catalogAnswer is a host's cataloged message, as the waiting request gets it.
type catalogAnswer struct {
	c   Catalog
	err error
}

// CatalogWindow asks owner's host for the catalog of one WINDOW of session (its
// tmux window id; "" = the session's active window) and waits for the answer.
//
// 🔴 owner IS THE VIEWING PRINCIPAL'S USER: the lookup is keyed by (owner,
// host), so another owner's host answers ErrHostOffline exactly as one that
// does not exist — and nothing is ever asked of it.
func (h *Hub) CatalogWindow(ctx context.Context, owner int64, host, session, window string) (Catalog, error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return Catalog{}, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	windowID, _, err := hc.target(session, window)
	if err != nil {
		return Catalog{}, err
	}
	if !hc.catalogReport {
		return Catalog{}, fmt.Errorf("%w (host %q)", ErrCatalogUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan catalogAnswer, 1)
	hc.catalogs[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.catalogs, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeCatalog, ID: id, Session: session, Window: windowID}); err != nil {
		return Catalog{}, fmt.Errorf("mirror: ask the host for its catalog: %w", err)
	}
	t := time.NewTimer(catalogTimeout)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.c, a.err
	case <-ctx.Done():
		return Catalog{}, ctx.Err()
	case <-t.C:
		return Catalog{}, ErrCatalogTimeout
	}
}

// cataloged hands the host's answer to the request waiting on id — the catalog
// normalised again here, whatever the host did.
func (hc *hostConn) cataloged(m Msg) {
	hc.mu.Lock()
	ch := hc.catalogs[m.ID]
	delete(hc.catalogs, m.ID)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != "" {
		ch <- catalogAnswer{err: fmt.Errorf("%w: %s", ErrCatalogRefused, CleanText(m.Error, 200))}
		return
	}
	var c Catalog
	if m.Catalog != nil {
		c = *m.Catalog
	}
	ch <- catalogAnswer{c: NormalizeCatalog(c)}
}
