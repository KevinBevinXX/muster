package agentspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// ccNoRepoGoldenPath pins a claude-code agent WITHOUT a repository, byte for byte.
//
// 🔴 IT WAS WRITTEN FROM THE CODE BEFORE REPOSITORY SUPPORT EXISTED (the base of
// feat/claude-code-agent-repo, ab08b60), so it is the "today" a repo-less
// claude-code agent — the live chief among them — must stay identical to. Two
// cases: a plain worker, and a chief-shaped one (instructions + the gateway
// sidecar). Like spec.golden.json it is evidence about CHANGE only; secrets are
// redacted to their names (goldenView).
const ccNoRepoGoldenPath = "testdata/claudecode-norepo.golden.json"

func TestAClaudeCodeSpecWithoutARepoIsUnchanged(t *testing.T) {
	plain := mustBuild(t, ccAgent(), ccConfig(), Options{})
	chief := mustBuild(t, ccAgent(), sidecarConfig(), Options{Instructions: "# chief\n\nprose\n", GatewaySidecar: &GatewaySidecar{}})
	got, err := json.MarshalIndent(map[string]any{"plain": goldenView(plain), "chief": goldenView(chief)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *goldenWrite {
		if err := os.WriteFile(filepath.FromSlash(ccNoRepoGoldenPath), got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("wrote %s; this run asserted nothing — re-run without -golden-write", ccNoRepoGoldenPath)
	}
	want, err := os.ReadFile(filepath.FromSlash(ccNoRepoGoldenPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("a claude-code spec with NO repository changed (it must be byte-identical to the pre-repo build).\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestAClaudeCodeAgentWithARepoIsToldToCheckItOutAndWorkThere pins, as
// LITERALS, what a repository adds to a claude-code spec: ccd's three variables
// (the repository, its branch, and the CLI's working directory = the checkout,
// under /data/workspace and named after the repository) and the declared Repo —
// and nothing else: no Init step, no file, no new credential. The fixture's
// owner, name and branch are pairwise distinct, so a mutant that used the owner
// as the directory, or dropped the branch, moves a value.
func TestAClaudeCodeAgentWithARepoIsToldToCheckItOutAndWorkThere(t *testing.T) {
	a := ccAgent()
	a.Repo, a.RepoBranch = "example-org/tide-charts", "release/7.2"
	spec := mustBuild(t, a, ccConfig(), Options{})
	plain := mustBuild(t, ccAgent(), ccConfig(), Options{})

	want := append(append([]provision.EnvVar(nil), plain.Env...),
		provision.EnvVar{Name: "CCD_REPO", Value: "example-org/tide-charts"},
		provision.EnvVar{Name: "CCD_REPO_BRANCH", Value: "release/7.2"},
		provision.EnvVar{Name: "CCD_WORKSPACE", Value: "/data/workspace/tide-charts"},
	)
	if !reflect.DeepEqual(spec.Env, want) {
		t.Errorf("env = %+v\nwant %+v", spec.Env, want)
	}
	if w := (provision.Repo{URL: "example-org/tide-charts", Branch: "release/7.2", Path: "/data/workspace/tide-charts"}); spec.Repo != w {
		t.Errorf("repo = %+v, want %+v", spec.Repo, w)
	}
	if len(spec.Init) != 0 || len(spec.Files) != 0 || !reflect.DeepEqual(spec.Secrets, plain.Secrets) ||
		!reflect.DeepEqual(spec.Runtime, plain.Runtime) || spec.Workspace != plain.Workspace || !reflect.DeepEqual(spec.Network, plain.Network) {
		t.Errorf("a repository changed more than the env and the declared repo: %+v", spec)
	}

	// No branch: the remote's default, so no CCD_REPO_BRANCH at all.
	a.RepoBranch = ""
	nb := mustBuild(t, a, ccConfig(), Options{})
	if _, ok := envValue(nb, "CCD_REPO_BRANCH"); ok {
		t.Errorf("an agent with no branch got CCD_REPO_BRANCH")
	}
	if v, _ := envValue(nb, "CCD_WORKSPACE"); v != "/data/workspace/tide-charts" {
		t.Errorf("CCD_WORKSPACE = %q", v)
	}
}

// TestARepoThatIsNotOwnerSlashNameIsRefused reaches the shape refusal with
// otherwise valid rows: traversal, a hidden directory, a missing owner, a URL,
// a nested path.
func TestARepoThatIsNotOwnerSlashNameIsRefused(t *testing.T) {
	for _, r := range []string{"example-org/..", "example-org/.hidden", "tide-charts", "/tide-charts",
		"https://github.com/example-org/tide-charts", "example-org/tide/charts"} {
		a := ccAgent()
		a.Repo = r
		if _, err := Build(a, ccConfig(), Options{}); err == nil || !strings.Contains(err.Error(), "which is not owner/name") {
			t.Errorf("repo %q: err = %v", r, err)
		}
	}
}
