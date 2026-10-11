package agentspec

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// The autosave daemon is a shell program, so these tests EXECUTE it — against
// real git repositories with a real bare remote — rather than asserting on its
// text. The script under test is written out from the embedded `autosaveScript`
// variable, so what runs here is byte-for-byte what ships into the agent pod.
//
// The shell is configurable because the two ends differ and the difference is
// load-bearing: this host's /bin/sh is bash, while the agent image's is
// Debian-based and its /bin/sh is dash. The suite is run at BOTH points (see the
// PR body); MUSTER_TEST_SH selects the interpreter.
func testShell() string {
	if sh := os.Getenv("MUSTER_TEST_SH"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture Author", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture Author", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitTry(dir string, args ...string) (string, bool) {
	out, err := gitRun(dir, args...)
	return out, err == nil
}

// autosaveFixture is a bare "origin" plus a working clone standing in for the
// agent's /data/repos/<name>, seeded with one commit.
type autosaveFixture struct {
	root   string
	origin string
	work   string
	script string
}

func newAutosaveFixture(t *testing.T) *autosaveFixture {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "workspace")

	gitCmd(t, root, "init", "--bare", "-b", "main", origin)
	gitCmd(t, root, "clone", "--quiet", origin, work)

	// Pairwise-distinct filenames and contents so a mutant that pushes the wrong
	// tree, parent or file cannot produce output identical to the correct code.
	writeFile(t, work, "README.md", "seed-readme-content\n")
	writeFile(t, work, ".gitignore", "ignored-secrets.env\nbuild/\nconfig.env\n")
	gitCmd(t, work, "add", "README.md", ".gitignore")
	gitCmd(t, work, "commit", "-m", "seed commit")
	gitCmd(t, work, "push", "--quiet", "origin", "main")

	script := filepath.Join(root, "muster-autosave.sh")
	if err := os.WriteFile(script, []byte(autosaveScript), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return &autosaveFixture{root: root, origin: origin, work: work, script: script}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func (f *autosaveFixture) baseEnv(ref string) []string {
	return append(os.Environ(),
		"MUSTER_WIP_REPO="+f.work,
		"MUSTER_WIP_REF="+ref,
		// ⚠ The NAME "origin", exactly as production passes it — not a filesystem
		// path. The two are not equivalent: a named remote makes git update
		// refs/remotes/origin/… after a successful push, which fires
		// `reference-transaction` locally. With a path remote it does not, so 14 of
		// the 15 push-path tests were exercising a shape production never uses and
		// were structurally blind to that hook. TestAutosaveHonoursGitignore keeps
		// the URL form, which the MUSTER_WIP_REMOTE knob also accepts.
		"MUSTER_WIP_REMOTE=origin",
		// 🔴 THE EXCLUDE LIST IS NOW PASSED IN, NOT BAKED INTO THE SCRIPT, and the
		// fixture must pass exactly what production passes or every exclude test
		// below is exercising a shape production never uses. That is not a
		// hypothetical: the upstream tests read the list out of the script, so a
		// fixture that forgot to set this would find an EMPTY exclude list and the
		// behavioural test would pass vacuously — nothing excluded, nothing
		// asserted. autosaveExcludeEnvValue() is the production value.
		"MUSTER_WIP_EXCLUDE="+mustExcludeEnv(testExcludePatterns),
		"MUSTER_WIP_INTERVAL=0",
		"MUSTER_WIP_LOG=-", // keep diagnostics on stdout for assertions
		"MUSTER_API_URL=", "MUSTER_HOOK_TOKEN=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// run executes the daemon for a bounded number of cycles.
func (f *autosaveFixture) run(t *testing.T, ref string, cycles int, extraEnv ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(testShell(), f.script)
	cmd.Dir = f.root
	cmd.Env = append(f.baseEnv(ref), "MUSTER_WIP_MAX_CYCLES="+strconv.Itoa(cycles))
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func (f *autosaveFixture) remoteFileAt(ref, path string) (string, bool) {
	return gitTry(f.origin, "show", ref+":"+path)
}

func (f *autosaveFixture) remoteRef(ref string) string {
	out, ok := gitTry(f.origin, "rev-parse", "--verify", ref)
	if !ok {
		return ""
	}
	return out
}

const testRef = "refs/heads/muster-wip/bold-moth-42"

// --- hooked repositories -------------------------------------------------
//
// 🔴 THE BLIND SPOT THAT SHIPPED THE DEFECT. Every autosave test and both live
// validation runs used a sandbox repository with NO hooks, which has NO hooks — so
// nothing ever exercised the case the daemon actually meets in production. Agent
// `bold-otter` (#49) was dispatched at civitai/civitai, which ships
// `.husky/pre-push` and sets `core.hooksPath = .husky/_`; every snapshot push ran
// a full typecheck, failed, and left `git ls-remote origin
// 'refs/heads/muster-wip/*'` with zero refs. Durability was off on the exact
// repository whose incident motivated the daemon.
//
// The two shapes below are DIFFERENT CODE PATHS in git, not two spellings of one
// thing, and civitai uses the second — so both are exercised everywhere hooks
// matter.

// hookShape installs a client-side hook the way some class of real repository
// does.
type hookShape struct {
	name    string
	install func(t *testing.T, f *autosaveFixture, hookName, body string)
}

var hookShapes = []hookShape{
	{
		// The plain case: an executable in the repo's own .git/hooks.
		name: "classic .git/hooks",
		install: func(t *testing.T, f *autosaveFixture, hookName, body string) {
			t.Helper()
			dir := filepath.Join(f.work, ".git", "hooks")
			writeFile(t, dir, hookName, body)
			if err := os.Chmod(filepath.Join(dir, hookName), 0o755); err != nil {
				t.Fatalf("chmod %s: %v", hookName, err)
			}
		},
	},
	{
		// civitai/civitai's shape: husky redirects hook lookup out of .git.
		name: "core.hooksPath husky",
		install: func(t *testing.T, f *autosaveFixture, hookName, body string) {
			t.Helper()
			dir := filepath.Join(f.work, ".husky", "_")
			writeFile(t, dir, hookName, body)
			if err := os.Chmod(filepath.Join(dir, hookName), 0o755); err != nil {
				t.Fatalf("chmod %s: %v", hookName, err)
			}
			gitCmd(t, f.work, "config", "core.hooksPath", ".husky/_")
		},
	},
}

// hookBody writes a SENTINEL FILE and then behaves like the real thing. The
// sentinel is the whole point: a hook that runs and passes satisfies an
// exit-code-only assertion, so "the push succeeded" proves nothing about whether
// third-party code executed. Only the absence of this file does.
func hookBody(sentinel, marker string, exitCode int) string {
	return "#!/bin/sh\n" +
		"echo " + marker + " >> " + sentinel + "\n" +
		"echo 'Running typecheck for all files'\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
}

func hookRan(t *testing.T, sentinel string) bool {
	t.Helper()
	_, err := os.Stat(sentinel)
	return err == nil
}

// TestAutosaveNeverRunsTheTargetRepoPrePushHook is the regression test for the
// live defect. RED at 5e7c2b03 (the merged autosave commit) in both shapes: the
// hook runs, writes its sentinel, and blocks the push so nothing reaches the
// remote.
//
// It carries its own POSITIVE CONTROL. Without one, a mis-installed hook would
// make the test pass for the wrong reason — an absent sentinel is exactly what a
// hook wired to nothing produces. The control push at the end must make the
// sentinel appear.
func TestAutosaveNeverRunsTheTargetRepoPrePushHook(t *testing.T) {
	for _, shape := range hookShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAutosaveFixture(t)
			sentinel := filepath.Join(f.root, "pre-push-ran")
			shape.install(t, f, "pre-push", hookBody(sentinel, "PRE-PUSH-HOOK-RAN", 1))

			writeFile(t, f.work, "rescue-me.txt", "work that must reach the remote\n")

			out, ok := f.run(t, testRef, 1)
			if !ok {
				t.Fatalf("daemon exited non-zero:\n%s", out)
			}

			// (1) The hook must not have EXECUTED. Not "the push exited 0".
			if hookRan(t, sentinel) {
				t.Errorf("the target repository's pre-push hook RAN. A machine-generated "+
					"snapshot must never execute a third party's verification — on "+
					"civitai/civitai this is a full typecheck, burned every cycle inside the "+
					"agent pod:\n%s", out)
			}
			// (2) And durability actually happened.
			if f.remoteRef(testRef) == "" {
				t.Fatalf("snapshot ref %s does not exist on the remote; the repo's hook blocked "+
					"durability entirely:\n%s", testRef, out)
			}
			if got, exists := f.remoteFileAt(testRef, "rescue-me.txt"); !exists ||
				!strings.Contains(got, "work that must reach the remote") {
				t.Errorf("snapshot did not carry the work: %q (exists=%v)", got, exists)
			}
			// (3) The hook's stdout must not be masquerading as a git diagnostic.
			if strings.Contains(out, "Running typecheck") {
				t.Errorf("hook output leaked into the daemon's diagnostics:\n%s", out)
			}

			// POSITIVE CONTROL: prove this fixture's hook CAN fire, so the assertions
			// above are not measuring a hook that was never wired up.
			head := gitCmd(t, f.work, "rev-parse", "HEAD")
			if _, ok := gitTry(f.work, "push", "--force", f.origin, head+":refs/heads/control-probe"); ok {
				t.Fatalf("control push SUCCEEDED past a hook that exits 1 — the %s fixture "+
					"installed no live hook, so this test proves nothing", shape.name)
			}
			if !hookRan(t, sentinel) {
				t.Fatalf("control push did not fire the hook either — the %s fixture is inert "+
					"and every assertion above passed vacuously", shape.name)
			}
		})
	}
}

// TestAutosaveNeverRunsTheTargetRepoReferenceTransactionHook covers the half of
// the hazard that `--no-verify` PROVABLY CANNOT REACH, and is therefore what
// makes the core.hooksPath override independently necessary rather than
// belt-and-braces.
//
// Measured: `git update-ref` fires `reference-transaction` (3x on git 2.55,
// 2x on the agent image's git 2.39.5 — the count is version-dependent, the fact
// is not), and `git push` to a NAMED remote fires it again locally when it
// updates refs/remotes/<remote>/…. `--no-verify` suppresses neither. The daemon
// runs both commands on every changed cycle.
//
// The fixture's default remote is now the NAME "origin" (see baseEnv), which is
// what makes the push half of this reachable at all.
//
// The hook exits 0 here on purpose: the assertion is "did third-party code
// execute", not "did something break", so a green cannot come from an unrelated
// failure. RED at 5e7c2b03.
func TestAutosaveNeverRunsTheTargetRepoReferenceTransactionHook(t *testing.T) {
	for _, shape := range hookShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAutosaveFixture(t)
			sentinel := filepath.Join(f.root, "reftx-ran")
			shape.install(t, f, "reference-transaction", hookBody(sentinel, "REFTX-HOOK-RAN", 0))

			writeFile(t, f.work, "work.txt", "changes the tree\n")

			// baseEnv already uses the NAMED remote, as production does — that is what
			// makes the push update a remote-tracking ref and fire the hook a second time.
			out, ok := f.run(t, testRef, 1)
			if !ok {
				t.Fatalf("daemon exited non-zero:\n%s", out)
			}
			if f.remoteRef(testRef) == "" {
				t.Fatalf("no snapshot pushed, so the run proves nothing about hooks:\n%s", out)
			}
			if hookRan(t, sentinel) {
				body, _ := os.ReadFile(sentinel)
				t.Errorf("the target repository's reference-transaction hook RAN (%q). "+
					"--no-verify does not cover this path; only overriding core.hooksPath "+
					"does:\n%s", strings.TrimSpace(string(body)), out)
			}

			// POSITIVE CONTROL: the same hook must fire for an ordinary ref update.
			head := gitCmd(t, f.work, "rev-parse", "HEAD")
			gitCmd(t, f.work, "update-ref", "refs/control/probe", head)
			if !hookRan(t, sentinel) {
				t.Fatalf("control update-ref did not fire the %s reference-transaction hook — "+
					"the fixture is inert and the assertion above passed vacuously", shape.name)
			}
		})
	}
}

