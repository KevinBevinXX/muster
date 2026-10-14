// Command muster-mirror shows this machine's tmux sessions in muster's Agents
// view as MIRRORED sessions — one card per tmux WINDOW. It is READ-ONLY unless started with -interactive. It is the optional host half of the mirror plugin: install it
// only on the machines whose sessions muster should show, and only against a
// muster running with MUSTER_MIRROR=1.
//
// It dials OUT to muster and opens no listener. Its one credential is this
// host's mirror token, minted on the muster side with
//
//	muster-server mirror token create <host>
//
// Usage:
//
//	muster-mirror -url https://muster.example.com -token-file ~/.config/muster-mirror/token
//
// Every flag also reads an environment variable (named in -help), so a systemd
// unit can configure it without putting the token in argv.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// e2eBackend replaces the tmux backend in a browser-test build (e2emirror.go,
// `-tags e2emirror`). nil in every shipped binary.
var e2eBackend func() mirrorhost.Backend

// buildRevision is stamped by the build (-X main.buildRevision=…).
var buildRevision = "dev"

const (
	envURL         = "MUSTER_MIRROR_URL"
	envToken       = "MUSTER_MIRROR_TOKEN"
	envTokenFile   = "MUSTER_MIRROR_TOKEN_FILE"
	envHost        = "MUSTER_MIRROR_HOST"
	envSocket      = "MUSTER_MIRROR_TMUX_SOCKET"
	envSocketPath  = "MUSTER_MIRROR_TMUX_SOCKET_PATH"
	envInteractive = "MUSTER_MIRROR_INTERACTIVE"
	envAllowHTTP   = "MUSTER_MIRROR_ALLOW_HTTP"
	envLabelsFile  = "MUSTER_MIRROR_LABELS_FILE"
	envPaneMapDir  = "MUSTER_MIRROR_PANE_MAP_DIR"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// defaultHost is this machine's short hostname, lowercased: what a token is
// minted for unless -host says otherwise.
func defaultHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	return h
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muster-mirror", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envBool := func(k string) bool { v := getenv(k); return v == "1" || strings.EqualFold(v, "true") }
	or := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	url := fs.String("url", getenv(envURL), "muster's base URL, https://… ("+envURL+")")
	tokenFile := fs.String("token-file", getenv(envTokenFile), "file holding this host's mirror token ("+envTokenFile+"; or the token itself in "+envToken+")")
	host := fs.String("host", or(getenv(envHost), defaultHost()), "this host's name; the token must have been minted for it ("+envHost+")")
	socket := fs.String("tmux-socket", getenv(envSocket), "tmux -L socket name; empty = the default server ("+envSocket+")")
	socketPath := fs.String("tmux-socket-path", getenv(envSocketPath), "tmux -S socket path ("+envSocketPath+")")
	interactive := fs.Bool("interactive", envBool(envInteractive), "let muster open INTERACTIVE terminals on this host — effectively a SHELL here: an interactive tmux client can switch to any session or open a new window (default: read-only; muster then offers no Interactive switch) ("+envInteractive+")")
	allowHTTP := fs.Bool("allow-http", envBool(envAllowHTTP), "TESTS AND LOCAL DEVELOPMENT ONLY: permit a plain http:// muster URL, which sends the token in cleartext ("+envAllowHTTP+")")
	labelsFile := fs.String("labels-file", getenv(envLabelsFile), "JSON object {\"<session>\": \"<codename>\"}: muster shows such a session as <codename>:<active window index> instead of its name; unreadable or invalid = no labels, logged, never fatal ("+envLabelsFile+")")
	paneMapDir := fs.String("pane-map-dir", getenv(envPaneMapDir), "directory of per-pane Claude Code records (claude-p<N>.json for pane %N, written by a hook on every event) that the rendered view resolves a pane's transcript from FIRST, each validated against the process in the pane; empty = not consulted ("+envPaneMapDir+")")
	list := fs.Bool("list", false, "print the sessions this host would register, and exit (a labelled session's line ends in a label=<label> column)")
	listClaude := fs.Bool("claude", false, "with -list: print, for each WINDOW whose active pane runs Claude Code, what its rendered view would stream (one line each: session, window id, index, claude, pane, state, session id, transcript, source = pane-map|sessions|guessed, then label=<label>); per session on a backend that does not list windows")
	listWindows := fs.Bool("windows", false, "with -list: print every WINDOW this host would register instead (one line each: session, window id, index, active|-, command, then claude <pane> <state> <session id> <transcript> <source> for a Claude Code window, then label=<label>)")
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *version {
		fmt.Fprintf(stdout, "muster-mirror %s (tunnel protocol %d)\n", buildRevision, mirror.ProtocolVersion)
		return 0
	}

	backend := newTmuxBackend(*socket, *socketPath, *paneMapDir)
	if e2eBackend != nil {
		backend = e2eBackend()
	}
	// No attach shrinks a session's window; the browser scrolls instead
	// (internal/termsize). Wrapped AFTER the e2e swap, so the browser suite
	// drives the same wrapper the shipped binary runs.
	backend = mirrorhost.NoShrink(backend)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := log.New(stderr, "", log.LstdFlags)
	codenames := loadCodenames(*labelsFile, logger)

	if *list && *listWindows {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printWindows(lctx, backend, codenames, false, stdout, stderr)
	}
	// -list -claude is PER WINDOW: a session's windows each run their own
	// Claude Code, and the rendered view resolves each on its own — a row per
	// session showed only the active window's.
	if _, ok := mirrorhost.WindowListerOf(backend); ok && *list && *listClaude {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printWindows(lctx, backend, codenames, true, stdout, stderr)
	}
	if *list {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sessions, err := backend.List(lctx)
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
			return 1
		}
		var claude map[string]mirrorhost.ClaudePane
		if cb, ok := mirrorhost.ClaudeOf(backend); ok && *listClaude {
			claude = cb.ClaudePanes(lctx, sessions)
		}
		labels, lerr := mirrorhost.SessionLabels(lctx, backend, codenames, sessions)
		if lerr != nil {
			logger.Printf("muster-mirror: read active windows for labels (labelling by codename alone): %v", lerr)
		}
		for _, s := range sessions {
			// The label is the LAST column, so the columns before it keep
			// their positions whether or not a session has one.
			suffix := ""
			if l, ok := labels[s]; ok {
				suffix = "\tlabel=" + l
			}
			if p, ok := claude[s]; ok {
				state := "idle"
				if p.Busy {
					state = "busy"
				}
				fmt.Fprintf(stdout, "%s\tclaude\t%s\t%s\t%s\t%s\t%s%s\n", s, p.Pane, state, p.Gen, p.Transcript, p.Source, suffix)
				continue
			}
			fmt.Fprintln(stdout, s+suffix)
		}
		return 0
	}

	token := strings.TrimSpace(getenv(envToken))
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: read the token file: %v\n", err)
			return 2
		}
		token = strings.TrimSpace(string(b))
	}
	if *url == "" || token == "" {
		fmt.Fprintf(stderr, "muster-mirror: -url (%s) and a token (-token-file / %s / %s) are required\n", envURL, envTokenFile, envToken)
		return 2
	}

	c, err := mirrorhost.New(mirrorhost.Config{
		URL: *url, Token: token, Host: *host, Backend: backend, Codenames: codenames,
		Interactive: *interactive, AllowHTTP: *allowHTTP, Logger: logger,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	mode := "read-only"
	if *interactive {
		mode = "INTERACTIVE (-interactive)"
	}
	logger.Printf("muster-mirror %s: host %s → %s, %s", buildRevision, *host, *url, mode)
	// A muster-mirror that was killed between creating a viewer session and
	// attaching to it left that session behind; nothing else would remove it.
	if vc, ok := mirrorhost.ViewCleanerOf(backend); ok {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ended, err := vc.CleanViewSessions(cctx)
		cancel()
		if len(ended) > 0 {
			logger.Printf("muster-mirror: removed %d stale viewer session(s): %q", len(ended), ended)
		}
		if err != nil {
			logger.Printf("muster-mirror: clean up stale viewer sessions: %v", err)
		}
	}
	if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("muster-mirror: %v", err)
		return 1
	}
	return 0
}

