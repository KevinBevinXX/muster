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

// allowlistLockWait bounds the wait for Hermes's lock.
const allowlistLockWait = 10 * time.Second

// ErrAllowlistUnreadable: the current allowlist is not a JSON object with an
// approvals list. The writer refuses to replace it (it would drop whatever
// the person approved); Hermes itself treats it as empty.
var ErrAllowlistUnreadable = errors.New("the shell-hook allowlist is not a JSON object with an approvals list; fix or remove it by hand")

// WriteAllowlist makes muster's (event, command) pairs — every HookEvents
// event with command — present in h's allowlist exactly once, removes
// muster-mirror hermes-hook entries for any OTHER command (an older binary's
// path), and keeps every other entry and key as it is. changed is false when
// the file already said exactly that, and then it is not rewritten.
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

	doc := map[string]any{}
	b, err := h.readAllowlist()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	default:
		if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &doc) != nil || doc == nil {
			return false, ErrAllowlistUnreadable
		}
	}
	var approvals []any
	if raw, ok := doc["approvals"]; ok {
		if approvals, ok = raw.([]any); !ok {
			return false, ErrAllowlistUnreadable
		}
	}
	want := map[string]bool{}
	for _, e := range HookEvents {
		want[e] = true
	}
	have := map[string]bool{}
	var kept []any
	for _, raw := range approvals {
		e, ok := raw.(map[string]any)
		event, _ := e["event"].(string)
		cmd, _ := e["command"].(string)
		switch {
		case ok && want[event] && cmd == command:
			if have[event] {
				changed = true // a duplicate of ours
				continue
			}
			have[event] = true
		case ok && musterHookCommand(cmd) && cmd != command:
			changed = true // an older muster-mirror's
			continue
		}
		kept = append(kept, raw)
	}
	mtime := scriptMtime(command)
	for _, ev := range HookEvents {
		if have[ev] {
			continue
		}
		changed = true
		kept = append(kept, map[string]any{"event": ev, "command": command,
			"approved_at": now.UTC().Format("2006-01-02T15:04:05.000000Z"), "script_mtime_at_approval": mtime})
	}
	if !changed {
		return false, nil
	}
	if kept == nil {
		kept = []any{}
	}
	doc["approvals"] = kept
	// Keys sorted and indented by two, as Hermes writes it (json.dump with
	// sort_keys, indent=2), and no HTML escaping: another entry's `&&` stays
	// `&&`.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return false, err
	}
	// Hermes's own temp-file prefix is ".shell-hooks-allowlist_"; any unique
	// name in the same directory renames the same way.
	return true, writeFileAtomic(filepath.Join(dir, filepath.Base(path)), out.Bytes())
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
	b, err := h.readAllowlist()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		var doc struct {
			Approvals []struct {
				Event   string `json:"event"`
				Command string `json:"command"`
			} `json:"approvals"`
		}
		if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &doc) != nil {
			return c, ErrAllowlistUnreadable
		}
		for _, e := range doc.Approvals {
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
