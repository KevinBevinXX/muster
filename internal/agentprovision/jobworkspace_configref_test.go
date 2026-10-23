package agentprovision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// A job's config_ref (jobOverlayScript step 3): the overlay is taken from that
// commit instead of the default branch's tip, and only when the default branch
// reaches it. Through the real script and real git, on the worktreetest
// fixture plus a SECOND default-branch commit (advanceDefault) whose
// configuration differs from the first's at every overlaid path it touches —
// so "the restore used commit X" is visible byte for byte, and a script that
// ignored config_ref (restoring origin/HEAD) or ignored origin/HEAD (restoring
// config_ref when absent) is told apart.

// v2Files is what the second default-branch commit changes: pairwise distinct
// from DefaultFiles and from CommitFiles at every path.
var v2Files = map[string]string{
	"CLAUDE.md":                     "# default instructions v2\n",
	".claude/skills/rev/SKILL.md":   "---\nname: rev\ndescription: default skill v2\n---\nv2 body\n",
	"sub/.claude/skills/x/SKILL.md": "---\nname: x\ndescription: default nested skill v2\n---\nv2 nested body\n",
	"AGENTS.md":                     "# default agents instructions v2\n",
}

// advanceDefault commits v2Files on top of origin's main (the fixture's
// Default) and pushes it, WITHOUT fetching it into the agent's checkout, and
// returns the new commit: the checkout's origin/HEAD is then stale (still
// Default) until a test fetches.
func advanceDefault(t *testing.T, r worktreetest.Repo) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "v2")
	worktreetest.Git(t, "", "clone", "-q", "file://"+r.Origin, src)
	for p, body := range v2Files {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		worktreetest.Git(t, src, "add", "--", p)
	}
	worktreetest.Git(t, src, "commit", "-q", "-m", "default v2")
	v2 := worktreetest.Git(t, src, "rev-parse", "HEAD")
	worktreetest.Git(t, src, "push", "-q", "origin", "HEAD:main")
	return v2
}

// prepareJobAt is prepareJob with a config ref.
func prepareJobAt(a *Adapter, r worktreetest.Repo, sha, configRef string) (string, error) {
	a.anonGitBase = r.AnonBase
	return a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, worktreetest.RepoName, worktreetest.PR, sha, configRef)
}

// overlayOf reads every overlaid path the two default-branch commits decide
// from the job's worktree: path → content, "<absent>" when missing.
func overlayOf(t *testing.T, wt string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range []string{"CLAUDE.md", "AGENTS.md", ".mcp.json", ".claude/settings.json", ".claude/skills/rev/SKILL.md", "sub/.claude/skills/x/SKILL.md", ".claude/agents/evil.md"} {
		b, err := os.ReadFile(filepath.Join(wt, p))
		switch {
		case os.IsNotExist(err):
			out[p] = "<absent>"
		case err != nil:
			t.Fatal(err)
		default:
			out[p] = string(b)
		}
	}
	return out
}

// wantOverlay is the overlay a source commit's files give: base, then v2 on top
// when v2 is set; every other overlaid path absent.
func wantOverlay(markers string, v2 bool) map[string]string {
	def := worktreetest.DefaultFiles(markers)
	out := map[string]string{}
	for _, p := range []string{"CLAUDE.md", "AGENTS.md", ".mcp.json", ".claude/settings.json", ".claude/skills/rev/SKILL.md", "sub/.claude/skills/x/SKILL.md", ".claude/agents/evil.md"} {
		if b, ok := def[p]; ok {
			out[p] = b
		} else {
			out[p] = "<absent>"
		}
		if b, ok := v2Files[p]; ok && v2 {
			out[p] = b
		}
	}
	return out
}

func diffOverlay(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %s = %q, want %q", label, p, got[p], w)
		}
	}
}

func removeJob(t *testing.T, a *Adapter, r worktreetest.Repo) {
	t.Helper()
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatal(err)
	}
	if refs := worktreetest.Git(t, r.Checkout, "for-each-ref", "refs/muster-jobs/"); refs != "" {
		t.Fatalf("a job ref outlived the job: %s", refs)
	}
}

