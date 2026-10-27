package agentspec

import (
	"fmt"
	"path"
	"strings"

	"github.com/ZacxDev/muster/internal/provision"
)

// The subsystem-store ("cairn") integration: the client, its credential, and the
// one gate that decides whether either exists.
//
// # What this is
//
// cairn is the client for a hosted, curated knowledge store about how an
// operator's subsystems actually work — durable notes that outlive an instance.
// The store itself is not part of muster; this file installs its PUBLIC client
// (github.com/ZacxDev/cairn, pinned by sha) into one eligible instance and places
// the credential that client reads.
//
// # 🔴 IT SHIPS OFF, AND "OFF" MEANS NOTHING IS EMITTED AT ALL
//
// [Config.CairnURL] and [Config.CairnToken] are BOTH-OR-NEITHER, and with either
// one empty this file contributes no Init entry, no file and no environment
// variable. That is not a courtesy default: a store URL without a token is
// useless in exactly the case a credential exists for, and a token without a URL
// has nowhere to go. [Config.CairnConfigured] is the ONE predicate for that
// question and every branch below reads it — including the caller's, which uses
// the same method to decide whether the instruction prose may claim the
// capability. Two separately-spelled gates is how a prompt comes to describe a
// credential the instance does not hold.
//
// # 🔴 IT IS SCOPED TO ONE AGENT BY THE CALLER, NOT BY THIS PACKAGE
//
// The credential is READ+WRITE across ALL scopes of the store. A dispatched
// worker is a short-lived agent running a model over somebody's repository
// contents; handing every one of them a write key widens the blast radius of a
// bad turn or a prompt injection from "one branch on one repo" to "the operator's
// curated knowledge base", which nothing here would notice and no test could
// restore. So eligibility is [Options.CairnEligible] — an explicit input — and
// the policy that sets it lives with the caller that knows which agent is the
// supervisor. This package deliberately does not compare an agent name against a
// constant; [Options.SeedFiles] makes the same argument for the same reason.
//
// Widening it later is a one-line change at that caller. Un-polluting a store a
// fleet of agents wrote to is not, which is the asymmetry that sets the default.
//
// # 🔴 WHAT CHANGED ON THE WAY ACROSS, AND WHY EACH CHANGE IS FORCED
//
// Upstream this was one ~41-line shell entry appended to a Helm chart's
// extraInitCommands, which ran in the APPLICATION container under `set -e`. Three
// properties of that environment do not hold here, and a verbatim port would have
// been broken in a way no test of the string could see:
//
//  1. 🔴 [provision.Spec.Init] RUNS IN A SEPARATE CONTAINER. The kubernetes
//     driver renders it as an initContainer (internal/provision/k8s/render.go),
//     and that container's own filesystem is discarded when it exits. Upstream's
//     entry downloaded the client to /opt and linked it into /usr/local/bin —
//     both of which would vanish before the agent ever ran. The only writable
//     surface both containers see is the WORKSPACE volume, which is why the
//     client lands under the workspace and the wrapper that reaches it is a
//     [provision.File] (placed by the driver into both containers) rather than
//     something the shell writes. k8s.workspaceVolumeName's own comment names
//     this as "the single most common way an init container is written and
//     silently does nothing".
//  2. 🔴 FAILURE IS HARD HERE, AND UPSTREAM'S WAS SOFT. Upstream wrapped the
//     whole entry in `{ … } || :` and echoed WARNING lines, because a bare
//     non-zero exit in that chart's startup script was a crash loop rather than
//     a missing file — so nothing in that block could ever be observed to fail.
//     muster's driver runs Init under `sh -eu` and states that a failing step
//     SHOULD stop the pod. Importing the swallow would have imported the
//     workaround without the problem, which is the specific mistake autosave.go's
//     header warns about.
//     ⚠ THE COST IS REAL AND A REVIEWER SHOULD WEIGH IT RATHER THAN WAVE IT
//     THROUGH: a transient failure fetching the client now crash-loops the
//     eligible instance instead of degrading it. The argument for accepting that
//     is the gate above — the install exists only because an operator supplied a
//     store credential, and the prose that ships with it TELLS the agent it has a
//     working client. A soft failure would ship exactly the falsehood the gate
//     exists to prevent, with no channel to report it; a crash loop is a log.
//  3. THE CREDENTIAL IS DATA, NOT A SHELL WRITE. Upstream read two env vars in
//     the init script, tested them with `[ -n … ]`, and printf'd a file under a
//     umask. Here it is a [provision.File] with Secret set, so the driver places
//     it at mode 0600 and a driver whose Capabilities.Secrets is false REFUSES
//     the spec instead of downgrading it. The `[ -n … ]` arm disappears with the
//     shell that needed it — the Go gate above is the only gate, and it is
//     type-checked.
//
// # ⚠ WHAT WAS DROPPED
//
//   - THE `SUBSYSTEM_STORE_*` COMPATIBILITY SPELLINGS. Upstream wrote four keys
//     into the credential file — the current `CAIRN_*` pair and the pre-rename
//     `SUBSYSTEM_STORE_*` pair — because its live pods could be running a client
//     older than the rename. muster has no such pod: [cairnRev] and this file
//     ship in one commit, so the only client an installation can get is the one
//     pinned here, which reads the current names. The old keys are not free —
//     read at the pinned rev, lib/env_aliases.py emits one deprecation line per
//     old key per process, to stderr, forever. And the failure mode if a client
//     older than the rename ever did run is LOUD: it refuses at exit 3 naming
//     the variables it wanted. Carrying a compatibility window for a client this
//     installation cannot produce would be prose and noise rather than safety.
//   - THE WARNING ECHOES. They belonged to the soft-failure arms, which are gone
//     with item 2 above.
const (
	// cairnRev pins the github.com/ZacxDev/cairn revision the client is fetched
	// from.
	//
	// 🔴 PINNED BY SHA, AND NOT VENDORED. The store deliberately has ONE reader
	// implementation, so a vendored copy in this repository would be a second one
	// with nothing to keep it honest. Fetching by sha from a PUBLIC repository
	// needs no credential and cannot move under us.
	//
	// 🔴 BUMPING THIS MEANS RE-DERIVING [cairnLibModules] IN THE SAME COMMIT.
	// The module list is a property of the revision, and
	// TestCairnRevAndLibModulesAreOneLedger fails on either half moving alone.
	// A rev bump can add, rename or drop a module — including one imported
	// lazily, which every probe the install runs would pass over.
	cairnRev = "a0745ed7973b0b8143c147e7cfb05e7d9d7b0a0a"

	// cairnSource is the raw-content base the client is fetched from.
	cairnSource = "https://raw.githubusercontent.com/ZacxDev/cairn/"

	// cairnWrapperPath is where the on-PATH entrypoint is placed. It is a
	// [provision.File], so it exists in BOTH the init container and the
	// application container — which is what lets the install's last probe run the
	// very command the agent will invoke.
	cairnWrapperPath = "/usr/local/bin/cairn"

	// cairnWrapperMode must be executable: the file IS the command on PATH.
	cairnWrapperMode = 0o755

	// CairnConfigPath is where the store credential is placed.
	//
	// 🔴 IT IS AN ABSOLUTE PATH OF OUR CHOOSING, AND [EnvCairnConfig] IS WHAT
	// MAKES THAT LEGAL. Read at the pinned revision from lib/cairn_instances.py's
	// config_path(): with the variable unset it returns the user's home directory
	// joined with `.config/subsystem-store/env`, and with it set it returns the
	// path verbatim. Relying on the default would mean asserting what HOME is
	// inside an image this package has never seen; naming the file explicitly
	// removes that assumption FOR THE CREDENTIAL.
	//
	// 🔴 FOR THE CREDENTIAL, AND NOT FOR THE CLIENT — THE SENTENCE ABOVE USED TO
	// CLAIM IT "REMOVES THE ASSUMPTION ENTIRELY", WHICH WAS AN OVER-CLAIM. Read at
	// the pinned revision: lib/subsystem_read_store.py's DEFAULT_CACHE_ROOT is
	// derived from the process's home directory, and lib/env_aliases.py's own
	// ledger records that NOTHING in either language reads a cache-root variable.
	// So every read verb still writes a home-derived cache and [EnvCairnConfig]
	// cannot move it. The assumption this constant removes is the one about where
	// the CREDENTIAL is read from — the half a spec can control. The cache half is
	// the image's by DEFAULT, and this constant does not move it.
	//
	// ⚠ "NOT BY AN ENVIRONMENT VARIABLE" IS NOT "NOT AT ALL", AND THIS PARAGRAPH
	// USED TO READ AS THOUGH IT WERE. Read at the pinned revision: the top-level
	// parser declares a `--cache` option, and the resolver returns the given path
	// whenever the flag APPEARED (the flag's action sets an explicit marker; the
	// default is false) rather than the home-derived default. So the cache root IS
	// movable — by an explicit flag, which is a thing [cairnWrapperFile] could pass
	// on every invocation. THE UNMITIGATED HAZARD IS THE DEFAULT WE SHIP, NOT AN
	// ABSENT CAPABILITY.
	//
	// THE CANDIDATE FIX IS RECORDED AND DELIBERATELY NOT TAKEN HERE: have the
	// wrapper exec the client with `--cache <workspace>/.muster/cairn-cache`,
	// derived from the workspace path [checkCairnWorkspace] already refuses to let
	// be unsafe, which removes the home-writability case below wherever the
	// workspace itself is writable. It is not a comment fix — it changes what every
	// installed agent runs — so it belongs to its own review, and one thing that
	// review must settle: five verbs (sync, ls-entries, doctor, an all-scopes
	// search, routes --check) REFUSE an explicit `--cache` when MORE THAN ONE
	// instance is configured, because it would make them share a directory. So the
	// flag is only unconditionally safe while a spec places exactly one credential,
	// which is what [CairnConfigured] gates today. THE CLOSING CONDITION is a change
	// to [cairnWrapperFile] that passes `--cache` under the workspace, checked by
	// that change's reviewer against one mechanical run: a read verb succeeding with
	// an unwritable home.
	//
	// 🔴 CONSEQUENCE: THE INSTALL CAN REPORT SUCCESS OVER A CLIENT THAT CANNOT
	// FUNCTION, AND THESE ARE THE CASES. Probes 1 and 3 are `--help` — the staged
	// file and then the command on PATH; probe 2 is the IMPORT probe, which this
	// file's own "DOWNLOAD -> VERIFY" section names as such. (The premise used to
	// read "all three probes are `--help`", which was wrong about probe 2 and is
	// corrected here because it is the stated premise of the hazard below.) The
	// conclusion is unchanged and was measured rather than inferred: with an EMPTY,
	// READ-ONLY home and no credential configured, probe 1's `--help` and probe 2's
	// import of every [cairnLibModules] entry each exit 0 and create nothing under
	// it. So neither of the following is observable at install time — each surfaces
	// later, when the agent runs a verb:
	//   - A HOME THE PROCESS CANNOT WRITE. Every read verb wants a cache beneath
	//     it; nothing in the install ever tries to create one.
	//   - A CREDENTIAL THE PROCESS CANNOT READ. The file lands 0600 (see
	//     [cairnCredentialMode]), and NOTHING in internal/provision sets a
	//     SecurityContext, RunAsUser, RunAsGroup or FSGroup — measured absent
	//     across the repository's own Go, not assumed — so under an image whose
	//     user is not the mount's owner the credential is unreadable. Measured
	//     client behaviour in that state is a refusal at exit 3 naming the two
	//     variables it wanted and the file it looked in: loud, but only once a verb
	//     runs.
	//
	// ⚠ AND THE RUNTIME IMAGE MUST PROVIDE `curl`, `python3`, AND A `timeout` THAT
	// ACCEPTS `-k`. The install entry uses all three and checks for none of them,
	// and nothing else in this repository records the requirement — so this is the
	// note whoever changes the image will see. A missing one fails the INSTALL
	// rather than the agent, which is the better of the two failures and still not
	// a diagnosed one.
	//
	// ⚠ THE SENTENCE ABOVE DESCRIBES THAT DEFAULT RATHER THAN QUOTING THE PYTHON,
	// and not for style. tests/leakscan.py's private-hostname rule matches a
	// dotted label ending in one of a handful of lab suffixes, and the upstream
	// call's receiver-plus-method spelling is one of those shapes — so quoting it
	// refuses the file. The first attempt to explain that in a comment quoted it
	// twice and refused the file twice, which is the gate working correctly on
	// prose rather than on infrastructure. Describe, do not quote.
	CairnConfigPath = "/etc/muster/cairn/env"

	// cairnCredentialMode is stated rather than left to [provision.File]'s
	// Secret default.
	//
	// ⚠ IT IS THE SAME VALUE THAT DEFAULT WOULD PRODUCE, AND WRITING IT IS STILL
	// WORTH IT: a reader asking "what mode does the store token land at" should
	// not have to resolve a zero value through another package to find out.
	cairnCredentialMode = 0o600

	// cairnDirName is the workspace-relative directory the client is installed
	// into. The leading dot keeps it out of the way of an agent's `ls`, and it
	// sits under one parent so a future second artefact has somewhere to go
	// without another top-level dotfile in somebody's repository checkout.
	cairnDirName = ".muster/cairn"

	// cairnStageDirName is where a fetch is staged before it replaces a previous
	// install.
	//
	// ⚠ THE SWAP IS DESTRUCTIVE AND ORDERED, WHICH ONLY MATTERS FOR A PERSISTENT
	// WORKSPACE. `mv` needs its destination gone or it nests the staging
	// directory inside it, so the old install is removed first. Under `sh -eu`
	// that line is unreachable unless the staged copy has already been fetched
	// AND verified, so a failed fetch leaves a previous good install alone —
	// which is stronger than the upstream ordering, not weaker.
	cairnStageDirName = ".muster/.cairn.part"

	// cairnProbeSentinel is what the import probe prints and the install greps
	// for. A probe whose output nobody reads passes on a python that wrote a
	// traceback to stderr and exited 0.
	cairnProbeSentinel = "CAIRN_LIB_OK"
)

