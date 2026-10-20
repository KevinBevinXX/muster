package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The hook channel: where `ccd hook` reports, and the credential it presents.
// ccd puts both in the environment of every CLI it starts (tmux `-e`; main.go),
// and nowhere else.
const (
	envHookSocket = "CCD_HOOK_SOCKET"
	envHookSecret = "CCD_HOOK_SECRET"
)

// unixClient is an HTTP client whose every connection is to the unix socket at
// path, whatever the URL's host.
func unixClient(path string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// runHook is `ccd hook <Event>`: the command the CLI's settings.json runs for each
// hook. It forwards the hook's stdin payload to the running ccd's hook socket
// ($CCD_HOOK_SOCKET), with the credential ccd gave the CLI ($CCD_HOOK_SECRET),
// and exits.
//
// 🔴 IT ALWAYS EXITS 0 AND PRINTS NOTHING ON STDOUT, AND BOTH ARE SAFETY
// PROPERTIES OF THE SESSION, NOT POLITENESS. For these events the CLI treats
// stdout as context to inject into the conversation (UserPromptSubmit,
// SessionStart) and exit code 2 as "block": a Stop hook that exited 2 would keep
// the turn running, and a UserPromptSubmit hook that did would refuse the prompt.
// A ccd that is down must cost the session nothing but ccd's own bookkeeping.
func runHook(args []string, stdin io.Reader, stderr io.Writer) int {
	if os.Getenv("CCD_HOOK_DISABLED") == "1" {
		return 0
	}
	if len(args) != 1 || args[0] == "" || strings.ContainsAny(args[0], "/?#") {
		fmt.Fprintln(stderr, "usage: ccd hook <EventName>")
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		fmt.Fprintln(stderr, "ccd hook: read stdin:", err)
		return 0
	}
	sock, secret := os.Getenv(envHookSocket), os.Getenv(envHookSecret)
	if sock == "" || secret == "" {
		// Not run by a CLI ccd started (ccd hands both to the CLI's pane).
		fmt.Fprintf(stderr, "ccd hook: %s or %s is not set; this hook is not reported\n", envHookSocket, envHookSecret)
		return 0
	}
	// The host is a placeholder: the connection is to the unix socket.
	url := "http://ccd/hook/" + args[0]
	// Its own pid: ccd walks up from it to the CLI that fired the hook, which is
	// how a nested `claude` is told from the primary (proctree.go).
	url += "?pid=" + strconv.Itoa(os.Getpid())
	// A lane's CLI (lanes.go) runs with CCD_LANE in its environment, which every
	// hook it runs inherits: the event is that lane's. Hex only, as laneID makes
	// it — anything else is dropped rather than put in a URL.
	if lane := os.Getenv("CCD_LANE"); lane != "" && isLaneID(lane) {
		url += "&lane=" + lane
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, "ccd hook:", err)
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := unixClient(sock, 5*time.Second).Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "ccd hook:", err)
		return 0
	}
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(stderr, "ccd hook: %s refused: %s\n", args[0], resp.Status)
	}
	resp.Body.Close()
	return 0
}
