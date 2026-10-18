// Command muster-mirror shows this machine's tmux sessions in muster's Agents
// view as MIRRORED sessions — one card per tmux WINDOW. It is READ-ONLY unless started with -interactive. It is the optional host half of the mirror plugin: install it
// only on the machines whose sessions muster should show, and only against a
// muster running with MUSTER_MIRROR=1.
//
// It dials OUT to muster and opens no listener — unless -mcp-listen is set,
// which serves a LOOPBACK-only MCP endpoint for this host's own Claude Code
// sessions (internal/mirrorhost's MCPProxy; docs/attention.md). Its one credential is this
// host's mirror token, minted on the muster side with
//
//	muster-server mirror token create <host>
//
// Usage:
//
//	muster-mirror -url https://muster.example.com -token-file ~/.config/muster-mirror/token
//
// `muster-mirror ask` is a different, short-lived command: Claude Code's
// PreToolUse hook for AskUserQuestion (ask.go).
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
	"path/filepath"
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

// e2eNoPromptIndex, in a browser-test build, makes the host behave as one that
// predates the prompt index (mirrorhost.Config.NoPromptIndex). nil in every
// shipped binary.
var e2eNoPromptIndex func() bool

// e2eNoCatalog, in a browser-test build, makes the host behave as one that
// predates the composer menu's catalog (mirrorhost.Config.NoCatalog). nil in
// every shipped binary.
var e2eNoCatalog func() bool

// e2eRefresh, in a browser-test build, is how often the host re-reads its
// windows (mirrorhost.Config.Refresh; 0 = mirrorhost's default) — so a spec
// waiting for a status flip to reach muster waits a fraction of a second, not
// the default refresh. nil in every shipped binary, which therefore has no way
// to change its refresh at all (TestTheShippedBuildCannotChangeItsRefresh).
var e2eRefresh func() time.Duration

// e2eWrap, in a browser-test build, wraps the backend AFTER NoShrink: a
// capability added there (e2emirror.go's copy-mode scroll) is found through the
// wrapper chain without hiding what NoShrink asserts on the backend it wraps.
var e2eWrap func(mirrorhost.Backend) mirrorhost.Backend

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
	envSnippets    = "MUSTER_MIRROR_SNIPPETS_FILE"
	envMCPListen   = "MUSTER_MIRROR_MCP_LISTEN"
	envMCPSecret   = "MUSTER_MIRROR_MCP_SECRET_FILE"
	// envWriteHookConfig: record this daemon's URL and token file for
	// `muster-mirror ask` (hookconfig.go). Set by the managed service only.
	envWriteHookConfig = "MUSTER_MIRROR_WRITE_HOOK_CONFIG"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// refreshOverride is a browser-test build's refresh, or 0 (mirrorhost's
