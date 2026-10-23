package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/sse"
)

// connector_requests_list / connector_request_decide (connector_decide_tools.go,
// migration 0035), over a real Postgres: the operator decides a pending
// connector request through one of their own MIRRORED Claude Code sessions
// (POST /mirror/mcp) — the same decision path the Connectors tab's request card
// takes, with where it came from recorded and shown — and nobody else can: not a
// dispatched agent (the requester included), not the chief, not a hook, agent or
// chief token on the mirror door, not another owner's host.
//
// ⚠ THESE TESTS NAME ONLY IDENTIFIERS THAT EXIST BEFORE THE FEATURE (the tool
// names and refusals are string literals; provenance is read with SQL), so the
// file also compiles against the pre-feature tree and each test is red there
// for a BEHAVIOURAL reason — not merely because it does not build.

// decideFixture is answerFixture (user 1's host "ws" listing focusSID and
// secondSID, user 1's "laptop", a member's host also named "ws"; user 1's
// agents op and op2 and chief; the member's worker) plus a stored connector
// "svc" of user 1's and an SSE bus, so a decision's nudge can be observed.
type decideFixture struct {
	*answerFixture
	bus *sse.Broadcaster
}

func newDecideFixture(t *testing.T) *decideFixture {
	t.Helper()
	f := &decideFixture{answerFixture: newAnswerFixture(t), bus: sse.New(64)}
	f.srv.bus = f.bus
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	return f
}

// request files agent token tok's request for connector name over POST /mcp,
// exactly as an agent does (connector_request), and returns its id.
func (f *decideFixture) request(t *testing.T, tok, name, reason string) int64 {
	t.Helper()
	text, isErr, rpc := f.tool(t, tok, "connector_request", map[string]any{"name": name, "reason": reason})
	if isErr || rpc != nil {
		t.Fatalf("connector_request %s: %s %+v", name, text, rpc)
	}
	var v struct {
		Grant   string `json:"grant"`
		GrantID int64  `json:"grantId"`
	}
	if err := json.Unmarshal([]byte(text), &v); err != nil || v.Grant != "pending" || v.GrantID == 0 {
		t.Fatalf("connector_request %s: %s", name, text)
	}
	return v.GrantID
}

// mirrorTool calls name through /mirror/mcp as token's host and session sid:
// the tool's text and whether it is a tool error.
func (f *decideFixture) mirrorTool(t *testing.T, token, sid, name string, args map[string]any) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall(name, args))
	if code != http.StatusOK || r == nil || r.Error != nil {
		t.Fatalf("%s as %s: HTTP %d %s", name, sid, code, body)
	}
	return toolText(t, r)
}

func (f *decideFixture) decide(t *testing.T, token, sid string, id int64, decision string) (string, bool) {
	t.Helper()
	return f.mirrorTool(t, token, sid, "connector_request_decide", map[string]any{"id": id, "decision": decision})
}

// grantRow is grant id read straight from the row ("" for NULL).
type grantRow struct{ status, by, source, via, host, session string }

func (f *decideFixture) grantRow(t *testing.T, id int64) grantRow {
	t.Helper()
	var g grantRow
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT status, decided_by, source,
		coalesce(decided_via, ''), coalesce(decided_via_host, ''), coalesce(decided_via_session, '')
		FROM connector_grants WHERE id = $1`, id).Scan(&g.status, &g.by, &g.source, &g.via, &g.host, &g.session); err != nil {
		t.Fatalf("read grant %d: %v", id, err)
	}
	return g
}

// auditDetails is every grant audit line about agent agentName, oldest first.
func (f *decideFixture) auditDetails(t *testing.T, agentName string) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT detail FROM connector_audit
		WHERE kind = 'grant' AND agent_name = $1 ORDER BY id`, agentName)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// listedRequests is connector_requests_list as session sid of token's host sees it.
func (f *decideFixture) listedRequests(t *testing.T, token, sid string) []map[string]any {
	t.Helper()
	text, isErr := f.mirrorTool(t, token, sid, "connector_requests_list", map[string]any{})
	var out struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil {
		t.Fatalf("connector_requests_list: %s", text)
	}
	return out.Requests
}

