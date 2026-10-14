package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/stt"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// DICTATION (transcribe.go), through the real mux and the real stt client,
// against a fake speech-to-text service.
// ---------------------------------------------------------------------------

// sttToken is the credential the fake service demands. A test that finds it in
// anything a browser or the log receives has found a leak.
const sttToken = "stt-secret-fixture-4f9c2a7e1b"

// webmHead is the start of a real WebM file (an EBML header whose DocType is
// "webm"): what Chrome's MediaRecorder writes first.
var webmHead = []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81\x01\x42\xf2\x81\x04\x42\xf3\x81\x08\x42\x82\x84webm\x42\x87\x81\x04\x42\x85\x81\x02")

func webmOf(n int) []byte {
	b := append([]byte(nil), webmHead...)
	for len(b) < n {
		b = append(b, byte(len(b)))
	}
	return b[:n]
}

// fakeSTT plays the service. Its answer is set per test; every request it
// receives is recorded.
type fakeSTT struct {
	srv      *httptest.Server
	mu       sync.Mutex
	status   int
	text     string
	delay    time.Duration
	hold     chan struct{} // when set, every request waits on it
	reqs     atomic.Int32
	auths    []string
	files    []string
	gotBytes []int
}

func newFakeSTT(t *testing.T) *fakeSTT {
	t.Helper()
	f := &fakeSTT{status: http.StatusOK, text: "  hello\nfrom the\tmic\x07  "}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs.Add(1)
		f.mu.Lock()
		status, text, delay, hold := f.status, f.text, f.delay, f.hold
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == "file" {
				b, _ := io.ReadAll(p)
				f.mu.Lock()
				f.files = append(f.files, p.FileName())
				f.gotBytes = append(f.gotBytes, len(b))
				f.mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			// 🔴 A HOSTILE FAILURE BODY: it echoes the credential, so a handler
			// that forwarded the service's words would leak it.
			fmt.Fprintf(w, `{"detail":"refused %s"}`, r.Header.Get("Authorization"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSTT) set(status int, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.text = status, text
}

// transcribeServer is a password-mode server whose dictation goes to fake (or
// is off when fake is nil), logging into logs.
func transcribeServer(t *testing.T, fake *fakeSTT, timeout time.Duration, logs io.Writer) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: stubAgentsStore{}, SessionLiveness: stubLiveness{}}
	if fake != nil {
		c, err := stt.New(fake.srv.URL, sttToken, timeout)
		if err != nil {
			t.Fatal(err)
		}
		ext.Transcriber = c
	}
	s.UseExtensions(ext)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

type txOpt struct {
	noCookie   bool
	origin     string // "" = the server's own
	noOrigin   bool
	chunked    bool // no Content-Length
	clientCtx  context.Context
	extraHeads map[string]string
}

// postAudio posts body to /transcribe as a signed-in browser on the server's
// own origin, unless o says otherwise.
func postAudio(t *testing.T, s *Server, ts *httptest.Server, body []byte, o txOpt) (*http.Response, string) {
	t.Helper()
	var rd io.Reader = bytes.NewReader(body)
	if o.chunked {
		rd = io.MultiReader(bytes.NewReader(body)) // hides the length: sent chunked
	}
	ctx := o.clientCtx
	if ctx == nil {
		ctx = context.Background()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+ui.TranscribePath, rd)
	if !o.chunked {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if !o.noCookie {
		req.Header.Set("Cookie", sessionCookieFor(s))
	}
	switch {
	case o.noOrigin:
	case o.origin != "":
		req.Header.Set("Origin", o.origin)
	default:
		req.Header.Set("Origin", ts.URL)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// noLeak fails when the credential is anywhere in what the browser got or the
// log holds.
func noLeak(t *testing.T, resp *http.Response, body, logs string) {
	t.Helper()
	if strings.Contains(body, sttToken) {
		t.Errorf("the STT TOKEN is in the response body: %q", body)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, sttToken) {
				t.Errorf("the STT TOKEN is in response header %s", k)
			}
		}
	}
	if strings.Contains(logs, sttToken) {
		t.Errorf("the STT TOKEN is in the log:\n%s", logs)
	}
}

// TestDictationTranscribesForASignedInSameOriginCaller is the happy path: the
// recording reaches the service under muster's bearer and a .webm name read
// from the BYTES, and the page gets the cleaned text as text/plain.
func TestDictationTranscribesForASignedInSameOriginCaller(t *testing.T) {
	fake := newFakeSTT(t)
	var logs syncBuf
	s, ts := transcribeServer(t, fake, 0, &logs)
	audio := webmOf(4096)
	resp, body := postAudio(t, s, ts, audio, txOpt{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%q); want 200", resp.StatusCode, body)
	}
	if body != "hello from the mic" {
		t.Errorf("body = %q; want the transcript, whitespace collapsed and the control character dropped", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; the page inserts only text/plain", ct)
	}
	if got := fake.auths; len(got) != 1 || got[0] != "Bearer "+sttToken {
		t.Errorf("the service saw Authorization %q; want muster's bearer", got)
	}
	if len(fake.files) != 1 || fake.files[0] != "recording.webm" || fake.gotBytes[0] != len(audio) {
		t.Errorf("the service got files %v (%v bytes); want recording.webm with all %d bytes", fake.files, fake.gotBytes, len(audio))
	}
	if !strings.Contains(logs.String(), "transcribe: user=") || !strings.Contains(logs.String(), "status=200") {
		t.Errorf("no audit line for the transcription:\n%s", logs.String())
	}
	if pp := resp.Header.Get("Permissions-Policy"); pp != "microphone=(self)" {
		t.Errorf("Permissions-Policy = %q; want microphone=(self) and nothing else", pp)
	}
	noLeak(t, resp, body, logs.String())
}

// TestDictationIsGatedLikeASend: no session, or a foreign/absent origin, is
// refused BEFORE the service is contacted — a cross-origin page must not be
// able to spend muster's credential.
func TestDictationIsGatedLikeASend(t *testing.T) {
	fake := newFakeSTT(t)
	var logs syncBuf
	s, ts := transcribeServer(t, fake, 0, &logs)
	for name, tc := range map[string]struct {
		o    txOpt
		want int
	}{
		"no session":   {txOpt{noCookie: true}, http.StatusUnauthorized},
		"cross-origin": {txOpt{origin: "https://evil.example.test"}, http.StatusForbidden},
		"null origin":  {txOpt{origin: "null"}, http.StatusForbidden},
		"no origin":    {txOpt{noOrigin: true}, http.StatusForbidden},
	} {
		resp, body := postAudio(t, s, ts, webmOf(1024), tc.o)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d (%q); want %d", name, resp.StatusCode, body, tc.want)
		}
		noLeak(t, resp, body, logs.String())
	}
	if n := fake.reqs.Load(); n != 0 {
		t.Errorf("the service was contacted %d time(s) by refused requests", n)
	}
}

// TestDictationOffIsNotFound: with no transcriber the route answers 404 (for a
// signed-in, same-origin caller) and nothing is drawn — see the ui tests.
func TestDictationOffIsNotFound(t *testing.T) {
	s, ts := transcribeServer(t, nil, 0, io.Discard)
	resp, body := postAudio(t, s, ts, webmOf(1024), txOpt{})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d (%q); want 404 with dictation off", resp.StatusCode, body)
	}
	if s.dictatePath() != "" {
		t.Errorf("dictatePath() = %q with no transcriber; want \"\" (no microphone drawn)", s.dictatePath())
	}
}

// TestDictationCapsTheRecording: over the byte cap is 413, whether the length
// was declared (refused before reading) or the body was streamed (refused when
// the cap is reached) — and the streamed case must not hand the service a
// truncated recording to transcribe as if it were whole.
func TestDictationCapsTheRecording(t *testing.T) {
	fake := newFakeSTT(t)
	var logs syncBuf
	s, ts := transcribeServer(t, fake, 0, &logs)

	big := webmOf(maxTranscribeBytes + 4096)
	resp, body := postAudio(t, s, ts, big, txOpt{})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || body != TranscribeTooLarge {
		t.Errorf("declared oversize: %d %q; want 413 %q", resp.StatusCode, body, TranscribeTooLarge)
	}
	if fake.reqs.Load() != 0 {
		t.Error("a declared oversize recording reached the service")
	}

	resp, body = postAudio(t, s, ts, big, txOpt{chunked: true})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || body != TranscribeTooLarge {
		t.Errorf("streamed oversize: %d %q; want 413 %q", resp.StatusCode, body, TranscribeTooLarge)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, n := range fake.gotBytes {
		t.Errorf("the service received a COMPLETE file part of %d bytes from an oversize stream — "+
			"a truncated recording was transcribed as though it were whole", n)
	}
	noLeak(t, resp, body, logs.String())

	// At the cap exactly is accepted (the cap is inclusive, so the bound is the
	// one documented).
	fake.mu.Unlock()
	resp, body = postAudio(t, s, ts, webmOf(maxTranscribeBytes), txOpt{chunked: true})
	fake.mu.Lock()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a recording of exactly the cap: %d %q; want 200", resp.StatusCode, body)
	}
}

// TestDictationRateLimitsPerUser: past the per-user ceiling in a window the
// answer is 429 and the service is not contacted.
func TestDictationRateLimitsPerUser(t *testing.T) {
	fake := newFakeSTT(t)
	s, ts := transcribeServer(t, fake, 0, io.Discard)
	for i := 0; i < transcribeRatePerUser; i++ {
		if resp, body := postAudio(t, s, ts, webmOf(600), txOpt{}); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %q; want 200 inside the limit", i+1, resp.StatusCode, body)
		}
	}
	before := fake.reqs.Load()
	resp, body := postAudio(t, s, ts, webmOf(600), txOpt{})
	if resp.StatusCode != http.StatusTooManyRequests || body != TranscribeRateLimited || resp.Header.Get("Retry-After") == "" {
		t.Errorf("over the limit: %d %q (Retry-After %q); want 429 %q", resp.StatusCode, body, resp.Header.Get("Retry-After"), TranscribeRateLimited)
	}
	if fake.reqs.Load() != before {
		t.Error("a rate-limited request reached the service")
	}
	// A new window admits again.
	s.now = func() time.Time { return time.Now().Add(transcribeRateWindow + time.Second) }
	if resp, _ := postAudio(t, s, ts, webmOf(600), txOpt{}); resp.StatusCode != http.StatusOK {
		t.Errorf("the next window: %d; want 200", resp.StatusCode)
	}
}

// TestDictationCapsWhatIsInFlight: with every slot held, the next is told the
// service is busy rather than queued behind it.
func TestDictationCapsWhatIsInFlight(t *testing.T) {
	fake := newFakeSTT(t)
	fake.hold = make(chan struct{})
	s, ts := transcribeServer(t, fake, 0, io.Discard)
	// Distinct users are not needed: the in-flight cap is process-wide, and it
	// is below the per-user rate limit.
	var wg sync.WaitGroup
	for i := 0; i < transcribeInFlight; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); postAudio(t, s, ts, webmOf(600), txOpt{}) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for fake.reqs.Load() < transcribeInFlight && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp, body := postAudio(t, s, ts, webmOf(600), txOpt{})
	close(fake.hold)
	wg.Wait()
	if resp.StatusCode != http.StatusServiceUnavailable || body != TranscribeBusy {
		t.Errorf("with %d in flight: %d %q; want 503 %q", transcribeInFlight, resp.StatusCode, body, TranscribeBusy)
	}
	if n := fake.reqs.Load(); n != transcribeInFlight {
		t.Errorf("the service saw %d requests; want exactly the %d slots", n, transcribeInFlight)
	}
}

