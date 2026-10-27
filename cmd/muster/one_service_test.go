package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ONE BASE URL IS SUFFICIENT — CHECKED, NOT ASSUMED.
//
// 🔴 THIS IS THE GUARD FOR TWO STRUCTURAL PROBLEMS THE EXTRACTION PLAN NAMED,
// AND NEITHER IS VISIBLE IN A GREEN BUILD.
//
//  1. config.apiURL is a SINGLE base URL. The upstream CLI had the same shape
//     while its verbs were about to straddle two services, which would have
//     made every verb aimed at the second one land on the first — as a 404 from
//     a router that has never heard of the route, i.e. exit 7, "this client is
//     newer than the server". A confident, wrong diagnosis.
//  2. client.warnSkew probes /health ONCE against a pinned buildVersion and
//     therefore ASSUMES ONE SERVER. With verbs spread over two, the note would
//     be a statement about whichever process happened to answer /health, and it
//     would be wrong about the other one silently and forever.
//
// Both problems have the same precondition — this binary talks to more than one
// service — and this test is what makes that precondition FALSE rather than
// merely unlikely. It runs every verb, reads the routes the server ACTUALLY
// received, and demands that each be in the server's own committed route table.
//
// ⚠ IT READS THE GOLDEN, NOT THE SERVER'S SOURCE, AND THAT IS THE THIRD
// STRUCTURAL PROBLEM RESOLVED. Upstream, the CLI's tier ledger scanned
// internal/api's source for route literals, which welded the CLI package to the
// server package: the CLI could not be split out without that test dying.
// internal/api/testdata/routes.golden is a committed ARTEFACT, regenerated and
// reviewed by its own gate in internal/api, so reading it couples this file to
// a checked-in file rather than to another package's AST.
//
// ⚠ WHAT IT DOES NOT CLAIM: nothing here says a route is on the right AUTH
// TIER. The golden's own header says it does not record auth wrappers. This is
// a reachability guard, and that is all.
// ---------------------------------------------------------------------------

// routeGoldenPath is the server's committed route table, relative to this
// package.
const routeGoldenPath = "../../internal/api/testdata/routes.golden"

