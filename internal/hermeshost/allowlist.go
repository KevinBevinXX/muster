package hermeshost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/atomicfile"
)

// THE SHELL-HOOK ALLOWLIST (docs/design/hermes.md, "Installation" and
// "Consent"). Hermes runs a configured shell hook only when its (event,
// command) pair is in $HERMES_HOME/shell-hooks-allowlist.json; a configured
// hook that is NOT is skipped by the gateway with a warning, and makes every
// new foreground CLI stop before its UI on `Allow this hook to run? [y/N]:`.
// So the host's configuration must allowlist EXACTLY every pair it
// configures — and upstream's Home Manager module has no option for it.
//
// WriteAllowlist is that writer, for the host's Home Manager activation to run
// (`muster-mirror hermes-allowlist`) after Hermes's own module. 🔴 IT WRITES
// THE WAY HERMES DOES (P:agent/shell_hooks.py, _locked_update_approvals and
// save_allowlist): it takes Hermes's flock on shell-hooks-allowlist.json.lock,
// reads the current file UNDER it, and writes a temp file (0600 from
// creation) that it renames into place. Without the lock a concurrent `[y/N]`
// answer is lost; without the rename a reader can see half a file; a
// read-only store symlink would break Hermes's own writes to the same file.
//
// It is the ONE Hermes file muster ever writes, and the mirroring daemon never
// does: only this command, run by the activation.

// HookSubcommand is the muster-mirror verb Hermes runs as the hook.
const HookSubcommand = "hermes-hook"

// HookCommand is the hook's command line for the muster-mirror binary at bin:
// what the host's Hermes config declares as each event's `command`, and what
// the allowlist must hold exactly.
func HookCommand(bin string) string { return bin + " " + HookSubcommand }

// ValidHookCommand reports why command cannot be the hook's command, nil when
// it can: an absolute path to a muster-mirror binary, with no whitespace in
// it (Hermes splits the command into argv), then exactly HookSubcommand.
func ValidHookCommand(command string) error {
	bin, verb, ok := strings.Cut(command, " ")
	switch {
	case !ok || verb != HookSubcommand:
		return fmt.Errorf("the hook command must be `<absolute path to muster-mirror> %s`, not %q", HookSubcommand, command)
	case !filepath.IsAbs(bin) || strings.ContainsAny(bin, " \t\n\r\"'\\$`"):
		return fmt.Errorf("the hook command's binary must be an absolute path with no spaces or quotes, not %q", bin)
	case filepath.Base(bin) != "muster-mirror":
		return fmt.Errorf("the hook command's binary must be muster-mirror, not %q", filepath.Base(bin))
	}
	return nil
}

// musterHookCommand: command is a muster-mirror hermes-hook command — this
// binary's or an older one's (a previous store path).
func musterHookCommand(command string) bool {
	f := strings.Fields(command)
	return len(f) == 2 && filepath.Base(f[0]) == "muster-mirror" && f[1] == HookSubcommand
}

// nixStore is the Nix store prefix: an allowlist linked there (a Home
// Manager file) is the one symlink the writer replaces.
var nixStore = "/nix/store/"

// allowlistLockWait bounds the wait for Hermes's lock.
const allowlistLockWait = 10 * time.Second

// ErrAllowlistUnreadable: the current allowlist is not a JSON object with an
// approvals list. The writer refuses to replace it (it would drop whatever
// the person approved); Hermes itself treats it as empty.
var ErrAllowlistUnreadable = errors.New("the shell-hook allowlist is not a JSON object with an approvals list; fix or remove it by hand")

// approval is one allowlist entry — the ONLY shape the writer keeps
// (Hermes's own: P:agent/shell_hooks.py, _record_approval). approved_at and
// script_mtime_at_approval are a string or null.
type approval struct {
	Event       string `json:"event"`
	Command     string `json:"command"`
	ApprovedAt  any    `json:"approved_at"`
	ScriptMtime any    `json:"script_mtime_at_approval"`
}

// parseApprovals reads an allowlist document: its approvals, each reduced to
// the approval schema. dropped says something else was there (another key,
// an entry of another shape, an unknown field) and is NOT carried over.
// ErrAllowlistUnreadable for a document that is not an object with an
// approvals list — never the decoder's own error, which quotes the file.
func parseApprovals(b []byte) (out []approval, dropped bool, err error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &doc) != nil || doc == nil {
		return nil, false, ErrAllowlistUnreadable
	}
	raw, ok := doc["approvals"]
	if !ok {
		return nil, len(doc) > 0, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, false, ErrAllowlistUnreadable
	}
	dropped = len(doc) > 1
	for _, e := range entries {
		var m map[string]any
		if json.Unmarshal(e, &m) != nil {
			dropped = true
			continue
		}
		a := approval{}
		ev, ok1 := m["event"].(string)
		cmd, ok2 := m["command"].(string)
		if !ok1 || !ok2 || ev == "" || cmd == "" {
			dropped = true
			continue
		}
		a.Event, a.Command = ev, cmd
		for k, v := range m {
			switch k {
			case "event", "command":
			case "approved_at", "script_mtime_at_approval":
				if _, isStr := v.(string); !isStr && v != nil {
					dropped = true
					continue
				}
				if k == "approved_at" {
					a.ApprovedAt = v
				} else {
					a.ScriptMtime = v
				}
			default:
				dropped = true
			}
		}
		out = append(out, a)
	}
	return out, dropped, nil
}

