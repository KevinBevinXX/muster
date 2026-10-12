package agentgateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// recordingMarker is the account pool as the gateway sees it.
type recordingMarker struct {
	calls []string
	err   error
}

func (m *recordingMarker) MarkFailure(_ context.Context, owner int64, account, failure, detail string) error {
	m.calls = append(m.calls, strconv.FormatInt(owner, 10)+"|"+account+"|"+failure+"|"+detail)
	return m.err
}

// ccdFailing answers every /v1/responses with ccd's typed failure body.
func ccdFailing(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func ccAgent() agents.Agent {
	a := fixtureAgent()
	a.Kind = agents.KindClaudeCode
	a.CCAccount = "work"
	// A member's agent: the mark must name THIS owner (an account is
	// (owner, name)), and 7 is a value no default could produce.
	a.OwnerID = 7
	return a
}

// ccd's own wire shape (cmd/ccd/failure.go). The messages are what the marks
// must carry.
const (
	ccdRateLimited = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"You've hit your limit · resets 5pm"}}`
	ccdAuthFailed  = `{"error":{"type":"auth_failed","code":"authentication_failed","upstream_status":401,"message":"OAuth access token is invalid"}}`
	ccdBusy        = `{"error":{"type":"busy","code":"busy","upstream_status":0,"message":"a turn is in progress"}}`
)

func markGateway(t *testing.T, srv *httptest.Server, m AccountMarker) *Gateway {
	t.Helper()
	gw, err := New(Config{
		Driver:   &fixedResolver{ep: endpointOf(t, srv.URL)},
		Runtime:  HooksSHA256(),
		Model:    testSentinel,
		Client:   srv.Client(),
		Accounts: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

// TestATypedCcdFailureMarksTheClaudeAccount: ccd's `rate_limited` (429) and
// `auth_failed` (502) mark the agent's account with ccd's own message, on both
// turn shapes (Chat, and ChatWithTools — muster's chat route tries tools first).
// Everything else — another failure type, an untyped body, a gateway-kind agent —
// marks nothing.
func TestATypedCcdFailureMarksTheClaudeAccount(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		agent  agents.Agent
		want   string
	}{
		{"rate limited", 429, ccdRateLimited, ccAgent(), "7|work|rate_limited|You've hit your limit · resets 5pm"},
		{"auth failed", 502, ccdAuthFailed, ccAgent(), "7|work|auth_failed|OAuth access token is invalid"},
		{"busy is not an account fault", 409, ccdBusy, ccAgent(), ""},
		{"an untyped 429 (a proxy) is not ccd", 429, `slow down`, ccAgent(), ""},
		{"a gateway-kind agent never marks", 429, ccdRateLimited, fixtureAgent(), ""},
	}
	for _, c := range cases {
		for _, shape := range []string{"chat", "tools"} {
			t.Run(c.name+"/"+shape, func(t *testing.T) {
				srv := ccdFailing(c.status, c.body)
				defer srv.Close()
				m := &recordingMarker{}
				gw := markGateway(t, srv, m)
				var err error
				if shape == "chat" {
					_, err = gw.Chat(context.Background(), c.agent, "k", "hello", nil)
				} else {
					_, err = gw.ChatWithTools(context.Background(), c.agent, "k", "", "hello", nil, nil, nil)
				}
				if err == nil {
					t.Fatal("a failing turn returned no error")
				}
				got := strings.Join(m.calls, "\n")
				if got != c.want {
					t.Fatalf("marks = %q, want %q", got, c.want)
				}
				var rt *agents.RuntimeError
				if !errors.As(err, &rt) || rt.Status != c.status {
					t.Fatalf("the caller lost the runtime error: %v", err)
				}
			})
		}
	}
}

func TestAFailedMarkIsReportedWithoutReplacingTheTurnsError(t *testing.T) {
	srv := ccdFailing(429, ccdRateLimited)
	defer srv.Close()
	gw := markGateway(t, srv, &recordingMarker{err: errors.New("db down")})
	_, err := gw.Chat(context.Background(), ccAgent(), "k", "hello", nil)
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) || rt.Type != "rate_limited" || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v", err)
	}
}

// TestAClaudeCodeTurnGetsItsOwnBudgetAndAGatewayTurnKeepsTheConfiguredClient.
func TestAClaudeCodeTurnGetsItsOwnBudgetAndAGatewayTurnKeepsTheConfiguredClient(t *testing.T) {
	base := &http.Client{Timeout: 7 * time.Minute}
	gw, err := New(Config{Driver: &fixedResolver{}, Runtime: HooksSHA256(), Model: testSentinel, Client: base})
	if err != nil {
		t.Fatal(err)
	}
	if c := gw.clientFor(agents.KindGateway); c != base {
		t.Fatal("the gateway kind must use the configured client itself")
	}
	if c := gw.clientFor(""); c != base {
		t.Fatal("an unset kind must use the configured client itself")
	}
	cc := gw.clientFor(agents.KindClaudeCode)
	if cc == base || cc.Timeout != 35*time.Minute {
		t.Fatalf("claude-code client timeout = %s, want a copy with 35m", cc.Timeout)
	}
	if base.Timeout != 7*time.Minute {
		t.Fatal("the copy mutated the configured client")
	}
	if cc.Transport != base.Transport {
		t.Fatal("the copy does not share the configured transport")
	}
}
