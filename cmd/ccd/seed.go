package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// `ccd seed` prepares CLAUDE_CONFIG_DIR so the interactive CLI starts straight at
// its prompt.
//
// 🔴 WITHOUT IT THE TUI NEVER REACHES A PROMPT, EVEN WITH A VALID TOKEN IN THE
// ENVIRONMENT. Two screens block it, both measured on the pinned CLI with the
// token set: the first-run theme picker (skipped by hasCompletedOnboarding) and
// the workspace trust dialog (skipped by projects[<cwd>].hasTrustDialogAccepted,
// keyed by the EXACT absolute working directory). A prompt pasted into either
// screen is consumed as menu input.
//
// It MERGES rather than overwrites: on a persistent volume the CLI rewrites
// .claude.json with its own state (session bookkeeping, per-project history), and
// a seed that replaced the file on every start would erase it. Only the keys
// below are asserted.
//
// settings.json is IMAGE-OWNED at the top level: every top-level key the
// template names replaces the file's key wholesale (so the hook set is exactly
// the template's), the keys in settingsForbidden are removed, and every other
// key is kept.

type seedOpts struct {
	configDir, workspace, settingsTemplate string
	check                                  bool
}

func runSeed(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o seedOpts
	fs.StringVar(&o.configDir, "config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR to seed")
	fs.StringVar(&o.workspace, "workspace", os.Getenv("CCD_WORKSPACE"), "absolute working directory the CLI runs in")
	fs.StringVar(&o.settingsTemplate, "settings-template", "", "settings.json template whose top-level keys are imposed")
	fs.BoolVar(&o.check, "check", false, "verify an existing seed instead of writing one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var err error
	if o.check {
		err = checkSeed(o)
	} else {
		err = seed(o, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccd seed:", err)
		return 1
	}
	if o.check {
		fmt.Fprintln(stdout, "ccd seed: ok:", filepath.Join(o.configDir, ".claude.json"), "and settings.json have the seeded shape")
	}
	return 0
}

func (o seedOpts) validate() error {
	if o.configDir == "" || !filepath.IsAbs(o.configDir) {
		return errors.New("--config-dir (or CLAUDE_CONFIG_DIR) must be an absolute path")
	}
	if o.workspace == "" || !filepath.IsAbs(o.workspace) {
		return errors.New("--workspace (or CCD_WORKSPACE) must be an absolute path: the trust key is the exact absolute cwd")
	}
	if o.settingsTemplate == "" {
		return errors.New("--settings-template is required")
	}
	return nil
}

func seed(o seedOpts, stderr io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	for _, d := range []string{o.configDir, o.workspace} {
		if err := ensureWritableDir(d, 0o700, stderr); err != nil {
			return err
		}
	}

	stPath := filepath.Join(o.configDir, ".claude.json")
	st, err := readJSONObject(stPath, stderr)
	if err != nil {
		return err
	}
	st["hasCompletedOnboarding"] = true
	if _, ok := st["theme"]; !ok {
		st["theme"] = "dark"
	}
	projects, _ := st["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	proj, _ := projects[filepath.Clean(o.workspace)].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
	}
	proj["hasTrustDialogAccepted"] = true
	projects[filepath.Clean(o.workspace)] = proj
	st["projects"] = projects
	// muster's MCP server (connect.go): registered when the pod is offered the
	// connector store, removed when it is not — so a pod restarted without it
	// does not keep a server entry that can only fail.
	seedMCP(st, os.Getenv(envConnectURL), os.Getenv(envAgentToken))
	if err := writeJSONAtomic(stPath, st); err != nil {
		return err
	}

	tmpl, err := os.ReadFile(o.settingsTemplate)
	if err != nil {
		return err
	}
	var want map[string]any
	if err := json.Unmarshal(tmpl, &want); err != nil {
		return fmt.Errorf("settings template %s: %w", o.settingsTemplate, err)
	}
	setPath := filepath.Join(o.configDir, "settings.json")
	have, err := readJSONObject(setPath, stderr)
	if err != nil {
		return err
	}
	for k, v := range want {
		have[k] = v
	}
	stripForbidden(have, setPath, stderr)
	return writeJSONAtomic(setPath, have)
}

// ensureWritableDir makes dir exist as a directory this process can create files
// in, as os.MkdirAll(dir, perm) does — except that an EMPTY directory on dir's path
// that this process cannot write in, where it needs to, is replaced by one it
// creates itself.
//
// 🔴 WHY: a container runtime creates an image's missing WORKDIR as ROOT, after
// mounting the volumes. The claude-code-agent image up to 0.49.0 had
// `WORKDIR /data/workspace`, so on a FRESH volume at /data the runtime made
// /data/workspace root-owned, mode 0755 (fsGroup only sets its group). Nothing
// failed while the CLI only ran there; once ccd cloned a repository into
// /data/workspace/<repo>, every new agent dispatched with one crash-looped on
// `mkdir /data/workspace/<repo>: permission denied`. The image no longer has that
// WORKDIR, but volumes created by those images keep the root-owned directory —
// this is what lets them start.
//
// The directory checked is the one that must be WRITTEN: dir itself when it
// exists (the CLI works in it), else its deepest existing ancestor (MkdirAll
// creates the rest inside it). Replacing it is os.Remove — rmdir — so it happens
// only when it is empty and its parent is writable by this process: exactly the
// directory this process could have created and removed itself, so no file and
// no other process's directory is ever lost. Anything else (a non-empty
// directory, a read-only parent, the volume's mount point, a SYMLINK to an
// unwritable directory — rmdir's guarantee does not hold through one) is an
// error naming the directory, its owner and mode, and how to recover.
//
// ⚠ BEHAVIOUR CHANGE (#158): a workspace that exists, is not writable and is NOT
// empty used to start (MkdirAll succeeds on an existing directory) and now
// crash-loops on this error. That is deliberate — the CLI could not write in it
// either — and the error says how to recover (unwritableRecovery).
func ensureWritableDir(dir string, perm os.FileMode, stderr io.Writer) error {
	dir = filepath.Clean(dir)
	p := dir
	var lfi os.FileInfo
	for {
		var err error
		if lfi, err = os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	fi, err := os.Stat(p)
	if err == nil && fi.IsDir() && unix.Access(p, unix.W_OK|unix.X_OK) != nil {
		owner, ownerUID := "", -1
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			owner, ownerUID = fmt.Sprintf("owner %d:%d, ", st.Uid, st.Gid), int(st.Uid)
		}
		// 🔴 A SYMLINK IS NEVER REPLACED. os.Remove on a symlink unlinks the LINK
		// whatever its target holds, so the rmdir-only guarantee above would not
		// hold through one: a workspace symlinked to a non-empty directory would
		// be "replaced" by an empty one, the target's files detached from it.
		// The mode checked is the link's own (Lstat); fi is its target's.
		if lfi != nil && lfi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink to a directory this process (uid %d) cannot create files in (%smode %s); "+
				"a symlink is never replaced. %s", p, os.Getuid(), owner, fi.Mode(), unwritableRecovery(p, ownerUID))
		}
		if rerr := os.Remove(p); rerr != nil {
			return fmt.Errorf("%s is a directory this process (uid %d) cannot create files in (%smode %s), and it "+
				"could not be replaced (%v). %s", p, os.Getuid(), owner, fi.Mode(), rerr, unwritableRecovery(p, ownerUID))
		}
		fmt.Fprintf(stderr, "ccd seed: replaced %s: it was an empty directory this process (uid %d) could not "+
			"create files in (%smode %s) — a container runtime creates a missing image WORKDIR as root on a fresh volume\n",
			p, os.Getuid(), owner, fi.Mode())
	}
	return os.MkdirAll(dir, perm)
}

