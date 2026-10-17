package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"sync"
)

// Asset is the URL a page links a /static/ file at: the path with
// ?v=<content hash> appended, e.g. "/static/app.css?v=3f9c1a0b2d4e".
//
// 🔴 A CONTENT HASH, NOT THE BUILD VERSION. muster 0.43.0 shipped a stylesheet
// change (the "N new" pill's .top-20) behind an unversioned /static/app.css,
// and Cloudflare and browsers kept serving the old copy (the edge answered
// max-age=14400 with a cf HIT): the pill drew at the bottom of the page. The
// hash changes exactly when the file's bytes do, so a release that leaves a
// file alone leaves every cache of it warm, and one that changes it is a new
// URL no cache has seen. The server marks a URL carrying the CURRENT hash
// immutable (see internal/api staticHandler).
//
// A path that is not in the embedded tree is returned unchanged — the
// static-assets test in internal/api fails on it first.
func Asset(path string) string { return assets.url(Static, path) }

// AssetHash is the hash Asset appends for path ("" if it does not ship).
func AssetHash(path string) string { return assets.hash(Static, path) }

// assetHashLen is how many hex digits of the SHA-256 the URL carries: 48 bits,
// ample to tell one build's file from the last.
const assetHashLen = 12

type assetCache struct {
	mu     sync.Mutex
	hashes map[string]string
}

var assets = &assetCache{}

func (c *assetCache) hash(fsys fs.FS, path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h, ok := c.hashes[path]; ok {
		return h
	}
	h := contentHash(fsys, path)
	if c.hashes == nil {
		c.hashes = map[string]string{}
	}
	c.hashes[path] = h
	return h
}

func (c *assetCache) url(fsys fs.FS, path string) string {
	if h := c.hash(fsys, path); h != "" {
		return path + "?v=" + h
	}
	return path
}

// contentHash is the first assetHashLen hex digits of the SHA-256 of the file
// /static/<rest> names in fsys (whose root holds the static/ directory), or ""
// when path is not under /static/ or the file does not exist.
func contentHash(fsys fs.FS, path string) string {
	rest, ok := strings.CutPrefix(path, "/static/")
	if !ok {
		return ""
	}
	b, err := fs.ReadFile(fsys, "static/"+rest)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:assetHashLen]
}