// --- the incident ---

func TestAutosavePushesUncommittedWorkOffPod(t *testing.T) {
	f := newAutosaveFixture(t)

	// The shape of bold-moth's loss: a modified tracked file, a brand-new
	// untracked file, and a new file in a new directory — none of it committed.
	writeFile(t, f.work, "README.md", "modified-by-agent\n")
	writeFile(t, f.work, "src/endpoint-helpers.ts", "export const fixed = true;\n")
	writeFile(t, f.work, "notes/diagnosis.md", "root cause: error envelope drops message\n")

	out, ok := f.run(t, testRef, 1)
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if f.remoteRef(testRef) == "" {
		t.Fatalf("snapshot ref %s does not exist on the remote; work was NOT made durable:\n%s", testRef, out)
	}
	for _, tc := range []struct{ path, want string }{
		{"README.md", "modified-by-agent"},
		{"src/endpoint-helpers.ts", "export const fixed = true;"},
		{"notes/diagnosis.md", "root cause: error envelope drops message"},
	} {
		got, exists := f.remoteFileAt(testRef, tc.path)
		if !exists {
			t.Errorf("%s missing from the pushed snapshot", tc.path)
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s pushed with wrong content: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestAutosavePreservesTrackedButIgnoredFiles is the regression test for review
// finding #4. A file that is BOTH tracked and matched by .gitignore (force-added,
// which real repos do for a committed config.env) was silently dropped from every
// snapshot and recorded as DELETED, because the private index was built empty so
// `git add -A` saw it as merely ignored. Recovering such a snapshot by merge would
// have deleted the file. The fix is `git read-tree HEAD` before the add.
func TestAutosavePreservesTrackedButIgnoredFiles(t *testing.T) {
	f := newAutosaveFixture(t)

	// config.env is in .gitignore (see the fixture) yet force-added and committed.
	writeFile(t, f.work, "config.env", "ORIGINAL=committed-value\n")
	gitCmd(t, f.work, "add", "-f", "config.env")
	gitCmd(t, f.work, "commit", "-m", "track a gitignored config")
	gitCmd(t, f.work, "push", "--quiet", "origin", "main")

	// The agent edits it — exactly the work that must not be lost.
	writeFile(t, f.work, "config.env", "EDITED=by-the-agent\n")
	writeFile(t, f.work, "other.txt", "forces a new tree\n")

	out, ok := f.run(t, testRef, 1)
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	got, exists := f.remoteFileAt(testRef, "config.env")
	if !exists {
		t.Fatalf("tracked-but-gitignored config.env was DROPPED from the snapshot; " +
			"the agent's edits are lost and the snapshot records the file as deleted")
	}
	if !strings.Contains(got, "EDITED=by-the-agent") {
		t.Errorf("config.env snapshotted with stale content: %q", got)
	}
	// And it must not show as a deletion against HEAD.
	diff, _ := gitTry(f.origin, "diff", "--name-status", "HEAD", testRef)
	if strings.Contains(diff, "D\tconfig.env") {
		t.Errorf("snapshot records config.env as DELETED:\n%s", diff)
	}
}

// --- staying out of the agent's way ---

func TestAutosaveLeavesAgentGitStateUntouched(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "README.md", "agent-edit-in-progress\n")
	writeFile(t, f.work, "untracked.txt", "scratch\n")

	headBefore := gitCmd(t, f.work, "rev-parse", "HEAD")
	branchBefore := gitCmd(t, f.work, "rev-parse", "--abbrev-ref", "HEAD")
	statusBefore := gitCmd(t, f.work, "status", "--porcelain")
	stagedBefore := gitCmd(t, f.work, "diff", "--cached", "--name-only")

	if out, ok := f.run(t, testRef, 1); !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if got := gitCmd(t, f.work, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := gitCmd(t, f.work, "rev-parse", "--abbrev-ref", "HEAD"); got != branchBefore {
		t.Errorf("branch switched: %s -> %s", branchBefore, got)
	}
	if got := gitCmd(t, f.work, "status", "--porcelain"); got != statusBefore {
		t.Errorf("working tree status changed:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	if got := gitCmd(t, f.work, "diff", "--cached", "--name-only"); got != stagedBefore {
		t.Errorf("daemon staged files into the agent's index: before %q, after %q", stagedBefore, got)
	}
}

// testExcludePatterns is a SYNTHETIC agent-runtime file list for the fixtures.
//
// 🔴 IT IS SYNTHETIC BECAUSE THERE IS NO LONGER A REAL DEFAULT TO READ, and that is
// the point rather than a workaround: the exclude list is the caller's
// (Config.AutosaveExcludePatterns), so a test that wanted "the production list"
// would be asserting a policy this package does not hold. What these fixtures pin is
// that the daemon HONOURS whatever it is handed — which is the only claim about the
// script that survives the list becoming a parameter.
//
// The names are deliberately unlike any real runtime's, and one is a directory, so
// the anchored/unanchored probe below has both shapes to work with.
var testExcludePatterns = []string{
	"/RUNTIME-SOUL.md",
	"/RUNTIME-IDENTITY.md",
	"/RUNTIME-TOOLS.md",
	"/RUNTIME-HEARTBEAT.md",
	"/RUNTIME-USER.md",
	"/RUNTIME-AGENTS.md",
	"/.runtime-state/",
}

// mustExcludeEnv renders patterns for a fixture, failing loudly rather than
// silently producing an empty list — an empty MUSTER_WIP_EXCLUDE would make every
// exclude assertion below pass vacuously.
func mustExcludeEnv(patterns []string) string {
	v, err := autosaveExcludeEnvValue(patterns)
	if err != nil {
		panic(err)
	}
	if v == "" {
		panic("mustExcludeEnv: empty value; exclude fixtures would assert nothing")
	}
	return v
}

// 🔴 THE UPSTREAM EXTRACTOR IS GONE ON PURPOSE, AND ITS ABSENCE IS THE POINT.
// It read the exclude ledger OUT OF THE SCRIPT with a regexp
// (`^\s*echo "(/[^"]*)"\s*$`) because install_excludes hardcoded seven `echo`
// lines. This port moved the list into Go so a different agent runtime can supply
// its own, so there is nothing in the script left to extract — the regexp would
// match ZERO lines and every fixture derived from it would silently test nothing.
// Deleting it is therefore not tidying: a surviving extractor returning an empty
// list is exactly the vacuous-green shape these tests exist to prevent.
//
// The ledger is now the CALLER's (Config.AutosaveExcludePatterns); this file's own
// testExcludePatterns stands in for one, pinned by
// TestAutosaveExcludeLedgerIsPinned below, and the SCRIPT's own job — honouring
// whatever it is handed — is pinned separately by
// TestTheScriptExcludesWhateverItIsGivenRatherThanAHardcodedList.

// excludePatternsWrittenByTheScript reads back what the daemon actually appended
// to .git/info/exclude, which is the only claim about the script's behaviour that
// survives the list becoming a parameter.
func excludePatternsWrittenByTheScript(t *testing.T, gitdir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(gitdir, "info", "exclude"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// excludeProbe turns one exclude pattern into the two paths that pin it: a ROOT
// copy the daemon must drop, and a NESTED copy of the same name the daemon must
// keep (the anchor).
type excludeProbe struct{ pattern, root, nested string }

func excludeProbes(patterns []string) []excludeProbe {
	probes := make([]excludeProbe, 0, len(patterns))
	for _, p := range patterns {
		name := strings.TrimPrefix(p, "/")
		if strings.HasSuffix(name, "/") { // a directory pattern, e.g. /.runtime-state/
			probes = append(probes, excludeProbe{p, name + "state.db", "vendor/" + name + "state.db"})
			continue
		}
		probes = append(probes, excludeProbe{p, name, "docs/" + name})
	}
	return probes
}

// TestAutosaveExcludeLedgerIsPinned is the DRIFT GUARD between the ledger in
// autosave.go and the fixtures below. The behavioural test derives its files from the script,
// so an 8th entry would be exercised automatically — but silently, with nobody
// having decided it belongs there. This makes any change to the list a conscious
// one.
func TestAutosaveExcludeLedgerIsPinned(t *testing.T) {
	want := []string{
		"/RUNTIME-SOUL.md", "/RUNTIME-IDENTITY.md", "/RUNTIME-TOOLS.md",
		"/RUNTIME-HEARTBEAT.md", "/RUNTIME-USER.md", "/RUNTIME-AGENTS.md", "/.runtime-state/",
	}
	got := testExcludePatterns
	if len(got) == 0 {
		t.Fatal("the exclude ledger is empty, so every fixture derived from it is testing nothing")
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
		if !wantSet[g] {
			t.Errorf("the exclude ledger gained %q with no decision recorded here; a new entry "+
				"suppresses a path in every third-party repo the fleet touches", g)
		}
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("the exclude ledger lost %q; that agent-runtime file will now be "+
				"force-pushed into someone else's repository", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("exclude ledger size %d != expected %d: %v", len(got), len(want), got)
	}
}

// TestAutosaveExcludesOpenclawRuntimeFiles: for a repo-backed agent the workspace
// IS the clone, so the agent runtime's own files sit untracked inside a third-party
// repo. USER.md is titled "About Your Human". They must never be force-pushed to
// a branch on someone else's repository — but a repo that genuinely TRACKS a file
// of the same name must be unaffected.
//
// ⚠ TWO PHASES, AND THEY NEED TWO REPO STATES. Git excludes apply only to
// UNTRACKED paths, so a name that the fixture tracks has NO observable exclude
// behaviour. The earlier single-phase version tracked root TOOLS.md, which is why
// `/TOOLS.md` could be DELETED from install_excludes with the suite green — six
// of seven entries were guarded against deletion and that one was guarded only
// against un-anchoring. One repo state cannot pin both halves; hence two.
func TestAutosaveExcludesOpenclawRuntimeFiles(t *testing.T) {
	probes := excludeProbes(testExcludePatterns)
	if len(probes) < 7 {
		t.Fatalf("only %d exclude entries extracted from the script; the fixture is not "+
			"covering the real ledger: %+v", len(probes), probes)
	}

	// Phase 1 — every entry UNTRACKED at the root. This is the phase that makes
	// deleting any entry observable, TOOLS.md included.
	t.Run("untracked runtime files are dropped, nested namesakes are kept", func(t *testing.T) {
		f := newAutosaveFixture(t)
		for _, p := range probes {
			writeFile(t, f.work, p.root, "agent runtime state: "+p.root+"\n")
			writeFile(t, f.work, p.nested, "the repo's own nested "+p.nested+"\n")
		}
		writeFile(t, f.work, "real-work.txt", "the actual change\n")

		if out, ok := f.run(t, testRef, 1); !ok {
			t.Fatalf("daemon exited non-zero:\n%s", out)
		}
		for _, p := range probes {
			if got, exists := f.remoteFileAt(testRef, p.root); exists {
				t.Errorf("%s is in install_excludes but %s was still pushed into the "+
					"third-party repo snapshot: %q", p.pattern, p.root, got)
			}
			got, exists := f.remoteFileAt(testRef, p.nested)
			if !exists {
				t.Errorf("%s was wrongly excluded — the exclude %s is not anchored to the "+
					"repo root, so it swallows the repository's own file of that name at any depth",
					p.nested, p.pattern)
				continue
			}
			if !strings.Contains(got, "the repo's own nested "+p.nested) {
				t.Errorf("%s snapshotted with wrong content: %q", p.nested, got)
			}
		}
		if _, exists := f.remoteFileAt(testRef, "real-work.txt"); !exists {
			t.Error("real-work.txt was wrongly excluded from the snapshot")
		}
	})

	// Phase 2 — the same names, TRACKED. The excludes must not over-reach: a repo
	// that genuinely commits a root TOOLS.md (or AGENTS.md) keeps it, and its
	// content must not go stale or vanish.
	t.Run("a repo's own tracked file of the same name survives", func(t *testing.T) {
		f := newAutosaveFixture(t)
		var tracked []string
		for _, p := range probes {
			if strings.Contains(p.root, "/") { // skip the directory-pattern probe
				continue
			}
			writeFile(t, f.work, p.root, "the repo's own committed "+p.root+"\n")
			tracked = append(tracked, p.root)
		}
		gitCmd(t, f.work, append([]string{"add"}, tracked...)...)
		gitCmd(t, f.work, "commit", "-m", "repo tracks files that collide with runtime file names")
		gitCmd(t, f.work, "push", "--quiet", "origin", "main")

		// The agent then EDITS them — exactly the work that must not be lost.
		for _, name := range tracked {
			writeFile(t, f.work, name, "EDITED by the agent: "+name+"\n")
		}
		writeFile(t, f.work, "real-work.txt", "forces a new tree\n")

		if out, ok := f.run(t, testRef, 1); !ok {
			t.Fatalf("daemon exited non-zero:\n%s", out)
		}
		for _, name := range tracked {
			got, exists := f.remoteFileAt(testRef, name)
			if !exists {
				t.Errorf("the repo's TRACKED %s was dropped from the snapshot; recovering it "+
					"by merge would DELETE the file", name)
				continue
			}
			if !strings.Contains(got, "EDITED by the agent: "+name) {
				t.Errorf("tracked %s snapshotted with stale content: %q", name, got)
			}
		}
		diff, _ := gitTry(f.origin, "diff", "--name-status", "HEAD", testRef)
		for _, name := range tracked {
			if strings.Contains(diff, "D\t"+name) {
				t.Errorf("snapshot records tracked %s as DELETED:\n%s", name, diff)
			}
		}
		// The exclude list is local-only and must never become a committed change.
		if st := gitCmd(t, f.work, "status", "--porcelain", "--", ".gitignore"); st != "" {
			t.Errorf("daemon modified the repo's .gitignore: %q", st)
		}
	})
}

// gitCallSites returns every place the daemon invokes `git`, as (line number,
// first argument) pairs, ignoring comments and the word "git" appearing inside
// prose or in flags like --git-dir.
//
// It is a LEDGER, not a keyword search: it enumerates the call sites and reports
// what each one passes, so adding a call site is what fails the guard — a
// spelled check ("the script contains core.hooksPath") would stay green while a
// brand-new unprotected `git push` sat next to it.
var (
	shellAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	shellNumber = regexp.MustCompile(`^[0-9]+$`)
	// Words after which the next token starts a COMMAND. `if`/`while`/`until`/
	// `elif` were missing, which is how `if git …` — the most idiomatic shape in
	// this very script — went unparsed.
	shellCommandKeywords = map[string]bool{
		"(": true, "{": true, "}": true, "!": true, "&&": true, "||": true, ";": true,
		"if": true, "elif": true, "then": true, "while": true, "until": true,
		"do": true, "else": true, "exec": true, "env": true, "command": true,
		"eval": true, "time": true, "timeout": true, "nohup": true,
	}
)

type gitCall struct {
	Line int
	Args []string
}

// Subcommand is the porcelain/plumbing verb, skipping `-c key=value` style
// configuration and any shell variable standing in for it.
func (c gitCall) Subcommand() string {
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		if a == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(a, "$") || strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue
		}
		return a
	}
	return ""
}

// gitProseOnCodeLines enumerates the places a NON-COMMENT line legitimately
// contains the word "git" without invoking it. Each entry must contain exactly
// one such token. Adding a new one is deliberate friction: it is the only way to
// keep the cross-check below exact.
// Matched per LINE, and every `git` token on a matching line is attributed to
// prose. Deliberately a stable ANCHOR rather than the full message text: keyed on
// exact strings, any rewording of a diagnostic would fail this guard with a
// baffling "parser disagreement" instead of the wording test that should own it.
// (If a line ever carried both prose and a real invocation the counts would
// disagree and fail loudly — which is the correct outcome, not a silent miss.)
var gitProseOnCodeLines = []string{
	"is not a git repository",
	"printf 'git failed with exit",
}

// gitTokenCount is the INDEPENDENT counter behind the cross-check. It shares no
// logic with gitCallSites on purpose — a bug common to both would defeat the
// point. It counts whole-word `git` on code lines, rejecting --git-dir,
// .gitignore, gitdir= and GIT_INDEX_FILE by character class alone.
func gitTokenCount(script string) (total, prose int) {
	wordChar := func(b byte) bool {
		return b == '-' || b == '.' || b == '_' || b == '/' ||
			(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for _, line := range strings.Split(script, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		isProse := false
		for _, p := range gitProseOnCodeLines {
			if strings.Contains(line, p) {
				isProse = true
				break
			}
		}
		for i := 0; i+3 <= len(line); i++ {
			if line[i:i+3] != "git" {
				continue
			}
			if i > 0 && wordChar(line[i-1]) {
				continue
			}
			if i+3 < len(line) && wordChar(line[i+3]) {
				continue
			}
			total++
			if isProse {
				prose++
			}
		}
	}
	return total, prose
}

func gitCallSites(script string) []gitCall {
	var sites []gitCall
	for i, line := range strings.Split(script, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, loc := range regexp.MustCompile(`git\s`).FindAllStringIndex(line, -1) {
			start := loc[0]
			// `git` must be a whole word: reject --git-dir, .gitignore, gitdir=.
			if start > 0 {
				if c := line[start-1]; c != ' ' && c != '\t' && c != '(' && c != ';' && c != '|' && c != '&' {
					continue
				}
			}
			// …and must sit at a COMMAND position, so "is not a git repository"
			// inside a message string is not mistaken for an invocation.
			fields := strings.Fields(line[:start])
			if len(fields) > 0 {
				prev := fields[len(fields)-1]
				isEnvAssign := shellAssign.MatchString(prev)
				isPrefixVar := strings.HasPrefix(prev, "$")
				// A literal command prefix's numeric argument, e.g. `timeout 60 git …`.
				isNumericArg := shellNumber.MatchString(prev)
				isOperator := shellCommandKeywords[prev] ||
					strings.HasSuffix(prev, "$(") || strings.HasSuffix(prev, "(") ||
					strings.HasSuffix(prev, ";") || strings.HasSuffix(prev, "|") ||
					strings.HasSuffix(prev, "&") || strings.HasSuffix(prev, "`") ||
					strings.HasSuffix(prev, "{")
				if !isEnvAssign && !isPrefixVar && !isNumericArg && !isOperator {
					continue
				}
			}
			rest := strings.Fields(line[start+len("git"):])
			if len(rest) == 0 {
				continue
			}
			sites = append(sites, gitCall{Line: i + 1, Args: rest})
		}
	}
	return sites
}

// TestAutosaveNeutralisesHooksOnEveryGitInvocation is a STRUCTURAL LEDGER GUARD,
// and is labelled as one deliberately.
//
// The behavioural tests above pin the two hook paths that exist TODAY. This pins
// the RELATIONSHIP — "no git command this daemon runs can execute code from the
// target repository" — across every call site, including ones added later and
// ones git may grow new hooks for. It is red at 5e7c2b03 (all 13 sites bare), so
// it is not an invariant guard, but its value is forward-looking.
func TestAutosaveNeutralisesHooksOnEveryGitInvocation(t *testing.T) {
	sites := gitCallSites(autosaveScript)

	// 🔴 CROSS-CHECK BY A DIFFERENT METHOD, because the parser above is the
	// instrument and an under-counting parser reports a reassuring, meaningless
	// green. The old assertion was a FLOOR (`< 10` against 13 real sites), so up to
	// three call sites could have vanished — or been written in a shape the parser
	// does not understand — without a word. This counts whole-word `git` tokens by
	// a scan that shares no logic with gitCallSites and demands exact agreement,
	// so an unparsed shape fails LOUDLY instead of silently shrinking the ledger.
	total, prose := gitTokenCount(autosaveScript)
	if want := total - prose; len(sites) != want {
		t.Fatalf("parser disagreement: %d whole-word `git` tokens on code lines, %d of them "+
			"known prose, but only %d parsed as call sites. Some invocation is in a shape "+
			"gitCallSites does not understand, so it is NOT being checked for $nohooks.\n"+
			"parsed: %+v", total, prose, len(sites), sites)
	}
	if len(sites) < 10 {
		t.Fatalf("the ledger found only %d git call sites in a daemon that has many; the "+
			"scanner is broken and this guard is measuring nothing: %v", len(sites), sites)
	}
	for _, c := range sites {
		if c.Args[0] != "$nohooks" {
			t.Errorf("autosave.sh:%d invokes `git %s` without $nohooks (first arg %q). That "+
				"invocation can execute hook code from whatever repository the agent was "+
				"dispatched at.", c.Line, c.Subcommand(), c.Args[0])
		}
	}
}

// TestAutosavePushDeclaresNoVerify pins the SECOND layer on the push.
//
// ⚠ HONEST LABEL: this is the only guard that can catch removing `--no-verify`,
// because $nohooks already suppresses pre-push on its own — the two layers mask
// each other behaviourally, by design. `--no-verify` stays because it is the
// escape the hook files themselves document, it states the intent where a reader
// looks, and it is independent of core.hooksPath resolution. A behavioural test
// for it cannot exist while the stronger layer is present; this is the trade.
// Red at 5e7c2b03.
func TestAutosavePushDeclaresNoVerify(t *testing.T) {
	var pushLines []string
	for _, line := range strings.Split(autosaveScript, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "git $nohooks push") || regexp.MustCompile(`(^|\s)git\s+push`).MatchString(line) {
			pushLines = append(pushLines, trimmed)
		}
	}
	if len(pushLines) != 1 {
		t.Fatalf("expected exactly one `git push` invocation in the daemon, found %d: %v",
			len(pushLines), pushLines)
	}
	if !strings.Contains(pushLines[0], "--no-verify") {
		t.Errorf("the snapshot push does not pass --no-verify, the escape the hook files "+
			"themselves document: %q", pushLines[0])
	}
	if !strings.Contains(pushLines[0], "$nohooks") {
		t.Errorf("the snapshot push does not neutralise core.hooksPath, so a husky-style "+
			"repository's hooks still run: %q", pushLines[0])
	}
}

// --- noise control and retry ---

func TestAutosaveSkipsUnchangedTree(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "one-and-only-change\n")

	out, ok := f.run(t, testRef, 3)
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if f.remoteRef(testRef) == "" {
		t.Fatalf("no snapshot pushed at all:\n%s", out)
	}
	if n := strings.Count(out, "pushed snapshot"); n != 1 {
		t.Errorf("3 cycles with 1 change should push exactly once, pushed %d times:\n%s", n, out)
	}
}

// TestAutosaveSuppressesFirstSnapshotOfPristineClone is the regression test for
// review finding #3's second half. Every agent used to force-push an empty
// snapshot at pod start, before any work existed.
func TestAutosaveSuppressesFirstSnapshotOfPristineClone(t *testing.T) {
	f := newAutosaveFixture(t)
	out, ok := f.run(t, testRef, 2) // no edits at all
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if ref := f.remoteRef(testRef); ref != "" {
		t.Errorf("a pristine clone force-pushed an empty snapshot (%s); on a recycled ref "+
			"that would clobber a dead agent's rescue branch:\n%s", ref, out)
	}
	if n := strings.Count(out, "pushed snapshot"); n != 0 {
		t.Errorf("expected zero pushes for an untouched clone, got %d:\n%s", n, out)
	}
}

// TestAutosaveStillSnapshotsLocalCommits guards the SAFETY CONDITION on that
// suppression. Seeding pushed_tree unconditionally from HEAD^{tree} would mark a
// clean-but-locally-committed worktree as "already pushed" and skip forever —
// losing precisely the committed-but-unpushed work the daemon exists to rescue
// (an agent stopped mid-task after committing locally). Suppression must apply only when HEAD is
// already on the remote.
func TestAutosaveStillSnapshotsLocalCommits(t *testing.T) {
	f := newAutosaveFixture(t)

	// The agent committed locally and never pushed; the worktree is CLEAN, so the
	// worktree tree == HEAD^{tree}.
	writeFile(t, f.work, "rescued.txt", "committed but never pushed\n")
	gitCmd(t, f.work, "add", "rescued.txt")
	gitCmd(t, f.work, "commit", "-m", "agent's local commit")
	if st := gitCmd(t, f.work, "status", "--porcelain"); st != "" {
		t.Fatalf("fixture should be clean, got %q", st)
	}

	out, ok := f.run(t, testRef, 1)
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if f.remoteRef(testRef) == "" {
		t.Fatalf("clean worktree with UNPUSHED local commits was treated as pristine; "+
			"the agent's committed work was never pushed:\n%s", out)
	}
	if got, exists := f.remoteFileAt(testRef, "rescued.txt"); !exists ||
		!strings.Contains(got, "committed but never pushed") {
		t.Errorf("local commit not carried by the snapshot: %q (exists=%v)", got, exists)
	}
}

func TestAutosavePushesAgainWhenWorkChanges(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "first-revision\n")

	// ⚠ Two cycles in ONE process. An earlier version ran two separate processes
	// and was blind to a "push once, then never again" latch, because pushed_tree
	// starts empty in a fresh process (mutant M10 survived). The workspace is
	// mutated between cycles by a post-receive hook on the remote — deterministic,
	// no timing dependency.
	target := filepath.Join(f.work, "work.txt")
	hookDir := filepath.Join(f.origin, "hooks")
	writeFile(t, hookDir, "post-receive",
		"#!/bin/sh\nif [ -f ./mutate-once ]; then rm -f ./mutate-once; "+
			"printf 'second-revision-different\\n' > "+target+"; fi\nexit 0\n")
	if err := os.Chmod(filepath.Join(hookDir, "post-receive"), 0o755); err != nil {
		t.Fatalf("chmod hook: %v", err)
	}
	writeFile(t, f.origin, "mutate-once", "armed\n")

	out, ok := f.run(t, testRef, 2)
	if !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if n := strings.Count(out, "pushed snapshot"); n != 2 {
		t.Errorf("work changed between cycles, so both must push; pushed %d time(s):\n%s", n, out)
	}
	got, exists := f.remoteFileAt(testRef, "work.txt")
	if !exists || !strings.Contains(got, "second-revision-different") {
		t.Errorf("ref did not advance to the newer work: %q (exists=%v)\n%s", got, exists, out)
	}
}

func TestAutosaveRetriesAfterFailedPush(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "rescue-me.txt", "work that must not be abandoned\n")

	hook := filepath.Join(f.origin, "hooks", "pre-receive")
	writeFile(t, filepath.Dir(hook), "pre-receive",
		"#!/bin/sh\nif [ -f ./reject-once ]; then rm -f ./reject-once; echo 'test hook: rejecting first push' >&2; exit 1; fi\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatalf("chmod hook: %v", err)
	}
	writeFile(t, f.origin, "reject-once", "armed\n")

	out, _ := f.run(t, testRef, 2)
	if !strings.Contains(out, "push to") || !strings.Contains(out, "failed") {
		t.Errorf("expected the first cycle to report a push failure:\n%s", out)
	}
	if f.remoteRef(testRef) == "" {
		t.Fatalf("after a rejected push the daemon never retried — work left unpushed:\n%s", out)
	}
	got, exists := f.remoteFileAt(testRef, "rescue-me.txt")
	if !exists || !strings.Contains(got, "work that must not be abandoned") {
		t.Errorf("retry pushed the wrong content: %q (exists=%v)", got, exists)
	}
}

// ⚠ This is the ONE test that drives MUSTER_WIP_REMOTE with a filesystem
// path/URL rather than the name "origin". The knob accepts both and the default
// is now the production shape (see baseEnv), so this holds the other polarity.
func TestAutosaveHonoursGitignore(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "kept.txt", "ordinary-work\n")
	writeFile(t, f.work, "ignored-secrets.env", "TOKEN=super-secret-value\n")
	writeFile(t, f.work, "build/artifact.bin", "large-binary-junk\n")

	if out, ok := f.run(t, testRef, 1, "MUSTER_WIP_REMOTE="+f.origin); !ok {
		t.Fatalf("daemon exited non-zero:\n%s", out)
	}
	if _, exists := f.remoteFileAt(testRef, "kept.txt"); !exists {
		t.Error("ordinary work was not snapshotted")
	}
	if got, exists := f.remoteFileAt(testRef, "ignored-secrets.env"); exists {
		t.Errorf("gitignored secret file was pushed to the remote: %q", got)
	}
	if got, exists := f.remoteFileAt(testRef, "build/artifact.bin"); exists {
		t.Errorf("gitignored build artifact was pushed to the remote: %q", got)
	}
}

// TestAutosaveAnchorsSnapshotLocallyWhenPushFails: review 🟡. A failed push used
// to leave the snapshot as a DANGLING commit, reachable only via `fsck
// --lost-found` and eligible for `gc`. It must be a real ref inside the pod.
func TestAutosaveSurvivesUnpushableRemote(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "unpushable-but-committed\n")

	out, _ := f.run(t, testRef, 2, "MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"))

	if !strings.Contains(out, "failed") {
		t.Errorf("a failing push must be reported:\n%s", out)
	}
	if !strings.Contains(out, "committed locally") {
		t.Errorf("the operator must be told the work is still local:\n%s", out)
	}
	// ⚠ Was `Count(out, "failed") >= 2`. The failure line now reads "… failed (git
	// failed with exit N: …)", so the word appears TWICE PER LINE and one cycle
	// satisfied the check — a daemon that gave up after a single failure passed.
	// Count a phrase that occurs exactly once per failure instead.
	if n := strings.Count(out, "is NOT durable off-pod"); n < 2 {
		t.Errorf("daemon stopped retrying after a failure (%d failure lines, want >=2):\n%s", n, out)
	}
	// The snapshot must be a reachable local ref, not a dangling object.
	local, ok := gitTry(f.work, "rev-parse", "--verify", "refs/muster-wip/last")
	if !ok || local == "" {
		t.Fatalf("failed push left no local ref; the snapshot is dangling and gc-eligible:\n%s", out)
	}
	got, exists := gitTry(f.work, "show", "refs/muster-wip/last:work.txt")
	if !exists || !strings.Contains(got, "unpushable-but-committed") {
		t.Errorf("local fallback ref does not carry the work: %q", got)
	}
	if out, ok := gitTry(f.work, "fsck", "--no-progress", "--connectivity-only"); !ok {
		t.Errorf("repo damaged by the daemon: %s", out)
	}
}

