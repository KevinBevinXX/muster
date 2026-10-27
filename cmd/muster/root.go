package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// rootDefaultTimeout is --timeout's default.
//
// Thirty seconds is a READ verb's budget: against a hung server a machine
// client should fail fast and let the caller decide. No verb in this binary
// blocks on a human — the approval-gated terminal writes stayed with the
// upstream CLI — so there is no family here that needs to outlast a person.
const rootDefaultTimeout = 30 * time.Second

// app holds the process-wide wiring. Everything the binary touches from the
// outside world (streams, environment, $HOME, the HTTP transport, the name it
// was invoked as) arrives here, so the whole command tree is exercisable from a
// test against httptest with no live server and no global state.
type app struct {
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	// homeDir is $HOME, used only to resolve a leading ~/ in --env-file.
	//
	// ⚠ IT IS `homeDir` RATHER THAN `home` FOR A GATE REASON, NOT A STYLE ONE.
	// tests/leakscan.py refuses private and lab hostnames, and its pattern is
	// "a label, a dot, then one of a small set of private suffixes". A Go
	// SELECTOR has exactly that shape, so a struct field whose name is one of
	// those suffixes makes every use of it read as infrastructure. Measured: the
	// short name produced three findings, one of them in a comment that merely
	// described the problem.
	//
	// The cheap side of the trade is here. Renaming one field costs nothing,
	// while widening the rule would blunt a gate that has to survive
	// publication. ⚠ THE SAME TRAP IS WAITING FOR ANY FIELD NAMED AFTER ONE OF
	// THE OTHER SUFFIXES — read the rule before choosing such a name.
	homeDir string
	// progName is the name this process was invoked as. See progname.go.
	progName string
	// newHTTP builds the transport. Tests override the timeout only; redirect
	// policy is pinned in newClient.
	newHTTP func(timeout time.Duration) *http.Client

	flagAPIURL  string
	flagToken   string
	flagEnvFile string
	flagTimeout time.Duration

	client *client
}

// emit writes a JSON document to stdout — the ONLY thing ever written to stdout.
func (a *app) emit(raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return wrapf(exitNonJSON, err, "server response was not valid JSON")
	}
	buf.WriteByte('\n')
	if _, err := a.stdout.Write(buf.Bytes()); err != nil {
		return wrapf(exitServerError, err, "writing stdout")
	}
	return nil
}

// api returns the configured client, resolving config on first use so that
// `--help` never requires credentials.
func (a *app) api() (*client, error) {
	if a.client != nil {
		return a.client, nil
	}
	envFile := expandHome(a.flagEnvFile, a.homeDir)
	vals, err := parseEnvFile(envFile)
	if err != nil {
		return nil, wrapf(exitUsage, err, "reading %s", envFile)
	}
	cfg, err := resolveConfig(a.flagEnvFile, vals, a.getenv, a.flagAPIURL, a.flagToken)
	if err != nil {
		return nil, err
	}
	c := newClient(cfg, a.newHTTP(a.flagTimeout), a.stderr)
	c.progName = a.progName
	a.client = c
	return a.client, nil
}

// call resolves config, emits the one-line version-skew note (to stderr) and
// performs the request.
func (a *app) call(ctx context.Context, req request) ([]byte, error) {
	c, err := a.api()
	if err != nil {
		return nil, err
	}
	c.warnSkew(ctx)
	return c.do(ctx, req)
}

// rootLong is the root command's help body, minus the exit-code table.
//
// ⚠ IT NAMES NO CLI BY NAME, because the binary answers to more than one (see
// progname.go) and a help text that spells one of them is wrong under the
// other.
const rootLong = `A machine client for muster's JSON API.

It exists so host tooling stops reaching into Postgres for things the API
already answers: ` + "`agent resolve <name>`" + ` turns an agent name into its id over
GET /api/agents, and every request path is refused before it is sent if a path
parameter is empty (the doubled-slash 301 class).

Config, lowest to highest precedence:
  ` + defaultEnvFile + `  ->  environment  ->  --api-url / --token
The token is read from ` + envToken + ` (or its older name ` + envTokenAlias + `, which
` + envToken + ` beats within one source). It is never accepted as a positional
argument (argv is world-readable via /proc) and is never printed.

TWO COMMAND FAMILIES REACH TASKS, AND THEY ARE NOT ALIASES.
  ` + "`task …`" + `        takes an explicit task id and attributes writes from the
                   provenance headers. This is the family for host tooling.
  ` + "`agent task …`" + `  carries NO id: the credential IS the identity, and the
                   server resolves which task is bound to the caller. This is
                   the family for a muster-dispatched agent working inside its
                   own instance. It authors comments as the agent and its status
                   route REFUSES ` + "`complete`" + `, which the id-bearing one allows.

`

func newRootCmd(a *app) *cobra.Command {
	name := a.progName
	if name == "" {
		name = defaultProgName
	}
	root := &cobra.Command{
		Use:           name,
		Short:         "Machine client for the muster JSON API",
		Long:          rootLong + exitCodeHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
		// cliVersion(), NOT buildVersion: `--version` is a PROVENANCE question
		// ("which muster is this?") and buildVersion answers a compatibility one
		// ("which server was it built against?"). See the comments on both
		// variables in client.go.
		Version:           cliVersion(),
		DisableAutoGenTag: true,
	}
	// No generated `completion` subcommand: this is a machine client, and the
	// command tree should contain exactly what is registered below — which is
	// also what makes the wiring ledger in tree_test.go countable.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.PersistentFlags().StringVar(&a.flagAPIURL, "api-url", "", "muster base URL (default from "+envAPIURL+")")
	root.PersistentFlags().StringVar(&a.flagToken, "token", "",
		"personal API token or hook token (default from "+envToken+"; prefer the env file — flags are visible in ps)")
	root.PersistentFlags().StringVar(&a.flagEnvFile, "env-file", defaultEnvFile, "KEY=VALUE config file")
	root.PersistentFlags().DurationVar(&a.flagTimeout, "timeout", rootDefaultTimeout, "per-request timeout")

	root.AddCommand(newTaskCmd(a), newAgentCmd(a), newTokenCmd(a), newWhoAmICmd(a), newFlowCmd(a), newRouteCmd(a), newPluginsCmd(a))
	return root
}
