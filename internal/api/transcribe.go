package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ZacxDev/muster/internal/stt"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// DICTATION: POST /transcribe — the message box's microphone.
//
// The browser records (MediaRecorder) and posts the recording here as the raw
// request body; muster forwards it to the speech-to-text service with ITS OWN
// credential (internal/stt) and answers with the text, as text/plain. The page
// inserts that text into the message box at the caret — it is never sent: the
// operator reads it and presses Send, which is the gated step.
//
// 🔴 ONE ROUTE FOR EVERY MESSAGE BOX. A mirrored session's two views and an
// agent's two views all post here; what the text is FOR is decided by the Send
// that follows, so this route needs no address.
//
// 🔴 GATED LIKE A SEND, MINUS THE STEP-UP. requireOperatorWrite: a signed-in
// session and a same-origin request — a cross-origin page must not be able to
// spend this server's speech-to-text credential, or probe it. The step-up a
// mirror send asks for is NOT asked here, on purpose: a transcription types
// nothing into any session. It returns words to the page that asked, and those
// words reach a terminal only through a Send, which is gated (fresh sign-in
// included) exactly as if they had been typed. Asking for a passkey to dictate
// would gate the same paste twice and add nothing.
//
// BOUNDED FOUR WAYS: a byte cap on the body (the page also stops recording at
// ui.DictateMaxSeconds), a per-user rate limit, a cap on transcriptions in
// flight across the process (the service is one GPU), and the client's timeout
// (stt.DefaultTimeout). The body is STREAMED to the service, never held whole.
//
// 🔴 THE TOKEN NEVER LEAVES THIS PROCESS. Nothing the service says is forwarded
// either: every answer here is one of this file's own sentences, so a failure
// body written for the token's holder never reaches a browser, and the audit
// line names an outcome, a size and a kind — never a credential.
// ---------------------------------------------------------------------------

// Transcriber turns a recording into text (internal/stt's Client).
type Transcriber interface {
	Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error)
}

const (
	// maxTranscribeBytes caps a recording. The page asks the browser for 64
	// kbit/s and stops at ui.DictateMaxSeconds (two minutes ≈ 1 MiB); browsers
	// treat the bitrate as a hint and AAC on iOS runs higher, so the cap leaves
	// room for several times that and still bounds what one request can make
	// this process forward.
	maxTranscribeBytes = 8 << 20
	// transcribeRatePerUser and transcribeRateWindow: how many recordings one
	// user may send in a window. A person dictating sends one every several
	// seconds at the most; this ceiling stops a loop, not a person.
	transcribeRatePerUser = 12
	transcribeRateWindow  = time.Minute
	// transcribeInFlight caps transcriptions running at once, process-wide. The
	// service runs one model on one GPU; queueing more behind it here only makes
	// every one of them slower to fail.
	transcribeInFlight = 3
)

// transcribeLimits is the route's throttle state; the zero value is ready.
type transcribeLimits struct {
	perUser boundedWindow
	mu      sync.Mutex
	running int
}

// enter claims an in-flight slot; the caller calls the returned func when done.
func (l *transcribeLimits) enter() (leave func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running >= transcribeInFlight {
		return nil, false
	}
	l.running++
	return func() {
		l.mu.Lock()
		l.running--
		l.mu.Unlock()
	}, true
}

// The route's answers. Each is muster's own sentence; the page shows it as is.
const (
	TranscribeTooLarge    = "That recording is too long to transcribe. Record a shorter one (up to two minutes)."
	TranscribeRateLimited = "Too many recordings in a short time. Wait a moment and try again."
	TranscribeBusy        = "The transcription service is busy. Try again in a moment."
	TranscribeEmptyBody   = "No recording arrived, so there was nothing to transcribe."
	TranscribeNotAudio    = "That is not a recording this server can read (WebM, MP4, Ogg or WAV audio)."
	TranscribeCutOff      = "The recording did not arrive whole, so nothing was transcribed. Try again."
	TranscribeUndecodable = "That recording could not be decoded, so nothing was transcribed. Record it again."
	TranscribeRefused     = "The transcription service refused this server's credential, so nothing was transcribed. " +
		"This is a fault in muster's configuration, not something signing in again would fix."
	TranscribeUnavailable = "The transcription service did not answer, so nothing was transcribed. Try again in a moment."
)

// transcribeKinds maps what http.DetectContentType reads from the first bytes to
// the filename the service is given (its decoder picks the container by the
// name's extension, see stt.Client.Transcribe).
//
// 🔴 READ FROM THE BYTES, NOT FROM THE BROWSER'S Content-Type. Chrome's
// `audio/webm;codecs=opus` and Safari's `audio/mp4` sniff as video/webm and
// video/mp4 (the sniffer reads the container, which does not say audio).
var transcribeKinds = map[string]string{
	"video/webm":      "recording.webm",
	"video/mp4":       "recording.mp4",
	"application/ogg": "recording.ogg",
	"audio/ogg":       "recording.ogg",
	"audio/wave":      "recording.wav",
}

// transcribeKindOf names the container head starts. Beyond the sniffer, any
// ISO-BMFF file (an `ftyp` box at offset 4) is MP4: Go only calls it video/mp4
// when a brand starts with "mp4", and a recorder may write only iso*/M4A
// brands — which would refuse a phone's whole recording as "not audio".
func transcribeKindOf(head []byte) string {
	if k := transcribeKinds[http.DetectContentType(head)]; k != "" {
		return k
	}
	if len(head) >= 8 && string(head[4:8]) == "ftyp" {
		return "recording.mp4"
	}
	return ""
}

