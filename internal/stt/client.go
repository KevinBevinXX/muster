// Package stt is muster's client for an OpenAI-style speech-to-text service:
// `POST /v1/audio/transcriptions`, a multipart body whose `file` part is the
// recording, authenticated with a bearer token.
//
// 🔴 IT EXISTS SO THE TOKEN NEVER REACHES A BROWSER. The message box's microphone
// records in the browser and posts the audio to muster (api's transcribe
// route); muster adds the credential here and answers with the text. A page that
// called the service itself would have to be handed the token.
//
// 🔴 ITS FAILURES ARE NAMED ERRORS, NOT A STATUS CODE AND NOT THE SERVICE'S BODY.
// The caller must tell "the recording is unusable" from "our credential is
// wrong" from "the service is not answering", and say a different thing to a
// different reader for each. Returning the upstream status would push that
// mapping into the handler, and forwarding the upstream sentence would put words
// written for the token's holder in front of a browser.
package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The answers Transcribe gives besides a transcript.
//
//	400            ErrUndecodable  the service could not decode the audio
//	401, 403       ErrRefused      the bearer token is missing or wrong
//	413            ErrTooLong      longer than the service accepts
//	anything else  ErrUnavailable  not loaded, decoder died, a 5xx, a dial
//	                               failure, a timeout, an unreadable 200
//
// 🔴 AN UNMAPPED STATUS IS ErrUnavailable, AND THAT DEFAULT IS THE SAFE
// DIRECTION: "try again in a moment" is honest and harmless for something this
// package does not understand, while ErrUndecodable would tell a person their
// recording can never work.
var (
	ErrUndecodable = errors.New("stt: the service could not decode that audio")
	ErrRefused     = errors.New("stt: the service refused this server's token")
	ErrTooLong     = errors.New("stt: the audio is longer than the service accepts")
	ErrUnavailable = errors.New("stt: the service could not answer")
)

// TranscribePath is the service's endpoint.
const TranscribePath = "/v1/audio/transcriptions"

// DefaultTimeout bounds one transcription: the upload, the model's run over the
// audio, and the answer.
//
// It is not a network-latency figure. The service runs a model over the
// recording synchronously; a two-minute dictation on a warm GPU answers in a few
// seconds, and the same on a cold one (the first request after the model
// loads) has been measured at roughly a third of real time. A minute covers
// that with room, and is still short enough that a wedged service frees the
// caller's slot rather than holding it.
const DefaultTimeout = 60 * time.Second

// maxResponseBytes bounds the 200 body read. A transcript of the audio muster
// accepts is a few KiB; the bound is on the wire rather than on the service's
// good behaviour, which is the only place it can be enforced.
const maxResponseBytes = 1 << 20

// maxDrainBytes bounds the courtesy drain of a body before the connection is
// reused. A failure body is a short JSON `{"detail": …}`.
const maxDrainBytes = 64 << 10

// Client is a speech-to-text client. Its zero value is not usable; see New.
type Client struct {
	base   string
	token  string
	client *http.Client
}

// New builds a Client for the service at baseURL (http(s)://host[:port], no
// path) presenting token. timeout ≤ 0 means DefaultTimeout.
//
// 🔴 NO PROXY AND NO REDIRECTS: every request carries the credential. An
// environment proxy would be handed it, and a redirect would carry it to
// wherever the answer pointed, so a 3xx is just a status that is not 200.
func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("stt: the service URL must be http(s)://host[:port] with no path, " +
			"query, fragment or credentials")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("stt: a service token is required")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		base:  u.Scheme + "://" + u.Host,
		token: token,
		client: &http.Client{
			Timeout:       timeout,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Endpoint is the URL Transcribe posts to (for the boot banner; it carries no
// credential).
func (c *Client) Endpoint() string { return c.base + TranscribePath }

// Transcribe sends audio and returns what the service heard ("" when it heard
// nothing).
//
// 🔴 filename IS LOAD-BEARING. The service names its temporary file with the
// name's extension and hands that file to its decoder, so webm bytes under a
// `.wav` name are the one input that makes a good recording undecodable. The
// caller sniffs the bytes and chooses it; this package cannot.
//
// The audio is STREAMED into the request body (io.Pipe), not buffered: the
// caller's reader is read exactly once, as the request is sent. When the
// caller's reader fails, the request fails and Transcribe returns
// ErrUnavailable wrapping that failure — the caller, which owns the reader,
// is the one that can tell a size cap from a client that went away.
//
// language and punctuation are sent explicitly although the service defaults
// them: a change of default on that side would otherwise silently change what
// lands in the message box.
func (c *Client) Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeBody(mw, audio, filename))
	}()
	// 🔴 THE WRITER IS JOINED, NOT JUST UNBLOCKED. Closing the pipe's reader
	// makes a blocked write fail, and waiting for the goroutine guarantees it
	// has stopped READING THE CALLER'S AUDIO before Transcribe returns — the
	// caller's reader is a request body, which must not be read after its
	// handler has returned (and whose read counters the caller inspects).
	defer func() {
		pr.Close()
		<-done
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+TranscribePath, pr)
	if err != nil {
		return "", fmt.Errorf("stt: build the request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// "Bearer " and nothing else: the service compares what follows the first
	// space, so any other scheme spelling is a 401 that looks like a wrong token.
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		// Dial, TLS, timeout, cancellation, or the audio reader failing: none is a
		// fact about the recording the service judged.
		return "", fmt.Errorf("%w: %v", ErrUnavailable, redact(err, c.token))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return "", StatusError(resp.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: a 200 whose body is not a transcript: %v", ErrUnavailable, err)
	}
	return out.Text, nil
}

// writeBody writes the multipart body: the file part, then the two fields.
func writeBody(mw *multipart.Writer, audio io.Reader, filename string) error {
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	// 🔴 A FAILED READ RETURNS HERE, BEFORE THE CLOSING BOUNDARY IS WRITTEN, and
	// the caller closes the pipe WITH that error — so the transport aborts the
	// request mid-body. The service is left with an unterminated part it cannot
	// parse, never a well-formed upload of whatever arrived before the failure
	// (a recording cut at the size cap, transcribed as though it were whole).
	// api's TestAnOversizeStreamNeverReachesTheServiceWhole holds this.
	if _, err := io.Copy(part, audio); err != nil {
		return err
	}
	if err := mw.WriteField("language", "en"); err != nil {
		return err
	}
	if err := mw.WriteField("punctuation", "true"); err != nil {
		return err
	}
	return mw.Close()
}

// StatusError maps a non-200 status onto the named errors. It is a function so
// the mapping — the whole contract with the service — is table-tested without
// a server.
func StatusError(code int) error {
	switch code {
	case http.StatusBadRequest:
		return fmt.Errorf("%w (status 400)", ErrUndecodable)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (status %d)", ErrRefused, code)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w (status 413)", ErrTooLong)
	default:
		return fmt.Errorf("%w (status %d)", ErrUnavailable, code)
	}
}

// redact is err's text with the token cut out. net/http does not put request
// headers in its errors, so this is belt and braces: the one string that must
// never reach a log is removed wherever it might appear.
func redact(err error, token string) string {
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}
	return msg
}
