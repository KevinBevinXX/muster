package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
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
	envConnectURL = "MUSTER_CONNECT_URL"
	envAgentToken = "MUSTER_HOOK_TOKEN"
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

// agentConnector is the subset of muster's /agent/connectors answers this
// command reads.
type agentConnector struct {
	Name   string   `json:"name"`
	Exists bool     `json:"exists"`
	Hosts  []string `json:"hosts"`
	Grant  string   `json:"grant"`
}

func (c agentConnector) usable() bool { return c.Exists && c.Grant == "approved" }

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
	if !c.usable() {
		fmt.Fprintf(stdout, "ccd connect-git: %s is %s for this agent; git talks to the platform directly. "+
			"Ask for it with the connector_request tool; once it is approved ccd routes git through muster "+
			"by itself (or run this again to do it at once).\n", name, c.Grant)
		return exitNotApproved
	}
	fmt.Fprintf(stdout, "ccd connect-git: git now reaches %s through muster's proxy (connector %s)\n",
		strings.Join(c.Hosts, ", "), name)
	return 0
}

// noRedirects is the client every call to muster uses. 🔴 IT NEVER FOLLOWS A
// REDIRECT: Go's client resends custom headers (X-Muster-Token among them) to
// a redirect's target, even on another host. muster's agent routes do not
// redirect, so a redirect here is something to refuse, not to follow.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// musterGET reads one of muster's agent JSON routes into out. It is a GET and
// files nothing.
func musterGET(ctx context.Context, base, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Muster-Token", token)
	resp, err := noRedirects.Do(req)
	if err != nil {
		return fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("muster answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("muster's answer is not JSON: %w", err)
	}
	return nil
}

// --- keeping git's routing in step with the grants ------------------------------
//
// 🔴 THE ROUTING FOLLOWS THE GRANT WITHOUT THE AGENT DOING ANYTHING. `ccd serve`
// runs syncGitRouting at start and then every gitSyncInterval: an approval
// shows up as routing, and a revocation (or a deleted connector) removes it,
// with no pod restart and no reliance on a model remembering to run a command.
// `ccd connect-git <name>` stays as the way to apply one connector at once.

// gitSyncInterval is how often ccd re-reads this agent's grants. One cheap GET.
const gitSyncInterval = 15 * time.Second

// syncGitRouting makes git's global config route exactly the connectors this
// agent holds an approved grant on, and nothing else under this muster's
// /connect/ prefix. It writes only what changed, so a git command running at the
// same moment never sees a routing torn down and rebuilt for nothing. It returns
// the names whose routing it changed.
func syncGitRouting(ctx context.Context, base, token string) ([]string, error) {
	var list struct {
		Connectors []agentConnector `json:"connectors"`
	}
	if err := musterGET(ctx, base, token, "/agent/connectors", &list); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var changed []string
	for _, c := range list.Connectors {
		seen[c.Name] = true
		did, err := applyGitRouting(base, token, c)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, c.Name)
		}
	}
	// Routing for a connector muster no longer lists (deleted, or its grant
	// gone with it) is removed too.
	for _, name := range routedConnectors(base) {
		if seen[name] {
			continue
		}
		if err := unsetGitRouting(proxyPrefix(base, name)); err != nil {
			return changed, err
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// runGitRoutingLoop runs syncGitRouting now and every interval until ctx ends.
// A failed round (muster unreachable) leaves the routing as it is: with muster
// down the proxy is down too, and tearing the routing out would only make the
// next round's restore a change.
func runGitRoutingLoop(ctx context.Context, base, token string, interval time.Duration, logf func(string, ...any)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		changed, err := syncGitRouting(rctx, base, token)
		cancel()
		switch {
		case err != nil:
			logf("ccd: git routing: %v", err)
		case len(changed) > 0:
			logf("ccd: git routing updated for %s", strings.Join(changed, ", "))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func proxyPrefix(base, name string) string { return base + "/connect/" + name + "/" }

// applyGitRouting brings one connector's routing in line with its grant and
// reports whether it wrote anything.
func applyGitRouting(base, token string, c agentConnector) (bool, error) {
	proxy := proxyPrefix(base, c.Name)
	want := map[string]string{}
	if c.usable() {
		want = desiredGitRouting(proxy, c.Hosts, token)
	}
	have := currentGitRouting(proxy)
	if mapsEqual(want, have) {
		return false, nil
	}
	if len(want) == 0 {
		return true, unsetGitRouting(proxy)
	}
	return true, setGitRouting(proxy, c.Hosts, token)
}

func git(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput()
	// Exit 5 from `--unset-all` means "nothing to unset", which is success here.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 5 && slices.Contains(args, "--unset-all") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// desiredGitRouting is every key=value setGitRouting writes, for comparison.
func desiredGitRouting(proxy string, hosts []string, token string) map[string]string {
	m := map[string]string{
		"http." + proxy + ".extraheader":     "X-Muster-Token: " + token,
		"http." + proxy + ".followredirects": "false",
	}
	for _, h := range hosts {
		m["url."+proxy+h+"/.insteadof"] = "https://" + h + "/"
	}
	return m
}

// currentGitRouting reads every key under this proxy prefix from git's global
// config. Keys come back with their section and variable lowercased and the
// subsection (the URL) verbatim.
func currentGitRouting(proxy string) map[string]string {
	have := map[string]string{}
	out, err := exec.Command("git", "config", "--global", "--null", "--get-regexp", `^(url|http)\.`).Output()
	if err != nil {
		return have
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		key, val, _ := strings.Cut(rec, "\n")
		var sub string
		switch {
		case strings.HasPrefix(key, "url."):
			sub = key[len("url."):]
		case strings.HasPrefix(key, "http."):
			sub = key[len("http."):]
		default:
			continue
		}
		if !strings.HasPrefix(sub, proxy) {
			continue
		}
		if _, dup := have[key]; dup {
			have[key] += "\x00" + val // a duplicate is a difference, never "equal"
			continue
		}
		have[key] = val
	}
	return have
}

// routedConnectors lists the connector names that have routing under base.
func routedConnectors(base string) []string {
	prefix := base + "/connect/"
	var names []string
	seen := map[string]bool{}
	for key := range currentGitRouting(prefix) {
		sub := strings.TrimPrefix(strings.TrimPrefix(key, "url."), "http.")
		name, _, _ := strings.Cut(strings.TrimPrefix(sub, prefix), "/")
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// setGitRouting points every https://<host>/ of the connector at the proxy, and
// on proxy URLs ONLY (an http.<url> key is scoped to URLs under that prefix):
//
//   - sends the agent's token as an extra header, and
//   - 🔴 turns redirects OFF. git's default ("initial") follows a redirect on
//     the first request and carries http.extraHeader to the redirect's target,
//     whatever its host — measured. The proxy already refuses to hand out an
//     off-allowlist redirect; this keeps the token in place if anything ever
//     answers one anyway.
func setGitRouting(proxy string, hosts []string, token string) error {
	if err := unsetGitRouting(proxy); err != nil {
		return err
	}
	for _, h := range hosts {
		if err := git("config", "--global", "--add", "url."+proxy+h+"/.insteadOf", "https://"+h+"/"); err != nil {
			return err
		}
	}
	if err := git("config", "--global", "http."+proxy+".followRedirects", "false"); err != nil {
		return err
	}
	return git("config", "--global", "http."+proxy+".extraHeader", "X-Muster-Token: "+token)
}

// unsetGitRouting removes every key setGitRouting writes for this proxy prefix.
func unsetGitRouting(proxy string) error {
	for key := range currentGitRouting(proxy) {
		sub := strings.TrimPrefix(key, "url.")
		if sub != key && strings.HasSuffix(sub, ".insteadof") {
			if err := git("config", "--global", "--unset-all", "url."+strings.TrimSuffix(sub, ".insteadof")+".insteadOf"); err != nil {
				return err
			}
		}
	}
	if err := git("config", "--global", "--unset-all", "http."+proxy+".followRedirects"); err != nil {
		return err
	}
	return git("config", "--global", "--unset-all", "http."+proxy+".extraHeader")
}
