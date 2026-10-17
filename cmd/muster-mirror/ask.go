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
// shows the call's questions in muster's rendered view of this window and
// answers the call with what a person chooses there (internal/askq). Register
// it in Claude Code's settings.json (docs/mirroring.md, "Answering questions"):
//
//	"PreToolUse": [{"matcher": "AskUserQuestion", "hooks": [{"type": "command",
//	  "command": "muster-mirror ask",
//	  "timeout": 660}]}]
//
// 🔴 IT ALWAYS EXITS 0, AND PRINTS ONLY ANSWERS A PERSON GAVE. Every failure —
// no URL or token, muster unreachable, a session muster does not list for this
// host, a read-only host, nobody answering within -timeout, a signal — prints
// nothing, and the CLI shows its own picker in the terminal. While it holds a
// call the terminal shows NO picker: answer in muster, or wait out -timeout.

// stdin is the hook payload's source (a test replaces it).
var stdin io.Reader = os.Stdin

const (
	envAskTimeout = "MUSTER_MIRROR_ASK_TIMEOUT"
	// defaultAskTimeout is how long a call is held for muster's answer. The
	// hook entry's own "timeout" must be longer (660 s in the documented entry).
	defaultAskTimeout = 10 * time.Minute
)

func runAsk(args []string, getenv func(string) string, in io.Reader, stdout, stderr io.Writer) (code int) {
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, "muster-mirror ask: "+f+"\n", a...) }
	defer func() {
		if r := recover(); r != nil {
			logf("internal error, leaving the question to the terminal: %v", r)
			code = 0
		}
	}()
	fs := flag.NewFlagSet("muster-mirror ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envBool := func(k string) bool { v := getenv(k); return v == "1" || strings.EqualFold(v, "true") }
	base := fs.String("url", getenv(envURL), "muster's base URL, https://… ("+envURL+"; default: the URL the host's muster-mirror recorded in "+defaultHookConfig+")")
	tokenFile := fs.String("token-file", getenv(envTokenFile), "file holding this host's mirror token ("+envTokenFile+"; or the token itself in "+envToken+"; default: the file the host's muster-mirror recorded, else "+DefaultTokenFile+")")
	allowHTTP := fs.Bool("allow-http", envBool(envAllowHTTP), "TESTS AND LOCAL DEVELOPMENT ONLY: permit a plain http:// muster URL ("+envAllowHTTP+")")
	timeout := fs.Duration("timeout", envDurationOr(getenv(envAskTimeout), defaultAskTimeout), "how long to hold the question for an answer in muster before the terminal's picker gets it ("+envAskTimeout+"); keep it under the hook entry's timeout")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		logf("read stdin: %v; leaving the question to the terminal", err)
		return 0
	}
	// What the host's muster-mirror recorded (hookconfig.go): the defaults for
	// whatever no flag or environment variable gave.
	if *base == "" || *tokenFile == "" {
		if hc, err := readHookConfig(hookConfigPath(getenv)); err == nil {
			if *base == "" {
				*base, *allowHTTP = hc.URL, *allowHTTP || hc.AllowHTTP
			}
			if *tokenFile == "" && hc.TokenFile != "" && strings.TrimSpace(getenv(envToken)) == "" {
				*tokenFile = hc.TokenFile
			}
		} else if *base == "" {
			logf("no -url, no %s, and no URL recorded by muster-mirror (%v)", envURL, err)
		}
	}
	token := strings.TrimSpace(getenv(envToken))
	if f := tokenFileOr(*tokenFile, getenv); f != "" {
		b, err := os.ReadFile(expandHome(f))
		if err != nil {
			// stderr: invisible to the picker, but in Claude Code's debug log.
			logf("cannot read the mirror token file %s: %v; leaving the question to the terminal", f, err)
			return 0
		}
		token = strings.TrimSpace(string(b))
	}
	u, err := url.Parse(*base)
	if *base == "" || token == "" || err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && *allowHTTP)) {
		logf("needs an https:// -url and a token (-token-file); leaving the question to the terminal")
		return 0
	}
	wait := askq.HookWait
	h := askq.Hook{
		Base: *base, Path: "/mirror/ask",
		Auth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
		Hold:   *timeout,
		Wait:   wait,
		Client: &http.Client{Timeout: wait + 15*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Logf:   logf,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	if out := h.Run(ctx, payload); out != nil {
		_, _ = stdout.Write(out)
	}
	return 0
}

// envDurationOr is v as a duration, or d when v is empty or not one.
func envDurationOr(v string, d time.Duration) time.Duration {
	if x, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && x > 0 {
		return x
	}
	return d
}