// WriteAllowlist makes muster's (event, command) pairs — every HookEvents
// event with command — present in h's allowlist exactly once, removes
// muster-mirror hermes-hook entries for any OTHER command, keeps every other
// approval, and writes NOTHING ELSE: only the approvals schema is carried
// over, so no foreign content (another key, another field) is ever copied
// into the rewritten file. changed is false when the file already said
// exactly that, and then it is not rewritten.
//
// 🔴 WHY OLDER muster-mirror ENTRIES ARE REMOVED: on Nix every upgrade
// changes the binary's store path, so each Home Manager switch would leave
// the previous path's five approvals behind — a growing list of consents for
// binaries that no longer exist (and that a garbage collection lets a later
// store path reuse). The command written is the one the host's Hermes config
// declares, which the activation passes in.
//
// 🔴 THE TARGET IS NEVER A SYMLINK OUT OF THE NIX STORE. A link into the
// store (a Home Manager file) is read and REPLACED by a real file — Hermes
// writes the same file when someone answers [y/N], which a read-only link
// would break. Any other symlink is refused, nothing written.
func (h Home) WriteAllowlist(command string, now time.Time) (changed bool, err error) {
	if err := ValidHookCommand(command); err != nil {
		return false, err
	}
	lockPath, path, dir, err := h.allowlistPaths()
	if err != nil {
		return false, err
	}
	unlock, err := lockFile(lockPath, allowlistLockWait)
	if err != nil {
		return false, err
	}
	defer unlock()

	var approvals []approval
	b, storeLink, err := h.readAllowlist()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	default:
		var dropped bool
		if approvals, dropped, err = parseApprovals(b); err != nil {
			return false, err
		}
		changed = dropped || storeLink
	}
	want := map[string]bool{}
	for _, e := range HookEvents {
		want[e] = true
	}
	have := map[string]bool{}
	var kept []approval
	for _, a := range approvals {
		switch {
		case want[a.Event] && a.Command == command:
			if have[a.Event] {
				changed = true // a duplicate of ours
				continue
			}
			have[a.Event] = true
		case musterHookCommand(a.Command) && a.Command != command:
			changed = true // an older muster-mirror's (see above)
			continue
		}
		kept = append(kept, a)
	}
	mtime := scriptMtime(command)
	for _, ev := range HookEvents {
		if have[ev] {
			continue
		}
		changed = true
		kept = append(kept, approval{Event: ev, Command: command,
			ApprovedAt: now.UTC().Format("2006-01-02T15:04:05.000000Z"), ScriptMtime: mtime})
	}
	if !changed {
		return false, nil
	}
	if kept == nil {
		kept = []approval{}
	}
	// Indented by two with no HTML escaping, as Hermes writes it (json.dump
	// with indent=2): another entry's `&&` stays `&&`.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"approvals": kept}); err != nil {
		return false, err
	}
	// The rename replaces a store link with the file itself.
	return true, atomicfile.Write(filepath.Join(dir, filepath.Base(path)), out.Bytes())
}

// scriptMtime is the hook binary's mtime as Hermes records it at approval
// (script_mtime_iso: UTC ISO-8601 with a Z), nil when it cannot be read. Only
// `hermes hooks doctor` compares it; the gateway never does.
func scriptMtime(command string) any {
	bin, _, _ := strings.Cut(command, " ")
	st, err := os.Stat(bin)
	if err != nil {
		return nil
	}
	return st.ModTime().UTC().Format("2006-01-02T15:04:05.000000Z")
}

// AllowlistCheck is what -list -hermes reports about the hook's consent.
type AllowlistCheck struct {
	// Command is the hook command checked for.
	Command string
	// Missing are the HookEvents whose (event, Command) pair is NOT
	// allowlisted: each such hook, if configured, is skipped by the gateway and
	// makes every new CLI stop on the [y/N] prompt.
	Missing []string
	// Other are the allowlisted muster-mirror hermes-hook entries for another
	// command, "event command" each: a host whose Hermes config names a
	// different binary than this one.
	Other []string
}

// OK: every pair is allowlisted.
func (c AllowlistCheck) OK() bool { return len(c.Missing) == 0 }

// CheckAllowlist compares h's allowlist with the pairs command needs. It
// READS the allowlist only. (The configured hooks are in config.yaml, which
// muster never reads: it can hold inline keys.)
func (h Home) CheckAllowlist(command string) (AllowlistCheck, error) {
	c := AllowlistCheck{Command: command}
	have := map[string]bool{}
	b, _, err := h.readAllowlist()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		approvals, _, err := parseApprovals(b)
		if err != nil {
			return c, err
		}
		for _, e := range approvals {
			switch {
			case e.Command == command:
				have[e.Event] = true
			case musterHookCommand(e.Command):
				c.Other = append(c.Other, e.Event+" "+e.Command)
			}
		}
	}
	for _, ev := range HookEvents {
		if !have[ev] {
			c.Missing = append(c.Missing, ev)
		}
	}
	return c, nil
}