func (s *Server) registerTranscribeRoutes(mux Mux) {
	mux.HandleFunc("POST "+ui.TranscribePath, s.requireOperatorWrite(s.handleTranscribe))
}

// dictatePath is what a message box's microphone posts to: ui.TranscribePath
// when this server has a transcriber, "" (no microphone drawn) when it has
// none. It is the ONE predicate every view reads, so a page and the route can
// never disagree about whether dictation exists.
func (s *Server) dictatePath() string {
	if s.ext.Transcriber == nil {
		return ""
	}
	return ui.TranscribePath
}

// readRecorder wraps the request body and keeps the first error a read met, so
// the handler can tell the size cap (http.MaxBytesError) from a client that
// went away — both arrive as the same failed upstream request.
type readRecorder struct {
	r   io.Reader
	n   int64
	err error
}

func (rr *readRecorder) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	rr.n += int64(n)
	if err != nil && err != io.EOF && rr.err == nil {
		rr.err = err
	}
	return n, err
}

// handleTranscribe is POST /transcribe: the body is the recording.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if s.ext.Transcriber == nil {
		// Feature off: as though the route did not exist. Registration stays
		// unconditional (see the package doc in server.go).
		http.NotFound(w, r)
		return
	}
	outcome, size, kind := 0, int64(0), ""
	defer func() {
		s.logger.Printf("transcribe: user=%d status=%d bytes=%d kind=%s", requestOwner(r.Context()), outcome, size, kind)
	}()
	say := func(status int, msg string) {
		outcome = status
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, msg)
	}

	// A declared length over the cap is refused before a byte is read.
	if r.ContentLength > maxTranscribeBytes {
		w.Header().Set("X-Muster-Max-Bytes", strconv.Itoa(maxTranscribeBytes))
		say(http.StatusRequestEntityTooLarge, TranscribeTooLarge)
		return
	}
	if !s.transcribe.perUser.take(strconv.FormatInt(requestOwner(r.Context()), 10), s.now(), transcribeRateWindow, transcribeRatePerUser) {
		w.Header().Set("Retry-After", strconv.Itoa(int(transcribeRateWindow/time.Second)))
		say(http.StatusTooManyRequests, TranscribeRateLimited)
		return
	}
	leave, ok := s.transcribe.enter()
	if !ok {
		w.Header().Set("Retry-After", "5")
		say(http.StatusServiceUnavailable, TranscribeBusy)
		return
	}
	defer leave()

	body := &readRecorder{r: http.MaxBytesReader(w, r.Body, maxTranscribeBytes)}
	br := bufio.NewReaderSize(body, 512)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		size = body.n
		s.writeTranscribeReadError(say, w, body.err)
		return
	}
	if len(head) == 0 {
		say(http.StatusBadRequest, TranscribeEmptyBody)
		return
	}
	kind = transcribeKindOf(head)
	if kind == "" {
		size = body.n
		say(http.StatusUnsupportedMediaType, TranscribeNotAudio)
		return
	}

	text, err := s.ext.Transcriber.Transcribe(r.Context(), br, kind)
	size = body.n
	if err != nil {
		if body.err != nil {
			// The upload failed, not the service: the cap, or a client gone.
			s.writeTranscribeReadError(say, w, body.err)
			return
		}
		switch {
		case errors.Is(err, stt.ErrUndecodable):
			say(http.StatusUnprocessableEntity, TranscribeUndecodable)
		case errors.Is(err, stt.ErrTooLong):
			say(http.StatusRequestEntityTooLarge, TranscribeTooLarge)
		case errors.Is(err, stt.ErrRefused):
			s.logger.Printf("transcribe: the speech-to-text service REFUSED this server's credential — " +
				"check MUSTER_STT_TOKEN against the service's token. Dictation answers 502 until they match.")
			say(http.StatusBadGateway, TranscribeRefused)
		default:
			s.logger.Printf("transcribe: the speech-to-text service did not answer: %v", err)
			say(http.StatusServiceUnavailable, TranscribeUnavailable)
		}
		return
	}
	if body.err != nil {
		// Belt and braces: the upload failed (the cap, or a client gone) yet the
		// service answered. Whatever it heard is a fragment; it is never handed
		// back as though the recording had arrived whole.
		s.writeTranscribeReadError(say, w, body.err)
		return
	}
	outcome = http.StatusOK
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, cleanTranscript(text))
}

// writeTranscribeReadError answers an upload that could not be read whole.
func (s *Server) writeTranscribeReadError(say func(int, string), w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		w.Header().Set("X-Muster-Max-Bytes", strconv.Itoa(maxTranscribeBytes))
		say(http.StatusRequestEntityTooLarge, TranscribeTooLarge)
		return
	}
	say(http.StatusBadRequest, TranscribeCutOff)
}

// maxTranscriptRunes bounds the text handed back. It is the order of the
// composer's own limit (a message is up to 16 KiB) and far above what two
// minutes of speech produce.
const maxTranscriptRunes = 8000

// cleanTranscript is text as the message box should receive it: trimmed,
// whitespace runs (newlines included) collapsed to one space, and every other
// control character dropped — the send routes refuse control characters, and a
// transcript that carried one would be text the operator could not send.
func cleanTranscript(text string) string {
	var b strings.Builder
	space, n := false, 0
	for _, r := range strings.TrimSpace(text) {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), r == unicode.ReplacementChar:
			continue
		}
		if n >= maxTranscriptRunes || (space && b.Len() > 0 && n+1 >= maxTranscriptRunes) {
			break
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return b.String()
}
