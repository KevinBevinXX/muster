package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// The bring-your-own-credentials leg of the isolation walk. The world wires the
// REAL credential store (sealed, under row-level security), the REAL plane and
// the REAL account pool over it, so the walk's routes — the credentials page,
// the dispatch form, the dispatch itself — answer from the same decision
// production does.

// isoKinds is api.AgentKinds over the real pool and plane (cmd/muster-server's
// kindSet, which this package cannot import).
type isoKinds struct {
	pool  *ccpool.Pool
	plane *credentials.Plane
}

func (k isoKinds) Enabled() []string { return []string{agents.KindGateway, agents.KindClaudeCode} }
func (k isoKinds) ClaudeAccounts(ctx context.Context, owner int64) ([]string, error) {
	return k.pool.Names(ctx, owner)
}
func (k isoKinds) SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error) {
	return k.pool.Select(ctx, owner, pin)
}
func (k isoKinds) Unavailable(ctx context.Context, owner int64) map[string]string {
	return k.plane.Unavailable(ctx, owner, k.Enabled())
}

// isoCredPage is api.CredentialStore over the real store and plane.
type isoCredPage struct {
	*credentials.PGStore
	plane *credentials.Plane
}

func (c isoCredPage) UsesEnvironment(ctx context.Context, owner int64) bool {
	return c.plane.UsesEnvironment(ctx, owner)
}

// The environment pool (the admin's) in the iso world, and A's own token.
const (
	isoEnvAccount = "isoenvpool"
	isoEnvToken   = "sk-ant-oat01-iso-ENV-pool-token-000000"
	isoATok       = "sk-ant-oat01-iso-member-A-token-111111"
	isoAORKey     = "sk-or-v1-iso-member-A-api-key-0002222"
)

// TestAnotherUsersCredentialsCannotBeSeenOrUsed: B (a member with no
// credentials) cannot see A's account on the credentials page or the dispatch
// form, cannot dispatch onto it (pinned or not), and is told what to add; the
// admin's environment pool never appears on a member's form; A — the control —
// sees and uses its own.
func TestAnotherUsersCredentialsCannotBeSeenOrUsed(t *testing.T) {
	w := newIsoWorld(t)
	acct := w.objA.ccAccount
	for _, c := range []cred{{sessionUser: w.b.uid}, {gotrueUser: w.b.uid}} {
		for _, path := range []string{"/account/credentials", "/ui/agents/new"} {
			body := w.do(w.h, "GET", path, "", c).Body.String()
			if strings.Contains(body, acct) {
				t.Errorf("B %s %s shows A's account %q", c, path, acct)
			}
			if strings.Contains(body, isoEnvAccount) {
				t.Errorf("B %s %s shows the ADMIN's environment pool", c, path)
			}
		}
		form := w.do(w.h, "GET", "/ui/agents/new", "", c).Body.String()
		for _, need := range []string{credentials.NeedClaude, credentials.NeedOpenRouter} {
			if !strings.Contains(form, htmlEscape(need)) {
				t.Errorf("B %s: the dispatch form does not say %q", c, need)
			}
		}
		for _, f := range []url.Values{
			{"kind": {agents.KindClaudeCode}, "cc_account": {acct}, "note_text": {"t"}, "action": {"save"}},
			{"kind": {agents.KindClaudeCode}, "cc_account": {isoEnvAccount}, "note_text": {"t"}, "action": {"save"}},
			{"kind": {agents.KindClaudeCode}, "note_text": {"t"}, "action": {"save"}},
			{"note_text": {"t"}, "action": {"save"}},
		} {
			rec := w.do(w.h, "POST", "/agents", f.Encode(), c)
			if rec.Code != http.StatusConflict {
				t.Errorf("B %s dispatched %v: %d %s; want 409", c, f, rec.Code, truncate(rec.Body.String(), 200))
			}
		}
	}
	// No agent of B's runs on any account.
	var n int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE owner_id=$1 AND cc_account <> ''`, w.b.uid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("B has %d claude-code agents (%v)", n, err)
	}

	// The control: A sees its account (and not the admin's pool), and a
	// claude-code dispatch by A lands on it.
	page := w.do(w.h, "GET", "/account/credentials", "", cred{sessionUser: w.a.uid}).Body.String()
	form := w.do(w.h, "GET", "/ui/agents/new", "", cred{sessionUser: w.a.uid}).Body.String()
	if !strings.Contains(page, acct) || !strings.Contains(form, acct) || strings.Contains(form, isoEnvAccount) {
		t.Fatalf("CONTROL: A's page/form: own account %v/%v, admin pool on form %v",
			strings.Contains(page, acct), strings.Contains(form, acct), strings.Contains(form, isoEnvAccount))
	}
	if strings.Contains(page, isoATok) || strings.Contains(page, strings.TrimPrefix(isoATok, credentials.ClaudeTokenPrefix)) {
		t.Fatal("A's credentials page echoes A's token")
	}
	rec := w.do(w.h, "POST", "/agents", url.Values{"kind": {agents.KindClaudeCode}, "note_text": {"t"}, "action": {"save"}}.Encode(),
		cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: A's claude-code dispatch: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var got string
	if err := w.pool.QueryRow(w.sys, `SELECT cc_account FROM agents WHERE owner_id=$1 AND cc_account <> '' ORDER BY id DESC LIMIT 1`, w.a.uid).Scan(&got); err != nil || got != acct {
		t.Fatalf("CONTROL: A's claude-code agent runs on %q (%v); want A's own %q", got, err, acct)
	}
	// And the admin's form lists the environment pool (the other control).
	if op := w.do(w.h, "GET", "/ui/agents/new", "", cred{sessionUser: w.op.uid}).Body.String(); !strings.Contains(op, isoEnvAccount) || strings.Contains(op, acct) {
		t.Fatalf("CONTROL: the admin's form: env pool %v, A's account %v", strings.Contains(op, isoEnvAccount), strings.Contains(op, acct))
	}
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(s)
}
