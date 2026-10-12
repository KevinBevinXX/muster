package mirror_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/provision"
)

// asUser is a request principal for user id (the shape requireSession attaches).
func asUser(id int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: id, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// memStore is mirror.Store in memory, with mirror_tokens' RLS semantics played:
// a token is owned by the principal that minted it, and the lookups the Service
// makes under authz.System see every owner's.
type memStore struct {
	mu      sync.Mutex
	tokens  map[string]mirror.Token // by hash
	nextTok int64
	// liveErr, when set, is what TokenLive answers (a database outage);
	// liveErrN > 0 limits it to that many calls.
	liveErr   error
	liveErrN  int
	liveCalls int
	// sawSystem records whether every Authenticate/TokenLive ran under System.
	nonSystem []string
	// authCalls counts Authenticate calls (database reads, in PGStore).
	authCalls int
}

func newMemStore() *memStore { return &memStore{tokens: map[string]mirror.Token{}} }

func (m *memStore) mint(t *testing.T, owner int64, host string) (string, mirror.Token) {
	t.Helper()
	tok, rec, err := m.CreateToken(asUser(owner), host)
	if err != nil {
		t.Fatal(err)
	}
	return tok, rec
}

func (m *memStore) system(ctx context.Context, what string) {
	if p, ok := authz.From(ctx); !ok || !p.All() {
		m.nonSystem = append(m.nonSystem, what)
	}
}

func (m *memStore) Authenticate(ctx context.Context, token string) (mirror.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.system(ctx, "Authenticate")
	m.authCalls++
	t, ok := m.tokens[mirror.HashToken(token)]
	if !ok || t.RevokedAt != nil {
		return mirror.Token{}, mirror.ErrBadToken
	}
	return t, nil
}

func (m *memStore) TokenLive(ctx context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.system(ctx, "TokenLive")
	m.liveCalls++
	if m.liveErr != nil && (m.liveErrN == 0 || m.liveCalls <= m.liveErrN) {
		return false, m.liveErr
	}
	for _, t := range m.tokens {
		if t.ID == id {
			return t.RevokedAt == nil, nil
		}
	}
	return false, nil
}

func (m *memStore) CreateToken(ctx context.Context, host string) (string, mirror.Token, error) {
	if err := mirror.ValidHost(host); err != nil {
		return "", mirror.Token{}, err
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return "", mirror.Token{}, errors.New("memStore: no owner (RLS would refuse)")
	}
	tok, err := mirror.NewToken()
	if err != nil {
		return "", mirror.Token{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextTok++
	rec := mirror.Token{ID: m.nextTok, OwnerID: p.UserID, Host: host, CreatedAt: time.Now()}
	m.tokens[mirror.HashToken(tok)] = rec
	return tok, rec, nil
}

func (m *memStore) ListTokens(context.Context) ([]mirror.Token, error) { return nil, nil }

func (m *memStore) RevokeToken(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, t := range m.tokens {
		if t.ID == id && t.RevokedAt == nil {
			now := time.Now()
			t.RevokedAt = &now
			m.tokens[h] = t
			return true, nil
		}
	}
	return false, nil
}

// fakeBackend is a host's tmux, played: a settable session list and terminals
// that record every byte typed into them and every resize.
type fakeBackend struct {
	mu       sync.Mutex
	sessions []string
	terms    []*fakeTerm
}

func (b *fakeBackend) set(s ...string) {
	b.mu.Lock()
	b.sessions = s
	b.mu.Unlock()
}

func (b *fakeBackend) List(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sessions...), nil
}

func (b *fakeBackend) Attach(_ context.Context, session string, readOnly bool, cols, rows uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	t := &fakeTerm{session: session, readOnly: readOnly, r: pr, w: pw, sizes: []string{fmt.Sprintf("%dx%d", cols, rows)}}
	b.mu.Lock()
	b.terms = append(b.terms, t)
	b.mu.Unlock()
	go func() { _, _ = pw.Write([]byte("SCREEN " + session + "\r\n")) }()
	return t, nil
}

func (b *fakeBackend) attached() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.terms)
}

