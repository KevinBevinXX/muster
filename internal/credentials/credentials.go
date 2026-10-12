// Package credentials is the per-owner model-provider credential store: the
// Claude Code `setup-token`s and the OpenRouter API key a MEMBER brings
// to muster, and the one place that decides whose credentials an owner's agents
// run on ([Plane]).
//
// # The operator decision this implements (accounts plan, 2026-10-11)
//
// Members BRING THEIR OWN credentials. A member's agents only ever use that
// member's credentials. The environment-configured pool
// (MUSTER_AGENT_CC_ACCOUNTS + MUSTER_AGENT_CC_TOKEN_*) and the operator's
// OpenRouter key (MUSTER_AGENT_OPENROUTER_KEY) stay the ADMIN's. A member with
// no credential for a kind cannot use that kind — NEVER a fallback to the
// admin's.
//
// # Storage
//
// One table, user_credentials (migration 0011), owned and under FORCE row-level
// security like every other owned table. Each secret is sealed (seal.go) with
// the owner, kind and name bound in the associated data, so a ciphertext copied
// to another owner's row does not open.
//
// 🔴 A SECRET IS NEVER RETURNED TO A BROWSER. The listing ([Summary]) carries a
// name, a fingerprint and timestamps — nothing a page could echo back. Opening
// a secret is [PGStore.ClaudeTokens] and [PGStore.OpenRouterKey], whose only
// callers are the pod-spec path (internal/agentprovision, through [Plane]).
//
// # OpenRouter: an ordinary API key, used as entered
//
// The member pastes an ordinary OpenRouter API key (operator decision on #69);
// it is sealed like a setup-token and placed, as entered, in the member's
// gateway pods as OPENROUTER_API_KEY. muster makes no OpenRouter call with it.
package credentials

import (
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// The two credential kinds.
const (
	KindClaude     = "claude"
	KindOpenRouter = "openrouter"
)

// OpenRouterName is the name of a user's one OpenRouter credential: the table's
// uniqueness is (owner, kind, name), and a user has exactly one OpenRouter key.
const OpenRouterName = "default"

// ClaudeTokenPrefix is the shape of a `claude setup-token` value.
const ClaudeTokenPrefix = "sk-ant-oat01-"

// OpenRouterKeyPrefix is the shape of an OpenRouter API key.
const OpenRouterKeyPrefix = "sk-or-"

// ErrNoCredential: the owner has no credential of the kind asked for. It is a
// REFUSAL, and its message is the one a member sees.
var ErrNoCredential = errors.New("no credential")

// ErrInvalid: an entered value is not the shape asked for. Its message never
// carries the value.
var ErrInvalid = errors.New("invalid credential")

// ErrNoStore: this deployment has no credential store (no encryption key).
var ErrNoStore = errors.New("credentials: this deployment has no credential store (MUSTER_CONNECTOR_ENCRYPTION_KEY is unset)")

// Summary is what a listing shows about one credential. 🔴 No secret, not
// even a truncation of one: the fingerprint is a one-way 48-bit hash.
type Summary struct {
	Kind        string
	Name        string
	Fingerprint string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ValidateClaudeToken checks a setup-token's SHAPE. The error never echoes it.
func ValidateClaudeToken(tok string) error {
	return validateShape(tok, ClaudeTokenPrefix, "a Claude setup token starts with "+ClaudeTokenPrefix+
		" (run `claude setup-token` and paste what it prints)")
}

// ValidateOpenRouterKey checks an OpenRouter key's SHAPE.
func ValidateOpenRouterKey(key string) error {
	return validateShape(key, OpenRouterKeyPrefix, "an OpenRouter key starts with "+OpenRouterKeyPrefix)
}

func validateShape(v, prefix, hint string) error {
	if !strings.HasPrefix(v, prefix) {
		return invalid(hint)
	}
	rest := v[len(prefix):]
	if len(rest) < 16 || len(v) > 400 {
		return invalid(hint + ", and is longer than that")
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return invalid(hint + "; the value has a character no key contains (a space or a line break from the copy?)")
		}
	}
	return nil
}

// ValidateAccountName is ccpool's rule: an account name becomes part of an
// agent row and is shown on a card.
func ValidateAccountName(name string) error {
	if !ccpool.ValidName(name) {
		return invalid("an account name is 1-32 of a-z, 0-9 and '-', not starting or ending with '-'")
	}
	return nil
}

// invalidError is an [ErrInvalid] whose message is the hint alone, so a page
// can show it as written.
type invalidError struct{ msg string }

func (e invalidError) Error() string        { return e.msg }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(msg string) error { return invalidError{msg: msg} }
