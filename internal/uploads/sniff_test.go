package uploads

import (
	"net/http"
	"strings"
	"testing"
)

// Real file heads, as the bytes a browser would upload.
var (
	pngHead  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	jpegHead = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	gifHead  = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00")
	webpHead = []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00")
	pdfHead  = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n")
	zipHead  = []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00")
)

// TestHTMLSVGAndXMLAreNeverServedInline: every shape of markup that would run
// script in muster's origin — HTML however it starts, SVG with or without an
// XML declaration, XHTML, plain XML — is served as an attachment typed
// application/octet-stream, whatever name it was uploaded under.
func TestHTMLSVGAndXMLAreNeverServedInline(t *testing.T) {
	for name, body := range map[string]string{
		"html doctype":     "<!DOCTYPE html><html><body><script>alert(document.cookie)</script></body></html>",
		"html bare script": "<script>fetch('/api/whoami')</script>",
		"html after space": "\n\n   <html><img src=x onerror=alert(1)>",
		"svg":              `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><circle r="5"/></svg>`,
		"svg with xml decl": `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"xhtml":        `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><script>alert(1)</script></html>`,
		"plain xml":    `<?xml version="1.0"?><root/>`,
		"html comment": "<!-- hi --><iframe src=javascript:alert(1)>",
	} {
		t.Run(name, func(t *testing.T) {
			sniffed := Sniff([]byte(body))
			sv := ServeAs(sniffed)
			if sv.Inline {
				t.Errorf("sniffed %q is served INLINE", sniffed)
			}
			// A bare <svg> sniffs as plain text, and is served as the plain text it
			// then is: an attachment, under nosniff, which no browser renders as
			// SVG. Everything else that sniffs as markup is octet-stream.
			if sv.ContentType != "application/octet-stream" && sv.ContentType != "text/plain; charset=utf-8" {
				t.Errorf("sniffed %q is served as %q; want application/octet-stream or text/plain", sniffed, sv.ContentType)
			}
			if strings.HasPrefix(sniffed, "text/html") || strings.HasPrefix(sniffed, "text/xml") {
				if sv.ContentType != "application/octet-stream" {
					t.Errorf("sniffed markup %q is served as %q; want application/octet-stream", sniffed, sv.ContentType)
				}
			}
			for _, bad := range []string{"html", "svg", "xml", "script"} {
				if strings.Contains(sv.ContentType, bad) {
					t.Errorf("served type %q names %s", sv.ContentType, bad)
				}
			}
			if d := Disposition(sv, "evil.html"); !strings.HasPrefix(d, "attachment;") {
				t.Errorf("Content-Disposition %q; want attachment", d)
			}
		})
	}
}

// TestOnlyRasterImagesAreInline: the allowlist, read from the BYTES. A PNG
// named .html is an image; an HTML file named .png is not; a PDF is a labelled
// attachment, never inline.
func TestOnlyRasterImagesAreInline(t *testing.T) {
	for name, tc := range map[string]struct {
		head   []byte
		want   string
		inline bool
		image  bool
	}{
		"png":  {pngHead, "image/png", true, true},
		"jpeg": {jpegHead, "image/jpeg", true, true},
		"gif":  {gifHead, "image/gif", true, true},
		"webp": {webpHead, "image/webp", true, true},
		"pdf":  {pdfHead, "application/pdf", false, false},
		"zip":  {zipHead, "application/zip", false, false},
		"text": {[]byte("just some notes\nline two\n"), "text/plain; charset=utf-8", false, false},
		"js":   {[]byte("alert(1);\nwindow.location='https://evil.example';"), "text/plain; charset=utf-8", false, false},
		"bin":  {[]byte{0x00, 0x01, 0x02, 0xfe, 0xff, 0x10}, "application/octet-stream", false, false},
	} {
		got := Sniff(tc.head)
		if got != tc.want {
			t.Errorf("%s: sniffed %q; want %q", name, got, tc.want)
			continue
		}
		sv := ServeAs(got)
		if sv.Inline != tc.inline {
			t.Errorf("%s: inline=%v; want %v", name, sv.Inline, tc.inline)
		}
		if InlineImage(got) != tc.image {
			t.Errorf("%s: InlineImage=%v; want %v", name, InlineImage(got), tc.image)
		}
		wantDisp := "attachment;"
		if tc.inline {
			wantDisp = "inline;"
		}
		if d := Disposition(sv, "x"); !strings.HasPrefix(d, wantDisp) {
			t.Errorf("%s: disposition %q; want %s", name, d, wantDisp)
		}
	}
}

// TestTheDispositionFilenameIsTheSafeName: a hostile name cannot break out of
// the header's quoted filename.
func TestTheDispositionFilenameIsTheSafeName(t *testing.T) {
	d := Disposition(ServeAs("image/png"), "a\"; filename=evil.html\r\nX-Injected: 1.png")
	if strings.ContainsAny(d, "\r\n") || strings.Count(d, `"`) != 2 {
		t.Errorf("disposition %q carries the name's quotes or line breaks", d)
	}
	if !strings.HasPrefix(d, `inline; filename="a_filename_evil.html_X-Injected_1.png"`) {
		t.Errorf("disposition = %q", d)
	}
}

// TestSetServeHeadersAloneMakesAFileInert: the helper sets every header itself,
// on an EMPTY header map — it does not lean on the server's page middleware
// (which also sets nosniff, so a route test cannot see the helper drop it).
func TestSetServeHeadersAloneMakesAFileInert(t *testing.T) {
	for sniffed, want := range map[string]struct{ ct, disp string }{
		"text/html; charset=utf-8": {"application/octet-stream", `attachment; filename="a.html"`},
		"text/xml; charset=utf-8":  {"application/octet-stream", `attachment; filename="a.html"`},
		"application/pdf":          {"application/pdf", `attachment; filename="a.html"`},
		"image/png":                {"image/png", `inline; filename="a.html"`},
	} {
		h := http.Header{}
		SetServeHeaders(h, sniffed, "a.html", 42)
		if h.Get("Content-Type") != want.ct || h.Get("Content-Disposition") != want.disp {
			t.Errorf("%s: Content-Type %q, Content-Disposition %q; want %q, %q", sniffed,
				h.Get("Content-Type"), h.Get("Content-Disposition"), want.ct, want.disp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", sniffed)
		}
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP %q", sniffed, csp)
		}
		if h.Get("Cache-Control") != "private, no-store" || h.Get("Content-Length") != "42" {
			t.Errorf("%s: Cache-Control %q, Content-Length %q", sniffed, h.Get("Cache-Control"), h.Get("Content-Length"))
		}
	}
}
