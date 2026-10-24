package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// plugin_drafts_list / plugin_draft_decide (plugin_draft_tools.go), through
// the real server against a real Postgres: the owner decides a pending reply
// draft through one of their own MIRRORED Claude Code sessions (POST
// /mirror/mcp) — the Asks page's own decision (decideDraftAs), with no fresh
// sign-in, with where it came from on its audit row — refused unless it names
// the exact body's SHA-256. Any of the owner's mirrored sessions may decide,
// the one a message was routed to included; nobody else can call them.
//
// The fixture is the session-routing one (routeFixture): the owner's host
// "tower" lists routeSID (a route's target) and otherRouteSID (another of the
// owner's sessions on the same host).

// draftToolFixture is routeFixture armed for the mirror door, with a plugin
// whose token it holds (so a claim can be made over HTTP) and a second
// member's mirror token on a host of the same name.
type draftToolFixture struct {
	*routeFixture
	tok       string // the owner's mirror token (host "tower")
	strangers string // another member's mirror token (host "tower" too)
	stranger  int64
	plugTok   string
	plugin    plugins.Plugin
}

func newDraftToolFixture(t *testing.T) *draftToolFixture {
	t.Helper()
	f := &draftToolFixture{routeFixture: newRouteFixture(t)}
	f.tok = f.withRouteReply(t)
	f.stranger = f.newMember(t)
	f.strangers = mirrorTok('s')
	f.srv.ext.MirrorTokens = fakeMirrorTokens{
		f.tok:       mirror.Token{ID: 9, OwnerID: f.owner, Host: "tower"},
		f.strangers: mirror.Token{ID: 10, OwnerID: f.stranger, Host: "tower"},
	}
	var err error
	if f.plugTok, f.plugin, err = f.plugins.Create(f.asOwner, "example-inbox", "Example inbox", "example-inbox-data"); err != nil {
		t.Fatal(err)
	}
	// The owner's browser, for the Asks page's control decisions.
	owner := f.owner
	f.srv.sessionUserOverride = func(*http.Request) int64 { return owner }
	return f
}

