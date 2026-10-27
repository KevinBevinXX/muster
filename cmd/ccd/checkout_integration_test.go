//go:build tmuxit

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// The repository checkout, through the WHOLE `ccd serve` binary supervising a
// real tmux pane (startServeAt): the production wiring end to end, with a
// local fake GitHub (checkout_test.go's fakeGitHub) and the fake CLI.
//
// ⚠ WHAT THIS PROVES AND WHAT IT DOES NOT. It proves ccd starts the CLI with
// its working directory = the checkout (the fake records its cwd), that the
// workspace trust ccd seeds is for that directory (the fake, like the real
// CLI, blocks on a trust screen and fires no hooks otherwise — so a session
// that STARTED proves trust), and that the checkout carries the repository's
// CLAUDE.md and .claude/skills/<name>/SKILL.md where the CLI looks for project
// skills. It does NOT prove the real CLI loads a project skill: the fake only
// lists them. The image smoke (images/claude-code-agent/smoke.sh) runs the REAL
// CLI on a checkout and shows it applying the repository's own .claude/ project
// settings with CLAUDE_PROJECT_DIR = the checkout.

// TestTmuxARepoAgentsCLIStartsInTheCheckout: CCD_REPO given, the CLI starts
// inside the clone, a turn round-trips, and /healthz reports the checkout.
func TestTmuxARepoAgentsCLIStartsInTheCheckout(t *testing.T) {
	gh := newFakeGitHub(t, t.TempDir())
	gh.publish(t, "example-org/app", map[string]string{
		"CLAUDE.md":                 "# app\n\nproject memory\n",
		".claude/skills/x/SKILL.md": "---\nname: x\ndescription: a fixture skill\n---\nbody\n",
	})
	r := startServeAt(t, "workspace/app", nil,
		append(gh.env(), "CCD_REPO=example-org/app", "CCD_REPO_BRANCH=feature")...)

	h := r.waitHealth(t, "started in the checkout", primaryReady)
	if h.Repo == nil || h.Repo.State != checkoutReady || h.Repo.Dir != r.ws || h.Repo.Repo != "example-org/app" {
		t.Fatalf("health repo = %+v", h.Repo)
	}
	argv := r.argvLog(t)
	if !strings.Contains(argv, "cwd="+r.ws+" skills=x\n") {
		t.Fatalf("the CLI did not start in the checkout %s with its skill x:\n%s", r.ws, argv)
	}
	for _, f := range []string{"CLAUDE.md", ".claude/skills/x/SKILL.md", "feature.txt"} {
		if _, err := os.Stat(filepath.Join(r.ws, filepath.FromSlash(f))); err != nil {
			t.Fatalf("%s missing from the checkout: %v", f, err)
		}
	}
	r.chat(t, "work in the repository")
	// muster's own reader (agentgateway.Gateway.Checkout, what the kickoff
	// deliverer gates on) reads this ccd's /healthz as ready.
	if st := musterReadsCheckout(t, r.gwAddr); st.State != agents.CheckoutReady {
		t.Fatalf("muster read the checkout as %+v, want ready", st)
	}
}

// musterReadsCheckout is muster's kickoff deliverer's read of a ccd's checkout:
// the real agentgateway.Gateway, Resolve then Checkout, over HTTP.
func musterReadsCheckout(t *testing.T, addr string) agents.CheckoutStatus {
	t.Helper()
	gw := mustersGateway(t, "http://"+addr)
	target, err := gw.Resolve(context.Background(), contractAgent)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	st, err := gw.Checkout(context.Background(), target)
	if err != nil {
		t.Fatalf("muster's checkout read: %v", err)
	}
	return st
}

// TestTmuxARepoThatCannotBeClonedKeepsTheCLIStoppedAndSaysWhy: the repository
// cannot be read. /healthz stays 200 (no restart loop) and says why; a turn is
// not_ready carrying the clone's failure and what to approve; the CLI never
// starts (no tmux session, the fake never ran); and ccd filed the github
// request once with this agent's token.
func TestTmuxARepoThatCannotBeClonedKeepsTheCLIStoppedAndSaysWhy(t *testing.T) {
	gh := newFakeGitHub(t, t.TempDir())
	fc := &fakeConnectors{grant: "none"}
	srv := fc.server(t)
	r := startServeAt(t, "workspace/private-app", nil,
		append(gh.env(), "CCD_REPO=example-org/private-app", "MUSTER_CONNECT_URL="+srv.URL,
			"MUSTER_HOOK_TOKEN=fixture-agent-token-5c2e")...)

	h := r.waitHealth(t, "waiting on the checkout", func(h health) bool {
		return h.Repo != nil && h.Repo.State == checkoutAwaitingGrant && strings.Contains(h.Repo.Detail, "approve the request ccd filed")
	})
	// 🔴 THE SEAM: muster's reader sees awaiting_grant, the state its kickoff
	// deliverer holds a first turn on (agents.DecideFirstTurn). A ccd that said
	// "failed" here would have the turn sent into not_ready and recorded failed.
	if st := musterReadsCheckout(t, r.gwAddr); st.State != agents.CheckoutAwaitingGrant ||
		!strings.Contains(st.Detail, "approve the request ccd filed") {
		t.Fatalf("muster read the checkout as %+v, want awaiting_grant with ccd's detail", st)
	}
	if !h.OK || h.Session == sessionStartedState {
		t.Fatalf("health %+v", h)
	}
	_, err := mustersGateway(t, "http://"+r.gwAddr).Chat(context.Background(), contractAgent, "s", "hello", nil)
	if err == nil || !strings.Contains(err.Error(), `"type":"not_ready"`) ||
		!strings.Contains(err.Error(), "the repository example-org/private-app could not be cloned") {
		t.Fatalf("turn err = %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if r.hasTmuxSession("cc") {
		t.Fatal("a tmux session exists: the CLI was started without the checkout")
	}
	if _, err := os.Stat(filepath.Join(r.cfg, "fake-argv.log")); err == nil {
		t.Fatalf("the CLI ran:\n%s", r.argvLog(t))
	}
	if reqs := fc.filed(); len(reqs) != 1 || !strings.Contains(reqs[0]["reason"], "clone example-org/private-app") {
		t.Fatalf("requests filed: %+v", reqs)
	}
}

// TestTmuxAnAgentWithNoRepoReportsNoCheckout: no CCD_REPO — today's shape (the
// live agent): no `repo` in /healthz, the CLI in the plain workspace.
func TestTmuxAnAgentWithNoRepoReportsNoCheckout(t *testing.T) {
	r := startServe(t)
	h := r.waitHealth(t, "first start", primaryReady)
	if h.Repo != nil {
		t.Fatalf("a repo-less agent reports a checkout: %+v", h.Repo)
	}
	if st := musterReadsCheckout(t, r.gwAddr); st != (agents.CheckoutStatus{}) {
		t.Fatalf("muster read a repo-less agent's checkout as %+v, want unknown", st)
	}
	if argv := r.argvLog(t); !strings.Contains(argv, "cwd="+r.ws+" skills=\n") {
		t.Fatalf("the CLI did not start in the plain workspace %s:\n%s", r.ws, argv)
	}
}
