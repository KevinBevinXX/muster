package web

import (
	"testing"
	"testing/fstest"
)

// The hash is the file's CONTENT: pinned literals (sha256 of each body, first
// 12 hex digits), so a change of the file is a change of the URL, and a file
// left alone keeps its URL.
func TestAnAssetURLCarriesItsContentHash(t *testing.T) {
	red := fstest.MapFS{"static/app.css": {Data: []byte("body{color:red}")}}
	blue := fstest.MapFS{"static/app.css": {Data: []byte("body{color:blue}")}}
	for _, tc := range []struct {
		name string
		c    *assetCache
		fsys fstest.MapFS
		path string
		want string
	}{
		{"a file", &assetCache{}, red, "/static/app.css", "/static/app.css?v=15c42ab7768d"},
		{"the same path, its bytes changed", &assetCache{}, blue, "/static/app.css", "/static/app.css?v=fab4811e9952"},
		{"a file that does not ship is left as it is", &assetCache{}, red, "/static/gone.css", "/static/gone.css"},
		{"a path outside /static/ is left as it is", &assetCache{}, red, "/app.css", "/app.css"},
	} {
		if got := tc.c.url(tc.fsys, tc.path); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The real tree: every file under static/ that a page links gets a hash.
func TestTheEmbeddedStylesheetHasAHash(t *testing.T) {
	if h := AssetHash("/static/app.css"); len(h) != assetHashLen {
		t.Fatalf("AssetHash(/static/app.css) = %q, want %d hex digits", h, assetHashLen)
	}
}
