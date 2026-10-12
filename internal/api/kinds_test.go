package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
)

// stubKinds is AgentKinds for tests: both kinds enabled, a fixed account list,
// and a recorded selection.
type stubKinds struct {
	enabled     []string
	accounts    []string
	pick        string
	err         error
	pins        *[]string
	unavailable map[string]string
	owners      *[]int64
}

func (k stubKinds) Enabled() []string {
	if k.enabled == nil {
		return []string{agents.KindGateway, agents.KindClaudeCode}
	}
	return k.enabled
}
func (k stubKinds) ClaudeAccounts(context.Context, int64) ([]string, error) { return k.accounts, nil }
func (k stubKinds) Unavailable(context.Context, int64) map[string]string    { return k.unavailable }
func (k stubKinds) SelectClaudeAccount(_ context.Context, owner int64, pin string) (string, error) {
	if k.owners != nil {
		*k.owners = append(*k.owners, owner)
	}
	if k.pins != nil {
		*k.pins = append(*k.pins, pin)
	}
	if k.err != nil {
		return "", k.err
	}
	if pin != "" {
		return pin, nil
	}
	return k.pick, nil
}

func kindsServer(t *testing.T, kinds AgentKinds) (*Server, http.Handler, *preflightStore) {
	t.Helper()
	store := &preflightStore{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Agents:          store,
		SessionLiveness: stubLiveness{},
		Provisioner:     newPreflightProvisioner(""),
		Kinds:           kinds,
	})
	return s, s.Handler(), store
}

func postDispatch(t *testing.T, s *Server, h http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("action", "save")
	req := httptest.NewRequest(http.MethodPost, "/agents", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAClaudeCodeDispatchStoresTheKindAndThePoolsAccount: the account is chosen
// BEFORE the row and stored on it; a pin reaches the pool verbatim.
func TestAClaudeCodeDispatchStoresTheKindAndThePoolsAccount(t *testing.T) {
	var pins []string
	s, h, store := kindsServer(t, stubKinds{pick: "work", pins: &pins})

	if rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}}); rec.Code != http.StatusOK {
		t.Fatalf("auto: %d %s", rec.Code, rec.Body)
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "cc_account": {"personal"}}); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	if rec := postDispatch(t, s, h, url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("no kind: %d %s", rec.Code, rec.Body)
	}
	rows := store.rows()
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	got := []string{rows[0].Kind + "/" + rows[0].CCAccount, rows[1].Kind + "/" + rows[1].CCAccount, rows[2].Kind + "/" + rows[2].CCAccount}
	want := []string{"claude-code/work", "claude-code/personal", "gateway/"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if strings.Join(pins, ",") != ",personal" {
		t.Fatalf("pool asked with pins %q, want auto then personal (and never for the gateway dispatch)", pins)
	}
}

// TestADispatchTheDeploymentCannotSatisfyIsRefusedBeforeTheRow: every refusal is
// a 409 with its reason, and NO row is created.
func TestADispatchTheDeploymentCannotSatisfyIsRefusedBeforeTheRow(t *testing.T) {
	cases := []struct {
		name  string
		kinds AgentKinds
		form  url.Values
		want  string
	}{
		{"kind not enabled", stubKinds{enabled: []string{agents.KindGateway}}, url.Values{"kind": {"claude-code"}}, "not enabled"},
		{"no kinds wired", nil, url.Values{"kind": {"claude-code"}}, "not enabled"},
		{"unknown kind", stubKinds{}, url.Values{"kind": {"teleport"}}, "unknown agent kind"},
		{"pin on a gateway agent", stubKinds{}, url.Values{"cc_account": {"work"}}, "only be pinned"},
		{"model on a claude-code agent", stubKinds{pick: "work"}, url.Values{"kind": {"claude-code"}, "model": {"openrouter/x/y"}}, "no per-agent model"},
		{"repo on a claude-code agent", stubKinds{pick: "work"}, url.Values{"kind": {"claude-code"}, "repo": {"example-org/tide"}}, "cannot be given a repository"},
		{"pool refuses", stubKinds{err: errors.New("ccpool: no usable Claude account: every account's current token failed")}, url.Values{"kind": {"claude-code"}}, "no usable Claude account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h, store := kindsServer(t, c.kinds)
			rec := postDispatch(t, s, h, c.form)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("got %d %q, want 409 containing %q", rec.Code, rec.Body.String(), c.want)
			}
			if n := len(store.rows()); n != 0 {
				t.Fatalf("a refused dispatch created %d row(s)", n)
			}
		})
	}
}