func (b *fakeBackend) term(t *testing.T, i int) *fakeTerm {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if len(b.terms) > i {
			ft := b.terms[i]
			b.mu.Unlock()
			return ft
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the backend never attached terminal #%d", i)
	return nil
}

type fakeTerm struct {
	session  string
	readOnly bool
	r        *io.PipeReader
	w        *io.PipeWriter

	mu     sync.Mutex
	typed  []byte
	sizes  []string
	closed bool
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.typed = append(f.typed, p...)
	f.mu.Unlock()
	return len(p), nil
}
func (f *fakeTerm) Resize(c, r uint16) error {
	f.mu.Lock()
	f.sizes = append(f.sizes, fmt.Sprintf("%dx%d", c, r))
	f.mu.Unlock()
	return nil
}
func (f *fakeTerm) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	_ = f.w.Close()
	return nil
}
func (f *fakeTerm) snapshot() (typed string, sizes []string, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.typed), append([]string(nil), f.sizes...), f.closed
}

// syncBuf is a goroutine-safe stdout for an attach.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rig is one muster (a real Service over memStore behind a real HTTP server).
type rig struct {
	svc   *mirror.Service
	store *memStore
	srv   *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store := newMemStore()
	svc := mirror.NewService(store, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirror.ConnectPath, svc.ServeConnect)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &rig{svc: svc, store: store, srv: srv}
}

// connect runs a REAL muster-mirror client (mirrorhost.Client) against the rig
// until the returned stop is called; done receives RunOnce's error.
func (r *rig) connect(t *testing.T, host, token string, b mirrorhost.Backend, interactive bool) (stop func(), done <-chan error) {
	t.Helper()
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: token, Host: host, Backend: b,
		Interactive: interactive, AllowHTTP: true, Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- c.RunOnce(ctx) }()
	t.Cleanup(cancel)
	return cancel, ch
}

// online polls until owner's host lists session.
func (r *rig) online(t *testing.T, owner int64, host, session string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%s/%s online for user %d", host, session, owner), func() bool {
		return r.svc.Hub().Online(owner, host, session)
	})
}

// eventually polls cond for up to 5s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// attach runs Service.Attach in the background as ctx's principal and returns
// the stdin writer, the screen, a resize channel and the attach's result.
func attach(t *testing.T, ctx context.Context, svc *mirror.Service, host, session string, readOnly bool) (stdin io.WriteCloser, screen *syncBuf, resize chan provision.TermSize, cancel func(), result <-chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	screen = &syncBuf{}
	resize = make(chan provision.TermSize, 4)
	resize <- provision.TermSize{Cols: 91, Rows: 37}
	ctx, c := context.WithCancel(ctx)
	ch := make(chan error, 1)
	go func() { ch <- svc.Attach(ctx, host, session, readOnly, pr, screen, resize) }()
	t.Cleanup(func() { c(); _ = pw.Close() })
	return pw, screen, resize, c, ch
}

// within waits for an attach result for up to 5s.
func within(t *testing.T, res <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// rawHost is a hand-driven host: a bare WebSocket speaking the wire, so a test
// can see exactly which frames muster sends — independent of muster-mirror's
// own layers.
type rawHost struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	data []string // binary frames received, "id:hex"
	text []mirror.Msg
}

func (r *rig) rawConnect(t *testing.T, token string, hello mirror.Msg) (*rawHost, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	var w mirror.Msg
	_ = json.Unmarshal(data, &w)
	if w.Error != "" {
		return nil, errors.New(w.Error)
	}
	h := &rawHost{ws: ws}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			h.mu.Lock()
			if typ == websocket.MessageBinary {
				id, p, _ := mirror.DecodeData(data)
				h.data = append(h.data, fmt.Sprintf("%d:%x", id, p))
			} else {
				var m mirror.Msg
				_ = json.Unmarshal(data, &m)
				h.text = append(h.text, m)
			}
			h.mu.Unlock()
		}
	}()
	return h, nil
}

func (h *rawHost) opened() (mirror.Msg, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.text {
		if m.Type == mirror.TypeOpen {
			return m, true
		}
	}
	return mirror.Msg{}, false
}

func (h *rawHost) frames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.data...)
}

func (h *rawHost) sendSessions(t *testing.T, sessions ...string) {
	t.Helper()
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: sessions})
	if err := h.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func hello(host string, interactive bool, sessions ...string) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeHello, Version: mirror.ProtocolVersion, Host: host, Sessions: sessions, Interactive: interactive}
}
