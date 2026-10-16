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

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// pluginTaskAnswer is the route's JSON answer.
type pluginTaskAnswer struct {
	ID        *int64 `json:"id"`
	Created   bool   `json:"created"`
	Commented bool   `json:"commented"`
}

// TestThePluginTaskRouteFindsOrCreatesOneTaskPerKey drives POST /plugin/v1/tasks
// through the real routes, wrappers and Postgres stores, with plugin tokens minted
// on /plugins:
//
//   - create → 201, a task carrying exactly the key, ungated and not auto;
//   - the same create again (a retry, or a second notice for the same upstream
//     record) → 200, the SAME id, still one task, and its body unchanged;
//   - a comment for that key → appended once; the same comment again → not;
//   - create + comment on an existing key → the comment, no second task;
//   - a comment for a key nobody filed → nothing written, id null;
//   - another member's plugin, same key → its OWN task; its comment-only call
//     for a key only A holds → id null, A's thread untouched;
//   - keys outside the closed namespace set, and the hook token, are refused.
func TestThePluginTaskRouteFindsOrCreatesOneTaskPerKey(t *testing.T) {
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
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "ptk"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ids := []int64{a, b}
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = ANY($1)`, `DELETE FROM plugins WHERE owner_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})

	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-task-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Agents: agents.NewPG(pool), Users: users.NewPG(pool), Plugins: plugins.NewPG(pool), Notes: notes.NewPG(pool)})
	const userHeader = "X-Plugin-Test-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "muster.test"
		if strings.HasPrefix(body, "{") {
			r.Header.Set("Content-Type", "application/json")
		} else if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		set(r)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	session := func(uid int64) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Cookie", sessionCookieFor(srv))
			r.Header.Set("Origin", "http://muster.test")
			r.Header.Set(userHeader, strconv.FormatInt(uid, 10))
		}
	}
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	mint := func(uid int64) string {
		t.Helper()
		rec := do("POST", "/plugins", url.Values{"name": {"example-inbox"}, "label": {"Example inbox"},
			"data_scope": {"example-inbox"}}.Encode(), session(uid))
		m := mintedPluginRE.FindStringSubmatch(rec.Body.String())
		if rec.Code != http.StatusOK || m == nil {
			t.Fatalf("mint for user %d: %d", uid, rec.Code)
		}
		return m[1]
	}
	aTok, bTok := mint(a), mint(b)
	post := func(tok string, body map[string]any) (int, pluginTaskAnswer, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := do("POST", "/plugin/v1/tasks", string(raw), bearer(tok))
		var out pluginTaskAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	asA := authz.WithPrincipal(ctx, authz.Principal{UserID: a, Role: authz.RoleMember, Via: authz.ViaSession})
	asB := authz.WithPrincipal(ctx, authz.Principal{UserID: b, Role: authz.RoleMember, Via: authz.ViaSession})
	ns := notes.NewPG(pool)
	tasksFor := func(c context.Context, key string) []notes.Note {
		t.Helper()
		got, err := ns.ListByTags(c, []string{key})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Unconfigured, the route holds the plugin off (503) rather than refusing (400).
	if code, _, raw := post(aTok, map[string]any{"key": "tracker:t" + suffix, "comment": "x"}); code != http.StatusServiceUnavailable {
		t.Fatalf("no namespaces configured: %d %s; want 503", code, raw)
	}
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })

	key := "tracker:t" + suffix
	create := map[string]any{"key": key, "create": map[string]any{"title": "Drain a node", "body": "first body"}}
	code, first, raw := post(aTok, create)
	if code != http.StatusCreated || first.ID == nil || !first.Created || first.Commented {
		t.Fatalf("create: %d %s; want 201 {id, created:true, commented:false}", code, raw)
	}
	task, err := ns.Get(asA, *first.ID)
	if err != nil || len(task.Tags) != 1 || task.Tags[0] != key || notes.IsGated(task.Tags) || notes.HasAutoDispatch(task.Tags) ||
		task.Title != "Drain a node" || task.Body != "first body" || task.OwnerID != a {
		t.Fatalf("the created task %+v (%v); want A's, tagged exactly [%s], ungated, not auto, with the given title and body", task, err, key)
	}

	again := map[string]any{"key": key, "create": map[string]any{"title": "Drain a node", "body": "second body"}}
	if code, out, raw := post(aTok, again); code != http.StatusOK || out.ID == nil || *out.ID != *first.ID || out.Created {
		t.Fatalf("create again: %d %s; want 200 with id %d, created:false", code, raw, *first.ID)
	}
	if got := tasksFor(asA, key); len(got) != 1 || got[0].Body != "first body" {
		t.Fatalf("after a repeat create: %d task(s) for %s (body %q); want one, unchanged", len(got), key, got[0].Body)
	}

	comment := map[string]any{"key": key, "comment": "[status] to do → in progress (upstream message 41)"}
	if code, out, raw := post(aTok, comment); code != http.StatusOK || out.ID == nil || *out.ID != *first.ID || !out.Commented {
		t.Fatalf("comment: %d %s; want 200 commented:true on %d", code, raw, *first.ID)
	}
	if code, out, raw := post(aTok, comment); code != http.StatusOK || out.Commented {
		t.Fatalf("the same comment again: %d %s; want 200 commented:false", code, raw)
	}
	both := map[string]any{"key": key, "create": map[string]any{"body": "x"}, "comment": "[assigned] again (upstream message 42)"}
	if code, out, raw := post(aTok, both); code != http.StatusOK || out.Created || !out.Commented || *out.ID != *first.ID {
		t.Fatalf("create+comment on an existing key: %d %s; want 200 created:false commented:true", code, raw)
	}
	cs, err := ns.ListComments(asA, *first.ID)
	if err != nil || len(cs) != 2 || cs[0].Author != "plugin:example-inbox" || cs[1].Body != "[assigned] again (upstream message 42)" {
		t.Fatalf("A's thread: %+v (%v); want the two distinct plugin comments, in order", cs, err)
	}

	stray := "tracker:none" + suffix
	if code, out, raw := post(aTok, map[string]any{"key": stray, "comment": "[status] x"}); code != http.StatusOK || out.ID != nil || out.Created || out.Commented {
		t.Fatalf("a comment for a key nobody filed: %d %s; want 200 {id:null}", code, raw)
	}
	if got := tasksFor(asA, stray); len(got) != 0 {
		t.Fatalf("a comment-only call created %d task(s)", len(got))
	}

	// Member B: the same key is B's own task; B cannot reach A's.
	if code, out, raw := post(bTok, map[string]any{"key": key, "comment": "[status] from B (upstream message 9)"}); code != http.StatusOK || out.ID != nil {
		t.Fatalf("B's comment-only call on a key only A holds: %d %s; want id null", code, raw)
	}
	code, bOut, raw := post(bTok, create)
	if code != http.StatusCreated || bOut.ID == nil || *bOut.ID == *first.ID {
		t.Fatalf("B's create with A's key: %d %s; want 201 with B's own task", code, raw)
	}
	if got := tasksFor(asB, key); len(got) != 1 || got[0].OwnerID != b {
		t.Fatalf("B sees %d task(s) for the key; want exactly B's one", len(got))
	}
	if cs, _ := ns.ListComments(asA, *first.ID); len(cs) != 2 {
		t.Fatalf("A's thread has %d comments after B's calls; want still 2", len(cs))
	}

	for _, bad := range []map[string]any{
		{"key": "gate:approved", "create": map[string]any{"body": "x"}},
		{"key": "auto:dispatch", "create": map[string]any{"body": "x"}},
		{"key": "clickup:abc", "create": map[string]any{"body": "x"}},
		{"key": "other:abc", "comment": "x"},
		{"key": "superseded-by:1", "comment": "x"},
		{"key": "TRACKER:abc", "comment": "x"},
		{"key": "tracker:", "comment": "x"},
		{"key": "tracker:a:b", "comment": "x"},
		{"key": key},
		{"key": key, "create": map[string]any{"body": " "}},
		{"key": key, "comment": "nul\x00"},
		{"key": key, "comment": "x", "extra": true},
	} {
		if code, _, raw := post(aTok, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s; want 400", bad, code, raw)
		}
	}
	if rec := do("POST", "/plugin/v1/tasks", `{"key":"`+key+`","comment":"x"}`, bearer(tokTestHook)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the hook token on the task route: %d; want 401", rec.Code)
	}
	if got := tasksFor(asA, key); len(got) != 1 {
		t.Fatalf("after every refusal: %d tasks for the key; want still 1", len(got))
	}
}
