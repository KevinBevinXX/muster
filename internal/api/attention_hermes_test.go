package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A WAITING HERMES WINDOW IN THE ATTENTION QUEUE (Hermes Phase 4).

const askHermesSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// hermesAskMirror is fakeMirror's host "ws" listing Hermes windows in session
// "main", whose host answers approvals when answers is set; it records each
// AnswerHermesApproval and answers err.
type hermesAskMirror struct {
	*fakeMirror
	answers bool
	mu      sync.Mutex
	calls   []string
	err     error
}

func (m *hermesAskMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.fakeMirror.Hosts(ctx)
	for i := range hs {
		hs[i].HermesAnswer = m.answers && hs[i].Online && hs[i].Interactive
	}
	return hs
}

func (m *hermesAskMirror) AnswerHermesApproval(ctx context.Context, host, session, window, promptID, choice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, strings.Join([]string{host, session, window, promptID, choice}, " "))
	return m.err
}

func (m *hermesAskMirror) answered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// askHermesWindows: @1 waits on an approval muster can answer, @2 on sudo, @3 on
// a clarify, @4 is a Hermes window of an older host (no ask), @5 is busy.
func askHermesWindows() []mirror.Window {
	w := func(id string, status string, p *mirror.Pending) mirror.Window {
		return mirror.Window{Session: "main", ID: id, Runtime: mirror.RuntimeHermes, AgentSession: askHermesSID, Status: status, StatusSince: 1792000001111, Pending: p}
	}
	return []mirror.Window{
		w("@1", mirror.StatusWaiting, &mirror.Pending{Kind: "approval", Summary: "terminal: rm -r ./build", Tool: "terminal", Detail: "recursive delete", PromptID: "p1", Choices: "once deny"}),
		w("@2", mirror.StatusWaiting, &mirror.Pending{Kind: "sudo", Summary: "Hermes is asking for a sudo password in the terminal", Tool: "sudo"}),
		w("@3", mirror.StatusWaiting, &mirror.Pending{Kind: "clarify", Summary: "Hermes is asking a question in the terminal", Tool: "clarify"}),
		w("@4", mirror.StatusWaiting, nil),
		w("@5", mirror.StatusBusy, nil),
	}
}

func hermesUser() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleMember, Via: authz.ViaSession})
}

func itemsByWindow(items []attentionItem) map[string]attentionItem {
	out := map[string]attentionItem{}
	for _, it := range items {
		out[it.card.ref.Window] = it
	}
	return out
}

// Every waiting Hermes window is an item saying what it waits on; only the
// approval of a host that answers approvals offers Allow once / Deny.
func TestWaitingHermesWindowsAreAttentionItems(t *testing.T) {
	for _, answers := range []bool{true, false} {
		m := &hermesAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: askHermesWindows()}, answers: answers}
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Mirror: m})
		by := itemsByWindow(s.waitingSessions(hermesUser()))
		if len(by) != 4 {
			t.Fatalf("answers=%v: %d items, want the four waiting windows: %+v", answers, len(by), by)
		}
		a := by["@1"]
		if a.Text != "terminal: rm -r ./build" || a.Detail != "recursive delete" || a.tool != "terminal" || a.hermes == nil || a.hermes.kind != "approval" {
			t.Fatalf("the approval: %+v", a)
		}
		if a.subject.Key != "hermes:"+askHermesSID || a.claudeSession != "hermes:"+askHermesSID {
			t.Errorf("the approval's focus key: %+v %q", a.subject, a.claudeSession)
		}
		html := attentionItemsHTML([]attentionItem{a}, time.Now())
		if got := strings.Contains(html, `data-hermes-choice="once"`) && strings.Contains(html, `data-hermes-choice="deny"`); got != answers {
			t.Errorf("answers=%v: the buttons are drawn = %v:\n%s", answers, got, html)
		}
		if !answers && !strings.Contains(html, "Answer it in the terminal.") {
			t.Errorf("no buttons, and no word of where to answer:\n%s", html)
		}
		for _, never := range []string{`value="session"`, `value="always"`} {
			if strings.Contains(html, never) {
				t.Errorf("offered %s", never)
			}
		}
		if html := attentionItemsHTML([]attentionItem{by["@2"]}, time.Now()); !strings.Contains(html, "muster never answers a password prompt") || strings.Contains(html, "<form") {
			t.Errorf("sudo:\n%s", html)
		}
		if html := attentionItemsHTML([]attentionItem{by["@3"]}, time.Now()); !strings.Contains(html, "Answer it in the terminal.") || strings.Contains(html, "data-hermes-approval") {
			t.Errorf("clarify:\n%s", html)
		}
		if old := by["@4"]; old.Text != hermesWaitingText {
			t.Errorf("an older host's waiting Hermes window: %+v", old)
		}
	}
}

