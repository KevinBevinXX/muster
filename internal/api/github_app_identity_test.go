package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/pins"
)

// listingApp is a GitHub App stand-in for the API's identity readers: it lists
// fixed installation repositories and records which agent asked for PRs.
type listingApp struct {
	accounts []string
	repos    []string
	problem  string
	prAgent  int64
}

func (l *listingApp) Repos(context.Context) ([]github.Repo, error) {
	out := make([]github.Repo, 0, len(l.repos))
	for _, n := range l.repos {
		out = append(out, github.Repo{FullName: n, Private: true})
	}
	return out, nil
}
func (l *listingApp) Accounts(context.Context) ([]string, error) { return l.accounts, nil }
func (l *listingApp) DefaultBranch(context.Context, string) (string, error) {
	return "trunk", nil
}
func (l *listingApp) ListPullRequests(_ context.Context, agentID int64, _, _ string) ([]github.PullRequest, error) {
	l.prAgent = agentID
	return []github.PullRequest{{Number: 3, Title: "from the app"}}, nil
}
func (l *listingApp) Problem(int64) string { return l.problem }

// TestTheReposTabListsTheAppsInstallationRepositories: with the App set, the
// tab is the installation's list (labelled by account, no Disconnect) — even
// when an OAuth connection also exists, which is listed nowhere.
func TestTheReposTabListsTheAppsInstallationRepositories(t *testing.T) {
	s, h, _ := kindsServer(t, stubKinds{pick: "work"})
	connectRepos(t, s, "oauth-account/only-via-oauth")
	s.ext.GitHubApp = &listingApp{accounts: []string{"example-org"}, repos: []string{"example-org/widget", "example-org/gadget"}}
	req := httptest.NewRequest(http.MethodGet, "/ui/repos", nil)
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"data-github-app", "example-org", "example-org/widget", "example-org/gadget"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Repos tab does not show %q:\n%s", want, body)
		}
	}
	for _, not := range []string{"only-via-oauth", "/github/disconnect"} {
		if strings.Contains(body, not) {
			t.Errorf("the Repos tab shows %q although the App is the identity:\n%s", not, body)
		}
	}
	// The App's problem is on the tab.
	s.ext.GitHubApp = &listingApp{accounts: []string{"example-org"}, problem: "GitHub refused the GitHub App's credentials (401)"}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/ui/repos", nil)
	admit(s, req)
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "data-github-app-problem") || !strings.Contains(rec.Body.String(), "(401)") {
		t.Fatalf("the App's problem is not on the Repos tab:\n%s", rec.Body)
	}
}

