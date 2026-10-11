package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ROUTE DRIFT GATE — forked from the project muster is being extracted from,
// where it is the migration's only mechanical proof that no route was dropped
// in the carve.
//
// WHAT THIS IS: muxRecorder is the second implementer of Mux. Handing it to
// RegisterRoutes — the same function a real server calls, and the only
// registration site in the module — records exactly what production
// registers. That sorted set is diffed against a CHECKED-IN golden file, so
// agreement is a real assertion rather than a tautology: the golden is not
// generated from the same source the recorder walks. A route added, removed,
// re-pathed or re-verbed cannot land without a human eyeballing a golden diff.
//
// TO REGENERATE after an intentional route change:
//
//	UPDATE_ROUTES_GOLDEN=1 go test ./internal/api -run TestRoutesMatchGolden
//
// then READ the diff in `git diff` before committing it.
//
// ---------------------------------------------------------------------------
// 🔴 KNOWN LIMIT, CARRIED FORWARD VERBATIM IN SUBSTANCE: THIS GATE DOES NOT
// SEE AUTH.
//
// The recorder's HandleFunc receives an ALREADY-WRAPPED
// func(http.ResponseWriter, *http.Request). By the time a handler reaches the
// mux, a wrapper that demands a session and a wrapper that demands nothing are
// the same type and are indistinguishable. So this gate catches ROUTE drift
// but NOT a route silently moving between authorisation tiers.
//
// 🔴 IN muster THAT LIMIT IS WORSE THAN IT WAS UPSTREAM, AND THIS IS THE
// SENTENCE TO READ TWICE. Upstream that blindness covered three wrappers on
// one process. The extraction plan's auth section counts FIVE doors already
// live upstream — a machine bearer token that is enforce-when-set (unset means
// OPEN), a browser session, a terminal token, an agent-resolved token and a
// browser-terminal tier — and specifies a SIXTH, fail-closed service-to-service
// door for exactly this split. Six tiers, one of which fails OPEN when its
// secret is missing, and a gate that cannot tell any of them apart. The plan
// names getting a route's tier wrong as one of the two most likely ways the
// carve ships broken.
//
// So: a clean diff from this gate is evidence about PATHS ONLY. It is not
// evidence that a moved route kept its tier, and it must never be quoted as
// such in a carve review. Closing that needs authorisation captured at
// registration time — a register-with-tier shim, or a declarative route table
// — and is deliberately out of scope here, as it was upstream.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 🔴 THE PRE-TRAFFIC DECISION, STATED RATHER THAN MADE SILENTLY.
//
// muster has no HTTP server and no routes yet. A gate forked into that state
// has two obvious shapes and BOTH are known failures:
//
//   - skip, or compare an empty golden against an empty recording and pass.
//     That is a green produced by there being nothing to check. It appears in
//     the run beside the real tests with the same tick and reads as coverage
//     of exactly the layer that has none.
//   - assert the routes that Phase 2 will eventually register. That is red
//     from the first commit until the carve finishes, and a permanently-red
//     gate is worse than no gate: it trains everyone to click through, and
//     then the one real failure is clicked through too.
//
// The shape chosen instead is a declared PHASE (`routePhase`, below) with a
// different load-bearing assertion in each:
//
//	pre-traffic  the golden is empty, the recording is empty, AND the module's
//	             own sources contain no literal route pattern anywhere. The
//	             third clause is what removes the vacuity: the empty set is
//	             checked against the CODE, not merely against another empty
//	             set. Add a route anywhere without wiring it through
//	             RegisterRoutes and flipping the phase, and this fails.
//	carving      the real set-equality gate is live. The floor is NOT enforced,
//	             because during an incremental carve any floor above the
//	             current count is a permanently-red gate — see routeFloor.
//	carved       the floor is armed too.
//
// THE TRADE-OFF THIS BUYS AND WHAT IT COSTS: the phase is a hand-maintained
// declaration, so it can be wrong. It is one constant, in this file, and two
// of the three transitions are forced by a test failure rather than by
// memory — a route appearing under `pre-traffic` fails; reaching `carved`
// fails the manifest test until the manifest is deleted. Only the
// `carving` -> `carved` flip is unforced, and the partition manifest's
// delete-by condition names it explicitly as part of the same pull request.
// ---------------------------------------------------------------------------

// routePhase declares where in the extraction this repository currently is.
// See the banner above for what each value asserts.
type routeGatePhase string

const (
	phasePreTraffic routeGatePhase = "pre-traffic"
	phaseCarving    routeGatePhase = "carving"
	phaseCarved     routeGatePhase = "carved"
)