// TestAutosavePushTimesOut: review 🟡. Without a timeout, a push into a TCP
// blackhole stalls this single-process loop forever — no further snapshots, no
// log line, no signal of any kind. Exercised for real via a custom git remote
// transport (`git-remote-sleepy`) that hangs, so the timeout must actually fire.
func TestAutosavePushTimesOut(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("coreutils `timeout` not available")
	}
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "push will hang\n")

	// git invokes `git-remote-<scheme>` from PATH for a `scheme::…` URL.
	bin := filepath.Join(f.root, "bin")
	writeFile(t, bin, "git-remote-sleepy", "#!/bin/sh\nsleep 120\n")
	if err := os.Chmod(filepath.Join(bin, "git-remote-sleepy"), 0o755); err != nil {
		t.Fatalf("chmod helper: %v", err)
	}

	start := time.Now()
	out, _ := f.run(t, testRef, 1,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MUSTER_WIP_REMOTE=sleepy::blackhole",
		"MUSTER_WIP_PUSH_TIMEOUT=2",
	)
	elapsed := time.Since(start)

	if elapsed > 30*time.Second {
		t.Fatalf("push was not bounded by the timeout (took %v):\n%s", elapsed, out)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("a timed-out push must be reported as a failure (took %v):\n%s", elapsed, out)
	}
	// The DIAGNOSIS, not just the fact of failure. `timeout` kills the child with
	// no output at all, so the old line ended in a bare dangling colon and an
	// operator could not tell a timeout from an auth failure.
	if !strings.Contains(out, "timed out after 2s") {
		t.Errorf("a timed-out push must say so; the operator cannot distinguish it from an "+
			"auth failure otherwise:\n%s", out)
	}
	if strings.Contains(out, "git failed") {
		t.Errorf("a timeout was misreported as a git error:\n%s", out)
	}
	if regexp.MustCompile(`(?m):\s*$`).MatchString(out) {
		t.Errorf("failure line ends in a dangling colon with nothing after it:\n%s", out)
	}
}