// sha is the hex SHA-256 of body, computed HERE (not by the code under test).
func sha(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// draft stores a message from sender (verified or not) delivered to the chief
// and has the chief draft body for it (reply_draft): the draft's id.
func (f *draftToolFixture) draft(t *testing.T, external, sender string, verified bool, body string) int64 {
	t.Helper()
	_, m := f.pluginMessage(t, "example-inbox", external, sender, verified)
	args, _ := json.Marshal(map[string]any{"message_id": m.ID, "body": body})
	out := f.srv.AgentToolDispatch(f.asOwner, f.chief)(replyDraftToolName, string(args))
	m2 := draftIDRE.FindStringSubmatch(out)
	if m2 == nil {
		t.Fatalf("reply_draft: %s", out)
	}
	id, _ := strconv.ParseInt(m2[1], 10, 64)
	return id
}

// routedDraft routes a message to routeSID and has that session answer it
// (route_reply): with no allowlist, the answer becomes a PENDING draft.
func (f *draftToolFixture) routedDraft(t *testing.T, external, body string) (int64, string) {
	t.Helper()
	_, m := f.pluginMessage(t, "example-inbox", external, "someone@example.test", true)
	rid := f.route(t, m)
	text, isErr := f.routeReplyCall(t, f.tok, routeSID, map[string]any{"route_id": rid, "body": body})
	var out struct {
		DraftID     int64  `json:"draft_id"`
		DraftStatus string `json:"draft_status"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || out.DraftStatus != "pending" || out.DraftID == 0 {
		t.Fatalf("route_reply: %s", text)
	}
	return out.DraftID, rid
}

// mcp calls a tool on /mirror/mcp as session sid of the host tok names.
func (f *draftToolFixture) mcp(t *testing.T, tok, sid, name string, args map[string]any) (string, bool) {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range mirrorAs(tok, sid) {
		h[k] = v
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall(name, args)), h)
	var r mcpReply
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.Error != nil {
		t.Fatalf("%s as %s: %d %s", name, sid, w.Code, w.Body)
	}
	return toolText(t, &r)
}

func (f *draftToolFixture) decide(t *testing.T, tok, sid string, id int64, decision, sum string) (string, bool) {
	t.Helper()
	return f.mcp(t, tok, sid, "plugin_draft_decide", map[string]any{"id": id, "decision": decision, "body_sha256": sum})
}

// listed is plugin_drafts_list as session sid of tok's host sees it.
func (f *draftToolFixture) listed(t *testing.T, tok, sid string) []map[string]any {
	t.Helper()
	text, isErr := f.mcp(t, tok, sid, "plugin_drafts_list", map[string]any{})
	var out struct {
		Drafts []map[string]any `json:"drafts"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || out.Total != len(out.Drafts) {
		t.Fatalf("plugin_drafts_list: %s", text)
	}
	return out.Drafts
}

// browser decides draft id on the Asks page as the owner, with a fresh sign-in.
func (f *draftToolFixture) browser(t *testing.T, id int64, how string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodPost, "/plugins/drafts/"+strconv.FormatInt(id, 10)+"/"+how, nil, map[string]string{
		"Cookie": sessionCookieFor(f.srv), "Origin": "http://example.com", "Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate"})
}

// draftRow is draft id's status, read straight from the row, and the
// decided_via of its newest decision audit row ("" when none names one).
type draftRow struct{ status, via string }

func (f *draftToolFixture) row(t *testing.T, id int64) draftRow {
	t.Helper()
	var r draftRow
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT d.status, coalesce((SELECT a.detail->>'decided_via'
		FROM audit_log a WHERE a.object = $2 AND a.action IN ('plugin.draft.approved', 'plugin.draft.rejected', 'plugin.draft.withdrawn')
		ORDER BY a.id DESC LIMIT 1), '') FROM plugin_reply_drafts d WHERE d.id = $1`, id, plugins.DraftObject(id)).
		Scan(&r.status, &r.via); err != nil {
		t.Fatalf("read draft %d: %v", id, err)
	}
	return r
}

// listedByID is the listing's draft id, or nil.
func listedByID(ds []map[string]any, id int64) map[string]any {
	for _, d := range ds {
		if int64(d["id"].(float64)) == id {
			return d
		}
	}
	return nil
}

// TestAMirroredSessionListsAndApprovesAReplyDraft is the feature: the chief
// drafts a reply to an UNVERIFIED sender; the owner's mirrored session lists it
// (sender marked UNVERIFIED, the message, the exact reply and its SHA-256) and
// approves it — with no fresh sign-in — and a plugin's claim that was already
// WAITING wakes with exactly that text at once — the approval's signal, not the
// claim's own poll, which this test lengthens to a minute — as it does for the
// Asks page's approval; the audit row says mcp from tower/<session>. The
// CONTROL is a second draft approved on the Asks page: decided_via 'browser' —
// so the provenance is the door's, not a constant. A rejected draft is not
// listed, and neither is an approved one.
func TestAMirroredSessionListsAndApprovesAReplyDraft(t *testing.T) {
	f := newDraftToolFixture(t)
	const body = "Yes, noon works."
	id := f.draft(t, "dt-approve", "Ada <ada@example.invalid>", false, body)
	ctl := f.draft(t, "dt-control", "Bob <bob@example.invalid>", true, "Control reply.")
	gone := f.draft(t, "dt-gone", "Cy <cy@example.invalid>", true, "Rejected on the page.")
	if w := f.browser(t, gone, "reject"); w.Code != http.StatusSeeOther {
		t.Fatalf("browser reject: %d %s", w.Code, w.Body)
	}

	ds := f.listed(t, f.tok, otherRouteSID)
	mine := listedByID(ds, id)
	if len(ds) != 2 || mine == nil || listedByID(ds, ctl) == nil || listedByID(ds, gone) != nil {
		t.Fatalf("plugin_drafts_list = %v, want exactly the two pending drafts", ds)
	}
	if mine["body"] != body || mine["body_sha256"] != sha(body) || mine["characters"] != float64(16) ||
		mine["sender"] != "Ada <ada@example.invalid> "+plugins.UnverifiedSenderMark || mine["sender_verified"] != false ||
		mine["message"] != "please do a thing" || mine["subject"] != "hello" || mine["plugin"] != "Example inbox" || mine["route"] != nil {
		t.Fatalf("the listed draft: %v", mine)
	}
	if c := listedByID(ds, ctl); c["sender"] != "Bob <bob@example.invalid>" || c["sender_verified"] != true {
		t.Fatalf("control: a VERIFIED sender is listed as %v", c["sender"])
	}

	// Only draftWake can wake the claim within the bound below: its own re-read
	// comes a minute later, past the claim's 20-second wait.
	prevPoll := claimPollInterval
	claimPollInterval = time.Minute
	t.Cleanup(func() { claimPollInterval = prevPoll })
	waited := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		waited <- f.call(t, http.MethodPost, "/plugin/v1/drafts/claim", strings.NewReader(`{"wait_seconds":20}`),
			map[string]string{"Authorization": "Bearer " + f.plugTok, "Content-Type": "application/json"})
	}()
	time.Sleep(300 * time.Millisecond) // the claim is waiting before the decision

	text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body))
	decided := time.Now()
	if isErr || !strings.Contains(text, `"status":"approved"`) || !strings.Contains(text, `"decided_from":"tower/`+otherRouteSID+`"`) {
		t.Fatalf("plugin_draft_decide approve: %s", text)
	}
	select {
	case w := <-waited:
		if took := time.Since(decided); took > 3*time.Second {
			t.Fatalf("the waiting claim answered %s after the approval; the approval did not wake it", took)
		}
		var c struct {
			DraftID int64  `json:"draft_id"`
			Body    string `json:"body"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &c) != nil || c.DraftID != id || c.Body != body {
			t.Fatalf("the waiting claim: %d %s", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting claim did not wake on the MCP approval within 3s (its own poll is a minute)")
	}
	if r := f.row(t, id); r != (draftRow{status: "approved", via: "mcp from tower/" + otherRouteSID}) {
		t.Fatalf("the approved draft: %+v", r)
	}

	// CONTROL: the Asks page approves the other one.
	if w := f.browser(t, ctl, "approve"); w.Code != http.StatusSeeOther {
		t.Fatalf("browser approve: %d %s", w.Code, w.Body)
	}
	if r := f.row(t, ctl); r != (draftRow{status: "approved", via: "browser"}) {
		t.Fatalf("the page's approval: %+v", r)
	}
	if r := f.row(t, gone); r != (draftRow{status: "rejected", via: "browser"}) {
		t.Fatalf("the page's rejection: %+v", r)
	}
	if left := f.listed(t, f.tok, otherRouteSID); len(left) != 0 {
		t.Fatalf("after the decisions plugin_drafts_list = %v", left)
	}
}

// TestAMirroredSessionRejectsAReplyDraft: reject discards it — rejected, from
// mcp, never claimable — and it leaves the listing.
func TestAMirroredSessionRejectsAReplyDraft(t *testing.T) {
	f := newDraftToolFixture(t)
	const body = "No thanks."
	id := f.draft(t, "dt-reject", "Ada <ada@example.invalid>", true, body)
	if text, isErr := f.decide(t, f.tok, routeSID, id, "reject", sha(body)); isErr || !strings.Contains(text, `"status":"rejected"`) {
		t.Fatalf("reject: %s", text)
	}
	if r := f.row(t, id); r != (draftRow{status: "rejected", via: "mcp from tower/" + routeSID}) {
		t.Fatalf("the rejected draft: %+v", r)
	}
	if _, ok, err := f.plugins.ClaimDraft(f.asOwner, f.plugin.ID); ok || err != nil {
		t.Fatalf("a rejected draft was claimable (%v %v)", ok, err)
	}
	if left := f.listed(t, f.tok, routeSID); len(left) != 0 {
		t.Fatalf("a rejected draft is still listed: %v", left)
	}
}

// TestPluginDraftDecideRefusesABodyItDidNotRead: a well-formed body_sha256 of
// any OTHER text is refused (the store's check, under the row lock), as is a
// malformed one or none (the tool's), and the draft stays pending, undecided,
// unclaimable. CONTROL: the right hash approves it.
func TestPluginDraftDecideRefusesABodyItDidNotRead(t *testing.T) {
	f := newDraftToolFixture(t)
	const body = "Noon works."
	id := f.draft(t, "dt-sha", "Ada <ada@example.invalid>", true, body)
	for _, d := range []string{"approve", "reject"} {
		text, isErr := f.decide(t, f.tok, otherRouteSID, id, d, sha(body+" "))
		if want := toolErr("draft %d: %s", id, "body_sha256 is not the SHA-256 of that draft's text: read the draft again and decide on the text it holds."); !isErr || text != want {
			t.Fatalf("%s with another text's hash: %s", d, text)
		}
	}
	for _, bad := range []string{"", strings.ToUpper(sha(body)), sha(body)[:63]} {
		if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", bad); !isErr || !strings.Contains(text, "body_sha256 must be the draft's body_sha256 exactly") {
			t.Fatalf("approve with body_sha256 %q: %s", bad, text)
		}
	}
	if r := f.row(t, id); r != (draftRow{status: "pending"}) {
		t.Fatalf("after refused decisions the draft is %+v", r)
	}
	if _, ok, _ := f.plugins.ClaimDraft(f.asOwner, f.plugin.ID); ok {
		t.Fatal("a draft refused on its hash was claimable")
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); isErr {
		t.Fatalf("control, the right hash: %s", text)
	}
}

