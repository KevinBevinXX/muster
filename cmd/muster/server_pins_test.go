package main

import (
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/taskstatus"
)

// ---------------------------------------------------------------------------
// THE CLIENT<->SERVER PINS.
//
// 🔴 EVERY IMPORT IN THIS FILE IS TEST-ONLY, AND THAT IS THE WHOLE ARRANGEMENT.
// The shipped binary deliberately does not link internal/api or internal/agents
// — a machine client that pulled the server and its store in would be a build
// edge inviting the next reach for a symbol, and this project's central claim is
// that the two halves are separable. So each value the client copies is pinned
// here instead, from a file the compiler keeps honest but the linker never sees.
//
// ⚠ EACH PIN IS BIDIRECTIONAL. A one-directional pin — "the client's value is in
// the server's set" — is the shape that let an upstream status vocabulary drift:
// a value added server-side left the check green while the client refused it.
// Equality in both directions is what makes a copy safe; anything less means the
// copy should be an import instead.
// ---------------------------------------------------------------------------

// taskstatusAll re-exports the shared vocabulary for the tests in this package
// that need it, so only THIS file carries the import.
func taskstatusAll() []string { return taskstatus.All() }

// TestTheCLIBuildVersionDefaultMatchesTheServers pins the two DEFAULTS equal,
// which is a claim about the UNSTAMPED build and nothing more.
//
// 🔴 ONLY ONE OF THESE TWO IS EVER STAMPED, SO DO NOT READ THIS AS "RELEASES
// AGREE". The server is linked with
// `-X github.com/ZacxDev/muster/internal/api.BuildVersion=${VERSION}` in
// Dockerfile's build stage; `-X main.buildVersion=…` is NOWHERE AN ACTUAL BUILD
// FLAG in this repository — the only live `-X` bindings are that one and
// flake.nix's `-X main.buildRevision=${version}`. The spelling is MENTIONED in
// several comments across the tree (no count is given here on purpose: one was,
// and it was wrong), and the mention worth knowing about is Dockerfile's, which
// RECOMMENDS adding `-X main.buildVersion=${VERSION}` to the agent-side CLI
// build as a deliberately separate change. Verify with a sweep, not from this
// comment: `find . -path ./.git -prune -o -type f -print0 | xargs -0 grep -n
// 'main\.buildVersion'` — plain recursive grep honours .gitignore here.
// Against any released server, therefore, /health answers a real semver while
// this client still says "dev", and warnSkew's note fires on EVERY command. That
// asymmetry is at RELEASE time, not in the defaults, and this test does not
// close it — closing it means stamping the CLI at release, which nothing in this
// tree does.
//
// ⚠ WHAT IT DOES BUY, which is why it is not vacuous: an UNSTAMPED server —
// `make run`, `go run ./cmd/muster-server`, docker-compose.test.yml, the whole
// local loop — reports api.BuildVersion's default from /health. Equal defaults
// are what keep that case SILENT instead of printing a skew note against a
// client that matches it perfectly. Measured: mutating api.BuildVersion turns
// this red with the message below.
func TestTheCLIBuildVersionDefaultMatchesTheServers(t *testing.T) {
	if buildVersion != api.BuildVersion {
		t.Fatalf("🔴 the CLI's default buildVersion is %q and the server's api.BuildVersion is %q.\n"+
			"warnSkew compares this client's literal against what /health reports, so a divergence "+
			"makes the version note fire on EVERY command on EVERY host against an UNSTAMPED server "+
			"— the local dev loop — which trains a reader to ignore the one signal that catches a "+
			"genuinely stale client. Only the server half is stamped at release (Dockerfile); the "+
			"CLI's is never stamped, so these DEFAULTS are the whole agreement there is.",
			buildVersion, api.BuildVersion)
	}
}

func TestTheUnarmedMarkerMatchesTheServer(t *testing.T) {
	if hookUnarmedField != api.HookUnarmedField {
		t.Fatalf("🔴 the client reads %q out of a 503 body and the server writes %q.\n"+
			"That marker is the ONLY discriminator between 'this surface is unarmed' (exit 9, arm "+
			"the server) and 'the backend is sick' (exit 1, retry). A mismatch silently reports "+
			"every arming refusal as an outage.", hookUnarmedField, api.HookUnarmedField)
	}
}

func TestTheAgentMessageLimitsMatchTheServers(t *testing.T) {
	if agentMessagesDefaultLimit != api.AgentMessagesDefaultLimit {
		t.Errorf("🔴 `agent messages --limit` defaults to %d and the server defaults to %d; --help "+
			"states a number the server will not apply", agentMessagesDefaultLimit, api.AgentMessagesDefaultLimit)
	}
	if agentMessagesMaxLimit != api.AgentMessagesMaxLimit {
		t.Errorf("🔴 `agent messages --help` advertises a ceiling of %d and the server clamps at %d",
			agentMessagesMaxLimit, api.AgentMessagesMaxLimit)
	}
}

// ⚠ THERE IS DELIBERATELY NO STRUCTURAL PIN FOR THE PROVENANCE HEADER NAMES,
// AND THE REASON IS WORTH STATING RATHER THAN LEAVING AS AN ABSENCE.
//
// The three X-Muster-* names the client sends are read by unexported functions
// in internal/api (taskSource, taskSessionHost), so nothing in this package can
// drive the server's real reader. The available substitutes are both worse than
// nothing: comparing two constants proves only that someone typed the same
// string twice, and scanning internal/api's SOURCE for the literals rebuilds
// exactly the CLI-to-server-source coupling this extraction removed.
//
// What pins the relationship instead is BEHAVIOURAL and lives in seam_test.go:
// the real command tree, against the real api.Handler, over a real database,
// watching one task's session thread move 0 -> 1. That test is Postgres-gated
// and SKIPS without one — but `make test` exports MUSTER_TEST_REQUIRE_DB=1,
// which turns that skip into a failure, so the project's own gate cannot produce
// a green that never exercised the seam.
