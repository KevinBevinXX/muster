package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// ---------------------------------------------------------------------------
// FILE ATTACHMENTS (uploads.go), through the real mux, the real service and
// signer, over in-memory row and object stores. Row-level security itself is
// tested against Postgres in internal/uploads and by the isolation walk.
// ---------------------------------------------------------------------------

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89-pixels")

type upClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *upClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *upClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type upWorld struct {
	s     *Server
	ts    *httptest.Server
	svc   *uploads.Service
	rows  *uploadstest.Store
	objs  *uploadstest.Objects
	clock *upClock
	logs  *syncBuf
}

func uploadServer(t *testing.T, on bool, max int64) *upWorld {
	t.Helper()
	logs := &syncBuf{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: stubAgentsStore{}, SessionLiveness: stubLiveness{}}
	w := &upWorld{s: s, rows: &uploadstest.Store{}, objs: &uploadstest.Objects{}, clock: &upClock{t: time.Now()}, logs: logs}
	if on {
		sg, err := uploads.NewSigner("api-upload-test-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		w.svc = &uploads.Service{Store: w.rows, Objects: w.objs, Signer: sg, PublicURL: "https://muster.example.test",
			MaxBytes: max, LinkTTL: time.Hour, Now: w.clock.now}
		ext.Uploads = w.svc
	}
	s.UseExtensions(ext)
	w.ts = httptest.NewServer(s.Handler())
	t.Cleanup(w.ts.Close)
	return w
}

type upOpt struct {
	noCookie bool
	origin   string
	fields   map[string]string
	field    string // the file part's name; "" = "file"
}

