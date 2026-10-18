package main

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// chiefAskRoute is a TEMPLATE for the reason agentMessagesRoute is: an empty
// name must fail locally rather than reach /api/agents//messages, which
// ServeMux cleans into a path that means something else.
const chiefAskRoute = "/api/agents/{name}/messages"

// chiefDefaultName is the agent slug `chief ask` resolves by default.
//
// ⚠ IT IS A SECOND COPY OF agents.ChiefName, AND THE DRIFT IS GUARDED FROM THE
// OTHER SIDE rather than by a build edge. Importing internal/agents to reach the
// original links pgx and its tree into a binary that never opens a database.
//
// ⚠ THE SIZE COST IS THE WEAKER HALF OF THE ARGUMENT, AND IT IS STATED AT WHAT
// WAS ACTUALLY MEASURED rather than at what it felt like: +457,106 bytes, 4.6%
// of the stripped-of-nothing artefact (9,986,494 -> 10,443,600, `go build -o`,
// one file adding the import and one reference). That alone would not decide it.
// What decides it is the BUILD EDGE: a client that links a store package invites
// the next reach for a symbol from it, and this project's whole claim is that the
// two halves are separable.
//
// The copy is safe only because the pin is BIDIRECTIONAL —
// TestTheChiefDefaultNameMatchesTheServers imports internal/agents from a TEST
// file and fails when the two disagree in EITHER direction. A one-directional
// pin is what made the upstream status-vocabulary copy a real defect, and that
// one was correctly resolved by importing instead. The difference is the pin,
// not the taste.
const chiefDefaultName = "chief"

// newChiefCmd held one verb (ask) until `replace` arrived.
//
// 🔴 THE FAMILY IS A DIRECTION, NOT A SERVICE, AND ONLY ONE OF ITS TWO
// DIRECTIONS LIVES HERE. Upstream `chief` held both: ASKING (operator -> agent)
// and WRITING (agent -> a terminal pane, approval-gated). The write half stayed
// with the upstream CLI along with the routes it calls, so this binary carries
// the ask half alone. It is kept as a family rather than flattened to a
// top-level verb because the name is what callers already type, and because a
// verb that grows a sibling later should not move.
func newChiefCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chief",
		Short: "Ask an agent a question, or replace the chief agent",
	}
	cmd.AddCommand(newChiefAskCmd(a), newChiefReplaceCmd(a))
	return cmd
}

// chiefReplaceRoute is the server's chief replacement.
const chiefReplaceRoute = "/api/chief/replace"

