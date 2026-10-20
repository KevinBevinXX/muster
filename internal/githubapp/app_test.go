package githubapp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

func agent(id int64, repo string) connectors.Caller {
	return connectors.Caller{AgentID: id, AgentName: fmt.Sprintf("agent-%d", id), Repo: repo}
}

// TestTheAppJWTVerifiesAgainstThePublicKeyWithGitHubsBounds: RS256 under the
// App's key, iss = the App ID, iat backdated exactly 60 s, exp 9 min ahead (so
// exp − iat is GitHub's 10-minute maximum, never more).
func TestTheAppJWTVerifiesAgainstThePublicKeyWithGitHubsBounds(t *testing.T) {
	k := key(t)
	now := time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC)
	tok, err := appJWT(987654, k, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a three-part JWT: %q", tok)
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(hdr) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatalf("header = %s", hdr)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the signature does not verify against the App's public key: %v", err)
	}
	// Negative control: another key's public half must NOT verify it.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if rsa.VerifyPKCS1v15(&other.PublicKey, crypto.SHA256, sum[:], sig) == nil {
		t.Fatal("negative control: the signature verified under an unrelated key")
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	// Pinned literally: the whole claim set, so an added or reshaped claim fails.
	want := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"987654"}`, now.Unix()-60, now.Unix()+540)
	if string(claims) != want {
		t.Fatalf("claims =\n%s\nwant\n%s", claims, want)
	}
}

func pemOf(t *testing.T, typ string, der []byte) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

// TestParsePrivateKeyAcceptsGitHubsFormatsAndRefusesTheRest.
func TestParsePrivateKeyAcceptsGitHubsFormatsAndRefusesTheRest(t *testing.T) {
	k := key(t)
	pkcs1 := pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k))
	der8, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := pemOf(t, "PRIVATE KEY", der8)
	for name, v := range map[string]string{
		"pkcs1 (GitHub's download)": pkcs1,
		"pkcs8":                     pkcs8,
		"flattened \\n":             strings.ReplaceAll(strings.TrimSpace(pkcs1), "\n", `\n`),
	} {
		got, err := ParsePrivateKey(v)
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		if !got.Equal(k) {
			t.Errorf("%s: parsed a different key", name)
		}
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	for name, v := range map[string]string{
		"empty":       "",
		"not pem":     "ghp_this-is-a-token-not-a-key",
		"ec key":      pemOf(t, "PRIVATE KEY", ecDER),
		"public key":  pemOf(t, "PUBLIC KEY", []byte("x")),
		"two blocks":  pkcs1 + pkcs1,
		"corrupt der": pemOf(t, "RSA PRIVATE KEY", []byte("not der")),
	} {
		if _, err := ParsePrivateKey(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestTheAgentMintCarriesExactlyItsOneRepositoryAndFourPermissions pins the
// mint request's body LITERALLY: one repository (the agent's, by name), and
// contents + pull_requests write, checks + statuses read — nothing else. A mutant that adds a
// permission, widens a level, or drops the repositories restriction changes
// these bytes.
func TestTheAgentMintCarriesExactlyItsOneRepositoryAndFourPermissions(t *testing.T) {
	app, f, _ := newTestApp(t)
	tok, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	m := f.mintsSoFar()
	if len(m) != 1 {
		t.Fatalf("%d mints, want 1", len(m))
	}
	const want = `{"repositories":["widget"],"permissions":{"contents":"write","pull_requests":"write","checks":"read","statuses":"read"}}`
	if m[0].body != want {
		t.Fatalf("the mint request body is\n%s\nwant exactly\n%s", m[0].body, want)
	}
	if m[0].installation != 42 {
		t.Fatalf("minted on installation %d, want example-org's (42)", m[0].installation)
	}
	if tok != m[0].token {
		t.Fatalf("Credential returned %q, not the minted token %q", tok, m[0].token)
	}
}

// TestATokenIsReusedUntilFiveMinutesBeforeItsExpiry measures the refresh
// boundary at two points on each side: one second before and one after
// expiry − 5 min (and the middle of the hour).
func TestATokenIsReusedUntilFiveMinutesBeforeItsExpiry(t *testing.T) {
	app, f, c := newTestApp(t)
	start := c.now()
	get := func() string {
		t.Helper()
		tok, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultAPIHost, "repos/example-org/widget/pulls")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	first := get()
	for _, at := range []time.Duration{30 * time.Minute, 55*time.Minute - time.Second} {
		c.set(start.Add(at))
		if got := get(); got != first || len(f.mintsSoFar()) != 1 {
			t.Fatalf("at +%s: a new token was minted (%d mints) though the first is valid for another %s",
				at, len(f.mintsSoFar()), time.Hour-at)
		}
	}
	c.set(start.Add(55*time.Minute + time.Second))
	if got := get(); got == first || len(f.mintsSoFar()) != 2 {
		t.Fatalf("at +55m1s (under 5 min left): the old token was reused (%d mints)", len(f.mintsSoFar()))
	}
	// Per agent: another agent on the SAME repository gets its own token.
	if _, _, err := app.Credential(context.Background(), agent(8, "example-org/widget"), DefaultGitHost, "example-org/widget"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.mintsSoFar()); n != 3 {
		t.Fatalf("a second agent reused the first agent's token (%d mints, want 3)", n)
	}
}

// TestCredentialRefusesEveryAmbiguousTarget: a path whose repository muster
// cannot decide is refused BEFORE a token is minted, with ErrTargetRefused —
// including the shapes that slipped through the first cut (case-varied
// `Repos`, a decoded leading slash, `repositories/<id>`).
func TestCredentialRefusesEveryAmbiguousTarget(t *testing.T) {
	app, f, _ := newTestApp(t)
	me := agent(7, "example-org/widget")
	for _, tc := range []struct{ host, rest string }{
		{DefaultGitHost, "example-org"},
		{DefaultGitHost, ""},
		{DefaultAPIHost, "repos/example-org"},
		{DefaultAPIHost, "Repos/example-org"},
		{DefaultAPIHost, "repos/example-org/widget/../gadget/pulls"},
		{DefaultAPIHost, "graphql/../repos/example-org/gadget"},
		{DefaultAPIHost, "repos/example-org/widget/%2e%2e/gadget"},
		{DefaultAPIHost, "repos/example-org/widget/%252e%252e/gadget"},
		{DefaultAPIHost, "%2Frepos/example-org/gadget"},
		{DefaultAPIHost, "/repos/example-org/gadget"},
		{DefaultAPIHost, "repositories/123456/contents/README.md"},
		{DefaultAPIHost, "REPOSITORIES/123456"},
		{DefaultAPIHost, "repos/example-org/%zz"},
		{DefaultAPIHost, "repos\\example-org\\gadget"},
		{"uploads.github.com", "repos/example-org/widget/releases"},
	} {
		tok, anon, err := app.Credential(context.Background(), me, tc.host, tc.rest)
		if !errors.Is(err, connectors.ErrTargetRefused) || tok != "" || anon {
			t.Errorf("%s/%s: tok=%q anon=%v err=%v, want ErrTargetRefused", tc.host, tc.rest, tok, anon, err)
		}
	}
	if _, _, err := app.Credential(context.Background(), agent(9, ""), DefaultGitHost, "example-org/widget.git"); !errors.Is(err, connectors.ErrTargetRefused) {
		t.Errorf("an agent with no repository: err = %v, want ErrTargetRefused", err)
	}
	if n := len(f.mintsSoFar()); n != 0 {
		t.Fatalf("%d token(s) minted for refused targets", n)
	}
	// Positive control: the agent's own repository, in each accepted shape
	// (case-insensitive, `Repos` too), and a repository-less API path, get the
	// token.
	for _, tc := range []struct{ host, rest string }{
		{DefaultGitHost, "example-org/widget.git/info/refs"},
		{DefaultGitHost, "Example-Org/Widget/git-receive-pack"},
		{DefaultAPIHost, "repos/example-org/widget/pulls"},
		{DefaultAPIHost, "REPOS/Example-Org/widget/check-runs"},
		{DefaultAPIHost, "graphql"},
	} {
		tok, anon, err := app.Credential(context.Background(), me, tc.host, tc.rest)
		if err != nil || anon || tok == "" {
			t.Errorf("positive control %s/%s: tok=%q anon=%v err=%v", tc.host, tc.rest, tok, anon, err)
		}
	}
}

// TestAnotherRepositoryIsAnonymousAndNeverGetsTheToken: a request for any
// repository but the agent's own — in any spelling GitHub routes to it — gets
// NO credential (anonymous), and nothing is minted for it.
func TestAnotherRepositoryIsAnonymousAndNeverGetsTheToken(t *testing.T) {
	app, f, _ := newTestApp(t)
	me := agent(7, "example-org/widget")
	for _, tc := range []struct{ host, rest string }{
		{DefaultGitHost, "example-org/gadget.git/info/refs"},
		{DefaultGitHost, "other-org/widget.git/git-upload-pack"},
		{DefaultAPIHost, "repos/example-org/gadget/pulls"},
		{DefaultAPIHost, "Repos/example-org/gadget/contents/x"},
		{DefaultAPIHost, "REPOS/Example-Org/Gadget"},
		{DefaultAPIHost, "repos/example-org/gad%67et"},
	} {
		tok, anon, err := app.Credential(context.Background(), me, tc.host, tc.rest)
		if err != nil || !anon || tok != "" {
			t.Errorf("%s/%s: tok=%q anon=%v err=%v, want anonymous with no token", tc.host, tc.rest, tok, anon, err)
		}
	}
	if n := len(f.mintsSoFar()); n != 0 {
		t.Fatalf("%d token(s) minted for other repositories", n)
	}
}

// TestARejectedTokenIsDroppedFromTheCache: after the upstream 401s an agent's
// token (Rejected), its next request mints anew; another agent's cache stays.
func TestARejectedTokenIsDroppedFromTheCache(t *testing.T) {
	app, f, _ := newTestApp(t)
	a, b := agent(7, "example-org/widget"), agent(8, "example-org/widget")
	for _, c := range []connectors.Caller{a, b} {
		if _, _, err := app.Credential(context.Background(), c, DefaultGitHost, "example-org/widget.git"); err != nil {
			t.Fatal(err)
		}
	}
	app.Rejected(a)
	for _, c := range []connectors.Caller{a, b} {
		if _, _, err := app.Credential(context.Background(), c, DefaultGitHost, "example-org/widget.git"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.mintsSoFar()); n != 3 {
		t.Fatalf("%d mints, want 3 (two, then one re-mint for the rejected agent only)", n)
	}
}

// TestAMint401IsAnOperatorFacingProblemAndClearsOnSuccess: a 401 (wrong key,
// skewed clock) is worded for the operator, is App-wide (Problem(0) and every
// agent), and goes away once a call succeeds.
func TestAMint401IsAnOperatorFacingProblemAndClearsOnSuccess(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.mu.Lock()
	f.mintStatus = http.StatusUnauthorized
	f.mu.Unlock()
	_, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git")
	if err == nil {
		t.Fatal("a 401 mint succeeded")
	}
	for _, want := range []string{"401", "MUSTER_GITHUB_APP_ID", "MUSTER_GITHUB_APP_PRIVATE_KEY", "clock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the 401 message does not say %q: %v", want, err)
		}
	}
	if app.Problem(0) == "" || app.Problem(7) == "" || app.Problem(99) == "" {
		t.Fatalf("a 401 is not App-wide: Problem(0)=%q Problem(7)=%q Problem(99)=%q", app.Problem(0), app.Problem(7), app.Problem(99))
	}
	f.mu.Lock()
	f.mintStatus = 0
	f.mu.Unlock()
	if _, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git"); err != nil {
		t.Fatal(err)
	}
	if p := app.Problem(7); p != "" {
		t.Fatalf("the problem outlived a successful mint: %q", p)
	}
}

// TestAWrongKeyIsRefusedByTheJWTCheck: the fake verifies JWTs, so an App
// configured with a key that is not the App's gets GitHub's 401 — the
// realistic shape of the "wrong key" failure, not a scripted status.
func TestAWrongKeyIsRefusedByTheJWTCheck(t *testing.T) {
	_, f, c := newTestApp(t)
	wrong, _ := rsa.GenerateKey(rand.Reader, 2048)
	app, err := New(Config{AppID: 12345, PrivateKey: wrong, BaseURL: f.srv.URL, Now: c.now, Accounts: []Account{{Login: "example-org", ID: 4200}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong key: err = %v, want the 401 message", err)
	}
	if app.Problem(0) == "" {
		t.Fatal("a wrong key left no App-wide problem")
	}
}

// TestA422MintNamesTheInstallationsRepositoryAccess: the repository not being
// in the installation is a per-agent problem, not an App-wide one.
func TestA422MintNamesTheInstallationsRepositoryAccess(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.mu.Lock()
	f.mintStatus = http.StatusUnprocessableEntity
	f.mu.Unlock()
	_, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git")
	if err == nil || !strings.Contains(err.Error(), "Repository access") {
		t.Fatalf("err = %v", err)
	}
	if app.Problem(0) != "" || app.Problem(8) != "" || app.Problem(7) == "" {
		t.Fatalf("a 422 must be agent 7's problem only: 0=%q 7=%q 8=%q", app.Problem(0), app.Problem(7), app.Problem(8))
	}
}

// TestAnOwnerWithoutAnInstallationIsRefusedByName.
func TestAnOwnerWithoutAnInstallationIsRefusedByName(t *testing.T) {
	app, f, _ := newTestApp(t)
	app.cfg.Accounts = append(app.cfg.Accounts, Account{Login: "someone-else", ID: 9900}) // allowlisted, but not installed
	_, _, err := app.Credential(context.Background(), agent(7, "someone-else/widget"), DefaultGitHost, "someone-else/widget.git")
	if err == nil || !strings.Contains(err.Error(), "not installed on someone-else") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.mintsSoFar()); n != 0 {
		t.Fatalf("%d mints", n)
	}
}

// TestAnInstallationOffTheAllowlistIsIgnored: the App is public, so a stranger
// can install it on their own account. That installation contributes no
// repository and mints no token, and is logged once, by login.
func TestAnInstallationOffTheAllowlistIsIgnored(t *testing.T) {
	app, f, _ := newTestApp(t) // allowlist: Example-Org (different case on purpose)
	f.install(77, "stranger", "stranger/payload")
	var logBuf strings.Builder
	app.cfg.Logger = log.New(&logBuf, "", 0)
	repos, err := app.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if strings.HasPrefix(r.FullName, "stranger/") {
			t.Fatalf("a non-allowlisted installation contributed %s", r.FullName)
		}
	}
	if len(repos) != 2 {
		t.Fatalf("control: %d repositories, want example-org's 2", len(repos))
	}
	_, _, err = app.Credential(context.Background(), agent(1, "stranger/payload"), DefaultGitHost, "stranger/payload.git")
	if err == nil || !strings.Contains(err.Error(), "MUSTER_GITHUB_APP_ACCOUNTS") {
		t.Fatalf("minting for a non-allowlisted account: err = %v", err)
	}
	for _, m := range f.mintsSoFar() {
		if m.installation == 77 {
			t.Fatalf("a token was minted on the stranger's installation: %s", m.body)
		}
	}
	if _, err := app.Accounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logBuf.String(), `"stranger"`) != 1 || strings.Contains(logBuf.String(), "payload") {
		t.Fatalf("the ignored installation is not logged exactly once by login only:\n%s", logBuf.String())
	}
	if _, err := New(Config{AppID: 1, PrivateKey: key(t)}); err == nil {
		t.Fatal("New accepted an App with no account allowlist")
	}
}

// TestReposListsEveryInstallationRepositoryAcrossPages: 150 repositories on
// one installation (two pages of 100) plus a second installation; the listing
// uses a metadata-read token, never an agent's.
func TestReposListsEveryInstallationRepositoryAcrossPages(t *testing.T) {
	c := &clock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	k := key(t)
	f := newFakeGitHub(t, &k.PublicKey, c.now)
	var many []string
	for i := 0; i < 150; i++ {
		many = append(many, fmt.Sprintf("example-org/repo-%03d", i))
	}
	f.install(42, "example-org", many...)
	f.install(43, "second-org", "second-org/thing")
	app, err := New(Config{AppID: 12345, PrivateKey: k, BaseURL: f.srv.URL, Now: c.now, Accounts: []Account{{Login: "example-org", ID: 4200}, {Login: "second-org", ID: 4300}}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 151 {
		t.Fatalf("%d repositories, want 151 (150 over two pages + 1)", len(repos))
	}
	seen := map[string]bool{}
	for _, r := range repos {
		seen[r.FullName] = true
	}
	for _, n := range []string{"example-org/repo-000", "example-org/repo-099", "example-org/repo-100", "example-org/repo-149", "second-org/thing"} {
		if !seen[n] {
			t.Errorf("%s missing", n)
		}
	}
	for _, m := range f.mintsSoFar() {
		if m.body != `{"permissions":{"metadata":"read"}}` {
			t.Errorf("a listing mint asked for %s, want metadata read only", m.body)
		}
	}
	// Cached for reposTTL: a second read inside it makes no listing call.
	calls := f.listCalls
	if _, err := app.Repos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.listCalls != calls {
		t.Fatalf("a read inside the cache window listed again (%d -> %d calls)", calls, f.listCalls)
	}
	c.set(c.now().Add(reposTTL + time.Second))
	if _, err := app.Repos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.listCalls == calls {
		t.Fatal("a read past the cache window did not list again")
	}
}

// TestDescribeIsTheGitHubPresetsShape: the virtual connector must look, to
// every reader, like the github connector ccd's git routing expects — name,
// hosts, basic with username x-access-token.
func TestDescribeIsTheGitHubPresetsShape(t *testing.T) {
	app, _, _ := newTestApp(t)
	d := app.Describe()
	p := connectors.GitHubPreset
	if d.Name != p.Key || d.Scheme != p.Scheme || d.Username != p.Username {
		t.Fatalf("Describe = %+v, want the github preset's name/scheme/username", d)
	}
	got, _ := json.Marshal(d.Hosts)
	want, _ := json.Marshal(p.Hosts)
	if string(got) != string(want) {
		t.Fatalf("hosts %s, want %s", got, want)
	}
}

// TestTheAllowlistMatchesTheAccountIDNotTheLogin is the re-registered-name
// attack: allowlisted example-org (id 4200) renames, a stranger registers the
// freed login `example-org` (a NEW id) and installs the public App. Matched by
// login it would pass; matched by id it contributes nothing and mints nothing.
// The genuine account, now under its new login, is still used — and the rename
// is reported once.
func TestTheAllowlistMatchesTheAccountIDNotTheLogin(t *testing.T) {
	c := &clock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	k := key(t)
	f := newFakeGitHub(t, &k.PublicKey, c.now)
	f.install(42, "example-org-renamed", "example-org-renamed/widget") // the genuine account: id 4200
	f.install(91, "example-org", "example-org/payload")                // a squatter on the freed login: id 9100
	var logBuf strings.Builder
	app, err := New(Config{AppID: 12345, PrivateKey: k, BaseURL: f.srv.URL, Now: c.now,
		Accounts: []Account{{Login: "example-org", ID: 4200}}, Logger: log.New(&logBuf, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "example-org-renamed/widget" {
		t.Fatalf("repositories = %+v, want only the genuine (renamed) account's", repos)
	}
	if _, _, err := app.Credential(context.Background(), agent(1, "example-org/payload"), DefaultGitHost, "example-org/payload.git"); err == nil {
		t.Fatal("a token was served for the squatter's repository")
	}
	for _, m := range f.mintsSoFar() {
		if m.installation == 91 {
			t.Fatalf("a token was minted on the squatter's installation: %s", m.body)
		}
	}
	if _, _, err := app.Credential(context.Background(), agent(2, "example-org-renamed/widget"), DefaultGitHost,
		"example-org-renamed/widget.git"); err != nil {
		t.Fatalf("the renamed genuine account is not served: %v", err)
	}
	if got := app.Renames(); len(got) != 1 || got[0] != "example-org→example-org-renamed (4200)" {
		t.Fatalf("Renames = %q", got)
	}
	_, _ = app.Repos(context.Background())
	if _, err := app.installations(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logBuf.String(), "is listed as"); n != 1 {
		t.Fatalf("the rename is logged %d times, want once:\n%s", n, logBuf.String())
	}
	if _, err := New(Config{AppID: 1, PrivateKey: k, Accounts: []Account{{Login: "example-org"}}}); err == nil {
		t.Fatal("New accepted an allowlisted account with no id")
	}
}

// TestExpiredTokensAreSweptFromTheCache pins the sweep in store: once a token
// is past its expiry, the next store drops it, so the cache is bounded by the
// agents active within the last hour.
func TestExpiredTokensAreSweptFromTheCache(t *testing.T) {
	app, _, c := newTestApp(t)
	start := c.now()
	for id := int64(1); id <= 3; id++ {
		if _, _, err := app.Credential(context.Background(), agent(id, "example-org/widget"), DefaultGitHost, "example-org/widget.git"); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int { app.mu.Lock(); defer app.mu.Unlock(); return len(app.tokens) }
	if n := count(); n != 3 {
		t.Fatalf("control: %d cached tokens, want 3", n)
	}
	// One second before expiry nothing is swept (a new agent's store keeps all).
	c.set(start.Add(time.Hour - time.Second))
	if _, _, err := app.Credential(context.Background(), agent(4, "example-org/widget"), DefaultGitHost, "example-org/widget.git"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 4 {
		t.Fatalf("before expiry: %d cached tokens, want 4", n)
	}
	// Past the first three's expiry, the next store drops them.
	c.set(start.Add(time.Hour + time.Second))
	if _, _, err := app.Credential(context.Background(), agent(5, "example-org/widget"), DefaultGitHost, "example-org/widget.git"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("after expiry: %d cached tokens, want 2 (agents 4 and 5)", n)
	}
}