// unwritableRecovery says how to recover from a directory ensureWritableDir
// refused to replace. 🔴 The pod never starts (the entrypoint's seed fails first,
// so the container crash-loops), so `kubectl exec` into it is impossible: the fix
// is made from ANOTHER pod that mounts the agent's volume (docs/provisioner.md).
func unwritableRecovery(p string, ownerUID int) string {
	fix := fmt.Sprintf("`chown %d %s` as root", os.Getuid(), p)
	if ownerUID == os.Getuid() {
		fix = fmt.Sprintf("`chmod u+w %s`", p)
	}
	return fmt.Sprintf("The agent's pod cannot start until it is fixed, so `kubectl exec` into it is impossible: "+
		"from a debug pod that mounts the agent's volume (PVC <agent>-workspace) at /data, run %s "+
		"(see docs/provisioner.md, \"A workspace ccd cannot write in\")", fix)
}

// settingsForbidden are the settings.json keys REMOVED from the persisted file,
// whatever wrote them (an operator's /config, a session editing its own config):
// by `ccd seed` when the pod starts, and by stripSettingsFile before EVERY start
// and in-pod restart of the CLI by the supervisor (supervise.go). `env` is applied
// to the CLI's environment, so it can set exactly the variables the entrypoint
// refuses (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, CLAUDE_CODE_USE_BEDROCK, …);
// `apiKeyHelper` is a command whose output the CLI uses as its API key. The
// template names neither, so "template keys replace the file's" alone never
// removed them.
//
// ⚠ SCOPE, IN FILES: <CLAUDE_CONFIG_DIR>/settings.json only. The settings*.json
// files under the WORKSPACE's .claude/ are project configuration the CLI also
// reads, and nothing here touches or checks them.
// ⚠ SCOPE, IN TIME: the removal happens before a CLI starts. A key written while
// a CLI is running is not removed until the next start, and whether the RUNNING
// CLI re-reads settings.json and applies it before then was not measured.
var settingsForbidden = []string{"env", "apiKeyHelper"}

