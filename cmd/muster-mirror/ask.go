package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
)

// `muster-mirror ask` is a Claude Code PreToolUse HOOK for AskUserQuestion: it
// tells muster the call's questions, so muster's rendered view of this window
// shows them AT ONCE, and returns at once so the terminal's own picker shows at
// once too — whichever is answered first wins. An answer given in muster is
// typed into that picker by this host's muster-mirror (internal/askdrive).
// Register it in Claude Code's settings.json (docs/mirroring.md, "Answering
// Claude's questions"):
//
//	"PreToolUse": [{"matcher": "AskUserQuestion", "hooks": [{"type": "command",
//	  "command": "muster-mirror ask", "timeout": 10}]}]
//
// 🔴 IT ALWAYS EXITS 0 AND NEVER PRINTS. It never holds the call and never
// answers it; every failure (no URL or token, muster unreachable) costs only
// the muster card. It gives muster askTimeout, no more: the picker is drawn
// once the hook returns.

// stdin is the hook payload's source (a test replaces it).
var stdin io.Reader = os.Stdin

// askTimeout bounds the registration: the picker waits for the hook.
const askTimeout = 3 * time.Second

func runAsk(args []string, getenv func(string) string, in io.Reader, _, stderr io.Writer) (code int) {
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, "muster-mirror ask: "+f+"\n", a...) }
	defer func() {
		if r := recover(); r != nil {
			logf("internal error, the question is the terminal's only: %v", r)
			code = 0
		}
	}()
	fs := flag.NewFlagSet("muster-mirror ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envBool := func(k string) bool { v := getenv(k); return v == "1" || strings.EqualFold(v, "true") }
	base := fs.String("url", getenv(envURL), "muster's base URL, https://… ("+envURL+"; default: the URL the host's muster-mirror recorded in "+defaultHookConfig+")")
	tokenFile := fs.String("token-file", getenv(envTokenFile), "file holding this host's mirror token ("+envTokenFile+"; or the token itself in "+envToken+"; default, only with the recorded URL: the file the host's muster-mirror recorded, else "+DefaultTokenFile+")")
	allowHTTP := fs.Bool("allow-http", envBool(envAllowHTTP), "TESTS AND LOCAL DEVELOPMENT ONLY: permit a plain http:// muster URL ("+envAllowHTTP+")")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		logf("read stdin: %v; the question is the terminal's only", err)
		return 0
	}
	// What the host's muster-mirror recorded (hookconfig.go): the URL when no
	// flag or environment variable gave one — and, ONLY THEN, its token.
	// 🔴 THE RECORDED TOKEN GOES ONLY TO THE RECORDED URL: a hook pointed
	// elsewhere by -url or MUSTER_MIRROR_URL must bring its own token
	// (-token-file, MUSTER_MIRROR_TOKEN_FILE or MUSTER_MIRROR_TOKEN), or it
	// fails open — it never sends this host's credential to a URL nobody
	// recorded for it.
	recordedURL := false
	var rec hookConfig
	if *base == "" {
		var err error
		if rec, err = readHookConfig(hookConfigPath(getenv)); err == nil && rec.URL != "" {
			*base, *allowHTTP, recordedURL = rec.URL, *allowHTTP || rec.AllowHTTP, true
		} else {
			logf("no -url, no %s, and no URL recorded by muster-mirror (%v)", envURL, err)
		}
	}
	token := strings.TrimSpace(getenv(envToken))
	tokenPath := *tokenFile
	if tokenPath == "" && token == "" && recordedURL {
		tokenPath = rec.TokenFile
		if tokenPath == "" {
			tokenPath = DefaultTokenFile
		}
	}
	if tokenPath != "" {
		b, err := os.ReadFile(expandHome(tokenPath))
		if err != nil {
			// stderr: invisible to the picker, but in Claude Code's debug log.
			logf("cannot read the mirror token file %s: %v; the question is the terminal's only", tokenPath, err)
			return 0
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" && *base != "" {
		logf("no mirror token for %s: a URL given by -url or %s needs its own token (-token-file, %s or %s); the question is the terminal's only",
			*base, envURL, envTokenFile, envToken)
		return 0
	}
	u, err := url.Parse(*base)
	if *base == "" || token == "" || err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && *allowHTTP)) {
		logf("needs an https:// -url and a token (-token-file); the question is the terminal's only")
		return 0
	}
	h := askq.Hook{
		Base: *base, Path: "/mirror/ask",
		Auth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
		Client: &http.Client{Timeout: askTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Logf:   logf,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	h.Run(ctx, payload)
	return 0
}
