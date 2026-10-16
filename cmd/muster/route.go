package main

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// `route reply` — reply_to_chief for a MIRRORED session (session routing,
// internal/plugins/routes.go). A Claude Code window on the operator's own host
// that the chief routed a plugin message into answers it from that window:
//
//	muster route reply rt_… --body "Noon works."
//	muster route reply rt_… --body-file - < answer.txt
//
// It posts to POST /api/routes/{route_id}/reply with the host's ordinary muster
// credential — a personal API token (or the shared hook token), which acts as the
// OWNER: the route is that owner's, and row-level security answers another
// owner's route as not found. A dispatched agent answers with the reply_to_chief
// TOOL instead.

const routeReplyRoute = "/api/routes/{id}/reply"

// routeIDPattern is the server's route id shape (plugins.ValidRouteID): checked
// here so a typo is a usage error, not a 404 that looks like someone else's route.
var routeIDPattern = regexp.MustCompile(`^rt_[0-9a-f]{24}$`)

func newRouteCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Answer a plugin message the chief routed to this session",
	}
	cmd.AddCommand(newRouteReplyCmd(a))
	return cmd
}

type routeReplyBody struct {
	Body string `json:"body"`
}

func newRouteReplyCmd(a *app) *cobra.Command {
	var (
		b        routeReplyBody
		bodyFile string
	)
	cmd := &cobra.Command{
		Use:   "reply <route_id>",
		Short: "POST /api/routes/{route_id}/reply — answer a routed message (reply_to_chief)",
		Long: `Answer the plugin message the chief routed to this session, by its route_id
(rt_…, in the routed block). The answer is sent to the sender as written — at
once when your owner allowlisted the sender, otherwise after your owner approves
it. The same answer again is a no-op; a different second answer is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if !routeIDPattern.MatchString(id) {
				return failf(exitUsage, "route id %q is not rt_ and 24 hex digits (it is in the routed block)", args[0])
			}
			if bodyFile != "" {
				if b.Body != "" {
					return failf(exitUsage, "--body and --body-file are mutually exclusive")
				}
				data, err := readBodyFile(bodyFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				b.Body = data
			}
			if strings.TrimSpace(b.Body) == "" {
				return failf(exitUsage, "an answer is required: pass --body or --body-file (- reads stdin)")
			}
			path, err := expandPath(routeReplyRoute, map[string]string{"id": id})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodPost, path: path, body: b, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
	cmd.Flags().StringVar(&b.Body, "body", "", "the answer, exactly as it should be sent")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the answer from a file, or - for stdin")
	return cmd
}
