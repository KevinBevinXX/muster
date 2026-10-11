package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// ---------------------------------------------------------------------------
// THE CHAT TIER'S WIRING, WHICH IS A SECOND AXIS ON THE SAME CALL.
//
// 🔴 EVERY TEST HERE IS ABOUT A COMBINATION, NOT ABOUT A VALUE. The two knobs are
// independent by design — lifecycle without chat is a deployment, and was the only
// one available for a whole step — so the states worth pinning are the CROSSINGS:
// chat on with lifecycle off (refused at boot), chat off with lifecycle on (the
// previous release's behaviour, which must not change), and both on.
// ---------------------------------------------------------------------------

// The runtime-config bundle every fixture in this package that names a runtime
// must carry, SPELLED ONCE.
//
// 🔴 THE BANNER PRINTS THEIR BYTE LENGTHS, so a second copy of either value would
// make TestTheChatWiredBannerDoesNotClaimReachability's pinned line disagree with
// whatever fixture produced it — and the failure would read as a banner change
// rather than as two fixtures. These are also what bannerBothDirections uses, so
// the pinned line's numbers are DERIVED from the same bytes rather than typed in.
//
// ⚠ NEITHER IS VALID FOR ANY REAL RUNTIME, DELIBERATELY. muster never parses
// either one — the whole point of runtimeconfig.go is that the schema is the
// operator's — so a fixture that looked like a real configuration would suggest
// this package validates something it does not.
const (
	fixtureRuntimeConfigTemplate = `{"example":{"key":"value"}}`
	fixtureRuntimeInstallScript  = "set -eu\nexec /example/entrypoint\n"
)

// gatewayTestConfig is a config that passes validate for the named driver and
// runtime, so each test varies one thing.
//
// ⚠ NAMING A RUNTIME NOW COMMITS A DEPLOYMENT TO THREE THINGS, NOT ONE, AND THIS
// HELPER SUPPLIES ALL THREE SO THAT EVERY POSITIVE CONTROL BUILT FROM IT IS A
// COMPLETE CONFIGURATION. The sentinel was the first; the runtime-config bundle's
// two keys are the second and third. A helper that supplied only the sentinel would
// make every "the same config WITH the field set must PASS" control in this file
// fail for the OTHER missing variable — a positive control that cannot distinguish
// the refusal it is about from any refusal at all.
func gatewayTestConfig(driver, runtime string) config {
	c := provisionerTestConfig(driver)
	c.AgentGateway = runtime
	if runtime != gatewayNone {
		c.AgentGatewayModel = "runtime-sentinel"
		c.AgentRuntimeConfig = fixtureRuntimeConfigTemplate
		c.AgentRuntimeInstall = fixtureRuntimeInstallScript
	}
	return c
}

// TestNamingARuntimeWiresAGatewayAndNotNamingOneWiresNothing pins both ends of the
// knob.
//
// 🔴 THE OFF END IS THE ONE THAT MATTERS, because it is what every existing
// deployment is: a released muster sets MUSTER_AGENT_PROVISIONER and nothing else,
// and an image bump that started sending agents chat turns would be a behaviour
// change nobody asked for. A nil gateway keeps both chat routes refusing at
// api.requireGatewayProvisioner exactly as before.
func TestNamingARuntimeWiresAGatewayAndNotNamingOneWiresNothing(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	prov, gw, err := buildAgentPlane(gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop, %s): %v", gatewayHooksSHA256, err)
	}
	if gw == nil {
		t.Fatal("naming a runtime produced no gateway, so both chat routes would keep refusing " +
			"and a dispatched agent's kickoff would still be undeliverable")
	}
	if got := gw.Runtime(); got != gatewayHooksSHA256 {
		t.Errorf("gw.Runtime() = %q, want %q — the banner reports this, so the wiring and the "+
			"banner cannot be allowed to disagree", got, gatewayHooksSHA256)
	}
	// CONTROL: the lifecycle half is unaffected by naming a runtime. Both are built
	// from one call now, and a change that wired chat by breaking lifecycle would
	// otherwise be invisible here.
	if prov == nil {
		t.Error("naming a runtime cost us the lifecycle provisioner")
	}

	off, offGw, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop, unset gateway): %v", err)
	}
	if offGw != nil {
		t.Errorf("an unset %s produced a %q gateway. The default must leave "+
			"api.Extensions.Gateway nil: a deployment that named only a provisioner gets exactly "+
			"the behaviour it had before this knob existed.", envAgentGateway, offGw.Runtime())
	}
	if off == nil {
		t.Error("the lifecycle provisioner is nil with the gateway unset, which is the " +
			"combination the previous release shipped")
	}

	// 🔴 THE ZERO-VALUE CONFIG IS ITS OWN CASE, for the reason
	// TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing records: buildApp
	// accepts configs that never went through loadConfig, so a resolver default that
	// was wrong would be invisible to the case above, which passes its value
	// explicitly.
	_, zeroGw, err := buildAgentPlane(config{}, stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane over a zero-value config: %v", err)
	}
	if zeroGw != nil {
		t.Errorf("a zero-value config produced a %q gateway. An unset %s must resolve to %q.",
			zeroGw.Runtime(), envAgentGateway, gatewayNone)
	}
}