// --- the failure diagnosis ---
//
// Three different things used to render identically: a git error, a push killed
// by `timeout` (exit 124, NO output — the line ended in a bare colon), and the
// target repo's own hook stdout. The last is fixed by not running hooks at all;
// these pin the other two apart.

// TestAutosaveReportsGitFailuresAsGitFailures: an unreachable remote is a git
// error and must be labelled as one, carrying git's own words. Red at 5e7c2b03,
// which printed the output after a bare "off-pod:" with no attribution.
func TestAutosaveReportsGitFailuresAsGitFailures(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "cannot be pushed\n")

	out, _ := f.run(t, testRef, 1, "MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"))

	if !strings.Contains(out, "git failed with exit") {
		t.Errorf("a git error must be attributed to git, with its status:\n%s", out)
	}
	if strings.Contains(out, "timed out") {
		t.Errorf("a git error was misreported as a timeout:\n%s", out)
	}
	// git's own words must survive.
	//
	// ⚠ This assertion used to be `Contains(out, "does-not-exist.git")` — a DEAD
	// ASSERTION. That substring comes from the daemon's own `$remote`
	// interpolation in the message PREFIX, not from git, so it was true whether or
	// not `$push_out` was included at all: mutation A12 (third branch reduced to
	// `printf 'git failed with exit %s'`, dropping the output entirely) left the
	// suite GREEN. Assert on text only git can emit.
	if !strings.Contains(out, "fatal:") {
		t.Errorf("git's own diagnostic was dropped from the failure line — the operator gets "+
			"an exit status and nothing to act on:\n%s", out)
	}
	if !strings.Contains(out, "does not appear to be a git repository") {
		t.Errorf("git's specific complaint did not survive into the failure line:\n%s", out)
	}
	if strings.Contains(out, "produced no output") {
		t.Errorf("git DID produce output; the outputless branch was taken anyway:\n%s", out)
	}
}