// TestTheRoutedSessionMayDecideItsOwnReply: a message routed to routeSID,
// answered there, becomes a pending draft that names its route. Any of the
// owner's mirrored sessions may decide it — the routed one included (the
// accepted cost of no step-up): routeSID approves the reply it wrote. The Asks
// page decides a second routed draft as usual.
func TestTheRoutedSessionMayDecideItsOwnReply(t *testing.T) {
	f := newDraftToolFixture(t)
	const body = "I will look into it."
	id, rid := f.routedDraft(t, "dt-routed", body)

	d := listedByID(f.listed(t, f.tok, routeSID), id)
	if d == nil || d["route"] == nil {
		t.Fatalf("the routed draft as its target lists it: %v", d)
	}
	if r := d["route"].(map[string]any); r["route_id"] != rid || r["target_kind"] != "mirror" || r["target"] != routeSID || r["target_label"] != "work:1 @ tower" || len(r) != 4 {
		t.Fatalf("the routed draft's route: %v", r)
	}
	if text, isErr := f.decide(t, f.tok, routeSID, id, "approve", sha(body)); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("the route's target approving its own reply: %s", text)
	}
	if r := f.row(t, id); r != (draftRow{status: "approved", via: "mcp from tower/" + routeSID}) {
		t.Fatalf("the routed session's approval: %+v", r)
	}

	page, _ := f.routedDraft(t, "dt-routed-page", "Second answer.")
	if w := f.browser(t, page, "reject"); w.Code != http.StatusSeeOther {
		t.Fatalf("the Asks page on a routed draft: %d %s", w.Code, w.Body)
	}
	if r := f.row(t, page); r != (draftRow{status: "rejected", via: "browser"}) {
		t.Fatalf("the page's decision on a routed draft: %+v", r)
	}
}