// cairnLibModules are the `lib/*.py` modules that make up the client at
// [cairnRev].
//
// The entrypoint (`cairn`, at the repository root) does
// `sys.path.insert(0, Path(__file__).resolve().parent / "lib")`, so the two must
// land as siblings; everything else it needs is the Python standard library.
//
// 🔴 ONE LIST FEEDS BOTH THE FETCH AND THE PROBE, DELIBERATELY. A file fetched
// but never imported ships unverified; a module imported but never fetched makes
// the install fail on every instance. Splitting them is how a probe silently
// stops covering what the install delivers —
// TestCairnFetchAndProbeCoverTheSameModules pins that both sides are generated
// from this one slice.
//
// ⚠ AND THAT COUPLING IS EXACTLY WHY IT CANNOT CATCH A MISSING ENTRY. Deleting an
// element removes it from the fetch AND from the probe, so the install reports
// success for a client missing a module. TestCairnRevAndLibModulesAreOneLedger is
// the guard for that half, against an independently-spelled ledger.
var cairnLibModules = []string{
	"cairn_doctor",
	"cairn_instances",
	"entry_shape",
	"env_aliases",
	"host_identity",
	"subsystem_read_store",
	"subsystem_recall",
	"subsystem_resolver",
	"timeouts",
}

// CairnConfigured reports whether a USABLE store credential exists: BOTH
// coordinates, non-blank.
//
// 🔴 IT IS EXPORTED BECAUSE THE CALLER NEEDS THE SAME ANSWER. The instance's spec
// and the instance's PROSE are built in two places, and they must not be able to
// disagree about whether the agent holds a credential — a prompt that describes a
// read+write key the pod does not carry makes the agent report a working
// subsystem as broken. One predicate, read by both, is what makes that
// impossible rather than merely unlikely.
func (c Config) CairnConfigured() bool {
	return strings.TrimSpace(c.CairnURL) != "" && strings.TrimSpace(c.CairnToken) != ""
}

