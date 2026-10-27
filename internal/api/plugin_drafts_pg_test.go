package api

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// draftSeam is the plugins seam test's plane (plugins_seam_pg_test.go) with Web
// Push wired: real Postgres stores, real routes, the real deliverer and gateway;
// only the agent runtime and the push service's transport are fakes. Member a
// owns everything; member b is the outsider.
type draftSeam struct {
	t       *testing.T
	ctx     context.Context
	pool    interface{ Close() }
	srv     *Server
	h       http.Handler
	ps      *plugins.PGStore
	as      *agents.PGStore
	rt      *pluginRuntime
	a, b    int64
	pusher  *payloadPusher
	dsn     string
	suffix  string
	userHdr string
}

func newDraftSeam(t *testing.T) *draftSeam {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := &draftSeam{t: t, ctx: ctx, pool: pool, dsn: dsn, suffix: strconv.FormatInt(time.Now().UnixNano(), 36), userHdr: "X-Draft-Test-User"}
	sys := authz.System(ctx)
	for _, id := range []*int64{&d.a, &d.b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "drf"+d.suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ids := []int64{d.a, d.b}
		for _, q := range []string{`DELETE FROM plugins WHERE owner_id = ANY($1)`, `DELETE FROM agents WHERE owner_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})
	d.rt = &pluginRuntime{}
	up := httptest.NewServer(d.rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	d.ps, d.as = plugins.NewPG(pool), agents.NewPG(pool)
	store := &memPush{}
	d.pusher = &payloadPusher{}
	claim(t, store, d.a, ep("draft-a"))
	claim(t, store, d.b, ep("draft-b"))
	d.srv = New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-draft-operator-pw"}, log.New(io.Discard, "", 0))
	d.srv.UseExtensions(Extensions{Agents: d.as, Users: users.NewPG(pool), Plugins: d.ps, PluginGateway: gw, Gateway: gw,
		Push: push.NewService(store, d.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	d.srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(d.userHdr), 10, 64)
		return v
	}
	d.h = d.srv.Handler()
	return d
}

func (d *draftSeam) asUser(uid int64) context.Context {
	return authz.WithPrincipal(d.ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func (d *draftSeam) do(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	if strings.HasPrefix(body, "{") {
		r.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	set(r)
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, r)
	return rec
}

// session is uid's browser on muster's own origin: fresh unless stale, a
// navigation (a form the browser submitted) when nav.
func (d *draftSeam) session(uid int64, stale, nav bool) func(*http.Request) {
	return func(r *http.Request) {
		c := sessionCookieFor(d.srv)
		if stale {
			c = staleCookieFor(d.srv)
		}
		r.Header.Set("Cookie", c)
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set(d.userHdr, strconv.FormatInt(uid, 10))
		if nav {
			r.Header.Set("Sec-Fetch-Dest", "document")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
		}
	}
}

func bearerTok(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func (d *draftSeam) mint(uid int64) string {
	d.t.Helper()
	rec := d.do("POST", "/plugins", url.Values{"name": {"example-inbox"}, "label": {"Example inbox"},
		"data_scope": {"example-inbox"}}.Encode(), d.session(uid, false, true))
	m := mintedPluginRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		d.t.Fatalf("mint for user %d: %d, no token on the page", uid, rec.Code)
	}
	// A freshly minted plugin has no target. If uid already has an agent, point
	// the plugin at it — see agent's header for why this has to be explicit.
	if id := d.standingAgent(uid); id != 0 {
		d.linkPlugins(uid, id)
	}
	return m[1]
}

// agent creates uid's standing agent and MAKES IT THE TARGET of uid's plugins.
//
// 🔴 THE LINK USED TO BE IMPLICIT IN THE DISPLAY NAME, AND NOW HAS TO BE
// WRITTEN. Delivery resolved its recipient by label — the lowest-id agent called
// `agent` — so creating this row was all a test had to do. A plugin now names
// its target (plugins.target_agent_id), so a fixture that only creates the agent
// leaves every message QUEUED with reason `no_target`, which is how each of
// these tests first failed. linkPlugins is called from BOTH ends (here and in
// mint) because tests create the agent and mint the plugin in either order.
func (d *draftSeam) agent(uid int64, name string) agents.Agent {
	d.t.Helper()
	a, err := d.as.Create(d.asUser(uid), agents.Agent{Name: name + "-" + d.suffix, Namespace: "devpod-" + name + "-" + d.suffix,
		DisplayName: "agent", Status: agents.StatusRunning, HooksToken: "drf-hooks-" + name + "-" + d.suffix})
	if err != nil {
		d.t.Fatal(err)
	}
	d.linkPlugins(uid, a.ID)
	return a
}

// linkPlugins points every one of uid's plugins at agent agentID. A plugin with
// a target already set is left alone, so a test that chose one deliberately
// keeps it.
func (d *draftSeam) linkPlugins(uid, agentID int64) {
	d.t.Helper()
	ps, err := d.ps.List(d.asUser(uid))
	if err != nil {
		d.t.Fatalf("link plugins for user %d: %v", uid, err)
	}
	for _, p := range ps {
		if p.TargetAgentID != nil && *p.TargetAgentID != 0 {
			continue
		}
		if err := d.ps.SetTargetAgent(d.asUser(uid), p.ID, agentID); err != nil {
			d.t.Fatalf("plugin %d -> agent %d: %v", p.ID, agentID, err)
		}
	}
}

// standingAgent is uid's lowest-id agent, or 0. mint uses it so a plugin minted
// AFTER the agent still gets a target.
func (d *draftSeam) standingAgent(uid int64) int64 {
	d.t.Helper()
	list, err := d.as.List(d.asUser(uid))
	if err != nil {
		return 0
	}
	var found int64
	for _, a := range list {
		if a.OwnerID == uid && (found == 0 || a.ID < found) {
			found = a.ID
		}
	}
	return found
}

func (d *draftSeam) post(tok, ext, extra string) int64 {
	d.t.Helper()
	rec := d.do("POST", "/plugin/v1/messages", `{"external_id":"`+ext+`","sender":"Ada <ada@example.invalid>",`+
		`"subject":"lunch?","body":"Are you free? <script>alert(1)</script>"`+extra+`}`, bearerTok(tok))
	var out struct {
		ID int64 `json:"id"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.ID == 0 {
		d.t.Fatalf("post %s: %d %s", ext, rec.Code, rec.Body.String())
	}
	d.srv.WaitForPluginDeliveries()
	return out.ID
}

func (d *draftSeam) claim(tok string, wait int) *httptest.ResponseRecorder {
	return d.do("POST", "/plugin/v1/drafts/claim", `{"wait_seconds":`+strconv.Itoa(wait)+`}`, bearerTok(tok))
}

func (d *draftSeam) draft(id int64) plugins.Draft {
	d.t.Helper()
	got, err := d.ps.Draft(d.asUser(d.a), id)
	if err != nil {
		d.t.Fatal(err)
	}
	return got
}

var draftIDRE = regexp.MustCompile(`"draft_id":(\d+)`)

// replyDraft calls reply_draft as agent a through the gateway transport's
// dispatch (the /mcp transport is TestTheChiefDraftsAReplyOverMCP's).
func (d *draftSeam) replyDraft(a agents.Agent, messageID int64, body string) (int64, string) {
	d.t.Helper()
	args, _ := json.Marshal(map[string]any{"message_id": messageID, "body": body})
	out := d.srv.AgentToolDispatch(d.asUser(a.OwnerID), a)(replyDraftToolName, string(args))
	m := draftIDRE.FindStringSubmatch(out)
	if m == nil {
		return 0, out
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id, out
}

// TestAReplyDraftGoesFromTheChiefToThePluginOnceThroughTheRealPlane is the
// whole reply loop, end to end over the real plane:
//
//  1. RETRY-ON-CLAIM: a message posted while its owner has no agent stays queued;
//     the agent appears; the plugin's next claim (nothing to hand out: 204)
//     delivers it — the only retry there is.
//  2. The agent drafts a reply (reply_draft). The owner gets the `plugin` push,
//     in muster's words, deep-linked to the card.
//  3. The Asks page's Reply drafts tab shows the card: the outside text
//     escaped, the draft spelled out.
//  4. A STALE approval is refused (403, both as a script and as a form), the
//     draft stays pending and a claim gets nothing. Another member cannot
//     approve it at all.
//  5. A fresh approval → the claim hands it out ONCE ({draft_id,
//     message_external_id, body, approved_at}), a second claim gets 204.
//  6. The result settles it as sent; the same result again is 200; the other is
//     409; another member's plugin gets 404 — and the card says Sent.
//  7. A second draft whose lease lapses is UNCONFIRMED, not re-offered, and its
//     card offers no approval (an approve POST is 409).
//  8. A new draft for that message, approved, is SUPERSEDED when the
//     unconfirmed one is reported sent late — and is never handed out.
func TestAReplyDraftGoesFromTheChiefToThePluginOnceThroughTheRealPlane(t *testing.T) {
	d := newDraftSeam(t)
	aTok, bTok := d.mint(d.a), d.mint(d.b)

	// 1. Retry-on-claim.
	m1 := d.post(aTok, "ext-1-"+d.suffix, "")
	if got, _ := d.ps.Message(d.asUser(d.a), m1); got.Status != plugins.StatusQueued || got.Attempts != 1 {
		t.Fatalf("with no agent the message is %s after %d attempt(s); want queued after 1", got.Status, got.Attempts)
	}
	agent := d.agent(d.a, "drfchief")
	d.rt.expect(agent)
	if rec := d.claim(aTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a claim with nothing approved: %d %s; want 204", rec.Code, rec.Body.String())
	}
	d.srv.WaitForPluginDeliveries()
	if got, _ := d.ps.Message(d.asUser(d.a), m1); got.Status != plugins.StatusDelivered {
		t.Fatalf("after the claim the queued message is %s (runtime saw %d turns); want delivered — the claim retries queued messages",
			got.Status, len(d.rt.seen()))
	}
	if n := len(d.rt.seen()); n != 1 {
		t.Fatalf("the runtime saw %d turns; want 1", n)
	}

	// 2. The agent drafts a reply; the owner is told WHO and WHERE only — the
	// sender is not verified, so not even the name it claims.
	draftID, out := d.replyDraft(agent, m1, "Yes, noon works.")
	if draftID == 0 || !strings.Contains(out, `"status":"pending"`) {
		t.Fatalf("reply_draft: %s", out)
	}
	d.srv.WaitForPushes()
	pl := d.pusher.to(ep("draft-a"))
	const wantBody = "Reply draft: unverified sender (Example inbox)"
	if len(pl) != 1 || pl[0].Title != DraftPushTitle || pl[0].Body != wantBody ||
		pl[0].Data["url"] != "/attention?tab=drafts#draft-"+strconv.FormatInt(draftID, 10) || pl[0].Type != "plugin" {
		t.Fatalf("the owner's push: %+v; want one `plugin` push naming who and where only, linked to the card", pl)
	}
	if got := d.pusher.to(ep("draft-b")); len(got) != 0 {
		t.Fatalf("the other member got %d pushes", len(got))
	}

	// 3. The card, on the Asks page's Reply drafts tab.
	page := d.do("GET", "/attention?tab=drafts", "", d.session(d.a, false, true)).Body.String()
	card := cardOf(t, page, draftID)
	for _, want := range []string{`data-draft-status="pending"`, "Are you free? &lt;script&gt;alert(1)&lt;/script&gt;",
		"Yes, noon works.", "16 characters", "data-draft-approve", "data-draft-reject", "data-step-up-form"} {
		if !strings.Contains(card, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Fatal("the outside sender's text is drawn unescaped")
	}
	if !strings.Contains(page, `id="term-confirm"`) {
		t.Fatal("the page has no confirm dialog to ask for a fresh sign-in")
	}
	if other := d.do("GET", "/attention?tab=drafts", "", d.session(d.b, false, true)).Body.String(); strings.Contains(other, "noon works") {
		t.Fatal("another member's Asks page shows this draft")
	}

	// 4. Stale approvals are refused.
	approve := "/plugins/drafts/" + strconv.FormatInt(draftID, 10) + "/approve"
	if rec := d.do("POST", approve, "", d.session(d.a, true, false)); rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale approval (script): %d %v; want 403 with X-Muster-Step-Up", rec.Code, rec.Header())
	}
	if rec := d.do("POST", approve, "", d.session(d.a, true, true)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), html.EscapeString(DraftStepUpRequired)) {
		t.Fatalf("a stale approval (form): %d; want 403 and the page saying why", rec.Code)
	}
	if rec := d.do("POST", approve, "", d.session(d.b, false, true)); rec.Code != http.StatusNotFound {
		t.Fatalf("another member approving: %d; want 404", rec.Code)
	}
	if got := d.draft(draftID); got.Status != plugins.DraftPending {
		t.Fatalf("after refused approvals the draft is %s; want pending", got.Status)
	}
	if rec := d.claim(aTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a claim before approval: %d %s; want 204", rec.Code, rec.Body.String())
	}

	// 5. Fresh approval → one claim.
	if rec := d.do("POST", approve, "", d.session(d.a, false, true)); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/attention?tab=archived&draft="+strconv.FormatInt(draftID, 10)+"#draft-"+strconv.FormatInt(draftID, 10) {
		t.Fatalf("a fresh approval: %d %s; want 303 to the card", rec.Code, rec.Header().Get("Location"))
	}
	rec := d.claim(aTok, 0)
	var c map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &c) != nil {
		t.Fatalf("the claim: %d %s", rec.Code, rec.Body.String())
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "approved_at,body,draft_id,message_external_id" || c["body"] != "Yes, noon works." ||
		c["message_external_id"] != "ext-1-"+d.suffix || int64(c["draft_id"].(float64)) != draftID {
		t.Fatalf("the claim body: %s", rec.Body.String())
	}
	if rec := d.claim(aTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a second claim: %d %s; want 204 — a draft is handed out once", rec.Code, rec.Body.String())
	}
	if rec := d.claim(bTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("another member's plugin claiming: %d; want 204", rec.Code)
	}
	if rec := d.do("POST", "/plugin/v1/drafts/claim", `{}`, bearerTok(tokTestHook)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the hook token claiming: %d; want 401", rec.Code)
	}

	// 6. The result.
	result := "/plugin/v1/drafts/" + strconv.FormatInt(draftID, 10) + "/result"
	if rec := d.do("POST", result, `{"ok":true,"external_ref":"up-1"}`, bearerTok(bTok)); rec.Code != http.StatusNotFound {
		t.Fatalf("another member's plugin reporting: %d; want 404", rec.Code)
	}
	if rec := d.do("POST", result, `{"external_ref":"up-1"}`, bearerTok(aTok)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a result without ok: %d; want 400", rec.Code)
	}
	for i, want := range []int{http.StatusOK, http.StatusOK} {
		rec := d.do("POST", result, `{"ok":true,"external_ref":"up-1","detail":"queued upstream"}`, bearerTok(aTok))
		if rec.Code != want || !strings.Contains(rec.Body.String(), `"status":"sent"`) {
			t.Fatalf("result #%d: %d %s; want %d sent", i+1, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := d.do("POST", result, `{"ok":false}`, bearerTok(aTok)); rec.Code != http.StatusConflict {
		t.Fatalf("the conflicting result: %d; want 409", rec.Code)
	}
	page = d.do("GET", "/attention?tab=archived", "", d.session(d.a, false, true)).Body.String()
	if card := cardOf(t, page, draftID); !strings.Contains(card, `data-draft-status="sent"`) || !strings.Contains(card, "up-1") ||
		strings.Contains(card, "data-draft-approve") {
		t.Fatalf("the sent card:\n%s", card)
	}

	// 7. A lapsed lease.
	m2 := d.post(aTok, "ext-2-"+d.suffix, "")
	second, out := d.replyDraft(agent, m2, "Second reply.")
	if second == 0 {
		t.Fatalf("second draft: %s", out)
	}
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(second, 10)+"/approve", "", d.session(d.a, false, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve the second: %d", rec.Code)
	}
	if rec := d.claim(aTok, 0); rec.Code != http.StatusOK {
		t.Fatalf("claim the second: %d", rec.Code)
	}
	dbtest.OwnerExec(t, d.dsn, `UPDATE plugin_reply_drafts SET claimed_at = claimed_at - interval '10 minutes',
		lease_until = lease_until - interval '10 minutes' WHERE id = $1`, second)
	if rec := d.claim(aTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a claim after the lease lapsed: %d %s; want 204 — never re-offered", rec.Code, rec.Body.String())
	}
	if got := d.draft(second); got.Status != plugins.DraftUnconfirmed {
		t.Fatalf("the lapsed draft is %s; want unconfirmed", got.Status)
	}
	page = d.do("GET", "/attention?tab=archived", "", d.session(d.a, false, true)).Body.String()
	if card := cardOf(t, page, second); strings.Contains(card, "data-step-up-form") || !strings.Contains(card, "It may have been") {
		t.Fatalf("the unconfirmed card: want the may-have-been-sent warning and NO approval form:\n%s", card)
	}
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(second, 10)+"/approve", "", d.session(d.a, false, true)); rec.Code != http.StatusConflict {
		t.Fatalf("approving an unconfirmed draft: %d; want 409 — it is never offered again", rec.Code)
	}
	// 8. The message-scoped rule: the agent files a NEW draft for the same
	// message (its card warns), the owner approves it — and then the plugin
	// reports the unconfirmed one sent after all. The new one must not go too.
	third, out := d.replyDraft(agent, m2, "Third reply.")
	if third == 0 {
		t.Fatalf("third draft: %s", out)
	}
	page = d.do("GET", "/attention?tab=archived", "", d.session(d.a, false, true)).Body.String()
	if card := cardOf(t, page, third); !strings.Contains(card, "data-draft-twice") {
		t.Fatalf("the new draft's card does not warn that an earlier reply may have gone:\n%s", card)
	}
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(third, 10)+"/approve", "", d.session(d.a, false, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve the third: %d", rec.Code)
	}
	if rec := d.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(second, 10)+"/result", `{"ok":true,"external_ref":"up-2"}`, bearerTok(aTok)); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"status":"sent"`) {
		t.Fatalf("the unconfirmed draft's late sent: %d %s", rec.Code, rec.Body.String())
	}
	if rec := d.claim(aTok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a claim after the late sent: %d %s; want 204 — the third draft is superseded", rec.Code, rec.Body.String())
	}
	if got := d.draft(third); got.Status != plugins.DraftSuperseded {
		t.Fatalf("the third draft is %s; want superseded", got.Status)
	}
}

// cardOf is draft id's <li> on an Asks page.
func cardOf(t *testing.T, page string, id int64) string {
	t.Helper()
	start := strings.Index(page, `id="draft-`+strconv.FormatInt(id, 10)+`"`)
	if start < 0 {
		t.Fatalf("no card for draft %d on the page", id)
	}
	end := strings.Index(page[start:], "</li>")
	return page[start : start+end]
}

// TestARejectNeedsNoFreshSignInAndAClaimWaitsForAnApproval: rejecting is never
// the risky direction, so a stale session may reject; and a claim that is
// waiting returns the draft as soon as it is approved, well before its wait.
func TestARejectNeedsNoFreshSignInAndAClaimWaitsForAnApproval(t *testing.T) {
	d := newDraftSeam(t)
	aTok := d.mint(d.a)
	agent := d.agent(d.a, "drfchief")
	d.rt.expect(agent)
	m := d.post(aTok, "ext-r-"+d.suffix, "")
	id, out := d.replyDraft(agent, m, "No thanks.")
	if id == 0 {
		t.Fatalf("draft: %s", out)
	}
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(id, 10)+"/reject", "", d.session(d.a, true, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a stale reject: %d; want 303", rec.Code)
	}
	if got := d.draft(id); got.Status != plugins.DraftRejected {
		t.Fatalf("the rejected draft is %s", got.Status)
	}
	// The decision withdraws the draft's `plugin` notification from A's devices
	// (the attention queue's clear): the tag the push that announced it set.
	d.srv.WaitForPushes()
	var resolved int
	for _, p := range d.pusher.to(ep("draft-a")) {
		if p.Type == push.TypeResolved && p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("rejecting draft %d sent %d resolved message(s); want 1: %+v", id, resolved, d.pusher.to(ep("draft-a")))
	}
	id2, _ := d.replyDraft(agent, m, "Maybe.")
	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- d.claim(aTok, 20) }()
	time.Sleep(300 * time.Millisecond)
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(id2, 10)+"/approve", "", d.session(d.a, false, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d", rec.Code)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Maybe.") {
			t.Fatalf("the waiting claim: %d %s", rec.Code, rec.Body.String())
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("the waiting claim took %s", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting claim did not return after the approval")
	}
}

// TestNoReplyIsStoredAndToldToTheChief: a message posted with no_reply:true is
// stored with the mark, the agent's turn says no reply is possible, and
// reply_draft refuses it. CONTROL: the same flow without the mark drafts.
func TestNoReplyIsStoredAndToldToTheChief(t *testing.T) {
	d := newDraftSeam(t)
	aTok := d.mint(d.a)
	agent := d.agent(d.a, "drfchief")
	d.rt.expect(agent)
	nr := d.post(aTok, "ext-nr-"+d.suffix, `,"no_reply":true`)
	if got, _ := d.ps.Message(d.asUser(d.a), nr); !got.NoReply || got.Status != plugins.StatusDelivered {
		t.Fatalf("the no_reply message: %+v", got)
	}
	seen := d.rt.seen()
	if len(seen) != 1 || !strings.Contains(seen[0], strings.ReplaceAll(plugins.NoReplySentence, `"`, `\"`)) {
		t.Fatalf("the agent's turn does not say no reply is possible: %q", seen)
	}
	if id, out := d.replyDraft(agent, nr, "hello"); id != 0 || !strings.Contains(out, "cannot be replied to") {
		t.Fatalf("reply_draft on a no_reply message: %s; want refused", out)
	}
	ok := d.post(aTok, "ext-ok-"+d.suffix, "")
	if seen := d.rt.seen(); strings.Contains(seen[len(seen)-1], "cannot be replied to") {
		t.Fatal("CONTROL: an ordinary message's turn says it cannot be replied to")
	}
	if id, out := d.replyDraft(agent, ok, "hello"); id == 0 {
		t.Fatalf("CONTROL: reply_draft on an ordinary message: %s", out)
	}
}

// TestTheChiefDraftsAReplyOverMCP: through the real /mcp door, the agent's own
// token files a draft for a message delivered to it; a worker of the same owner
// is not offered the tool (tools/call is refused); a message delivered to the
// worker, and a no_reply one, are refused to the agent with words.
func TestTheChiefDraftsAReplyOverMCP(t *testing.T) {
	g := newGatedFixture(t)
	call := func(token string, args map[string]any) (string, bool, *mcpError) {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": replyDraftToolName, "arguments": args}})
		r, code := g.mcp(t, token, string(raw), nil)
		if r == nil {
			t.Fatalf("/mcp answered HTTP %d", code)
		}
		if r.Error != nil {
			return "", false, r.Error
		}
		text, isErr := toolText(t, r)
		return text, isErr, nil
	}
	m := g.deliveredMessage(t, g.owner, g.agent.ID, "mcp-1")
	text, isErr, rpcErr := call(g.chiefTok, map[string]any{"message_id": m.ID, "body": "Thanks!"})
	if rpcErr != nil || isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("the agent's reply_draft: %q isErr=%v rpc=%v", text, isErr, rpcErr)
	}
	// 🔴 A WORKER IS OFFERED reply_draft NOW AND IS STILL REFUSED THIS CALL — on
	// the MESSAGE, not on its rank. The tool was the labelled agent's alone, so
	// /mcp answered a worker with an rpc error ("unknown tool"); the gate that
	// actually protects a plugin's conversation is DeliveredTo, which this
	// message fails because it was delivered to another agent.
	if text, isErr, rpcErr := call(g.workerTok, map[string]any{"message_id": m.ID, "body": "x"}); rpcErr != nil {
		t.Fatalf("a worker was not offered reply_draft over /mcp: %v", rpcErr)
	} else if !isErr || !strings.Contains(text, "not a plugin message delivered to you") {
		t.Fatalf("a worker's reply_draft on another agent's message: %q isErr=%v; want the delivered-to-you refusal", text, isErr)
	}
	wm := g.deliveredMessage(t, g.owner, g.worker.ID, "mcp-2")
	if text, isErr, _ := call(g.chiefTok, map[string]any{"message_id": wm.ID, "body": "x"}); !isErr || !strings.Contains(text, "not a plugin message delivered to you") {
		t.Fatalf("a message delivered to the worker: %q; want refused", text)
	}
	ps, _ := g.plugins.List(g.asOwner)
	nr, _, err := g.plugins.Submit(g.asOwner, ps[0].ID, plugins.MessageIn{ExternalID: "mcp-nr", Body: "do not answer", NoReply: true})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.plugins.BeginDelivery(g.asOwner, nr.ID, g.agent.ID); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := g.plugins.RecordDelivery(g.asOwner, nr.ID, g.agent.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatal(err)
	}
	if text, isErr, _ := call(g.chiefTok, map[string]any{"message_id": nr.ID, "body": "x"}); !isErr || !strings.Contains(text, "cannot be replied to") {
		t.Fatalf("a no_reply message: %q; want refused", text)
	}
}