// TestAutosaveReportsAnOutputlessKillDistinctly covers the third case an operator
// meets: the push dies producing NOTHING. In production that is the OOM killer
// taking git inside a memory-capped agent pod — exit 137, empty output. The line
// must still say something, not trail off.
//
// Simulated with a `timeout` shim that exits 137 without speaking, which also
// proves the timeout branch keys on 124 SPECIFICALLY rather than on "the prefix
// was present and something failed".
func TestAutosaveReportsAnOutputlessKillDistinctly(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "push gets killed\n")

	bin := filepath.Join(f.root, "shimbin")
	writeFile(t, bin, "timeout", "#!/bin/sh\nexit 137\n")
	if err := os.Chmod(filepath.Join(bin, "timeout"), 0o755); err != nil {
		t.Fatalf("chmod shim: %v", err)
	}

	out, _ := f.run(t, testRef, 1,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if !strings.Contains(out, "no output") {
		t.Errorf("a push that died silently must say so rather than trailing off:\n%s", out)
	}
	if !strings.Contains(out, "exit 137") {
		t.Errorf("the exit status is the only evidence left when there is no output:\n%s", out)
	}
	if strings.Contains(out, "timed out") {
		t.Errorf("exit 137 is not a timeout (124 is); the branch is keyed on the wrong "+
			"thing:\n%s", out)
	}
	if regexp.MustCompile(`(?m):\s*$`).MatchString(out) {
		t.Errorf("failure line ends in a dangling colon:\n%s", out)
	}
}

