package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The human-only approval verbs: `attention ask|get|wait` over
// /api/attention/asks (migration 0045). They need a personal API token.
//
// 🔴 THESE VERBS ASK; NONE ANSWERS. An ask raised here can be answered only by
// a person on the /attention page (what the phone's notification opens). Its
// approval is spent by `flow enqueue --approval <id>` on exactly one muse
// dispatch whose text is the ask's payload.

const (
	attentionAsksRoute = "/api/attention/asks"
	attentionAskRoute  = "/api/attention/asks/{id}"
)

type attentionAskBody struct {
	Title          string `json:"title"`
	Payload        string `json:"payload"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

func newAttentionCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attention",
		Short: "Raise and read human-only approvals (needs a personal API token)",
	}
	cmd.AddCommand(newAttentionAskCmd(a), newAttentionGetCmd(a), newAttentionWaitCmd(a))
	return cmd
}

func newAttentionAskCmd(a *app) *cobra.Command {
	var (
		b           attentionAskBody
		payloadFile string
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "ask",
		Short: "POST /api/attention/asks — ask the operator to approve a payload (human-only)",
		Long: `Raise a HUMAN-ONLY approval: an Attention ask (it notifies the operator's
phone) showing --title and the payload verbatim, with the options Approve and
Deny. Only a person on the Attention page can answer it; no agent and no token
can. The payload is read EXACTLY (no trimming) from --payload-file (- is
stdin); the server stores its sha256 (printed as payloadSha256), and an
approval queues only a muse dispatch whose text has that digest. Prints the
ask; follow it with ` + "`attention wait <id>`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(b.Title) == "" {
				return failf(exitUsage, "--title is required")
			}
			if payloadFile == "" {
				return failf(exitUsage, "--payload-file is required (- reads stdin)")
			}
			data, err := readBodyFile(payloadFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			b.Payload = data
			if timeout < 0 || timeout%time.Second != 0 {
				return failf(exitUsage, "--timeout %s must be a positive whole number of seconds", timeout)
			}
			b.TimeoutSeconds = int(timeout / time.Second)
			body, err := a.call(cmd.Context(), request{method: http.MethodPost, path: attentionAsksRoute, body: b, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().StringVar(&b.Title, "title", "", "the ask's title (what the card leads with)")
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "the exact text the approval covers; - reads stdin")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long the ask stays answerable (default 24h, 1m..168h)")
	return cmd
}

func attentionAskPath(arg string) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil || id <= 0 {
		return "", failf(exitUsage, "ask id %q is not a positive integer", arg)
	}
	return expandPath(attentionAskRoute, map[string]string{"id": strconv.FormatInt(id, 10)})
}

func newAttentionGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "GET /api/attention/asks/{id} — one ask: status, answer, approved, consumed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := attentionAskPath(args[0])
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

func newAttentionWaitCmd(a *app) *cobra.Command {
	var timeout, interval time.Duration
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "poll GET /api/attention/asks/{id} until it is answered, cancelled or expired",
		Long: `Wait for one ask to leave "open", polling every --interval, and print it.
Exit 0 when it was APPROVED (a human-only ask answered Approve on a human
surface), 5 when it ended any other way (denied, cancelled, expired, or a
non-approval answer), 10 when --timeout passed while it was still open.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := attentionAskPath(args[0])
			if err != nil {
				return err
			}
			if interval <= 0 {
				return failf(exitUsage, "--interval must be positive")
			}
			deadline := time.Now().Add(timeout)
			for {
				body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: path, cred: credHook})
				if err != nil {
					return err
				}
				var v struct {
					Status   string `json:"status"`
					Approved bool   `json:"approved"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					return wrapf(exitNonJSON, err, "server response was not valid JSON")
				}
				if v.Status != "open" {
					if err := a.emit(body); err != nil {
						return err
					}
					if v.Approved {
						return nil
					}
					return failf(exitConflict, "ask %s ended %s without an approval", args[0], v.Status)
				}
				if timeout > 0 && time.Now().Add(interval).After(deadline) {
					if err := a.emit(body); err != nil {
						return err
					}
					return failf(exitAborted, "ask %s is still open after %s", args[0], timeout)
				}
				select {
				case <-cmd.Context().Done():
					return wrapf(exitAborted, cmd.Context().Err(), "waiting for ask %s", args[0])
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up (exit 10) after this long; 0 waits for the ask's own expiry")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "how often to poll")
	return cmd
}