// readRouteGolden returns the route patterns the server registers.
func readRouteGolden(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(routeGoldenPath))
	if err != nil {
		t.Fatalf("reading the server's route golden at %s: %v.\n"+
			"This guard's whole claim is that every route this CLI calls is served by THIS project. "+
			"If the golden moved, point this constant at it — do not delete the guard.",
			routeGoldenPath, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

// concreteToPattern turns an exercised route ("GET /api/tasks/5") into the
// pattern shape the golden records ("GET /api/tasks/{id}").
//
// 🔴 IT MATCHES BY SHAPE, SEGMENT BY SEGMENT, RATHER THAN BY STRING. A literal
// map from concrete route to pattern would be a third copy of the same
// information and would go stale in the one direction that matters: a verb
// repointed at a new route would simply be absent from the map, and an absent
// entry reads as "nothing to check".
func concreteToPattern(concrete string, golden map[string]bool) (string, bool) {
	if golden[concrete] {
		return concrete, true
	}
	method, path, ok := strings.Cut(concrete, " ")
	if !ok {
		return "", false
	}
	want := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for g := range golden {
		gm, gp, ok := strings.Cut(g, " ")
		if !ok || gm != method {
			continue
		}
		have := strings.Split(strings.TrimPrefix(gp, "/"), "/")
		if len(have) != len(want) {
			continue
		}
		match := true
		for i := range have {
			if strings.HasPrefix(have[i], "{") && strings.HasSuffix(have[i], "}") {
				// A wildcard segment matches any NON-EMPTY concrete segment. The
				// empty case is what expandPath refuses, so it must not match here
				// either.
				if want[i] == "" {
					match = false
					break
				}
				continue
			}
			if have[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return g, true
		}
	}
	return "", false
}

func TestEveryWiredRouteIsServedByThisProject(t *testing.T) {
	golden := readRouteGolden(t)

	// POSITIVE CONTROL, first: prove the reader and the matcher can produce a
	// non-zero answer at all. A zero from a parser wired to nothing is
	// indistinguishable from a clean result.
	if len(golden) < 50 {
		t.Fatalf("the route golden parsed to %d routes; this guard is not reading the server's "+
			"route table", len(golden))
	}
	if _, ok := concreteToPattern("GET /api/tasks/5", golden); !ok {
		t.Fatalf("the matcher cannot resolve a route the golden demonstrably carries "+
			"(GET /api/tasks/{id}); every assertion below would be vacuous. Golden has %d routes.",
			len(golden))
	}
	// NEGATIVE CONTROL: it must also be able to say NO. Without this, a matcher
	// that returned true unconditionally would pass every assertion below.
	if got, ok := concreteToPattern("GET /definitely/not/a/muster/route", golden); ok {
		t.Fatalf("the matcher resolved a route that does not exist, to %q — it cannot refuse, "+
			"so it is testing nothing", got)
	}
	// And the arity half of the same control: a right-prefix, wrong-length path
	// must not match a wildcard pattern.
	if got, ok := concreteToPattern("GET /api/tasks/5/extra/segments", golden); ok {
		t.Fatalf("the matcher resolved an over-long path to %q; wildcard segments must not "+
			"swallow extra ones", got)
	}

	// 🔴 THE ROUTES ARE OBSERVED ON THE WIRE, NOT READ OUT OF THE LEDGER, AND
	// THAT WAS A MUTATION FINDING RATHER THAN a design choice made up front.
	// Checking `w.wantRoute` against the golden checks the LEDGER's claim about
	// where a verb goes — so repointing a verb's route CONSTANT at another
	// service's path left this test green (measured: SURVIVED, 1 pass, 0 fail)
	// while the ledger row still named the old, served path. The chain
	// code -> ledger -> golden did close, via TestEveryCommandReachesItsRoute,
	// but a guard that needs a second guard to mean what its message says is a
	// guard whose message is wrong. Running the verbs and reading what the server
	// actually received makes this one pin code -> golden directly.
	observed := observedRoutes(t)
	if len(observed) == 0 {
		t.Fatal("no routes were observed; the sweep exercised nothing and every assertion below " +
			"would be vacuous")
	}
	var missing []string
	for route, verbs := range observed {
		if _, ok := concreteToPattern(route, golden); !ok {
			sort.Strings(verbs)
			missing = append(missing, strings.Join(verbs, ", ")+" -> "+route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("🔴 %d observed request(s) went to routes this project does NOT serve:\n  %s\n\n"+
			"config.apiURL is a single base URL and warnSkew probes ONE /health, so a verb aimed at "+
			"a second service would silently land on this one — as exit 7, 'this client is newer "+
			"than the server', which is a confident wrong diagnosis. Either the route belongs in "+
			"muster (add it, and the golden will record it), or the verb belongs in the other "+
			"binary. Do not add a second base URL without revisiting warnSkew, which assumes one "+
			"server.", len(missing), strings.Join(missing, "\n  "))
	}
}

// observedRoutes runs every verb in the ledger against a fake server and
// returns {"METHOD /path": [verb names]} for every request that reached the
// wire.
//
// ⚠ IT IGNORES EXIT CODES ON PURPOSE. A verb repointed at an unserved route
// FAILS (the fake answers its router 404, exit 7) — and the request it sent on
// the way is exactly the evidence this sweep wants. Insisting on exit 0 would
// throw away the observation in the one case that matters.
//
// 🔴 IT ALSO CAPTURES THE SKEW PROBE, which no verb calls and no ledger row
// names. warnSkew's GET /health is a request this binary makes, so it belongs in
// the same check; deriving the set from the ledger alone would leave it
// unexamined.
func observedRoutes(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, w := range wiring {
		h := newHarness(t)
		// A server that is NEWER than this client, so the skew probe's note fires
		// and the probe is genuinely exercised rather than short-circuited.
		h.version = "0.0.0-newer-than-this-cli"
		h.json("GET /api/agents", http.StatusOK,
			`[{"id":11,"name":"clever-fox"},{"id":12,"name":"agent"}]`)
		h.json("GET /api/agents/agent/messages", http.StatusOK, `{"messages":[],"count":0}`)
		h.json("GET /api/tasks", http.StatusOK, `[]`)
		h.json("GET /api/tasks/5", http.StatusOK, `{"id":5}`)
		h.json("POST /api/tasks", http.StatusOK, `{"id":5}`)
		h.json("PATCH /api/tasks/5/status", http.StatusOK, `{"id":5}`)
		h.json("POST /api/tasks/5/comments", http.StatusOK, `{"id":5,"author":"claude-code"}`)
		h.json("GET /agent/task", http.StatusOK, `{"id":42}`)
		h.json("POST /agent/task/comment", http.StatusOK, `{"id":1}`)
		h.json("PATCH /agent/task/status", http.StatusOK, `{"id":42}`)

		h.runCLI(w.args...)
		for _, r := range h.requests() {
			key := r.method + " " + strings.SplitN(r.uri, "?", 2)[0]
			if !slices.Contains(out[key], w.name) {
				out[key] = append(out[key], w.name)
			}
		}
	}
	return out
}

// TestTheSkewProbeTargetIsObservedAndServed pins the probe explicitly, so its
// presence in the sweep above is asserted rather than assumed.
//
// 🔴 WITHOUT THIS, A warnSkew THAT NEVER FIRED WOULD PASS THE SWEEP VACUOUSLY —
// a route nobody requests cannot be observed going to the wrong place.
func TestTheSkewProbeTargetIsObservedAndServed(t *testing.T) {
	golden := readRouteGolden(t)
	if !golden["GET /health"] {
		t.Fatalf("🔴 warnSkew probes GET /health and this project does not register it.\n"+
			"The probe would hit whatever else answers that URL, and the version note would be a "+
			"statement about a different process. Golden carries %d routes.", len(golden))
	}
	observed := observedRoutes(t)
	callers, ok := observed["GET /health"]
	if !ok {
		t.Fatalf("🔴 no verb produced a GET /health, so the version-skew probe never fired. The "+
			"route sweep cannot see a probe that does not happen. Observed: %v", observed)
	}
	if len(callers) != len(wiring) {
		t.Errorf("the skew probe fired for %d of %d verbs (%v); every verb goes through app.call, "+
			"so one that skips it is reaching the transport another way", len(callers), len(wiring), callers)
	}
}
