package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The browser-flow verbs: `flow enqueue|ls|status|cancel` over /api/flows/jobs.
//
// 🔴 THEY WORK ONLY WITH A PERSONAL TOKEN IN envToken. The server's flow API
// (api.requireAPIToken) refuses the shared hook secret by construction — it
// acts as the admin and is held by every hook script on every host, and a flow
// drives a logged-in browser — so on a hook-configured host these verbs exit 3
// (auth). Mint a personal token at /account/tokens.
//
// `flow cancel` withdraws a queued job or dismisses one that needs the
// operator; a running job is the runner's until it reports (409). There is no
// runner-token verb: minting a runner token is on the /flows page (a browser
// session), because minting a credential is a person's act.

const (
	flowJobsRoute = "/api/flows/jobs"
	flowJobRoute  = "/api/flows/jobs/{id}"
	flowCancel    = "/api/flows/jobs/{id}/cancel"
)

type flowEnqueueBody struct {
	Flow       string            `json:"flow"`
	Host       string            `json:"host"`
	Instance   string            `json:"instance"`
	Params     map[string]string `json:"params,omitempty"`
	TTLSeconds int               `json:"ttlSeconds,omitempty"`
}

func newFlowCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flow",
		Short: "Queue and read browser-flow jobs (needs a personal API token)",
	}
	cmd.AddCommand(newFlowEnqueueCmd(a), newFlowLsCmd(a), newFlowStatusCmd(a), newFlowCancelCmd(a))
	return cmd
}

func newFlowEnqueueCmd(a *app) *cobra.Command {
	var (
		b      flowEnqueueBody
		params []string
		ttl    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "enqueue <flow>",
		Short: "POST /api/flows/jobs — queue a catalogue flow for one of your hosts' browsers",
		Long: `Queue a browser flow (muse.poll, muse.send) for HOST's runner to claim and run
on browser INSTANCE. Parameters are --param name=value (repeatable): muse.send
takes text (required), thread or session, marker, wait and task; muse.poll
takes thread or session, and task. session is a Muse thread uuid (lowercase),
exclusive with thread; marker (md-<6 hex>) needs session, and the send
completes only when the reply's last line is "DONE <marker>"; wait is the
seconds to await the reply (1..3600). A job
not claimed within --ttl (default 15m, 1m..24h) becomes "expired" and never
runs late. Prints the job; follow it with ` + "`flow status <id>`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b.Flow = strings.TrimSpace(args[0])
			if len(params) > 0 {
				b.Params = map[string]string{}
			}
			for _, p := range params {
				k, v, ok := strings.Cut(p, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return failf(exitUsage, "--param %q is not name=value", p)
				}
				if _, dup := b.Params[k]; dup {
					return failf(exitUsage, "--param %s given twice", k)
				}
				b.Params[k] = v
			}
			if ttl < 0 || ttl%time.Second != 0 {
				return failf(exitUsage, "--ttl %s must be a positive whole number of seconds", ttl)
			}
			b.TTLSeconds = int(ttl / time.Second)
			body, err := a.call(cmd.Context(), request{method: http.MethodPost, path: flowJobsRoute, body: b, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().StringVar(&b.Host, "host", "laptop", "the runner host the job is for (its browser bridge's host label)")
	cmd.Flags().StringVar(&b.Instance, "instance", "personal", "the browser instance (Brave profile) on that host")
	cmd.Flags().StringArrayVar(&params, "param", nil, "flow parameter name=value (repeatable)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the job may wait to be claimed (default 15m)")
	return cmd
}

func newFlowLsCmd(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "GET /api/flows/jobs — your flow jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := request{method: http.MethodGet, path: flowJobsRoute, cred: credHook}
			if limit > 0 {
				req.query = url.Values{"limit": {strconv.Itoa(limit)}}
			}
			body, err := a.call(cmd.Context(), req)
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many (1..200; default 100)")
	return cmd
}

// flowJobPath expands route with a job id argument, refusing a non-positive
// one before anything is sent.
func flowJobPath(route, arg string) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil || id <= 0 {
		return "", failf(exitUsage, "job id %q is not a positive integer (see `flow ls`)", arg)
	}
	return expandPath(route, map[string]string{"id": strconv.FormatInt(id, 10)})
}

func newFlowCancelCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: "POST /api/flows/jobs/{id}/cancel — withdraw a queued job, or dismiss one that needs you",
		Long: `Cancel one of your flow jobs: a queued job becomes cancelled before any
runner claims it, and a needs_operator job is dismissed. A running or finished
job cannot be cancelled (409, exit 5; stop the runner to stop a running one
now). Another user's id is "not found" (exit 4). Prints the job.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := flowJobPath(flowCancel, args[0])
			if err != nil {
				return err
			}
			body, err := a.call(cmd.Context(), request{method: http.MethodPost, path: path, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

func newFlowStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "GET /api/flows/jobs/{id} — one job: status, output, detail",
		Long: `Print one of your flow jobs. Statuses: queued, running, done, failed,
needs_operator (a write whose runner went silent, or a flow that needs a human),
expired (not claimed before its ttl), cancelled. Another user's id is "not
found" (exit 4).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := flowJobPath(flowJobRoute, args[0])
			if err != nil {
				return err
			}
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: path, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}
