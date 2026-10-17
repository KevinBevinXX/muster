package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// 🔴 EVERY /static/ URL A PAGE LINKS CARRIES ITS FILE'S CONTENT HASH.
//
// muster 0.43.0 shipped its one stylesheet change (.top-20, which places the
// "N new" pill) behind an unversioned /static/app.css. Cloudflare answered
// max-age=14400 with a cf HIT and browsers kept their copy, so the pill drew at
// the bottom of the page for hours. A link that names the bytes it wants
// (?v=<sha256 prefix>) is a new URL whenever the bytes change.
//
// The expected hash is computed HERE, from the embedded file, independently of
// web.Asset — a test that asked web.Asset what to expect would pass whatever it
// appended.
// ---------------------------------------------------------------------------

// wantHash is the first 12 hex digits of the SHA-256 of web/static/<rest>.
func wantHash(t *testing.T, path string) string {
	t.Helper()
	b, err := web.Static.ReadFile("static/" + strings.TrimPrefix(path, "/static/"))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// staticLink matches every href/src that points under /static/ in markup.
var staticLink = regexp.MustCompile(`(?:href|src)="(/static/[^"?]+)(?:\?v=([^"]*))?"`)

// checkVersioned asserts every /static/ link in body carries its file's hash,
// and returns how many it checked.
func checkVersioned(t *testing.T, page, body string) int {
	t.Helper()
	ms := staticLink.FindAllStringSubmatch(body, -1)
	for _, m := range ms {
		if want := wantHash(t, m[1]); m[2] != want {
			t.Errorf("%s links %s with ?v=%q, want ?v=%s (its content hash)", page, m[1], m[2], want)
		}
	}
	return len(ms)
}

func TestEveryStaticLinkCarriesItsContentHash(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, &memPins{})
	shell := getOwed(t, s, h, "/agents").Body.String()
	// The stylesheet itself, spelled out: the one 0.43.0 shipped stale.
	css := `<link rel="stylesheet" href="/static/app.css?v=` + wantHash(t, "/static/app.css") + `">`
	if !strings.Contains(shell, css) {
		t.Errorf("the shell does not link %s", css)
	}
	if n := checkVersioned(t, "GET /agents", shell); n < 4 {
		t.Errorf("control: only %d /static/ links on the shell (stylesheet, icons, htmx, idiomorph, sse expected)", n)
	}

	// The standalone auth documents (login.go) link the stylesheet too.
	ls := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	rec := httptest.NewRecorder()
	ls.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	login := rec.Body.String()
	if n := checkVersioned(t, "GET /login", login); n < 1 {
		t.Errorf("control: GET /login linked no /static/ file:\n%s", login)
	}

	// The service worker precaches the stylesheet under the URL pages link.
	sw, err := ServiceWorkerSource()
	if err != nil {
		t.Fatal(err)
	}
	if want := `const APP_CSS = "/static/app.css?v=` + wantHash(t, "/static/app.css") + `";`; !strings.Contains(string(sw), want) {
		t.Errorf("sw.js does not precache the versioned stylesheet: want %s", want)
	}
}

// Only the URL that names the CURRENT bytes is cached for long; the bare path
// and a stale ?v= are revalidated.
func TestOnlyACurrentlyVersionedStaticURLIsImmutable(t *testing.T) {
	h := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0)).Handler()
	cur := wantHash(t, "/static/app.css")
	for _, tc := range []struct{ url, want string }{
		{"/static/app.css?v=" + cur, "public, max-age=31536000, immutable"},
		{"/static/app.css", "no-cache"},
		{"/static/app.css?v=000000000000", "no-cache"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", tc.url, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("GET %s: Cache-Control %q, want %q", tc.url, got, tc.want)
		}
	}
}

// 🔴 THE WRAPPING IS PINNED AT THE SOURCE, TOO: every /static/ file a view in
// internal/ui names is passed through web.Asset. A new view that spells a bare
// Href("/static/…") would render an unversioned link this test sees before any
// cache does — the rendered-page check above only covers the pages it renders.
func TestEveryStaticPathInTheViewsGoesThroughWebAsset(t *testing.T) {
	dir := filepath.Join("..", "ui")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile(`"/static/[A-Za-z0-9._/-]+\.[A-Za-z0-9]+"`)
	wrapped := regexp.MustCompile(`web\.Asset\("/static/[A-Za-z0-9._/-]+\.[A-Za-z0-9]+"\)`)
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		all, ok := len(lit.FindAllString(src, -1)), len(wrapped.FindAllString(src, -1))
		seen += all
		if all != ok {
			t.Errorf("internal/ui/%s names %d /static/ files and passes %d through web.Asset", name, all, ok)
		}
	}
	if seen < 10 {
		t.Fatalf("control: found only %d /static/ literals in internal/ui — the pattern no longer reads the views", seen)
	}
}