// TestAGatewayWithoutADriverIsRefusedAtBoot pins the cross-check between the two
// knobs.
//
// 🔴 THE ALTERNATIVE IS A SERVER THAT BOOTS, PRINTS "CHAT: WIRED", AND CANNOT REACH
// ANY AGENT. This binary's only source of an instance's address is the provisioning
// driver, so a named runtime with no named driver is a chat tier that resolves
// nothing — and it would fail inside a request handler, once per turn, as an error
// about an endpoint rather than about configuration.
//
// ⚠ THE REFUSAL MUST NAME BOTH VARIABLES. An operator in this state set one of them
// deliberately; a message naming only the other sends them to change the wrong one.
func TestAGatewayWithoutADriverIsRefusedAtBoot(t *testing.T) {
	cfg := gatewayTestConfig(provisionerNone, gatewayHooksSHA256)
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted a named runtime with no named driver. The server " +
			"would boot, announce CHAT: WIRED, and answer every turn with an endpoint " +
			"resolution error from inside a handler.")
	}
	for _, want := range []string{envAgentGateway, envAgentProvisioner} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s.\n  got: %v", want, err)
		}
	}

	// POSITIVE CONTROL: the same configuration with a driver named must PASS, or the
	// assertion above is satisfied by any validation failure at all.
	if err := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a named runtime WITH a named driver was refused: %v", err)
	}
	// CONTROL: the no-driver state is fine as long as no runtime is named, which is
	// the default every existing deployment is in.
	if err := gatewayTestConfig(provisionerNone, gatewayNone).validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: neither knob set was refused: %v", err)
	}
}

// TestAnUnknownRuntimeNameIsRefusedAtBoot pins the legality check.
//
// 🔴 WITHOUT IT A TYPO IS INDISTINGUISHABLE FROM "off". buildGateway's default arm
// would be reached, and a nil gateway means both chat routes refuse — so
// a one-character typo in the scheme name would present as a deployment that
// simply never turned chat on, with a banner that agrees.
func TestAnUnknownRuntimeNameIsRefusedAtBoot(t *testing.T) {
	err := gatewayTestConfig(provisionerNoop, "hooks-sha255").validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted an unknown runtime name, so a typo presents as " +
			"\"chat was never enabled\" rather than as a configuration error")
	}
	if !strings.Contains(err.Error(), envAgentGateway) {
		t.Errorf("the refusal does not name %s.\n  got: %v", envAgentGateway, err)
	}
	// It must name what IS legal, or the operator has to read the source to fix it.
	for _, want := range gatewayChoices {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name the legal value %q.\n  got: %v", want, err)
		}
	}
}

