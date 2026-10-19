package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// A CHAT SESSION'S RUNTIME AND MODEL (migration 0030).
//
// The chief's pod runs two runtimes side by side: ccd (Claude Code) and the
// model-gateway runtime (agentspec's sidecar). A session is tagged with one of
// them, and a model, when it is created, and both are fixed for its life (a
// database trigger refuses a change). muster routes each turn by the tag
// (internal/api's sessionRoute).
//
//	SessionRuntimeLegacy     ""            the agent's own kind; for a claude-code
//	                                       agent, the ONE CLI ccd has always run.
//	                                       Every session that existed before 0030.
//	SessionRuntimeClaudeCode "claude-code" a CLI of its own in ccd, started with
//	                                       --model when Model is set.
//	SessionRuntimeGateway    "gateway"     the model-gateway runtime, with Model as
//	                                       the per-request model override.
const (
	SessionRuntimeLegacy     = ""
	SessionRuntimeClaudeCode = KindClaudeCode
	SessionRuntimeGateway    = KindGateway
)

// SessionRuntimes is every explicit runtime, in the order a picker shows them.
var SessionRuntimes = []string{SessionRuntimeClaudeCode, SessionRuntimeGateway}

// SessionRuntimeLabel is what the session list and the picker show.
//
// ⚠ THE GATEWAY RUNTIME IS LABELLED BY ITS MODEL PROVIDER, NOT BY ITS PRODUCT:
// the runtime image's own name is a denied token in this public repository
// (tests/leakscan.py), and what the operator chooses here is "an OpenRouter
// model" anyway.
func SessionRuntimeLabel(runtime string) string {
	switch runtime {
	case SessionRuntimeClaudeCode:
		return "Claude Code"
	case SessionRuntimeGateway:
		return "OpenRouter"
	}
	return "Default"
}

// claudeModelPattern is what a Claude Code session's model may be: an alias
// ("opus", "sonnet", "haiku") or a full model id, optionally with the CLI's
// "[1m]" context suffix. It is argv to the CLI (`--model <it>`), so it is a
// closed character set rather than "anything non-empty"; ccd checks the same
// set again before it starts a CLI.
var claudeModelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,79}(\[1m\])?$`)

// gatewayModelPattern is an OpenRouter model as GET /api/openrouter/models
// returns it: "openrouter/<vendor>/<model>", the model part allowing the
// catalogue's ":free"-style suffixes.
var gatewayModelPattern = regexp.MustCompile(`^openrouter/[a-z0-9][a-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)

// ClaudeCodeModels are the models the picker offers for a Claude Code session.
// Aliases, so a model release does not need a muster release; any id matching
// the pattern above is accepted from the API as well.
var ClaudeCodeModels = []string{"opus", "sonnet", "haiku"}

// ValidateSessionRuntime checks an explicit (runtime, model) pair a caller asks
// a new session to carry, and returns the model as it will be stored.
func ValidateSessionRuntime(runtime, model string) (string, error) {
	model = strings.TrimSpace(model)
	switch runtime {
	case SessionRuntimeClaudeCode:
		if model != "" && !claudeModelPattern.MatchString(model) {
			return "", fmt.Errorf("%q is not a Claude model (an alias such as opus, sonnet or haiku, or a model id)", model)
		}
	case SessionRuntimeGateway:
		if !gatewayModelPattern.MatchString(model) {
			return "", fmt.Errorf("%q is not an OpenRouter model (openrouter/<vendor>/<model>, as the model search returns it)", model)
		}
	default:
		return "", fmt.Errorf("runtime %q is not one a session can be started on (want %s)", runtime, strings.Join(SessionRuntimes, " or "))
	}
	return model, nil
}

// SessionRuntimeStore is the part of the session store migration 0030 added.
// PGStore implements it; it is a separate interface so the many fakes of
// [Store] do not each grow three methods, and a caller asserts it and falls
// back to the pre-0030 behaviour when the store lacks it.
type SessionRuntimeStore interface {
	// CreateSessionOn opens a new session on an explicit runtime and model
	// (ValidateSessionRuntime first). It is never the default.
	CreateSessionOn(ctx context.Context, agentID int64, agentName, runtime, model string) (ChatSession, error)
	// DefaultSession returns the agent's default session, electing one when it
	// has none: its legacy-key session, else its oldest, else a new legacy one.
	// The election and an existing default are the same row for every caller
	// (a partial unique index allows one).
	DefaultSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error)
	// SetDefaultSession makes sessionID — which must be agentID's — the agent's
	// default, in one transaction with clearing the previous one.
	SetDefaultSession(ctx context.Context, agentID, sessionID int64) error
	// SessionByKey loads the agent's session with this session key.
	SessionByKey(ctx context.Context, agentID int64, sessionKey string) (ChatSession, error)
}
