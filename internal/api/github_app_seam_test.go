package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// THE GITHUB APP AT ITS SEAMS — synthetic throughout: a fake GitHub on loopback
// (placeholder account example-org, placeholder repositories, counter-minted
// tokens), a fresh RSA key, and a real `git` cloning through muster's real
// proxy from a real `git http-backend`.

// fakeGitHubUpstream is the GitHub API (installations, token mints) AND git's
// smart-HTTP server for bare repositories under root, on one loopback listener.
// A git request is served only when it carries basic x-access-token:<a token
// this fake minted FOR THAT REPOSITORY> — GitHub's own enforcement, modelled.
type fakeGitHubUpstream struct {
	srv  *httptest.Server
	root string

	mu    sync.Mutex
	mints []string // request bodies, in order
	// refuseApp answers every App-JWT call 401, as GitHub does for a key that
	// is not the App's or a JWT outside its validity window.
	refuseApp bool
	// public repositories are served to anyone, with no Authorization.
	public  map[string]bool
	minted  map[string]string // token -> repository name it was minted for
	gitSeen []string          // "<path> <authorization>" per git request
	apiSeen []string          // the Authorization of each /repos/ request
}

var mintedRepoName = regexp.MustCompile(`"repositories":\["([^"]+)"\]`)

func newFakeGitHubUpstream(t *testing.T) *fakeGitHubUpstream {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is required (CI runners have it): ", err)
	}
	f := &fakeGitHubUpstream{root: t.TempDir(), minted: map[string]string{}, public: map[string]bool{}}
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + f.root, "GIT_HTTP_EXPORT_ALL=1"}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/app/") && f.refuseApp:
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		case p == "/app/installations":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, `{"message":"no JWT"}`, http.StatusUnauthorized)
				return
			}
			// A second installation, on an account NOT on the allowlist: a stranger
			// who installed the public App on their own repositories.
			_, _ = io.WriteString(w, `[{"id":42,"account":{"login":"example-org"}},{"id":77,"account":{"login":"stranger"}}]`)
		case p == "/app/installations/77/access_tokens":
			// A real installation: it mints, and its token lists the stranger's
			// repository. Only the allowlist keeps muster from using it.
			f.mu.Lock()
			f.mints = append(f.mints, "STRANGER INSTALLATION")
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "stranger-installation-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case p == "/app/installations/42/access_tokens":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.mints = append(f.mints, string(body))
			tok := fmt.Sprintf("fake-seam-token-%04d", len(f.mints))
			if m := mintedRepoName.FindStringSubmatch(string(body)); m != nil {
				f.minted[tok] = m[1]
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": tok,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case p == "/installation/repositories":
			if r.Header.Get("Authorization") == "token stranger-installation-token" {
				_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"full_name":"stranger/payload","private":false}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"full_name":"example-org/widget","private":true}]}`)
		case strings.HasPrefix(p, "/repos/"):
			// The REST API: record and echo what arrived (the echo exercises the
			// proxy's redactor; the record is what GitHub was actually sent).
			f.mu.Lock()
			f.apiSeen = append(f.apiSeen, r.Header.Get("Authorization"))
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"path": p, "authorization": r.Header.Get("Authorization")})
		default:
			auth := r.Header.Get("Authorization")
			f.mu.Lock()
			f.gitSeen = append(f.gitSeen, p+" "+auth)
			f.mu.Unlock()
			// owner/name.git/...: the token must have been minted for `name`.
			segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
			if auth == "" && len(segs) >= 2 && f.public[segs[0]+"/"+strings.TrimSuffix(segs[1], ".git")] {
				backend.ServeHTTP(w, r)
				return
			}
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
			user, tok, _ := strings.Cut(string(raw), ":")
			f.mu.Lock()
			repoName := f.minted[tok]
			f.mu.Unlock()
			if len(segs) < 2 || user != "x-access-token" || repoName == "" || repoName+".git" != segs[1] {
				w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			backend.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// addRepo creates a bare repository owner/name.git with one commit holding
// README.md.
func (f *fakeGitHubUpstream) addRepo(t *testing.T, fullName string) {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(f.root, fullName+".git")
	env := gitTestEnv(t)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", work},
		{"-C", work, "-c", "user.email=a@example.com", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		runGit(t, env, args...)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("synthetic "+fullName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, env, "-C", work, "add", "README.md")
	runGit(t, env, "-C", work, "-c", "user.email=a@example.com", "-c", "user.name=a", "commit", "-q", "-m", "readme")
	runGit(t, env, "clone", "-q", "--bare", work, bare)
}

func (f *fakeGitHubUpstream) port() string {
	_, p, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	return p
}

func gitTestEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func runGit(t *testing.T, env []string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// fakeTestPolicy maps *.fake.test to loopback over plain HTTP, as the e2e build
// does; every other check is production's.
func fakeTestPolicy() connectors.Policy {
	return connectors.Policy{AllowLoopback: true, InsecureHTTP: true,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasSuffix(host, ".fake.test") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("no such host %s", host)
		}}
}

// appServer is a Server over f's pool and connector store with the GitHub App
// configured — the server as it is after a restart that set
// MUSTER_GITHUB_APP_ID / _PRIVATE_KEY (cmd/muster-server's buildApp wiring).
func appServer(t *testing.T, f *connFixture, up *fakeGitHubUpstream) (*Server, http.Handler, *githubapp.App) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := githubapp.New(githubapp.Config{AppID: 12345, PrivateKey: k, BaseURL: up.srv.URL,
		GitHost: "github.fake.test:" + up.port(), APIHost: "api.fake.test:" + up.port(), Accounts: []string{"example-org"}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: connectors.WithVirtual(f.store, app, authz.IsAdmin),
		GitHubApp: app, ConnectorPolicy: fakeTestPolicy()})
	return s, s.Handler(), app
}

func serveCall(t *testing.T, h http.Handler, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func agentView(t *testing.T, h http.Handler, token string) AgentConnectorView {
	t.Helper()
	w := serveCall(t, h, "GET", "/agent/connectors/github", nil, map[string]string{"X-Muster-Token": token})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /agent/connectors/github = %d %s", w.Code, w.Body)
	}
	var v AgentConnectorView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestAPendingGitHubRequestFiledBeforeTheAppBecomesApprovableAndTheCloneSucceeds
// is the live case, at the seam: an agent dispatched to a private repository
// filed its `github` request when NO github connector existed (ccd does that on
// its first failed clone). The server then restarts with the GitHub App
// configured. The SAME pending request — not re-filed, no row edited — must now
// show Approve, approving it must succeed, and the agent's next clone (ccd's
// retry, through muster's proxy exactly as ccd's git routing sends it) must
// check the repository out with a token minted for that repository alone.
func TestAPendingGitHubRequestFiledBeforeTheAppBecomesApprovableAndTheCloneSucceeds(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, "example-org/widget")
	up.addRepo(t, "example-org/gadget")
	up.addRepo(t, "public-org/dependency")
	up.public["public-org/dependency"] = true
	const tok = "seam-agent-token-0123456789abcdef"
	id := f.seedAgent(t, "quiet-fixture", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	// 1. Before the App: the request is filed for a connector that does not exist.
	w := f.call(t, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"clone example-org/widget"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("filing the request: %d %s", w.Code, w.Body)
	}
	var filed AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &filed)
	if filed.Exists || filed.Grant != connectors.StatusPending || filed.GrantID == 0 {
		t.Fatalf("control: before the App the request must be pending on a connector that does not exist: %+v", filed)
	}
	page := f.call(t, "GET", "/ui/connectors", nil, f.operator()).Body.String()
	if strings.Contains(page, `data-approve="`+strconv.FormatInt(filed.GrantID, 10)+`"`) {
		t.Fatal("control: the request card offered Approve before any github connector existed")
	}

	// 2. The server restarts with the GitHub App configured.
	srv, h, _ := appServer(t, f, up)
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}

	v := agentView(t, h, tok)
	if !v.Exists || v.Grant != connectors.StatusPending || v.GrantID != filed.GrantID {
		t.Fatalf("after the App: the agent sees %+v, want the SAME pending grant %d on an existing connector", v, filed.GrantID)
	}
	page = serveCall(t, h, "GET", "/ui/connectors", nil, op).Body.String()
	if !strings.Contains(page, `data-approve="`+strconv.FormatInt(filed.GrantID, 10)+`"`) {
		t.Fatalf("the existing request card does not offer Approve once the App backs github:\n%s", page)
	}

	// 3. The owner approves the existing request.
	w = serveCall(t, h, "POST", "/ui/connector-requests/"+strconv.FormatInt(filed.GrantID, 10)+"/approve", nil, op)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "approved for github") {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if v := agentView(t, h, tok); !v.Exists || v.Grant != connectors.StatusApproved {
		t.Fatalf("after approval the agent sees %+v; ccd routes git only when exists && approved", v)
	}

	// 4. The agent's next clone, through muster's proxy as ccd's routing sends it.
	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	env := gitTestEnv(t)
	clone := func(repo, dir string) ([]byte, error) {
		c := exec.Command("git", "-c", "http."+proxy+".extraHeader=X-Muster-Token: "+tok,
			"-c", "http."+proxy+".followRedirects=false",
			"clone", "-q", proxy+"github.fake.test:"+up.port()+"/"+repo+".git", dir)
		c.Env = env
		return c.CombinedOutput()
	}
	dir := filepath.Join(t.TempDir(), "checkout")
	if out, err := clone("example-org/widget", dir); err != nil {
		t.Fatalf("the approved agent's clone failed: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || !strings.Contains(string(b), "example-org/widget") {
		t.Fatalf("the checkout has no README from the repository: %q %v", b, err)
	}
	up.mu.Lock()
	mints, seen := append([]string(nil), up.mints...), append([]string(nil), up.gitSeen...)
	up.mu.Unlock()
	const wantMint = `{"repositories":["widget"],"permissions":{"contents":"write","pull_requests":"write","checks":"read","statuses":"read"}}`
	if len(mints) != 1 || mints[0] != wantMint {
		t.Fatalf("mints = %q, want exactly one: %s", mints, wantMint)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:fake-seam-token-0001"))
	for _, s := range seen {
		if !strings.HasSuffix(s, " "+wantAuth) {
			t.Errorf("a git request reached GitHub without the minted token: %s", s)
		}
	}
	if len(seen) == 0 {
		t.Fatal("positive control: no git request reached the fake GitHub")
	}

	// 5. The same agent, another repository: forwarded ANONYMOUSLY — a public
	// dependency clones, a private one fails as it would for anyone, nothing is
	// minted for either, and no request for them carries a credential.
	if out, err := clone("public-org/dependency", filepath.Join(t.TempDir(), "dep")); err != nil {
		t.Fatalf("a public dependency did not clone through the anonymous pass-through: %v\n%s", err, out)
	}
	if out, err := clone("example-org/gadget", filepath.Join(t.TempDir(), "other")); err == nil {
		t.Fatalf("the agent cloned a PRIVATE repository that is not its own:\n%s", out)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.mints) != 1 {
		t.Fatalf("a token was minted for another repository: %q", up.mints)
	}
	others := up.gitSeen[len(seen):]
	if len(others) == 0 {
		t.Fatal("positive control: no request for the other repositories reached GitHub")
	}
	for _, s := range others {
		if !strings.HasSuffix(s, " ") {
			t.Errorf("a request for another repository carried a credential: %s", s)
		}
	}
}

// TestTheProxyInjectsTheAppTokenForTheAgentsRepoAndRefusesAnother, over the
// REST host: the agent's own /repos/... gets the minted token injected (and
// never sees it in the response — the redactor); another repository is a 403
// from muster naming why, and nothing reaches GitHub for it.
func TestTheProxyInjectsTheAppTokenForTheAgentsRepoAndRefusesAnother(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	const tok = "proxy-agent-token-0123456789abcdef"
	id := f.seedAgent(t, "proxy-fixture", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, h, _ := appServer(t, f, up)
	g, _, err := f.store.RequestGrant(f.ctx, id, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceApp); err != nil {
		t.Fatal(err)
	}
	api := "/connect/github/api.fake.test:" + up.port() + "/repos/example-org/"
	agentHdr := map[string]string{"X-Muster-Token": tok}

	w := serveCall(t, h, "GET", api+"widget/pulls", nil, agentHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("own repository: %d %s", w.Code, w.Body)
	}
	var echo map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &echo)
	up.mu.Lock()
	n, sent := len(up.mints), append([]string(nil), up.apiSeen...)
	up.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d mints for one request", n)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:fake-seam-token-0001"))
	if len(sent) != 1 || sent[0] != wantAuth {
		t.Fatalf("GitHub was sent Authorization %q, want the minted token as basic x-access-token (%q)", sent, wantAuth)
	}
	if strings.Contains(w.Body.String(), "fake-seam-token-0001") || strings.Contains(w.Body.String(), wantAuth[len("Basic "):]) ||
		!strings.Contains(echo["authorization"], connectors.RedactedMarker) {
		t.Fatalf("the upstream's echo of the injected credential reached the agent: %q", echo["authorization"])
	}

	// Another repository: anonymous — no Authorization reaches GitHub, no mint.
	w = serveCall(t, h, "GET", api+"gadget/pulls", nil, agentHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("another repository: %d %s", w.Code, w.Body)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.apiSeen) != 2 || up.apiSeen[1] != "" {
		t.Fatalf("the other repository's request reached GitHub with Authorization %q, want none", up.apiSeen)
	}
	if len(up.mints) != 1 {
		t.Fatalf("a token was minted for another repository: %q", up.mints)
	}
}

// TestTheManagedGitHubConnectorCannotBeReplacedByAStoredOne: with the App
// configured, creating/rotating/deleting `github` is refused (the App is the
// identity), and the card says why there are no buttons.
func TestTheManagedGitHubConnectorCannotBeReplacedByAStoredOne(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	srv, h, _ := appServer(t, f, up)
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}
	body, hdr := form(map[string][]string{"preset": {"github"}, "name": {"github"}, "secret": {connTestSecret}})
	w := serveCall(t, h, "POST", "/connectors", body, merge(op, hdr))
	if !strings.Contains(w.Body.String(), "provided by the server") {
		t.Fatalf("creating a stored github connector under the App: %d %s", w.Code, w.Body)
	}
	if _, err := f.store.GetConnector(f.ctx, "github"); err == nil {
		t.Fatal("a stored github row was created while the App is configured")
	}
	page := serveCall(t, h, "GET", "/ui/connectors", nil, op).Body.String()
	if !strings.Contains(page, `data-connector-managed="github"`) || strings.Contains(page, `data-delete="github"`) {
		t.Fatalf("the github card is not marked managed / still offers Delete:\n%s", page)
	}
}

// TestTheRequestCardShowsTheAppsProblem: when the App's credentials are refused
// (here: GitHub's 401 at the boot check), the pending github request card says
// so — the owner learns an approval alone will not make the clone succeed —
// and so does the github connector's card.
func TestTheRequestCardShowsTheAppsProblem(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.refuseApp = true
	const tok = "problem-agent-token-0123456789abcdef"
	id := f.seedAgent(t, "problem-fixture", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	srv, h, app := appServer(t, f, up)
	if _, err := app.Check(context.Background()); err == nil {
		t.Fatal("control: the boot check passed against a GitHub refusing the App")
	}
	if _, _, err := f.store.RequestGrant(f.ctx, id, "github", "clone"); err != nil {
		t.Fatal(err)
	}
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}
	page := serveCall(t, h, "GET", "/ui/connectors", nil, op).Body.String()
	if strings.Count(page, "data-connector-problem") < 2 || !strings.Contains(page, "(401)") ||
		!strings.Contains(page, "MUSTER_GITHUB_APP_PRIVATE_KEY") {
		t.Fatalf("the request card and the connector card do not both show the App's 401 problem:\n%s", page)
	}
}

// TestAMembersAgentGetsNoAppConnectorAndCannotBeApprovedOntoIt (R2): on a
// multi-user muster the App is the admin's. A MEMBER's agent sees no github
// connector (exists=false), its pending request is not approvable through the
// App — whoever clicks Approve — and the proxy mints nothing for it. The
// admin's own agent, in the same server, still gets all three (control).
func TestAMembersAgentGetsNoAppConnectorAndCannotBeApprovedOntoIt(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, "example-org/widget")
	srv, h, _ := appServer(t, f, up)
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}

	var member int64
	sys := authz.System(f.ctx)
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"gh-app-member-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s := authz.System(context.Background())
		_, _ = f.pool.Exec(s, `DELETE FROM agents WHERE owner_id = $1`, member)
		_, _ = f.pool.Exec(s, `DELETE FROM users WHERE id = $1`, member)
	})
	const mtok, atok = "member-agent-token-0123456789abcdef", "admin-agent-token-0123456789abcdefg"
	var mid int64
	if err := f.pool.QueryRow(sys, `INSERT INTO agents (name, namespace, hooks_token, owner_id, repo)
		VALUES ('member-gh', 'ns-member-gh', $1, $2, 'example-org/widget') RETURNING id`, mtok, member).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	aid := f.seedAgent(t, "admin-gh", atok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}

	if v := agentView(t, h, mtok); v.Exists {
		t.Fatalf("a member's agent sees the App-backed github connector: %+v", v)
	}
	if v := agentView(t, h, atok); !v.Exists {
		t.Fatalf("control: the admin's agent does not see it: %+v", v)
	}

	// The member's agent files its request; the operator (an admin) tries to
	// approve it. It must stay pending: for its OWNER there is no github
	// connector to approve it onto.
	w := serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{}`),
		map[string]string{"X-Muster-Token": mtok, "Content-Type": "application/json"})
	var filed AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &filed)
	if filed.GrantID == 0 {
		t.Fatalf("filing the member's request: %d %s", w.Code, w.Body)
	}
	_ = serveCall(t, h, "POST", "/ui/connector-requests/"+strconv.FormatInt(filed.GrantID, 10)+"/approve", nil, op)
	g, err := f.store.GetGrant(sys, filed.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status == connectors.StatusApproved {
		t.Fatalf("a member's agent was approved onto the App-backed github connector (source %q)", g.Source)
	}

	// The MEMBER approves their own agent's request (the approve handler under
	// the member's principal — a member session; requireSession is not what is
	// under test). It must not be approvable onto the App.
	memberCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: member, Role: authz.RoleMember, Via: authz.ViaSession})
	req := httptest.NewRequest("POST", "/ui/connector-requests/"+strconv.FormatInt(filed.GrantID, 10)+"/approve", nil).WithContext(memberCtx)
	req.SetPathValue("id", strconv.FormatInt(filed.GrantID, 10))
	rec := httptest.NewRecorder()
	srv.handleConnectorApprove(rec, req)
	if g, _ := f.store.GetGrant(sys, filed.GrantID); g.Status == connectors.StatusApproved {
		t.Fatalf("a member approved their own agent onto the App-backed github connector (source %q)", g.Source)
	}
	if !strings.Contains(rec.Body.String(), "does not exist") {
		t.Fatalf("control: the member's approve did not reach the existence check: %s", rec.Body)
	}

	// Even with an approval forced into the row, the proxy mints nothing for
	// the member's agent: for its owner, github is not the App.
	if _, err := f.store.DecideGrant(sys, filed.GrantID, true, "forced", connectors.SourceApp); err != nil {
		t.Fatal(err)
	}
	_ = serveCall(t, h, "GET", "/connect/github/github.fake.test:"+up.port()+"/example-org/widget.git/info/refs",
		nil, map[string]string{"X-Muster-Token": mtok})
	up.mu.Lock()
	n := len(up.mints)
	up.mu.Unlock()
	if n != 0 {
		t.Fatalf("the proxy minted an App token for a member's agent: %d mint(s)", n)
	}
}