// TestAConfigRefPinsTheOverlayToThatDefaultBranchCommit: with the checkout's
// origin/HEAD at v2,
//   - config_ref = Default (an older default-branch commit): the overlay holds
//     DEFAULT's bytes, not v2's — the race this exists for (the caller hashed
//     Default; the branch moved before the job ran);
//   - config_ref = v2: v2's bytes;
//   - no config_ref: v2's bytes, origin/HEAD as before.
//
// Then, with the checkout's origin/HEAD STALE (still Default) and v2 only on
// the origin, config_ref = v2 is fetched anonymously, found on the default
// branch, and used — and the fetched ref is removed with the worktree.
func TestAConfigRefPinsTheOverlayToThatDefaultBranchCommit(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	markers := filepath.Join(d.root, "markers")
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), markers)
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	v2 := advanceDefault(t, r)

	t.Run("stale origin/HEAD: config_ref is fetched from the default branch", func(t *testing.T) {
		if got := worktreetest.Git(t, r.Checkout, "rev-parse", "refs/remotes/origin/HEAD"); got != r.Default {
			t.Fatalf("control: origin/HEAD = %s, want the stale %s", got, r.Default)
		}
		if _, err := prepareJobAt(a, r, r.Commit, v2); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		diffOverlay(t, "config_ref v2 (fetched)", overlayOf(t, wt), wantOverlay(markers, true))
		removeJob(t, a, r)
	})

	worktreetest.Git(t, r.Checkout, "fetch", "-q", "origin")
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "refs/remotes/origin/HEAD"); got != v2 {
		t.Fatalf("control: origin/HEAD = %s after the fetch, want %s", got, v2)
	}
	for _, tc := range []struct {
		name, ref string
		v2        bool
	}{
		{"config_ref the older default commit", r.Default, false},
		{"config_ref the tip", v2, true},
		{"no config_ref", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepareJobAt(a, r, r.Commit, tc.ref); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if got := worktreetest.Git(t, wt, "rev-parse", "HEAD"); got != r.Commit {
				t.Fatalf("worktree HEAD = %s, want the commit %s", got, r.Commit)
			}
			if b, _ := os.ReadFile(filepath.Join(wt, "f")); string(b) != "two\n" {
				t.Fatalf("f (not overlaid) = %q, want the commit's", b)
			}
			diffOverlay(t, tc.name, overlayOf(t, wt), wantOverlay(markers, tc.v2))
			removeJob(t, a, r)
		})
	}
}

// TestAConfigRefOffTheDefaultBranchIsRefused: the pull request's own commit
// (fetched into the checkout by the job itself a step earlier — the commit
// whose configuration the overlay exists to keep out), a commit on another
// branch (present in the checkout, so it is the ancestry that refuses it), and
// an id that names no object are each refused, naming the default branch's
// tip, with no worktree made. The positive control is the same job with
// config_ref the default branch's commit, which is made.
func TestAConfigRefOffTheDefaultBranchIsRefused(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	worktreetest.Git(t, r.Checkout, "fetch", "-q", "origin", "stray")
	for _, tc := range []struct{ name, ref string }{
		{"the pull request's own commit", r.Commit},
		{"a commit on another branch", r.Stray},
		{"no such object", strings.Repeat("e", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareJobAt(a, r, r.Commit, tc.ref)
			if err == nil || !strings.Contains(err.Error(), ConfigRefNotOnDefaultBranchMessage) || !strings.Contains(err.Error(), r.Default) {
				t.Fatalf("prepare = %v, want an error carrying %q and the default branch's tip %s", err, ConfigRefNotOnDefaultBranchMessage, r.Default)
			}
			if _, err := os.Lstat(wt); !os.IsNotExist(err) {
				t.Fatalf("a worktree was created (lstat: %v)", err)
			}
			removeJob(t, a, r)
		})
	}
	if _, err := prepareJobAt(a, r, r.Commit, r.Default); err != nil {
		t.Fatalf("control: config_ref the default branch's commit: prepare = %v", err)
	}
	removeJob(t, a, r)
}

// TestAMalformedConfigRefNeverReachesTheInstance: config_ref is a full commit
// id or nothing; anything else is refused before any exec.
func TestAMalformedConfigRefNeverReachesTheInstance(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	before := d.count()
	sha := strings.Repeat("a", 40)
	for _, ref := range []string{"main", "HEAD", sha[:12], strings.Repeat("A", 40), sha + "0", "-" + sha[1:]} {
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, worktreetest.RepoName, 7, sha, ref); err == nil {
			t.Errorf("config ref %q was accepted", ref)
		}
	}
	if d.count() != before {
		t.Fatalf("refusals ran %d exec(s)", d.count()-before)
	}
}
