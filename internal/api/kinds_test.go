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

// TestADispatchCanMakeAClaudeCodeAgentAPoolWorker: worker_pool is stored on a
// claude-code agent's row; on a gateway agent, or as a name the pattern
// refuses, it is a 409 before any row exists.
func TestADispatchCanMakeAClaudeCodeAgentAPoolWorker(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	if rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "worker_pool": {"reviewers"}}); rec.Code != http.StatusOK {
		t.Fatalf("claude-code worker: %d %s", rec.Code, rec.Body)
	}
	if rows := store.rows(); len(rows) != 1 || rows[0].WorkerPool != "reviewers" || rows[0].Kind != agents.KindClaudeCode {
		t.Fatalf("rows = %+v, want one claude-code row in pool reviewers", rows)
	}
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"worker_pool": {"reviewers"}}, `worker_pool "reviewers": only a claude-code agent can be a machine-job worker`},
		{url.Values{"kind": {"claude-code"}, "worker_pool": {"-bad"}}, `worker_pool "-bad" is not 1-63 of [a-z0-9-], not starting with '-'`},
	} {
		rec := postDispatch(t, s, h, tc.form)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%v = %d %q, want 409 carrying %q", tc.form, rec.Code, rec.Body, tc.want)
		}
	}
	if n := len(store.rows()); n != 1 {
		t.Fatalf("%d rows, want 1 (the refusals created none)", n)
	}
}

// TestAClaudeCodeDispatchKeepsItsRepository: a claude-code agent takes a
// repository and branch, as a gateway agent does — stored on the row, which is
// what agentspec turns into the pod's checkout. (Before repositories existed for
// this kind the same form was a 409.)
func TestAClaudeCodeDispatchKeepsItsRepository(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	connectRepos(t, s, "example-org/tide")
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"},
		"repo": {"example-org/tide"}, "repo_branch": {"release/3"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("claude-code + repo: %d %s", rec.Code, rec.Body)
	}
	rows := store.rows()
	if len(rows) != 1 || rows[0].Kind != agents.KindClaudeCode || rows[0].CCAccount != "work" ||
		rows[0].Repo != "example-org/tide" || rows[0].RepoBranch != "release/3" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestAClaudeCodeDispatchTakesOnlyAConnectedRepository: from the dispatch form
// or the API, a claude-code agent's repository must be one of the caller's
// connected GitHub repositories — the predicate task_dispatch uses
// (connectedRepo) — because its session starts pre-trusted in the checkout.
// Refused 409 before the row; a gateway agent's repository is not (its runtime
// image, not a pre-trusted Claude Code session, works in it).
func TestAClaudeCodeDispatchTakesOnlyAConnectedRepository(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	connectRepos(t, s, "example-org/tide")
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"stranger/x"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), chiefDispatchRepo) || len(store.rows()) != 0 {
		t.Fatalf("unconnected repo: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	s2, h2, store2 := kindsServer(t, stubKinds{pick: "work"})
	rec = postDispatch(t, s2, h2, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"example-org/tide"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), chiefDispatchNoGitHub) || len(store2.rows()) != 0 {
		t.Fatalf("no GitHub connection: %d %q rows=%d", rec.Code, rec.Body, len(store2.rows()))
	}
	if rec := postDispatch(t, s, h, url.Values{"note_text": {"t"}, "repo": {"stranger/x"}}); rec.Code != http.StatusOK {
		t.Fatalf("gateway + unconnected repo (unchanged): %d %s", rec.Code, rec.Body)
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