// TestTheDurabilityAlarmLeavesForTheTaskService is the seam guard for the ONE
// task-side path this repository builds outside the in-pod CLI.
//
// 🔴 WHY IT IS A SEPARATE TEST FROM THE ONE BELOW. That one asserts the alarm
// FIRES, once per episode, with the right body and credential — and it is
// satisfied by a daemon that posts to whichever single host it was handed. Only a
// configuration where the two base URLs are DIFFERENT can tell "it went to the
// task service" from "it went to the only server in the fixture", and that
// configuration is the one production enters at cutover. Until this guard existed
// the whole suite was green with POST /agent/task/comment built on
// $MUSTER_API_URL — a task-side route on the router base URL, which 404s at
// cutover and takes the durability alarm silent with it.
//
// 🔴 IT ASSERTS THE RELATIONSHIP IN BOTH DIRECTIONS. A daemon that sent
// everything to the task host would pass the first subtest and fail the second;
// one that ignored MUSTER_API_URL entirely passes the second and fails the
// first. Neither subtest alone is evidence.
func TestTheDurabilityAlarmLeavesForTheTaskService(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	// recorder returns a server plus a reader for the paths it was asked for.
	recorder := func(t *testing.T) (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		}))
		t.Cleanup(srv.Close)
		return srv, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), paths...)
		}
	}

	t.Run("with two different base URLs the alarm goes to the TASK host", func(t *testing.T) {
		f := newAutosaveFixture(t)
		writeFile(t, f.work, "work.txt", "cannot be pushed\n")
		router, routerPaths := recorder(t)
		task, taskPaths := recorder(t)
		// FIXTURE CONTROL. Two servers that happened to share an address would make
		// every verdict below vacuous — the same failure the ledger seam guard in
		// the in-pod CLI guards against.
		if router.URL == task.URL {
			t.Fatalf("both fixture servers are at %s; this test cannot tell the two base URLs apart", router.URL)
		}
		out, _ := f.run(t, testRef, 6,
			"MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"),
			"MUSTER_API_URL="+router.URL,
			"MUSTER_API_URL="+task.URL,
			"MUSTER_HOOK_TOKEN=test-tok",
		)
		got := taskPaths()
		if len(got) != 1 || got[0] != "/agent/task/comment" {
			t.Fatalf("the TASK host saw %v, want exactly one POST /agent/task/comment.\n"+
				"🔴 /agent/task* is TASK-side. Built on $MUSTER_API_URL it 404s the moment the "+
				"cutover repoints the task service, curl -sf fails, and the durability alarm — the "+
				"one message that cannot survive in a pod-local log — goes silent.\ndaemon output:\n%s",
				got, out)
		}
		if other := routerPaths(); len(other) != 0 {
			t.Errorf("the ROUTER host saw %v; a task-side path is still resolving against "+
				"MUSTER_API_URL", other)
		}
	})

	t.Run("with no task URL configured the alarm falls back to the router host", func(t *testing.T) {
		f := newAutosaveFixture(t)
		writeFile(t, f.work, "work.txt", "cannot be pushed\n")
		router, routerPaths := recorder(t)
		out, _ := f.run(t, testRef, 6,
			"MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"),
			"MUSTER_API_URL="+router.URL,
			"MUSTER_HOOK_TOKEN=test-tok",
		)
		got := routerPaths()
		if len(got) != 1 || got[0] != "/agent/task/comment" {
			t.Fatalf("the ROUTER host saw %v, want exactly one POST /agent/task/comment.\n"+
				"The ${MUSTER_API_URL:-$MUSTER_API_URL} default is what keeps this change "+
				"inert: the emitted shell has no `set -u`, so without the default an unset task URL "+
				"expands to empty and the POST goes to a bare relative path curl refuses.\n"+
				"daemon output:\n%s", got, out)
		}
	})
}

// TestAutosaveNotifiesTheTaskThreadWhenDurabilityIsLost covers the review's
// "diagnostics may be invisible" point. Everything else this daemon prints lands
// in a file inside an ephemeral pod — which dies with exactly the work it was
// reporting on — so a log line cannot be the channel for the one message that
// matters. On a sustained push failure it posts to the agent self-service API
// using the MUSTER_HOOK_TOKEN already in the pod, surfacing on the task thread.
//
// It must fire ONCE per episode, not once per cycle.
func TestAutosaveNotifiesTheTaskThreadWhenDurabilityIsLost(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "cannot be pushed\n")

	var mu sync.Mutex
	var posts []string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, r.Method+" "+r.URL.Path+" "+string(body))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	out, _ := f.run(t, testRef, 6,
		"MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"),
		"MUSTER_API_URL="+srv.URL,
		"MUSTER_HOOK_TOKEN=test-tok",
	)

	mu.Lock()
	defer mu.Unlock()
	if len(posts) == 0 {
		t.Fatalf("durability was lost for 6 cycles and the human was never told:\n%s", out)
	}
	if len(posts) != 1 {
		t.Errorf("notification must fire once per episode, not per cycle; got %d posts:\n%v", len(posts), posts)
	}
	if !strings.Contains(posts[0], "/agent/task/comment") {
		t.Errorf("posted to the wrong endpoint: %q", posts[0])
	}
	if !strings.Contains(posts[0], "NOT being saved off-pod") {
		t.Errorf("notification does not say durability is off: %q", posts[0])
	}
	if auths[0] != "Bearer test-tok" {
		t.Errorf("notification not authenticated with the pod's hooks token: %q", auths[0])
	}
}

func TestAutosaveRefusesIncompleteConfig(t *testing.T) {
	for _, tc := range []struct{ name, repo, ref string }{
		{"no ref", "/data/repos/widgets", ""},
		{"no repo", "", "refs/heads/muster-wip/x-1"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutosaveFixture(t)
			cmd := exec.Command(testShell(), f.script)
			cmd.Dir = f.root
			cmd.Env = append(os.Environ(),
				"MUSTER_WIP_REPO="+tc.repo, "MUSTER_WIP_REF="+tc.ref,
				"MUSTER_WIP_MAX_CYCLES=1", "MUSTER_WIP_INTERVAL=0", "MUSTER_WIP_LOG=-",
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("daemon exited 0 with incomplete config; it must fail loudly:\n%s", out)
			}
			if !strings.Contains(string(out), "are required") {
				t.Errorf("expected an explicit config error, got:\n%s", out)
			}
		})
	}
}

