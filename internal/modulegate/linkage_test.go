// Package modulegate holds the gates that are about the MODULE rather than
// about any package in it: what links into a binary, and whether the claims
// this tree's comments make about its own test suite are true.
//
// 🔴 IT IS TEST FILES ONLY, ON PURPOSE. A gate that shipped an exported symbol
// would be a package that itself links into nothing, which is precisely the
// state linkage_test.go exists to detect — the gate would be an instance of the
// defect. `go build ./...` produces nothing here; `go test ./...` runs it.
package modulegate

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// modulePath is this module's import path. Spelled once.
const modulePath = "github.com/ZacxDev/muster"

// ---------------------------------------------------------------------------
// THE LINK GRAPH.
//
// 🔴 WHAT THIS GATE IS FOR, IN ONE SENTENCE: for the whole of phase 2 this
// module's HTTP layer, its views and fourteen other packages compiled, were
// tested, and LINKED INTO NO BINARY. `go list -deps ./cmd/...` resolved exactly
// four packages of this module — cmd/muster, cmd/muster-migrate, internal/db
// and internal/taskstatus — and internal/api appeared in none of them. A reader
// who opened routes.go, saw a RegisterRoutes that registers 99 routes, and
// concluded that a service serves them was reading correctly and concluding
// wrongly. Nothing could tell them otherwise, because "is this reachable from a
// process" is not a question any test in a package can ask about itself.
//
// 🔴 THIS GATE ALONE IS NOT ENOUGH AND MUST NOT BE READ AS ENOUGH. Linkage is a
// claim about the IMPORT GRAPH, and an import can be a blank `_` import that
// executes nothing. What would this test still accept? A cmd/ package that
// imports internal/api and never calls Handler; one that builds a Server and
// never listens. TestTheServerListensAndServesHealth in cmd/muster-server is
// the other half — it drives a real listener on a real port — and the two are
// only meaningful together. Deleting either leaves a hole the other does not
// cover.
// ---------------------------------------------------------------------------

