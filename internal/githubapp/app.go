// Package githubapp is muster's GitHub identity when a GitHub App is configured
// (MUSTER_GITHUB_APP_ID + MUSTER_GITHUB_APP_PRIVATE_KEY): the `github`
// connector's credential, the Repos tab's repository list, and the
// "is this a connected repository" check behind every dispatch door.
//
// 🔴 ONLY INSTALLATIONS ON ALLOWLISTED ACCOUNTS EXIST (Config.Accounts,
// MUSTER_GITHUB_APP_ACCOUNTS, required). The App must be public ("Any account")
// to be installable on an organization its owner does not own, so ANYONE can
// install it on their own account. An installation on an account that is not
// on the list contributes no repository, mints no token and passes no
// connected-repository check: it is ignored, and logged once by login.
//
// 🔴 THE LIST IS MATCHED ON THE ACCOUNT'S NUMERIC ID, NEVER ITS LOGIN. A login
// is renamable and, once freed, re-registrable: if an allowlisted `acme`
// renames, anyone can register `acme`, install the public App, and — matched by
// login — pass the allowlist. The id never changes and is never reused. The
// login in each entry is a label and a cross-check only: an installation whose
// id matches but whose login differs (a legitimate rename) is still used, and
// the rename is logged once and reported (App.Renames, the boot check line).
//
// 🔴 AN AGENT'S TOKEN IS MINTED FOR THAT AGENT'S ONE REPOSITORY, WITH EXACTLY
// FOUR PERMISSIONS. Each proxied request from an approved agent to its own
// repository is served with an installation access token minted by POST
// /app/installations/{id}/access_tokens with `repositories: [<the agent's
// repo>]` and `permissions: {contents: write, pull_requests: write, checks:
// read, statuses: read}` (GitHub adds metadata: read implicitly). Nothing wider
// is ever asked for on an agent's behalf: the request body is a fixed struct
// (mintRequest), pinned literally by the tests.
//
// 🔴 THE TOKEN NEVER LEAVES MUSTER. It is injected by the connector proxy
// (internal/connectors, /connect/github/...) on the way out, exactly as a stored
// connector's secret is, and is held only in this process's memory: cached per
// (agent, repository) until refreshBefore of its expiry (GitHub issues them for
// one hour), never written to the database, a log line or a response.
//
// 🔴 A REQUEST FOR ANOTHER REPOSITORY CARRIES NO CREDENTIAL AT ALL. It is
// forwarded anonymously (a public dependency or submodule clones as it would
// for anyone; a private one fails as it would for anyone), and the App token is
// never attached to it. A path whose repository cannot be decided — a `.`/`..`
// or encoded traversal, a leading empty segment, a `repositories/<id>` path —
// is refused outright. See targetRepo.
//
// The App's own credential (a 10-minute RS256 JWT signed with the private key)
// is used only for /app/... calls — listing installations and minting tokens —
// and never leaves muster either.
//
// 🔴 THE APP IS THE ADMIN'S, NOT EVERY USER'S. The OAuth connection it replaces
// was one GitHub account per muster user; the App is the deployment's. So it is
// offered only to admin-role principals: the `github` connector is presented
// to them alone (connectors.WithVirtual's allow), and internal/api's
// github_source.go gives the App's repositories to them alone. A member keeps
// their own OAuth connection and stored connectors, as before.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/github"
)

// ConnectorName is the connector the App backs. It is the name ccd's checkout
// and git routing ask for (cmd/ccd: githubConnector) and the GitHub preset's key.
const ConnectorName = "github"

// Defaults for the hosts the connector serves. Config overrides them only in
// tests and the e2e build (a fake GitHub on loopback).
const (
	DefaultGitHost = "github.com"
	DefaultAPIHost = "api.github.com"
)

// refreshBefore is how long before its expiry a cached token is replaced. A
// token handed to a request must outlive it: a clone of a large repository can
// take minutes, and GitHub checks the token per request, not per connection.
const refreshBefore = 5 * time.Minute

// installationsTTL bounds how stale the installation list (which accounts the
// App is installed on) may be. A miss for an owner refreshes it at once.
const installationsTTL = 10 * time.Minute

