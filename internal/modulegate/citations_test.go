package modulegate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A COMMENT THAT NAMES A TEST IS MAKING A CHECKABLE CLAIM.
//
// 🔴 THE MEASUREMENT THAT PUT THIS FILE HERE: 104 distinct `Test*` identifiers
// were cited in this module's NON-TEST sources, in 127 places, all of them in
// comments. 42 resolved to a test that exists. SIXTY-TWO DID NOT. Two of those
// were refusal claims a maintainer would act on — "TestChiefThreadSearchIsOperatorOnly
// asserts both of them" beside a route that returns chat message excerpts, and
// "TestEveryStatusWriterIsLabelled … not the compiler" beside a hazard the same
// comment says was measured to compile clean. Both tests now exist. The rest are
// the subject of the ledger below.
//
// 🔴 WHY THIS IS WORSE THAN AN UNDOCUMENTED INVARIANT, AND NOT MERELY UNTIDY. A
// comment naming a guard tells the next reader the question is already settled.
// It is the one form of documentation that actively stops the check being made —
// so a citation to a test that does not exist does not leave coverage where it
// was, it removes the prompt that would have created it.
//
// 🔴 WHAT THIS GUARD WOULD STILL ACCEPT. It resolves a NAME. It cannot tell
// whether the test that answers to that name asserts anything like what the
// comment claims — a citation pointing at a real but unrelated test passes here.
// That is a limit of any mechanical check on prose; the mitigation is that a
// name has to be written by somebody, and the ledger below makes every
// unresolved one visible to review rather than invisible to everyone.
//
// 🔴 KNOWN LIMIT, MEASURED RATHER THAN SUPPOSED: THIS GATE SEES ONLY
// `Test[A-Z]…` NAMES, AND A CITATION SPELLED ANY OTHER WAY WALKS STRAIGHT PAST
// IT. testIdent (below) requires a capital after `Test`, which is what keeps
// `Tests` and `Testing` out — and also what makes every lower-case guard name
// invisible. The worked example, found by the round-1 audit of the very arc
// that built this gate: cmd/muster-server/config.go carried "envNamesAreDocumented
// asserts these constants against the documentation", and an enumerating grep
// over the module returned exactly ONE occurrence of that identifier — the
// sentence itself. A fabricated citation, written in the same arc as the gate
// that exists to prevent fabricated citations, merging green.
//
// ⚠ THE REMEDY IS A CONVENTION, NOT A WIDER REGEX, AND THAT IS A DELIBERATE
// CHOICE RATHER THAN AN OVERSIGHT. Widening this to "any identifier cited in a
// comment" would have to resolve every symbol any comment in the module names —
// stdlib types, other packages' functions, camelCase English — and the
// false-positive rate would make it a permanently-red gate, which this project's
// own notes call worse than no gate because it trains everyone to click through.
// So: CITE A GUARD BY ITS `Test*` NAME, which is the spelling this gate can
// check. config.go now cites TestEveryServerEnvNameIsSpelledOnceAndRead, which
// exists and is resolved below.
//
// 🔴 THIS PARAGRAPH IS A RECORDED GAP, NOT COVERAGE. Nothing mechanical enforces
// the convention it states. Read it as "here is what this gate cannot see", and
// do not quote the gate's clean verdict as evidence about a citation whose name
// does not start with `Test`.
// ---------------------------------------------------------------------------

