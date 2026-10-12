package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// The personal-API-token verbs (accounts PR 6): `token ls|revoke` over
// /api/tokens, and `whoami` over /api/whoami.
//
// 🔴 THERE IS NO `token create`. Minting is browser-session only (the server
// has no create route on /api/tokens): a token that could mint a token would
// let a leaked one mint a child that outlives revoking it.
//
// 🔴 `token …` WORKS ONLY WITH A PERSONAL TOKEN IN envToken. The server's
// token API (api.requireAPIToken) refuses the shared hook secret — minting a
// credential is a person's act, and the hook secret names nobody — so on a
// hook-configured host these verbs exit 3 (auth).

const (
	tokensRoute      = "/api/tokens"
	tokenRevokeRoute = "/api/tokens/{id}"
	whoamiRoute      = "/api/whoami"
)

func newTokenCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "List and revoke your personal API tokens (mint them in the browser at /account/tokens)",
	}
	cmd.AddCommand(newTokenLsCmd(a), newTokenRevokeCmd(a))
	return cmd
}

func newTokenLsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "GET /api/tokens — your tokens (never their values), newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: tokensRoute, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

func newTokenRevokeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "DELETE /api/tokens/{id} — revoke one of your tokens (immediately)",
		Long: `Revoke one of your tokens by id (from ` + "`token ls`" + `). It stops working on the
very next request. Revoking the token you are using is allowed and is the last
thing it does. Another user's id is "not found" (exit 4).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
			if err != nil || id <= 0 {
				return failf(exitUsage, "token id %q is not a positive integer (see `token ls`)", args[0])
			}
			path, err := expandPath(tokenRevokeRoute, map[string]string{"id": strconv.FormatInt(id, 10)})
			if err != nil {
				return err
			}
			body, err := a.call(cmd.Context(), request{method: http.MethodDelete, path: path, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

func newWhoAmICmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "GET /api/whoami — the user (and token) your credential acts as",
		Long: `Print who the configured credential acts as: userId, role, and via —
"api-token" for a personal token (with the token's id and name), "hook" for the
shared hook secret (which acts as the admin, user 1).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: whoamiRoute, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}