// cairnEnabled is the whole gate: the caller declared this agent eligible AND the
// installation has a usable credential.
func cairnEnabled(cfg Config, opts Options) bool {
	return opts.CairnEligible && cfg.CairnConfigured()
}

// cairnClientDir is where the client's files live inside the instance.
func cairnClientDir(workspace string) string { return path.Join(workspace, cairnDirName) }

// cairnStageDir is where a fetch is staged.
func cairnStageDir(workspace string) string { return path.Join(workspace, cairnStageDirName) }

// cairnEntrypoint is the python script the wrapper execs.
func cairnEntrypoint(workspace string) string { return path.Join(cairnClientDir(workspace), "cairn") }

// checkCairnWorkspace refuses a workspace path this file cannot safely
// interpolate.
//
// 🔴 THE PATH REACHES A SHELL *AND* A PYTHON STRING LITERAL. The install entry is
// a [provision.Spec.Init] string — the least portable field in the spec by its
// own doc — and the import probe embeds the same path inside
// `sys.path.insert(0,'…')`. A quote, a dollar or a backtick would break out of
// one or both, so it is refused at build time where the caller can see it rather
// than at container start where it reads as a broken image.
//
// ⚠ IT IS NOT A GENERAL PATH VALIDATOR and must not grow into one. It refuses the
// characters THESE two interpolations cannot survive; everything else about a
// legal absolute path is the driver's business.
func checkCairnWorkspace(workspace string) error {
	const unsafe = "'\"`$\\ \t\n"
	if i := strings.IndexAny(workspace, unsafe); i >= 0 {
		return fmt.Errorf("agentspec: the cairn install cannot be built for workspace %q: it contains %q, "+
			"which this package interpolates into both a shell command and a python string literal "+
			"(set Config.WorkspacePath to a path without quotes, whitespace, `$`, a backslash or a backtick, "+
			"or leave Config.CairnURL/CairnToken empty)", workspace, workspace[i:i+1])
	}
	return nil
}

