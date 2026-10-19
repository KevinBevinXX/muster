package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// runHook is `ccd hook <Event>`: the command the CLI's settings.json runs for each
// hook. It forwards the hook's stdin payload to the running ccd's loopback
// listener and exits.
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
	base := os.Getenv("CCD_HOOK_URL")
	if base == "" {
		base = defaultHookURL
	}
	url := strings.TrimSuffix(base, "/") + "/hook/" + args[0]
	// Its own pid: ccd walks up from it to the CLI that fired the hook, which is
	// how a nested `claude` is told from the primary (proctree.go).
	url += "?pid=" + strconv.Itoa(os.Getpid())
	// A lane's CLI (lanes.go) runs with CCD_LANE in its environment, which every
	// hook it runs inherits: the event is that lane's. Hex only, as laneID makes
	// it — anything else is dropped rather than put in a URL.
	if lane := os.Getenv("CCD_LANE"); lane != "" && isLaneID(lane) {
		url += "&lane=" + lane
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, "ccd hook:", err)
		return 0
	}
	resp.Body.Close()
	return 0
}