// nextConnectorEvent is the next connector.changed event the operator's (user
// 1's) streams receive within a second, or "".
func nextConnectorEvent(ch <-chan sse.Event) string {
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Name == "connector.changed" {
				return ev.Data
			}
		case <-deadline:
			return ""
		}
	}
}

// TestAMirroredSessionListsAndApprovesAConnectorRequest is the feature: an
// agent asks for a connector over its own MCP door and blocks on
// connector_status; the operator's mirrored session lists the request (agent,
// task, connector, hosts, reason) and approves it; the grant is approved BY THE
// OPERATOR with its provenance (mcp, ws, that session) on the row and on the
// audit line, the operator's Connectors tab is nudged and shows where it came
// from, and the agent's own wait wakes approved with its usage — exactly as for
// a browser approval. The CONTROL is a second agent's request approved on the
// tab: the same grant, audit verb and nudge, and no provenance anywhere — so
// the provenance is the MCP door's, not a constant.
func TestAMirroredSessionListsAndApprovesAConnectorRequest(t *testing.T) {
	f := newDecideFixture(t)
	// The control asks for ANOTHER connector: an approval of svc is shared
	// (0043), so a second svc request would be approved by the first decision.
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc2", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	id := f.request(t, f.opTok, "svc", "to read the widget API")
	ctl := f.request(t, f.op2Tok, "svc2", "control")

	reqs := f.listedRequests(t, f.ws, focusSID)
	var mine map[string]any
	for _, r := range reqs {
		if int64(r["id"].(float64)) == id {
			mine = r
		}
	}
	if len(reqs) != 2 || mine == nil {
		t.Fatalf("connector_requests_list = %v, want both pending requests", reqs)
	}
	if mine["agent"] != f.op.Name || int64(mine["agent_id"].(float64)) != f.op.ID || mine["connector"] != "svc" ||
		mine["reason"] != "to read the widget API" || mine["connector_exists"] != true ||
		!strings.Contains(mine["approving_grants"].(string), "api.example.com") {
		t.Fatalf("the listed request: %v", mine)
	}

	_, ch := f.bus.Subscribe(1)
	waited := make(chan string, 1)
	go func() {
		text, _, _ := f.tool(t, f.opTok, "connector_status", map[string]any{"name": "svc", "wait_seconds": 20})
		waited <- text
	}()
	time.Sleep(200 * time.Millisecond) // the wait is in flight before the decision

	text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
	if isErr || !strings.Contains(text, `"status":"approved"`) || !strings.Contains(text, `"decided_from":"ws/`+focusSID+`"`) {
		t.Fatalf("connector_request_decide approve: %s", text)
	}
	if g := f.grantRow(t, id); g != (grantRow{status: "approved", by: "operator", source: "stored", via: "mcp", host: "ws", session: focusSID}) {
		t.Fatalf("the approved grant: %+v", g)
	}
	if got := nextConnectorEvent(ch); got != `{"id":"`+strconv.FormatInt(id, 10)+`"}` {
		t.Fatalf("the operator's tab was nudged with %q", got)
	}
	select {
	case w := <-waited:
		if !strings.Contains(w, `"grant":"approved"`) || !strings.Contains(w, "/connect/svc/api.example.com/") {
			t.Fatalf("the agent's connector_status wait woke with %s", w)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's connector_status wait did not wake on the MCP approval")
	}
	if got := f.auditDetails(t, f.op.Name); len(got) != 3 || got[0] != "requested" ||
		got[1] != "approved via MCP from ws/"+focusSID+" (host proven by its mirror token; session as named by the caller)" ||
		got[2] != "approval shared as standing grant 1: later requests for svc from any of your agents are approved automatically (approved via MCP from ws/"+focusSID+")" {
		t.Fatalf("the requester's audit lines: %q", got)
	}

	// CONTROL: the tab approves the other agent's request.
	if w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(ctl, 10)+"/approve", nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("control approve: %d %s", w.Code, w.Body)
	}
	if g := f.grantRow(t, ctl); g != (grantRow{status: "approved", by: "operator", source: "stored"}) {
		t.Fatalf("the tab's grant: %+v", g)
	}
	if got := nextConnectorEvent(ch); got != `{"id":"`+strconv.FormatInt(ctl, 10)+`"}` {
		t.Fatalf("control: the tab's approval nudged %q", got)
	}
	if got := f.auditDetails(t, f.op2.Name); len(got) != 3 || got[1] != "approved" || !strings.HasPrefix(got[2], "approval shared as standing grant 2: ") {
		t.Fatalf("control: the tab's audit lines: %q", got)
	}

	// The tab shows where each came from: the MCP one names ws/<session>, the
	// tab's names nothing.
	w := f.call(t, http.MethodGet, "/ui/connectors", nil, f.operator())
	page := w.Body.String()
	if !strings.Contains(page, `data-grant-via="ws/`+focusSID+`"`) || strings.Count(page, "data-grant-via=") != 1 ||
		!strings.Contains(page, "approved via MCP from ws/"+focusSID) {
		t.Fatalf("the Connectors tab does not show the MCP approval's source exactly once (%d):\n%s", w.Code, page)
	}
	if left := f.listedRequests(t, f.ws, focusSID); len(left) != 0 {
		t.Fatalf("after both decisions connector_requests_list = %v", left)
	}
}

