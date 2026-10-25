package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// OPENCODE'S CREDENTIAL (docs/design/opencode.md, docs/mirroring.md
// "opencode windows"): muster-mirror reads the local opencode v2 server with
// a PAIR TOKEN of its own, kept in a file (0600) and renewed by the daemon
// before it expires. `muster-mirror opencode-pair` mints the first one.
const (
	// envOpenCodeTokenFile names the token file (default
	// ~/.muster/opencode-token).
	envOpenCodeTokenFile = "MUSTER_MIRROR_OPENCODE_TOKEN_FILE"
	// envOpenCodeToken is a token itself, with envOpenCodeURL its server:
	// for a host that hands the token over another way. Never renewed.
	envOpenCodeToken = "MUSTER_MIRROR_OPENCODE_TOKEN"
	envOpenCodeURL   = "MUSTER_MIRROR_OPENCODE_URL"
)

// openCodeTokenFileDefault is the token file's default: ~/.muster/opencode-token,
// beside the mirror token devrc places at ~/.muster/mirror-token.
func openCodeTokenFileDefault(getenv func(string) string) string {
	if v := getenv(envOpenCodeTokenFile); v != "" {
		return v
	}
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".muster", "opencode-token")
}

// runOpenCodePair is `muster-mirror opencode-pair`: mint this host's opencode
// token and write it to the token file.
//
// 🔴 IT READS THE OPENCODE SERVER'S PASSWORD — from the service's
// registration file, the one place opencode keeps it for clients — for the
// one request that issues a pairing code, and keeps it nowhere. It prints
// what it read and wrote, and the new token's FINGERPRINT and expiry: never
// the password, the code or the token.
func runOpenCodePair(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muster-mirror opencode-pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	regs := opencodehost.DefaultRegistrationPaths(getenv)
	registration := fs.String("registration", "", "the opencode service's registration file, {url, password} (default: the first of "+strings.Join(regs, ", ")+" that exists)")
	tokenFile := fs.String("token-file", openCodeTokenFileDefault(getenv), "where to write the token, 0600 ("+envOpenCodeTokenFile+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "muster-mirror opencode-pair: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *registration != "" {
		regs = []string{*registration}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := opencodehost.Pair(ctx, regs, expandHome(*tokenFile))
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror opencode-pair: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: read the server URL and password from %s (the password was used for one request and is not kept)\n", res.Registration)
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: paired with %s: token %s, expires %s\n", res.URL, res.Fingerprint, res.Expires.UTC().Format(time.RFC3339))
	replaced := ""
	if res.Replaced != "" {
		replaced = ", replacing token " + res.Replaced
	}
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: wrote %s (mode 0600)%s; muster-mirror renews it before it expires\n", res.TokenFile, replaced)
	return 0
}

// openCodeCredentials is the daemon's opencode credential: the token file at
// path, else a token from the environment, else none (nil, with why).
func openCodeCredentials(path string, getenv func(string) string) (*opencodehost.Credentials, string) {
	if tok := strings.TrimSpace(getenv(envOpenCodeToken)); tok != "" {
		c, err := opencodehost.EnvCredentials(getenv(envOpenCodeURL), tok)
		if err != nil {
			return nil, fmt.Sprintf("%s: %v", envOpenCodeToken, err)
		}
		return c, ""
	}
	if path == "" {
		return nil, "no opencode token file (HOME is not set; set -opencode-token-file)"
	}
	c, err := opencodehost.LoadCredentials(expandHome(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Sprintf("no opencode token at %s (pair this host with `muster-mirror opencode-pair`)", path)
	}
	if err != nil {
		return nil, err.Error()
	}
	return c, ""
}

// withOpenCode is b with its opencode windows resolved from src, when b is the
// tmux backend.
func withOpenCode(b mirrorhost.Backend, src *mirrorhost.OpenCodeSource) mirrorhost.Backend {
	if t, ok := b.(mirrorhost.Tmux); ok {
		t.OpenCode = src
		return t
	}
	return b
}

// startOpenCode logs the daemon's opencode credential c (nil: why there is
// none) and keeps it renewed until ctx ends.
func startOpenCode(ctx context.Context, c *opencodehost.Credentials, why string, logger *log.Logger) *opencodehost.Credentials {
	if c == nil {
		logger.Printf("muster-mirror: opencode: %s", why)
		return nil
	}
	logger.Printf("muster-mirror: opencode: token %s for %s, expires %s", opencodehost.Fingerprint(c.Token()), c.URL(), c.Expires().UTC().Format(time.RFC3339))
	go opencodehost.KeepRenewed(ctx, c, logger, opencodehost.RenewEvery, time.Now)
	return c
}
