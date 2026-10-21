package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// printViMode is `-list -hermes`'s vi-mode line: whether this host's Hermes
// runs in vi mode (display.vim_mode), in which case the host never sends to
// it, -hermes-send or not.
func printViMode(h hermeshost.Home, stdout io.Writer) {
	switch on, err := h.ViMode(); {
	case err != nil:
		fmt.Fprintf(stdout, "vi-mode\tunreadable (%v): muster sends nothing to this Hermes\n", err)
	case on:
		fmt.Fprintln(stdout, "vi-mode\ton (display.vim_mode): muster never sends to this Hermes, -hermes-send or not")
	default:
		fmt.Fprintln(stdout, "vi-mode\toff")
	}
}

// printGateway is `-list -hermes`'s gateway line: the home's gateway as its
// control socket's identify names it, checked as the viewer checks it before
// attaching (hermeshost.Home.Identify) — the pin above all. It asks identify
// only: no ticket, no WebSocket, nothing answered.
func printGateway(ctx context.Context, h hermeshost.Home, stdout io.Writer) {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	g, err := h.Identify(gctx)
	switch {
	case errors.Is(err, hermeshost.ErrNoGateway):
		fmt.Fprintln(stdout, "gateway\tnone running (a Hermes CLI starts one; until then there is nothing to check)")
	case errors.Is(err, hermeshost.ErrGatewayNotPin):
		fmt.Fprintf(stdout, "gateway\tNOT THE PIN\t%v: muster attaches no viewer, so every Hermes approval is answered in the terminal\n", err)
	case err != nil:
		fmt.Fprintf(stdout, "gateway\trefused\t%v: muster attaches no viewer, so every Hermes approval is answered in the terminal\n", err)
	default:
		fmt.Fprintf(stdout, "gateway\tcode_sha %s (the pin)\tpid=%d\tinstance=%s\n", g.CodeSHA, g.PID, g.InstanceID)
	}
}

// printHermes is `-list -hermes` (docs/design/hermes.md, "Listing"; docs/
// mirroring.md): what this host knows about its Hermes windows, for a person
// asking why one has no rendered view or no status.
//
// The host part: the Hermes home and the status directory, and the HOOK's
// CONSENT (printHookConsent) — for each muster-mirror hook command Hermes's
// config.yaml DECLARES, whether its (event, command) pairs are in the home's
// shell-hooks-allowlist.json. A pair that is missing makes the gateway skip
// that hook and every new Hermes CLI stop on `[y/N]`; the windows whose
// status that hook would write read as unknown.
//
// Then each Hermes window: its pane, CLI process and terminal; the breadcrumb;
// the session and its lineage; the newest status file; the status muster
// reports, and why a window has no rendered view or an unknown status.
//
// And the home's GATEWAY (printGateway): whether one is running, and whether
// it is the pinned commit muster's viewer attaches to.
func printHermes(ctx context.Context, backend mirrorhost.Backend, home, statusDir string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "hermes-home\t%s\n", home)
	fmt.Fprintf(stdout, "status-dir\t%s\n", orDash(statusDir))
	if bin, err := os.Executable(); err == nil {
		fmt.Fprintf(stdout, "this-binary\t%s\n", hermeshost.HookCommand(bin))
	}
	printHookConsent(hermeshost.NewHome(home), home, stdout)
	printViMode(hermeshost.NewHome(home), stdout)
	printGateway(ctx, hermeshost.NewHome(home), stdout)

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

// printHookConsent reports consent PER DECLARED COMMAND.
//
// 🔴 A FIX IS PRINTED ONLY FOR THE COMMAND HERMES DECLARES, and only when it
// is the one muster-mirror command declared: the allowlist writer removes
// every OTHER muster-mirror hermes-hook entry (each Nix upgrade leaves one
// behind), so a fix naming any other command — this binary's path, say —
// would REMOVE the declared command's consent. With two declared, no fix is
// safe, and none is printed.
func printHookConsent(h hermeshost.Home, home string, stdout io.Writer) {
	declared, derr := h.DeclaredHooks()
	byCmd := map[string][]string{}
	var cmds []string
	switch {
	case derr != nil:
		fmt.Fprintf(stdout, "hook-config\tunreadable\t%v\n", derr)
	default:
		for _, d := range declared {
			if !hermeshost.MusterHookCommand(d.Command) {
				continue
			}
			fmt.Fprintf(stdout, "hook-declared\t%s\t%s\n", d.Event, d.Command)
			if _, ok := byCmd[d.Command]; !ok {
				cmds = append(cmds, d.Command)
			}
			byCmd[d.Command] = append(byCmd[d.Command], d.Event)
		}
		switch len(cmds) {
		case 0:
			fmt.Fprintln(stdout, "hook-config\tnone\tconfig.yaml declares no muster-mirror hermes-hook: no Hermes window has a status (see docs/mirroring.md, \"Installing the Hermes status hook\")")
		case 1:
		default:
			fmt.Fprintf(stdout, "hook-config\tseveral\t%d muster-mirror hermes-hook commands are declared; the allowlist writer keeps one, so fix config.yaml first\n", len(cmds))
		}
	}
	other := map[string]bool{}
	for _, c := range cmds {
		check, err := h.CheckAllowlist(c)
		if err != nil {
			fmt.Fprintf(stdout, "hook-consent\tunreadable\t%v\n", err)
			return
		}
		missing := map[string]bool{}
		for _, ev := range check.Missing {
			missing[ev] = true
		}
		var lacking []string
		for _, ev := range byCmd[c] {
			if missing[ev] {
				lacking = append(lacking, ev)
			}
		}
		if len(lacking) == 0 {
			fmt.Fprintf(stdout, "hook-consent\tok for %s\n", c)
		} else {
			fix := ""
			if len(cmds) == 1 {
				fix = fmt.Sprintf("\tfix: muster-mirror hermes-allowlist -hermes-home %q -command %q", home, c)
			}
			fmt.Fprintf(stdout, "hook-consent\tMISSING for %s\t%s\t(the gateway skips these hooks, and every new Hermes CLI stops on `Allow this hook to run? [y/N]`)%s\n",
				c, strings.Join(lacking, ","), fix)
		}
		var undeclared []string
		have := map[string]bool{}
		for _, ev := range byCmd[c] {
			have[ev] = true
		}
		for _, ev := range hermeshost.HookEvents {
			if !have[ev] {
				undeclared = append(undeclared, ev)
			}
		}
		if len(undeclared) > 0 {
			fmt.Fprintf(stdout, "hook-undeclared\t%s\t%s\t(no status is written on these events)\n", c, strings.Join(undeclared, ","))
		}
		for _, o := range check.Other {
			other[o] = true
		}
	}
	declaredSet := map[string]bool{}
	for _, c := range cmds {
		declaredSet[c] = true
	}
	var others []string
	for o := range other {
		if _, cmd, _ := strings.Cut(o, " "); !declaredSet[cmd] {
			others = append(others, o)
		}
	}
	sort.Strings(others)
	for _, o := range others {
		fmt.Fprintf(stdout, "hook-other\t%s\t(allowlisted, not declared: an older binary's; the next hermes-allowlist removes it)\n", o)
	}
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
