package api

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// EVERY ROUTE THAT SERVES UPLOADED BYTES SERVES THEM NON-EXECUTABLE.
//
// A file served from muster's own origin and rendered by the browser as a
// document (HTML, SVG, XHTML, anything sniffed as one) runs its script AS
// muster, with the operator's session: stored XSS. The three routes that serve
// bytes someone uploaded are
//
//   - GET /f/{id}?exp&sig          the signed link (uploads)
//   - GET /uploads/{id}/file       the owner's view (uploads)
//   - GET /tasks/{id}/attachments/{aid}   a task's attachment (notes)
//
// and each must answer every hostile fixture below with ALL of: a type that
// names no markup, `nosniff`, a sandboxing CSP, and an `attachment`
// disposition — unless the bytes are one of the four raster image types, the
// only thing allowed inline. The fixtures are synthetic.
// ---------------------------------------------------------------------------

// hostileFixtures: what an attacker would upload, each under a name and a
// claimed Content-Type that try to make it look harmless.
var hostileFixtures = []struct {
	name, filename, claimed string
	data                    []byte
}{
	{"html", "cute.png", "text/html",
		[]byte("<!DOCTYPE html><html><body><script>fetch('/api/whoami').then(r=>r.text()).then(alert)</script></body></html>")},
	{"svg", "logo.svg", "image/svg+xml",
		[]byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(document.domain)"><script>alert(1)</script></svg>`)},
	{"bare svg", "logo.svg", "image/svg+xml",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(document.domain)"><circle r="5"/></svg>`)},
	// A PDF/HTML polyglot: the bytes sniff as a PDF, the tail is a page. A PDF
	// is not one of the inline image types, so it must not be inline either.
	{"pdf-html polyglot", "report.pdf", "text/html",
		[]byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n<html><body><script>alert(document.domain)</script></body></html>\n%%EOF\n")},
	// A GIF/JS/HTML polyglot: valid as a GIF header, valid as script, and
	// carrying markup. It sniffs as an image and MAY be inline as image/gif —
	// so the guard here is that nosniff and the sandbox still come with it.
	{"gif-html polyglot", "pixel.gif", "text/html",
		[]byte("GIF89a/*\x01\x00\x01\x00\x80\x00\x00*/=1;alert(document.domain)//<html><script>alert(1)</script></html>")},
}

// inlineAllowed is the whole inline allowlist, written out literally rather
// than read from the code under test.
var inlineAllowed = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// assertNotExecutable checks the headers that make a response non-executable.
func assertNotExecutable(t *testing.T, label string, h http.Header) {
	t.Helper()
	ct := strings.ToLower(h.Get("Content-Type"))
	for _, bad := range []string{"html", "xml", "svg", "script"} {
		if strings.Contains(ct, bad) {
			t.Errorf("%s: Content-Type %q names %s", label, ct, bad)
		}
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q; want nosniff", label, got)
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("%s: Content-Security-Policy = %q; want sandbox and default-src 'none'", label, csp)
	}
	disp := h.Get("Content-Disposition")
	if !inlineAllowed[ct] && !strings.HasPrefix(disp, "attachment;") {
		t.Errorf("%s: Content-Disposition = %q for %q; only png/jpeg/gif/webp may be inline", label, disp, ct)
	}
	if strings.Count(disp, `"`) != 2 || strings.ContainsAny(disp, "\r\n") {
		t.Errorf("%s: Content-Disposition %q lets the filename break out of its quotes", label, disp)
	}
}

// --- the uploads routes -----------------------------------------------------

func TestUploadRoutesServeHostileFilesNonExecutable(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	for _, fx := range hostileFixtures {
		rep := mustUpload(t, w, fx.filename, fx.data)
		link := linkPath(t, strings.Fields(rep.Line[strings.Index(rep.Line, ") ")+2:])[0])
		for route, c := range map[string]struct {
			path     string
			signedIn bool
		}{"signed link": {link, false}, "owner's view": {rep.Preview, true}} {
			resp, body := w.do(t, http.MethodGet, c.path, c.signedIn)
			if resp.StatusCode != http.StatusOK || !bytes.Equal(body, fx.data) {
				t.Fatalf("%s %s: %d, %d bytes", fx.name, route, resp.StatusCode, len(body))
			}
			assertNotExecutable(t, fx.name+" via "+route, resp.Header)
		}
	}
}

// The positive control: a PNG is still an inline image/png on both routes.
// Without it a handler that refused every file, or made everything an
// attachment, would pass the test above.
func TestUploadRoutesStillServeAPNGInline(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	rep := mustUpload(t, w, "shot.png", pngBytes)
	link := linkPath(t, strings.Fields(rep.Line[strings.Index(rep.Line, ") ")+2:])[0])
	for route, c := range map[string]struct {
		path     string
		signedIn bool
	}{"signed link": {link, false}, "owner's view": {rep.Preview, true}} {
		resp, body := w.do(t, http.MethodGet, c.path, c.signedIn)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, pngBytes) {
			t.Fatalf("%s: %d", route, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s: Content-Type %q; want image/png", route, ct)
		}
		if d := resp.Header.Get("Content-Disposition"); d != `inline; filename="shot.png"` {
			t.Errorf("%s: Content-Disposition %q; want inline", route, d)
		}
		assertNotExecutable(t, "png via "+route, resp.Header)
	}
}

// --- task attachments -------------------------------------------------------

// attachServeStore serves stored attachments by id.
type attachServeStore struct {
	attachCaptureStore
	byID map[int64]notes.Attachment
}

func (s *attachServeStore) GetAttachment(_ context.Context, id int64) (notes.Attachment, error) {
	a, ok := s.byID[id]
	if !ok {
		return notes.Attachment{}, pgx.ErrNoRows
	}
	return a, nil
}

// getTaskAttachment fetches /tasks/{note}/attachments/{aid} through the real
// mux, signed in.
func getTaskAttachment(t *testing.T, s *Server, note, aid int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+strconv.FormatInt(note, 10)+"/attachments/"+strconv.FormatInt(aid, 10), nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func newTaskAttachmentServer(store *attachServeStore) *Server {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: store, Agents: stubAgentsStore{}, SessionLiveness: stubLiveness{}})
	return s
}

// A row stored with the CLIENT's claimed type (every row written before this
// fix, and what an attacker controls) must still be served non-executable: the
// serve path reads the bytes, never the stored claim.
func TestTaskAttachmentServesHostileFilesNonExecutable(t *testing.T) {
	store := &attachServeStore{byID: map[int64]notes.Attachment{}}
	for i, fx := range hostileFixtures {
		id := int64(i + 1)
		store.byID[id] = notes.Attachment{ID: id, NoteID: 7, Filename: fx.filename + "\"; x=\"y", ContentType: fx.claimed,
			SizeBytes: int64(len(fx.data)), Data: fx.data}
	}
	s := newTaskAttachmentServer(store)
	for i, fx := range hostileFixtures {
		rec := getTaskAttachment(t, s, 7, int64(i+1))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), fx.data) {
			t.Fatalf("%s: %d, %d bytes", fx.name, rec.Code, rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Type"); got == fx.claimed {
			t.Errorf("%s: the client's claimed Content-Type %q was echoed", fx.name, got)
		}
		assertNotExecutable(t, fx.name+" via task attachment", rec.Header())
	}
}

// Positive control for the route above: a PNG stored under a claimed
// text/html is served as what its bytes are, inline.
func TestTaskAttachmentStillServesAPNGInline(t *testing.T) {
	store := &attachServeStore{byID: map[int64]notes.Attachment{
		1: {ID: 1, NoteID: 7, Filename: "shot.png", ContentType: "text/html", SizeBytes: int64(len(pngBytes)), Data: pngBytes},
	}}
	rec := getTaskAttachment(t, newTaskAttachmentServer(store), 7, 1)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("png: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type %q; want image/png", ct)
	}
	if d := rec.Header().Get("Content-Disposition"); d != `inline; filename="shot.png"` {
		t.Errorf("Content-Disposition %q; want inline", d)
	}
	assertNotExecutable(t, "png via task attachment", rec.Header())
}