// routePhase is the declaration itself. Flip it when routes land; see
// RegisterRoutes' doc comment for the three things that move together.
const routePhase = phaseCarving

// goldenPath is the checked-in expected route set.
const goldenPath = "testdata/routes.golden"

// manifestPath is the migration-time partition manifest: which side of the
// extraction each upstream route lands on. It is meant to be DELETED when the
// carve is reconciled — its header states the delete-by condition, and
// TestRoutePartitionManifestIsATotalPartition fails once that condition is
// declared met, so the file cannot quietly outlive its purpose.
const manifestPath = "testdata/routes.partition.tsv"

// ---------------------------------------------------------------------------
// routeFloor is the positive control: once routes exist, the recorder must
// observe at least this many registrations or it is wired to nothing and every
// comparison in this file passes vacuously against an empty golden.
//
// 🔴 PROVENANCE, AND — MORE IMPORTANTLY — WHAT IT IS NOT.
//
// This number is DERIVED FROM THE PARTITION MANIFEST. It is NOT measured from
// a run, because there is no run to measure: muster registers nothing yet. The
// upstream project's equivalent constant was a hardcoded 100 whose own comment
// asserted the real count was 106 — while the golden beside it held 162. A
// floor with no live provenance drifts into decoration, and nobody notices,
// because a floor that is far too low passes exactly like one that is right.
//
// The working, in full, so it can be checked rather than believed:
//
//	  82   rows the manifest assigns to `muster`
//	+ 14   rows it assigns to `shared` — the shell/infra surface BOTH projects
//	       register, duplicated rather than moved, so muster registers them too
//	----
//	  96   projected post-carve registrations   (= routeFloorProjection at the carve; 81 now, see below)
//	- 16   carve margin, ~17%: routes the carve is expected to drop or fold
//	       rather than move (the manifest's own header flags a five-row block
//	       it is least certain about, and two more rows whose fate the source
//	       plan contradicts itself on)
//	----
//	  80   routeFloor
//
// Derived 2026-09 from a 162-route upstream golden. The month, not the day, is
// deliberate: this repository's leak gate refuses a dated observation claim,
// and that gate is not to be edited to accommodate a comment.
//
// 🔴 THE PROJECTION HAS BEEN REPLACED BY A MEASUREMENT, AND THE ARITHMETIC
// ABOVE IS KEPT ONLY AS THE PROVENANCE OF THE NUMBER IT REPLACED.
//
// MEASURED, 2026-09, against the carved tree: RegisterRoutes registers
// **99 routes**, counted from the regenerated testdata/routes.golden with the
// same filter this file's reader applies. The floor is that count less a ~10%
// margin:
//
//	  99   measured registrations after the API carve
//	-  9   margin for routes a later chunk folds or drops
//	----
//	  90   routeFloor
//
// 🔴 99, NOT THE 96 THE MANIFEST PROJECTED, AND THE THREE-ROUTE DIFFERENCE WAS
// ACCOUNTED FOR RATHER THAN ABSORBED: `GET /api/directories` (the task-create
// picker, rebuilt across the seam) plus `GET /runbooks` and `GET /privileges`
// (two document routes the nav linked to). Every one of the manifest's 96
// muster-and-shared rows WAS registered, and no `router` row was — the
// migration's mechanical proof that nothing was dropped.
//
// ⚠ RE-DERIVED WHEN PRIVILEGES AND RUNBOOKS WERE REMOVED (PR #49). That removal
// deleted 17 routes: the 15 the manifest listed (now deleted from it, so the
// projection is 81) and the two document routes above. MEASURED after it:
// RegisterRoutes registers **106** routes — connectors and the approval relay
// had grown the table since the carve. The floor cannot follow the measurement
// up, because TestRoutePartitionManifestIsATotalPartition requires it to sit
// BELOW the manifest projection; it is the projection less the same ~10%
// margin:
//
//	  81   manifest projection (67 muster + 14 shared)
//	-  9   margin
//	----
//	  72   routeFloor
//
// routeFloorProjection is checked against the MANIFEST, not against the code,
// by TestRoutePartitionManifestIsATotalPartition.
//
// Do not "fix" a red floor by lowering it to whatever number makes the run
// green: that is the failure this whole comment exists to prevent, and it has
// already happened once upstream.
//
// It is enforced ONLY in phase `carved`. Enforcing it during `carving` would
// red the gate from the first route to the eightieth.
// ---------------------------------------------------------------------------
const (
	routeFloorProjection = 81
	routeFloor           = 72
)

// muxRecorder implements Mux and records patterns instead of routing.
type muxRecorder struct{ patterns []string }

func (m *muxRecorder) HandleFunc(pat string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pat)
}