// unportedCitations is every `Test*` name cited in a non-test source of this
// module that resolves to no test HERE.
//
// 🔴 THEY ARE NOT FABRICATIONS, AND THE DISTINCTION DECIDES WHAT TO DO ABOUT
// THEM. Every name below names a test that exists UPSTREAM, in the project this
// domain layer was carved out of. The handlers moved; their suites did not —
// internal/api/doc_seams.go entry 4 states that at length, measures this
// package's coverage at the point of the carve, and records the closing
// condition: a pull request that ports the fixture layer and then the suites.
// This map is that entry's instrumentation. Each name leaving it is one guard
// arriving.
//
// 🔴 THE TEST FAILS WHEN THIS MAP GROWS *OR* SHRINKS, AND GROWING IS THE
// IMPORTANT DIRECTION. A plain allowlist would absorb a NEWLY invented citation
// — a comment written today claiming a guard nobody has written — which is a
// different and worse thing than an unported one, because there is no upstream
// test to port. A new entry here has to be added deliberately, and a reviewer
// gets to ask which of the two it is.
//
// ⚠ IT IS KEYED BY FILE, NOT BY file:line. Line numbers move whenever anything
// above them is edited, and a ledger that reddens on an unrelated edit is a
// ledger people delete.
var unportedCitations = map[string][]string{
	"cmd/muster/task.go": {"TestTaskProvenanceHeadersMatchTheServer"},

	"internal/api/agents.go": {
		"TestAFailedThreadListReadIsNEVERRenderedAsAnEmptyHistory",
		"TestReservedAgentNamesAreNotPoolShaped",
	},
	"internal/api/auth.go": {
		"TestEveryBrowserSurfaceRequiresAHumanSession",
		"TestTheAgentSteeringWriteRefusesAnUnarmedServer",
	},
	"internal/api/ext.go": {"TestExtensionsWiringPairsNotesWithALivenessProbe"},
	"internal/api/login.go": {
		"TestLoginPageIsAccessible",
		"TestTheLoginPathConstantMatchesItsRoutes",
	},
	"internal/api/machine_agents.go": {
		"TestTheMachineAgentMessageWriteSteersIntoTheSessionTheReadReturns",
	},
	"internal/api/merge.go": {
		"TestSupersededByTagIsAlwaysAValidTag",
		"TestSupersededByTagStaysDescriptive",
		"TestTaskMergeStampsALoserAlreadyAtTheTagCap",
	},
	"internal/api/notes.go": {
		"TestAPITaskListAppliesLimitAfterStatusFilter",
		"TestAPITaskListRejectsEveryNonStatus",
		"TestPatchAddAndRemoveSameTagResolvesToAdded",
		"TestPatchProjectReassignmentNeverLeavesTwoProjectTags",
		"TestRejectedEditDoesNotRecordWorked",
		"TestRenderNoteCardCallSitesAreLedgered",
		"TestSingleTaskResponsesAllCarryTheAgent",
		"TestTaskSourceAllowlistExcludesPrivilegedIdentities",
	},
	"internal/api/notes_routes.go": {
		"TestDirectorySearchRouteIsRegisteredAtThePathTheRendererEmits",
		"TestEveryBrowserSurfaceRequiresAHumanSession",
		"TestEveryRoutePatternIsALiteralTheLedgerCanRead",
	},
	"internal/api/projects.go": {
		"TestProjectsCountCompleteTasks",
		"TestProjectsDropDismissedTasks",
	},
	"internal/api/view.go": {"TestDetailRouteAndViewSeamAgreeOnEveryIdSpelling"},

	"internal/notes/notes.go": {
		"TestPrivilegedAuthorSitesAreLedgered",
		"TestReapCommentAuthorIsUnclaimable",
	},
	"internal/notes/tags.go": {
		"TestProjectTagIsNotRouting",
		"TestProjectValueNeverReachesAPrometheusLabel",
		"TestProjectsCountCompleteTasks",
	},

	"internal/ui/agents_detail.go": {
		"TestAnUntitledSessionsPlaceholderIsPerSurfaceAndLedgered",
		"TestTheShellPlusAnOpenChiefPanelHasNoDuplicateIDs",
	},
	"internal/ui/components.go": {
		"TestEveryTabpanelDeclaresItsSwapHonestly",
		"TestEveryTasksListAjaxIsSourcedAtTheContainer",
		"TestHeaderAndContentShareOneWidth",
		"TestNavigationIsDerivedFromOneRegistry",
		"TestOffShellPagesClaimNoCurrentPage",
		"TestSidebarMarksExactlyOneCurrentPage",
		"TestSidebarTabBoostIsOwnedByThePanelOwner",
		"TestSidebarTabsCarryNoTabRoles",
		"TestTaskDirtyGuardSeesTags",
	},
	"internal/ui/merge.go": {"TestEveryTasksListAjaxIsSourcedAtTheContainer"},
	"internal/ui/notes.go": {
		"TestDirectoriesReadsRequestHistory",
		"TestNoteCardRendersStatusAndComments",
		"TestTasksSkeletonReservesTheAlwaysRenderedChipRow",
	},
	"internal/ui/task_detail.go": {
		"TestDetailPageAttachesNoInheritedRequestStampAnywhere",
		"TestNoDocumentsBodyCarriesAnythingABoostedNavCanStrand",
		"TestRouteContentWidthRelationship",
		"TestSidebarTabBoostIsOwnedByThePanelOwner",
	},
	"internal/ui/task_not_found.go": {"TestRouteContentWidthRelationship"},
}