// The write side, end to end: a task created with an attachment stores the
// uploader's LABEL as given (content_type has one meaning: what the uploader
// said), and the bytes are still SERVED as what they are — serve-time sniffing
// is the only authority. Create, then fetch through the real route.
func TestTaskCreateKeepsTheLabelAndServesTheSniffedType(t *testing.T) {
	cases := []struct {
		name, claimed, want string
		data                []byte
	}{
		{"html claimed as an image", "image/png", "application/octet-stream", hostileFixtures[0].data},
		{"svg claimed as svg", "image/svg+xml", "application/octet-stream", hostileFixtures[1].data},
		{"html claimed as html", "text/html", "application/octet-stream", hostileFixtures[0].data},
		// The control: the served type is derived from the bytes, not defaulted —
		// a PNG claimed as HTML is served as the image/png it is.
		{"png claimed as html", "text/html", "image/png", pngBytes},
		// A correctly labelled PDF keeps its label for the UI.
		{"pdf labelled pdf", "application/pdf", "application/pdf", hostileFixtures[3].data},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("body", "a task with an attachment")
			hdr := textproto.MIMEHeader{}
			hdr.Set("Content-Disposition", `form-data; name="attachments"; filename="f.bin"`)
			hdr.Set("Content-Type", tc.claimed)
			part, err := mw.CreatePart(hdr)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write(tc.data)
			_ = mw.Close()
			req := httptest.NewRequest(http.MethodPost, "/tasks", &buf)
			req.Header.Set("Content-Type", mw.FormDataContentType())

			store := &attachServeStore{byID: map[int64]notes.Attachment{}}
			s := newTaskAttachmentServer(store)
			rec := httptest.NewRecorder()
			s.handleNoteCreate(rec, req)
			if rec.Code != http.StatusOK || len(store.attached) != 1 {
				t.Fatalf("create: %d, %d attachment(s)", rec.Code, len(store.attached))
			}
			att := store.attached[0]
			if att.ContentType != tc.claimed {
				t.Errorf("stored ContentType %q; want the uploader's label %q", att.ContentType, tc.claimed)
			}
			store.byID[att.ID] = att
			got := getTaskAttachment(t, s, att.NoteID, att.ID)
			if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), tc.data) {
				t.Fatalf("serve: %d", got.Code)
			}
			if ct := got.Header().Get("Content-Type"); ct != tc.want {
				t.Errorf("served Content-Type %q; want %q (labelled %q)", ct, tc.want, tc.claimed)
			}
			assertNotExecutable(t, tc.name, got.Header())
		})
	}
}
