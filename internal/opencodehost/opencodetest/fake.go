// Package opencodetest is a FAKE opencode v2 server for muster's tests and
// browser suite: the routes muster-mirror reads (shaped from upstream's
// packages/protocol/openapi.json at 6ec5682), HTTP Basic auth with user
// "opencode", and pair tokens signed the way upstream signs them
// (packages/server/src/auth.ts), so a token it issues has the real shape and
// lifetime. Every request is recorded (Requests), which is how tests prove
// muster-mirror never calls anything else.
//
// Everything it serves is synthetic: tests put sessions and messages in.
package opencodetest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenTTL is upstream's SESSION_TTL_SECONDS: 30 days.
const TokenTTL = 30 * 24 * time.Hour

// Request is one request the server received.
type Request struct {
	Method string
	Path   string
	// Authorized: it carried a credential the server accepted.
	Authorized bool
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	password string
	now      func() time.Time
	codes    map[string]bool
	requests []Request
	sessions []json.RawMessage
	messages map[string][]json.RawMessage
	running  map[string]bool
	subs     map[chan string]bool
	seq      int
	// RefuseRedeem makes /auth/connect refuse every code.
	refuseRedeem bool
}

// New starts a fake opencode server whose password is password.
func New(password string) *Server {
	s := &Server{password: password, now: time.Now, codes: map[string]bool{}, messages: map[string][]json.RawMessage{},
		running: map[string]bool{}, subs: map[chan string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// SetNow sets the server's clock (token issue and expiry).
func (s *Server) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Rotate replaces the server password, which revokes every token it issued
// (upstream: "rotating the password revokes every session").
func (s *Server) Rotate(password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = password
}

// RefuseRedeem makes the code redemption fail (a 401), as an expired code does.
func (s *Server) RefuseRedeem(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseRedeem = on
}

// Issue is a token the server accepts, issued at the server's now.
func (s *Server) Issue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueLocked()
}

// IssueAt is a token expiring at exp (which may be in the past).
func (s *Server) IssueAt(exp time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + sign(s.password, e)
}

func (s *Server) issueLocked() string {
	e := strconv.FormatInt(s.now().Add(TokenTTL).Unix(), 10)
	return e + "." + sign(s.password, e)
}

func sign(password, payload string) string {
	k := hmac.New(sha256.New, []byte(password))
	k.Write([]byte("opencode-session-v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// authorized reports whether r carries the password or a valid token.
func (s *Server) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "opencode" {
		return false
	}
	if pass == s.password {
		return true
	}
	exp, sig, ok := strings.Cut(pass, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || !s.now().Before(time.Unix(n, 0)) {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(sign(s.password, exp)))
}

// Requests is every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// PutSession adds or replaces a session (by id): a synthetic Session.Info,
// {"id", "title", "parentID"?, "time": {"created", "updated"}}.
func (s *Server) PutSession(id, title, parent string) {
	m := map[string]any{"id": id, "title": title, "projectID": "prj_fixture", "cost": 0,
		"tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
		"time":   map[string]any{"created": 1700000000000, "updated": 1700000000000},
		"location": map[string]any{"directory": "/srv/fixture"}}
	if parent != "" {
		m["parentID"] = parent
	}
	b, _ := json.Marshal(m)
	s.mu.Lock()
	for i, raw := range s.sessions {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		if cur.ID == id {
			s.sessions[i] = b
			s.mu.Unlock()
			s.Emit(id, "session.renamed", map[string]any{"title": title})
			return
		}
	}
	s.sessions = append(s.sessions, b)
	s.mu.Unlock()
	s.Emit(id, "session.created", map[string]any{"title": title})
}

// PutMessage adds message m (a synthetic Session.Message.Info) to session
// sid, or replaces the one with the same "id" in place — as a streaming
// assistant message is rewritten — and announces it on the event stream.
func (s *Server) PutMessage(sid string, m map[string]any) {
	b, _ := json.Marshal(m)
	s.mu.Lock()
	replaced := false
	for i, raw := range s.messages[sid] {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		if cur.ID == m["id"] {
			s.messages[sid][i], replaced = b, true
		}
	}
	if !replaced {
		s.messages[sid] = append(s.messages[sid], b)
	}
	s.mu.Unlock()
	s.Emit(sid, "session.message.content.updated", map[string]any{"messageID": m["id"]})
}

// SetStatus sets session sid busy or idle, as session.status announces it.
func (s *Server) SetStatus(sid, status string) {
	s.mu.Lock()
	if status == "busy" {
		s.running[sid] = true
	} else {
		delete(s.running, sid)
	}
	s.mu.Unlock()
	s.Emit(sid, "session.status", map[string]any{"status": map[string]any{"type": status}})
}

// Emit sends an event of type typ for session sid (data gets sessionID) to
// every open event stream.
func (s *Server) Emit(sid, typ string, data map[string]any) {
	d := map[string]any{}
	for k, v := range data {
		d[k] = v
	}
	if sid != "" {
		d["sessionID"] = sid
	}
	s.mu.Lock()
	s.seq++
	frame, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_fixture%06d", s.seq), "created": s.now().UnixMilli(), "type": typ,
		"location": map[string]any{"directory": "/srv/fixture"}, "data": d})
	for ch := range s.subs {
		select {
		case ch <- string(frame):
		default:
		}
	}
	s.mu.Unlock()
}

// Subscribers is how many event streams are open.
func (s *Server) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ok := s.authorized(r)
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Authorized: ok})
	s.mu.Unlock()
	path := r.URL.Path
	if code, isConnect := strings.CutPrefix(path, "/auth/connect/"); isConnect && r.Method == http.MethodGet {
		s.connect(w, r, code)
		return
	}
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`))
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/api/info":
		writeJSON(w, map[string]any{"version": "2.0.0-fixture", "pid": 1, "urls": []string{s.URL}, "paths": map[string]any{}, "capabilities": map[string]any{}})
	case r.Method == http.MethodPost && path == "/api/pair":
		code := randomCode()
		s.mu.Lock()
		s.codes[code] = true
		s.mu.Unlock()
		writeJSON(w, map[string]any{"code": code, "expires_in": 300})
	case r.Method == http.MethodGet && path == "/api/session":
		s.listSessions(w, r)
	case r.Method == http.MethodGet && path == "/api/session/active":
		s.mu.Lock()
		data := map[string]any{}
		for id := range s.running {
			data[id] = map[string]any{"type": "running"}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": data})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/session/") && strings.HasSuffix(path, "/message"):
		s.listMessages(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/session/"), "/message"))
	case r.Method == http.MethodGet && path == "/api/event":
		s.events(w, r)
	default:
		http.Error(w, `{"_tag":"NotFound"}`, http.StatusNotFound)
	}
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request, code string) {
	s.mu.Lock()
	valid := s.codes[code] && !s.refuseRedeem
	delete(s.codes, code)
	var tok string
	if valid {
		tok = s.issueLocked()
	}
	s.mu.Unlock()
	if !valid {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError","message":"Pairing link expired or already used"}`))
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	writeJSON(w, map[string]any{"token": tok})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	var list []json.RawMessage
	for i := len(s.sessions) - 1; i >= 0; i-- { // newest first
		var cur struct {
			ParentID string `json:"parentID"`
		}
		_ = json.Unmarshal(s.sessions[i], &cur)
		if q.Get("parentID") == "null" && cur.ParentID != "" {
			continue
		}
		list = append(list, s.sessions[i])
	}
	s.mu.Unlock()
	start := 0
	if c := q.Get("cursor"); c != "" {
		start, _ = strconv.Atoi(c)
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	if start > len(list) {
		start = len(list)
	}
	end := min(start+limit, len(list))
	resp := map[string]any{"data": nonNil(list[start:end]), "cursor": map[string]any{}}
	if end < len(list) {
		resp["cursor"] = map[string]any{"next": strconv.Itoa(end)}
	}
	writeJSON(w, resp)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, sid string) {
	q := r.URL.Query()
	s.mu.Lock()
	all, ok := s.messages[sid]
	all = append([]json.RawMessage(nil), all...)
	s.mu.Unlock()
	if !ok {
		known := false
		s.mu.Lock()
		for _, raw := range s.sessions {
			var cur struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &cur)
			known = known || cur.ID == sid
		}
		s.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"SessionNotFoundError"}`))
			return
		}
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	out := make([]json.RawMessage, 0, limit)
	if q.Get("order") == "asc" {
		out = append(out, all[:min(limit, len(all))]...)
	} else {
		for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, all[i])
		}
	}
	writeJSON(w, map[string]any{"data": out, "cursor": map[string]any{}})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 256)
	s.mu.Lock()
	s.subs[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"evt_fixture000000\",\"type\":\"server.connected\",\"data\":{}}\n\n")
	fl.Flush()
	beat := time.NewTicker(200 * time.Millisecond)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		case <-beat.C:
			s.mu.Lock()
			ok := s.authorized(r)
			s.mu.Unlock()
			if !ok {
				return // a rotated password ends the stream
			}
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		}
	}
}

// Sessions is the ids of the sessions put, in order.
func (s *Server) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, raw := range s.sessions {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		out = append(out, cur.ID)
	}
	sort.Strings(out)
	return out
}

func nonNil(l []json.RawMessage) []json.RawMessage {
	if l == nil {
		return []json.RawMessage{}
	}
	return l
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