// TestAMirroredSessionDeniesAConnectorRequest: deny is the tab's deny — the
// grant is denied with its provenance, the agent's status reads denied, and it
// is no longer listed.
func TestAMirroredSessionDeniesAConnectorRequest(t *testing.T) {
	f := newDecideFixture(t)
	id := f.request(t, f.opTok, "svc", "please")
	text, isErr := f.decide(t, f.ws, secondSID, id, "deny")
	if isErr || !strings.Contains(text, `"status":"denied"`) {
		t.Fatalf("deny: %s", text)
	}
	if g := f.grantRow(t, id); g != (grantRow{status: "denied", by: "operator", source: "stored", via: "mcp", host: "ws", session: secondSID}) {
		t.Fatalf("the denied grant: %+v", g)
	}
	if st, _, _ := f.tool(t, f.opTok, "connector_status", map[string]any{"name": "svc"}); !strings.Contains(st, `"grant":"denied"`) || strings.Contains(st, "usage") {
		t.Fatalf("the agent's status after the deny: %s", st)
	}
	if got := f.auditDetails(t, f.op.Name); len(got) != 2 || !strings.HasPrefix(got[1], "denied via MCP from ws/"+secondSID) {
		t.Fatalf("audit: %q", got)
	}
	if left := f.listedRequests(t, f.ws, focusSID); len(left) != 0 {
		t.Fatalf("a denied request is still listed: %v", left)
	}
}

