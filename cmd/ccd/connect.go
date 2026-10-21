package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// muster's connector store, from inside the pod.
//
// Two pieces, both driven by the pod's environment and both inert without it:
//
//   - `ccd seed` registers muster's MCP server (MUSTER_CONNECT_URL + "/mcp") in
//     the CLI's user config, so the session has the connectors_list /
//     connector_request / connector_status tools. See seedMCP.
//   - git routing through muster's proxy for every connector THIS agent holds
//     an approved grant on: for each of the connector's hosts,
//     `url.<proxy>.insteadOf https://<host>/`, plus — on proxy URLs only — the
//     agent's own token as an extra header and redirects turned off. With an
//     approved grant EVERY request git makes to those hosts goes through the
//     proxy — every repository, not only the agent's. For a stored connector
//     each carries its credential; for the GitHub-App-backed `github`
//     connector, only the agent's OWN repository gets the App token and every
//     other repository is forwarded anonymously (a public dependency or
//     submodule clones as for anyone; a private one fails as for anyone).
//     Without an approved grant the routing is REMOVED, so git talks to the
//     platform directly (public clones keep working) rather than through a
//     proxy that would refuse it. `ccd serve` keeps it in step with the grants by itself
//     (syncGitRouting, every gitSyncInterval); `ccd connect-git [connector]`
//     applies one connector at once. Neither ever ASKS for access: requesting
//     a connector has one path, the connector_request tool.
//
// 🔴 NO PLATFORM CREDENTIAL EVER REACHES THIS POD. The only secret written here
// is the agent's OWN muster token, which the pod already holds in its
// environment (MUSTER_HOOK_TOKEN): muster adds the platform credential on the
// way out.

const (
	envConnectURL = podagent.EnvConnectURL
	envAgentToken = podagent.EnvAgentToken
	// mcpServerName is the key muster's server is registered under, so its tools
	// appear to the session as mcp__muster__<tool>.
	mcpServerName = "muster"
)

// seedMCP adds (or, when the pod is not offered the connector store, removes)
// muster's entry in the CLI's user-scope mcpServers. Every other server an
// operator registered is kept.
func seedMCP(st map[string]any, connectURL, token string) {
	servers, _ := st["mcpServers"].(map[string]any)
	if connectURL == "" || token == "" {
		if servers != nil {
			delete(servers, mcpServerName)
			if len(servers) == 0 {
				delete(st, "mcpServers")
			}
		}
		return
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers[mcpServerName] = map[string]any{
		"type":    "http",
		"url":     strings.TrimRight(connectURL, "/") + "/mcp",
		"headers": map[string]any{"X-Muster-Token": token},
	}
	st["mcpServers"] = servers
}

// exitNotApproved is connect-git's exit when the agent holds no approved grant:
// distinct from a failure, so a script can tell "ask the operator" from "broken".
const exitNotApproved = 3

func runConnectGit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect-git", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait for muster")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := "github"
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	base := strings.TrimRight(os.Getenv(envConnectURL), "/")
	token := os.Getenv(envAgentToken)
	if base == "" || token == "" {
		fmt.Fprintf(stderr, "ccd connect-git: %s and %s must be set (this pod is not offered muster's connector store)\n",
			envConnectURL, envAgentToken)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var c agentConnector
	if err := musterGET(ctx, base, token, "/agent/connectors/"+url.PathEscape(name), &c); err != nil {
		fmt.Fprintln(stderr, "ccd connect-git:", err)
		return 1
	}
	if c.Grant == "" {
		fmt.Fprintln(stderr, "ccd connect-git: muster's answer carries no grant status")
		return 1
	}
	c.Name = name
	if _, err := applyGitRouting(base, token, c); err != nil {
		fmt.Fprintln(stderr, "ccd connect-git:", err)
		return 1
	}
	if !c.Usable() {
		fmt.Fprintf(stdout, "ccd connect-git: %s is %s for this agent; git talks to the platform directly. "+
			"Ask for it with the connector_request tool; once it is approved ccd routes git through muster "+
			"by itself (or run this again to do it at once).\n", name, c.Grant)
		return exitNotApproved
	}
	fmt.Fprintf(stdout, "ccd connect-git: git now reaches %s through muster's proxy (connector %s)\n",
		strings.Join(c.Hosts, ", "), name)
	return 0
}

// The pieces below are internal/podagent's (hermesd shares them); these names
// keep ccd's call sites and tests reading as before.
var noRedirects = podagent.NoRedirects

type agentConnector = podagent.AgentConnector

func musterGET(ctx context.Context, base, token, path string, out any) error {
	return podagent.MusterGET(ctx, base, token, path, out)
}

func musterPOST(ctx context.Context, base, token, path string, body any) error {
	return podagent.MusterPOST(ctx, base, token, path, body)
}

// --- keeping git's routing in step with the grants ------------------------------
//
// 🔴 THE ROUTING FOLLOWS THE GRANT WITHOUT THE AGENT DOING ANYTHING. `ccd serve`
// runs syncGitRouting at start and then every gitSyncInterval: an approval
// shows up as routing, and a revocation (or a deleted connector) removes it,
// with no pod restart and no reliance on a model remembering to run a command.
// `ccd connect-git <name>` stays as the way to apply one connector at once.

const gitSyncInterval = podagent.GitSyncInterval

func syncGitRouting(ctx context.Context, base, token string) ([]string, error) {
	return podagent.SyncGitRouting(ctx, base, token)
}

func runGitRoutingLoop(ctx context.Context, base, token string, interval time.Duration, logf func(string, ...any)) {
	podagent.RunGitRoutingLoop(ctx, "ccd", base, token, interval, logf)
}

func applyGitRouting(base, token string, c agentConnector) (bool, error) {
	return podagent.ApplyGitRouting(base, token, c)
}
