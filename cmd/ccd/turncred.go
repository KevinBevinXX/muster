package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/ZacxDev/muster/internal/agentgateway"
)

// The credentials that drive a turn API — ccd's own /v1/responses, and the
// retired gateway runtime's (envGatewayUpstreamBearer) — as ccd receives them
// in its environment.
//
// 🔴 ccd READS THEM ONCE AND REMOVES THEM FROM ITS OWN ENVIRONMENT BEFORE IT
// STARTS ANYTHING (takeTurnCredentials), because everything ccd starts inherits
// that environment: the tmux server, whose global environment every pane gets,
// so the CLI, and so every tool call the CLI makes. Before this, a Bash tool call
// could read HOOKS_TOKEN, derive ccd's bearer and drive ccd's turn API, or read
// CCD_GATEWAY_UPSTREAM_BEARER and drive the gateway runtime directly.
//
// What the CLI KEEPS, and why none of it drives a turn API:
//   - CCD_HOOK_SECRET: the hook channel's per-process credential, which `ccd hook`
//     needs (hook.go). It is checked only by the hook listener, never by
//     /v1/responses (TestNothingTheCLIKeepsDrivesTheTurnAPI).
//   - MUSTER_HOOK_TOKEN: the agent's own muster identity, which the session needs
//     (the approval hook, the MCP entry, git routing). The bearer muster sends a
//     claude-code agent is a separate random credential (agents.Agent.GatewaySecret),
//     so it is NOT derivable from this token — and serve refuses to start when the
//     bearer it was given is (exposedTurnCredential).
//   - CLAUDE_CODE_OAUTH_TOKEN: the model credential, which the CLI itself needs.
var turnCredentialEnv = []string{envGatewayBearer, envGatewayUpstreamBearer, envHooksToken}

const (
	envHooksToken    = "HOOKS_TOKEN"
	envGatewayBearer = "MUSTER_GATEWAY_BEARER"
	// envGatewayUpstreamBearer is agentspec.EnvCCDGatewayUpstreamBearer: the
	// credential of the gateway runtime that once ran beside a chief's ccd. A pod
	// built before it was removed still carries it, so ccd still unsets it.
	envGatewayUpstreamBearer = "CCD_GATEWAY_UPSTREAM_BEARER"
)

// turnCredentials is what takeTurnCredentials took out of the environment.
type turnCredentials struct {
	hooksToken, gatewayBearer string
}

// takeTurnCredentials reads every turnCredentialEnv variable and unsets it, so no
// process ccd starts from here on inherits it.
//
// ⚠ os.Unsetenv changes the environment ccd hands its CHILDREN; it does not change
// /proc/<ccd>/environ, which is the block ccd was exec'd with. That is what
// hideFromSameUser is for.
func takeTurnCredentials() turnCredentials {
	c := turnCredentials{
		hooksToken:    os.Getenv(envHooksToken),
		gatewayBearer: os.Getenv(envGatewayBearer),
	}
	for _, name := range turnCredentialEnv {
		_ = os.Unsetenv(name)
	}
	return c
}

// cliKeeps are the credential-carrying variables that stay in the CLI's
// environment because the session needs them (see turnCredentialEnv's note) and
// from which a turn credential could be had. Only the agent's muster token:
// the model token is a different secret muster never derives a bearer from, so
// a check against it could not fire.
var cliKeeps = []string{envAgentToken}

// exposedTurnCredential names the variable the CLI keeps from which bearer can be
// had — the value itself, or muster's legacy hooks-sha256 derivation of it — or
// returns "" when there is none.
//
// 🔴 THIS IS THE CHECK THAT MAKES THE SCRUB MEAN SOMETHING. A pod whose spec
// predates agents.Agent.GatewaySecret is handed HOOKS_TOKEN and MUSTER_HOOK_TOKEN
// with ONE value, and its bearer is sha256("gw-"+that value): removing
// HOOKS_TOKEN from the CLI's environment then hides nothing, because
// MUSTER_HOOK_TOKEN gives the same bearer. serve refuses to start on such a spec
// rather than serve a turn API the session can drive.
func exposedTurnCredential(bearer string, getenv func(string) string) string {
	for _, name := range cliKeeps {
		v := getenv(name)
		if v == "" {
			continue
		}
		if v == bearer || agentgateway.HooksSHA256().Bearer(v) == bearer {
			return name
		}
	}
	return ""
}

// hideFromSameUser marks ccd non-dumpable (prctl PR_SET_DUMPABLE 0).
//
// 🔴 IT CLOSES THE SAME-UID /proc READ THAT os.Unsetenv CANNOT. The CLI and every
// tool it runs share ccd's uid, and /proc/<ccd>/environ still holds the
// environment ccd was exec'd with — HOOKS_TOKEN and MUSTER_GATEWAY_BEARER
// included. The kernel lets a process read another's environ, mem, or attach
// ptrace only when it may ptrace it, and a non-dumpable process may be ptraced
// only with CAP_SYS_PTRACE, which the claude-code-agent pod does not grant
// (agentspec.ClaudeCodeSecurity is Restricted: the k8s driver drops ALL
// capabilities and runs as a non-root uid). Children are dumpable
// again: execve resets the flag, so this hides ccd alone.
//
// ⚠ IT DOES NOT HIDE WHAT ccd WROTE ELSEWHERE, and it does not stop a process of
// ccd's user from typing into the CLI's pane with `tmux send-keys` (the residual
// #163 documents) — which is input to the same session, not the turn API.
func hideFromSameUser() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("ccd: prctl(PR_SET_DUMPABLE, 0): %w", err)
	}
	return nil
}
