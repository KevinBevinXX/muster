package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agents"
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

// chatAgents adds the chat-session reads the chat socket makes to termAgents.
type chatAgents struct{ termAgents }

func (chatAgents) LatestOrCreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: 1, AgentID: agentID}, nil
}

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
		Agents:          chatAgents{},
		SessionLiveness: stubLiveness{},
		Provisioner:     newFakeTerminal(),
		Gateway:         stubGateway{},
		Mirror:          &fakeMirror{online: true},
	})
	f.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: f.store})
	f.s.gt.sweepInterval = 50 * time.Millisecond // before the first tracked request starts it
	h := f.s.Handler()
	f.edge, f.machine = BrowserListener(h), h
	ts := httptest.NewServer(f.edge)
	t.Cleanup(ts.Close)
	// User 1 is the one fakeMirror shows hosts to.
	sub := fake.AddUser("op@example.com", "correct horse battery")
	f.store.add(users.User{ID: 1, GotrueSub: sub, Email: "op@example.com"})
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
	return f.openWSWait(t, name, path, at, rt, true)
}

// openWSWait dials a socket; ready says whether to wait for the terminal's
// "ready" frame (the chat socket sends none).
func (f *streamFixture) openWSWait(t *testing.T, name, path, at, rt string, ready bool) opened {
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
	if ready {
		readUntil(t, c, "ready")
	}
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
		f.openWSWait(t, "agent chat", "/agents/keen-wren/ws", at, rt, false),
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

// TestADisableFromAnotherProcessEndsOpenStreams: the disable an operator
// runs is the CLI, in another process — it writes disabled_at and nothing
// else. The disable sweep must end that user's open SSE, terminals and chat
// within its interval, and a later re-enable (also row-only) lets them back.
func TestADisableFromAnotherProcessEndsOpenStreams(t *testing.T) {
	f := newStreamFixture(t)
	at, rt, _ := f.fake.Mint(f.sub)
	ss := f.openAll(t, at, rt)
	requireOpen(t, "before the disable", ss)
	if _, err := f.store.SetDisabled(context.Background(), 1, true); err != nil { // the row only, as the CLI does
		t.Fatal(err)
	}
	for _, s := range ss {
		if !s.ended(2 * time.Second) {
			t.Errorf("a row-only disable: the %s stream is still open after 2s (sweep every 50ms)", s.name)
		}
		s.close()
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("a request after the sweep saw the disable: %d, want 401", w.Code)
	}
	if _, err := f.store.SetDisabled(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a row-only re-enable was never noticed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAFailedDisableSweepCancelsNothing: a database error in the sweep is not
// "every user is gone". Treating it so would sign every open browser out on a
// DB blip.
func TestAFailedDisableSweepCancelsNothing(t *testing.T) {
	f := newStreamFixture(t)
	at, rt, _ := f.fake.Mint(f.sub)
	ss := f.openAll(t, at, rt)
	f.store.mu.Lock()
	f.store.lookupErr = errors.New("conn refused")
	f.store.mu.Unlock()
	requireOpen(t, "through failing sweeps (50ms interval, observed 300ms+)", ss)
	time.Sleep(300 * time.Millisecond)
	requireOpen(t, "through failing sweeps", ss)
	for _, s := range ss {
		s.close()
	}
}