// TestAMissingSentinelIsRefusedAtBoot pins the second half of what naming a runtime
// commits a deployment to.
//
// 🔴 THE SENTINEL HAS NO DEFENSIBLE DEFAULT AND ITS ABSENCE IS INVISIBLE UNTIL A
// TURN. Both gateway endpoints require the wire's `model` field; an empty one is an
// HTTP 400 from inside a chat handler, per turn, with a message about a model —
// which reads as a model-configuration problem rather than as an unset variable.
// Boot is the only place that failure can be attributed correctly.
//
// ⚠ IT IS A SEPARATE REFUSAL FROM THE DRIVER CROSS-CHECK, not a second clause of
// it, because the two are independently reachable: a deployment can name a runtime
// AND a driver and still omit the sentinel.
func TestAMissingSentinelIsRefusedAtBoot(t *testing.T) {
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	cfg.AgentGatewayModel = ""
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted a named runtime with no sentinel. Every chat turn " +
			"would come back 400 from inside a handler, naming the model field.")
	}
	if !strings.Contains(err.Error(), envAgentGatewayModel) {
		t.Errorf("the refusal does not name %s, so the operator cannot act on it.\n  got: %v",
			envAgentGatewayModel, err)
	}

	// POSITIVE CONTROL: the same config WITH a sentinel must pass, or the assertion
	// above is satisfied by any validation failure at all.
	if err := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a named runtime with a sentinel was refused: %v", err)
	}
	// CONTROL: with no runtime named the sentinel is irrelevant and must not be
	// required — otherwise every existing deployment stops booting.
	noGateway := provisionerTestConfig(provisionerNoop)
	noGateway.AgentGatewayModel = ""
	if err := noGateway.validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: a deployment with no gateway was refused for having no "+
			"sentinel: %v", err)
	}
}

