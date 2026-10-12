package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestTheGitHubOAuthStateKeyIsWired (accounts PR 3b, round-2 audit 🟢3):
// buildApp derives the OAuth state's MAC key from MUSTER_GITHUB_ENCRYPTION_KEY
// and wires it beside the GitHub store. A server with the key and an OAuth App
// starts the flow (302 to GitHub, a `uid.nonce.mac` state cookie); with the
// StateKey line deleted from main.go it answers 503 — the api package refuses
// to mint a state under no key — so this is the test that sees that line.
func TestTheGitHubOAuthStateKeyIsWired(t *testing.T) {
	cfg := dbConfig(t)
	cfg.UIPassword = "state-wiring-test-password"
	cfg.GitHubEncKey = []byte("0123456789abcdef0123456789abcdef")
	cfg.GitHubClientID, cfg.GitHubClientSecret = "state-wiring-client", "state-wiring-secret"
	base := startServer(t, cfg)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login, err := client.PostForm(base+"/login", url.Values{"password": {cfg.UIPassword}})
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	var session *http.Cookie
	for _, c := range login.Cookies() {
		if c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("login set no session cookie (%d)", login.StatusCode)
	}
	req, _ := http.NewRequest("GET", base+"/github/connect", nil)
	req.AddCookie(session)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var state string
	for _, c := range resp.Cookies() {
		if c.Name == "muster_gh_state" {
			state = c.Value
		}
	}
	if resp.StatusCode != http.StatusFound || strings.Count(state, ".") != 2 || !strings.HasPrefix(state, "1.") {
		t.Fatalf("GET /github/connect with the key set: %d, state %q; want a 302 and a `1.<nonce>.<mac>` state", resp.StatusCode, state)
	}
}