// postFile uploads data as a signed-in, same-origin browser unless o says
// otherwise.
func (w *upWorld) postFile(t *testing.T, name string, data []byte, o upOpt) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range o.fields {
		_ = mw.WriteField(k, v)
	}
	field := o.field
	if field == "" {
		field = "file"
	}
	fw, _ := mw.CreateFormFile(field, name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, w.ts.URL+ui.UploadPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if !o.noCookie {
		req.Header.Set("Cookie", sessionCookieFor(w.s))
	}
	if o.origin != "" {
		req.Header.Set("Origin", o.origin)
	} else {
		req.Header.Set("Origin", w.ts.URL)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (w *upWorld) do(t *testing.T, method, path string, signedIn bool) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, w.ts.URL+path, nil)
	if signedIn {
		req.Header.Set("Cookie", sessionCookieFor(w.s))
		req.Header.Set("Origin", w.ts.URL)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func mustUpload(t *testing.T, w *upWorld, name string, data []byte) uploadReply {
	t.Helper()
	resp, body := w.postFile(t, name, data, upOpt{fields: map[string]string{"host": "laptop", "session": "work", "window": "@1"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %q", name, resp.StatusCode, body)
	}
	var rep uploadReply
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("upload reply %q: %v", body, err)
	}
	return rep
}

// linkPath is the path+query of a signed link (the test server is not at the
// public URL the link names).
func linkPath(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

// TestAnUploadIsStoredAndAnsweredWithItsLine: the happy path. The type is the
// sniffed one (the name says .txt, the bytes say PNG), the key is random, the
// row records the composer it came from, and the line carries a link on the
// PUBLIC URL.
func TestAnUploadIsStoredAndAnsweredWithItsLine(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	rep := mustUpload(t, w, "Screen Shot.txt", pngBytes)
	if rep.ContentType != "image/png" || !rep.Image || rep.Size != int64(len(pngBytes)) || rep.Name != "Screen_Shot.txt" {
		t.Errorf("reply = %+v", rep)
	}
	if !uploads.ValidPublicID(rep.ID) || rep.Preview != "/uploads/"+rep.ID+"/file" || rep.Revoke != "/uploads/"+rep.ID+"/revoke" {
		t.Errorf("reply addresses = %+v", rep)
	}
	if !strings.HasPrefix(rep.Line, "[attached: Screen_Shot.txt (image/png, ") ||
		!strings.Contains(rep.Line, " https://muster.example.test/f/"+rep.ID+"?exp=") {
		t.Errorf("line = %q", rep.Line)
	}
	if refs := uploads.FindRefs(rep.Line); len(refs) != 1 || refs[0].PublicID != rep.ID {
		t.Errorf("the line does not parse back: %+v", refs)
	}
	rows := w.rows.Rows()
	if len(rows) != 1 || rows[0].Filename != "Screen Shot.txt" || rows[0].Size != int64(len(pngBytes)) {
		t.Fatalf("rows = %+v", rows)
	}
	if keys := w.objs.Keys(); len(keys) != 1 || keys[0] != "uploads/"+rep.ID {
		t.Errorf("object keys = %v", keys)
	}
}

// TestAnUploadIsGatedLikeASend: no session is 401; a foreign, null or absent
// origin is 403 — and nothing is stored for any of them.
func TestAnUploadIsGatedLikeASend(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	for name, tc := range map[string]struct {
		o    upOpt
		want int
	}{
		"no session":   {upOpt{noCookie: true}, http.StatusUnauthorized},
		"cross-origin": {upOpt{origin: "https://evil.example.test"}, http.StatusForbidden},
		"null origin":  {upOpt{origin: "null"}, http.StatusForbidden},
	} {
		resp, body := w.postFile(t, "a.png", pngBytes, tc.o)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d (%q); want %d", name, resp.StatusCode, body, tc.want)
		}
	}
	if n := len(w.objs.Keys()); n != 0 {
		t.Errorf("%d object(s) stored by refused requests", n)
	}
}

// TestUploadsOffIsNotFound: with no service every route — the signed link
// included — answers 404, and no composer draws a paperclip.
func TestUploadsOffIsNotFound(t *testing.T) {
	w := uploadServer(t, false, 0)
	if resp, body := w.postFile(t, "a.png", pngBytes, upOpt{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /uploads: %d (%q); want 404", resp.StatusCode, body)
	}
	id := "AAAAAAAAAAAAAAAAAAAAAA"
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/uploads/" + id + "/file"},
		{http.MethodPost, "/uploads/" + id + "/revoke"},
		{http.MethodGet, "/f/" + id + "?exp=9999999999&sig=" + strings.Repeat("A", 43)},
	} {
		if resp, _ := w.do(t, c.method, c.path, true); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %d; want 404", c.method, c.path, resp.StatusCode)
		}
	}
	if w.s.uploadPath() != "" {
		t.Errorf("uploadPath() = %q with uploads off; want \"\"", w.s.uploadPath())
	}
}

// TestTheUploadCapIsEnforcedByTheRoute: exactly the cap is stored; one byte
// more is 413 with the cap in a header, and nothing is stored.
func TestTheUploadCapIsEnforcedByTheRoute(t *testing.T) {
	const max = 4096
	w := uploadServer(t, true, max)
	if resp, body := w.postFile(t, "ok.bin", bytes.Repeat([]byte{0xff}, max), upOpt{}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a file of exactly the cap: %d %q", resp.StatusCode, body)
	}
	resp, body := w.postFile(t, "big.bin", bytes.Repeat([]byte{0xff}, max+1), upOpt{})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || body != UploadTooLarge {
		t.Errorf("one byte over: %d %q; want 413 %q", resp.StatusCode, body, UploadTooLarge)
	}
	if h := resp.Header.Get("X-Muster-Max-Bytes"); h != strconv.Itoa(max) {
		t.Errorf("X-Muster-Max-Bytes = %q; want %d", h, max)
	}
	if n := len(w.objs.Keys()); n != 1 {
		t.Errorf("%d objects; want only the one at the cap", n)
	}
	// Far over, so the body cap (not the part cap) is what trips.
	resp, _ = w.postFile(t, "huge.bin", bytes.Repeat([]byte{0xff}, max+256<<10), upOpt{})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("far over: %d; want 413", resp.StatusCode)
	}
	if resp, body := w.postFile(t, "empty.bin", nil, upOpt{}); resp.StatusCode != http.StatusBadRequest || body != UploadEmpty {
		t.Errorf("empty: %d %q; want 400 %q", resp.StatusCode, body, UploadEmpty)
	}
	if resp, body := w.postFile(t, "x.bin", []byte("x"), upOpt{field: "notfile"}); resp.StatusCode != http.StatusBadRequest || body != UploadNoFile {
		t.Errorf("no file part: %d %q; want 400 %q", resp.StatusCode, body, UploadNoFile)
	}
}

// TestTheSignedLinkServesTheFileAndNothingElse: the link — with NO session —
// serves the bytes with the server's headers; the same path unsigned,
// tampered, expired or revoked is 404, one answer for all.
func TestTheSignedLinkServesTheFileAndNothingElse(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	rep := mustUpload(t, w, "shot.png", pngBytes)
	link := linkPath(t, strings.Fields(rep.Line[strings.Index(rep.Line, ") ")+2:])[0])

	resp, body := w.do(t, http.MethodGet, link, false)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, pngBytes) {
		t.Fatalf("the signed link: %d, %d bytes; want 200 and the %d uploaded", resp.StatusCode, len(body), len(pngBytes))
	}
	for k, want := range map[string]string{
		"Content-Type":           "image/png",
		"Content-Disposition":    `inline; filename="shot.png"`,
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, no-store",
		"Content-Length":         strconv.Itoa(len(pngBytes)),
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q; want a sandboxing default-src 'none'", csp)
	}

	u, _ := url.Parse(link)
	q := u.Query()
	sig := q.Get("sig")
	tampered := url.Values{"exp": {q.Get("exp")}, "sig": {"B" + sig[1:]}}
	if sig[0] == 'B' {
		tampered.Set("sig", "C"+sig[1:])
	}
	extended := url.Values{"exp": {strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)}, "sig": {sig}}
	for name, path := range map[string]string{
		"unsigned":        u.Path,
		"tampered sig":    u.Path + "?" + tampered.Encode(),
		"extended expiry": u.Path + "?" + extended.Encode(),
		"another id":      "/f/BBBBBBBBBBBBBBBBBBBBBB?" + q.Encode(),
	} {
		if resp, body := w.do(t, http.MethodGet, path, false); resp.StatusCode != http.StatusNotFound || bytes.Contains(body, pngBytes[:8]) {
			t.Errorf("%s: %d (%d bytes); want 404 and none of the file", name, resp.StatusCode, len(body))
		}
	}

	// Expired: the same link, after its expiry.
	w.clock.set(time.Now().Add(2 * time.Hour))
	if resp, _ := w.do(t, http.MethodGet, link, false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("expired: %d; want 404", resp.StatusCode)
	}
	w.clock.set(time.Now())
	if resp, _ := w.do(t, http.MethodGet, link, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL: back inside its expiry the link answers %d", resp.StatusCode)
	}

	// Revoked, through the owner's route: 204, then the link is 404 and the
	// object is gone.
	if resp, _ := w.do(t, http.MethodPost, rep.Revoke, false); resp.StatusCode == http.StatusNoContent {
		t.Error("revoke WITHOUT a session answered 204")
	}
	if resp, body := w.do(t, http.MethodPost, rep.Revoke, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner's revoke: %d %q", resp.StatusCode, body)
	}
	if resp, _ := w.do(t, http.MethodGet, link, false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after revoke: %d; want 404", resp.StatusCode)
	}
	if w.objs.Has("uploads/" + rep.ID) {
		t.Error("the object outlived its revocation")
	}
	if resp, _ := w.do(t, http.MethodPost, "/uploads/BBBBBBBBBBBBBBBBBBBBBB/revoke", true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoking an unknown id: %d; want 404", resp.StatusCode)
	}
}