// default) in every shipped binary.
func refreshOverride() time.Duration {
	if e2eRefresh == nil {
		return 0
	}
	return e2eRefresh()
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
	// `muster-mirror ask`: the AskUserQuestion hook (ask.go), a short-lived
	// command of its own — not the host daemon.
	if len(args) > 0 && args[0] == "ask" {
		return runAsk(args[1:], getenv, stdin, stdout, stderr)
	}
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
	writeHook := fs.Bool("write-hook-config", envBool(envWriteHookConfig), "record this daemon's URL and token file (never the token) for `muster-mirror ask`, in "+defaultHookConfig+" ("+envHookConfig+"); for the host's managed service only — an ad-hoc daemon must not repoint the hook ("+envWriteHookConfig+")")
	snippetsFile := fs.String("snippets-file", getenv(envSnippets), "JSON array of {\"trigger\", \"label\", \"body\"}: the prewritten messages muster's message bar offers after `:`; re-read when it changes; missing or invalid = none, never fatal (default "+mirrorhost.DefaultSnippetsFile()+"; "+envSnippets+")")
	mcpListen := fs.String("mcp-listen", getenv(envMCPListen), "serve muster's attention and task tools to this host's Claude Code sessions as an MCP endpoint at http://<addr>/mcp — a LOOPBACK address only, e.g. 127.0.0.1:47823; each call needs the secret in -mcp-secret-file and names its session with X-Muster-Pane: ${TMUX_PANE}; empty = off ("+envMCPListen+")")
	mcpSecretFile := fs.String("mcp-secret-file", or(getenv(envMCPSecret), mirrorhost.DefaultMCPSecretFile()), "the local MCP endpoint's secret, created (0600) when missing ("+envMCPSecret+")")
	catalog := fs.Bool("catalog", false, "print the composer menu's catalog this host would report for a pane whose working directory is the current one — commands, skills (source and name) and snippet triggers — and exit")
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
	if e2eWrap != nil {
		backend = e2eWrap(backend)
	}
	ctx, stop := runContext()
	defer stop()
	logger := log.New(stderr, "", log.LstdFlags)
	codenames := loadCodenames(*labelsFile, logger)

	if *catalog {
		cwd, _ := os.Getwd()
		c := (&mirrorhost.CatalogScanner{SnippetsFile: *snippetsFile}).Scan(cwd)
		for _, e := range c.Commands {
			fmt.Fprintf(stdout, "%s\t%s\t/%s\t%s\n", e.Source, e.Kind, e.Name, e.Description)
		}
		for _, sn := range c.Snippets {
			fmt.Fprintf(stdout, "snippet\t%s\t%s\n", sn.Trigger, sn.Label)
		}
		if c.Truncated {
			fmt.Fprintln(stdout, "# truncated to the wire's bounds")
		}
		return 0
	}
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
		NoPromptIndex: e2eNoPromptIndex != nil && e2eNoPromptIndex(),
		NoCatalog:     e2eNoCatalog != nil && e2eNoCatalog(),
		SnippetsFile:  *snippetsFile,
		Refresh:       refreshOverride(),
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *mcpListen != "" {
		// 🔴 OPT-IN, SO NEVER FATAL: a port in use, a secret file with the wrong
		// mode or anything else that stops the MCP endpoint is logged and the
		// host mirrors without it — mirroring is the job this process exists for.
		if err := startMCP(ctx, *mcpListen, *mcpSecretFile, backend, &mirrorhost.MCPProxy{URL: *url, Token: token, AllowHTTP: *allowHTTP, Logger: logger}, logger); err != nil {
			logger.Printf("muster-mirror: the MCP endpoint is OFF, mirroring continues without it: %v", err)
		}
	}
	mode := "read-only"
	if *interactive {
		mode = "INTERACTIVE (-interactive)"
	}
	logger.Printf("muster-mirror %s: host %s → %s, %s", buildRevision, *host, *url, mode)
	// Record where muster is for `muster-mirror ask` (hookconfig.go), whose
	// Claude Code hook does not inherit this service's environment — only when
	// told to (the managed service), and never in a browser-test build.
	if *writeHook && e2eBackend == nil {
		hc := hookConfig{URL: *url, AllowHTTP: *allowHTTP}
		if *tokenFile != "" {
			if abs, err := filepath.Abs(expandHome(*tokenFile)); err == nil {
				hc.TokenFile = abs
			}
		}
		if p := hookConfigPath(getenv); writeHookConfig(p, hc) != nil {
			logger.Printf("muster-mirror: could not record %s for `muster-mirror ask` (the hook then needs -url)", p)
		}
	}
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

// runContext is the daemon's life: until SIGINT or SIGTERM. A test replaces it
// to end a daemon it started.
var runContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

// startMCP serves the local MCP endpoint (internal/mirrorhost's MCPProxy) on
// listen until ctx ends, completing proxy with the secret and the backend's
// window and tmux-server lookups. An error means the endpoint is not served.
func startMCP(ctx context.Context, listen, secretFile string, backend mirrorhost.Backend, proxy *mirrorhost.MCPProxy, logger *log.Logger) error {
	secret, err := mirrorhost.EnsureMCPSecret(secretFile)
	if err != nil {
		return err
	}
	l, err := mirrorhost.ListenMCP(listen)
	if err != nil {
		return err
	}
	proxy.Secret = secret
	proxy.Windows, _ = mirrorhost.WindowListerOf(backend)
	proxy.Socket, _ = mirrorhost.ServerSocketOf(backend)
	logger.Printf("muster-mirror: MCP endpoint for this host's Claude Code sessions at http://%s/mcp (secret in %s)", l.Addr(), secretFile)
	go func() {
		if err := mirrorhost.ServeMCP(ctx, l, proxy); err != nil {
			logger.Printf("muster-mirror: MCP endpoint: %v", err)
		}
	}()
	return nil
}
