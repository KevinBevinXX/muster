package agentprovision

import (
	"context"
	"io"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 EVERY EXEC muster RUNS INTO AN AGENT'S CONTAINER GOES THROUGH a.exec OR
// a.execTTY, AND BOTH STRIP THE CONTAINER'S TURN CREDENTIALS.
//
// An exec does not inherit ccd's environment (which ccd has already scrubbed);
// it inherits the CONTAINER's, which holds MUSTER_GATEWAY_BEARER and, for a
// chief, CCD_GATEWAY_UPSTREAM_BEARER (agentspec.TurnCredentialEnv). Two routes
// took them from there to a tool call in the agent's own session:
//   - a job checkout runs git in the workspace the session writes, so a planted
//     core.fsmonitor or .git/hooks/post-checkout ran with them
//     (TestAJobCheckoutRunsNoPlantedCommandAndShowsItNoTurnCredential);
//   - a terminal's `tmux attach` client lives, dumpable, for as long as the
//     terminal is open, so any same-uid process could read them from
//     /proc/<pid>/environ (TestATerminalAttachHoldsNoTurnCredential).
//
// `env -u NAME …` removes them from the exec'd process and everything it
// starts, whatever that is. TestEveryExecIntoAnAgentGoesThroughTheScrub fails a
// new call of provision.Exec / provision.ExecTTY anywhere else in the module.
//
// ⚠ IT NEEDS `env` IN THE IMAGE (coreutils or busybox; every exec here already
// needs `sh`). It does not stop a planted command from RUNNING as the agent's
// user — which the session already is — only from seeing the credentials.

// agentExecCommand is cmd run with every agentspec.TurnCredentialEnv variable
// removed from its environment.
func agentExecCommand(cmd []string) []string {
	out := make([]string, 0, 1+2*len(agentspec.TurnCredentialEnv)+len(cmd))
	out = append(out, "env")
	for _, name := range agentspec.TurnCredentialEnv {
		out = append(out, "-u", name)
	}
	return append(out, cmd...)
}

// exec is provision.Exec into an agent's instance, with the credentials stripped.
func (a *Adapter) exec(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return provision.Exec(ctx, a.driver, ref, agentExecCommand(cmd), stdin, stdout, stderr)
}

// execTTY is provision.ExecTTY into an agent's instance, with the credentials
// stripped.
func (a *Adapter) execTTY(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	return provision.ExecTTY(ctx, a.driver, ref, agentExecCommand(cmd), stdin, stdout, resize)
}
