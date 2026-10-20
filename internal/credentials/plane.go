package credentials

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// Personal is what [Plane] reads from the per-owner store ([PGStore]).
type Personal interface {
	ClaudeTokens(ctx context.Context, owner int64) (map[string]string, error)
	OpenRouterKey(ctx context.Context, owner int64) (string, bool, error)
	HasOpenRouterKey(ctx context.Context, owner int64) (bool, error)
}

// RoleReader reads a user's role (internal/users.PGStore.Role).
type RoleReader interface {
	Role(ctx context.Context, id int64) (authz.Role, error)
}

// The messages a member sees for a kind they have no credential for. One
// spelling each, shared by the dispatch form, the refusal and the tests.
const (
	NeedClaude     = "Add your Claude setup token in Account → Credentials to use Claude Code agents."
	NeedOpenRouter = "Add your OpenRouter key in Account → Credentials to use Standard agents."
)

// Plane decides whose model-provider credentials an OWNER's agents run on.
//
// 🔴 [Plane.usesEnvironment] IS THE ONE PREDICATE, AND EVERY QUESTION GOES
// THROUGH IT: which Claude accounts the dispatch form lists, which account a
// dispatch may select, which token a pod is built with, which OpenRouter key a
// gateway pod gets, and whether a kind is available at all. An admin uses the
// environment (the pool and MUSTER_AGENT_OPENROUTER_KEY); anyone else uses ONLY
// what they entered — and when they entered nothing, the answer is a refusal,
// never the environment's. A second copy of this decision (one in the api,
// one in the provisioner) is how a member's dispatch would one day list only
// their own accounts while their pod was built from the admin's.
type Plane struct {
	// EnvClaude is the environment pool (nil: the claude-code kind is off).
	EnvClaude ccpool.Source
	// EnvOpenRouterKey is MUSTER_AGENT_OPENROUTER_KEY ("" is today's
	// admin behaviour: a gateway pod with no key placed).
	EnvOpenRouterKey string
	// Personal is the per-owner store (nil: none configured — every member
	// then has no credential).
	Personal Personal
	// Roles reads an owner's role. nil: user 1 is admin, everyone else a
	// member (the seeded row, as internal/api's roleOf does with no store).
	Roles  RoleReader
	Logger *log.Logger
}

var _ ccpool.Source = (*Plane)(nil)

// usesEnvironment reports whether owner's agents run on the ENVIRONMENT's
// credentials — true exactly for an admin.
//
// 🔴 IT READS THE ROLE UNDER authz.System (a System-ledger site): the pod-spec
// path runs for an agent's owner from loops with no principal, and a member's
// principal cannot read the role of anyone but itself. It reads one column of
// one row and returns one boolean.
//
// 🔴 EVERY FAILURE IS "NO", NEVER "YES". A read error, a missing row or an
// unknown role means the member's own credentials — a wrong "yes" builds a
// member's pod with the operator's subscription; a wrong "no" refuses a
// dispatch with a message saying why.
func (p *Plane) usesEnvironment(ctx context.Context, owner int64) bool {
	if owner <= 0 {
		return false
	}
	if p.Roles == nil {
		return owner == authz.OperatorUserID
	}
	r, err := p.Roles.Role(authz.System(ctx), owner)
	if err != nil {
		if p.Logger != nil {
			p.Logger.Printf("credentials: reading user %d's role: %v (using their own credentials)", owner, err)
		}
		return false
	}
	return r == authz.RoleAdmin
}

// UsesEnvironment is [Plane.usesEnvironment] for the credentials page, which
// tells an admin where their credentials come from.
func (p *Plane) UsesEnvironment(ctx context.Context, owner int64) bool {
	return p.usesEnvironment(ctx, owner)
}

// Accounts implements [ccpool.Source]: owner's Claude accounts.
func (p *Plane) Accounts(ctx context.Context, owner int64) (map[string]string, error) {
	if p.usesEnvironment(ctx, owner) {
		if p.EnvClaude == nil {
			return map[string]string{}, nil
		}
		return p.EnvClaude.Accounts(ctx, owner)
	}
	if p.Personal == nil {
		return map[string]string{}, nil
	}
	return p.Personal.ClaudeTokens(ctx, owner)
}

// OpenRouterKey is the key owner's gateway pods are given. For a member with
// no key it is [ErrNoCredential] — never the environment's.
func (p *Plane) OpenRouterKey(ctx context.Context, owner int64) (string, error) {
	if p.usesEnvironment(ctx, owner) {
		return p.EnvOpenRouterKey, nil
	}
	if p.Personal == nil {
		return "", fmt.Errorf("%w: %s", ErrNoCredential, NeedOpenRouter)
	}
	key, ok, err := p.Personal.OpenRouterKey(ctx, owner)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoCredential, NeedOpenRouter)
	}
	return key, nil
}

// Unavailable returns, for each of kinds owner may NOT use for want of a
// credential, the sentence that says what to add. A kind absent from the map
// is available.
func (p *Plane) Unavailable(ctx context.Context, owner int64, kinds []string) map[string]string {
	out := map[string]string{}
	if p.usesEnvironment(ctx, owner) {
		return out
	}
	for _, k := range kinds {
		switch k {
		case agents.KindClaudeCode:
			acc, err := p.Accounts(ctx, owner)
			if err != nil || len(acc) == 0 {
				out[k] = NeedClaude
			}
		case agents.KindGateway, agents.KindHermes:
			// Both run on the OpenRouter key (the hermes kind by operator
			// decision: docs/design/hermes.md, "Credentials").
			ok := false
			if p.Personal != nil {
				var err error
				ok, err = p.Personal.HasOpenRouterKey(ctx, owner)
				ok = ok && err == nil
			}
			if !ok {
				out[k] = NeedOpenRouter
			}
		}
	}
	return out
}

// IsNoCredential reports whether err is a missing-credential refusal.
func IsNoCredential(err error) bool { return errors.Is(err, ErrNoCredential) }
