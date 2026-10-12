package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAnAgentsFlowRequestRunsOnlyAfterItsOwnerApprovesAndOnlyOnTheOwnersRunner
// drives the agent door end to end through the real routes and Postgres:
//
//	member A's agent asks over /mcp (its own token, the connect surface)
//	-> nothing is queued; the admin's session and member B's cannot decide it
//	-> A approves on /flows -> exactly one job, owned by A
//	-> the admin's runner and B's runner (same host label) claim nothing;
//	   A's runner claims it; the agent's browser_flow_status shows the job.
//
// A second request A DENIES never produces a job, and the agent sees "denied".
func TestAnAgentsFlowRequestRunsOnlyAfterItsOwnerApprovesAndOnlyOnTheOwnersRunner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var a, b int64
	for _, id := range []*int64{&a, &b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "fr"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	asUser := func(uid int64) context.Context {
		role := authz.RoleMember
		if uid == authz.OperatorUserID {
			role = authz.RoleAdmin
		}
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
	}
	as := agents.NewPG(pool)
	agentTok := "flowreq-agent-token-" + suffix
	_, err = as.Create(asUser(a), agents.Agent{Name: "frag" + suffix, Namespace: "ns-frag" + suffix, HooksToken: agentTok,
		Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	fs := flows.NewPG(pool)
	host := "lap" + suffix
	runner := map[int64]string{}
	for _, uid := range []int64{a, b, authz.OperatorUserID} {
		tok, _, err := fs.CreateRunnerToken(asUser(uid), host)
		if err != nil {
			t.Fatal(err)
		}
		runner[uid] = tok
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		ids := []int64{a, b, authz.OperatorUserID}
		for _, q := range []string{`DELETE FROM flow_requests WHERE owner_id = ANY($1)`, `DELETE FROM flow_jobs WHERE owner_id = ANY($1)`,
			`DELETE FROM flow_runner_tokens WHERE owner_id = ANY($1)`, `DELETE FROM agents WHERE owner_id = ANY($1) AND name LIKE 'frag%'`} {
			_, _ = pool.Exec(bg, q, ids)
		}
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = ANY($1)`, []int64{a, b})
	})

	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "flow-request-operator-pw"}, log.New(io.Discard, "", 0))
	// /mcp is the connect surface; it is armed only with a connector store.
	cs, err := connectors.NewPG(pool, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv.UseExtensions(Extensions{Agents: as, Users: users.NewPG(pool), Flows: fs, Connectors: cs})
	const userHeader = "X-FlowReq-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = "muster.test"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	session := func(uid int64) map[string]string {
		return map[string]string{"Cookie": sessionCookieFor(srv), "Origin": "http://muster.test",
			userHeader: strconv.FormatInt(uid, 10), "Content-Type": "application/x-www-form-urlencoded"}
	}
	tool := func(name, args string) map[string]any {
		t.Helper()
		rec := do("POST", "/mcp", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`,
			map[string]string{"X-Muster-Token": agentTok, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
		var rep mcpReply
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rep) != nil || rep.Error != nil {
			t.Fatalf("tools/call %s: %d %s", name, rec.Code, rec.Body)
		}
		text, _ := toolText(t, &rep)
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s result is not JSON: %s", name, text)
		}
		return out
	}
	reqOf := func(v map[string]any) map[string]any { m, _ := v["request"].(map[string]any); return m }
	claim := func(uid int64) int {
		return do("POST", "/runner/flows/claim", `{"instances":["personal"],"waitSeconds":1}`,
			map[string]string{"Authorization": "Bearer " + runner[uid], "Content-Type": "application/json"}).Code
	}
	jobs := func() int {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM flow_jobs WHERE owner_id = ANY($1)`, []int64{a, b, authz.OperatorUserID}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 1. The agent asks. Nothing is queued.
	v := tool("browser_flow_request", fmt.Sprintf(`{"flow":"muse.poll","host":%q,"instance":"personal","params":{"thread":"Side chat"},"reason":"check the reply"}`, host))
	q := reqOf(v)
	if q == nil || q["status"] != "pending" || q["ownerId"] != float64(a) {
		t.Fatalf("browser_flow_request = %v", v)
	}
	id := strconv.FormatInt(int64(q["id"].(float64)), 10)
	if n := jobs(); n != 0 {
		t.Fatalf("%d jobs exist before the owner decided", n)
	}
	if s := reqOf(tool("browser_flow_status", `{"request_id":`+id+`}`)); s["status"] != "pending" {
		t.Fatalf("status before a decision = %v", s)
	}

	// 2. Neither the admin nor member B can decide it; the agent's own token
	// cannot reach the decide route at all.
	for _, uid := range []int64{authz.OperatorUserID, b} {
		if rec := do("POST", "/flows/requests/"+id+"/approve", "", session(uid)); rec.Code != http.StatusNotFound {
			t.Fatalf("user %d approving A's agent's request: %d; want 404", uid, rec.Code)
		}
	}
	if rec := do("POST", "/flows/requests/"+id+"/approve", "", map[string]string{"X-Muster-Token": agentTok}); rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
		t.Fatalf("the AGENT's own token decided its request: %d", rec.Code)
	}
	if n := jobs(); n != 0 {
		t.Fatalf("%d jobs exist after refused decisions", n)
	}

	// 3. A sees it on /flows and approves: one job, A's.
	if page := do("GET", "/flows", "", session(a)).Body.String(); !strings.Contains(page, "check the reply") || !strings.Contains(page, `data-flow-request-id="`+id+`"`) {
		t.Fatal("A's /flows page does not show the pending request")
	}
	if page := do("GET", "/flows", "", session(b)).Body.String(); strings.Contains(page, "check the reply") {
		t.Fatal("B's /flows page shows A's agent's request")
	}
	if rec := do("POST", "/flows/requests/"+id+"/approve", "", session(a)); rec.Code != http.StatusSeeOther {
		t.Fatalf("A approving: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	if n := jobs(); n != 1 {
		t.Fatalf("%d jobs after the approval; want 1", n)
	}

	// 4. Only A's runner can claim it.
	for _, uid := range []int64{authz.OperatorUserID, b} {
		if code := claim(uid); code != http.StatusNoContent {
			t.Fatalf("user %d's runner (same host) claim: %d; want 204", uid, code)
		}
	}
	if code := claim(a); code != http.StatusOK {
		t.Fatalf("A's runner claim: %d; want 200", code)
	}
	st := tool("browser_flow_status", `{"request_id":`+id+`}`)
	job, _ := st["job"].(map[string]any)
	if reqOf(st)["status"] != "approved" || job == nil || job["status"] != "running" || job["ownerId"] != float64(a) {
		t.Fatalf("status after approval and claim = %v", st)
	}

	// 5. A denied request never produces a job.
	v = tool("browser_flow_request", fmt.Sprintf(`{"flow":"muse.poll","host":%q,"instance":"personal"}`, host))
	id2 := strconv.FormatInt(int64(reqOf(v)["id"].(float64)), 10)
	if rec := do("POST", "/flows/requests/"+id2+"/deny", "", session(a)); rec.Code != http.StatusSeeOther {
		t.Fatalf("A denying: %d", rec.Code)
	}
	if rec := do("POST", "/flows/requests/"+id2+"/approve", "", session(a)); rec.Code != http.StatusConflict {
		t.Fatalf("approving a denied request: %d; want 409", rec.Code)
	}
	if s := tool("browser_flow_status", `{"request_id":`+id2+`}`); reqOf(s)["status"] != "denied" || s["job"] != nil {
		t.Fatalf("status of the denied request = %v", s)
	}
	if n := jobs(); n != 1 {
		t.Fatalf("%d jobs after the deny; want still 1", n)
	}

	// 6. The screen on agent-composed text, and the per-agent cap.
	if v := tool("browser_flow_request", fmt.Sprintf(`{"flow":"muse.send","host":%q,"instance":"personal","params":{"text":"ssh to 192.0.2.7"}}`, host)); v["error"] == nil {
		t.Fatalf("agent text with an IP was filed: %v", v)
	}
	for i := 0; i < flows.MaxPendingRequestsPerAgent; i++ {
		tool("browser_flow_request", fmt.Sprintf(`{"flow":"muse.poll","host":%q,"instance":"personal"}`, host))
	}
	if v := tool("browser_flow_request", fmt.Sprintf(`{"flow":"muse.poll","host":%q,"instance":"personal"}`, host)); v["error"] == nil ||
		!strings.Contains(v["error"].(string), "too many pending") {
		t.Fatalf("a request past the per-agent cap = %v", v)
	}
}