// TestMarkupIsServedAsAnAttachment: an HTML file named like an image, through
// the signed link and the owner's view alike, is application/octet-stream
// and an attachment — never text/html inline from muster's origin.
func TestMarkupIsServedAsAnAttachment(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	evil := []byte("<!DOCTYPE html><script>fetch('/api/whoami').then(r=>r.text()).then(alert)</script>")
	rep := mustUpload(t, w, "cute.png", evil)
	if rep.Image {
		t.Errorf("an HTML file was reported as an image: %+v", rep)
	}
	link := linkPath(t, strings.Fields(rep.Line[strings.Index(rep.Line, ") ")+2:])[0])
	for name, c := range map[string]struct {
		path     string
		signedIn bool
	}{"signed link": {link, false}, "owner's view": {rep.Preview, true}} {
		resp, body := w.do(t, http.MethodGet, c.path, c.signedIn)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, evil) {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s: Content-Type %q; want application/octet-stream", name, ct)
		}
		if d := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(d, "attachment;") {
			t.Errorf("%s: Content-Disposition %q; want attachment", name, d)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", name)
		}
	}
}

// TestTheOwnersViewNeedsASession: the thumbnail route is session-gated.
func TestTheOwnersViewNeedsASession(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	rep := mustUpload(t, w, "shot.png", pngBytes)
	if resp, body := w.do(t, http.MethodGet, rep.Preview, true); resp.StatusCode != http.StatusOK || !bytes.Equal(body, pngBytes) {
		t.Fatalf("owner: %d", resp.StatusCode)
	}
	resp, body := w.do(t, http.MethodGet, rep.Preview, false)
	if resp.StatusCode == http.StatusOK || bytes.Contains(body, pngBytes[:8]) {
		t.Errorf("no session: %d with %d bytes; want a refusal and none of the file", resp.StatusCode, len(body))
	}
}
