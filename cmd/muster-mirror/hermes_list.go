package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// printHermes is `-list -hermes` (docs/design/hermes.md, "Listing"; docs/
// mirroring.md): what this host knows about its Hermes windows, for a person
// asking why one has no rendered view or no status.
//
// The host part: the Hermes home and the status directory, and the HOOK's
// CONSENT — whether every (event, command) pair the hook needs is in the
// home's shell-hooks-allowlist.json. A pair that is missing makes the gateway
// skip that hook and every new Hermes CLI stop on `[y/N]`; the windows whose
// status that hook would write read as unknown. (The configured hooks are in
// config.yaml, which muster never reads.)
//
// Then each Hermes window: its pane, CLI process and terminal; the breadcrumb;
// the session and its lineage; the newest status file; the status muster
// reports, and why a window has no rendered view or an unknown status.
//
// Not reported: the gateway's code_sha, which needs the gateway viewer
// (Phase 4).
func printHermes(ctx context.Context, backend mirrorhost.Backend, home, statusDir, command string, stdout, stderr io.Writer) int {
	if command == "" {
		bin, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: find this binary (pass -hermes-hook-command): %v\n", err)
			return 1
		}
		command = hermeshost.HookCommand(bin)
	}
	fmt.Fprintf(stdout, "hermes-home\t%s\n", home)
	fmt.Fprintf(stdout, "status-dir\t%s\n", statusDir)
	fmt.Fprintf(stdout, "hook-command\t%s\n", command)
	c, err := hermeshost.NewHome(home).CheckAllowlist(command)
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "hook-consent\tunreadable\t%v\n", err)
	case c.OK():
		fmt.Fprintf(stdout, "hook-consent\tok\t%d events allowlisted\n", len(hermeshost.HookEvents))
	default:
		fmt.Fprintf(stdout, "hook-consent\tMISSING\t%s\t(the gateway skips these hooks, and every new Hermes CLI stops on `Allow this hook to run? [y/N]`; run `muster-mirror hermes-allowlist -command %q`)\n",
			strings.Join(c.Missing, ","), command)
	}
	for _, o := range c.Other {
		fmt.Fprintf(stdout, "hook-other\t%s\t(a muster-mirror hermes-hook for another binary: the Hermes config may name a path other than %q)\n", o, command)
	}
	fmt.Fprintln(stdout, "gateway\tcode_sha not checked (the gateway viewer is Phase 4)")

	wl, ok := mirrorhost.WindowListerOf(backend)
	if !ok {
		fmt.Fprintln(stderr, "muster-mirror: this backend lists sessions only, not windows")
		return 1
	}
	windows, err := wl.Windows(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
		return 1
	}
	n := 0
	for _, w := range windows {
		a := w.Agent
		if a == nil || a.Runtime != mirror.RuntimeHermes {
			continue
		}
		n++
		fmt.Fprintf(stdout, "window\t%s\t%s\tpane=%s\tcli=%d\ttty=%s\tsession=%s\tstatus=%s\n",
			w.Session, w.ID, a.Pane, a.PID, orDash(w.TTY), orDash(a.Gen), stateOrUnknown(a.Status))
		ps := a.Hermes
		if ps == nil {
			fmt.Fprintln(stdout, "  resolved\tno (this muster-mirror has no Hermes home configured)")
			continue
		}
		if ps.Breadcrumb.SessionID != "" {
			fmt.Fprintf(stdout, "  breadcrumb\t%s\tts=%s\n", ps.Breadcrumb.SessionID,
				time.Unix(0, int64(ps.Breadcrumb.TS*1e9)).UTC().Format(time.RFC3339))
		} else {
			fmt.Fprintln(stdout, "  breadcrumb\tnone")
		}
		if len(ps.Lineage) > 0 {
			tip := ps.Lineage.Tip()
			end := "open"
			if tip.Ended {
				end = "ended:" + orDash(tip.EndReason)
			}
			fmt.Fprintf(stdout, "  lineage\t%d session(s)\ttip=%s\t%s\n", len(ps.Lineage), tip.ID, end)
			if f, found := hermeshost.StatusDir(statusDir).Newest(ctx, ps.Lineage.IDs()); found {
				fmt.Fprintf(stdout, "  status-file\t%s\t%s\tsince=%s\twriter=%d\n", f.SessionID, f.Status,
					time.UnixMilli(f.Since).UTC().Format(time.RFC3339), f.WriterPID)
			} else {
				fmt.Fprintln(stdout, "  status-file\tnone")
			}
		}
		if ps.NoView != "" {
			fmt.Fprintf(stdout, "  no-view\t%s\n", ps.NoView)
		}
		if ps.Status.Reason != "" {
			fmt.Fprintf(stdout, "  unknown\t%s\n", ps.Status.Reason)
		}
	}
	fmt.Fprintf(stdout, "hermes-windows\t%s\n", strconv.Itoa(n))
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func stateOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