// TestTheChatWiredBannerDoesNotClaimReachability pins the correction an audit forced.
//
// 🔴 THE LINE SHIPPED CLAIMING THE CHAT ROUTES WERE LIVE "over the same driver as
// lifecycle", AND THAT WAS FALSE FOR EVERY AGENT THIS BINARY PROVISIONS. The routes
// really do stop refusing; that was the only part that was true.
//
// ⚠ BOTH MECHANISMS THAT MADE IT FALSE ARE FIXED NOW, AND THIS TEST STILL EXISTS
// BECAUSE THE CLAIM IT FORBIDS IS NOT. The first version of this paragraph read
// "agentspec.Build renders a spec with no port and no endpoint, so the driver resolves
// no address and a chat turn fails PER TURN". agentspec.Build declares the gateway port
// now and ships the token under the name the bearer is derived from, so neither failure
// is reachable — but the container half of that derivation is a shell command in another
// repository and NO turn has been made against an instance this binary created. The line
// says "NOT VERIFIED REACHABLE" for that reason, which is a weaker claim than the old
// one and a stronger one than "live". Those are three different sentences and this test
// is what keeps them apart.
//
// 🔴 IT PINS THE WHOLE NORMALISED LINE, AND THE SUBSTRING VERSION OF THIS TEST WAS
// MEASURED WALKABLE. That version required four phrases and forbade two, and its own
// comment claimed "IT PINS THE CLAIM, NOT A KEYWORD" — an audit then wrote a banner
// carrying all four required phrases, neither forbidden one, and the sentence "chat is
// FULLY USABLE against any agent, including one this binary provisioned … the old
// warning is obsolete; ignore them", and THIS TEST PASSED. A guard that reads as
// coverage while providing none is worse than no guard, because it stops the next
// person looking. When the artifact under test IS PROSE, only the whole string is a
// machine-readable claim.
//
// ⚠ THE COST IS REAL AND IS ACCEPTED: any cosmetic reword of that banner line fails
// this test and wantLine must be updated with it. That is the price of the property.
// The forbidden list stays alongside — a pinned line is walkable one way, by
// regenerating the golden, and re-pinning is a reflex; the negatives survive a
// careless re-pin because they encode INTENT rather than text.
func TestTheChatWiredBannerDoesNotClaimReachability(t *testing.T) {
	onOut, _ := bannerBothDirections(t)
	line := lineNaming(onOut, envAgentGateway)
	if line == "" {
		t.Fatalf("no banner line names %s at all.\nbanner:\n%s", envAgentGateway, onOut)
	}

	// Built from the same constants the banner formats, so a renamed variable or a
	// changed scheme spelling moves BOTH sides and this stays a claim about the
	// SENTENCE rather than about the values interpolated into it.
	wantLine := "agent provisioning CHAT: WIRED " + envAgentGateway + "=" + gatewayHooksSHA256 +
		" (" + envAgentGatewayModel + "=runtime-sentinel) — the two chat routes no longer " +
		"refuse at api.requireGatewayProvisioner. 🔴 WIRED IS NOT VERIFIED REACHABLE: both " +
		"in-repo blockers are closed — the spec declares port " +
		strconv.Itoa(agentspec.DefaultGatewayPort) + " so the driver renders a Service and " +
		"resolves an address, and the row's token now ships as " + agentspec.EnvGatewayToken +
		", the variable this bearer is derived from — but the container half of that " +
		"derivation lives in the agent image's own deployment, which nothing here can read, " +
		"so the first turn against an agent this binary provisioned is still the measurement. " +
		"A dispatch with a kickoff is ACCEPTED: the kickoff deliverer (internal/agentkickoff, " +
		"every " + agentkickoff.DefaultInterval.String() + ") hands each owned agent's pending " +
		"note to its instance through this gateway once the instance is ready, and records a " +
		"failed or EMPTY first turn in agents.kickoff_error. The agent runtime-config bundle IS " +
		"INSTALLED: " + envAgentRuntimeConfig +
		" (27 bytes) is placed at " + agentspec.RuntimeConfigPath + ", " + envAgentRuntimeInstall +
		" (" + strconv.Itoa(len(fixtureRuntimeInstallScript)) + " bytes) REPLACES the image's entrypoint as the container's command, and the " +
		"derived credential travels as " + agentspec.EnvGatewayBearer + ". 🔴 THAT SCRIPT IS " +
		"THE OPERATOR'S AND NOTHING HERE CAN CHECK IT: if it does not install the file and " +
		"re-exec the real entrypoint, the instance does not serve — the startup probe is " +
		"what reports that. See cmd/muster-server/doc_seams.go entry 1"

	if line != wantLine {
		t.Errorf("the CHAT: WIRED line changed.\n  got:  %s\n  want: %s\n"+
			"  This line is PINNED WHOLE on purpose: it carries a correction an audit forced —\n"+
			"  the routes stop refusing, and nobody has yet made a turn against an agent THIS\n"+
			"  binary provisioned, so the line claims the two mechanisms are fixed and claims\n"+
			"  NOTHING about a turn succeeding. A reword that keeps that distinction is fine;\n"+
			"  update wantLine with it. A reword that collapses it — in EITHER direction, to\n"+
			"  \"the routes are live\" or back to \"declares no port\" — restores a banner\n"+
			"  stating a falsehood, and a substring version of this test was MEASURED to pass\n"+
			"  over exactly that.",
			line, wantLine)
	}

	// 🔴 THE NEGATIVES ARE NOT REDUNDANT WITH THE PIN. They are what survives a lazy
	// re-pin: regenerating wantLine from a bad line is one paste, and these phrases
	// encode INTENT rather than text.
	//
	// ⚠ THE THIRD ONE IS NEW AND IT GUARDS THE OPPOSITE DIRECTION FROM THE OTHER TWO.
	// "declares no port" was the TRUE half of the old line and is now false: the spec
	// declares one, and a banner restating the old diagnosis would send an operator to
	// fix a Service that exists. So this list now forbids both a claim that over-states
	// (the routes are live) and a claim that under-states (there is no port) — a pinned
	// line can regress in either direction and only the first was covered.
	for _, forbidden := range []string{
		"chat routes are live",
		"over the same driver as lifecycle",
		"declares no port",
	} {
		if strings.Contains(strings.ToLower(line), strings.ToLower(forbidden)) {
			t.Errorf("the CHAT: WIRED line has regained the retracted claim %q.\n  line: %s",
				forbidden, line)
		}
	}
}

