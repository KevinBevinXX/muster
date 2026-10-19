package main

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

// TestCcdReadsTheRepositoryVariablesAgentspecWrites: the variables muster
// writes into a claude-code pod for its repository (internal/agentspec
// claudeCodeEnv) are the ones ccd reads (checkout.go, and CCD_WORKSPACE in
// main.go). A rename on either side leaves an agent whose repository is never
// checked out — and whose CLI starts at once in an empty directory — with
// nothing failing but this.
func TestCcdReadsTheRepositoryVariablesAgentspecWrites(t *testing.T) {
	for _, c := range []struct{ ccd, spec string }{
		{envRepo, agentspec.EnvCCDRepo},
		{envRepoBranch, agentspec.EnvCCDRepoBranch},
		{"CCD_WORKSPACE", agentspec.EnvCCDWorkspace},
	} {
		if c.ccd != c.spec {
			t.Errorf("ccd reads %q, agentspec writes %q", c.ccd, c.spec)
		}
	}
	// And the checkout is under the volume the image mounts, so a pod restart
	// finds it.
	if agentspec.ClaudeCodeWorkspacePath != "/data/workspace" {
		t.Errorf("ClaudeCodeWorkspacePath = %q", agentspec.ClaudeCodeWorkspacePath)
	}
}
