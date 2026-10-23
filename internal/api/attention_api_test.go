package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAHumanOnlyApprovalGatesAMuseDispatchEndToEnd (migration 0045), over the
// real routes and Postgres:
//   - the personal token raises a human-only ask (201): the server's digest of
//     the payload, the two options, and a phone push like any other ask;
//   - another user cannot read it; the hook token cannot raise one;
//   - a dispatch is refused (403) before the ask is answered, and after it is
//     answered on the /attention VIEW (a browser session) it is queued (201);
//   - the same approval a second time, and a different text, are refused 403;
//   - a plain muse.send with no marker is queued without any approval.
func TestAHumanOnlyApprovalGatesAMuseDispatchEndToEnd(t *testing.T) {
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
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "hoapi"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	asUser := func(uid int64) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	}
	ts := apitokens.NewPG(pool)
	aTok, _, err := ts.Create(asUser(a), "dispatch", 0)
	if err != nil {
		t.Fatal(err)
	}
	bTok, _, err := ts.Create(asUser(b), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := []int64{a, b}
		for _, q := range []string{`DELETE FROM flow_jobs WHERE owner_id = ANY($1)`, `DELETE FROM attention_asks WHERE owner_id = ANY($1)`,
			`DELETE FROM api_tokens WHERE user_id = ANY($1)`, `DELETE FROM audit_log WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, a, ep("ho-phone"))
	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "ho-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Users: users.NewPG(pool), APITokens: ts, Flows: flows.NewPG(pool),
		Attention: attention.NewPG(pool),
		Push:      push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	const userHeader = "X-Ho-Test-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body, bearer string, session int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = "muster.test"
		if session > 0 {
			r.Header.Set("Cookie", sessionCookieFor(srv))
			r.Header.Set("Origin", "http://muster.test")
			r.Header.Set(userHeader, strconv.FormatInt(session, 10))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	const text = "[dispatch md-c0ffee]\n\nSummarise the reading list.\n"
	ask := func(payload string) attentionAskView {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"title": "Approve this muse dispatch?", "payload": payload, "timeoutSeconds": 600})
		rec := do("POST", "/api/attention/asks", string(body), aTok, 0)
		var v attentionAskView
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("raise an approval: %d %s", rec.Code, rec.Body)
		}
		return v
	}
	v := ask(text)
	if !v.HumanOnly || v.Status != "open" || v.Approved || v.PayloadSHA256 != attention.PayloadSHA256(text) {
		t.Fatalf("the raised approval = %+v", v)
	}
	// The phone is notified like any other ask (summary only on the lock screen).
	srv.WaitForPushes()
	got := pusher.to(ep("ho-phone"))
	if len(got) != 1 || got[0].Type != attentionNotificationType || got[0].Body != "api token (dispatch) is asking" ||
		got[0].Data["url"] != "/attention#"+attentionAskKey(v.ID) {
		t.Fatalf("pushes = %+v; want one attention push for the approval", got)
	}
	// Doors: the hook token cannot raise one; another user cannot read it.
	if rec := do("POST", "/api/attention/asks", `{"title":"x","payload":"y"}`, tokTestHook, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hook token raising an approval: %d; want 401", rec.Code)
	}
	if rec := do("GET", "/api/attention/asks/"+strconv.FormatInt(v.ID, 10), "", bTok, 0); rec.Code != http.StatusNotFound {
		t.Fatalf("another user reading the approval: %d; want 404", rec.Code)
	}

	job := func(t2 string, approval int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"flow": "muse.send", "host": "laptop", "instance": "personal", "approval": approval,
			"params": map[string]string{"text": t2, "session": "3f2a9c1e-8b4d-4e7a-9c3b-1d2e3f4a5b6c", "marker": "md-c0ffee", "wait": "917"}})
		return do("POST", "/api/flows/jobs", string(body), aTok, 0)
	}
	if rec := job(text, v.ID); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not been answered yet") {
		t.Fatalf("a dispatch before the approval: %d %s; want 403", rec.Code, rec.Body)
	}
	// The person approves on the /attention view (the human surface).
	if rec := do("POST", "/attention/asks/"+strconv.FormatInt(v.ID, 10)+"/answer", url.Values{"option": {attention.ApproveAnswer}}.Encode(), "", a); rec.Code >= 400 {
		t.Fatalf("approve on the view: %d %s", rec.Code, rec.Body)
	}
	var after attentionAskView
	if rec := do("GET", "/api/attention/asks/"+strconv.FormatInt(v.ID, 10), "", aTok, 0); rec.Code != http.StatusOK ||
		json.Unmarshal(rec.Body.Bytes(), &after) != nil || !after.Approved {
		t.Fatalf("after approval the ask reads %d %+v; want approved", rec.Code, after)
	}
	if rec := job(text+" (edited)", v.ID); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not the text that was approved") {
		t.Fatalf("a changed brief: %d %s; want 403", rec.Code, rec.Body)
	}
	if rec := job(text, v.ID); rec.Code != http.StatusCreated {
		t.Fatalf("the approved dispatch: %d %s; want 201", rec.Code, rec.Body)
	}
	if rec := job(text, v.ID); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "already queued a job") {
		t.Fatalf("the approval a second time: %d %s; want 403", rec.Code, rec.Body)
	}
	// A plain send needs nothing.
	if rec := do("POST", "/api/flows/jobs", `{"flow":"muse.send","host":"laptop","instance":"personal","params":{"text":"hi"}}`, aTok, 0); rec.Code != http.StatusCreated {
		t.Fatalf("a plain send: %d %s; want 201", rec.Code, rec.Body)
	}
}