func (m *muxRecorder) Handle(pat string, _ http.Handler) {
	m.patterns = append(m.patterns, pat)
}

// Compile-time proof the recorder and the real mux are interchangeable — this
// is what makes the recording a claim about production and not about the test.
// The second assertion lives in routes.go, beside the interface.
var _ Mux = (*muxRecorder)(nil)

// recordRoutes runs the real registration path against a recorder and returns
// the sorted patterns.
//
// ⚠ IT TAKES NO FIXTURE, AND THAT WILL CHANGE. Upstream this function builds a
// server with EVERY optional capability enabled, because registration there is
// conditional — a nil dependency makes a whole block of routes return early,
// and a fixture missing one field makes the golden a record of the FIXTURE
// rather than of the code. That is not a hypothetical: it cost that project a
// golden seven routes short, with two separate guards green over it.
//
// 🔴 PHASE 2 OBLIGATION, NAMED HERE SO IT IS NOT REDISCOVERED THE HARD WAY:
// the moment muster's RegisterRoutes gains its first `if dep == nil { return }`,
// this function needs a fixture with every dependency supplied, AND the two
// upstream guards that hold that down must be ported with it —
//
//	one asserting every conditional branch IS reached (a sentinel route per
//	guard), and
//	one asserting the sentinel table is COMPLETE, deriving the guard set from
//	the source rather than hardcoding it.
//
// The split is the whole lesson: the first is a claim about the rows that
// exist, only the second is a claim about the guards that exist, and it was
// the missing second one that let the seven-route hole through. They are NOT
// forked here because muster has no conditional registration to guard yet, and
// a guard over an empty relationship is coverage prose.
func recordRoutes(t *testing.T) []string {
	t.Helper()

	rec := &muxRecorder{}
	RegisterRoutes(rec)

	got := append([]string(nil), rec.patterns...)
	sort.Strings(got)
	return got
}

// diffRoutes is THE comparison the gate makes. Every control below calls this
// same function, so a control proves something about the real gate rather than
// about a parallel implementation that happens to look similar.
//
// It is a MULTISET diff, not a positional one: a route registered twice is a
// real defect (the second registration panics on a real ServeMux) and shows up
// here as a surplus rather than being masked by both sides sorting the same.
// Empty string = the sets are equal.
func diffRoutes(want, got []string) string {
	count := func(rs []string) map[string]int {
		m := make(map[string]int, len(rs))
		for _, r := range rs {
			m[r]++
		}
		return m
	}
	w, g := count(want), count(got)

	seen := make(map[string]bool, len(w)+len(g))
	var all []string
	for _, m := range []map[string]int{w, g} {
		for r := range m {
			if !seen[r] {
				seen[r] = true
				all = append(all, r)
			}
		}
	}
	sort.Strings(all)

	var lines []string
	for _, r := range all {
		for i := 0; i < w[r]-g[r]; i++ {
			lines = append(lines, "- "+r) // in the golden, not registered
		}
		for i := 0; i < g[r]-w[r]; i++ {
			lines = append(lines, "+ "+r) // registered, not in the golden
		}
	}
	return strings.Join(lines, "\n")
}

// readGolden parses the checked-in golden, applying the same filter the
// regenerator's header describes: comment lines and blank lines are not
// routes.
func readGolden(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with UPDATE_ROUTES_GOLDEN=1)", goldenPath, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// goldenHeader returns the comment block at the top of the golden, so
// regeneration preserves it instead of silently discarding the banner that
// tells a reader this file does not record auth.
func goldenHeader(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		break
	}
	return strings.Join(out, "\n")
}

