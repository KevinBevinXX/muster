package opencodehost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// THE CLIENT of one opencode v2 server (the shapes are upstream's
// packages/protocol/openapi.json at 6ec5682; see docs/design/opencode.md).
//
// 🔴 IT TALKS TO A LOOPBACK SERVER ONLY (CheckLocal): the credential it sends
// reaches the whole opencode API, so a URL from a file or a flag that names
// any other host is refused before a byte is sent.

// User is the HTTP Basic user every opencode credential goes with.
const User = "opencode"

// ErrUnauthorized: the server refused the credential (401). For a pair
// token: it expired, or the server's password was rotated (which revokes
// every token at once).
var ErrUnauthorized = errors.New("the opencode server refused the credential (401)")

// maxJSON bounds one JSON response body.
const maxJSON = 16 << 20

// maxEventLine bounds one line of the event stream.
const maxEventLine = 4 << 20

// CheckLocal refuses a server URL that is not http://<loopback>:<port>.
func CheckLocal(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("opencode server URL %q is not http://127.0.0.1:<port>", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("opencode server URL %q is not a loopback address; muster-mirror sends its opencode credential to this host's own server only", raw)
	}
	return nil
}

// Client reads one opencode server.
type Client struct {
	base   string
	secret func() string
	http   *http.Client
}

// NewClient is a client of the server at base (CheckLocal), authenticating
// with secret() — a pair token, or (Pair only) the server password — read at
// each request, so a renewed token is used from the next request on.
func NewClient(base string, secret func() string) (*Client, error) {
	if err := CheckLocal(base); err != nil {
		return nil, err
	}
	return &Client{base: strings.TrimSuffix(base, "/"), secret: secret,
		// No overall timeout: the event stream is long-lived. Every other
		// request carries its own context deadline.
		http: &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second},
			// A redirect would carry the credential somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// URL is the server's base URL.
func (c *Client) URL() string { return c.base }

// request is a request to path on c's server, authenticated unless anonymous.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, anonymous bool) (*http.Request, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if !anonymous {
		req.SetBasicAuth(User, c.secret())
	}
	return req, nil
}

// do sends req and decodes a 200's JSON body into out.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode %s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSON {
		return fmt.Errorf("opencode %s %s: the response is larger than %d bytes", req.Method, req.URL.Path, maxJSON)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("opencode %s %s: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// get is a GET of path into out: THE way this client reads.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, q, false)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

// Info is GET /api/info: the server's version (the one call that proves a
// credential without reading any session).
func (c *Client) Info(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/info", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// Session is one opencode session (Session.Info), as much of it as muster
// reads.
type Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID,omitempty"`
	Title    string `json:"title,omitempty"`
	Time     struct {
		Created float64 `json:"created"`
		Updated float64 `json:"updated"`
	} `json:"time"`
}

// maxSessionPages bounds a Sessions read (sessionPage sessions a page).
const (
	maxSessionPages = 20
	sessionPage     = 100
)

// Sessions is every ROOT session the server knows (GET /api/session,
// parentID=null), newest first, at most maxSessionPages pages.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var out []Session
	q := url.Values{"parentID": {"null"}, "limit": {strconv.Itoa(sessionPage)}, "order": {"desc"}}
	for page := 0; page < maxSessionPages; page++ {
		var resp struct {
			Data   []Session `json:"data"`
			Cursor struct {
				Next *string `json:"next"`
			} `json:"cursor"`
		}
		if err := c.get(ctx, "/api/session", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if len(resp.Data) < sessionPage || resp.Cursor.Next == nil || *resp.Cursor.Next == "" {
			break
		}
		// A cursor carries its own order; the two cannot be combined.
		q = url.Values{"parentID": {"null"}, "limit": {strconv.Itoa(sessionPage)}, "cursor": {*resp.Cursor.Next}}
	}
	return out, nil
}

// Running is the sessions running now (GET /api/session/active).
func (c *Client) Running(ctx context.Context) (map[string]bool, error) {
	var resp struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/session/active", nil, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(resp.Data))
	for id, a := range resp.Data {
		if a.Type == "running" {
			out[id] = true
		}
	}
	return out, nil
}

// Messages is session id's newest limit messages, OLDEST FIRST
// (GET /api/session/{id}/message, order=desc, reversed).
func (c *Client) Messages(ctx context.Context, id string, limit int) ([]Message, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%q is not an opencode session id", id)
	}
	var resp struct {
		Data []Message `json:"data"`
	}
	q := url.Values{"order": {"desc"}, "limit": {strconv.Itoa(limit)}}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(id)+"/message", q, &resp); err != nil {
		return nil, err
	}
	out := resp.Data
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ValidSessionID reports whether id is a session id this client asks about:
// "ses" and up to 125 more bytes of [0-9A-Za-z_-] (Session.ID's pattern is
// "^ses"; muster's wire bound is 128).
func ValidSessionID(id string) bool {
	if !strings.HasPrefix(id, "ses") || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Event is one frame of the event stream (GET /api/event).
type Event struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// SessionID is the session an event names (data.sessionID), "" for none.
func (e Event) SessionID() string {
	var d struct {
		SessionID string `json:"sessionID"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.SessionID
}

// Status is a session.status event's status type (idle, busy, retry), ""
// for any other event.
func (e Event) Status() string {
	if e.Type != "session.status" {
		return ""
	}
	var d struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.Status.Type
}

// Events reads the server's event stream until ctx ends or the stream does,
// calling fn for each frame in order; connected is called once the server
// has accepted the request (before any frame). It returns ErrUnauthorized for
// a refused credential, and otherwise the reason the stream ended.
func (c *Client) Events(ctx context.Context, connected func(), fn func(Event)) error {
	req, err := c.request(ctx, http.MethodGet, "/api/event", nil, false)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("opencode GET /api/event: HTTP %d", resp.StatusCode)
	}
	if connected != nil {
		connected()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxEventLine)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				var e Event
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &e) == nil && e.Type != "" {
					fn(e)
				}
				data = data[:0]
			}
		case strings.HasPrefix(line, ":"):
			// A comment: the server's heartbeat.
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.ErrUnexpectedEOF
}

// mint mints a new pair token with c's credential: POST /api/pair for a
// single-use code (the server holds it five minutes), then the
// UNAUTHENTICATED GET /auth/connect/{code} that redeems it — with
// Accept: application/json, which is what makes the server answer {token}
// rather than set a browser cookie. 🔴 THE ONLY NON-GET THIS PACKAGE SENDS,
// and only Pair and Credentials.Renew call it.
func (c *Client) mint(ctx context.Context) (string, error) {
	req, err := c.request(ctx, http.MethodPost, "/api/pair", nil, false)
	if err != nil {
		return "", err
	}
	var code struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := c.do(req, &code); err != nil {
		return "", err
	}
	if code.Code == "" || strings.ContainsAny(code.Code, "/?#") {
		return "", errors.New("opencode POST /api/pair returned no usable pairing code")
	}
	req, err = c.request(ctx, http.MethodGet, "/auth/connect/"+url.PathEscape(code.Code), nil, true)
	if err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := c.do(req, &tok); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return "", errors.New("opencode refused to redeem the pairing code it had just issued")
		}
		return "", err
	}
	if _, ok := Expiry(tok.Token); !ok {
		return "", errors.New("opencode's pairing answer holds no pair token")
	}
	return tok.Token, nil
}
