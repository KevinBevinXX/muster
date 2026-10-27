package api

// toolNameAliases maps a RETIRED tool name to the one that replaced it, for the
// two places that take a name from outside: dispatchAgentTool and /mcp's
// tools/call.
//
// 🔴 agents_list WAS RENAMED fleet_list BECAUSE THE GATEWAY RUNTIME HAS A BUILT-IN
// TOOL OF THAT NAME, AND IT REFUSES THE WHOLE TURN OVER IT. A gateway-runtime
// agent on a live deployment failed EVERY turn with "client tool name conflict:
// agents_list"; the runtime image's code (tag 2026.5.7) checks every client tool
// against its own built-ins before the turn runs and throws on any match. The
// rename is for every runtime, not only the gateway, so a tool has ONE name
// wherever it is offered;
// TestNoChiefToolCollidesWithTheGatewayRuntimesBuiltins pins the whole registry
// against the runtime's built-in set.
//
// ⚠ THE ALIAS IS FOR A CLAUDE CODE AGENT THAT IS ALREADY RUNNING. Its CLI read
// /mcp's tool list when it started and keeps it until it restarts, so until then
// it still calls the old name. The alias is NEVER LISTED — a listing is what the
// gateway runtime checks, so listing the old name would bring the conflict back.
//
// CLOSING CONDITION: delete the entry once every claude-code agent pod has
// restarted since the release that renamed the tool (its pod's start time is
// later than that deploy: `kubectl get pod -n <agent ns> -o
// jsonpath='{.items[*].status.startTime}'`). Whoever deploys the release after
// that one checks it.
var toolNameAliases = map[string]string{
	"agents_list": fleetListToolName,
}

// canonicalToolName is name, or the name a retired one was renamed to.
func canonicalToolName(name string) string {
	if to, ok := toolNameAliases[name]; ok {
		return to
	}
	return name
}
