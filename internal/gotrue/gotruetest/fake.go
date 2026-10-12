// Package gotruetest is an in-memory GoTrue for muster's tests: the endpoints
// internal/gotrue calls, with the behaviours muster depends on modelled on the
// MEASURED behaviour of GoTrue v2.189.0 (see internal/gotrue's comments).
//
// ⚠ IT IS A FAKE, AND IT IS ONLY AS TRUE AS ITS MODEL. What it models:
// password and refresh grants with rotation (a spent token's direct parent
// replay is accepted, any older replay revokes the session), local and global
// logout, admin create (with email_confirm required to sign in and NO
// password-length check, as measured), ban, delete, generate_link, and a
// service_role check on /admin/*. The e2e suite runs the REAL image for the
// claim → login → refresh → logout path; this fake exists so every guard can be
// driven from `go test` with a controllable clock.
package gotruetest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// Issuer and Audience are what the fake stamps on every access token.
const (
	Issuer   = "muster-auth"
	Audience = "authenticated"
)

// User is a fake account.
type User struct {
	ID        string
	Email     string
	Password  string
	Confirmed bool
	Banned    bool
}

type refreshToken struct {
	session string
	parent  string
	child   string
	used    bool
}

type session struct {
	user    string
	revoked bool
}

// Fake is a running fake GoTrue.
type Fake struct {
	URL    string
	Secret []byte
	srv    *httptest.Server

	mu        sync.Mutex
	now       time.Time
	accessTTL time.Duration
	// createDelay holds every /admin/users call open this long, to widen a
	// race a concurrency test wants to observe.
	createDelay time.Duration
	users       map[string]*User
	sessions    map[string]*session
	refresh     map[string]*refreshToken
	calls       []Call
}

// Call is one request the fake served.
type Call struct {
	Method   string
	Path     string
	Query    string
	ClientIP string
	Status   int
}

// New starts a fake and closes it when t ends.
func New(t interface {
	Helper()
	Cleanup(func())
}, secret []byte) *Fake {
	t.Helper()
	f := &Fake{
		Secret:    secret,
		now:       time.Now(),
		accessTTL: 15 * time.Minute,
		users:     map[string]*User{},
		sessions:  map[string]*session{},
		refresh:   map[string]*refreshToken{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// Now is the fake's clock. Muster's verifier and client should share it.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// SetAccessTTL sets the lifetime of access tokens minted from now on.
func (f *Fake) SetAccessTTL(d time.Duration) {
	f.mu.Lock()
	f.accessTTL = d
	f.mu.Unlock()
}

// SetCreateDelay makes every admin create take d before it answers.
func (f *Fake) SetCreateDelay(d time.Duration) {
	f.mu.Lock()
	f.createDelay = d
	f.mu.Unlock()
}

// AddUser creates a confirmed user and returns its id.
func (f *Fake) AddUser(email, password string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := newUUID()
	f.users[id] = &User{ID: id, Email: email, Password: password, Confirmed: true}
	return id
}

// User returns a copy of a user, and whether it exists.
func (f *Fake) User(id string) (User, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return User{}, false
	}
	return *u, true
}

// UserCount is how many users exist.
func (f *Fake) UserCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

// Calls returns every request served so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CountCalls counts served requests to method+path (any status).
func (f *Fake) CountCalls(method, path string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

// SessionRevoked reports whether the session sid has been revoked.
func (f *Fake) SessionRevoked(sid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	return !ok || s.revoked
}

// Verifier returns a verifier matching what this fake signs, on its clock.
func (f *Fake) Verifier() gotrue.Verifier {
	return gotrue.Verifier{Secret: f.Secret, Issuer: Issuer, Audience: Audience, Now: f.Now}
}

// Client returns a client pointed at this fake, on its clock.
func (f *Fake) Client() *gotrue.Client {
	return &gotrue.Client{BaseURL: f.URL, Secret: f.Secret, Issuer: Issuer, Now: f.Now}
}

// Mint signs an access token for user id in a new session, as a password grant
// would, without going through HTTP. It returns (access, refresh, sessionID).
func (f *Fake) Mint(id string) (string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid := newUUID()
	f.sessions[sid] = &session{user: id}
	at, rt := f.issueLocked(sid, "")
	return at, rt, sid
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w, code: 200}
	f.route(sw, r)
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		ClientIP: r.Header.Get(gotrue.ClientIPHeader), Status: sw.code})
	f.mu.Unlock()
}