// reposTTL bounds how stale the installation repository list may be. It is read
// on every Repos tab render and every dispatch check, and changes only when the
// owner edits the installation's repository access on GitHub.
const reposTTL = time.Minute

// maxRepoPages caps the repository listing (100 per page).
const maxRepoPages = 30

// Config is what New needs.
type Config struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
	// Accounts are the GitHub accounts (users or organizations) whose
	// installations are used, matched by ID. REQUIRED: New refuses an empty
	// list or an entry with no ID, because the App is installable by anyone.
	Accounts []Account
	// BaseURL is the REST API base; "" means github's (github.APIBaseURL()).
	BaseURL string
	// GitHost and APIHost are the hosts the connector serves; "" means
	// DefaultGitHost / DefaultAPIHost.
	GitHost, APIHost string
	HTTP             *http.Client
	Now              func() time.Time
	Logger           *log.Logger
}

// App is a configured GitHub App.
type App struct {
	cfg Config

	mintMu sync.Mutex // serialises mints: one token per key at a time

	mu         sync.Mutex
	installs   []installation
	installsAt time.Time
	tokens     map[tokenKey]token
	repos      []github.Repo
	reposAt    time.Time
	ignored    map[string]bool  // logins of non-allowlisted installations, logged once
	renamed    map[int64]string // allowlisted id -> its login on GitHub, when it differs from the listed one
	problems   map[int64]string // per agent: the last mint's failure
	global     string           // the last App-JWT call's failure (auth, rate limit)
}

type installation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		// Type is "User" or "Organization" for the accounts the allowlist's
		// ids name; an Enterprise installation's account id is from another
		// numbering space and must never match one (allowed).
		Type string `json:"type"`
	} `json:"account"`
}

// Account is one allowlisted GitHub account: its immutable numeric ID (what is
// matched) and the login it was listed under (a label, and the rename check).
type Account struct {
	Login string
	ID    int64
}

// tokenKey names a cached token: an agent's repository token (agent > 0), or an
// installation's listing token (agent == 0, repo == "installation:<id>").
type tokenKey struct {
	agent int64
	repo  string
}

type token struct {
	value   string
	expires time.Time
}

// New builds an App. It makes no network call; Check does.
func New(cfg Config) (*App, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("githubapp: the App ID must be a positive integer")
	}
	if cfg.PrivateKey == nil {
		return nil, errors.New("githubapp: no private key")
	}
	if len(cfg.Accounts) == 0 {
		return nil, errors.New("githubapp: no allowlisted accounts: anyone can install a public App, so the " +
			"accounts whose installations are used must be named")
	}
	for _, acc := range cfg.Accounts {
		if acc.ID <= 0 {
			return nil, fmt.Errorf("githubapp: allowlisted account %q has no numeric ID: a login alone can be "+
				"renamed and re-registered by someone else", acc.Login)
		}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = github.APIBaseURL()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.GitHost == "" {
		cfg.GitHost = DefaultGitHost
	}
	if cfg.APIHost == "" {
		cfg.APIHost = DefaultAPIHost
	}
	cfg.GitHost, cfg.APIHost = strings.ToLower(cfg.GitHost), strings.ToLower(cfg.APIHost)
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &App{cfg: cfg, tokens: map[tokenKey]token{}, problems: map[int64]string{}, ignored: map[string]bool{},
		renamed: map[int64]string{}}, nil
}

// AppID is the configured App ID.
func (a *App) AppID() int64 { return a.cfg.AppID }

// allowed reports whether in's ACCOUNT ID is on the allowlist. A matching id
// under a different login (a rename) is allowed, and recorded once (Renames).
// A matching login under a different id is NOT allowed: that is a different
// account that took a freed name.
func (a *App) allowed(in installation) bool {
	if in.Account.Type != "User" && in.Account.Type != "Organization" {
		return false // an Enterprise (or unknown) account: its id is not a user/org id
	}
	for _, acc := range a.cfg.Accounts {
		if acc.ID != in.Account.ID {
			continue
		}
		if !strings.EqualFold(acc.Login, in.Account.Login) {
			a.mu.Lock()
			first := a.renamed[acc.ID] != in.Account.Login
			a.renamed[acc.ID] = in.Account.Login
			a.mu.Unlock()
			if first && a.cfg.Logger != nil {
				a.cfg.Logger.Printf("github app: allowlisted account %d is listed as %q but is now %q on GitHub "+
					"(renamed); it is still used — update MUSTER_GITHUB_APP_ACCOUNTS to %s:%d", acc.ID, acc.Login,
					in.Account.Login, in.Account.Login, acc.ID)
			}
		}
		return true
	}
	return false
}

