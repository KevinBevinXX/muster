package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A FAKE GitHub REST API, synthetic throughout: placeholder accounts
// (example-org), placeholder repositories, and tokens minted by a counter. It
// verifies every App call's JWT against the test key's public half, so a wrong
// signature or a claim out of bounds is a 401 here exactly as on GitHub.

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// key is one RSA key per test binary (generating one per test is slow).
func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

type mint struct {
	installation int64
	body         string
	token        string
}

type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	pub *rsa.PublicKey
	now func() time.Time

	mu            sync.Mutex
	installations []installation
	repos         map[int64][]string // installation id -> full names
	mints         []mint
	mintStatus    int // non-zero: answer every mint with this status
	listCalls     int
	tokenTTL      time.Duration
	// rawInstallations, when set, is GET /app/installations' body VERBATIM —
	// GitHub's own JSON, not the production struct re-encoded, so a wrong json
	// tag on that struct cannot agree with itself and pass.
	rawInstallations string
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey, now func() time.Time) *fakeGitHub {
	f := &fakeGitHub{t: t, pub: pub, now: now, repos: map[int64][]string{}, tokenTTL: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) install(id int64, login string, repos ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := installation{ID: id}
	in.Account.Login = login
	in.Account.ID = id * 100 // the account's id: distinct from the installation's
	in.Account.Type = "Organization"
	f.installations = append(f.installations, in)
	f.repos[id] = repos
}

func (f *fakeGitHub) mintsSoFar() []mint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mint(nil), f.mints...)
}

// verifyJWT checks an App JWT the way GitHub does: RS256 under the App's key,
// iat not in the future, exp after now and at most 10 minutes after iat.
func (f *fakeGitHub) verifyJWT(auth string) error {
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return fmt.Errorf("not a bearer JWT")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Iat, Exp int64 }
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	now := f.now().Unix()
	if c.Iat > now || c.Exp <= now || c.Exp-c.Iat > 600 {
		return fmt.Errorf("claims out of bounds: iat=%d exp=%d now=%d", c.Iat, c.Exp, now)
	}
	return nil
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(p, "/app/") {
		if err := f.verifyJWT(r.Header.Get("Authorization")); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"message":"A JSON web token could not be decoded"}`)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && p == "/app/installations":
		if f.rawInstallations != "" {
			_, _ = io.WriteString(w, f.rawInstallations)
			return
		}
		_ = json.NewEncoder(w).Encode(f.installations)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/app/installations/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/app/installations/"), 10, 64)
		for _, in := range f.installations {
			if in.ID == id {
				_ = json.NewEncoder(w).Encode(in)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/access_tokens"):
		if f.mintStatus != 0 {
			w.WriteHeader(f.mintStatus)
			_, _ = io.WriteString(w, `{"message":"scripted failure"}`)
			return
		}
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/app/installations/"), "/access_tokens"), 10, 64)
		body, _ := io.ReadAll(r.Body)
		tok := fmt.Sprintf("fake-installation-token-%04d", len(f.mints)+1)
		f.mints = append(f.mints, mint{installation: id, body: string(body), token: tok})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": f.now().Add(f.tokenTTL).UTC().Format(time.RFC3339)})
	case r.Method == http.MethodGet && p == "/installation/repositories":
		f.listCalls++
		var inst int64 = -1
		for _, m := range f.mints {
			if "token "+m.token == r.Header.Get("Authorization") {
				inst = m.installation
			}
		}
		if inst < 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		all := f.repos[inst]
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		lo, hi := (page-1)*per, page*per
		if lo > len(all) {
			lo = len(all)
		}
		if hi > len(all) {
			hi = len(all)
		}
		repos := []map[string]any{}
		for _, n := range all[lo:hi] {
			repos = append(repos, map[string]any{"full_name": n, "private": true, "html_url": "https://github.com/" + n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(all), "repositories": repos})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// newTestApp is an App against a fresh fake with one installation on
// example-org holding example-org/widget and example-org/gadget.
func newTestApp(t *testing.T) (*App, *fakeGitHub, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	k := key(t)
	f := newFakeGitHub(t, &k.PublicKey, c.now)
	f.install(42, "example-org", "example-org/widget", "example-org/gadget")
	app, err := New(Config{AppID: 12345, PrivateKey: k, BaseURL: f.srv.URL, Now: c.now, Accounts: []Account{{Login: "Example-Org", ID: 4200}}})
	if err != nil {
		t.Fatal(err)
	}
	return app, f, c
}