// TestAClaudeCodeDispatchChecksTheAppsRepositoriesFirst: the App takes
// precedence over the OAuth connection at the dispatch door — an App repository
// is accepted, and an OAuth-only one is refused while the App is configured.
func TestAClaudeCodeDispatchChecksTheAppsRepositoriesFirst(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	connectRepos(t, s, "oauth-account/only-via-oauth")
	s.ext.GitHubApp = &listingApp{repos: []string{"example-org/widget"}}
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"oauth-account/only-via-oauth"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), taskDispatchRepo) || len(store.rows()) != 0 {
		t.Fatalf("an OAuth-only repository under the App: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	rec = postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"Example-Org/Widget"}})
	if rec.Code != http.StatusOK || len(store.rows()) != 1 {
		t.Fatalf("an installation repository: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	if b := store.rows()[0].RepoBranch; b != "trunk" {
		t.Fatalf("the default branch came from %q, want the App's (trunk)", b)
	}
}

// TestTheOAuthConnectionStillDecidesWithoutAnApp: the fallback — no App, the
// OAuth connection's repositories are the connected ones, as before.
func TestTheOAuthConnectionStillDecidesWithoutAnApp(t *testing.T) {
	s, _, _ := kindsServer(t, stubKinds{pick: "work"})
	if why := s.connectedRepo(context.Background(), "oauth-account/repo"); why != taskDispatchNoGitHub {
		t.Fatalf("no App, no OAuth: %q", why)
	}
	connectRepos(t, s, "oauth-account/repo")
	if why := s.connectedRepo(context.Background(), "oauth-account/repo"); why != "" {
		t.Fatalf("no App, OAuth-connected repo refused: %q", why)
	}
	if why := s.connectedRepo(context.Background(), "example-org/widget"); !strings.Contains(why, taskDispatchRepo) {
		t.Fatalf("no App, an unconnected repo: %q", why)
	}
}

// TestTaskDispatchsEffectiveRepoIsCheckedAgainstTheApp: task_dispatch's
// effective-repository check (#150) uses the App's installation repositories
// for an ADMIN owner's agent — and, for a MEMBER owner's agent (R2), the App's
// repositories are not connected at all.
func TestTaskDispatchsEffectiveRepoIsCheckedAgainstTheApp(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "needs a repo")
	f.srv.ext.GitHubApp = &listingApp{repos: []string{"example/app"}}
	callAs := func(role authz.Role, args map[string]any) string {
		p, _ := authz.From(f.asOwner)
		p.Role = role
		raw, _ := json.Marshal(args)
		return f.srv.dispatchAgentTool(authz.WithPrincipal(f.asOwner, p), f.agent, taskDispatchTool, string(raw))
	}
	f.mustRefuse(t, callAs(authz.RoleMember, map[string]any{"task_id": n.ID, "repo": "example/app"}), taskDispatchNoGitHub, n.ID)
	f.mustRefuse(t, callAs(authz.RoleAdmin, map[string]any{"task_id": n.ID, "repo": "stranger/app"}), taskDispatchRepo, n.ID)
	text := callAs(authz.RoleAdmin, map[string]any{"task_id": n.ID, "repo": "example/app"})
	if strings.Contains(text, `"error"`) {
		t.Fatalf("an installation repository, admin owner: %s", text)
	}
}

// TestAMemberGetsNoAppRepositories (R2): for a member principal the App's
// repositories are not listed, not connected, and the Repos tab is the
// member's own (here: none) — while an admin principal gets the App's.
func TestAMemberGetsNoAppRepositories(t *testing.T) {
	s, h, _ := kindsServer(t, stubKinds{pick: "work"})
	s.ext.GitHubApp = &listingApp{accounts: []string{"example-org"}, repos: []string{"example-org/widget"}}
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if why := s.connectedRepo(member, "example-org/widget"); why == "" {
		t.Fatal("a member's dispatch check accepted an App repository")
	}
	if why := s.connectedRepo(admin, "example-org/widget"); why != "" {
		t.Fatalf("control: the admin's dispatch check refused an App repository: %q", why)
	}
	if list, err := s.githubRepos(member); err == nil && len(list) > 0 {
		t.Fatalf("a member's repository list holds App repositories: %+v", list)
	}
	for _, c := range []struct {
		ctx  context.Context
		want bool
	}{{member, false}, {admin, true}} {
		req := httptest.NewRequest(http.MethodGet, "/ui/repos", nil).WithContext(c.ctx)
		rec := httptest.NewRecorder()
		s.handleReposContent(rec, req)
		if got := strings.Contains(rec.Body.String(), "example-org/widget"); got != c.want {
			t.Errorf("Repos tab shows the App repository = %v, want %v:\n%s", got, c.want, rec.Body)
		}
	}
	_ = h
}

// TestTheAgentCardShowsTheAppsProblemForARepoAgent.
func TestTheAgentCardShowsTheAppsProblemForARepoAgent(t *testing.T) {
	s, _, _ := kindsServer(t, stubKinds{pick: "work"})
	s.ext.GitHubApp = &listingApp{problem: "GitHub refused the GitHub App's credentials (401) while minting"}
	withRepo := s.cardViewIndexed(agents.Agent{ID: 1, Name: "a", Repo: "example-org/widget", OwnerID: 1}, nil, time.Time{}, 0,
		pins.Set{}, nil)
	if !strings.Contains(withRepo.GitHubProblem, "(401)") {
		t.Fatalf("a repository agent's card carries no App problem: %+v", withRepo.GitHubProblem)
	}
	noRepo := s.cardViewIndexed(agents.Agent{ID: 2, Name: "b"}, nil, time.Time{}, 0,
		pins.Set{}, nil)
	if noRepo.GitHubProblem != "" {
		t.Fatalf("an agent without a repository shows the App's problem: %q", noRepo.GitHubProblem)
	}
	memberOwned := s.cardViewIndexed(agents.Agent{ID: 3, Name: "c", Repo: "example-org/widget", OwnerID: 2}, nil, time.Time{}, 0,
		pins.Set{}, nil)
	if memberOwned.GitHubProblem != "" {
		t.Fatalf("a member's agent shows the (admin-only) App's problem: %q", memberOwned.GitHubProblem)
	}
}