// TestAGrantApprovedUnderTheStoredTokenNeedsReapprovalOnceTheAppIsConfigured
// (Y4), through the real routes: a github grant approved against a stored
// token (source stored) reads PENDING to the agent once the App backs github,
// and so does an App-approved grant once the App is gone — so ccd stops
// routing, and the owner is asked again.
func TestAGrantApprovedUnderTheStoredTokenNeedsReapprovalOnceTheAppIsConfigured(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	const tok = "reopen-agent-token-0123456789abcdef"
	id := f.seedAgent(t, "reopen-gh", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	g, _, err := f.store.RequestGrant(f.ctx, id, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceStored); err != nil {
		t.Fatal(err)
	}
	if v := agentView(t, f.h, tok); v.Grant != connectors.StatusApproved {
		t.Fatalf("control: without the App the stored approval stands: %+v", v)
	}
	// A second agent, approved the same way, is read only through the PROXY,
	// so each door's re-open is exercised on its own (neither can lean on the
	// other having re-opened the grant first).
	const ptok = "reopen-proxy-agent-token-0123456789ab"
	pid := f.seedAgent(t, "reopen-gh-proxy", ptok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	pg, _, err := f.store.RequestGrant(f.ctx, pid, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, pg.ID, true, "operator", connectors.SourceStored); err != nil {
		t.Fatal(err)
	}
	_, h, _ := appServer(t, f, up)
	// The PROXY: the stale stored-token approval must not be served with an
	// App token.
	w := serveCall(t, h, "GET", "/connect/github/github.fake.test:"+up.port()+"/example-org/widget.git/info/refs",
		nil, map[string]string{"X-Muster-Token": ptok})
	up.mu.Lock()
	minted := len(up.mints)
	up.mu.Unlock()
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"grant":"pending"`) || minted != 0 {
		t.Fatalf("the proxy served a stored-token approval with the App (%d mints): %d %s", minted, w.Code, w.Body)
	}
	if v := agentView(t, h, tok); v.Grant != connectors.StatusPending || v.GrantID != g.ID {
		t.Fatalf("with the App, a stored-token approval still reads %+v; want the same grant pending again", v)
	}
	// Approve it under the App: source app, approved.
	srv, h2, _ := appServer(t, f, up)
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}
	_ = serveCall(t, h2, "POST", "/ui/connector-requests/"+strconv.FormatInt(g.ID, 10)+"/approve", nil, op)
	if ng, _ := f.store.GetGrant(f.ctx, g.ID); ng.Status != connectors.StatusApproved || ng.Source != connectors.SourceApp {
		t.Fatalf("approving under the App: %+v", ng)
	}
	// The App removed: the App approval must not start serving a stored token.
	if v := agentView(t, f.h, tok); v.Grant != connectors.StatusPending {
		t.Fatalf("without the App, an App approval still reads %+v", v)
	}
}

// TestAStrangersInstallationConnectsNothing (R1): the App is public, so a
// stranger installs it on stranger/payload. With the allowlist naming only
// example-org, that repository is not a connected one — the chief's
// task_dispatch and every dispatch door refuse it — and nothing is minted on
// the stranger's installation. The admin's real repository is accepted
// (control).
func TestAStrangersInstallationConnectsNothing(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	srv, _, _ := appServer(t, f, up)
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if why := srv.connectedRepo(admin, "stranger/payload"); !strings.Contains(why, chiefDispatchRepo) {
		t.Fatalf("a stranger's installation repository is connected: %q", why)
	}
	if why := srv.connectedRepo(admin, "example-org/widget"); why != "" {
		t.Fatalf("control: the allowlisted installation's repository is refused: %q", why)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, m := range up.mints {
		if m == "STRANGER INSTALLATION" {
			t.Fatal("a token was minted on the stranger's installation")
		}
	}
}
