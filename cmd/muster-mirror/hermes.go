package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/procstat"
)

// HERMES (docs/design/hermes.md, Phase 2; docs/mirroring.md, "Hermes
// windows"): two short-lived commands beside the daemon.
//
//   - `muster-mirror hermes-hook` is the Hermes SHELL HOOK the host's Hermes
//     config registers for pre_llm_call, on_human_input_request,
//     on_human_input_resolved, on_session_end and on_session_finalize. It
//     writes one status file per session (internal/hermeshost's HandleHook),
//     which the daemon reads as the window's status.
//   - `muster-mirror hermes-allowlist` is the shell-hook ALLOWLIST writer the
//     host's Home Manager activation runs after Hermes's own module: it makes
//     exactly the hook's (event, command) pairs present, under Hermes's lock
//     (internal/hermeshost's WriteAllowlist). The daemon never writes a Hermes
//     file.

const (
	envHermesHome      = "MUSTER_MIRROR_HERMES_HOME"
	envHermesStatusDir = "MUSTER_MIRROR_HERMES_STATUS_DIR"
)

// hermesHomeDefault is the daemon's Hermes home: MUSTER_MIRROR_HERMES_HOME,
// else the home a Hermes CLI with this environment uses ($HERMES_HOME, else
// ~/.hermes).
func hermesHomeDefault(getenv func(string) string) string {
	if d := getenv(envHermesHome); d != "" {
		return expandHome(d)
	}
	return hermeshost.DefaultHomeDir(getenv)
}

// hermesStatusDirDefault is the status files' directory:
// MUSTER_MIRROR_HERMES_STATUS_DIR, else hermeshost.DefaultStatusDir; "" when
// it names the home directory and there is none (the daemon then says so and
// reads no status: every Hermes window is unknown).
func hermesStatusDirDefault(getenv func(string) string) string {
	d := getenv(envHermesStatusDir)
	if d == "" {
		d = hermeshost.DefaultStatusDir
	}
	p, ok := homePath(d)
	if !ok {
		return ""
	}
	return p
}

// runHermesHook is `muster-mirror hermes-hook`.
//
// 🔴 IT PRINTS NOTHING TO STDOUT AND ALWAYS EXITS 0: Hermes parses a hook's
// stdout as a directive (a "block" decision, context injected into the
// prompt) and exit code 2 as "block". stdout is not even passed in. Every
// failure — no status directory, a held lock, a bad payload — writes nothing,
// which muster reads as an unknown status, never idle; it is said on stderr,
// which Hermes keeps out of the conversation.
//
// 🔴 IT NEVER READS TMUX_PANE OR THE TTY: it runs in the Hermes GATEWAY,
// whose environment holds the TMUX_PANE of whichever pane auto-started it
// (S2). The only environment it reads names muster's own record
// (MUSTER_MIRROR_HOOK_CONFIG) and the user's home.
//
// Where it writes: the directory the host's daemon recorded in its hook
// config (-write-hook-config), else hermeshost.DefaultStatusDir — the
// daemon's own default, so a host whose daemon records nothing still agrees.
func runHermesHook(getenv func(string) string, in io.Reader, stderr io.Writer, now func() time.Time) (code int) {
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, "muster-mirror hermes-hook: "+f+"\n", a...) }
	defer func() {
		if r := recover(); r != nil {
			logf("internal error, no status written: %v", r)
			code = 0
		}
	}()
	dir := ""
	if recPath, ok := homePath(hookConfigPathRaw(getenv)); ok {
		if rec, err := readHookConfig(recPath); err == nil && rec.HermesStatusDir != "" {
			dir = rec.HermesStatusDir
		}
	}
	if dir == "" {
		var ok bool
		if dir, ok = homePath(hermeshost.DefaultStatusDir); !ok {
			// 🔴 Loudly, and nowhere: never a literal "~" directory under the
			// gateway's working directory.
			logf("HOME is not set and the daemon recorded no status directory: no status written")
			return 0
		}
	}
	// The payload is streamed: no size cap (hermeshost.HandleHook).
	err := hermeshost.HandleHook(in, hermeshost.StatusDir(dir), now(), gatewayWriter())
	switch {
	case err == nil, errors.Is(err, hermeshost.ErrIgnored):
	default:
		logf("no status written (the payload was dropped): %v", err)
	}
	return 0
}

// gatewayWriter is the process running this hook — Hermes's gateway, the
// hook's parent — and its /proc start time: what tells a status file's
// writer from a reused pid.
func gatewayWriter() hermeshost.Writer {
	ppid := os.Getppid()
	w := hermeshost.Writer{PID: ppid}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(ppid), "stat"))
	if err != nil {
		return w
	}
	if st, ok := procstat.Parse(b); ok {
		w.Start = st.Start
	}
	return w
}

// runHermesAllowlist is `muster-mirror hermes-allowlist`: the host's Home
// Manager activation runs it after Hermes's own module, so every Home Manager
// switch makes the hook's pairs present exactly (and drops an older binary's).
func runHermesAllowlist(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muster-mirror hermes-allowlist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("hermes-home", hermesHomeDefault(getenv), "the Hermes home whose shell-hooks-allowlist.json to write ("+envHermesHome+", else $HERMES_HOME, else ~/.hermes)")
	command := fs.String("command", "", "the hook command the host's Hermes config declares, exactly: `<absolute path to muster-mirror> "+hermeshost.HookSubcommand+"` (default: this binary's path)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *command == "" {
		bin, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror hermes-allowlist: find this binary (pass -command): %v\n", err)
			return 2
		}
		*command = hermeshost.HookCommand(bin)
	}
	changed, err := hermeshost.NewHome(*home).WriteAllowlist(*command, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror hermes-allowlist: %s: %v\n", *home, err)
		return 1
	}
	verb := "already allowlisted"
	if changed {
		verb = "allowlisted"
	}
	fmt.Fprintf(stdout, "%s %d hook events for %q in %s\n", verb, len(hermeshost.HookEvents), *command, filepath.Join(*home, "shell-hooks-allowlist.json"))
	return 0
}