// stripForbidden deletes settingsForbidden from a settings object, naming each key
// it removes (never its value), and reports whether it removed any.
func stripForbidden(settings map[string]any, path string, stderr io.Writer) bool {
	removed := false
	for _, k := range settingsForbidden {
		if _, ok := settings[k]; ok {
			delete(settings, k)
			removed = true
			fmt.Fprintf(stderr, "ccd seed: removed %q from %s (it can carry a credential or endpoint that "+
				"outranks the session's subscription token)\n", k, path)
		}
	}
	return removed
}

// stripSettingsFile applies stripForbidden to <configDir>/settings.json in place.
// The file is rewritten only when a key was removed; a missing file is left
// missing. The supervisor runs it before every CLI start.
//
// A settings.json that is not a JSON object is an ERROR here, not moved aside as
// `ccd seed` does: moving it would leave the CLI with no hook template. The
// supervisor answers the error by not starting the CLI — before the first start
// ccd exits; before a restart the pane stays dead until crash_loop — so either way
// the pod restarts, and the entrypoint's `ccd seed` moves the file aside and
// writes the template.
func stripSettingsFile(configDir string, stderr io.Writer) error {
	path := filepath.Join(configDir, "settings.json")
	settings, err := readJSONObject(path, nil)
	if err != nil {
		return err
	}
	if !stripForbidden(settings, path, stderr) {
		return nil
	}
	return writeJSONAtomic(path, settings)
}

// checkSeed verifies the shape `seed` produces, for the image's smoke test.
func checkSeed(o seedOpts) error {
	if err := o.validate(); err != nil {
		return err
	}
	st, err := readJSONObject(filepath.Join(o.configDir, ".claude.json"), nil)
	if err != nil {
		return err
	}
	if st["hasCompletedOnboarding"] != true {
		return errors.New(".claude.json: hasCompletedOnboarding is not true (the theme picker will block the TUI)")
	}
	projects, _ := st["projects"].(map[string]any)
	proj, _ := projects[filepath.Clean(o.workspace)].(map[string]any)
	if proj == nil || proj["hasTrustDialogAccepted"] != true {
		return fmt.Errorf(".claude.json: projects[%q].hasTrustDialogAccepted is not true (the trust dialog will block the TUI)", o.workspace)
	}
	servers, _ := st["mcpServers"].(map[string]any)
	_, registered := servers[mcpServerName]
	if offered := os.Getenv(envConnectURL) != "" && os.Getenv(envAgentToken) != ""; offered != registered {
		return fmt.Errorf(".claude.json: muster's MCP server registered=%v but the connector store offered=%v (%s)",
			registered, offered, envConnectURL)
	}
	tmpl, err := os.ReadFile(o.settingsTemplate)
	if err != nil {
		return err
	}
	var want map[string]any
	if err := json.Unmarshal(tmpl, &want); err != nil {
		return err
	}
	have, err := readJSONObject(filepath.Join(o.configDir, "settings.json"), nil)
	if err != nil {
		return err
	}
	for k, v := range want {
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(have[k])
		if string(a) != string(b) {
			return fmt.Errorf("settings.json: key %q differs from the template", k)
		}
	}
	for _, k := range settingsForbidden {
		if _, ok := have[k]; ok {
			return fmt.Errorf("settings.json: carries %q, which seed removes", k)
		}
	}
	return nil
}

// readJSONObject reads a JSON object file; a missing file is an empty object. A
// file that is not a JSON object is moved aside (when stderr is non-nil) rather
// than blocking every future start of the pod on a hand-repair.
func readJSONObject(path string, stderr io.Writer) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		if stderr == nil {
			return nil, fmt.Errorf("%s is not a JSON object", path)
		}
		aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("%s is not a JSON object and could not be moved aside: %v", path, rerr)
		}
		fmt.Fprintf(stderr, "ccd seed: %s was not a JSON object; moved to %s and starting fresh\n", path, aside)
		return map[string]any{}, nil
	}
	return m, nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ccd-seed-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
