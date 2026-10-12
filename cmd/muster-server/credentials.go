package main

import (
	"context"
	"errors"
	"log"

	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// buildCredentialPlane builds the ONE decision of whose model-provider
// credentials an owner's agents run on (credentials.Plane): the environment's
// for an admin, the owner's own — and only those — for anyone else.
//
// personal and roles may be nil (no credential store / no database). With no
// personal store every member simply HAS NO credential: their kinds are
// unavailable, never served from the environment.
//
// ⚠ CALLERS PASS A CONCRETE POINTER ONLY WHEN IT IS NON-nil. A typed nil inside
// the interface would make the plane's nil checks false (buildGatewayWith's
// doc has the same trap).
func buildCredentialPlane(cfg config, personal credentials.Personal, roles credentials.RoleReader, logger *log.Logger) (*credentials.Plane, error) {
	p := &credentials.Plane{
		EnvOpenRouterKey: cfg.AgentOpenRouterKey,
		Personal:         personal,
		Roles:            roles,
		Logger:           logger,
	}
	if cfg.claudeCodeEnabled() {
		env, err := ccpool.NewStatic(cfg.AgentCCTokens)
		if err != nil {
			return nil, err
		}
		p.EnvClaude = env
	}
	return p, nil
}

// credResolver is agentprovision.CredentialResolver over the account pool and
// the plane: a pod's Claude token is the pool's token for the agent's OWNER and
// account; its OpenRouter key is the plane's answer for that owner.
type credResolver struct {
	pool  *ccpool.Pool
	plane *credentials.Plane
}

func (r credResolver) ClaudeToken(ctx context.Context, owner int64, account string) (string, error) {
	if r.pool == nil {
		return "", errors.New("the claude-code kind is not enabled on this deployment")
	}
	return r.pool.Token(ctx, owner, account)
}

func (r credResolver) OpenRouterKey(ctx context.Context, owner int64) (string, error) {
	return r.plane.OpenRouterKey(ctx, owner)
}

// credentialPage is api.CredentialStore: the per-owner store, with the plane's
// "does this user run on the environment's credentials" beside it.
type credentialPage struct {
	*credentials.PGStore
	plane *credentials.Plane
}

func (c credentialPage) UsesEnvironment(ctx context.Context, owner int64) bool {
	return c.plane.UsesEnvironment(ctx, owner)
}
