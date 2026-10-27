package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// `plugins retry <message-id>` — the /plugins page's Retry button, for a session
// without a browser: hand a plugin message the agent never confirmed (its turn
// was cut off, so it settled `unconfirmed`) to the CURRENT agent again. It posts
// POST /api/plugins/messages/{id}/retry with the hook credential — a personal
// token acts as its owner; the shared hook token as the operator, on an armed
// server only.

const pluginRetryRoute = "/api/plugins/messages/{id}/retry"

func newPluginsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Act on plugin messages",
	}
	cmd.AddCommand(newPluginsRetryCmd(a))
	return cmd
}

func newPluginsRetryCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <message-id>",
		Short: "POST " + pluginRetryRoute + " — hand an unconfirmed plugin message to the agent again",
		Long: `Retry a plugin message whose delivery turn was cut off (status unconfirmed):
it goes back in the queue and is delivered to your CURRENT agent the way every
message is. Refused (exit 5, nothing changed) when the message is queued,
being delivered or delivered, when the agent already drafted a reply to it that
is pending, approved or sent, when it was routed to a session, or when you have
no agent. The answer is 202 {id, status: queued, retrying: true}; the turn runs
after it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
			if err != nil || id <= 0 {
				return failf(exitUsage, "message id %q is not a positive number (it is on /plugins)", args[0])
			}
			path, err := expandPath(pluginRetryRoute, map[string]string{"id": strconv.FormatInt(id, 10)})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodPost, path: path, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
}