// TestTheDispatchFormOffersThePickerOnlyWithTwoKinds.
func TestTheDispatchFormOffersThePickerOnlyWithTwoKinds(t *testing.T) {
	for _, c := range []struct {
		kinds AgentKinds
		want  bool
	}{
		{nil, false},
		{stubKinds{enabled: []string{agents.KindGateway}}, false},
		{stubKinds{accounts: []string{"work", "personal"}}, true},
	} {
		s, h, _ := kindsServer(t, c.kinds)
		req := httptest.NewRequest(http.MethodGet, "/ui/agents/new", nil)
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%d", rec.Code)
		}
		if got := strings.Contains(body, "data-kind-picker"); got != c.want {
			t.Fatalf("kinds %v: picker present = %v, want %v", c.kinds, got, c.want)
		}
		if c.want && (!strings.Contains(body, `<option value="personal">personal</option>`) || strings.Contains(body, "sk-ant")) {
			t.Fatalf("account options missing (or a token present):\n%s", body)
		}
	}
}

// modelStore answers the model route's reads for one agent and records SetModel.
type modelStore struct {
	agents.Store
	agent  agents.Agent
	sets   int
	getErr error
}

func (m *modelStore) Get(context.Context, int64) (agents.Agent, error) { return m.agent, m.getErr }
func (m *modelStore) SetModel(_ context.Context, _ int64, model string) (agents.Agent, error) {
	if errors.Is(m.getErr, pgx.ErrNoRows) {
		return agents.Agent{}, pgx.ErrNoRows
	}
	m.sets++
	m.agent.Model = model
	return m.agent, nil
}

// TestAModelCannotBeSetOnAClaudeCodeAgentLater: the second model write refuses
// what the first (dispatch) refuses, and stores nothing. The gateway agent is
// the control.
func TestAModelCannotBeSetOnAClaudeCodeAgentLater(t *testing.T) {
	for _, c := range []struct {
		kind string
		want int
	}{{agents.KindClaudeCode, http.StatusConflict}, {agents.KindGateway, http.StatusOK}} {
		st := &modelStore{agent: agents.Agent{ID: 12, Name: "quiet-heron", Kind: c.kind, Status: agents.StatusStopped}}
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
		s.UseExtensions(Extensions{Agents: st, SessionLiveness: stubLiveness{}, Provisioner: newPreflightProvisioner("")})
		form := url.Values{"model": {""}}
		req := httptest.NewRequest(http.MethodPost, "/agents/12/model", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		admit(s, req)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("kind %s: %d %q, want %d", c.kind, rec.Code, rec.Body.String(), c.want)
		}
		if wantSets := map[bool]int{true: 0, false: 1}[c.kind == agents.KindClaudeCode]; st.sets != wantSets {
			t.Fatalf("kind %s: SetModel called %d time(s), want %d", c.kind, st.sets, wantSets)
		}
	}
}

// TestTheModelRouteFailsClosedOnAKindReadError: a read error other than "no such
// agent" refuses the write (500, nothing stored) instead of storing a model on
// an agent whose kind could not be checked; a missing agent is still a 404.
func TestTheModelRouteFailsClosedOnAKindReadError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{{errors.New("conn reset"), http.StatusInternalServerError}, {pgx.ErrNoRows, http.StatusNotFound}} {
		st := &modelStore{agent: agents.Agent{ID: 12, Name: "quiet-heron"}, getErr: c.err}
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
		s.UseExtensions(Extensions{Agents: st, SessionLiveness: stubLiveness{}, Provisioner: newPreflightProvisioner("")})
		req := httptest.NewRequest(http.MethodPost, "/agents/12/model", strings.NewReader("model="))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		admit(s, req)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != c.want || st.sets != 0 {
			t.Fatalf("Get error %v: %d (sets %d), want %d and nothing stored", c.err, rec.Code, st.sets, c.want)
		}
	}
}