// TestTheRetractedReachabilityClaimIsGoneFromEveryNonTestSource is the sweep half,
// and it is the SECOND attempt — the first was walkable in two measured ways.
//
// 🔴 A RETRACTION IS A SWEEP OVER THE WHOLE TREE, AND MINE WAS AN EDIT WHERE I WAS
// LOOKING. The round that corrected the banner left "The two CHAT ROUTES are live" in
// doc_seams.go — the one document the corrected banner POINTS A READER AT. The sweep
// that missed it was a lowercase fixed-string grep.
//
// 🔴 AND THE GUARD WRITTEN TO FIX THAT WAS ITSELF WALKABLE, MEASURED, TWICE. It matched
// a regexp against raw file bytes over a FOUR-FILE list, so:
//
//   - restoring the sentence WRAPPED one word earlier survived, because the pattern
//     forbade a newline inside the phrase and in the gap — and an ordinary comment
//     rewrap is the likeliest way that text ever comes back;
//   - the same sentence in internal/api/ext.go survived, because that file was not on
//     the list — and ext.go is where api.Gateway is DECLARED, i.e. the single most
//     likely place for a reader to restate it.
//
// It was named "…TREEWIDE" while sweeping 4 of 121 non-test Go files, and its docstring
// claimed "it covers the sources a reader arrives at" and a "narrow and named
// exemption" it did not implement. That is the same defect one level up: a guard
// reading as coverage while providing none.
//
// SO THIS VERSION: every non-test .go file in the module, and the comment text
// NORMALISED first — leading "//" and indentation stripped and lines joined — so a
// wrap cannot hide the phrase. The cost is that it reads the whole tree on every run;
// measured in milliseconds, which is not a reason to check less.
func TestTheRetractedReachabilityClaimIsGoneFromEveryNonTestSource(t *testing.T) {
	// The claim as a RELATIONSHIP, not a spelling: "chat routes" near a reachability
	// word. Applied to normalised text, so newlines are no longer part of the puzzle.
	re := regexp.MustCompile(`(?i)chat[ _-]?routes?.{0,80}?(live|usable|reachable)`)

	// 🔴 THE EXCLUSION IS A PINNED LIST OF THIS TREE'S OWN CORRECTIONS, NOT A NEGATION
	// VOCABULARY — AND THE VOCABULARY VERSION WAS MEASURED WALKABLE FOUR WAYS. The first
	// attempt excluded any match containing not/never/no longer/rather than, because the
	// wider sweep had flagged the corrections themselves ("not the same as reachable",
	// "WIRED IS NOT REACHABLE") — the reachability word is present there precisely to be
	// denied, which is the recorded FAIL-CLOSED hyphen trap and runs the DANGEROUS way,
	// pressuring the correction to be reworded to appease the guard.
	//
	// But a negation word ANYWHERE in the match suppressed the hit, and the regexp's
	// match is non-greedy, so the window is exactly where such a word sits. An audit
	// injected four sentences that assert reachability THROUGH a negation and all four
	// survived with the suite green:
	//
	//	"The two chat routes are not merely wired but live."
	//	"The two CHAT ROUTES, never usable before this change, are live."
	//	"The two chat routes no longer refuse and are now fully live."
	//	"The chat routes now serve rather than refuse, and are usable end to end."
	//
	// The first uses this PR's own wired-vs-live vocabulary, i.e. the likeliest way the
	// retracted claim comes back. So the exclusion is INVERTED: only the exact
	// corrections this tree contains are allowed, pinned as literals. Anything else is
	// flagged whether it is negated or not.
	//
	// ⚠ THE COST, NAMED HONESTLY THIS TIME: rewording a correction fires this guard, and
	// the fix is to add the new wording here. That is deliberate — it forces a human to
	// look at the sentence, which is the whole point, and it is the opposite of the first
	// attempt's cost, which was silently allowing a false claim.
	// ⚠ THE SECOND ENTRY IS THE COST THIS GUARD'S OWN HEADER PROMISED, PAID. The banner
	// line it covers used to end "🔴 WIRED IS NOT REACHABLE" and now reads "🔴 WIRED IS
	// NOT VERIFIED REACHABLE", because the two mechanisms behind the old wording are
	// fixed and what remains is missing EVIDENCE rather than a known defect. Inserting
	// one word moved the match out from under the OLD entry — `isCorrection` compares
	// whole strings in both directions — so the new wording is pinned here, which is
	// exactly the "forces a human to look at the sentence" the note above describes.
	//
	// 🔴 AND THE OLD ENTRY IS *REPLACED*, NOT KEPT BESIDE IT, BECAUSE KEEPING IT
	// WHITELISTED THE CLAIM THIS PR MADE FALSE. A first draft of this list carried both
	// spellings under a comment claiming "internal/modulegate and doc_seams.go still
	// carry the un-VERIFIED spelling in their own prose". They do not: `git grep -l
	// 'WIRED IS NOT REACHABLE'` returns exactly ONE file — THIS one, which the sweep
	// skips as _test.go — and modulegate's copy is a test file regardless. So the entry
	// suppressed nothing real while licensing the old sentence anywhere in the tree.
	// Measured, with the pair that makes it a defect rather than dead weight: plant
	// "…🔴 WIRED IS NOT REACHABLE: agentspec.Build declares no port, so this driver
	// resolves no address." in internal/api/ext.go — where api.Gateway is DECLARED, i.e.
	// the likeliest place for a reader to restate it — and the sweep SURVIVED with the
	// old entry present and CAUGHT it once removed. Removing the NEW entry instead goes
	// red on main.go, which is the control proving that green was about the old entry
	// and not about this sweep being blind to deletions.
	//
	// ⚠ SO THE RULE THIS LIST OBEYS, STATED ONCE: AN ENTRY IS A LICENCE, AND A LICENCE
	// FOR A SENTENCE THE TREE NO LONGER CONTAINS IS A LICENCE FOR SOMEBODY TO ADD IT
	// BACK. When a correction is reworded, REPLACE its entry — and prove the old one is
	// unused by planting it in a swept file and watching the sweep catch it.
	allowedCorrections := []string{
		"stop REFUSING — which is not the same as reachable",
		"chat routes no longer refuse at api.requireGatewayProvisioner. 🔴 WIRED IS NOT VERIFIED REACHABLE",
	}
	isCorrection := func(m string) bool {
		for _, ok := range allowedCorrections {
			if strings.Contains(m, ok) || strings.Contains(ok, m) {
				return true
			}
		}
		return false
	}

	root := moduleRootForSweep(t)
	var swept, hits int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		swept++
		for _, m := range re.FindAllString(normaliseComments(string(body)), -1) {
			if isCorrection(m) {
				continue // one of this tree's own corrections — see the note above
			}
			hits++
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s asserts the retracted reachability claim: %q\n"+
				"  The chat routes stop REFUSING, and that is all this tree may say. The two\n"+
				"  mechanisms that used to block a turn — no declared port, and the token\n"+
				"  shipped under a name the bearer is not derived from — are fixed, but NO turn\n"+
				"  has been made against an agent this binary provisioned, and the container\n"+
				"  half of the bearer derivation lives in another repository. See\n"+
				"  cmd/muster-server/doc_seams.go entry 1 for what that leaves open.", rel, m)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// 🔴 POSITIVE CONTROLS ON THE PATTERN'S SHAPES. ⚠ An earlier wording called these
	// "ONE PER HOLE THE PREVIOUS VERSION HAD", which was not the mapping: the
	// single-line form was already matched by that version (so it is not one of its
	// holes), the wrapped form IS one, the file-reference-in-the-gap one closes a hole
	// the docstring never named, and the FILE-COVERAGE hole — the claim living in a file
	// that was not on the old four-file list — has no control here at all; it is covered
	// by the walk plus the swept-count floor below, which is a different instrument. A
	// sentence reading as a coverage map that is not one is the defect this whole guard
	// exists to fix, so it is stated accurately rather than tidily.
	for _, ctl := range []struct{ name, text string }{
		{"the single-line form that was missed",
			"// NOTHING CALLS THE GATEWAY ON THE DISPATCH PATH. The two CHAT ROUTES are live;"},
		{"the WRAPPED form that survived the previous guard",
			"// NOTHING CALLS THE GATEWAY ON THE DISPATCH PATH. The two CHAT ROUTES are\n// live; the kickoff is not a route."},
		{"a restatement with a file reference in the gap",
			"// With a Gateway set the two chat routes (see ext.go) are live."},
	} {
		if !re.MatchString(normaliseComments(ctl.text)) {
			t.Errorf("positive control FAILED (%s): the pattern cannot match %q, so the sweep's "+
				"zero over %d file(s) says nothing about that shape", ctl.name, ctl.text, swept)
		}
	}
	// 🔴 CONTROLS ON THE EXCLUSION, IN BOTH DIRECTIONS — an exclusion that swallowed the
	// real claim would make every zero above vacuous, and one that rejected the tree's
	// own corrections would fail this suite on its own fix. The first four are the
	// sentences that WALKED the previous vocabulary-based version.
	for _, mustFlag := range []string{
		"The two CHAT ROUTES are live;",
		"the two chat routes are live over the same driver as lifecycle.",
		"chat routes are FULLY USABLE against any agent",
		"The two chat routes are not merely wired but live.",
		"The two CHAT ROUTES, never usable before this change, are live.",
		"The two chat routes no longer refuse and are now fully live.",
		"The chat routes now serve rather than refuse, and are usable end to end.",
		// Split across a Go concatenation, the way the banner's own strings are written.
		`fmt.Sprintf("the two chat routes are " + "live over the same driver")`,
	} {
		m := re.FindString(normaliseComments(mustFlag))
		if m == "" {
			t.Errorf("control FAILED: the relationship pattern does not match %q, so the sweep "+
				"cannot see that shape at all", mustFlag)
			continue
		}
		if isCorrection(m) {
			t.Errorf("control FAILED: the exclusion swallows the reachability claim %q "+
				"(matched %q), so every zero this sweep reports is vacuous about that shape",
				mustFlag, m)
		}
	}
	for _, mustSkip := range allowedCorrections {
		m := re.FindString(normaliseComments(mustSkip))
		if m != "" && !isCorrection(m) {
			t.Errorf("control FAILED: this tree's own correction %q is not excluded (matched "+
				"%q), so the guard fails on its own fix", mustSkip, m)
		}
	}

	// And the instrument must be reading a real tree, not an empty one.
	if swept < 50 {
		t.Fatalf("swept only %d non-test .go file(s) under %s — this module has well over 100, "+
			"so the walk is not reading what it claims to", swept, root)
	}
	t.Logf("swept %d non-test .go file(s), %d hit(s)", swept, hits)
}