// cairnWrapperFile is the on-PATH entrypoint, as data.
//
// 🔴 IT IS A FILE AND NOT A SHELL WRITE, AND THAT IS WHAT MAKES THE WHOLE
// INSTALL WORK AT ALL. See item 1 of this file's header: an Init step's writes to
// the container filesystem do not survive into the application container, so the
// command on PATH cannot be created by the install script. Placed as a file, the
// driver puts it in both containers — which also means the install's last probe
// exercises the real wrapper rather than a staged copy.
//
// ⚠ IT INVOKES python3 EXPLICITLY RATHER THAN RELYING ON THE UPSTREAM SHEBANG. A
// raw-content fetch delivers neither a reliable mode bit nor an interpreter
// resolution, and `exec python3 <path>` leaves
// `Path(__file__).resolve().parent` pointing at the install directory so the
// client's own `lib/` sibling still resolves.
func cairnWrapperFile(workspace string) provision.File {
	return provision.File{
		Path:    cairnWrapperPath,
		Mode:    cairnWrapperMode,
		Content: []byte("#!/bin/sh\nexec python3 " + cairnEntrypoint(workspace) + " \"$@\"\n"),
	}
}

// cairnCredentialFile is the store credential, as a confidential file.
//
// The two keys are the spellings the client at [cairnRev] reads; see this file's
// header for why the pre-rename aliases are not written.
func cairnCredentialFile(cfg Config) provision.File {
	return provision.File{
		Path:    CairnConfigPath,
		Mode:    cairnCredentialMode,
		Secret:  true,
		Content: []byte("CAIRN_URL=" + strings.TrimSpace(cfg.CairnURL) + "\nCAIRN_TOKEN=" + strings.TrimSpace(cfg.CairnToken) + "\n"),
	}
}