// TestDictationMapsTheServicesAnswers: each failure is muster's own sentence
// and status — never the service's body (which here echoes the credential).
func TestDictationMapsTheServicesAnswers(t *testing.T) {
	for _, tc := range []struct {
		upstream int
		status   int
		body     string
	}{
		{http.StatusBadRequest, http.StatusUnprocessableEntity, TranscribeUndecodable},
		{http.StatusUnauthorized, http.StatusBadGateway, TranscribeRefused},
		{http.StatusForbidden, http.StatusBadGateway, TranscribeRefused},
		{http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, TranscribeTooLarge},
		{http.StatusInternalServerError, http.StatusServiceUnavailable, TranscribeUnavailable},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable, TranscribeUnavailable},
		{http.StatusBadGateway, http.StatusServiceUnavailable, TranscribeUnavailable},
	} {
		fake := newFakeSTT(t)
		fake.set(tc.upstream, "")
		var logs syncBuf
		s, ts := transcribeServer(t, fake, 0, &logs)
		resp, body := postAudio(t, s, ts, webmOf(2048), txOpt{})
		if resp.StatusCode != tc.status || body != tc.body {
			t.Errorf("service %d: muster answered %d %q; want %d %q", tc.upstream, resp.StatusCode, body, tc.status, tc.body)
		}
		if tc.upstream == http.StatusUnauthorized && !strings.Contains(logs.String(), "MUSTER_STT_TOKEN") {
			t.Errorf("a refused credential is not named in the log for the operator:\n%s", logs.String())
		}
		noLeak(t, resp, body, logs.String())
	}
}