// testIdent matches a Go test function name. The capital after `Test` is what
// keeps ordinary words (`Tests`, `Testing`) out.
var testIdent = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)

// testDecl matches a test function DECLARATION.
var testDecl = regexp.MustCompile(`^func (Test[A-Za-z0-9_]+)\(`)

// commentLead strips a Go line comment's leading `//` and whitespace, so a name
// wrapped onto the next comment line can be rejoined.
var commentLead = regexp.MustCompile(`^\s*(//\s*|\*\s*)?`)

// leadingIdentRun matches the identifier characters a wrapped name continues
// with.
var leadingIdentRun = regexp.MustCompile(`^[A-Za-z0-9_]+`)

// TestEveryCitedTestExistsOrIsLedgered is the gate.
func TestEveryCitedTestExistsOrIsLedgered(t *testing.T) {
	root := moduleRoot(t)
	sources := goFiles(t, root)

	defined := map[string]bool{}
	for _, f := range sources.tests {
		for _, line := range readLines(t, f) {
			if m := testDecl.FindStringSubmatch(line); m != nil {
				defined[m[1]] = true
			}
		}
	}

	// 🔴 POSITIVE CONTROLS, BOTH DIRECTIONS, REPORTED AS A PAIR. A "0 unresolved"
	// verdict is indistinguishable from a scanner wired to nothing, so the
	// extraction has to be shown able to find a name that IS there and to reject
	// one that is not.
	const knownDefined = "TestRegistrationIsIndependentOfDependencies"
	if !defined[knownDefined] {
		t.Fatalf("positive control FAILED: %s is declared in internal/api/registration_test.go "+
			"and this scan did not find it. It read %d test file(s) and extracted %d "+
			"declaration(s); the instrument is not measuring.",
			knownDefined, len(sources.tests), len(defined))
	}
	if defined["TestThisNameIsNotDeclaredAnywhereInThisModule"] {
		t.Fatal("negative control FAILED: the defined set claims to contain a name that " +
			"is written nowhere. The extraction is matching too widely.")
	}
	t.Logf("controls: %d test declaration(s) across %d test file(s); %q resolves, an "+
		"invented name does not", len(defined), len(sources.tests), knownDefined)

	// 🔴 CITATIONS ARE COLLECTED TWICE, AND THE FIRST PASS IS WHAT MAKES THE
	// WRAP REPAIR SOUND. A test name split across a `//` line wrap — three of
	// them in this tree — reads as a truncated identifier, and reporting the
	// fragment as MISSING is a false finding about a name that is perfectly
	// fine. The repair joins a line-final fragment with the next comment line's
	// leading identifier run, but ONLY when the joined form is already known: a
	// declared test, or the same name cited WHOLE somewhere else. It can
	// therefore never invent a name, in either direction — it cannot manufacture
	// a resolution, and it cannot manufacture a ledger entry.
	whole := collectCitations(t, root, sources.nonTests, nil)
	known := map[string]bool{}
	for name := range defined {
		known[name] = true
	}
	for _, c := range whole {
		known[c.name] = true
	}
	cited := collectCitations(t, root, sources.nonTests, known)

	resolved := 0
	unresolved := cited[:0:0]
	for _, c := range cited {
		if defined[c.name] {
			resolved++
			continue
		}
		unresolved = append(unresolved, c)
	}
	cited = unresolved

	// 🔴 A SECOND POSITIVE CONTROL, ON THE CITATION SIDE. Zero citations found
	// would make every assertion below vacuously true.
	if resolved == 0 {
		t.Fatalf("positive control FAILED: not one of the %d citation(s) found in %d "+
			"non-test source(s) resolved to a defined test. That is implausible and "+
			"means the two sets are being built differently.",
			len(cited), len(sources.nonTests))
	}

	got := map[string]map[string]bool{}
	for _, c := range cited {
		if got[c.file] == nil {
			got[c.file] = map[string]bool{}
		}
		got[c.file][c.name] = true
	}

	// Unresolved citations that are not on the ledger.
	for file, names := range got {
		for name := range names {
			if !contains(unportedCitations[file], name) {
				t.Errorf("%s cites %s, which is declared nowhere in this module and is "+
					"not on the ledger.\n"+
					"  A comment naming a guard tells the next reader the question is\n"+
					"  settled — it does not leave coverage where it was, it removes the\n"+
					"  prompt that would have created it.\n"+
					"  Write the test, correct the name, or add it to unportedCitations\n"+
					"  in internal/modulegate/citations_test.go and say which it is.",
					file, name)
			}
		}
	}

	// Ledger entries that now resolve, or that nothing cites any more.
	var stale int
	for file, names := range unportedCitations {
		for _, name := range names {
			switch {
			case defined[name]:
				stale++
				t.Errorf("%s: %s is on the not-ported ledger but a test of that name EXISTS "+
					"now.\n  Remove the entry — the comment citing it is true again.", file, name)
			case !got[file][name]:
				stale++
				t.Errorf("%s: %s is on the not-ported ledger but nothing in that file cites "+
					"it any more.\n  Remove the entry, or the ledger is describing a comment\n"+
					"  that no longer exists.", file, name)
			}
		}
	}

	t.Logf("citations: %d resolved, %d unresolved across %d file(s), %d stale ledger entr(ies)",
		resolved, len(cited), len(got), stale)
}