// cairnInstallCommand is the single [provision.Spec.Init] entry that installs the
// client.
//
// It is a function rather than an inline append so a guard can assert on the
// ENTRY: entries are joined with "\n" before a shell sees them, which makes a
// line appended after this one indistinguishable from a line inside it when you
// only look at the joined string.
//
// # 🔴 THE WHOLE FETCH IS UNDER ONE WALL-CLOCK BOUND, AND THAT IS NOT HARDENING
//
// `--connect-timeout` bounds only the CONNECT; a server that answers 200 and then
// trickles is the dangerous regime, and ten sequential `--max-time 20` fetches
// would cost 200 s of startup with nothing able to stop them. A blackholed host
// and an HTTP error are both fast, which is exactly why the slow case hides. The
// outer `timeout` is the bound that makes the guarantee; the inner `--max-time`
// is a softer one so a single stalled file cannot spend the whole loop budget.
//
// Worst case, ARITHMETIC — TestTheCairnInstallIsBoundedInWallClock recomputes it
// from the rendered string so it cannot drift into prose only:
//
//	fetch loop   timeout -k 5 60   ->  60 + 5 = 65 s   (ONE bound, any file count)
//	staged --help          -k 1 10 ->  10 + 1 = 11 s
//	staged import probe    -k 1 10 ->  10 + 1 = 11 s
//	on-PATH --help         -k 1 10 ->  10 + 1 = 11 s
//	                                   ------------
//	                                        98 s
//
// ⚠ THERE IS NO PROBE BUDGET TO MEASURE THAT AGAINST, AND THE UPSTREAM ONE DOES
// NOT TRANSFER. Upstream derived a 305 s ceiling from a Helm chart's
// startupProbe. muster's kubernetes driver renders NO probes and sets no
// activeDeadlineSeconds (measured: the strings do not appear in
// internal/provision/k8s), so the only real ceiling is a Deployment's
// progressDeadlineSeconds, whose kubernetes default is 600 s and which nothing
// here sets. The guard therefore pins a STATED budget, and says so rather than
// dressing it as a measurement.
//
// # NO HEREDOC, AND NO MULTI-LINE `python3 -c`
//
// ⚠ IT IS A STYLE PREFERENCE HERE, AND ITS GUARD HAS BEEN DELETED. Upstream had a
// measured reason — its chart re-indented every line of the value, so a heredoc
// terminator never matched its opener. muster's driver joins entries with "\n" and
// hands them to `sh -eu -c`, where a heredoc would in fact survive, so the reason
// does NOT transfer. A test asserting the absence was therefore the workaround
// imported without the problem — the mistake item 2 of this header warns about,
// applied to a test — and it is gone rather than kept as an "invariant guard".
// The entry is still written without either construct, because HOW Init is
// rendered belongs to the driver and [provision.Spec.Init]'s own doc calls itself
// a shell contract and the least portable field in the spec.
//
// THE CLOSING CONDITION FOR RE-ADDING THE GUARD is the first driver that renders
// [provision.Spec.Init] BY A DIFFERENT JOINING RULE than "\n" into `sh -eu -c`. At
// that point the constraint is real here rather than inherited, and the person who
// checks it is the reviewer of whichever change adds that driver.
//
// # DOWNLOAD -> VERIFY -> MOVE -> RE-VERIFY ON PATH
//
// The client must IMPORT before it is reachable, which is what catches a
// truncated body, a missing module or a revision whose layout moved.
//
// ⚠ AN EARLIER VERSION OF THIS PARAGRAPH JUSTIFIED THE DESIGN ON A FALSE PROPERTY
// OF THE TOOL, and the correction is recorded rather than quietly swapped because
// the same false sentence ALSO shipped to the agent — see [supervisorCairnSection],
// corrected there too. It read: "A `--version` style probe cannot do this job:
// `cairn --version` exits 0 printing usage, exactly like a verb that does not
// exist." Measured at [cairnRev]: there is no `--version` option anywhere in the
// client, and the top-level parser declares its subcommand REQUIRED, so
// `cairn --version` exits 2 with an argparse usage error. A bogus verb also exits
// 2. What was measured, stated as measured and no further: the two share an EXIT
// STATUS and differ in their OUTPUT — the last line of the usage error reads
// "the following arguments are required: cmd" for the first and
// "argument cmd: invalid choice" for the second. That is no argument at all
// against an exit-code probe, because `--help` is the invocation that exits 0 when
// the client is importable and runnable. Probe 1 is exactly such a probe, and it
// works.
//
// ⚠ THIS PARAGRAPH HAS NOW BEEN WRITTEN THREE TIMES. The version before this one
// claimed the two were "indistinguishable by their OUTPUT, not by their exit
// status", which is the opposite of both halves of the measurement above. The
// commit message and the agent-facing prose were right; only this comment was
// wrong. The correction here is a restatement of what the two invocations printed
// and exited, not a fresh argument — a fresh argument is what produced the error
// twice.
//
// 🔴 SO PROBE 2, THE IMPORT PROBE, HAS NO INDEPENDENT REASON AT THIS REVISION, AND
// THAT IS WRITTEN DOWN RATHER THAN REPLACED WITH A FRESH ARGUMENT. Measured two
// ways at [cairnRev] — `python3 -X importtime` over probe 1's exact command, and
// sys.modules after it — `cairn --help` imports ALL NINE of [cairnLibModules].
// Eight are top-level imports of the entrypoint; the ninth, cairn_doctor, sits
// inside a function whose own docstring states it is NOT lazy in effect, because
// the parser builder calls it to render help text on every invocation. Every
// module probe 2 imports, probe 1 has therefore already imported, and probe 1's
// exit status is checked under `sh -eu`.
//
// It is RETAINED rather than deleted for one narrow and conditional reason, stated
// as conditional on purpose: it imports the LEDGER's names directly, so it would
// catch an element of [cairnLibModules] that the entrypoint's own startup path
// does not import. No such element exists at this revision, so today it verifies
// nothing probe 1 does not. THE CLOSING CONDITION FOR DELETING IT is a [cairnRev]
// bump after which probe 1 is re-measured (`python3 -X importtime <client> --help`)
// and still imports every element of the ledger; the person who checks it is the
// reviewer of that bump. Its 11 s of the stated 98 s budget is what buys the delay.
//
// 🔴 THE THIRD PROBE IS NOT REDUNDANT, and nothing above touches its reason. The
// staged probes run a python file by path; the last one runs the real command on
// PATH, which is the only one that covers the wrapper, its mode bit, and the
// resolved `lib/` sibling together. Upstream added it after a run that printed
// "installed" while the wrapper write had failed — a success echo that was a claim
// about the DOWNLOAD, not about the command the agent would invoke.
func cairnInstallCommand(workspace string) (string, error) {
	if err := checkCairnWorkspace(workspace); err != nil {
		return "", err
	}
	if len(cairnLibModules) == 0 {
		// Unreachable with the ledger above, and stated anyway: an empty list
		// would render a fetch of the entrypoint alone and an import probe that
		// imports nothing, i.e. an install that verifies itself vacuously.
		return "", fmt.Errorf("agentspec: cairnLibModules is empty, so the install would fetch no modules and its import probe would verify nothing")
	}

	files := "cairn"
	imports := ""
	for i, m := range cairnLibModules {
		files += " lib/" + m + ".py"
		if i > 0 {
			imports += ","
		}
		imports += m
	}

	stage := cairnStageDir(workspace)
	dir := cairnClientDir(workspace)
	return "rm -rf " + stage + "\n" +
		"mkdir -p " + stage + "/lib\n" +
		"timeout -k 5 60 sh -c 'for f in " + files + "; do curl -sf --connect-timeout 5 --max-time 20 \"" + cairnSource + cairnRev + "/$f\" -o \"" + stage + "/$f\" || exit 1; done'\n" +
		"timeout -k 1 10 python3 " + stage + "/cairn --help >/dev/null 2>&1\n" +
		"timeout -k 1 10 python3 -c \"import sys;sys.path.insert(0,'" + stage + "/lib');import " + imports + ";print('" + cairnProbeSentinel + "')\" 2>/dev/null | grep -q '^" + cairnProbeSentinel + "$'\n" +
		"rm -rf " + dir + "\n" +
		"mv " + stage + " " + dir + "\n" +
		"timeout -k 1 10 " + cairnWrapperPath + " --help >/dev/null 2>&1\n" +
		"echo \"cairn installed: " + cairnWrapperPath + " -> " + dir + " @ " + cairnRev + "\"", nil
}