// listed reports whether login is the listed label of an allowlisted account
// (for an error message only; never an authorisation).
func (a *App) listed(login string) bool {
	for _, acc := range a.cfg.Accounts {
		if strings.EqualFold(acc.Login, login) {
			return true
		}
	}
	return false
}

// Renames are the allowlisted accounts whose login on GitHub differs from the
// listed one, as "listed→current (id)", sorted.
func (a *App) Renames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, acc := range a.cfg.Accounts {
		if now, ok := a.renamed[acc.ID]; ok {
			out = append(out, fmt.Sprintf("%s→%s (%d)", acc.Login, now, acc.ID))
		}
	}
	sort.Strings(out)
	return out
}

// --- the connector ---------------------------------------------------------------

// Describe is the `github` connector's public description: the same hosts and
// auth shape as the GitHub preset (basic, username x-access-token — the one
// shape both git over HTTPS and the REST API accept), with no stored secret.
func (a *App) Describe() connectors.Connector {
	return connectors.Connector{
		Name:      ConnectorName,
		Platform:  "github-app",
		Hosts:     []string{a.cfg.APIHost, a.cfg.GitHost},
		Scheme:    connectors.SchemeBasic,
		Username:  "x-access-token",
		CreatedAt: connectors.VirtualCreatedAt,
		UpdatedAt: connectors.VirtualCreatedAt,
	}
}

// Credential is the installation token for caller's own repository; a request
// for ANOTHER repository is anonymous (no credential at all); a request whose
// repository cannot be decided is refused (targetRepo).
func (a *App) Credential(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error) {
	repo := strings.TrimSpace(caller.Repo)
	if repo == "" {
		return "", false, fmt.Errorf("%w: the %s connector is backed by a GitHub App, which mints a token for the "+
			"calling agent's own repository, and agent %s was dispatched without one (an agent with no "+
			"repository, an agent, has no github access while the App is configured)", connectors.ErrTargetRefused,
			ConnectorName, caller.AgentName)
	}
	target, scoped, err := a.targetRepo(host, rest)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", connectors.ErrTargetRefused, err)
	}
	if scoped && !strings.EqualFold(target, repo) {
		return "", true, nil // another repository: anonymous, never the App token
	}
	tok, err := a.repoToken(ctx, caller.AgentID, repo)
	a.mu.Lock()
	if err != nil {
		a.problems[caller.AgentID] = err.Error()
	} else {
		delete(a.problems, caller.AgentID)
	}
	a.mu.Unlock()
	return tok, false, err
}

// Rejected drops every cached token of caller's: GitHub answered 401 to one
// (revoked, or the installation's access changed), so the next request mints.
func (a *App) Rejected(caller connectors.Caller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.tokens {
		if k.agent == caller.AgentID {
			delete(a.tokens, k)
		}
	}
}

// Problem is the operator-facing reason the App cannot currently serve agentID:
// an App-wide failure (its credentials refused, rate limited) first, else that
// agent's last mint failure. "" when nothing is known to be wrong.
func (a *App) Problem(agentID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.global != "" {
		return a.global
	}
	if agentID > 0 {
		return a.problems[agentID]
	}
	return ""
}