// normaliseComments strips Go line-comment markers and indentation and joins the
// remaining text, so a claim WRAPPED across comment lines reads as one string.
//
// 🔴 IT IS WHY THE SWEEP IS WRAP-PROOF, AND THE PREVIOUS VERSION'S ABSENCE OF IT IS
// WHAT LET A REWRAP WALK PAST. It deliberately does NOT try to parse Go — a block
// comment or a string literal is normalised too, which can only make the sweep see
// MORE, and over-matching here fails loudly rather than silently.
func normaliseComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "//")
		b.WriteString(strings.TrimSpace(t))
		b.WriteByte(' ')
	}
	// 🔴 GO STRING CONCATENATION IS COLLAPSED TOO, AND THE SWEEP'S FIRST RUN PROVED IT
	// HAS TO BE. The banner's own correction is written across a `"+ "` join, so the
	// joined text read `…no longer "+ "refuse at…` and did not match the pinned
	// correction — the guard flagged its own fix. A claim split across a concatenation is
	// exactly as hidden as one split across a comment wrap: same class, same remedy.
	return goStringJoin.ReplaceAllString(b.String(), "")
}

// goStringJoin matches the `" + "` seam between two halves of a concatenated Go string
// literal, in any spacing.
var goStringJoin = regexp.MustCompile(`"\s*\+\s*"`)

