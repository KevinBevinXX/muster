package uploads

import (
	"net/http"
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

// inlineTypes are the ONLY types served inline: raster images and PDF. Each is
// a format a browser renders without running anything from the file in the
// page's origin.
var inlineTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// labelledTypes are served with their own type, as an ATTACHMENT. Every other
// sniffed type — text/html, text/xml (SVG sniffs as XML or as text), any
// script, anything unrecognised — is served as application/octet-stream.
var labelledTypes = map[string]bool{
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
// `X-Content-Type-Options: nosniff` and a sandboxing CSP (the handler's), so a
// browser does not second-guess the type given here.
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
