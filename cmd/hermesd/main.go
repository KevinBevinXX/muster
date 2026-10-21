// Command hermesd is the in-pod supervisor that makes Hermes a muster agent
// runtime: the `hermes` agent kind (docs/design/hermes.md, "The hermes agent
// kind"). It is to Hermes what cmd/ccd is to Claude Code.
//
// It runs ONE `hermes gateway run` with Hermes's API server bound to loopback,
// and speaks muster's existing agent wire on top of it:
//
//   - POST /v1/responses — muster's chat/kickoff transport. Every turn is a
//     Hermes run (POST /v1/runs) whose events hermesd reads from the run's SSE
//     stream (turn.go). A failed turn is a non-200 with a typed JSON error
//     (wire.go), never a 200 with no text.
//   - GET /healthz (and GET /, identically) — 200 unless the gateway is
//     crash-looping or refused to start; the body names the state, `recovering`
//     until start-up recovery has finished (health.go).
//
// The gateway is supervised (supervise.go): restarted with backoff, latched on
// a crash loop, and after EVERY start — hermesd's own and every in-pod restart
// — recovered before it is given a turn (recover.go): executions a crash left
// `unknown` are resolved, intents recorded before a crash are replayed with the
// same idempotency key and bytes, and sessions posted under an API key that is
// no longer the one on the volume are retired.
//
// Environment:
//
//	MUSTER_GATEWAY_BEARER / HOOKS_TOKEN  the bearer /v1/responses accepts (as ccd)
//	HERMES_HOME              Hermes's home on the volume        (default /data/hermes)
//	HERMESD_WORKSPACE        the agent's working directory: terminal.cwd (default /data/workspace)
//	HERMESD_REPO             owner/name: clone https://github.com/<it> into HERMESD_WORKSPACE
//	                         before the gateway starts (internal/podagent's checkout)
//	HERMESD_REPO_BRANCH      the branch to clone (default: the remote's default)
//	HERMESD_INSTRUCTIONS     muster's instructions, sent on every run (default /etc/hermesd/instructions.md)
//	HERMESD_MODEL            the model slug (OpenRouter); empty leaves Hermes's default
//	HERMESD_MODEL_BASE_URL   TEST-ONLY: an OpenAI-compatible base URL to use INSTEAD of
//	                         OpenRouter (the image smoke test's stub model; its key is
//	                         still ${OPENROUTER_API_KEY}). agentspec never sets it, and
//	                         hermesd logs it at boot
//	HERMESD_LISTEN           muster's wire                      (default :18789)
//	HERMESD_API_PORT         Hermes's API server, on 127.0.0.1  (default 8642)
//	HERMESD_RELAY_TIMEOUT    how long an approval waits for muster before hermesd
//	                         answers deny itself; must be shorter than the rendered
//	                         approvals.timeout                  (default 240s)
//	OPENROUTER_API_KEY       the provider credential, passed to the gateway only
//	MUSTER_CONNECT_URL / MUSTER_HOOK_TOKEN  muster's connector surface: git routing,
//	                         the approval relay and the MCP server (inert when unset)
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] != "serve" {
		fmt.Fprintf(os.Stderr, "hermesd: unknown command %q (serve)\n", args[0])
		return 2
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("hermesd: %s=%q is not a positive duration", name, v)
	}
	return d, nil
}

func envPort(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("hermesd: %s=%q is not a port", name, v)
	}
	return n, nil
}
