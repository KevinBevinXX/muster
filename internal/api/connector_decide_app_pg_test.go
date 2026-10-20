package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnMCPApprovalOfTheAppsGithubGrantsOnlyTheAgentsOwnRepository: with the
// GitHub App configured, a mirrored session lists two pending `github`
// requests — an agent dispatched to example-org/widget, and one dispatched with
// NO repository — and approves both over MCP. Both grants are approved against
// the App (source 'app'), exactly as the tab's approval records them. The
// listing and the decision say the no-repository approval grants nothing
// usable, and the proxy agrees: that agent's request is refused, while the
// other agent's request to its own repository is served. (It answers, by
// behaviour, whether an approval can make an agent with repo "" push: it
// cannot.) The CONTROL for the note is the agent with a repository, whose
// listing says what it gets instead.
func TestAnMCPApprovalOfTheAppsGithubGrantsOnlyTheAgentsOwnRepository(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, "example-org/widget")
	const noRepoTok, repoTok = "norepo-agent-token-0123456789abcdef", "repo-agent-token-0123456789abcdef0"
	noRepo := f.seedAgent(t, "norepo-fixture", noRepoTok)
	withRepo := f.seedAgent(t, "repo-fixture", repoTok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, withRepo); err != nil {
		t.Fatal(err)
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := githubapp.New(githubapp.Config{AppID: 12345, PrivateKey: k, BaseURL: up.srv.URL,
		GitHost: "github.fake.test:" + up.port(), APIHost: "api.fake.test:" + up.port(), Accounts: []githubapp.Account{{Login: "example-org", ID: 4200}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := mirrorTok('a')
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: connectors.WithVirtual(f.store, app, authz.IsAdmin),
		GitHubApp: app, ConnectorPolicy: fakeTestPolicy(),
		Mirror:       &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: attnWindows(mirror.StatusWaiting)},
		MirrorTokens: fakeMirrorTokens{ws: mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}}})
	h := s.Handler()

	ids := map[int64]int64{}
	for _, a := range []int64{noRepo, withRepo} {
		g, _, err := f.store.RequestGrant(f.ctx, a, "github", "clone")
		if err != nil {
			t.Fatal(err)
		}
		ids[a] = g.ID
	}
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		w := serveCall(t, h, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(name, args)),
			map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + ws, mirrorSessionHeader: focusSID})
		var r mcpReply
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.Error != nil {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		return toolText(t, &r)
	}

	text, isErr := call("connector_requests_list", map[string]any{})
	var out struct {
		Requests []struct {
			ID      int64  `json:"id"`
			Repo    string `json:"repo"`
			Grants  string `json:"approving_grants"`
			Problem string `json:"problem"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || len(out.Requests) != 2 {
		t.Fatalf("connector_requests_list: %s", text)
	}
	for _, r := range out.Requests {
		switch r.ID {
		case ids[noRepo]:
			if r.Repo != "" || !strings.HasPrefix(r.Grants, "approving grants NOTHING usable") {
				t.Errorf("the no-repository request: %+v", r)
			}
		case ids[withRepo]:
			if r.Repo != "example-org/widget" || !strings.Contains(r.Grants, "1-hour token limited to it") || strings.Contains(r.Grants, "NOTHING") {
				t.Errorf("control, the request with a repository: %+v", r)
			}
		}
	}

	for _, a := range []int64{noRepo, withRepo} {
		text, isErr := call("connector_request_decide", map[string]any{"id": ids[a], "decision": "approve"})
		if isErr || !strings.Contains(text, `"status":"approved"`) || strings.Contains(text, "NOTHING usable") != (a == noRepo) {
			t.Fatalf("approve %d: %s", ids[a], text)
		}
		var st, src, from string
		if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT status, source, coalesce(decided_via_host || '/' || decided_via_session, '')
			FROM connector_grants WHERE id = $1`, ids[a]).Scan(&st, &src, &from); err != nil ||
			st != connectors.StatusApproved || src != connectors.SourceApp || from != "ws/"+focusSID {
			t.Fatalf("grant %d: %s %s %q %v", ids[a], st, src, from, err)
		}
	}

	api := "/connect/github/api.fake.test:" + up.port() + "/repos/example-org/widget/pulls"
	if w := serveCall(t, h, http.MethodGet, api, nil, map[string]string{"X-Muster-Token": noRepoTok}); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "dispatched without one") {
		t.Fatalf("the approved no-repository agent's github request: %d %s", w.Code, w.Body)
	}
	if w := serveCall(t, h, http.MethodGet, api, nil, map[string]string{"X-Muster-Token": repoTok}); w.Code != http.StatusOK {
		t.Fatalf("control, the agent's own repository: %d %s", w.Code, w.Body)
	}
}