// TestConnectorRequestDecideRefusesDecidedForeignMissingAndBadRequests: a
// request already approved or denied ("no longer pending (it is …)"), another
// owner's (row-level security: the same words as a missing id, and it stays
// pending and unlisted), a missing one, a request for a connector that does not
// exist (its answer is a credential, never entered over MCP) and a decision that
// is neither approve nor deny.
func TestConnectorRequestDecideRefusesDecidedForeignMissingAndBadRequests(t *testing.T) {
	f := newDecideFixture(t)
	approved := f.request(t, f.opTok, "svc", "")
	if text, isErr := f.decide(t, f.ws, focusSID, approved, "approve"); isErr {
		t.Fatal(text)
	}
	for _, d := range []string{"approve", "deny"} {
		if text, isErr := f.decide(t, f.ws, secondSID, approved, d); !isErr || text != `{"error":"connector request `+strconv.FormatInt(approved, 10)+` is no longer pending (it is approved)"}` {
			t.Fatalf("%s of an approved request: %s", d, text)
		}
	}
	// Another connector: op's approval of svc above is shared (0043).
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc2", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	denied := f.request(t, f.op2Tok, "svc2", "")
	if w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(denied, 10)+"/deny", nil, f.operator()); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if text, _ := f.decide(t, f.ws, focusSID, denied, "approve"); text != `{"error":"connector request `+strconv.FormatInt(denied, 10)+` is no longer pending (it is denied)"}` {
		t.Fatalf("approving a denied request: %s", text)
	}
	if g := f.grantRow(t, denied); g.status != "denied" || g.via != "" {
		t.Fatalf("the tab-denied request after an MCP approve attempt: %+v", g)
	}

	theirs := f.request(t, f.workerTok, "svc", "the member's") // f.member's worker
	for _, d := range []string{"approve", "deny"} {
		if text, _ := f.decide(t, f.ws, focusSID, theirs, d); text != `{"error":"no such connector request: `+strconv.FormatInt(theirs, 10)+`"}` {
			t.Fatalf("%s of another owner's request: %s", d, text)
		}
	}
	if g := f.grantRow(t, theirs); g.status != "pending" {
		t.Fatalf("another owner's request is %+v; want pending", g)
	}
	for _, r := range f.listedRequests(t, f.ws, focusSID) {
		if int64(r["id"].(float64)) == theirs {
			t.Fatalf("another owner's request is listed: %v", r)
		}
	}
	if text, _ := f.decide(t, f.ws, focusSID, 9_000_000_123, "approve"); text != `{"error":"no such connector request: 9000000123"}` {
		t.Fatalf("a missing request: %s", text)
	}

	ghost := f.request(t, f.op2Tok, "ghost", "a connector nobody configured")
	if text, isErr := f.decide(t, f.ws, focusSID, ghost, "approve"); !isErr || !strings.Contains(text, "connector ghost does not exist, so request "+strconv.FormatInt(ghost, 10)+" cannot be approved over MCP") {
		t.Fatalf("approving a request for a missing connector: %s", text)
	}
	if g := f.grantRow(t, ghost); g.status != "pending" {
		t.Fatalf("the missing connector's request is %+v; want pending", g)
	}
	if text, isErr := f.decide(t, f.ws, focusSID, ghost, "maybe"); !isErr || !strings.Contains(text, `decision must be \"approve\" or \"deny\"`) {
		t.Fatalf("a decision that is neither: %s", text)
	}
	if g := f.grantRow(t, ghost); g.status != "pending" {
		t.Fatalf("after a bad decision the request is %+v", g)
	}
}

// TestOnlyAMirroredSessionDecidesAConnectorRequest: no dispatched agent — the
// requester itself, another of the owner's, the owner's chief — can list or
// decide a request: not through the gateway dispatcher (which runs ANY name a
// runtime hands it), not over POST /mcp (unlisted, so unknown), and neither tool
// is offered on either list; the hook token, an agent's and the chief's token
// are 401 on the mirror door. The request stays pending. CONTROL: the mirrored
// session approves it.
func TestOnlyAMirroredSessionDecidesAConnectorRequest(t *testing.T) {
	f := newDecideFixture(t)
	id := f.request(t, f.opTok, "svc", "let me in")
	args := `{"id":` + strconv.FormatInt(id, 10) + `,"decision":"approve"}`
	for name, a := range map[string]struct{ tok string }{"the requester": {f.opTok}, "another worker": {f.op2Tok}, "the chief": {f.chiefTok}} {
		ag := f.chief
		switch a.tok {
		case f.opTok:
			ag = f.op
		case f.op2Tok:
			ag = f.op2
		}
		for tool, want := range map[string]string{
			"connector_request_decide": `{"error":"connector_request_decide is available only to your owner's own mirrored Claude Code sessions; only your owner decides a connector request"}`,
			"connector_requests_list":  `{"error":"connector_requests_list is available only to your owner's own mirrored Claude Code sessions; only your owner decides a connector request"}`,
		} {
			if got := f.srv.dispatchAgentTool(asUser(1), ag, tool, args); got != want {
				t.Errorf("%s calling %s through the gateway dispatcher: %s", name, tool, got)
			}
			if _, _, rpc := f.tool(t, a.tok, tool, map[string]any{"id": id, "decision": "approve"}); rpc == nil || rpc.Code != -32602 {
				t.Errorf("%s calling %s over POST /mcp: %+v, want unknown tool", name, tool, rpc)
			}
			if f.listed(t, a.tok)[tool] {
				t.Errorf("%s is offered %s on /mcp", name, tool)
			}
			for _, d := range f.srv.AgentToolDefsFor(asUser(1), ag) {
				if d.Name == tool {
					t.Errorf("%s is offered %s as a gateway tool", name, tool)
				}
			}
		}
	}
	for name, tok := range map[string]string{"the hook token": connTestHookToken, "the chief's token": f.chiefTok, "an agent's token": f.opTok} {
		if code, _, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), mcpToolCall("connector_request_decide", map[string]any{"id": id, "decision": "approve"})); code != http.StatusUnauthorized {
			t.Errorf("%s on /mirror/mcp: %d %s, want 401", name, code, body)
		}
	}
	// A host acts only as its own sessions: ws naming laptop's session, or a
	// session no window shows, runs nothing.
	for _, sid := range []string{laptopSID, "9f8e7d6c-5b4a-4392-8a1b-0c9d8e7f6a5b"} {
		if text, isErr := f.decide(t, f.ws, sid, id, "approve"); !isErr || !strings.Contains(text, "lists no window running Claude session") {
			t.Errorf("ws as session %s: %s", sid, text)
		}
	}
	if g := f.grantRow(t, id); g.status != "pending" {
		t.Fatalf("the request is %+v after the refused callers; want pending", g)
	}
	// CONTROL: the mirrored session approves it.
	if text, isErr := f.decide(t, f.ws, focusSID, id, "approve"); isErr {
		t.Fatalf("control: %s", text)
	}
}

