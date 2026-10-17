package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// httpClient bounds GitHub API/OAuth calls.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// apiBaseURL is the GitHub REST API base. A package var so tests can point it at
// an httptest server.
var apiBaseURL = "https://api.github.com"

// UseAPIBaseURL points every GitHub call at another API base. Only the e2eterm
// browser-test build calls it (its fake GitHub); a deployable binary never does.
func UseAPIBaseURL(u string) { apiBaseURL = u }

// Repo is a repository as listed for the Repos tab and the dispatch modal.
type Repo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
}

// AuthorizeURL builds the GitHub OAuth authorize URL for the web flow.
func AuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "repo")
	q.Set("state", state)
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

// ExchangeCode swaps an OAuth code for an access token, returning the token and
// the granted scopes.
func ExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (token, scopes string, err error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken      string `json:"access_token"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}
	if body.Error != "" {
		return "", "", fmt.Errorf("github oauth: %s: %s", body.Error, body.ErrorDescription)
	}
	if body.AccessToken == "" {
		return "", "", fmt.Errorf("github oauth: empty access token")
	}
	return body.AccessToken, body.Scope, nil
}

// FetchLogin returns the authenticated user's login for the given token.
func FetchLogin(ctx context.Context, token string) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := apiGet(ctx, token, apiBaseURL+"/user", &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

// ListRepos returns the authenticated user's repositories (up to a few pages,
// newest-updated first).
func ListRepos(ctx context.Context, token string) ([]Repo, error) {
	var all []Repo
	for page := 1; page <= 5; page++ {
		var batch []Repo
		u := apiBaseURL + "/user/repos?per_page=100&sort=updated&page=" + strconv.Itoa(page)
		if err := apiGet(ctx, token, u, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// DefaultBranch returns a repository's default branch (e.g. "main", "trunk").
// repo is "owner/name". Used to clone the right branch when the dispatcher
// didn't specify one (GitHub's per-repo default is not always "main").
func DefaultBranch(ctx context.Context, token, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := apiGet(ctx, token, apiBaseURL+"/repos/"+repo, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// PullRequest is a repository pull request, flattened for the worker tool. The
// GitHub API nests author/branch under user/head/base; ListPullRequests maps
// those into the flat fields below.
type PullRequest struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	User      string `json:"user"`
	HeadRef   string `json:"head_ref"`
	BaseRef   string `json:"base_ref"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"`
}

// ListPullRequests returns a repo's pull requests (newest-updated first, up to
// 50). repo is "owner/name". state is one of "open", "closed", "all"; an empty
// or unrecognized value defaults to "open". The token stays server-side — it is
// never returned to the caller.
func ListPullRequests(ctx context.Context, token, repo, state string) ([]PullRequest, error) {
	switch state {
	case "open", "closed", "all":
	default:
		state = "open"
	}
	// raw mirrors the GitHub PR JSON shape we care about, before flattening.
	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		Draft     bool   `json:"draft"`
		UpdatedAt string `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", "50")
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	u := apiBaseURL + "/repos/" + repo + "/pulls?" + q.Encode()
	if err := apiGet(ctx, token, u, &raw); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(raw))
	for _, p := range raw {
		out = append(out, PullRequest{
			Number:    p.Number,
			Title:     p.Title,
			State:     p.State,
			HTMLURL:   p.HTMLURL,
			User:      p.User.Login,
			HeadRef:   p.Head.Ref,
			BaseRef:   p.Base.Ref,
			Draft:     p.Draft,
			UpdatedAt: p.UpdatedAt,
		})
	}
	return out, nil
}

func apiGet(ctx context.Context, token, urlStr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api %s: status %d", urlStr, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
