// Command muster-mirror shows this machine's tmux sessions in muster's Agents
// view as MIRRORED sessions. It is READ-ONLY unless started with -interactive. It is the optional host half of the mirror plugin: install it
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
	list := fs.Bool("list", false, "print the sessions this host would register, and exit")
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

	var backend mirrorhost.Backend = mirrorhost.Tmux{Socket: *socket, SocketPath: *socketPath}
	if e2eBackend != nil {
		backend = e2eBackend()
	}
	// No attach shrinks a session's window; the browser scrolls instead
	// (internal/termsize). Wrapped AFTER the e2e swap, so the browser suite
	// drives the same wrapper the shipped binary runs.
	backend = mirrorhost.NoShrink(backend)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *list {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sessions, err := backend.List(lctx)
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
			return 1
		}
		for _, s := range sessions {
			fmt.Fprintln(stdout, s)
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

	logger := log.New(stderr, "", log.LstdFlags)
	c, err := mirrorhost.New(mirrorhost.Config{
		URL: *url, Token: token, Host: *host, Backend: backend,
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
	if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("muster-mirror: %v", err)
		return 1
	}
	return 0
}
