package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// printOpenCode is `-list -opencode`: diagnose this host's opencode windows —
// the token (by fingerprint) and whether the server accepts it, then each
// opencode window: its pane, TUI process, pane title, the session the title
// matched ("matched by title") and why a window has no rendered view.
//
// It sends the server one GET (/api/info) besides the listing's own reads,
// and prints no secret: the token by fingerprint only.
func printOpenCode(ctx context.Context, backend mirrorhost.Backend, creds *opencodehost.Credentials, why string, stdout, stderr io.Writer) int {
	switch {
	case creds == nil:
		fmt.Fprintf(stdout, "token\tnone\t%s\n", why)
	default:
		from := creds.Path
		if from == "" {
			from = envOpenCodeToken
		}
		left := time.Until(creds.Expires()).Round(time.Hour)
		fmt.Fprintf(stdout, "token\t%s\t%s\texpires=%s\tin=%s\n", from, opencodehost.Fingerprint(creds.Token()),
			creds.Expires().UTC().Format(time.RFC3339), left)
		cl, err := creds.Client()
		var version string
		if err == nil {
			ictx, cancel := context.WithTimeout(ctx, 3*time.Second)
			version, err = cl.Info(ictx)
			cancel()
		}
		switch {
		case errors.Is(err, opencodehost.ErrUnauthorized):
			fmt.Fprintf(stdout, "server\t%s\tREFUSED the token (expired, or the server password was rotated): run muster-mirror opencode-pair\n", creds.URL())
		case err != nil:
			fmt.Fprintf(stdout, "server\t%s\tunreachable: %v\n", creds.URL(), err)
		default:
			fmt.Fprintf(stdout, "server\t%s\tok\tversion=%s\n", creds.URL(), version)
		}
	}
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
		if a == nil || a.Runtime != mirror.RuntimeOpenCode {
			continue
		}
		n++
		fmt.Fprintf(stdout, "window\t%s\t%s\tpane=%s\ttui=%d\ttty=%s\tsession=%s\tstatus=%s\tsource=%s\n",
			w.Session, w.ID, a.Pane, a.PID, orDash(w.TTY), orDash(a.Gen), stateOrUnknown(a.Status), a.Source)
		oc := a.OpenCode
		if oc == nil {
			fmt.Fprintln(stdout, "  resolved\tno (this muster-mirror resolves no opencode sessions)")
			continue
		}
		if oc.PaneTitle != "" {
			fmt.Fprintf(stdout, "  pane-title\t%q\n", oc.PaneTitle)
		}
		if oc.Match.Session != "" {
			how := "matched by title"
			if oc.Match.Truncated {
				how += " (the title is cut: a prefix match)"
			}
			fmt.Fprintf(stdout, "  match\t%s\t%s\n", oc.Match.Session, how)
		}
		if oc.NoView != "" {
			fmt.Fprintf(stdout, "  no-view\t%s\n", oc.NoView)
		}
	}
	fmt.Fprintf(stdout, "opencode-windows\t%s\n", strconv.Itoa(n))
	return 0
}
