package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// memAccounts is an AccountStore that records audit rows and serves nothing
// else; the stream test needs only the disable path.
type memAccounts struct {
	mu    sync.Mutex
	audit []users.AuditEntry
	rows  *memUsers // the disable path's rows
	// bumpErr makes ResetCredentials fail.
	bumpErr error
}

var errMemAccounts = errors.New("memAccounts: not modelled")

func (m *memAccounts) CreateInvite(context.Context, users.Actor, []byte, string, time.Time) (users.Invite, error) {
	return users.Invite{}, errMemAccounts
}
func (m *memAccounts) ListInvites(context.Context) ([]users.Invite, error) { return nil, nil }
func (m *memAccounts) RevokeInvite(context.Context, users.Actor, int64) error {
	return errMemAccounts
}
func (m *memAccounts) OpenInvite(context.Context, []byte) (users.Invite, error) {
	return users.Invite{}, users.ErrInvalidInvite
}
func (m *memAccounts) RedeemInvite(context.Context, []byte, string, string, func(context.Context) (string, error), func(string)) (users.User, error) {
	return users.User{}, users.ErrInvalidInvite
}
func (m *memAccounts) ListUsers(context.Context) ([]users.User, error) { return nil, nil }
func (m *memAccounts) SetRole(context.Context, users.Actor, int64, authz.Role) (users.User, error) {
	return users.User{}, errMemAccounts
}
func (m *memAccounts) Audit(_ context.Context, by users.Actor, action, object string, detail map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, users.AuditEntry{UserID: by.UserID, Via: string(by.Via), Action: action, Object: object, Detail: detail})
	return nil
}
func (m *memAccounts) SetDisabledAs(ctx context.Context, by *users.Actor, id int64, disabled bool) (users.User, error) {
	u, err := m.rows.SetDisabled(ctx, id, disabled)
	if err == nil && by != nil {
		action := "user.enable"
		if disabled {
			action = "user.disable"
		}
		_ = m.Audit(ctx, *by, action, users.UserObject(id), nil)
	}
	return u, err
}
func (m *memAccounts) ResetCredentials(_ context.Context, id int64) (time.Time, error) {
	if m.bumpErr != nil {
		return time.Time{}, m.bumpErr
	}
	now := time.Now()
	if m.rows != nil {
		m.rows.mu.Lock()
		if u, ok := m.rows.rows[id]; ok {
			u.SessionsNotBefore = &now
		}
		m.rows.mu.Unlock()
	}
	return now, nil
}
func (m *memAccounts) Audited(ctx context.Context, by users.Actor, action, object string, detail map[string]any, fn func() error) error {
	if err := fn(); err != nil {
		return err
	}
	return m.Audit(ctx, by, action, object, detail)
}
func (m *memAccounts) RecentAudit(context.Context, int) ([]users.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]users.AuditEntry(nil), m.audit...), nil
}

// TestDisablingAMemberEndsItsStreams: an admin disabling a member through
// /admin/users ends every stream that member has open AT ONCE — through #65's
// DisableUser and its registry, not the periodic sweep — and bans the GoTrue
// user, while the admin's own stream (a different user) stays open.
func TestDisablingAMemberEndsItsStreams(t *testing.T) {
	f := newStreamFixture(t)
	// The disable SWEEP (accounts PR 4) would also end the streams within its
	// interval; push it out of reach, so what ends them here is the page
	// calling DisableUser — the in-process, immediate path.
	f.s.gt.sweepInterval = time.Hour
	acct := &memAccounts{rows: f.store}
	f.s.gt.Accounts = acct
	msub := f.member("m@example.com", 7, authz.RoleMember)
	ma1, mr1, _ := f.fake.Mint(msub)
	ma2, mr2, _ := f.fake.Mint(msub)
	member := []opened{
		f.openSSE(t, ma1, mr1),
		f.openWS(t, "agent terminal", "/agents/keen-wren/terminal/ws", ma1, mr1),
		f.openSSE(t, ma2, mr2),
	}
	aa, ar, _ := f.fake.Mint(f.sub)
	admin := []opened{f.openSSE(t, aa, ar)}
	requireOpen(t, "before the disable", append(member, admin...))
	if w := do(f.edge, "POST", "/admin/users/7/disable", url.Values{}, withCookies(aa, ar)); w.Code != http.StatusSeeOther {
		t.Fatalf("disable through the page: %d %s", w.Code, truncate(w.Body.String(), 300))
	}
	requireEnded(t, "after an admin disabled the member", member)
	requireOpen(t, "the ADMIN's own stream after disabling someone else", admin)
	for _, s := range admin {
		s.close()
	}
	if u, _ := f.fake.User(msub); !u.Banned {
		t.Fatal("the disable through the page did not ban the GoTrue user")
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(ma1, mr1), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("the member's next request: %d, want 401", w.Code)
	}
	if a, _ := acct.RecentAudit(context.Background(), 10); len(a) != 1 || a[0].Action != "user.disable" || a[0].UserID != 1 || a[0].Object != "user:7" {
		t.Fatalf("audit %+v", a)
	}
}

