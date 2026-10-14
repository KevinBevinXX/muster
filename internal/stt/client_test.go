package stt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureToken = "stt-fixture-token-0123456789"

// seen is what the fake service received.
type seen struct {
	method, path, auth, filename, language, punctuation, ctype string
	audio                                                      []byte
}

// fakeService answers like the real one: it parses the multipart body, records
// it, and answers status with body (or a transcript for 200).
func fakeService(t *testing.T, status int, body string, got *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.auth, got.ctype = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			got.language, got.punctuation = r.FormValue("language"), r.FormValue("punctuation")
			if f, h, err := r.FormFile("file"); err == nil {
				got.filename = h.Filename
				got.audio, _ = io.ReadAll(f)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTranscribeSendsWhatTheServiceDeclares pins the wire contract with the
// service: the path, the bearer, the multipart `file` part under the caller's
// filename, and the two fields sent explicitly.
func TestTranscribeSendsWhatTheServiceDeclares(t *testing.T) {
	var got seen
	srv := fakeService(t, http.StatusOK, `{"text":"hello there","duration_s":1.2,"elapsed_s":0.1}`, &got)
	c, err := New(srv.URL+"/", fixtureToken, 0)
	if err != nil {
		t.Fatal(err)
	}
	text, err := c.Transcribe(context.Background(), strings.NewReader("AUDIO-BYTES"), "recording.webm")
	if err != nil || text != "hello there" {
		t.Fatalf("Transcribe = %q, %v; want the service's text", text, err)
	}
	if got.method != http.MethodPost || got.path != "/v1/audio/transcriptions" {
		t.Errorf("request = %s %s; want POST /v1/audio/transcriptions", got.method, got.path)
	}
	if got.auth != "Bearer "+fixtureToken {
		t.Errorf("Authorization = %q; want the bearer", got.auth)
	}
	if !strings.HasPrefix(got.ctype, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type = %q", got.ctype)
	}
	if got.filename != "recording.webm" || string(got.audio) != "AUDIO-BYTES" {
		t.Errorf("file part = %q (%q); want the caller's name and bytes", got.filename, got.audio)
	}
	if got.language != "en" || got.punctuation != "true" {
		t.Errorf("language=%q punctuation=%q; want en/true sent explicitly", got.language, got.punctuation)
	}
}

// TestStatusMapping is the whole contract with the service's failure answers.
func TestStatusMapping(t *testing.T) {
	for code, want := range map[int]error{
		400: ErrUndecodable, 401: ErrRefused, 403: ErrRefused, 413: ErrTooLong,
		500: ErrUnavailable, 502: ErrUnavailable, 503: ErrUnavailable, 404: ErrUnavailable, 302: ErrUnavailable,
	} {
		if err := StatusError(code); !errors.Is(err, want) {
			t.Errorf("StatusError(%d) = %v; want %v", code, err, want)
		}
	}
	// Through a server too, so the mapping is the one Transcribe applies.
	for code, want := range map[int]error{400: ErrUndecodable, 401: ErrRefused, 413: ErrTooLong, 503: ErrUnavailable} {
		var got seen
		srv := fakeService(t, code, `{"detail":"x"}`, &got)
		c, _ := New(srv.URL, fixtureToken, 0)
		if _, err := c.Transcribe(context.Background(), strings.NewReader("a"), "r.webm"); !errors.Is(err, want) {
			t.Errorf("a %d answer gave %v; want %v", code, err, want)
		}
	}
}

// TestARedirectIsNotFollowed: following one would carry the bearer to wherever
// the answer pointed.
//
// ⚠ 302 AND 303 ARE THE CASES THAT MATTER. net/http cannot replay a streamed
// body, so it never follows a 307/308 for this request whatever CheckRedirect
// says — a 307-only test passed with the guard deleted. A 302/303 is followed
// as a body-less GET, which is exactly what the guard is for.
func TestARedirectIsNotFollowed(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect} {
		var reached atomic.Int32
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
		t.Cleanup(elsewhere.Close)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/steal", code)
		}))
		t.Cleanup(srv.Close)
		c, _ := New(srv.URL, fixtureToken, 0)
		if _, err := c.Transcribe(context.Background(), strings.NewReader("a"), "r.webm"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("a %d gave %v; want ErrUnavailable", code, err)
		}
		if reached.Load() != 0 {
			t.Errorf("a %d was FOLLOWED: the request went to another server", code)
		}
	}
}

// TestATimeoutIsUnavailableAndNamesNoToken: a wedged service frees the caller,
// and the error it leaves carries no credential.
func TestATimeoutIsUnavailableAndNamesNoToken(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	c, _ := New(srv.URL, fixtureToken, 150*time.Millisecond)
	start := time.Now()
	_, err := c.Transcribe(context.Background(), strings.NewReader("a"), "r.webm")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a wedged service gave %v; want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the timeout did not bound the call: %s", d)
	}
	if strings.Contains(err.Error(), fixtureToken) {
		t.Errorf("the error carries the token: %v", err)
	}
}

// failingReader yields some bytes and then fails, like a request body that hit
// its size cap.
type failingReader struct{ sent bool }

var errCap = errors.New("cap reached")

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errCap
}

// TestAFailingAudioReaderFailsTheRequest: the audio is streamed, so a reader
// that fails mid-way must fail the upload rather than send a truncated file the
// service would transcribe as if it were whole.
func TestAFailingAudioReaderFailsTheRequest(t *testing.T) {
	var got seen
	srv := fakeService(t, http.StatusOK, `{"text":"should not be returned"}`, &got)
	c, _ := New(srv.URL, fixtureToken, 0)
	text, err := c.Transcribe(context.Background(), &failingReader{}, "r.webm")
	if err == nil || text != "" {
		t.Fatalf("Transcribe over a failing reader = %q, %v; want a failure", text, err)
	}
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), errCap.Error()) {
		t.Errorf("error = %v; want ErrUnavailable naming the reader's failure", err)
	}
}

// TestNewRefusesAnUnusableConfiguration.
func TestNewRefusesAnUnusableConfiguration(t *testing.T) {
	for _, u := range []string{"", "stt.example.test:8000", "ftp://x", "http://", "http://h/v1", "http://u:p@h", "http://h?x=1", "http://h#f"} {
		if _, err := New(u, fixtureToken, 0); err == nil {
			t.Errorf("New(%q) accepted a URL that is not http(s)://host[:port]", u)
		}
	}
	if _, err := New("http://stt.example.test:8000", "  ", 0); err == nil {
		t.Error("New accepted an empty token")
	}
	c, err := New("https://stt.example.test:8443/", fixtureToken, 0)
	if err != nil || c.Endpoint() != "https://stt.example.test:8443/v1/audio/transcriptions" {
		t.Errorf("New(valid) = %v, %v", c, err)
	}
}