// TestAutosaveRunsUnboundedByDefault is the regression test for review blocker
// #2. Every other shell test sets MUSTER_WIP_MAX_CYCLES explicitly, so NO test
// ever ran the daemon in its production configuration — and flipping the default
// from 0 to 1 left the whole suite green. Under that mutant the daemon takes one
// snapshot at pod start, logs "pushed snapshot", and exits: healthy-looking logs,
// original bug restored.
//
// The seam is left UNSET here. A failing remote gives one observable line per
// cycle without needing the seam.
func TestAutosaveRunsUnboundedByDefault(t *testing.T) {
	f := newAutosaveFixture(t)
	writeFile(t, f.work, "work.txt", "keeps-failing-to-push\n")

	outPath := filepath.Join(f.root, "daemon.log")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	defer outFile.Close()

	cmd := exec.Command(testShell(), f.script)
	cmd.Dir = f.root
	// NOTE: MUSTER_WIP_MAX_CYCLES deliberately absent — that is the point.
	cmd.Env = append(os.Environ(),
		"MUSTER_WIP_REPO="+f.work,
		"MUSTER_WIP_REF="+testRef,
		"MUSTER_WIP_REMOTE="+filepath.Join(f.root, "does-not-exist.git"),
		"MUSTER_WIP_INTERVAL=1",
		"MUSTER_WIP_LOG=-",
		"MUSTER_API_URL=", "MUSTER_HOOK_TOKEN=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		body, _ := os.ReadFile(outPath)
		t.Fatalf("daemon EXITED on its own after %v — with the seam unset it must run "+
			"forever, or an agent gets exactly one snapshot at pod start:\n%s", err, body)
	case <-time.After(4 * time.Second):
	}
	_ = cmd.Process.Kill()
	<-done

	body, _ := os.ReadFile(outPath)
	if n := strings.Count(string(body), "failed"); n < 2 {
		t.Errorf("expected >=2 cycles in 4s at a 1s interval, saw %d:\n%s", n, body)
	}
}

// --- wiring: the daemon actually reaches the pod ---

// autosaveInvocationPattern matches an ACTUAL invocation of the daemon carrying both
// required env vars and backgrounded — not the install line, and not any stray
// "&". Review blocker #1: the previous guards were satisfied by `Contains(init,
// autosavePath)` (true of the install line) and `Contains(init, "&")` (true of
// any backgrounded command), so replacing the launch line with an unrelated
// command left the suite green.
//
// ⚠ Tolerant of ADDITIONAL env assignments on the line, on purpose. A stricter
// pattern that enumerated the exact three variables made this guard fire for any
// change to the launcher's environment — so the separate seam guards below (the
// MAX_CYCLES and LOG seams) could never be shown to catch anything themselves,
// because this one always failed first. Structure is pinned here; the specific
// variables are asserted separately against the matched line.
var autosaveInvocationPattern = regexp.MustCompile(
	`(?m)^\s*(?:[A-Z_]+=(?:'[^']*'|\S*) )+/bin/sh ` + regexp.QuoteMeta(autosavePath) + ` &$`)

func TestAutosaveRefIsUniqueForAllTime(t *testing.T) {
	recycled := autosaveRef("bold-moth", 42)
	reused := autosaveRef("bold-moth", 917) // same name, later agent
	if recycled == reused {
		t.Fatalf("two agents sharing a recycled NAME share a snapshot ref (%s); the second "+
			"would clobber the first's rescue branch", recycled)
	}
	if a, b := autosaveRef("bold-moth", 42), autosaveRef("brave-heron", 42); a == b {
		t.Fatalf("distinct agents share a snapshot ref: %s", a)
	}
	for _, ref := range []string{recycled, reused} {
		if !strings.HasPrefix(ref, "refs/heads/muster-wip/") {
			t.Errorf("snapshot ref %q is not namespaced under muster-wip/", ref)
		}
	}
}

// TestAutosaveCadenceIsSane: mutating the interval to 0 turns the daemon into a
// hot loop re-walking the whole worktree continuously on an emptyDir.
func TestAutosaveCadenceIsSane(t *testing.T) {
	if autosaveIntervalSeconds < autosaveMinIntervalSeconds {
		t.Errorf("snapshot cadence %ds is below the %ds floor; the daemon would re-walk the "+
			"worktree continuously", autosaveIntervalSeconds, autosaveMinIntervalSeconds)
	}
	init := autosaveInitCommands("x", 1, "/data/repos/x", testExcludePatterns)
	if !strings.Contains(init, "MUSTER_WIP_INTERVAL="+strconv.Itoa(autosaveIntervalSeconds)) {
		t.Errorf("launcher does not pass the configured cadence:\n%s", init)
	}
}

// TestAutosaveAndCredentialHelperShipTogether is a SEAM guard: the daemon can
// only push because it inherits the container-level GIT_CONFIG_* env that
// installs the credential helper — a relationship no single-component test owns.
//
// ⚠ Checked across EVERY provisioning regime. An earlier version asserted it only
// for a repo-backed agent WITH a token, where a mutant that starts the daemon on
// the wrong condition still produces both halves, so it survived (S1).
// ---------------------------------------------------------------------------
// THE WIRING TESTS, REWRITTEN FOR provision.Spec.
//
// 🔴 THE UPSTREAM VERSIONS OF THESE SIX ASSERTED ON A HELM VALUE MAP and are
// deliberately NOT ported shape-for-shape. They read
// `buildHelmValues(...)["extraInitCommands"]` and then grepped a single joined
// shell string for a base64 payload, an install line and an invocation — because
// that string was the only delivery channel the chart offered. muster has
// provision.Spec, so the same claims become assertions about a File and an Init
// entry, which is the whole point of the port: the payload is DATA now and a test
// can compare bytes instead of parsing shell.
//
// ⚠ TWO UPSTREAM TESTS ARE DROPPED RATHER THAN REWRITTEN, AND NOT SILENTLY:
//
//   - TestAutosaveAndCredentialHelperShipTogether asserted the daemon and a git
//     credential-helper init line were emitted together. muster's
//     provision.Repo is DECLARED, NOT CLONED, so nothing here writes a credential
//     helper — whoever performs the clone owns that, and coupling the two in this
//     package would assert a relationship neither half of which it controls.
//   - TestNoAutosaveWithoutPushableAuth asserted the daemon is suppressed when
//     there is no pushable credential. Build cannot know that for the same reason;
//     the gate here is the presence of a repository, and the daemon itself answers
//     the credential question at runtime by posting a durability alarm to the
//     agent's own task thread. That alarm path is still covered, by
//     TestAutosaveNotifiesTheTaskThreadWhenDurabilityIsLost above.
//
// Both claims survive in a weaker-but-true form as
// TestNoAutosaveForAnAgentWithNoRepositoryToPushTo below. Recording which
// coverage went away, and why, is the point of this comment.
// ---------------------------------------------------------------------------

// specWithRepo builds a spec for a repo-backed agent, which is the regime the
// autosave daemon exists for.
func specWithRepo(t *testing.T) provision.Spec {
	t.Helper()
	return mustBuild(t, fixtureAgent(), fixtureConfig(), Options{})
}

func autosaveFileIn(spec provision.Spec) (provision.File, bool) {
	for _, f := range spec.Files {
		if f.Path == autosavePath {
			return f, true
		}
	}
	return provision.File{}, false
}

func autosaveInitIn(spec provision.Spec) (string, bool) {
	for _, cmd := range spec.Init {
		if autosaveInvocationPattern.MatchString(cmd) {
			return cmd, true
		}
	}
	return "", false
}

// TestTheDaemonIsDeliveredAsBytesRatherThanAsAShellPayload is the assertion that
// would fail if anyone reintroduced the upstream install shell.
//
// 🔴 IT COMPARES THE FULL BYTES, not a substring. A substring check would pass for
// a truncated script, and a truncated autosave.sh is precisely the failure whose
// symptom is "the rescue branch was never created" — discovered only when someone
// goes looking for work that is already gone.
func TestTheDaemonIsDeliveredAsBytesRatherThanAsAShellPayload(t *testing.T) {
	spec := specWithRepo(t)

	f, ok := autosaveFileIn(spec)
	if !ok {
		var paths []string
		for _, sf := range spec.Files {
			paths = append(paths, sf.Path)
		}
		t.Fatalf("no autosave file at %s; spec carries %v", autosavePath, paths)
	}
	if string(f.Content) != autosaveScript {
		t.Errorf("delivered script differs from the embedded one: %d bytes vs %d",
			len(f.Content), len(autosaveScript))
	}
	if f.EffectiveMode()&0o111 == 0 {
		t.Errorf("autosave file mode %v is not executable", f.EffectiveMode())
	}

	// The upstream install machinery must be absent from every Init entry. Each of
	// these three is a separate way the payload could have travelled as shell.
	for _, cmd := range spec.Init {
		for _, forbidden := range []string{"base64", "chmod", autosaveScript} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("Init entry carries %q, so the script is travelling as shell rather than as data:\n%s",
					forbidden, cmd)
			}
		}
	}
}

