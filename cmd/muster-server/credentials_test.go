package main

import (
	"context"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/credentials"
)

// TestTheAgentPlaneResolvesCredentialsPerOwner: the agent plane this binary
// builds resolves every spec's credentials for the ROW's owner — even when no
// plane is handed in (buildAgentPlane, the pre-accounts wiring): a deployment
// with an environment OpenRouter key accepts the admin's dispatch and REFUSES a
// member's, naming what the member must add. There is no wiring of this binary
// whose adapter builds a member's pod from the environment.
func TestTheAgentPlaneResolvesCredentialsPerOwner(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	cfg.AgentOpenRouterKey = "sk-or-v1-FAKE-env-wiring-key-555555"

	for _, c := range []struct {
		owner  int64
		refuse bool
	}{{1, false}, {41, true}} {
		store := newDispatchRecorder()
		store.agent.OwnerID = c.owner
		prov, _, err := buildAgentPlane(cfg, store, logger)
		if err != nil || prov == nil {
			t.Fatalf("buildAgentPlane: %v", err)
		}
		err = prov.Dispatch(context.Background(), store.agent.ID, true)
		if c.refuse {
			if err == nil || !strings.Contains(err.Error(), credentials.NeedOpenRouter) {
				t.Fatalf("owner %d (a member, no credential) dispatched onto the environment's key: %v\n  transcript: %s",
					c.owner, err, store.transcript())
			}
			if strings.Contains(err.Error(), cfg.AgentOpenRouterKey) || strings.Contains(store.transcript(), cfg.AgentOpenRouterKey) {
				t.Fatalf("the refusal carries the key")
			}
			continue
		}
		if err != nil {
			t.Fatalf("control: the admin's dispatch: %v\n  transcript: %s", err, store.transcript())
		}
	}
}
