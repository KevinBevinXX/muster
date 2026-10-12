package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/credentials"
)

// memCreds is CredentialStore in memory, keyed by the principal's user.
type memCreds struct {
	claude map[int64]map[string]string
	envFor map[int64]bool
}

func (m *memCreds) uid(ctx context.Context) int64 { return authz.MustFrom(ctx).UserID }
func (m *memCreds) List(ctx context.Context) ([]credentials.Summary, error) {
	var out []credentials.Summary
	for name := range m.claude[m.uid(ctx)] {
		out = append(out, credentials.Summary{Kind: credentials.KindClaude, Name: name, Fingerprint: "fp0123456789"})
	}
	return out, nil
}
func (m *memCreds) PutClaude(ctx context.Context, name, tok string) error {
	if err := credentials.ValidateAccountName(name); err != nil {
		return err
	}
	if err := credentials.ValidateClaudeToken(tok); err != nil {
		return err
	}
	if m.claude[m.uid(ctx)] == nil {
		m.claude[m.uid(ctx)] = map[string]string{}
	}
	m.claude[m.uid(ctx)][name] = tok
	return nil
}
func (m *memCreds) PutOpenRouter(context.Context, string) error { return nil }
func (m *memCreds) Delete(ctx context.Context, _, name string) (bool, error) {
	_, ok := m.claude[m.uid(ctx)][name]
	delete(m.claude[m.uid(ctx)], name)
	return ok, nil
}
func (m *memCreds) UsesEnvironment(_ context.Context, owner int64) bool { return m.envFor[owner] }

func credServer(t *testing.T, store CredentialStore, uid int64) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Credentials: store})
	s.sessionUserOverride = func(*http.Request) int64 { return uid }
	return s.Handler()
}

func credDo(t *testing.T, h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = "muster.test"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", "http://muster.test")
	s := &Server{auth: AuthConfig{UIPassword: testUIPassword}}
	req.Header.Set("Cookie", sessionCookieFor(s))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAMemberAddsAndDeletesTheirOwnClaudeAccountAndTheTokenIsNeverShown: the
// page round trip — a well-shaped token is saved and listed by NAME and
// fingerprint only; a malformed one is refused with the hint and is not echoed
// (the input is not prefilled); deleting an account you do not have is a 404.
func TestAMemberAddsAndDeletesTheirOwnClaudeAccountAndTheTokenIsNeverShown(t *testing.T) {
	const tok = "sk-ant-oat01-FAKE-page-roundtrip-0123456789"
	store := &memCreds{claude: map[int64]map[string]string{}, envFor: map[int64]bool{}}
	h := credServer(t, store, 41)

	rec := credDo(t, h, "POST", "/account/credentials/claude", url.Values{"name": {"personal"}, "token": {tok}})
	if rec.Code != http.StatusSeeOther || store.claude[41]["personal"] != tok {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	page := credDo(t, h, "GET", "/account/credentials", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-credential-account="personal"`) {
		t.Fatalf("page: %d %s", page.Code, page.Body)
	}
	if strings.Contains(page.Body.String(), strings.TrimPrefix(tok, credentials.ClaudeTokenPrefix)) {
		t.Fatal("the page echoes the token")
	}
	bad := "sk-ant-oat01-has a space-0123456789"
	rec = credDo(t, h, "POST", "/account/credentials/claude", url.Values{"name": {"two"}, "token": {bad}})
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "has a space") {
		t.Fatalf("a malformed token: %d, echoed %v", rec.Code, strings.Contains(rec.Body.String(), "has a space"))
	}
	if !strings.Contains(rec.Body.String(), credentials.ClaudeTokenPrefix) {
		t.Fatalf("the refusal does not say what a token looks like: %s", rec.Body)
	}
	if rec := credDo(t, h, "POST", "/account/credentials/claude/nope/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("delete of an account the caller lacks: %d", rec.Code)
	}
	if rec := credDo(t, h, "POST", "/account/credentials/claude/personal/delete", url.Values{}); rec.Code != http.StatusSeeOther || len(store.claude[41]) != 0 {
		t.Fatalf("delete own: %d, left %v", rec.Code, store.claude[41])
	}
}

// TestTheAdminHasNothingToAddAndWritesAreRefused: the environment's user is
// told where their credentials come from, and every write is refused — a
// stored admin credential would be one nothing reads.
func TestTheAdminHasNothingToAddAndWritesAreRefused(t *testing.T) {
	store := &memCreds{claude: map[int64]map[string]string{}, envFor: map[int64]bool{1: true}}
	h := credServer(t, store, 1)
	if page := credDo(t, h, "GET", "/account/credentials", nil); !strings.Contains(page.Body.String(), "data-credentials-environment") ||
		strings.Contains(page.Body.String(), `name="token"`) {
		t.Fatalf("admin page: %s", page.Body)
	}
	rec := credDo(t, h, "POST", "/account/credentials/claude", url.Values{"name": {"x"}, "token": {"sk-ant-oat01-FAKE-admin-write-0123456789"}})
	if rec.Code != http.StatusConflict || len(store.claude[1]) != 0 {
		t.Fatalf("an admin write: %d, stored %v", rec.Code, store.claude[1])
	}
	// No store at all: the page says so, a write is a 503, and nothing panics.
	none := credServer(t, nil, 41)
	if rec := credDo(t, none, "GET", "/account/credentials", nil); !strings.Contains(rec.Body.String(), "no credential store") {
		t.Fatalf("no store: %d %s", rec.Code, rec.Body)
	}
	if rec := credDo(t, none, "POST", "/account/credentials/claude", url.Values{"name": {"x"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store write: %d", rec.Code)
	}
}

// TestADispatchOfAKindTheCallerHasNoCredentialForIsRefusedWithWhatToAdd: the
// refusal happens BEFORE any row, names what to add, and the selection is asked
// for the CALLER (user 1 via the admitted session here).
func TestADispatchOfAKindTheCallerHasNoCredentialForIsRefusedWithWhatToAdd(t *testing.T) {
	var owners []int64
	s, h, store := kindsServer(t, stubKinds{pick: "work", owners: &owners,
		unavailable: map[string]string{agents.KindGateway: credentials.NeedOpenRouter}})
	rec := postDispatch(t, s, h, url.Values{"note_text": {"t"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), credentials.NeedOpenRouter) {
		t.Fatalf("gateway with no key: %d %s", rec.Code, rec.Body)
	}
	if len(store.created) != 0 {
		t.Fatalf("a refused dispatch created %d row(s)", len(store.created))
	}
	if rec := postDispatch(t, s, h, url.Values{"kind": {agents.KindClaudeCode}, "note_text": {"t"}}); rec.Code != http.StatusOK {
		t.Fatalf("control: claude-code (available): %d %s", rec.Code, rec.Body)
	}
	if len(owners) != 1 || owners[0] != authz.OperatorUserID {
		t.Fatalf("selection was asked for owners %v; want the caller [1]", owners)
	}
	// The form disables the unavailable kind and says what to add.
	req := httptest.NewRequest("GET", "/ui/agents/new", nil)
	admit(s, req)
	form := httptest.NewRecorder()
	h.ServeHTTP(form, req)
	body := form.Body.String()
	if !strings.Contains(body, `data-kind-unavailable-reason="gateway"`) || !strings.Contains(body, credentials.NeedOpenRouter) {
		t.Fatalf("the form does not say what to add:\n%s", body)
	}
}