// TestAClaimAnswersWhileADeliveryIsStillRunning: a delivered message is a TOOL
// turn and can take minutes, so neither the claim-time retry nor a delivery
// already running may hold a claim's answer — and one message is never handed
// to the agent twice at once.
//
//  1. A message queued while its owner had no agent; then an agent whose runtime
//     HOLDS every turn. A claim must answer (204) while the retry it started is
//     still inside that held turn — and a second claim meanwhile starts nothing
//     new. Released, the message is delivered, in ONE turn.
//  2. A message whose POST-time delivery is held: a claim answers, and its retry
//     — which finds the same message still queued — does not send it again (the
//     agent's session claim refuses the second attempt). One turn in all.
func TestAClaimAnswersWhileADeliveryIsStillRunning(t *testing.T) {
	d := newDraftSeam(t)
	aTok := d.mint(d.a)
	claimWithin := func(what string) {
		t.Helper()
		done := make(chan int, 1)
		go func() { done <- d.claim(aTok, 0).Code }()
		select {
		case code := <-done:
			if code != http.StatusNoContent {
				t.Fatalf("%s: the claim answered %d; want 204", what, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the claim did not answer within 5s while a delivery was running — it waited for the delivery", what)
		}
	}
	arrivedWithin := func(arrived <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no turn reached the runtime — the delivery never started", what)
		}
	}

	// 1. The claim-time retry is detached.
	m1 := d.post(aTok, "slow-1-"+d.suffix, "")
	agent := d.agent(d.a, "slowchief")
	d.rt.expect(agent)
	arrived, release := d.rt.hold()
	t.Cleanup(release)
	claimWithin("a claim whose retry is delivering")
	arrivedWithin(arrived, "the claim's retry")
	// While its turn is held the message is being delivered to the agent (audit
	// round 1: the agent may cite it on that turn), and not offered again.
	if got, _ := d.ps.Message(d.asUser(d.a), m1); got.Status != plugins.StatusDelivering {
		t.Fatalf("the message is %s while its turn is still held; want delivering", got.Status)
	}
	claimWithin("a second claim while the retry is still delivering")
	release()
	d.srv.WaitForPluginDeliveries()
	if got, _ := d.ps.Message(d.asUser(d.a), m1); got.Status != plugins.StatusDelivered {
		t.Fatalf("after the release the message is %s; want delivered", got.Status)
	}
	if n := len(d.rt.seen()); n != 1 {
		t.Fatalf("the runtime saw %d turns for one message; want 1", n)
	}

	// 2. A POST-time delivery is running when a claim's retry finds the message.
	arrived, release = d.rt.hold()
	t.Cleanup(release)
	rec := d.do("POST", "/plugin/v1/messages", `{"external_id":"slow-2-`+d.suffix+`","sender":"Ada <ada@example.invalid>",`+
		`"subject":"again","body":"Still free?"}`, bearerTok(aTok))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	arrivedWithin(arrived, "the POST's delivery")
	claimWithin("a claim while the POST's delivery is running")
	release()
	d.srv.WaitForPluginDeliveries()
	if n := len(d.rt.seen()); n != 2 {
		t.Fatalf("the runtime saw %d turns for two messages; want 2 — the claim's retry sent the running one again", n)
	}
}
