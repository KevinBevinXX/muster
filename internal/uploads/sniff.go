package uploads

import (
	"net/http"
	"strconv"
	"strings"
)

// SniffBytes is how much of a file Classify reads: http.DetectContentType's
// own window.
const SniffBytes = 512

// Sniff is the content type of a file whose first bytes are head, read from
// the bytes alone (http.DetectContentType). The client's Content-Type and the
// filename's extension are never consulted.
func Sniff(head []byte) string {
	if len(head) > SniffBytes {
		head = head[:SniffBytes]
	}
	return http.DetectContentType(head)
}

// inlineTypes are the ONLY types served inline: four raster image formats,
// each one a browser renders as an image without running anything from the
// file. SVG is NOT one of them (it is a document that runs script), and
// neither is PDF: a PDF served inline from muster's origin is rendered by
// whatever viewer the client has — on the Android app's WebView, or a browser
// with a plugin viewer, that is not guaranteed to be isolated from muster's
// origin — so a PDF is a download like any other non-image.
var inlineTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// labelledTypes are served with their own type, as an ATTACHMENT. Every other
// sniffed type — text/html, text/xml (SVG sniffs as XML or as text), any
// script, anything unrecognised — is served as application/octet-stream.
var labelledTypes = map[string]bool{
	"application/pdf":              true,
	"text/plain; charset=utf-8":    true,
	"text/plain; charset=utf-16be": true,
	"text/plain; charset=utf-16le": true,
	"application/zip":              true,
	"application/x-gzip":           true,
	"application/x-rar-compressed": true,
	"application/ogg":              true,
	"audio/mpeg":                   true,
	"audio/wave":                   true,
	"audio/aiff":                   true,
	"audio/basic":                  true,
	"audio/midi":                   true,
	"video/mp4":                    true,
	"video/webm":                   true,
	"video/avi":                    true,
	"image/bmp":                    true,
	"image/x-icon":                 true,
	"font/woff":                    true,
	"font/woff2":                   true,
	"font/ttf":                     true,
	"font/otf":                     true,
}

// InlineImage reports whether contentType is an image a page may show in an
// <img> (a thumbnail): the raster images of inlineTypes.
func InlineImage(contentType string) bool {
	return inlineTypes[contentType] && strings.HasPrefix(contentType, "image/")
}

// sandboxCSP is the Content-Security-Policy on EVERY served file. `sandbox`
// (with no allow-* token) gives the response an opaque origin and runs no
// script even if a browser did render it as a document; `default-src 'none'`
// loads nothing. img-src 'self' and inline style are what a browser's own
// image-viewer document needs to show an inline image.
const sandboxCSP = "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox"

// SetServeHeaders sets every header that makes a served file inert, for a file
// whose SNIFFED type (Sniff, never a client's claim) is sniffedType. It is the
// ONE place these headers are decided: every route that serves bytes someone
// uploaded — the signed link, the owner's view, a task's attachment — calls
// it, so they cannot drift. size < 0 sets no Content-Length.
//
// It overwrites (Set, never Add) whatever an earlier middleware put there, the
// app's own page CSP included: that one allows inline script.
func SetServeHeaders(h http.Header, sniffedType, filename string, size int64) {
	sv := ServeAs(sniffedType)
	h.Set("Content-Type", sv.ContentType)
	h.Set("Content-Disposition", Disposition(sv, filename))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", sandboxCSP)
	// Revocable and short-lived: no cache — Cloudflare's included — may keep it.
	h.Set("Cache-Control", "private, no-store")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
}

// Serving is how a stored file goes out over HTTP.
type Serving struct {
	// ContentType is the response's Content-Type.
	ContentType string
	// Inline is true only for inlineTypes; everything else is an attachment.
	Inline bool
}

// ServeAs is how a file whose sniffed type is contentType is served.
//
// 🔴 HTML, SVG AND XML ARE NEVER INLINE AND NEVER CARRY THEIR OWN TYPE. A file
// served inline as text/html (or image/svg+xml) from muster's origin is a
// stored XSS: its script runs as muster. The response also carries
// `X-Content-Type-Options: nosniff` and a sandboxing CSP (SetServeHeaders), so
// a browser does not second-guess the type given here.
func ServeAs(contentType string) Serving {
	if inlineTypes[contentType] {
		return Serving{ContentType: contentType, Inline: true}
	}
	if labelledTypes[contentType] {
		return Serving{ContentType: contentType}
	}
	return Serving{ContentType: "application/octet-stream"}
}

// Disposition is the Content-Disposition for a file named name served as s:
// `inline` or `attachment`, with the SafeName as the filename.
func Disposition(s Serving, name string) string {
	kind := "attachment"
	if s.Inline {
		kind = "inline"
	}
	return kind + `; filename="` + SafeName(name) + `"`
}