// notLinkedLedger is every package of this module that is deliberately NOT
// reachable from any binary, with the reason.
//
// 🔴 IT IS AN ASSERTED LEDGER, NOT AN ALLOWLIST, AND THE TEST FAILS WHEN THE SET
// GROWS *OR* SHRINKS. A plain allowlist would silently absorb the next package
// that stops linking — which is exactly how sixteen of them accumulated. An
// entry leaving the ledger is also a failure: it means something started
// linking that was documented as unreachable, and the reason below is now a
// lie a reader would trust.
var notLinkedLedger = map[string]string{
	"internal/dbtest": "the Postgres test harness. It is imported only by _test.go files " +
		"BY DESIGN — a test fixture that linked into the server binary would ship " +
		"CREATE DATABASE privileges into production.",

	"internal/modulegate": "this package. Test files only; it has no importable symbol.",

	"internal/gotrue/gotruetest": "the in-memory GoTrue the Go tests drive the session tier " +
		"against. Imported only by _test.go files: a fake identity provider linked into the " +
		"server would be a way to mint sessions.",
	"internal/hermeshost/hermestest": "fake Hermes homes (a scratch state.db from the pinned schema, " +
		"breadcrumbs, decoy secret files) for the Hermes mirroring tests. Imported only by _test.go files and " +
		"the browser suite's build-tagged fake host (cmd/muster-mirror's e2emirror build, never shipped): " +
		"a writer of Hermes's state.db linked into muster-mirror would break its read-only promise.",
	"internal/agentprovision/worktreetest": "the repository a machine job's worktree is made from in " +
		"the job tests: an origin, a clone, and a commit carrying hostile agent configuration (hooks and an " +
		"MCP server that write marker files). Imported only by _test.go files, the real-CLI one among them.",
	"internal/uploads/uploadstest": "in-memory fakes of the upload row store and object store, for the " +
		"route tests. Imported only by _test.go files: a row store with no row-level security linked " +
		"into the server would be a way round the uploads table's owner isolation.",

	"internal/askdrive/fakepicker": "a stand-in for Claude Code's AskUserQuestion picker on a raw tty, " +
		"built by internal/mirrorhost's tmuxit suite to run in a real tmux pane. A test tool: no shipped " +
		"binary types into a fake picker.",

	"e2e/seed": "the browser suite's fixture loader (`go run ./e2e/seed`). It TRUNCATES " +
		"the board before seeding, so it must never be reachable from a shipped binary; it " +
		"also refuses any database whose name does not end in _e2e.",
	"e2e/mkmember": "the byo-credentials browser spec's member maker (`go run ./e2e/mkmember`). " +
		"It writes a users row as the login role, bypassing row-level security, so it must " +
		"never be reachable from a shipped binary; it refuses any database not ending in _e2e.",
	"e2e/mkchat": "the mirror-chat browser spec's agent-with-a-chat maker (`go run ./e2e/mkchat`), " +
		"which also deletes that agent: fixture writes, so it must never be reachable from a " +
		"shipped binary; it refuses any database not ending in _e2e.",
	"e2e/internal/e2edb": "the e2e fixture commands' one database guard (refuse any database " +
		"not ending in _e2e, as pgx resolves the name). Imported only by the e2e/ commands above, " +
		"which are themselves unreachable from a shipped binary.",
	"e2e/heldfix": "the held-kickoff browser spec's fixture (`go run ./e2e/heldfix`): an agent row whose first " +
		"turn the kickoff deliverer holds for its github grant, written as the login role with row-level " +
		"security's scope set to all, so it must never be reachable from a shipped binary; it refuses any " +
		"database not ending in _e2e.",
	"e2e/sqfix": "the send-queue browser spec's fixture (`go run ./e2e/sqfix`): ages a queued message, or strands it " +
		"mid-send, so the server's own dispatcher fails it; run by the spec, never linked into a binary.",
	"e2e/pluginfix": "the plugins browser spec's fixture (`go run ./e2e/pluginfix`): a chief agent, a " +
		"delivered message and a lapsed lease, written as the login role with row-level security's " +
		"scope set to all, so it must never be reachable from a shipped binary; it refuses any " +
		"database not ending in _e2e.",

	// 🔴 internal/agentspec AND internal/provision/k8s WERE BOTH HERE, AND THEIR
	// DEPARTURE IS THE ONLY MECHANICAL SIGNAL PLAN STEP 22b HAD. Both entries said
	// so in their own text: agentspec's read "THIS ENTRY DISAPPEARING IS THE
	// SIGNAL THAT 22b LANDED … Do not wire a fake consumer to clear it", and
	// k8s's read "This entry disappearing is the signal that the seam CLOSED".
	// What removed them is cmd/muster-server/provisioner.go — it CONSTRUCTS the
	// Kubernetes driver from a real in-cluster client and hands it to
	// internal/agentprovision, which builds specs with agentspec.Build. Neither
	// arrived via a test fixture or a blank import; the deletion of these two
	// entries is what makes that a checkable claim rather than a described one.
	//
	// ⚠ THE LEDGER IS NOT WHERE THE REST OF THAT STEP IS CHECKED. Linkage says
	// "reachable from a process", nothing more. internal/agentgateway implements the
	// chat half now and links the same way — through cmd/muster-server. ⚠ THAT IS A
	// CLAIM ABOUT LINKAGE AND NOTHING ELSE, and the list of what it does not cover
	// is SHORTER now without the point changing. It read: "a chat turn against an
	// agent this binary PROVISIONED still resolves no address (agentspec declares no
	// port) and would carry a credential the container never received, and a KICKOFF
	// is undeliverable besides". The first two are fixed — agentspec declares the
	// gateway port and ships the row's token under the name the bearer is derived
	// from — and NEITHER fix was visible here, which is this gate's whole point
	// restated from the other side: the ledger stayed green through a defect that
	// made the linked code unusable, and it stays green through the change that made
	// it usable. The KICKOFF it also named ("nothing calls the gateway on the
	// dispatch path") is closed by internal/agentkickoff; what remains open is a
	// turn against an instance this binary created ON A CLUSTER, which no in-repo
	// test can make. See cmd/muster-server/doc_seams.go entry 1. TestBuildingTheKubernetesProvisionerUsesTheRealDriver,
	// TestAnAgentThisBinaryProvisionsResolvesAnEndpoint, the gateway's own wiring
	// tests and the boot banner's two-tier readback are the parts this gate cannot
	// be.

	"internal/provision/provisiontest": "the driver contract suite, run by driver " +
		"implementations from their own _test.go files. Same reason as internal/dbtest.",
	"images/hermes-agent/smoke": "the hermes image's smoke test (`make hermes-smoke`, `go run`): it starts " +
		"Docker containers, a stub model and a fake muster relay on loopback, so it is a harness run by hand " +
		"against a built image and must never be reachable from a shipped binary.",
}

// TestTheHTTPLayerLinksIntoABinary is the gate that makes "muster serves" a
// fact about the link graph rather than a sentence in a README.
func TestTheHTTPLayerLinksIntoABinary(t *testing.T) {
	root := moduleRoot(t)
	linked := depsOf(t, root, "./cmd/...")

	// 🔴 POSITIVE CONTROL FIRST, AND ITS VERDICT IS REPORTED ALONGSIDE THE
	// RESULT. A `go list` that returned nothing — wrong directory, broken
	// toolchain, a module that does not build — produces an EMPTY set, and an
	// empty set would sail through a membership assertion phrased as "is api
	// absent? then fail" only because nothing is present at all. internal/db has
	// been reachable from cmd/muster-migrate since before this gate existed, so
	// its presence proves the instrument can observe a link.
	if !linked[modulePath+"/internal/db"] {
		t.Fatalf("positive control FAILED: internal/db is not in the deps of ./cmd/..., "+
			"which it has been since cmd/muster-migrate existed. This gate is not "+
			"measuring the link graph — it read %d package(s) in total. Check that "+
			"`go list -deps ./cmd/...` works in %s.", len(linked), root)
	}
	t.Logf("positive control: internal/db is linked, and `go list -deps ./cmd/...` "+
		"resolved %d packages in total", len(linked))

	// The HTTP layer and the view layer, named individually. They are named
	// rather than derived because these two are the deliverable: a partition
	// check alone would pass if both moved onto the ledger with a plausible
	// reason written beside them.
	for _, want := range []string{"internal/api", "internal/ui"} {
		if !linked[modulePath+"/"+want] {
			t.Errorf("%s is NOT reachable from any binary.\n"+
				"  It compiles and its tests pass, and no process can execute a line of it.\n"+
				"  That was true of this module for the whole of phase 2 and the README\n"+
				"  said so in one place while contradicting itself in another.\n"+
				"  Wire it into a cmd/ package, or the routes it registers serve nobody.", want)
		}
	}
}