func (f *Fake) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/health":
		writeJSON(w, 200, map[string]string{"version": "fake"})
	case r.Method == http.MethodPost && p == "/token":
		f.token(w, r)
	case r.Method == http.MethodPost && p == "/logout":
		f.logout(w, r)
	case strings.HasPrefix(p, "/admin/"):
		if !f.isAdmin(r) {
			writeErr(w, 403, "not_admin", "User not allowed")
			return
		}
		f.adminRoute(w, r)
	default:
		writeErr(w, 404, "not_found", "no such route")
	}
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Query().Get("grant_type") {
	case "password":
		for _, u := range f.users {
			if strings.EqualFold(u.Email, body["email"]) && u.Password == body["password"] {
				if !u.Confirmed {
					writeErr(w, 400, "email_not_confirmed", "Email not confirmed")
					return
				}
				if u.Banned {
					writeErr(w, 400, "user_banned", "User is banned")
					return
				}
				sid := newUUID()
				f.sessions[sid] = &session{user: u.ID}
				at, rt := f.issueLocked(sid, "")
				f.writeSessionLocked(w, at, rt, u)
				return
			}
		}
		writeErr(w, 400, "invalid_credentials", "Invalid login credentials")
	case "refresh_token":
		rt, ok := f.refresh[body["refresh_token"]]
		if !ok {
			writeErr(w, 400, "refresh_token_not_found", "Invalid Refresh Token: Refresh Token Not Found")
			return
		}
		s := f.sessions[rt.session]
		if s == nil || s.revoked {
			writeErr(w, 400, "session_not_found", "Session not found")
			return
		}
		u := f.users[s.user]
		if u == nil || u.Banned {
			writeErr(w, 400, "user_banned", "User is banned")
			return
		}
		if rt.used {
			// The direct parent of the live token is accepted (measured);
			// anything older revokes the session (measured).
			child := f.refresh[rt.child]
			if child == nil || child.used {
				s.revoked = true
				writeErr(w, 400, "refresh_token_already_used", "Invalid Refresh Token: Already Used")
				return
			}
			// The live child is handed back again rather than minting a sibling.
			at, _ := f.issueLocked(rt.session, "")
			f.writeSessionLocked(w, at, rt.child, u)
			return
		}
		rt.used = true
		at, next := f.issueLocked(rt.session, body["refresh_token"])
		rt.child = next
		f.writeSessionLocked(w, at, next, u)
	default:
		writeErr(w, 400, "unsupported_grant_type", "unsupported grant")
	}
}

func (f *Fake) logout(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	v := gotrue.Verifier{Secret: f.Secret, Issuer: Issuer, Audience: Audience, Now: func() time.Time { return f.now }}
	c, err := v.Verify(tok)
	if err != nil {
		writeErr(w, 401, "bad_jwt", "invalid JWT")
		return
	}
	switch r.URL.Query().Get("scope") {
	case "global":
		for _, s := range f.sessions {
			if s.user == c.Subject {
				s.revoked = true
			}
		}
	default:
		if s := f.sessions[c.SessionID]; s != nil {
			s.revoked = true
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) isAdmin(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	// The service token carries no aud, so the audience half of Verifier does
	// not apply: check alg, signature, role and exp directly.
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	var h struct {
		Alg string `json:"alg"`
	}
	var c gotrue.Claims
	if json.Unmarshal(hb, &h) != nil || json.Unmarshal(pb, &c) != nil || h.Alg != gotrue.Algorithm {
		return false
	}
	m := hmac.New(sha256.New, f.Secret)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return false
	}
	return c.Role == "service_role" && c.Issuer == Issuer && f.Now().Before(time.Unix(c.ExpiresAt, 0))
}

func (f *Fake) adminRoute(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	delay := f.createDelay
	f.mu.Unlock()
	if r.Method == http.MethodPost && p == "/admin/users" && delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && p == "/admin/users":
		email, _ := body["email"].(string)
		pw, _ := body["password"].(string)
		for _, u := range f.users {
			if strings.EqualFold(u.Email, email) {
				writeErr(w, 422, "email_exists", "A user with this email address has already been registered")
				return
			}
		}
		confirm, _ := body["email_confirm"].(bool)
		id := newUUID()
		f.users[id] = &User{ID: id, Email: email, Password: pw, Confirmed: confirm}
		writeJSON(w, 200, map[string]any{"id": id, "email": email})
	case r.Method == http.MethodPost && p == "/admin/generate_link":
		writeJSON(w, 200, map[string]any{"action_link": "https://example.com/verify?type=" + toString(body["type"])})
	case strings.HasPrefix(p, "/admin/users/"):
		id := strings.TrimPrefix(p, "/admin/users/")
		u := f.users[id]
		if u == nil {
			writeErr(w, 404, "user_not_found", "User not found")
			return
		}
		switch r.Method {
		case http.MethodPut:
			if d, ok := body["ban_duration"].(string); ok {
				u.Banned = d != "none"
				if u.Banned {
					for _, s := range f.sessions {
						if s.user == id {
							s.revoked = true
						}
					}
				}
			}
			writeJSON(w, 200, map[string]any{"id": id})
		case http.MethodDelete:
			delete(f.users, id)
			writeJSON(w, 200, map[string]any{})
		default:
			writeErr(w, 405, "method", "method not allowed")
		}
	default:
		writeErr(w, 404, "not_found", "no such admin route")
	}
}

func (f *Fake) issueLocked(sid, _ string) (access, refresh string) {
	s := f.sessions[sid]
	u := f.users[s.user]
	email := ""
	if u != nil {
		email = u.Email
	}
	at, err := gotrue.Sign(f.Secret, gotrue.Claims{
		Subject: s.user, Issuer: Issuer, Audience: gotrue.Audience{Audience},
		IssuedAt: f.now.Unix(), ExpiresAt: f.now.Add(f.accessTTL).Unix(),
		Role: "authenticated", Email: email, SessionID: sid,
	})
	if err != nil {
		panic(err)
	}
	rt := randHex(16)
	f.refresh[rt] = &refreshToken{session: sid}
	return at, rt
}

func (f *Fake) writeSessionLocked(w http.ResponseWriter, at, rt string, u *User) {
	writeJSON(w, 200, map[string]any{
		"access_token": at, "token_type": "bearer", "expires_in": int(f.accessTTL / time.Second),
		"refresh_token": rt, "user": map[string]any{"id": u.ID, "email": u.Email},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ec, msg string) {
	writeJSON(w, code, map[string]any{"code": code, "error_code": ec, "msg": msg})
}

func toString(v any) string { s, _ := v.(string); return s }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newUUID() string {
	h := randHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