// newTmuxBackend is tmuxBackend; a test replaces it to see what run passes.
var newTmuxBackend = tmuxBackend

// tmuxBackend is the host's tmux, with its rendered-view lookup reading the pane
// map in paneMapDir ("" = none).
func tmuxBackend(socket, socketPath, paneMapDir string) mirrorhost.Backend {
	return mirrorhost.Tmux{Socket: socket, SocketPath: socketPath,
		Claude: mirrorhost.ClaudeLocator{PaneMapDir: paneMapDir}}
}

// printWindows is `-list -windows`: every window this host would register — or,
// with claudeOnly (`-list -claude`), each window whose active pane runs Claude
// Code, without the active/command columns.
func printWindows(ctx context.Context, backend mirrorhost.Backend, codenames map[string]string, claudeOnly bool, stdout, stderr io.Writer) int {
	wl, ok := mirrorhost.WindowListerOf(backend)
	if !ok {
		fmt.Fprintln(stderr, "muster-mirror: this backend lists sessions only, not windows")
		return 1
	}
	raw, err := wl.Windows(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
		return 1
	}
	labels := map[string]string{}
	for _, w := range mirrorhost.WireWindows(raw, codenames, true, time.Now()) {
		labels[w.Session+"\x00"+w.ID] = w.Label
	}
	for _, w := range raw {
		active := "-"
		if w.Active {
			active = "active"
		}
		if claudeOnly && w.Claude == nil {
			continue
		}
		line := fmt.Sprintf("%s\t%s\t%d\t%s\t%s", w.Session, w.ID, w.Index, active, w.Command)
		if claudeOnly {
			line = fmt.Sprintf("%s\t%s\t%d", w.Session, w.ID, w.Index)
		}
		if p := w.Claude; p != nil {
			state := "idle"
			if p.Busy {
				state = "busy"
			}
			line += fmt.Sprintf("\tclaude\t%s\t%s\t%s\t%s\t%s", p.Pane, state, p.Gen, p.Transcript, p.Source)
		}
		if l := labels[w.Session+"\x00"+w.ID]; l != "" {
			line += "\tlabel=" + l
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// loadCodenames reads the labels file at path ("" = none configured). It is
// NEVER fatal: a missing, unreadable or invalid file is logged once, here at
// start, and the host runs with no labels — muster then shows raw session
// names, exactly as before labels existed. Entries it had to drop are named.
func loadCodenames(path string, logger *log.Logger) map[string]string {
	if path == "" {
		return nil
	}
	codenames, skipped, err := mirrorhost.LoadCodenames(path)
	if err != nil {
		logger.Printf("muster-mirror: no session labels: %v", err)
		return nil
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		logger.Printf("muster-mirror: the labels file %s has %d invalid entr(y/ies), ignored: %q", path, len(skipped), skipped)
	}
	logger.Printf("muster-mirror: %d session label(s) from %s", len(codenames), path)
	return codenames
}