func newChiefReplaceCmd(a *app) *cobra.Command {
	var kind, account, model, confirm string
	cmd := &cobra.Command{
		Use:   "replace",
		Short: "POST " + chiefReplaceRoute + " — delete the chief and re-create it on another agent kind",
		Long: `Replace your chief agent: DELETE the agent holding your reserved chief slug
(with its chat history, focus, connectors and workspace volume) and create a new
one under the same slug on --kind (and --cc-account for claude-code).

Without --confirm NOTHING is changed: the server answers with the plan — what it
would delete and what it would create — and this exits ` + "2" + `. Re-run with
--confirm set to the slug the plan names (` + chiefDefaultName + ` for the operator) to apply it.

The server refuses (exit 5, nothing changed) while a turn is being delivered to
the chief, while any plugin message is still ` + "`delivering`" + ` to it, or while another
agent is labelled "chief". It is safe to re-run: if a previous run deleted the old
chief and failed to create the new one, the same command creates it; if the chief
is already on the requested kind and account, it is re-applied, not deleted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(kind) == "" {
				return failf(exitUsage, "--kind is required (gateway or claude-code).")
			}
			raw, err := a.call(cmd.Context(), request{
				method: http.MethodPost,
				path:   chiefReplaceRoute,
				body: map[string]any{
					"kind": strings.TrimSpace(kind), "cc_account": strings.TrimSpace(account),
					"model": strings.TrimSpace(model), "confirm": strings.TrimSpace(confirm),
				},
				cred: credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "the new chief's agent kind: gateway or claude-code (required)")
	cmd.Flags().StringVar(&account, "cc-account", "", "claude-code only: the Claude account to pin (empty lets the pool choose)")
	cmd.Flags().StringVar(&model, "model", "", "gateway only: the model (empty = the deployment default)")
	cmd.Flags().StringVar(&confirm, "confirm", "", "the chief slug being replaced; without it nothing is changed")
	return cmd
}

func newChiefAskCmd(a *app) *cobra.Command {
	var (
		text  string
		agent string
	)
	cmd := &cobra.Command{
		Use:   "ask",
		Short: "POST /api/agents/{name}/messages — ask an agent and print its reply",
		Long: `Send one message to an agent's current chat session and print the reply.

--agent addresses ANY provisioned agent, not only the default; it merely
DEFAULTS to ` + chiefDefaultName + `. It is resolved over GET /api/agents first, so an absent or
ambiguous name — or a display name rather than a slug — is settled here rather
than becoming a request aimed at the wrong agent.

🔴 IT NEEDS ONE CREDENTIAL, AND BOTH ITS STEPS PRESENT THE SAME ONE. GET
/api/agents is behind requireHookToken and POST /api/agents/{name}/messages
behind requireArmedHookToken; both compare against the server's single shared
secret (or both accept your personal token), so both use ` + envToken + `.

⚠ THE TWO WRAPPERS DIFFER IN THE UNCONFIGURED CASE, and the difference is
visible from here: on a server with no ` + envTokenAlias + ` configured the ROSTER
READ is served open (enforce-when-set, back-compat for callers that predate the
token) while the WRITE answers 503 and this verb exits 9, not 3 — the remedy is
to arm the server, not to fix your token.

The message lands in the same transcript the operator sees on the agent's detail
page — and the same chat session ` + "`agent messages <name>`" + ` reads by default —
so a conversation held here is readable afterwards. The response carries
` + "`agent`, `agentId` and `sessionId`" + ` beside the reply, so which agent answered
is in the output rather than inferred.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Refused before any request: an unset shell variable must not become an
			// empty message the agent answers anyway.
			if strings.TrimSpace(text) == "" {
				return failf(exitUsage, "--text is required: asking nothing would still spend an agent turn.")
			}
			name := strings.ToLower(strings.TrimSpace(agent))
			if name == "" {
				return failf(exitUsage, "--agent is empty; refusing to resolve.")
			}
			// 🔴 THE ROSTER READ IS KEPT EVEN THOUGH THE WRITE IS KEYED ON THE NAME,
			// AND IT IS NOT A LEFTOVER. resolveAgent settles an ambiguous or
			// display-name --agent against the real roster and prints the known agents
			// when nothing matches; posting the raw flag straight at the path would
			// turn a capitalised name into a 404 and an ambiguous one into a message
			// delivered to whichever row the server happened to match.
			//
			// ⚠ WHAT IS DROPPED IS THE ID. The write addresses brief.Name, so the
			// resolve is a VALIDATION step rather than a name->id translation the route
			// depends on — which is why a server-side 404 is still a coherent answer if
			// the roster moves between the two calls.
			_, brief, err := a.resolveAgent(cmd.Context(), name)
			if err != nil {
				return err
			}
			path, err := expandPath(chiefAskRoute, map[string]string{"name": brief.Name})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{
				method: http.MethodPost,
				path:   path,
				body:   map[string]any{"message": text},
				// credHook, the SAME credential the roster read above presents — and
				// that is a claim about the CREDENTIAL, not about the wrapper. The two
				// routes sit behind DIFFERENT gates that happen to compare against the
				// same secret; see this verb's help for what still differs between them.
				cred: credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "the question to ask (required)")
	cmd.Flags().StringVar(&agent, "agent", chiefDefaultName, "agent slug to ask")
	return cmd
}