// TestAnMCPApprovalCanStillBeRevokedOnTheTab: a revocation is made on the tab
// and replaces the decision, so it clears the MCP provenance in the same
// statement (0035's CHECK refuses a revoked row that keeps it); the audit
// trail keeps where the approval came from.
func TestAnMCPApprovalCanStillBeRevokedOnTheTab(t *testing.T) {
	f := newDecideFixture(t)
	id := f.request(t, f.opTok, "svc", "")
	if text, isErr := f.decide(t, f.ws, focusSID, id, "approve"); isErr {
		t.Fatal(text)
	}
	w := f.call(t, http.MethodDelete, "/connector-grants/"+strconv.FormatInt(id, 10), nil, f.operator())
	if g := f.grantRow(t, id); w.Code != http.StatusOK || g != (grantRow{status: "revoked", by: "operator", source: "stored"}) {
		t.Fatalf("revoke of an MCP approval: %d %+v %s", w.Code, g, w.Body)
	}
	// got[2] is the approval being shared (0043); the revoke here is of this
	// agent's grant alone.
	if got := f.auditDetails(t, f.op.Name); len(got) != 4 || !strings.HasPrefix(got[1], "approved via MCP from ws/") ||
		!strings.HasPrefix(got[2], "approval shared as standing grant ") || got[3] != "revoked" {
		t.Fatalf("audit: %q", got)
	}
}

// TestConnectorRequestsListIsBounded: 55 pending requests (11 agents at the
// per-agent cap of 5) list as 50, with the total and truncated
// saying so.
func TestConnectorRequestsListIsBounded(t *testing.T) {
	f := newDecideFixture(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	for i := 0; i < 11; i++ {
		tok := "bound-agent-token-" + strconv.Itoa(i) + "-" + suffix
		f.agentFor(t, 1, "bound"+strconv.Itoa(i)+"-"+suffix, "", tok)
		for j := 0; j < 5; j++ {
			f.request(t, tok, "svc"+strconv.Itoa(j), "")
		}
	}
	text, isErr := f.mirrorTool(t, f.ws, focusSID, "connector_requests_list", map[string]any{})
	var out struct {
		Requests  []map[string]any `json:"requests"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || len(out.Requests) != 50 || out.Total != 55 || !out.Truncated {
		t.Fatalf("55 pending requests listed as %d (total %d, truncated %v): %.300s", len(out.Requests), out.Total, out.Truncated, text)
	}
}
