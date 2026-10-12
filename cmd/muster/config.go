package main

import (
	"bufio"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"strings"
)

// defaultEnvFile is where this CLI looks for its credentials when nothing is
// exported and no flag is given.
//
// ⚠ IT IS NOT DERIVED FROM THE INVOKED PROGRAM NAME, even though the binary
// answers to more than one (see progName). An alias changes WHICH NAME the
// process is running under; it does not change where this machine keeps
// muster's credentials, and a config path that moved with argv[0] would send
// two invocations of the same binary to two different files for no reason a
// reader could predict. Point a differently-provisioned host at its own file
// with --env-file.
const defaultEnvFile = "~/.muster/muster.env"

const (
	// envAPIURL is the ONE base URL this binary speaks to.
	//
	// 🔴 ONE URL IS CORRECT HERE, AND IT IS A CONSEQUENCE OF THE TWO-BINARY
	// SPLIT RATHER THAN AN ASSUMPTION THAT SURVIVED IT. The upstream CLI this
	// was extracted from also had a single base URL, and that WAS a problem
	// there: its verbs were about to straddle two services. This binary's verbs
	// do not — every route in the wiring ledger is served by muster's own
	// mux — so one URL is sufficient rather than merely convenient.
	//
	// That is a claim about the route set, so it is CHECKED rather than
	// asserted: TestEveryWiredRouteIsServedByThisProject reads
	// internal/api/testdata/routes.golden and fails if any wired route is not in
	// it. A verb aimed at a second service would have to add a route the golden
	// does not carry, and that test is what turns "one URL is enough" into a
	// measurement.
	envAPIURL = "MUSTER_API_URL"
	// envToken is the ONE credential this binary presents (accounts PR 6).
	//
	// ⚠ WHAT IT HOLDS DEPENDS ON WHO RUNS THE CLIENT, and the cases are not
	// interchangeable:
	//   - a PERSON: their own personal API token (`mst_…`, minted at
	//     /account/tokens or with `token create`). It acts as that person on
	//     every machine-tier route, and it is the only credential `token …`
	//     accepts.
	//   - an operator HOST's hook scripts: the server's shared hook secret
	//     (api.AuthConfig.HookToken), acting as the machine user.
	//   - a managed AGENT POD: that pod's OWN per-agent token, which is what
	//     api.requireAgentToken resolves to an agent row. The `agent task`
	//     verbs work only there.
	envToken = "MUSTER_TOKEN"
	// envTokenAlias is the name this credential had before personal tokens,
	// and it is STILL READ: every hook host's env file and every agent pod
	// (internal/agentspec injects it) spells it this way. Within one source
	// envToken wins over it; a higher-precedence source still wins over both.
	envTokenAlias = "MUSTER_HOOK_TOKEN"
)

// config is the resolved connection config. The token is NEVER printed: it is
// not included in any error message, and it is only ever read from the env
// file, the process environment, or --token. It is never accepted as a
// positional argument, because argv is world-readable through /proc.
type config struct {
	apiURL string
	token  string
}

// parseEnvFile reads a KEY=VALUE file (comments, blank lines, an optional
// `export ` prefix, optionally quoted values). A missing file is NOT an error —
// the environment or flags may supply everything.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	// Tokens can be long (a token plus a URL); 1 MiB is plenty and bounds the
	// read of a file that is not supposed to be huge.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = unquote(strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquote strips one layer of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// expandHome resolves a leading ~/ against $HOME. It deliberately does not
// shell out and does not handle ~user.
func expandHome(path string, homeDir string) string {
	if path == "~" {
		return homeDir
	}
	if strings.HasPrefix(path, "~/") && homeDir != "" {
		return homeDir + path[1:]
	}
	return path
}

// resolveConfig applies the documented precedence, LOWEST to HIGHEST:
//
//	the env file  ->  process environment  ->  --api-url / --token
//
// A later source only overrides an earlier one when it actually supplies a
// value, so an empty environment variable does not blank out the file.
func resolveConfig(envFilePath string, fileVals map[string]string, getenv func(string) string, flagURL, flagToken string) (config, error) {
	var c config
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&c.apiURL, fileVals[envAPIURL])
	set(&c.token, fileVals[envTokenAlias])
	set(&c.token, fileVals[envToken])
	set(&c.apiURL, getenv(envAPIURL))
	set(&c.token, getenv(envTokenAlias))
	set(&c.token, getenv(envToken))
	set(&c.apiURL, flagURL)
	set(&c.token, flagToken)

	if c.apiURL == "" {
		return config{}, failf(exitUsage,
			"no API URL: set %s in %s, export it, or pass --api-url (e.g. --api-url http://127.0.0.1:8080)",
			envAPIURL, envFilePath)
	}
	u, err := url.Parse(c.apiURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return config{}, failf(exitUsage, "invalid API URL %q: want an absolute http(s) URL", c.apiURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return config{}, failf(exitUsage, "invalid API URL scheme %q: want http or https", u.Scheme)
	}
	c.apiURL = strings.TrimSuffix(c.apiURL, "/")
	return c, nil
}