// targetRepo is the repository a proxied request is for. scoped is false for an
// API path that names no repository (/graphql, /rate_limit, /user …): those
// carry the agent's token, which can reach only the agent's repository anyway.
//
// The path is FULLY percent-decoded first and compared without case, as GitHub
// routes it:
//
//   - github.com: every path is <owner>/<repo>[.git]/… (git's smart-HTTP
//     protocol, and the web URLs). A path with fewer segments is refused.
//   - api.github.com: /repos/<owner>/<repo>/… (any case) names a repository;
//     /repositories/<id>/… is REFUSED (an id muster cannot match to the
//     agent's repository without another API call); any other path is not
//     repository-scoped.
//
// Refused outright, because the upstream may resolve them to another
// repository after this check: a `.` or `..` segment, a leading empty segment
// (`%2Frepos/…` decodes to one), a `%` left after decoding (double encoding), a
// backslash, and an escape that does not decode.
func (a *App) targetRepo(host, rest string) (repo string, scoped bool, err error) {
	p, err := url.PathUnescape(rest)
	if err != nil {
		return "", false, errors.New("the request path does not decode")
	}
	if strings.ContainsAny(p, "%\\") {
		return "", false, errors.New("the request path is encoded more than once or holds a backslash")
	}
	segs := strings.Split(p, "/")
	if segs[0] == "" {
		return "", false, errors.New("the request path begins with an empty segment")
	}
	for _, s := range segs {
		if s == "." || s == ".." {
			return "", false, errors.New("the request path has a '.' or '..' segment")
		}
	}
	switch host {
	case a.cfg.GitHost:
		if len(segs) < 2 || segs[1] == "" {
			return "", false, fmt.Errorf("%s/%s names no repository", host, p)
		}
		return segs[0] + "/" + strings.TrimSuffix(segs[1], ".git"), true, nil
	case a.cfg.APIHost:
		switch strings.ToLower(segs[0]) {
		case "repos":
			if len(segs) < 3 || segs[1] == "" || segs[2] == "" {
				return "", false, fmt.Errorf("%s/%s names no repository", host, p)
			}
			return segs[1] + "/" + segs[2], true, nil
		case "repositories":
			return "", false, fmt.Errorf("%s/%s names a repository by id; use /repos/<owner>/<name>", host, p)
		}
		return "", false, nil
	}
	return "", false, fmt.Errorf("host %s is not served by the GitHub App connector", host)
}

// mintRequest is the body of every agent token mint. 🔴 IT IS THE WHOLE
// PERMISSION GRANT: a field added here widens what every agent's token can do.
// The tests pin its JSON literally.
type mintRequest struct {
	Repositories []string        `json:"repositories"`
	Permissions  mintPermissions `json:"permissions"`
}

type mintPermissions struct {
	Contents     string `json:"contents"`
	PullRequests string `json:"pull_requests"`
	Checks       string `json:"checks"`
	Statuses     string `json:"statuses"`
}

func agentMintRequest(repoName string) mintRequest {
	return mintRequest{
		Repositories: []string{repoName},
		Permissions:  mintPermissions{Contents: "write", PullRequests: "write", Checks: "read", Statuses: "read"},
	}
}

// listingMintRequest is the body of the installation-wide token muster itself
// uses to LIST repositories and read a default branch. Metadata read only: it
// can see which repositories exist, and nothing in them. It is never injected
// into an agent's request.
var listingMintRequest = map[string]any{"permissions": map[string]string{"metadata": "read"}}

// repoToken returns a cached or freshly minted token for agentID's repo.
func (a *App) repoToken(ctx context.Context, agentID int64, repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: %q is not owner/name", connectors.ErrTargetRefused, repo)
	}
	key := tokenKey{agent: agentID, repo: strings.ToLower(repo)}
	if t, ok := a.cached(key); ok {
		return t, nil
	}
	a.mintMu.Lock()
	defer a.mintMu.Unlock()
	if t, ok := a.cached(key); ok {
		return t, nil
	}
	inst, err := a.installationFor(ctx, owner)
	if err != nil {
		return "", err
	}
	t, err := a.mint(ctx, inst, agentMintRequest(name), "minting a token for "+repo)
	if err != nil {
		return "", err
	}
	a.store(key, t)
	return t.value, nil
}

