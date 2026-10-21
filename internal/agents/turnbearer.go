package agents

// TurnBearer is the credential muster presents to a's turn API, and so the one a's
// pod must accept: a's own [Agent.GatewaySecret] when it has one, else derive
// over a's hooks token (the deployment's agentgateway.Runtime scheme).
//
// 🔴 ONE RULE, ONE PLACE. The sender (agentgateway's reach) and the spec builder
// (agentspec's claude-code Build, which ships the pod the value it must accept)
// both call this, so the two cannot disagree — a disagreement is a 401 on every
// turn that reads as a bad credential.
//
// ⚠ THE FALLBACK IS FOR ROWS, NOT FOR KINDS. Only claude-code rows are ever
// minted a secret (agentprovision's ensureGatewaySecret); every other kind, and a
// claude-code row whose pod predates the secret, keeps the derived bearer its pod
// was built with. Such a claude-code pod's session can derive that bearer from
// its own MUSTER_HOOK_TOKEN, and its ccd (from this change on) refuses to start
// on it. Stop then Start is the one route that moves such an agent over: Start
// re-applies a claude-code spec, which mints the secret and ships it (a
// claude-code model change is refused 409, so it is not a route).
func TurnBearer(a Agent, derive func(hooksToken string) string) string {
	if a.GatewaySecret != "" {
		return a.GatewaySecret
	}
	return derive(a.HooksToken)
}