// moduleRootForSweep finds the directory holding go.mod, walking up from the test's
// own working directory. Spelled here rather than hardcoded so the sweep cannot
// silently read a subtree.
func moduleRootForSweep(t *testing.T) string {
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
			t.Fatalf("no go.mod above %s, so the sweep has no root", dir)
		}
		dir = parent
	}
}

// TestAnUnknownProvisionerNameIsStillRefused is the control for the oneOf consolidation.
//
// ⚠ IT IS HERE BECAUSE A REFACTOR MOVED A PREDICATE, NOT BECAUSE THE PROVISIONER CHANGED.
// Folding two identical membership loops into oneOf touched the provisioner's legality
// check, and nothing in this package asserted THAT branch — so the consolidation could
// have broken it silently. Mutation-checked: making oneOf always return true kills this.
func TestAnUnknownProvisionerNameIsStillRefused(t *testing.T) {
	cfg := provisionerTestConfig("kubernets") // one letter, the realistic typo
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted an unknown driver name, so a typo would fall through " +
			"to buildDriver's unreachable default arm at dispatch time instead of failing at boot")
	}
	if !strings.Contains(err.Error(), envAgentProvisioner) {
		t.Errorf("the refusal does not name %s.\n  got: %v", envAgentProvisioner, err)
	}
	// POSITIVE CONTROL: a legal name must still pass, or the assertion above is
	// satisfied by any refusal at all.
	if err := provisionerTestConfig(provisionerNoop).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a legal driver name was refused: %v", err)
	}
}