// TestAnMCPDecisionWithdrawsTheDraftsNotification: the chief's draft sends the
// owner its `plugin` push; deciding it over MCP withdraws that notification
// (one `resolved` message with the draft's tag), exactly as the Asks page's
// decision does. CONTROL: before the decision there is none.
func TestAnMCPDecisionWithdrawsTheDraftsNotification(t *testing.T) {
	f := newDraftToolFixture(t)
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, f.owner, ep("dt-owner"))
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	const body = "See you then."
	id := f.draft(t, "dt-notify", "Ada <ada@example.invalid>", true, body)
	tag := "plugin-draft-" + strconv.FormatInt(id, 10)
	resolved := func() (announced, cleared int) {
		f.srv.WaitForPushes()
		for _, p := range pusher.to(ep("dt-owner")) {
			switch {
			case p.Type == "plugin" && p.Tag == tag:
				announced++
			case p.Type == push.TypeResolved && p.Tag == tag:
				cleared++
			}
		}
		return
	}
	if a, c := resolved(); a != 1 || c != 0 {
		t.Fatalf("control: before the decision the draft was announced %d time(s) and cleared %d; want 1 and 0", a, c)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "reject", sha(body)); isErr {
		t.Fatal(text)
	}
	if _, c := resolved(); c != 1 {
		t.Fatalf("the MCP decision cleared draft %d's notification %d time(s); want 1: %+v", id, c, pusher.to(ep("dt-owner")))
	}
}

// TestPluginDraftDecideRefusesADecidedDraft: an approved draft can be neither
// approved again nor rejected (a withdrawal is the Asks page's), a rejected one
// not approved, a missing id is "No such draft." — and none of them moves.
func TestPluginDraftDecideRefusesADecidedDraft(t *testing.T) {
	f := newDraftToolFixture(t)
	a := f.draft(t, "dt-dec-a", "Ada <ada@example.invalid>", true, "First.")
	if text, isErr := f.decide(t, f.tok, otherRouteSID, a, "approve", sha("First.")); isErr {
		t.Fatal(text)
	}
	for _, d := range []string{"approve", "reject"} {
		if text, isErr := f.decide(t, f.tok, routeSID, a, d, sha("First.")); !isErr ||
			text != toolErr("draft %d: That draft is no longer awaiting this decision (it is approved).", a) {
			t.Fatalf("%s of an approved draft: %s", d, text)
		}
	}
	if r := f.row(t, a); r != (draftRow{status: "approved", via: "mcp from tower/" + otherRouteSID}) {
		t.Fatalf("the approved draft after refused decisions: %+v", r)
	}
	r := f.draft(t, "dt-dec-r", "Ada <ada@example.invalid>", true, "Second.")
	if w := f.browser(t, r, "reject"); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, r, "approve", sha("Second.")); !isErr ||
		text != toolErr("draft %d: That draft is no longer awaiting this decision (it is rejected).", r) {
		t.Fatalf("approving a rejected draft: %s", text)
	}
	if got := f.row(t, r); got != (draftRow{status: "rejected", via: "browser"}) {
		t.Fatalf("the rejected draft after an MCP approve: %+v", got)
	}
	if text, _ := f.decide(t, f.tok, otherRouteSID, 9_000_000_123, "approve", sha("x")); text != toolErr("draft 9000000123: No such draft.") {
		t.Fatalf("a missing draft: %s", text)
	}
}

