package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheGatewayPushRecipeKeepsTheTokenOnTheProxy runs the git command the
// gateway prompt teaches, in a real shell with real git, against a stand-in
// proxy that answers with a redirect to another host. The proxy must see the
// agent's token (control); the other host must see nothing — no token, no
// request.
func TestTheGatewayPushRecipeKeepsTheTokenOnTheProxy(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is required (CI runners have it): ", err)
	}
	const token = "gateway-agent-token-0123456789"
	note := int64(7)
	a := agents.Agent{ID: 1, Name: "pusher", Repo: "example-org/example-repo", NoteID: &note}
	prompt := newConnFixture(t).srv.AgentInstructions(a)
	_, rest, ok := strings.Cut(prompt, "For git: `")
	recipe, _, ok2 := strings.Cut(rest, "`")
	if !ok || !ok2 || !strings.Contains(recipe, "git ") {
		t.Fatalf("the prompt carries no git recipe:\n%s", prompt)
	}
	recipe = strings.Replace(recipe, "<branch>", "HEAD:refs/heads/agent-branch", 1)

	var mu sync.Mutex
	var offHost, proxied []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		offHost = append(offHost, fmt.Sprintf("%s %s %v", r.Method, r.URL, r.Header))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(evil.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.Header.Get("X-Muster-Token"))
		mu.Unlock()
		http.Redirect(w, r, evil.URL+"/steal"+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
	}))
	t.Cleanup(proxy.Close)

	repo := t.TempDir()
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"MUSTER_CONNECT_URL="+proxy.URL, "MUSTER_HOOK_TOKEN="+token)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=a@example.com", "-c", "user.name=a",
		"commit", "-q", "--allow-empty", "-m", "x"}} {
		c := exec.Command(gitBin, args...)
		c.Dir, c.Env = repo, env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	sh := exec.Command("bash", "-c", recipe)
	sh.Dir, sh.Env = repo, env
	out, _ := sh.CombinedOutput() // the push fails either way; what matters is who saw what

	mu.Lock()
	defer mu.Unlock()
	if len(proxied) == 0 || proxied[0] != token {
		t.Fatalf("control: the proxy saw %q — the recipe did not reach it (%s): %s", proxied, recipe, out)
	}
	for _, s := range offHost {
		if strings.Contains(s, token) {
			t.Fatalf("the recipe carried the agent's token to the redirect target: %s\nrecipe: %s", s, recipe)
		}
	}
	if len(offHost) != 0 {
		t.Fatalf("the recipe followed a redirect off the proxy: %v\nrecipe: %s", offHost, recipe)
	}
}

// TestTheGitPushRecipeIsPinned pins the WHOLE recipe. The behavioural test
// above cannot tell a proxy-scoped extraHeader from an unscoped one — with
// redirects off, `git push <proxy-url>` only ever talks to that URL — so the
// scoping (which keeps the token off any other URL git might be pointed at in
// the same command) is held here, literally.
func TestTheGitPushRecipeIsPinned(t *testing.T) {
	const want = `git -c "http.$MUSTER_CONNECT_URL/connect/github/.extraHeader=X-Muster-Token: $MUSTER_HOOK_TOKEN" ` +
		`-c "http.$MUSTER_CONNECT_URL/connect/github/.followRedirects=false" ` +
		`push $MUSTER_CONNECT_URL/connect/github/github.com/example-org/example-repo.git`
	if got := GitPushRecipe("github", "github.com/example-org/example-repo.git"); got != want {
		t.Fatalf("GitPushRecipe =\n%s\nwant\n%s", got, want)
	}
}

// TestTheRequestToolSaysWhyPastTheCap: past the per-agent pending cap the
// connector_request tool returns the cap's own message (what to do next), not a
// bare "the tool failed".
func TestTheRequestToolSaysWhyPastTheCap(t *testing.T) {
	f := newConnFixture(t)
	id := f.seedAgent(t, "tool-cap-agent", "tool-cap-agent-token-0123456789")
	a := agents.Agent{ID: id, Name: "tool-cap-agent"}
	var last string
	for i := 0; i < 6; i++ {
		last = f.srv.dispatchAgentTool(f.ctx, a, "connector_request", fmt.Sprintf(`{"name":"want-%d"}`, i))
	}
	if !strings.Contains(last, "waiting") || strings.Contains(last, "the tool failed") {
		t.Fatalf("the sixth request answered %s, want the cap's message", last)
	}
}

// TestTheMCPInstructionsSayGitFollowsTheGrant: a claude-code agent is told git
// routing follows an approval and a revocation by itself, and how to apply it
// at once.
func TestTheMCPInstructionsSayGitFollowsTheGrant(t *testing.T) {
	in := mcpInstructions()
	for _, want := range []string{"ccd connect-git <connector>", "revoked", ProxyRecipe("<connector>", "<host>")} {
		if !strings.Contains(in, want) {
			t.Errorf("the MCP instructions do not say %q:\n%s", want, in)
		}
	}
}
