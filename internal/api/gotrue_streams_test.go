package api

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// Open streams outlive the request that opened them: an SSE stream and a
// terminal socket were each checked ONCE, at connect. These tests open each
// kind with a gotrue session, then disable the user or sign the session out,
// and require the stream to END — with a sibling stream of a DIFFERENT
// session as the control that a local sign-out must leave alone.

type streamFixture struct {
	*gtFixture
	ts  *httptest.Server
	sub string
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	fake := gotruetest.New(t, gtSecret)
	f := &gtFixture{fake: fake, store: newMemUsers()}
	f.s = New(sse.New(16), AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.com"},
		log.New(io.Discard, "", 0))
	f.s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          termAgents{},
		SessionLiveness: stubLiveness{},
		Provisioner:     newFakeTerminal(),
		Mirror:          &fakeMirror{online: true},
	})
	f.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: f.store})
	h := f.s.Handler()
	f.edge, f.machine = BrowserListener(h), h
	ts := httptest.NewServer(f.edge)
	t.Cleanup(ts.Close)
	// User 1 is the one fakeMirror shows hosts to.
	sub := fake.AddUser("op@example.com", "correct horse battery")
	f.store.add(users.User{ID: 1, GotrueSub: sub, Email: "op@example.com", Role: authz.RoleAdmin})
	return &streamFixture{gtFixture: f, ts: ts, sub: sub}
}

// opened is one live stream: ended() reports whether it has closed by deadline.
type opened struct {
	name  string
	ended func(time.Duration) bool
	close func()
}

func cookieHeader(at, rt string) string {
	return accessCookieName + "=" + at + "; " + refreshCookieName + "=" + rt
}

func (f *streamFixture) openSSE(t *testing.T, at, rt string) opened {
	t.Helper()
	req, _ := http.NewRequest("GET", f.ts.URL+"/events", nil)
	req.Header.Set("Cookie", cookieHeader(at, rt))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE: %d", resp.StatusCode)
	}
	t.Cleanup(func() { resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("SSE did not open: %q", line)
	}
	eof := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(eof)
	}()
	return opened{"sse", func(d time.Duration) bool {
		select {
		case <-eof:
			return true
		case <-time.After(d):
			return false
		}
	}, func() { resp.Body.Close() }}
}

func (f *streamFixture) openWS(t *testing.T, name, path, at, rt string) opened {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", cookieHeader(at, rt))
	h.Set("Origin", f.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("%s: dial: %v", name, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	readUntil(t, c, "ready")
	gone := make(chan struct{})
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				close(gone)
				return
			}
		}
	}()
	return opened{name, func(d time.Duration) bool {
		select {
		case <-gone:
			return true
		case <-time.After(d):
			return false
		}
	}, func() { c.CloseNow() }}
}

// openAll opens one stream of every kind on session (at, rt).
func (f *streamFixture) openAll(t *testing.T, at, rt string) []opened {
	return []opened{
		f.openSSE(t, at, rt),
		f.openWS(t, "agent terminal", "/agents/keen-wren/terminal/ws", at, rt),
		f.openWS(t, "mirror terminal", ui.MirrorTerminalWSPath("ws", "main"), at, rt),
	}
}

func requireEnded(t *testing.T, why string, ss []opened) {
	t.Helper()
	for _, s := range ss {
		if !s.ended(5 * time.Second) {
			t.Errorf("%s: the %s stream is still open", why, s.name)
		}
		s.close()
	}
}

func requireOpen(t *testing.T, why string, ss []opened) {
	t.Helper()
	for _, s := range ss {
		if s.ended(300 * time.Millisecond) {
			t.Errorf("%s: the %s stream ended, but it belongs to a session that was not signed out", why, s.name)
		}
	}
}

// TestEndingASessionEndsItsOpenStreams: disable ends every stream of the
// user; a local sign-out ends ONLY that session's streams; sign-out-everywhere
// ends all of them.
func TestEndingASessionEndsItsOpenStreams(t *testing.T) {
	t.Run("disable", func(t *testing.T) {
		f := newStreamFixture(t)
		a1, r1, _ := f.fake.Mint(f.sub)
		a2, r2, _ := f.fake.Mint(f.sub)
		ss := append(f.openAll(t, a1, r1), f.openAll(t, a2, r2)...)
		requireOpen(t, "before the disable", ss)
		if err := f.s.DisableUser(context.Background(), 1, true); err != nil {
			t.Fatal(err)
		}
		requireEnded(t, "after DisableUser", ss)
	})
	t.Run("local sign-out", func(t *testing.T) {
		f := newStreamFixture(t)
		a1, r1, _ := f.fake.Mint(f.sub)
		a2, r2, _ := f.fake.Mint(f.sub)
		mine, other := f.openAll(t, a1, r1), f.openAll(t, a2, r2)
		if w := do(f.edge, "POST", "/logout", nil, withCookies(a1, r1)); w.Code != http.StatusSeeOther {
			t.Fatalf("logout: %d", w.Code)
		}
		requireEnded(t, "after this session signed out", mine)
		requireOpen(t, "after ANOTHER session signed out", other)
		for _, s := range other {
			s.close()
		}
	})
	t.Run("sign out everywhere", func(t *testing.T) {
		f := newStreamFixture(t)
		a1, r1, _ := f.fake.Mint(f.sub)
		a2, r2, _ := f.fake.Mint(f.sub)
		ss := append(f.openAll(t, a1, r1), f.openAll(t, a2, r2)...)
		if w := do(f.edge, "POST", "/logout", map[string][]string{"scope": {"global"}}, withCookies(a1, r1)); w.Code != http.StatusSeeOther {
			t.Fatalf("logout: %d", w.Code)
		}
		requireEnded(t, "after sign-out-everywhere", ss)
	})
}