// TestTheDaemonIsStartedByExactlyOneBackgroundedInitEntry pins the launcher.
func TestTheDaemonIsStartedByExactlyOneBackgroundedInitEntry(t *testing.T) {
	spec := specWithRepo(t)

	var matches []string
	for _, cmd := range spec.Init {
		if autosaveInvocationPattern.MatchString(cmd) {
			matches = append(matches, cmd)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 autosave invocation, got %d: %v\nspec.Init = %v",
			len(matches), matches, spec.Init)
	}
	cmd := matches[0]

	// 🔴 BACKGROUNDED. The daemon is an infinite loop; foregrounded it would block
	// the instance's own process from starting, turning a durability feature into a
	// total outage. The regexp above already requires the trailing `&`, so this
	// re-asserts it explicitly rather than relying on a reader spotting it there.
	if !strings.HasSuffix(strings.TrimSpace(cmd), "&") {
		t.Errorf("autosave invocation is not backgrounded, so it would block instance startup:\n%s", cmd)
	}

	// Every variable the daemon requires must be on that one line.
	for _, required := range []string{"MUSTER_WIP_REPO=", "MUSTER_WIP_REF=", "MUSTER_WIP_INTERVAL=", "MUSTER_WIP_EXCLUDE="} {
		if !strings.Contains(cmd, required) {
			t.Errorf("autosave invocation omits %s:\n%s", required, cmd)
		}
	}
}

// TestTheDaemonWatchesTheDeclaredRepoPathAndNotTheWorkspaceRoot is the mutant that
// upstream caught too: watching the workspace instead of the checkout snapshots
// the agent runtime's own droppings and misses the actual work.
func TestTheDaemonWatchesTheDeclaredRepoPathAndNotTheWorkspaceRoot(t *testing.T) {
	spec := specWithRepo(t)
	cmd, ok := autosaveInitIn(spec)
	if !ok {
		t.Fatal("no autosave invocation")
	}

	// Literal, not recomputed from spec.Repo.Path — comparing the implementation
	// to itself would pass for any path, including the workspace root.
	want := "MUSTER_WIP_REPO='/srv/agent-work/tide-charts'"
	if !strings.Contains(cmd, want) {
		t.Errorf("invocation does not watch the checkout:\nwant substring %s\ngot %s", want, cmd)
	}
	if strings.Contains(cmd, "MUSTER_WIP_REPO='/srv/agent-work'") {
		t.Error("invocation watches the WORKSPACE ROOT rather than the checkout, so snapshots " +
			"would carry the agent runtime's own files and miss the repository's history")
	}
}

// TestTheSnapshotRefInTheInvocationIsTheAgentSpecificOne closes the loop between
// autosaveRef's uniqueness argument and what is actually passed.
func TestTheSnapshotRefInTheInvocationIsTheAgentSpecificOne(t *testing.T) {
	spec := specWithRepo(t)
	cmd, ok := autosaveInitIn(spec)
	if !ok {
		t.Fatal("no autosave invocation")
	}
	// fixtureAgent is id 4291, name harbour-kestrel. Spelled out rather than
	// derived: autosaveRef(a.Name, a.ID) here would be the implementation agreeing
	// with itself, and the id is the entire safety argument.
	want := "MUSTER_WIP_REF='refs/heads/muster-wip/harbour-kestrel-4291'"
	if !strings.Contains(cmd, want) {
		t.Errorf("invocation does not carry the agent-specific rescue ref:\nwant substring %s\ngot %s", want, cmd)
	}
}

// TestTheExcludeListReachesTheDaemonAsAParameter pins the one structural change
// this port makes: the list travels from Config, through Build, onto the daemon's
// command line — and nothing in between substitutes a built-in.
func TestTheExcludeListReachesTheDaemonAsAParameter(t *testing.T) {
	cfg := fixtureConfig()
	cfg.AutosaveExcludePatterns = testExcludePatterns
	spec := mustBuild(t, fixtureAgent(), cfg, Options{})
	cmd, ok := autosaveInitIn(spec)
	if !ok {
		t.Fatal("no autosave invocation")
	}
	for _, pattern := range testExcludePatterns {
		if !strings.Contains(cmd, pattern) {
			t.Errorf("exclude pattern %q does not reach the daemon:\n%s", pattern, cmd)
		}
	}

	// 🔴 AND THE NEGATIVE HALF: a Config that sets NO patterns must produce an EMPTY
	// list, not a built-in one. Without this, reinstating a hardcoded default would
	// pass the loop above (the caller's patterns still arrive) while shipping a
	// second runtime's filenames to everyone — which is the defect the repository's
	// own secret gate caught in the first draft of this port.
	bare := fixtureConfig()
	bare.AutosaveExcludePatterns = nil
	bareSpec := mustBuild(t, fixtureAgent(), bare, Options{})
	bareCmd, ok := autosaveInitIn(bareSpec)
	if !ok {
		t.Fatal("no autosave invocation for the no-patterns case")
	}
	if !strings.Contains(bareCmd, "MUSTER_WIP_EXCLUDE=''") {
		t.Errorf("with no configured patterns the daemon was handed a non-empty exclude list, "+
			"so this package is substituting a built-in:\n%s", bareCmd)
	}
}

// TestAPatternContainingTheListSeparatorIsRefused: the list is colon-separated, and
// a gitignore pattern may legally contain a colon. Splitting it silently would
// produce two wrong patterns, one of which could exclude something real.
func TestAPatternContainingTheListSeparatorIsRefused(t *testing.T) {
	cfg := fixtureConfig()
	cfg.AutosaveExcludePatterns = []string{"/ok.md", "/has:colon.md"}
	_, err := Build(fixtureAgent(), cfg, Options{})
	if err == nil {
		t.Fatal("Build accepted an exclude pattern containing the list separator")
	}
	if !strings.Contains(err.Error(), "colon") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestTheScriptExcludesWhateverItIsGivenRatherThanAHardcodedList is the
// BEHAVIOURAL half of that change, and it is new coverage rather than a port.
//
// 🔴 THE LEDGER TEST AND THIS ONE ARE DIFFERENT CLAIMS. The ledger pins WHICH
// patterns muster passes today; this pins that the daemon honours whatever it is
// handed. Only the second would catch a script that ignored the variable and fell
// back to a hardcoded list — which is exactly what the port had to remove, and
// exactly what a careless revert would restore.
func TestTheScriptExcludesWhateverItIsGivenRatherThanAHardcodedList(t *testing.T) {
	f := newAutosaveFixture(t)

	// A pattern no agent runtime uses and no upstream list contains, so a script
	// that ignored MUSTER_WIP_EXCLUDE could not produce it by accident.
	const custom = "/SOME-OTHER-RUNTIME-STATE.md"
	writeFile(t, f.work, "SOME-OTHER-RUNTIME-STATE.md", "state from a runtime muster has never heard of")
	writeFile(t, f.work, "real-work.txt", "the work that must be saved")

	if _, ok := f.run(t, "refs/heads/muster-wip/custom-1", 1, "MUSTER_WIP_EXCLUDE="+custom); !ok {
		t.Fatal("daemon did not complete a cycle")
	}

	gitdir := filepath.Join(f.work, ".git")
	written := excludePatternsWrittenByTheScript(t, gitdir)
	if len(written) == 0 {
		t.Fatal("the daemon wrote NO exclude patterns, so this test cannot tell an honoured " +
			"parameter from an ignored one")
	}
	found := false
	for _, w := range written {
		if w == custom {
			found = true
		}
	}
	if !found {
		t.Errorf("the daemon did not honour MUSTER_WIP_EXCLUDE=%q; it wrote %v", custom, written)
	}

	// 🔴 AND THE NEGATIVE HALF: none of muster's own default patterns may appear,
	// because they were NOT passed. Without this, a script that appended a
	// hardcoded list IN ADDITION to the parameter would pass the check above.
	for _, def := range testExcludePatterns {
		for _, w := range written {
			if w == def {
				t.Errorf("the daemon wrote %q, which was not in MUSTER_WIP_EXCLUDE — it is still "+
					"carrying a hardcoded list alongside the parameter", def)
			}
		}
	}
}

// TestNoAutosaveForAnAgentWithNoRepositoryToPushTo is the surviving form of the two
// dropped upstream gate tests. See the comment at the top of this section.
func TestNoAutosaveForAnAgentWithNoRepositoryToPushTo(t *testing.T) {
	a := fixtureAgent()
	a.Repo, a.RepoBranch = "", ""
	spec := mustBuild(t, a, fixtureConfig(), Options{})

	if _, ok := autosaveFileIn(spec); ok {
		t.Error("an agent with no repository got the autosave daemon; there is no ref to push a " +
			"snapshot to, so the daemon would log a failure every cycle forever")
	}
	if cmd, ok := autosaveInitIn(spec); ok {
		t.Errorf("an agent with no repository got an autosave invocation: %s", cmd)
	}

	// Control: the same fixture WITH a repo does get it, so the absence above is
	// attributable to the repo being empty rather than to the detector failing.
	withRepo := specWithRepo(t)
	if _, ok := autosaveFileIn(withRepo); !ok {
		t.Fatal("the control case did not get the daemon either, so this test proves nothing " +
			"about the no-repo case")
	}
}