// listingToken is the installation's metadata-read token (see listingMintRequest).
func (a *App) listingToken(ctx context.Context, inst installation) (string, error) {
	key := tokenKey{repo: "installation:" + strconv.FormatInt(inst.ID, 10)}
	if t, ok := a.cached(key); ok {
		return t, nil
	}
	a.mintMu.Lock()
	defer a.mintMu.Unlock()
	if t, ok := a.cached(key); ok {
		return t, nil
	}
	t, err := a.mint(ctx, inst, listingMintRequest, "minting a listing token for "+inst.Account.Login)
	if err != nil {
		return "", err
	}
	a.store(key, t)
	return t.value, nil
}

func (a *App) cached(k tokenKey) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[k]
	if !ok || !a.cfg.Now().Before(t.expires.Add(-refreshBefore)) {
		return "", false
	}
	return t.value, true
}

// store caches t, and drops every cached token past its expiry, so the map is
// bounded by the agents active in the last hour (a deleted agent's tokens go
// with the rest).
func (a *App) store(k tokenKey, t token) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	for kk, tt := range a.tokens {
		if !now.Before(tt.expires) {
			delete(a.tokens, kk)
		}
	}
	a.tokens[k] = t
}

// mint calls POST /app/installations/{id}/access_tokens with body.
func (a *App) mint(ctx context.Context, inst installation, body any, op string) (token, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return token{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	u := a.cfg.BaseURL + "/app/installations/" + strconv.FormatInt(inst.ID, 10) + "/access_tokens"
	if err := a.appCall(ctx, http.MethodPost, u, raw, &out, op, inst.Account.Login); err != nil {
		return token{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return token{}, fmt.Errorf("GitHub's answer while %s carried no token or no expiry", op)
	}
	return token{value: out.Token, expires: out.ExpiresAt}, nil
}

// installationFor is the installation on owner's account.
func (a *App) installationFor(ctx context.Context, owner string) (installation, error) {
	for attempt := 0; attempt < 2; attempt++ {
		list, err := a.installations(ctx, attempt > 0)
		if err != nil {
			return installation{}, err
		}
		for _, in := range list {
			if strings.EqualFold(in.Account.Login, owner) {
				return in, nil
			}
		}
	}
	if !a.listed(owner) {
		return installation{}, fmt.Errorf("%s is not on MUSTER_GITHUB_APP_ACCOUNTS (or is not the account "+
			"it lists by id), so the GitHub App does not serve its repositories", owner)
	}
	return installation{}, fmt.Errorf("the GitHub App (ID %d) is not installed on %s: install it there "+
		"(github.com/apps/<app>/installations/new) and give it access to the repository", a.cfg.AppID, owner)
}

// installations lists the App's installations (or the pinned one), cached.
func (a *App) installations(ctx context.Context, force bool) ([]installation, error) {
	a.mu.Lock()
	if !force && a.installs != nil && a.cfg.Now().Sub(a.installsAt) < installationsTTL {
		list := a.installs
		a.mu.Unlock()
		return list, nil
	}
	a.mu.Unlock()
	list := []installation{}
	for page := 1; page <= 10; page++ {
		var batch []installation
		u := a.cfg.BaseURL + "/app/installations?per_page=100&page=" + strconv.Itoa(page)
		if err := a.appCall(ctx, http.MethodGet, u, nil, &batch, "listing the App's installations", ""); err != nil {
			return nil, err
		}
		// 🔴 THE ALLOWLIST IS APPLIED HERE, AT THE ONE SOURCE every reader
		// (repository listing, minting, default branch) takes installations from.
		for _, in := range batch {
			if a.allowed(in) {
				list = append(list, in)
				continue
			}
			a.mu.Lock()
			first := !a.ignored[strings.ToLower(in.Account.Login)]
			a.ignored[strings.ToLower(in.Account.Login)] = true
			a.mu.Unlock()
			if first && a.cfg.Logger != nil {
				a.cfg.Logger.Printf("github app: ignoring the installation on %q: not on MUSTER_GITHUB_APP_ACCOUNTS",
					in.Account.Login)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	a.mu.Lock()
	a.installs, a.installsAt = list, a.cfg.Now()
	a.mu.Unlock()
	return list, nil
}

// Check lists the (allowlisted) installations, recording the outcome as the
// App-wide problem.
// cmd/muster-server runs it once at boot, in the background.
func (a *App) Check(ctx context.Context) ([]string, error) {
	list, err := a.installations(ctx, true)
	if err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(list))
	for _, in := range list {
		logins = append(logins, in.Account.Login)
	}
	return logins, nil
}

// appCall makes a request authenticated as the App (its JWT) and decodes a 2xx
// answer into out. Its outcome sets or clears the App-wide problem when it is
// one (auth, rate limit), since those affect every agent.
func (a *App) appCall(ctx context.Context, method, u string, body []byte, out any, op, account string) error {
	jwt, err := appJWT(a.cfg.AppID, a.cfg.PrivateKey, a.cfg.Now())
	if err != nil {
		return err
	}
	err = a.do(ctx, method, u, "Bearer "+jwt, body, out, op, account)
	a.mu.Lock()
	var ae *apiError
	switch {
	case err == nil:
		a.global = ""
	case errors.As(err, &ae) && ae.appWide():
		a.global = err.Error()
	}
	a.mu.Unlock()
	return err
}

func (a *App) do(ctx context.Context, method, u, auth string, body []byte, out any, op, account string) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach GitHub while %s: %v", op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return newAPIError(resp, raw, op, account, a.cfg.AppID, a.cfg.Now())
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("GitHub's answer while %s is not the expected JSON: %v", op, err)
	}
	return nil
}

// --- repositories ---------------------------------------------------------------

// Repos lists every repository the App's installation(s) can access — the Repos
// tab, the dispatch pickers and every connected-repository check.
func (a *App) Repos(ctx context.Context) ([]github.Repo, error) {
	a.mu.Lock()
	if a.repos != nil && a.cfg.Now().Sub(a.reposAt) < reposTTL {
		list := a.repos
		a.mu.Unlock()
		return list, nil
	}
	a.mu.Unlock()
	insts, err := a.installations(ctx, false)
	if err != nil {
		return nil, err
	}
	all := []github.Repo{}
	for _, in := range insts {
		tok, err := a.listingToken(ctx, in)
		if err != nil {
			return nil, err
		}
		got := 0
		for page := 1; page <= maxRepoPages; page++ {
			var batch struct {
				TotalCount   int           `json:"total_count"`
				Repositories []github.Repo `json:"repositories"`
			}
			u := a.cfg.BaseURL + "/installation/repositories?per_page=100&page=" + strconv.Itoa(page)
			if err := a.do(ctx, http.MethodGet, u, "token "+tok, nil, &batch,
				"listing the repositories of the installation on "+in.Account.Login, in.Account.Login); err != nil {
				return nil, err
			}
			all = append(all, batch.Repositories...)
			got += len(batch.Repositories)
			if len(batch.Repositories) < 100 || got >= batch.TotalCount {
				break
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return strings.ToLower(all[i].FullName) < strings.ToLower(all[j].FullName) })
	a.mu.Lock()
	a.repos, a.reposAt = all, a.cfg.Now()
	a.mu.Unlock()
	return all, nil
}

// Accounts are the logins the App is installed on, for the Repos tab's label.
func (a *App) Accounts(ctx context.Context) ([]string, error) {
	insts, err := a.installations(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.Account.Login)
	}
	return out, nil
}

// DefaultBranch reads repo's default branch with the installation's listing token.
func (a *App) DefaultBranch(ctx context.Context, repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%q is not owner/name", repo)
	}
	inst, err := a.installationFor(ctx, owner)
	if err != nil {
		return "", err
	}
	tok, err := a.listingToken(ctx, inst)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := a.do(ctx, http.MethodGet, a.cfg.BaseURL+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), "token "+tok, nil, &r,
		"reading "+repo, owner); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// ListPullRequests lists agentID's own repository's pull requests with that
// agent's repository token (server-side; the worker tool returns only the list).
func (a *App) ListPullRequests(ctx context.Context, agentID int64, repo, state string) ([]github.PullRequest, error) {
	tok, err := a.repoToken(ctx, agentID, repo)
	if err != nil {
		return nil, err
	}
	return github.ListPullRequestsAt(ctx, a.cfg.BaseURL, tok, repo, state)
}