// TestRoutesMatchGolden is the gate: set equality — grow OR shrink both fail —
// between what RegisterRoutes actually registers and the checked-in golden.
//
// ⚠ IN PHASE `pre-traffic` THIS COMPARISON IS BETWEEN TWO EMPTY SETS AND
// THEREFORE PROVES NOTHING ON ITS OWN. Its non-vacuity today comes entirely
// from TestRouteGatePositiveControl, which checks the emptiness against the
// module's sources. Read them as one gate, not two.
func TestRoutesMatchGolden(t *testing.T) {
	got := recordRoutes(t)

	if os.Getenv("UPDATE_ROUTES_GOLDEN") != "" {
		header := goldenHeader(t)
		if header == "" {
			t.Fatalf("%s has no comment header — regeneration would drop the banner "+
				"explaining that this file does not record auth wrappers. Restore the header first.", goldenPath)
		}
		body := header + "\n"
		if len(got) > 0 {
			body += strings.Join(got, "\n") + "\n"
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(body), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s with %d routes — REVIEW THE DIFF before committing", goldenPath, len(got))
		return
	}

	if d := diffRoutes(readGolden(t), got); d != "" {
		t.Fatalf("route set drifted from %s (- golden, + recorded):\n%s\n\n"+
			"If the change is intentional: eyeball the diff, then regenerate with\n"+
			"  UPDATE_ROUTES_GOLDEN=1 go test ./internal/api -run TestRoutesMatchGolden",
			goldenPath, d)
	}
}

// TestRouteGatePositiveControl is the "can it ever observe anything" control,
// and in phase `pre-traffic` it is the only thing making this file's green
// mean anything at all.
//
// It reports the PAIR (observed, expected-bound) rather than a bare count,
// because a reassuring zero from a recorder wired to nothing is
// indistinguishable from an honest zero — that is the whole reason the
// source scan exists.
func TestRouteGatePositiveControl(t *testing.T) {
	got := recordRoutes(t)
	golden := readGolden(t)
	inSource := scanLiteralRoutePatterns(t, moduleRoot(t))

	switch routePhase {
	case phasePreTraffic:
		t.Logf("phase %q: %d routes recorded, %d in the golden, %d literal route patterns in the module's sources",
			routePhase, len(got), len(golden), len(inSource))

		if len(got) != 0 {
			t.Errorf("phase is %q but RegisterRoutes registered %d route(s): %v\n\n"+
				"Routes have landed. Flip routePhase to %q, regenerate the golden with "+
				"UPDATE_ROUTES_GOLDEN=1, and read the diff.", routePhase, len(got), got, phaseCarving)
		}
		if len(golden) != 0 {
			t.Errorf("phase is %q but %s holds %d route(s). One of the two is wrong; "+
				"the golden is regenerated from the code, so start there.", routePhase, goldenPath, len(golden))
		}
		// 🔴 THE CLAUSE THAT MAKES THE EMPTY SET AN ASSERTION RATHER THAN AN
		// ACCIDENT. Without it, "the recorder is wired to nothing" and "there
		// is genuinely nothing to record" produce the identical observable.
		if len(inSource) != 0 {
			var lines []string
			for pat, pos := range inSource {
				lines = append(lines, fmt.Sprintf("  %q at %s", pat, pos))
			}
			sort.Strings(lines)
			t.Errorf("phase is %q — meaning this gate believes muster registers no routes — but the "+
				"module's own sources register %d literal route pattern(s):\n%s\n\n"+
				"Either wire them through RegisterRoutes and flip routePhase to %q, or they are a "+
				"registration this gate cannot see, which is the exact hole this control exists to close.",
				routePhase, len(inSource), strings.Join(lines, "\n"), phaseCarving)
		}

	case phaseCarving:
		t.Logf("phase %q: %d routes recorded, projection is %d (routeFloor %d, NOT enforced in this phase)",
			routePhase, len(got), routeFloorProjection, routeFloor)
		if len(got) == 0 {
			t.Fatalf("phase is %q but the recorder observed 0 routes — it is wired to nothing. "+
				"If the carve has genuinely not started, routePhase should still be %q.", routePhase, phasePreTraffic)
		}
		assertNoRouteOutsideTheChokepoint(t, got, inSource)

	case phaseCarved:
		t.Logf("phase %q: %d routes recorded, floor %d (projection was %d)",
			routePhase, len(got), routeFloor, routeFloorProjection)
		if len(got) < routeFloor {
			t.Fatalf("recorder observed %d routes, floor is %d — the recorder is wired to nothing, or a "+
				"registration block was dropped in the carve.\n\n"+
				"🔴 DO NOT LOWER THE FLOOR TO MAKE THIS PASS unless you have re-derived it from a measured "+
				"run and written that measurement beside the constant. See the routeFloor banner.",
				len(got), routeFloor)
		}
		assertNoRouteOutsideTheChokepoint(t, got, inSource)

	default:
		t.Fatalf("routePhase is %q, which is not one of %q/%q/%q — a phase this file does not "+
			"understand silently disables every assertion keyed on it",
			routePhase, phasePreTraffic, phaseCarving, phaseCarved)
	}
}

// assertNoRouteOutsideTheChokepoint pins the premise the whole gate rests on:
// RegisterRoutes is the ONLY registration site. Every literal route pattern
// the module's sources hand to Handle/HandleFunc must show up in the recording.
//
// It is CONTAINMENT, deliberately, not equality — a pattern built at runtime
// (a loop, a constant joined to a prefix) is registered but not a literal, and
// demanding equality would red on every such route while proving nothing
// extra. The direction that matters is the one checked: a literal in the
// source that the recorder never saw is a registration happening somewhere
// this gate cannot see.
func assertNoRouteOutsideTheChokepoint(t *testing.T, recorded []string, inSource map[string]string) {
	t.Helper()
	have := make(map[string]bool, len(recorded))
	for _, r := range recorded {
		have[r] = true
	}
	var orphans []string
	for pat, pos := range inSource {
		if !have[pat] {
			orphans = append(orphans, fmt.Sprintf("  %q at %s", pat, pos))
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("%d literal route pattern(s) exist in the module's sources but were NOT recorded by "+
			"RegisterRoutes:\n%s\n\nThe golden describes a SUBSET of what this server serves, and says "+
			"nothing about the rest. Route them through RegisterRoutes.", len(orphans), strings.Join(orphans, "\n"))
	}
	t.Logf("chokepoint holds: %d literal route pattern(s) in source, all present in the %d recorded",
		len(inSource), len(recorded))
}

// ---------------------------------------------------------------------------
// The negative controls. Both run against a synthetic fixture ALWAYS, and
// additionally against the real recorded set once one exists — a table rather
// than an either/or, so neither case quietly stops running.
//
// 🔴 WHY A FIXTURE AT ALL. Upstream both controls seed themselves from the
// live recording. Here the live recording is empty, and you cannot remove a
// route from an empty set — the shrink control would have no precondition to
// meet and would either skip or fail. Skipping it is the outcome to avoid
// above all others: the shrink half is what makes this gate SET EQUALITY
// rather than CONTAINMENT, and therefore the only thing that would catch a
// route silently disappearing in the carve. A containment-only gate is the
// precise failure this test exists to prevent, so the shrink control runs from
// day one on data it is guaranteed to have.
// ---------------------------------------------------------------------------

// controlFixtureRoutes is the synthetic base set for the controls.
//
// The three entries are pairwise distinct in verb AND path, and share no
// substring with the probe or with any real route, so a diff naming one of
// them cannot be a coincidence of matching text.
var controlFixtureRoutes = []string{
	"DELETE /control/gamma/{id}",
	"GET /control/alpha",
	"POST /control/beta",
}

// controlBase is one base set a negative control runs against.
type controlBase struct {
	name string
	set  []string
	// drop is the route the shrink control removes. Empty means "the first
	// one", which is all that can be said about a set whose contents are not
	// known until Phase 2.
	drop string
}

func controlBases(t *testing.T) []controlBase {
	t.Helper()
	bases := []controlBase{{name: "fixture", set: controlFixtureRoutes, drop: "GET /control/alpha"}}
	if got := recordRoutes(t); len(got) > 0 {
		bases = append(bases, controlBase{name: "recorded", set: got})
	} else {
		t.Logf("no routes recorded yet (phase %q) — the controls run against the fixture only; "+
			"they will additionally run against the real set the moment one exists, with no edit here",
			routePhase)
	}
	return bases
}

// TestRouteGateNegativeControlGrow is the "can it go red" control for an ADDED
// route: seed a copy of the base set with a pattern absent from it, run the
// SAME diffRoutes the real gate uses, and require the diff to NAME that route.
// Asserting merely "non-empty" would let an unrelated failure pass this
// control.
func TestRouteGateNegativeControlGrow(t *testing.T) {
	const probe = "PUT /control/__grow_probe"

	for _, base := range controlBases(t) {
		t.Run(base.name, func(t *testing.T) {
			for _, r := range base.set {
				if r == probe {
					t.Fatalf("control precondition broken: %q is already in the %s base set", probe, base.name)
				}
			}

			grown := append([]string{probe}, base.set...)
			sort.Strings(grown)

			d := diffRoutes(base.set, grown)
			if d == "" {
				t.Fatalf("negative control FAILED: diffRoutes saw no difference after adding %q to the %s "+
					"base set — the gate cannot go red", probe, base.name)
			}
			if !strings.Contains(d, probe) {
				t.Fatalf("negative control FAILED FOR THE WRONG REASON: the diff is non-empty but does not "+
					"name %q:\n%s", probe, d)
			}
			t.Logf("grow control OK against the %s base (%d routes): diff names the added route:\n%s",
				base.name, len(base.set), d)
		})
	}
}

// TestRouteGateNegativeControlShrink is the symmetric control for a REMOVED
// route. This is what proves the gate is set equality, not containment — and
// therefore the half that would catch a route vanishing in the carve.
//
// 🔴 IT RUNS BOTH ORIENTATIONS, AND THE SECOND ONE IS THE POINT. Upstream this
// control drops a route from the GOLDEN side only. Because the dropped route
// then shows up as a SURPLUS in the other operand, that exercises the same
// branch of the comparison the grow control already exercises — so a diff that
// could report a surplus and nothing else would pass both controls while being
// blind to the one direction the carve actually threatens: a route the golden
// still expects that is no longer registered.
//
//	golden-lost-a-route   the golden was edited, the code was not
//	code-lost-a-route     the code dropped a route the golden still expects.
//	                      This is the carve-drop direction, and it reaches the
//	                      deficit branch nothing else here reaches.
func TestRouteGateNegativeControlShrink(t *testing.T) {
	orientations := []struct {
		name string
		args func(full, shrunk []string) (want, got []string)
	}{
		{"golden-lost-a-route", func(full, shrunk []string) ([]string, []string) { return shrunk, full }},
		{"code-lost-a-route", func(full, shrunk []string) ([]string, []string) { return full, shrunk }},
	}

	for _, base := range controlBases(t) {
		for _, o := range orientations {
			t.Run(base.name+"/"+o.name, func(t *testing.T) {
				if len(base.set) == 0 {
					t.Fatalf("%s base set is empty; cannot run the shrink control", base.name)
				}
				dropped := base.drop
				if dropped == "" {
					dropped = base.set[0]
				}

				shrunk := make([]string, 0, len(base.set))
				found := false
				for _, r := range base.set {
					if r == dropped {
						found = true
						continue
					}
					shrunk = append(shrunk, r)
				}
				if !found {
					t.Fatalf("control precondition broken: %q is not in the %s base set", dropped, base.name)
				}

				want, got := o.args(base.set, shrunk)
				d := diffRoutes(want, got)
				if d == "" {
					t.Fatalf("shrink control FAILED (%s): diffRoutes saw no difference after removing %q "+
						"from the %s base set — the gate is containment, not set equality",
						o.name, dropped, base.name)
				}
				if !strings.Contains(d, dropped) {
					t.Fatalf("shrink control FAILED FOR THE WRONG REASON (%s): the diff is non-empty but "+
						"does not name %q:\n%s", o.name, dropped, d)
				}
				t.Logf("shrink control OK (%s) against the %s base (%d routes): diff names the removed route:\n%s",
					o.name, base.name, len(base.set), d)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// The source scan, and its own controls.
//
// 🔴 A SCANNER'S ZERO IS A CLAIM ABOUT THE SCANNER. In phase `pre-traffic` the
// whole non-vacuity argument rests on this thing returning 0 — which is
// exactly what it would return if it parsed no files, or matched nothing it
// should match. So it reports how many files it parsed and fails outright on
// zero, and TestRoutePatternScannerCanSeeALiteralRoute feeds it a directory
// that MUST produce a non-zero count and watches the number move.
// ---------------------------------------------------------------------------

// moduleRoot walks up from the working directory to the directory holding
// go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.mod — the scan would cover nothing")
		}
		dir = parent
	}
}

// otherServerRoutes ledgers the binaries in this module that serve their OWN HTTP
// surface — not muster-server's, so not RegisterRoutes' — keyed by directory, with
// the exact route set each one registers.
//
// 🔴 IT IS AN EXEMPTION WITH A LEDGER, NOT A HOLE. The chokepoint scan skips these
// directories, and TestOtherServersRouteLedger re-scans each one and asserts SET
// EQUALITY against the list here, so a route added to (or dropped from) one of
// these servers still fails a test — this one, instead of the chokepoint's.
//
//   - cmd/ccd: the in-pod supervisor of the claude-code-agent image. Its
//     /v1/responses is muster's agent wire served FROM the pod, consumed by
//     internal/agentgateway; /hook/{event} is served only on a loopback listener;
//     GET /{$} is the same health check as /healthz, at muster's default agent
//     health path ("/").
var otherServerRoutes = map[string][]string{
	"cmd/ccd": {"GET /healthz", "GET /{$}", "POST /hook/{event}", "POST /v1/responses"},
}

// TestOtherServersRouteLedger is the other half of the exemption above.
func TestOtherServersRouteLedger(t *testing.T) {
	root := moduleRoot(t)
	for dir, want := range otherServerRoutes {
		got := scanLiteralRoutePatterns(t, filepath.Join(root, filepath.FromSlash(dir)))
		var have []string
		for pat := range got {
			have = append(have, pat)
		}
		sort.Strings(have)
		w := append([]string(nil), want...)
		sort.Strings(w)
		if strings.Join(have, "\n") != strings.Join(w, "\n") {
			t.Errorf("%s registers %q, but otherServerRoutes ledgers %q — update the ledger (and review "+
				"why the route set changed)", dir, have, w)
		}
	}
}

// scanLiteralRoutePatterns parses every non-test .go file under root and
// returns each literal string handed as the first argument to a .Handle or
// .HandleFunc call, mapped to its source position.
//
// ⚠ DECLARED LIMITS, so a zero from this is read for what it is. It sees only
// STRING LITERALS — a pattern assembled at runtime is invisible to it. It
// keys on the method NAME, so an unrelated method called Handle on some other
// type would be a false positive; that is the safe direction (a false positive
// is a loud failure someone reads, a false negative is a silent hole).
func scanLiteralRoutePatterns(t *testing.T, root string) map[string]string {
	t.Helper()

	out := make(map[string]string)
	fset := token.NewFileSet()
	parsed := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "testdata", "vendor", "node_modules":
				return filepath.SkipDir
			}
			if path != root {
				if rel, rerr := filepath.Rel(root, path); rerr == nil {
					if _, other := otherServerRoutes[filepath.ToSlash(rel)]; other {
						return filepath.SkipDir // held by TestOtherServersRouteLedger instead
					}
				}
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pat, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			out[pat] = fset.Position(lit.Pos()).String()
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if parsed == 0 {
		t.Fatalf("the route-pattern scan parsed 0 non-test .go files under %s — it is wired to nothing, "+
			"which returns the same zero as a module that genuinely registers no routes", root)
	}
	t.Logf("route-pattern scan: %d non-test .go files parsed under %s, %d literal pattern(s) found",
		parsed, root, len(out))
	return out
}

// TestRoutePatternScannerCanSeeALiteralRoute is the positive control on the
// instrument. A scanner that can never match anything reports the same zero as
// a clean module; this feeds it a case that MUST produce a non-zero count and
// checks that the number moves, and a case that must NOT match so the scanner
// is not simply matching everything.
func TestRoutePatternScannerCanSeeALiteralRoute(t *testing.T) {
	dir := t.TempDir()

	src := `package probe

import "net/http"

func wire(mux *http.ServeMux) {
	mux.HandleFunc("GET /scanner/control/visible", nil)
	mux.Handle("POST /scanner/control/also-visible", nil)
	// Not a literal: assembled at runtime. A DECLARED limit, asserted here so
	// it stays declared rather than becoming a surprise.
	prefix := "GET /scanner/control/"
	mux.HandleFunc(prefix+"assembled", nil)
	// Not a registration at all.
	_ = mux.Handler
}
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	// A _test.go file must be ignored, or the scan would police test fixtures.
	if err := os.WriteFile(filepath.Join(dir, "probe_test.go"),
		[]byte("package probe\n\nfunc init() { }\n"), 0o644); err != nil {
		t.Fatalf("write probe test source: %v", err)
	}

	got := scanLiteralRoutePatterns(t, dir)

	want := []string{"GET /scanner/control/visible", "POST /scanner/control/also-visible"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("positive control FAILED: the scanner did not find %q — its zero on the real module "+
				"is therefore not evidence of anything. Found: %v", w, keysOf(got))
		}
	}
	if _, ok := got["GET /scanner/control/assembled"]; ok {
		t.Errorf("the scanner reported a pattern it cannot actually see (%q was assembled at runtime) — "+
			"the declared limit above is wrong", "GET /scanner/control/assembled")
	}
	if len(got) != len(want) {
		t.Errorf("scanner found %d pattern(s), expected exactly %d: %v", len(got), len(want), keysOf(got))
	}
	t.Logf("scanner positive control OK: %d on a source that must match, against 0 on the real module",
		len(got))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The partition manifest.
//
// The manifest is the only machine-readable record of which side of the
// extraction each upstream route was meant to land on; the plan document it
// came from states bucket totals and never enumerates them. This test is what
// keeps it a record rather than a document: a total partition, sorted, with
// declared counts that must match the rows.
//
// 🔴 IT IS ALSO THE ARTIFACT'S OWN EXPIRY. In phase `carved` — the phase whose
// arrival means the carve is reconciled — this test FAILS and says to delete
// both the manifest and itself. A migration artifact with no forced end is how
// a file nobody can close gets created.
// ---------------------------------------------------------------------------

// manifestDestinations is the closed set of destination values.
var manifestDestinations = map[string]bool{"muster": true, "router": true, "shared": true}

type manifestRow struct {
	destination string
	registrar   string
	route       string
	line        int
}

func readManifest(t *testing.T) ([]manifestRow, map[string]int) {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manifestPath, err)
	}

	var rows []manifestRow
	declared := map[string]int{}
	for n, line := range strings.Split(string(b), "\n") {
		n++
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// `# counts: muster=82 router=66 shared=14 total=162`
			if idx := strings.Index(trimmed, "counts:"); idx >= 0 {
				for _, kv := range strings.Fields(trimmed[idx+len("counts:"):]) {
					k, v, ok := strings.Cut(kv, "=")
					if !ok {
						t.Fatalf("%s:%d: counts field %q is not key=value", manifestPath, n, kv)
					}
					i, cerr := strconv.Atoi(v)
					if cerr != nil {
						t.Fatalf("%s:%d: counts field %q has a non-numeric value", manifestPath, n, kv)
					}
					declared[k] = i
				}
			}
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("%s:%d: expected exactly 3 tab-separated fields, got %d: %q",
				manifestPath, n, len(fields), line)
		}
		rows = append(rows, manifestRow{
			destination: strings.TrimSpace(fields[0]),
			registrar:   strings.TrimSpace(fields[1]),
			route:       strings.TrimSpace(fields[2]),
			line:        n,
		})
	}
	return rows, declared
}

func TestRoutePartitionManifestIsATotalPartition(t *testing.T) {
	if routePhase == phaseCarved {
		t.Fatalf("routePhase is %q, which is the manifest's declared delete-by condition: the carve is "+
			"reconciled, so %s has no remaining reader. Delete it and delete this test, in one pull "+
			"request, as its header says.", routePhase, manifestPath)
	}

	rows, declared := readManifest(t)

	// Instrument control: a parse that found nothing satisfies every check
	// below trivially.
	if len(rows) == 0 {
		t.Fatalf("parsed 0 rows from %s — the parser is wired to nothing, not reading an empty partition",
			manifestPath)
	}

	counts := map[string]int{}
	seen := make(map[string]int, len(rows))
	var prev string
	for _, r := range rows {
		if !manifestDestinations[r.destination] {
			t.Errorf("%s:%d: destination %q is not one of muster/router/shared — an unknown destination "+
				"is a row the step-17 reconciliation cannot check", manifestPath, r.line, r.destination)
			continue
		}
		if r.registrar == "" {
			t.Errorf("%s:%d: empty upstream-registrar for %q", manifestPath, r.line, r.route)
		}
		verb, path, ok := strings.Cut(r.route, " ")
		if !ok || verb == "" || !strings.HasPrefix(path, "/") {
			t.Errorf("%s:%d: %q is not a `METHOD /path` pattern", manifestPath, r.line, r.route)
		}
		if first, dup := seen[r.route]; dup {
			t.Errorf("%s:%d: route %q is already on line %d — a route in two buckets is not a partition",
				manifestPath, r.line, r.route, first)
		}
		seen[r.route] = r.line
		if prev != "" && r.route < prev {
			t.Errorf("%s:%d: %q sorts before %q on the previous row — the file must be sorted by route "+
				"so a diff against a golden is readable", manifestPath, r.line, r.route, prev)
		}
		prev = r.route
		counts[r.destination]++
	}

	total := len(rows)
	for _, k := range []string{"muster", "router", "shared"} {
		if declared[k] != counts[k] {
			t.Errorf("header declares %s=%d, rows give %d — a header count is a claim about the rows, "+
				"and this one is wrong", k, declared[k], counts[k])
		}
	}
	if declared["total"] != total {
		t.Errorf("header declares total=%d, rows give %d", declared["total"], total)
	}
	if counts["muster"]+counts["router"]+counts["shared"] != total {
		t.Errorf("the three buckets sum to %d but there are %d rows — some row has a destination this "+
			"test skipped", counts["muster"]+counts["router"]+counts["shared"], total)
	}

	// The projection routeFloor is derived from. Pinned here rather than in a
	// comment, so the constant and the manifest cannot drift apart silently —
	// which is exactly how upstream's floor comment came to assert 106 beside
	// a 162-route golden.
	if projected := counts["muster"] + counts["shared"]; projected != routeFloorProjection {
		t.Errorf("routeFloorProjection is %d but the manifest projects %d muster-side registrations "+
			"(%d muster + %d shared). routeFloor (%d) is DERIVED FROM THAT NUMBER — re-derive both, and "+
			"write the new working beside the constant.",
			routeFloorProjection, projected, counts["muster"], counts["shared"], routeFloor)
	}
	if routeFloor >= routeFloorProjection {
		t.Errorf("routeFloor (%d) is not below routeFloorProjection (%d) — a floor at or above the "+
			"expected count reds on a legitimate carve", routeFloor, routeFloorProjection)
	}

	t.Logf("partition holds: %d rows — muster=%d router=%d shared=%d; muster-side projection %d, floor %d",
		total, counts["muster"], counts["router"], counts["shared"],
		counts["muster"]+counts["shared"], routeFloor)
}