// TestEveryPackageEitherLinksOrIsLedgered is the relationship half: the module
// partitions into "reachable from a binary" and "written down as not, with a
// reason", and nothing sits outside both.
//
// 🔴 IT IS THE HALF THAT CATCHES THE *NEXT* ONE. Naming internal/api and
// internal/ui pins today's finding; this pins the class. A package added in six
// months that links into nothing fails here with no test written for it.
func TestEveryPackageEitherLinksOrIsLedgered(t *testing.T) {
	root := moduleRoot(t)
	all := depsOf(t, root, "./...")
	linked := depsOf(t, root, "./cmd/...")

	// 🔴 POSITIVE CONTROL ON THE SECOND INSTRUMENT TOO. `./...` and `./cmd/...`
	// are separate invocations and either can come back empty. A module always
	// has more packages than it has linked packages here, so this comparison
	// cannot be satisfied by two empty sets.
	if len(all) == 0 || len(all) <= len(linked) {
		t.Fatalf("positive control FAILED: `go list -deps ./...` saw %d package(s) and "+
			"`./cmd/...` saw %d. The first must be strictly larger — at minimum "+
			"internal/dbtest is in one and not the other.", len(all), len(linked))
	}

	var unlinked []string
	for pkg := range all {
		rel, ok := relPath(pkg)
		if !ok || linked[pkg] {
			continue
		}
		unlinked = append(unlinked, rel)
	}
	sort.Strings(unlinked)

	seen := map[string]bool{}
	for _, rel := range unlinked {
		seen[rel] = true
		if _, ok := notLinkedLedger[rel]; !ok {
			t.Errorf("%s links into NO binary and is not on the ledger.\n"+
				"  Either wire it into a cmd/ package, or add it to notLinkedLedger\n"+
				"  in this file with the reason it is unreachable on purpose.", rel)
		}
	}
	for rel := range notLinkedLedger {
		if !seen[rel] {
			t.Errorf("%s is on the not-linked ledger but IS reachable from a binary now.\n"+
				"  The reason recorded beside it is no longer true, and a reader would\n"+
				"  trust it. Remove the entry — and if it was a seam, say so in the\n"+
				"  change that closed it.", rel)
		}
	}

	t.Logf("link partition: %d package(s) reachable from ./cmd/..., %d ledgered as not: %s",
		countOwn(linked), len(unlinked), strings.Join(unlinked, ", "))
}

// relPath turns a full import path into its path relative to the module, and
// reports whether it belongs to this module at all.
func relPath(importPath string) (string, bool) {
	if importPath == modulePath {
		return ".", true
	}
	rest, ok := strings.CutPrefix(importPath, modulePath+"/")
	return rest, ok
}

// countOwn counts how many of a dep set belong to this module.
func countOwn(set map[string]bool) int {
	n := 0
	for p := range set {
		if _, ok := relPath(p); ok {
			n++
		}
	}
	return n
}

// depsOf returns the transitive import closure of pattern as a set.
//
// ⚠ A FAILURE HERE IS t.Fatal, NEVER t.Skip. `tests/verdict.py` refuses a run
// that skipped anything, because `go test` exits 0 with skipped tests and a skip
// is invisible in a verdict. A gate that quietly skipped when the toolchain was
// missing would be a gate that reports green on the one machine where it could
// not run.
func depsOf(t *testing.T, root, pattern string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pattern)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s in %s: %v\n%s", pattern, root, err, stderr)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

// moduleRoot finds the directory holding go.mod by walking up from the test's
// working directory.
//
// ⚠ IT DOES NOT USE `go env GOMOD`. That reports the go.mod of whatever module
// the CURRENT DIRECTORY belongs to, which is the same answer here and a
// different one the moment this file is vendored or copied — and the failure
// would be a gate measuring another module's link graph and passing.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.Contains(string(b), "module "+modulePath) {
				return dir
			}
			t.Fatalf("found a go.mod at %s but it is not %s's", dir, modulePath)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod for %s above the test's working directory", modulePath)
		}
		dir = parent
	}
}