// TestAMemberCannotReachAnyAdminRoute walks every route the isolation walk
// classifies admin-only, as each member's session (A and B) — through the
// password-mode server AND a real gotrue session on the twin — B's agent token
// and — since admin-only is a SESSION tier — the shared hook token: every one
// is refused, and the target user and invite are unchanged. The CONTROL is the
// admin's own session reaching each route with its working status, so each
// refusal is a fact about the role, not a malformed request.
func TestAMemberCannotReachAnyAdminRoute(t *testing.T) {
	w := newIsoWorld(t)
	ctx := authz.System(context.Background())
	csub := w.gt.AddUser("c-target@iso.test", "c-target-password")
	var cID int64
	if err := w.pool.QueryRow(ctx, `INSERT INTO users (gotrue_sub, email, role, display_name) VALUES ($1::uuid, 'c-target@iso.test', 'member', 'c')
		RETURNING id`, csub).Scan(&cID); err != nil {
		t.Fatal(err)
	}
	inv, err := users.NewPG(w.pool).CreateInvite(ctx, users.Actor{UserID: 1, Via: authz.ViaSession},
		claimHash("admin-walk-"+w.suffix), "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = 1 OR object IN ($1, $2)`, users.UserObject(cID), "invite:"+strconv.FormatInt(inv.ID, 10))
		_, _ = w.pool.Exec(bg, `DELETE FROM invites WHERE id = $1 OR created_by = 1 AND claim_user_id IS NULL`, inv.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, cID)
	})
	var routes []string
	for r, c := range isolationClasses {
		if c == classAdminOnly {
			routes = append(routes, r)
		}
	}
	sort.Strings(routes)
	if len(routes) != 8 {
		t.Fatalf("the admin-only class has %d routes, want 8: %v", len(routes), routes)
	}
	concrete := func(route string) (string, string, string) {
		method, path, _ := strings.Cut(route, " ")
		path = strings.Replace(path, "{id}", strconv.FormatInt(cID, 10), 1)
		if strings.HasPrefix(path, "/admin/invites/") {
			path = "/admin/invites/" + strconv.FormatInt(inv.ID, 10) + "/revoke"
		}
		body := ""
		switch route {
		case "POST /admin/users/{id}/role":
			body = "role=admin"
		case "POST /admin/invites":
			body = "role=member"
		}
		return method, path, body
	}
	row := func() string {
		var js string
		_ = w.pool.QueryRow(ctx, `SELECT (SELECT row_to_json(u)::text FROM users u WHERE id=$1) || (SELECT row_to_json(i)::text FROM invites i WHERE id=$2)`, cID, inv.ID).Scan(&js)
		return js
	}
	before := row()
	var invites int
	countInvites := func() int {
		_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM invites`).Scan(&invites)
		return invites
	}
	invitesBefore := countInvites()
	refused := 0
	for _, route := range routes {
		m, p, b := concrete(route)
		for _, c := range []cred{{sessionUser: w.a.uid}, {sessionUser: w.b.uid}, {gotrueUser: w.a.uid}, {gotrueUser: w.b.uid},
			{agentToken: w.b.token}, {hook: true}} {
			rec := w.do(w.h, m, p, b, c)
			want := http.StatusForbidden
			if c.sessionUser == 0 && c.gotrueUser == 0 {
				want = http.StatusUnauthorized
			}
			if rec.Code != want {
				t.Errorf("%s as %s: %d, want %d\n  body: %s", route, c, rec.Code, want, truncate(rec.Body.String(), 200))
				continue
			}
			if strings.Contains(rec.Body.String(), "c-target@iso.test") {
				t.Errorf("LEAK: %s as %s names the target user", route, c)
			}
			refused++
		}
	}
	if refused != 6*len(routes) {
		t.Errorf("refused %d of %d member/machine requests", refused, 6*len(routes))
	}
	if after := row(); after != before {
		t.Errorf("a member's walk changed the target rows:\n before %s\n after  %s", before, after)
	}
	if countInvites() != invitesBefore {
		t.Error("a member's walk created an invite")
	}

	// --- the CONTROL: the admin's session, each route's working answer ---
	wantOp := map[string]int{
		"GET /admin/users":                  http.StatusOK,
		"GET /admin/invites":                http.StatusOK,
		"POST /admin/invites":               http.StatusOK,
		"POST /admin/users/{id}/disable":    http.StatusSeeOther,
		"POST /admin/users/{id}/enable":     http.StatusSeeOther,
		"POST /admin/users/{id}/role":       http.StatusSeeOther,
		"POST /admin/users/{id}/reset-link": http.StatusOK,
		"POST /admin/invites/{id}/revoke":   http.StatusSeeOther,
	}
	order := []string{"GET /admin/users", "GET /admin/invites", "POST /admin/invites", "POST /admin/users/{id}/disable",
		"POST /admin/users/{id}/enable", "POST /admin/users/{id}/role", "POST /admin/users/{id}/reset-link", "POST /admin/invites/{id}/revoke"}
	for _, route := range order {
		m, p, b := concrete(route)
		rec := w.do(w.h, m, p, b, cred{sessionUser: w.op.uid})
		if rec.Code != wantOp[route] {
			t.Errorf("CONTROL: the admin's %s: %d, want %d\n  body: %s", route, rec.Code, wantOp[route], truncate(rec.Body.String(), 300))
		}
		if route == "GET /admin/users" && !strings.Contains(rec.Body.String(), "c-target@iso.test") {
			t.Error("CONTROL: the admin's user list does not show the target, so a member's clean body proves nothing")
		}
	}
	if after := row(); after == before {
		t.Error("CONTROL: the admin's walk changed nothing, so the member's unchanged rows prove nothing")
	}
}

// TestAResetEndsTheOtherDevicesOpenStreams (round-3 MED): moving the session
// cutoff refuses the other devices' NEXT requests, but a stream already open
// was admitted before it — so the reset must END them too, as a disable and a
// sign-out-everywhere do. Every kind of stream (SSE, agent terminal, mirror
// terminal) opened by another device of the user ends; the session the reset
// signs in still works.
func TestAResetEndsTheOtherDevicesOpenStreams(t *testing.T) {
	f := newStreamFixture(t)
	f.s.gt.sweepInterval = time.Hour // the sweep must not be what ends them
	f.fake.UseRealClock()
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	at, rt, _ := f.fake.Mint(f.sub)
	other := f.openAll(t, at, rt)
	requireOpen(t, "before the reset", other)
	w := do(f.edge, "POST", ResetPath, url.Values{"token": {f.fake.RecoveryHash(f.sub)},
		"password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d %s", w.Code, truncate(w.Body.String(), 300))
	}
	for _, s := range other {
		if !s.ended(2 * time.Second) {
			t.Errorf("the other device's %s stream is still open 2 s after the reset", s.name)
		}
		s.close()
	}
	ck := setCookies(w)
	if g := do(f.edge, "GET", "/account", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)); g.Code != http.StatusOK {
		t.Fatalf("the session the reset signed in: %d, want 200", g.Code)
	}
}

// TestAResetWhoseCutoffCannotMoveSaysAskForANewLink (round-3 LOW): by then
// GoTrue has SPENT the single-use recovery token, so "try the link again" is
// wrong advice.
func TestAResetWhoseCutoffCannotMoveSaysAskForANewLink(t *testing.T) {
	f := newStreamFixture(t)
	f.s.gt.Accounts = &memAccounts{rows: f.store, bumpErr: errors.New("database down")}
	w := do(f.edge, "POST", ResetPath, url.Values{"token": {f.fake.RecoveryHash(f.sub)},
		"password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if w.Code == http.StatusSeeOther || !strings.Contains(w.Body.String(), "Ask for a new reset link") ||
		strings.Contains(w.Body.String(), "Try the link again") {
		t.Fatalf("a failed cutoff move: %d %s", w.Code, truncate(w.Body.String(), 400))
	}
}

// stalledRead arms f.store so the next BySub stalls after reading its row,
// and returns (reading: closed once the read is in flight, release).
func stalledRead(f *gtFixture) (<-chan struct{}, func()) {
	reading, release := make(chan struct{}), make(chan struct{})
	f.store.mu.Lock()
	f.store.stallNext = func() { close(reading); <-release }
	f.store.mu.Unlock()
	return reading, func() { close(release) }
}

// TestARequestInFlightAcrossAResetIsRefusedAtAdmission (round-4 A): a
// request that passed the cutoff check BEFORE the reset moved the cutoff, and
// registers its stream (track) AFTER the reset ended the user's streams, must
// be refused there — the reset is recorded under the same lock as a
// sign-out-everywhere (notBefore[sub] = cutoff), which track checks. The
// reset's own new session (iat > cutoff) is still admitted.
func TestARequestInFlightAcrossAResetIsRefusedAtAdmission(t *testing.T) {
	f := newStreamFixture(t)
	f.s.gt.sweepInterval = time.Hour
	f.fake.UseRealClock()
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	at, rt, _ := f.fake.Mint(f.sub)
	reading, release := stalledRead(f.gtFixture)
	got := make(chan int, 1)
	go func() { got <- do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR).Code }()
	<-reading // the old request has read the PRE-reset row and is resolving
	w := do(f.edge, "POST", ResetPath, url.Values{"token": {f.fake.RecoveryHash(f.sub)},
		"password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", w.Code)
	}
	release()
	if code := <-got; code != http.StatusUnauthorized {
		t.Fatalf("a request that resolved before the reset and registered after it: %d, want 401", code)
	}
	ck := setCookies(w)
	if g := do(f.edge, "GET", "/account", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)); g.Code != http.StatusOK {
		t.Fatalf("the reset's own new session: %d, want 200", g.Code)
	}
}

// TestAStaleReadDoesNotRePoisonTheUserCache (round-4 B): a user-row read in
// flight across an invalidation (forgetUser, after the row changed) must not
// write its pre-change row back into the 30 s cache. The row change here is a
// session cutoff moved WITHOUT this process's reset record — as a cutoff moved
// elsewhere would be — so the cache is the only thing that can carry it.
func TestAStaleReadDoesNotRePoisonTheUserCache(t *testing.T) {
	f := newStreamFixture(t)
	f.fake.SetAccessTTL(time.Hour)
	at, rt, _ := f.fake.Mint(f.sub)
	reading, release := stalledRead(f.gtFixture)
	done := make(chan struct{})
	go func() { do(f.edge, "GET", "/account", nil, withCookies(at, rt)); close(done) }()
	<-reading
	cut := f.fake.Now().Add(time.Second)
	f.store.mu.Lock()
	f.store.rows[1].SessionsNotBefore = &cut
	f.store.mu.Unlock()
	f.s.gt.forgetUser(1)
	release()
	<-done
	if g := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); g.Code != http.StatusUnauthorized {
		t.Fatalf("the pre-cutoff token after a stale read re-cached the old row: %d, want 401", g.Code)
	}
	// CONTROL: without the in-flight read the same sequence refuses too.
	f2 := newStreamFixture(t)
	f2.fake.SetAccessTTL(time.Hour)
	at2, rt2, _ := f2.fake.Mint(f2.sub)
	do(f2.edge, "GET", "/account", nil, withCookies(at2, rt2))
	cut2 := f2.fake.Now().Add(time.Second)
	f2.store.mu.Lock()
	f2.store.rows[1].SessionsNotBefore = &cut2
	f2.store.mu.Unlock()
	f2.s.gt.forgetUser(1)
	if g := do(f2.edge, "GET", "/account", nil, withCookies(at2, rt2), asXHR); g.Code != http.StatusUnauthorized {
		t.Fatalf("CONTROL: %d, want 401", g.Code)
	}
}

// TestAStaleReadAcrossADisableIsStillRefused: the same in-flight read across
// a DISABLE re-caches the pre-disable row too, but track refuses the user
// (g.disabled); and once the generation guard drops the stale write, the next
// request reads the disabled row itself. An invariant guard: it held on
// 7ba2569 (track already covered disable); kept so the two layers stay two.
func TestAStaleReadAcrossADisableIsStillRefused(t *testing.T) {
	f := newStreamFixture(t)
	f.fake.SetAccessTTL(time.Hour)
	sub := f.member("d@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	reading, release := stalledRead(f.gtFixture)
	done := make(chan struct{})
	go func() { do(f.edge, "GET", "/account", nil, withCookies(at, rt)); close(done) }()
	<-reading
	if err := f.s.DisableUser(context.Background(), 7, true); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if g := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); g.Code != http.StatusUnauthorized {
		t.Fatalf("after a disable with a stale read in flight: %d, want 401", g.Code)
	}
}