// citation is one `Test*` name written in one non-test file.
type citation struct{ file, name string }

// collectCitations scans files for cited test names.
//
// known, when non-nil, enables the line-wrap repair: a name that ends exactly at
// end-of-line is joined with the next comment line's leading identifier run IF
// the joined form is in known. A nil known collects only whole-line names, which
// is what builds `known` in the first place.
func collectCitations(t *testing.T, root string, files []string, known map[string]bool) []citation {
	t.Helper()
	var out []citation
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatalf("relativising %s: %v", f, err)
		}
		lines := readLines(t, f)
		for i, line := range lines {
			for _, loc := range testIdent.FindAllStringIndex(line, -1) {
				name := line[loc[0]:loc[1]]
				if known != nil && loc[1] == len(strings.TrimRight(line, " \t")) && i+1 < len(lines) {
					next := commentLead.ReplaceAllString(lines[i+1], "")
					if run := leadingIdentRun.FindString(next); run != "" && known[name+run] {
						name += run
					}
				}
				out = append(out, citation{rel, name})
			}
		}
	}
	return out
}

// --- file plumbing -----------------------------------------------------------

type sourceSet struct{ tests, nonTests []string }

// goFiles enumerates every .go file under root.
//
// ⚠ IT WALKS THE TREE RATHER THAN SHELLING OUT TO grep. The interactive `grep`
// on the machine this was written on is a ugrep function that honours
// .gitignore, so a recursive search silently skips generated and ignored paths
// and returns a confident zero. A walk cannot do that.
func goFiles(t *testing.T, root string) sourceSet {
	t.Helper()
	var s sourceSet
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			s.tests = append(s.tests, path)
		} else {
			s.nonTests = append(s.nonTests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(s.tests)
	sort.Strings(s.nonTests)
	if len(s.tests) == 0 || len(s.nonTests) == 0 {
		t.Fatalf("positive control FAILED: the walk found %d test and %d non-test .go "+
			"file(s) under %s", len(s.tests), len(s.nonTests), root)
	}
	return s
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(string(b), "\n")
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
