package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/ZacxDev/muster/internal/github"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
)

// Accounts PR 3b — the named tests for each per-user guard that is not a route
// (the isolation walk, isolation_test.go, covers the routes).

// --- the approval relay ------------------------------------------------------

// ownerRelayAgents resolves two agents by token: the operator's (user 1, admin) and
// a member's (user 2).
type ownerRelayAgents struct{ agents.Store }

func (ownerRelayAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	switch tok {
	case "relay-admin-agent-token-0123456789":
		return agents.Agent{ID: 1, Name: "admin-agent", OwnerID: 1}, nil
	case "relay-member-agent-token-012345678":
		return agents.Agent{ID: 2, Name: "member-agent", OwnerID: 2}, nil
	}
	return agents.Agent{}, errors.New("no such agent")
}

// TestTheApprovalRelayFilesOnlyForAnAdminsAgents (operator decision): an
// admin-owned agent's prompt becomes a card on the operator's router; a
// member's agent gets 503 with the hook-unarmed marker, so ccd falls back to
// the in-terminal prompt in that member's own pod, and no card is minted.
// RED when the role check is dropped (the member's card is minted, 201) and
// when roleOf answers admin for everyone (the pre-3b constant).
func TestTheApprovalRelayFilesOnlyForAnAdminsAgents(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: ownerRelayAgents{}, Users: stubUsers{}})
	gate := &isoGate{}
	s.UseApprovalGate(gate)
	h := s.Handler()
	file := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/agent/approvals", strings.NewReader(`{"tool":"Bash","command":"ls"}`))
		r.Header.Set("X-Muster-Token", tok)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := file("relay-member-agent-token-012345678")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), HookUnarmedField) {
		t.Fatalf("a member's agent filing a card: %d %s; want 503 carrying %s", w.Code, w.Body, HookUnarmedField)
	}
	if gate.n != 0 {
		t.Fatalf("a member's prompt minted %d card(s) on the operator's router", gate.n)
	}
	// Control: the admin's agent files.
	if w := file("relay-admin-agent-token-0123456789"); w.Code != http.StatusCreated || gate.n != 1 {
		t.Fatalf("the admin's agent filing a card: %d %s (minted %d); want 201 and one card", w.Code, w.Body, gate.n)
	}
}

// --- the router: only an admin's events and notifications --------------------

type recRouter struct {
	stubRouter
	got chan string
}

func (r recRouter) PublishEvent(_ context.Context, name, data string) error {
	r.got <- "publish " + name + " " + data
	return nil
}

func (r recRouter) Notify(_ context.Context, n RouterNotification) error {
	r.got <- "notify " + n.Title
	return nil
}

// TestOnlyAnAdminsEventsReachTheRouter: the router is the operator's (its push
// subscriptions are the operator's devices, its bus the operator's page). A
// member's event is delivered to that member's SSE subscribers and NOT
// mirrored; a member's "ready for review" is not pushed. The admin's are.
// RED when routerServes (or the mirror's role check) admits a member.
func TestOnlyAnAdminsEventsReachTheRouter(t *testing.T) {
	s := New(sse.New(8), AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}})
	rr := recRouter{got: make(chan string, 16)}
	s.UseRouter(rr)

	s.broadcastTo(2, EventTaskChanged, "41") // a member's
	s.notifyTaskDone(notes.Note{ID: 41, OwnerID: 2, Status: notes.StatusReadyForReview, Title: "member's"})
	s.notifyTaskCreated(notes.Note{ID: 42, OwnerID: 2, Title: "member's"})
	s.WaitForPushes()
	select {
	case got := <-rr.got:
		t.Fatalf("a member's event reached the operator's router: %s", got)
	default:
	}

	// Control: the admin's do.
	s.broadcastTo(1, EventTaskChanged, "43")
	s.notifyTaskDone(notes.Note{ID: 43, OwnerID: 1, Status: notes.StatusReadyForReview, Title: "admin's"})
	s.WaitForPushes()
	var got []string
	for len(got) < 2 {
		select {
		case g := <-rr.got:
			got = append(got, g)
		case <-time.After(2 * time.Second):
			t.Fatalf("the admin's event and push did not both reach the router: %v", got)
		}
	}
}