// TestDictationTimesOut: a service that does not answer frees the request with
// 503 inside the client's timeout.
func TestDictationTimesOut(t *testing.T) {
	fake := newFakeSTT(t)
	fake.delay = 3 * time.Second
	var logs syncBuf
	s, ts := transcribeServer(t, fake, 200*time.Millisecond, &logs)
	start := time.Now()
	resp, body := postAudio(t, s, ts, webmOf(1024), txOpt{})
	if resp.StatusCode != http.StatusServiceUnavailable || body != TranscribeUnavailable {
		t.Errorf("a silent service: %d %q; want 503 %q", resp.StatusCode, body, TranscribeUnavailable)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the request took %s: the timeout did not bound it", d)
	}
	noLeak(t, resp, body, logs.String())
}

// TestDictationRefusesWhatIsNotAudio: the kind is read from the bytes; an
// empty body or a non-recording never reaches the service.
func TestDictationRefusesWhatIsNotAudio(t *testing.T) {
	fake := newFakeSTT(t)
	s, ts := transcribeServer(t, fake, 0, io.Discard)
	if resp, body := postAudio(t, s, ts, nil, txOpt{}); resp.StatusCode != http.StatusBadRequest || body != TranscribeEmptyBody {
		t.Errorf("empty: %d %q", resp.StatusCode, body)
	}
	if resp, body := postAudio(t, s, ts, []byte("<html><body>not audio</body></html>"), txOpt{}); resp.StatusCode != http.StatusUnsupportedMediaType || body != TranscribeNotAudio {
		t.Errorf("html: %d %q", resp.StatusCode, body)
	}
	if fake.reqs.Load() != 0 {
		t.Error("a non-recording reached the service")
	}
	// The other kinds a browser or a test clip produces are named for the
	// service by their bytes.
	for name, head := range map[string][]byte{
		"recording.mp4": []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"),
		// An ISO-BMFF file whose brands never start with "mp4" (Go's sniffer
		// does not call it video/mp4): still MP4.
		"recording.mp4 (M4A brands)": []byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A isom"),
		"recording.ogg":              []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00"),
		"recording.wav":              []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
	} {
		body := append(append([]byte(nil), head...), make([]byte, 600)...)
		if resp, txt := postAudio(t, s, ts, body, txOpt{}); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %q; want 200", name, resp.StatusCode, txt)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	want := map[string]bool{"recording.mp4": true, "recording.ogg": true, "recording.wav": true}
	for _, f := range fake.files {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("the service was not sent %v (got %v)", want, fake.files)
	}
}

// TestCleanTranscript pins what the box receives.
func TestCleanTranscript(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"  hi  ":                    "hi",
		"one\n\ntwo\tthree":         "one two three",
		"bell\x07 and\x00null":      "bell andnull",
		"keep — unicode, ok? “yes”": "keep — unicode, ok? “yes”",
	} {
		if got := cleanTranscript(in); got != want {
			t.Errorf("cleanTranscript(%q) = %q; want %q", in, got, want)
		}
	}
	if got := cleanTranscript(strings.Repeat("a ", maxTranscriptRunes)); len([]rune(got)) > maxTranscriptRunes {
		t.Errorf("a long transcript was not bounded: %d runes", len([]rune(got)))
	}
}