// TestOnlyTheOwnersMirroredSessionsDecideAReplyDraft: another member's mirror
// host (a host of the same name) neither sees nor decides the owner's draft —
// "No such draft." exactly as a missing id; no dispatched agent (the chief,
// which wrote it, and a worker) can list or decide one, through the gateway
// dispatcher or POST /mcp, nor is offered either tool; the handler refuses a
// non-mirrored caller however it got there; a session the host does not list
// runs nothing. The draft stays pending. CONTROL: the owner's session decides.
func TestOnlyTheOwnersMirroredSessionsDecideAReplyDraft(t *testing.T) {
	f := newDraftToolFixture(t)
	const body = "Mine."
	id := f.draft(t, "dt-owner", "Ada <ada@example.invalid>", true, body)

	if ds := f.listed(t, f.strangers, routeSID); len(ds) != 0 {
		t.Fatalf("another member's session lists %v", ds)
	}
	for _, d := range []string{"approve", "reject"} {
		if text, isErr := f.decide(t, f.strangers, routeSID, id, d, sha(body)); !isErr || text != toolErr("draft %d: No such draft.", id) {
			t.Fatalf("%s by another member's session: %s", d, text)
		}
	}
	args, _ := json.Marshal(map[string]any{"id": id, "decision": "approve", "body_sha256": sha(body)})
	for _, a := range []struct {
		name  string
		tok   string
		agent agents.Agent
	}{{"chief", f.chiefTok, f.chief}, {"worker", f.workerTok, f.worker}} {
		for tool, want := range map[string]string{
			"plugin_draft_decide": toolErr(pluginDraftMirrorOnly, "plugin_draft_decide"),
			"plugin_drafts_list":  toolErr(pluginDraftMirrorOnly, "plugin_drafts_list"),
		} {
			got := f.surfaces(t, a.agent, a.tok, tool, string(args))
			if got["gateway"] != want {
				t.Errorf("%s calling %s through the gateway dispatcher: %s", a.name, tool, got["gateway"])
			}
			if !strings.HasPrefix(got["mcp"], "rpc-error -32602 unknown tool") {
				t.Errorf("%s calling %s over POST /mcp: %s", a.name, tool, got["mcp"])
			}
			for _, d := range f.srv.AgentToolDefsFor(f.asOwner, a.agent) {
				if d.Name == tool {
					t.Errorf("%s is offered %s as a gateway tool", a.name, tool)
				}
			}
		}
	}
	if got := f.srv.toolPluginDraftDecide(f.asOwner, callerOfAgent(f.chief), string(args)); got != toolErr(pluginDraftMirrorOnly, "plugin_draft_decide") {
		t.Errorf("the handler, an agent caller: %s", got)
	}
	if got := f.srv.toolPluginDraftDecide(f.asOwner, toolCaller{asker: attention.Asker{System: true, MirrorHost: "tower", MirrorSession: otherRouteSID}}, string(args)); got != toolErr(pluginDraftMirrorOnly, "plugin_draft_decide") {
		t.Errorf("the handler, a system caller naming a session: %s", got)
	}
	if text, isErr := f.decide(t, f.tok, "5e55a1d0-0000-4000-8000-0000000dead0", id, "approve", sha(body)); !isErr || !strings.Contains(text, "lists no window running Claude session") {
		t.Errorf("a session the host does not list: %s", text)
	}
	if r := f.row(t, id); r != (draftRow{status: "pending"}) {
		t.Fatalf("after the refused callers the draft is %+v", r)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); isErr {
		t.Fatalf("control, the owner's session: %s", text)
	}
}

// TestRouteTargetMirrorIsFocusKindMirror pins the two spellings of a mirrored
// session's route target kind equal (an invariant guard: the route tool writes
// focus's, route_reply's binding reads plugins').
func TestRouteTargetMirrorIsFocusKindMirror(t *testing.T) {
	if plugins.RouteTargetMirror != string(focus.KindMirror) {
		t.Fatalf("plugins.RouteTargetMirror %q != focus.KindMirror %q", plugins.RouteTargetMirror, focus.KindMirror)
	}
}