// --- the SSE owner of a background event ------------------------------------

// TestAnEventWithNoOwnerIsDroppedAndReported: a broadcast from a context with
// no user (System, or none) reaches nobody, and is reported — never sent to
// every stream.
func TestAnEventWithNoOwnerIsDroppedAndReported(t *testing.T) {
	bus := sse.New(8)
	s := New(bus, AuthConfig{}, log.New(io.Discard, "", 0))
	var dropped []sse.Event
	s.droppedEventProbe = func(ev sse.Event) { dropped = append(dropped, ev) }
	_, ch := bus.Subscribe(1)
	s.broadcast(authz.System(context.Background()), EventTaskChanged, "7")
	s.broadcast(context.Background(), EventTaskChanged, "8")
	select {
	case ev := <-ch:
		t.Fatalf("an owner-less event was delivered: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %d owner-less events; want both reported", len(dropped))
	}
	// Control: a user's request event is delivered to that user.
	s.broadcast(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession}), EventTaskChanged, "9")
	select {
	case ev := <-ch:
		if !strings.Contains(ev.Data, `"9"`) {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("user 1's own event never arrived")
	}
}

// TestAMirroredHostsNudgeReachesItsOwnerOnly: the mirror plugin names the
// host's owner (cmd/muster-server wires BroadcastAgentsChangedFor); the nudge
// reaches that owner's streams and no one else's. #63 shipped it as
// BroadcastAgentChanged("") — no agent, so no owner — which under the owner
// filter would reach nobody at all.
func TestAMirroredHostsNudgeReachesItsOwnerOnly(t *testing.T) {
	bus := sse.New(8)
	s := New(bus, AuthConfig{}, log.New(io.Discard, "", 0))
	_, two := bus.Subscribe(2)
	_, three := bus.Subscribe(3)
	s.BroadcastAgentsChangedFor(2)
	select {
	case ev := <-two:
		if ev.Name != EventAgentChanged {
			t.Fatalf("owner 2 got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("the host's owner never got the nudge")
	}
	select {
	case ev := <-three:
		t.Fatalf("another user got owner 2's host nudge: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTheReaperNotifiesOnlyTheTasksOwner: the idle-task reaper runs as System
// (every user's tasks), so its event's owner must come from the ROW
// (notes.ListSummaries reads owner_id). Real Postgres; A's idle task is
// reaped and only A's stream hears about it.
func TestTheReaperNotifiesOnlyTheTasksOwner(t *testing.T) {
	w := newIsoWorld(t)
	_, aCh := w.bus.Subscribe(w.a.uid)
	_, bCh := w.bus.Subscribe(w.b.uid)
	if _, err := w.pool.Exec(w.sys, `UPDATE notes SET updated_at = now() - interval '30 days' WHERE id=$1`, w.a.task.ID); err != nil {
		t.Fatal(err)
	}
	w.srv.SetTaskReapAfter(24 * time.Hour)
	w.srv.reapIdleTasks(w.sys, time.Now())
	want := `"` + strconv.FormatInt(w.a.task.ID, 10) + `"`
	deadline := time.After(2 * time.Second)
	for got := false; !got; {
		select {
		case ev := <-aCh:
			got = strings.Contains(ev.Data, want)
		case <-deadline:
			t.Fatalf("A never heard that its idle task %s was reaped", want)
		}
	}
	for {
		select {
		case ev := <-bCh:
			if strings.Contains(ev.Data, want) {
				t.Fatalf("B heard about A's reaped task: %+v", ev)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

type listAgents struct {
	agents.Store
	list []agents.Agent
}

func (l listAgents) List(context.Context) ([]agents.Agent, error) { return l.list, nil }

func (l listAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	for _, a := range l.list {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("not found")
}

// TestEachUserProvisionsTheirOwnChief: POST /agent/provision as a member

// --- connectors: a grant names its connector by NAME --------------------------

// TestAGrantNeverResolvesAnotherOwnersConnector is #61's audit note, closed:
// connector_grants.connector_name is a name, not a foreign key, and names are
// unique per OWNER — so B can hold an APPROVED grant naming A's connector's
// name. The proxy must resolve that name within B (who has no such connector)
// and never inject A's secret. Control: A's agent, same name, is injected.
func TestAGrantNeverResolvesAnotherOwnersConnector(t *testing.T) {
	w := newIsoWorld(t)
	if _, err := w.pool.Exec(w.sys, `INSERT INTO connector_grants (agent_id, owner_id, connector_name, status, reason)
		VALUES ($1, $2, $3, 'approved', 'by name')`, w.b.agent.ID, w.b.uid, w.aConnName); err != nil {
		t.Fatal(err)
	}
	path := "/connect/" + w.aConnName + "/" + w.upHost + "/probe"
	for _, h := range []http.Handler{w.h, w.connectH} {
		rec := w.do(h, "GET", path, "", cred{agentToken: w.b.token})
		if rec.Code < 400 {
			t.Errorf("B's agent through its by-name grant: %d %s; want a refusal", rec.Code, truncate(rec.Body.String(), 200))
		}
		if strings.Contains(rec.Body.String(), "upstream ok") {
			t.Errorf("B's request reached A's upstream")
		}
	}
	if n := w.up.sawSecret(w.canary + "-SECRET"); n != 0 {
		t.Fatalf("A's secret was injected for B's agent %d time(s)", n)
	}
	// Control.
	if rec := w.do(w.connectH, "GET", path, "", cred{agentToken: w.a.token}); rec.Code != 200 || w.up.sawSecret(w.canary+"-SECRET") != 1 {
		t.Fatalf("control: A's agent through its own grant: %d %s (secret seen %d)", rec.Code, truncate(rec.Body.String(), 200), w.up.sawSecret(w.canary+"-SECRET"))
	}
}

// --- GitHub OAuth state ------------------------------------------------------

// TestTheGitHubOAuthStateIsBoundToItsUser: a state minted for A is refused in
// B's callback (same browser cookie and query, different session user), and
// accepted in A's — where the request then stops at the missing code, which is
// what proves the state check passed.
func TestTheGitHubOAuthStateIsBoundToItsUser(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{GitHub: stubGitHubStore{}, Users: stubUsers{},
		GitHubOAuth: GitHubOAuthConfig{ClientID: "id", ClientSecret: "secret", BaseURL: "http://muster.test",
			StateKey: []byte("iso-oauth-state-key-0123456789abcdef")}})
	const a, b = 5, 6
	s.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(isoUserHeader), 10, 64)
		return v
	}
	h := s.Handler()

	// A starts the flow: the state comes back in the cookie and the redirect.
	r := httptest.NewRequest("GET", "/github/connect", nil)
	r.Header.Set("Cookie", sessionCookieFor(s))
	r.Header.Set(isoUserHeader, strconv.Itoa(a))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var state string
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookie {
			state = c.Value
		}
	}
	if rec.Code != http.StatusFound || !strings.HasPrefix(state, strconv.Itoa(a)+".") {
		t.Fatalf("A's connect: %d, state %q; want a redirect and a state naming user %d", rec.Code, state, a)
	}
	callback := func(uid int, st string) string {
		r := httptest.NewRequest("GET", "/github/callback?state="+url.QueryEscape(st), nil)
		r.Header.Set("Cookie", sessionCookieFor(s)+"; "+oauthStateCookie+"="+st)
		r.Header.Set(isoUserHeader, strconv.Itoa(uid))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Body.String()
	}
	if got := callback(b, state); !strings.Contains(got, "invalid oauth state") {
		t.Errorf("B completing A's flow: %q; want the state refused", got)
	}
	forged := strconv.Itoa(b) + state[strings.IndexByte(state, '.'):]
	if got := callback(b, forged); !strings.Contains(got, "invalid oauth state") {
		t.Errorf("B with A's state re-labelled as B's: %q; want the mac to refuse it", got)
	}
	if got := callback(a, state); !strings.Contains(got, "missing code") {
		t.Errorf("control: A completing its own flow: %q; want it past the state check (to the missing code)", got)
	}
}

// TestTheGitHubOAuthStateNeedsAKey (audit 🟡3): with no state key nothing is
// minted (connect answers 503) and no callback is accepted — a MAC under an
// empty key is a public function of its input. And the key is derived from the
// GitHub encryption key under its own label, never from MUSTER_UI_PASSWORD.
func TestTheGitHubOAuthStateNeedsAKey(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{GitHub: stubGitHubStore{}, Users: stubUsers{},
		GitHubOAuth: GitHubOAuthConfig{ClientID: "id", ClientSecret: "secret", BaseURL: "http://muster.test"}})
	h := s.Handler()
	r := httptest.NewRequest("GET", "/github/connect", nil)
	r.Header.Set("Cookie", sessionCookieFor(s))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("connect with no state key: %d; want 503", rec.Code)
	}
	if st, ok := s.githubOAuthState(1, "nonce"); ok || st != "" {
		t.Fatalf("minted %q with no key", st)
	}
	if s.githubOAuthStateNames("1.nonce."+hexMAC(nil, "1.nonce"), 1) {
		t.Fatal("a state MACed under the empty key was accepted")
	}
	// And a state with an EMPTY mac: with no key the expected mac is "" too,
	// so only the no-key refusal itself stops hmac.Equal("", "") (round-2 🟡2).
	if s.githubOAuthStateNames("1.nonce.", 1) {
		t.Fatal("a state with an empty mac was accepted under no key")
	}
	k1, err := github.OAuthStateKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil || len(k1) != 32 || string(k1) == "0123456789abcdef0123456789abcdef" {
		t.Fatalf("OAuthStateKey = %x, %v; want a distinct 32-byte key", k1, err)
	}
	if _, err := github.OAuthStateKey(nil); err == nil {
		t.Fatal("OAuthStateKey derived a key from nothing")
	}
}

func hexMAC(key []byte, body string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// TestTheActiveListIsTheCallersOwn (round-2 audit 🟡1): the active-agent
// registry is one server-wide map fed by every user's chat turns. Each reader
// sees only its own user's entries, a principal-less reader none — and the
// panel ALSO drops any entry whose agent the caller cannot read (the second,
// independent check, here exercised alone by a store that cannot see it).
func TestTheActiveListIsTheCallersOwn(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.markAgentActive(1, "ones-agent", "thinking")
	s.markAgentActive(2, "twos-agent", "responding")
	s.markAgentActive(0, "nobodys-agent", "thinking")
	names := func(owner int64) []string {
		var out []string
		for _, a := range s.activeAgentsList(owner) {
			out = append(out, a.Name)
		}
		return out
	}
	if got := names(1); len(got) != 1 || got[0] != "ones-agent" {
		t.Errorf("user 1's active list = %v; want [ones-agent]", got)
	}
	if got := names(2); len(got) != 1 || got[0] != "twos-agent" {
		t.Errorf("user 2's active list = %v; want [twos-agent]", got)
	}
	if got := names(0); len(got) != 0 {
		t.Errorf("a principal-less reader's active list = %v; want none", got)
	}
	// The panel's lookup: an entry the caller's store cannot read is dropped.
	s.UseExtensions(Extensions{Agents: listAgents{list: nil}})
	one := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if rows := s.activeNotificationRows(one); len(rows) != 0 {
		t.Errorf("rows for an agent the caller cannot read: %+v; want none", rows)
	}
}