// hermesServer is the real mux with the attention routes and m.
func hermesServer(t *testing.T, m Mirror) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
		Mirror: m, Attention: emptyAttention{}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// emptyAttention is an attention store with nothing in it.
type emptyAttention struct{ attention.Store }

func (emptyAttention) Open(context.Context) ([]attention.Ask, error) { return nil, nil }
func (emptyAttention) Answered(context.Context, time.Time, int) ([]attention.Ask, error) {
	return nil, nil
}

func postApprove(t *testing.T, ts *httptest.Server, cookie, origin string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+attentionHermesPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", origin)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// 🔴 The answer passes the gates of the Claude ask-answer path, each before the plugin is asked;
// a good one reaches it and comes back to the queue with the stale-prompt
// words.
func TestAHermesApprovalAnswerIsGated(t *testing.T) {
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "prompt": {"p1"}, "choice": {"deny"}}
	for _, c := range []struct {
		name   string
		stale  bool
		origin string
		window string
		status int
		asked  bool
	}{
		{"another origin", false, "https://evil.example", "@1", http.StatusForbidden, false},
		{"a stale sign-in", true, "", "@1", http.StatusForbidden, false},
		{"a window that is not listed", false, "", "@9", http.StatusNotFound, false},
		{"fresh, same origin", false, "", "@1", http.StatusSeeOther, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &hermesAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: askHermesWindows()}, answers: true}
			s, ts := hermesServer(t, m)
			cookie := sessionCookieFor(s)
			if c.stale {
				cookie = staleCookieFor(s)
			}
			origin := ts.URL
			if c.origin != "" {
				origin = c.origin
			}
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			f.Set("window", c.window)
			resp, body := postApprove(t, ts, cookie, origin, f)
			if resp.StatusCode != c.status || (len(m.answered()) == 1) != c.asked {
				t.Fatalf("%d %q, asked %v", resp.StatusCode, body, m.answered())
			}
			if c.asked {
				if got := m.answered()[0]; got != "ws main @1 p1 deny" {
					t.Fatalf("asked %q", got)
				}
				if loc := resp.Header.Get("Location"); loc != attentionPath+"?answered=hermes" {
					t.Fatalf("redirect %q", loc)
				}
				_, page := mirrorGet(t, s, ts, attentionPath+"?answered=hermes")
				if !strings.Contains(page, hermesAnsweredText) {
					t.Fatalf("the page does not say the terminal's prompt is stale:\n%s", page)
				}
			}
		})
	}
}

// The plugin's refusals are the page's words, never "answered".
func TestAHermesApprovalAnswersRefusalsAreSaid(t *testing.T) {
	for err, want := range map[error]string{
		mirror.ErrHermesApprovalGone:      hermesGoneText,
		mirror.ErrHermesAnswerUnsupported: hermesNoAnswerText,
		mirror.ErrHermesUnreachable:       hermesUnreachText,
		mirror.ErrHermesAnswerUnknown:     hermesUnknownText,
		mirror.ErrStepUpRequired:          "",
	} {
		m := &hermesAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: askHermesWindows()}, answers: true,
			err: errors.Join(mirror.ErrAskNotSent, err)}
		s, ts := hermesServer(t, m)
		resp, body := postApprove(t, ts, sessionCookieFor(s), ts.URL, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "prompt": {"p1"}, "choice": {"once"}})
		if resp.StatusCode < 400 || strings.Contains(body, hermesAnsweredText) {
			t.Errorf("%v: %d", err, resp.StatusCode)
		}
		if want != "" && !strings.Contains(body, strings.ReplaceAll(want, "'", "&#39;")) {
			t.Errorf("%v: the page does not say %q", err, want)
		}
	}
}

// 🟢 A Hermes item's text and detail are drawn left to right, isolated from
// the page's bidi context (audit #212).
func TestAHermesItemIsDrawnLeftToRightIsolated(t *testing.T) {
	m := &hermesAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: askHermesWindows()}, answers: true}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Mirror: m})
	html := attentionItemsHTML([]attentionItem{itemsByWindow(s.waitingSessions(hermesUser()))["@1"]}, time.Now())
	for _, want := range []string{`data-attention-text><bdi dir="ltr">terminal: rm -r ./build</bdi>`, `data-attention-detail><bdi dir="ltr">recursive delete</bdi>`} {
		if !strings.Contains(html, want) {
			t.Errorf("not drawn left to right, isolated (%s):\n%s", want, html)
		}
	}
}
