package podagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// git routing through muster's proxy for every connector THIS agent holds an
// approved grant on: for each of the connector's hosts,
// `url.<proxy>.insteadOf https://<host>/`, plus — on proxy URLs only — the
// agent's own token as an extra header and redirects turned off. With an
// approved grant EVERY request git makes to those hosts goes through the proxy —
// every repository, not only the agent's. For a stored connector each carries
// its credential; for the GitHub-App-backed `github` connector, only the
// agent's OWN repository gets the App token and every other repository is
// forwarded anonymously. Without an approved grant the routing is REMOVED, so
// git talks to the platform directly (public clones keep working) rather than
// through a proxy that would refuse it. Neither ever ASKS for access:
// requesting a connector has one path, the connector_request tool.
//
// 🔴 NO PLATFORM CREDENTIAL EVER REACHES THE POD. The only secret written here
// is the agent's OWN muster token, which the pod already holds in its
// environment ([EnvAgentToken]): muster adds the platform credential on the
// way out.
//
// 🔴 IT WRITES git's GLOBAL CONFIG, WHICH IS $HOME/.gitconfig OF THE PROCESS
// THAT RUNS IT. ccd's HOME is the agent's, so the
// lines land where the agent's git reads them.

// AgentConnector is the subset of muster's /agent/connectors answers read here.
type AgentConnector struct {
	Name   string   `json:"name"`
	Exists bool     `json:"exists"`
	Hosts  []string `json:"hosts"`
	Grant  string   `json:"grant"`
}

// Usable reports whether the connector exists and this agent's grant on it is
// approved.
func (c AgentConnector) Usable() bool { return c.Exists && c.Grant == "approved" }

// GitSyncInterval is how often the routing is re-read from the grants. One
// cheap GET.
const GitSyncInterval = 15 * time.Second

// SyncGitRouting makes git's global config route exactly the connectors this
// agent holds an approved grant on, and nothing else under this muster's
// /connect/ prefix. It writes only what changed, so a git command running at the
// same moment never sees a routing torn down and rebuilt for nothing. It returns
// the names whose routing it changed.
func SyncGitRouting(ctx context.Context, base, token string) ([]string, error) {
	var list struct {
		Connectors []AgentConnector `json:"connectors"`
	}
	if err := MusterGET(ctx, base, token, "/agent/connectors", &list); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var changed []string
	for _, c := range list.Connectors {
		seen[c.Name] = true
		did, err := ApplyGitRouting(base, token, c)
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
		if err := unsetGitRouting(ProxyPrefix(base, name)); err != nil {
			return changed, err
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// RunGitRoutingLoop runs SyncGitRouting now and every interval until ctx ends.
// A failed round (muster unreachable) leaves the routing as it is: with muster
// down the proxy is down too, and tearing the routing out would only make the
// next round's restore a change. who prefixes the log lines ("ccd").
func RunGitRoutingLoop(ctx context.Context, who, base, token string, interval time.Duration, logf func(string, ...any)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		changed, err := SyncGitRouting(rctx, base, token)
		cancel()
		switch {
		case err != nil:
			logf("%s: git routing: %v", who, err)
		case len(changed) > 0:
			logf("%s: git routing updated for %s", who, strings.Join(changed, ", "))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ProxyPrefix is connector name's prefix on muster's proxy.
func ProxyPrefix(base, name string) string { return base + "/connect/" + name + "/" }

// ApplyGitRouting brings one connector's routing in line with its grant and
// reports whether it wrote anything.
func ApplyGitRouting(base, token string, c AgentConnector) (bool, error) {
	proxy := ProxyPrefix(base, c.Name)
	want := map[string]string{}
	if c.Usable() {
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
