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

// ptFixture is one member with a minted plugin token on a real server: the
// real routes, wrappers and Postgres stores, namespaces configured to `tracker`.
type ptFixture struct {
	t       *testing.T
	h       http.Handler
	srv     *Server
	tok     string
	uid     int64
	asOwner context.Context
	notes   *notes.PGStore
	suffix  string
}

func newPTFixture(t *testing.T) *ptFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &ptFixture{t: t, notes: notes.NewPG(pool), suffix: strconv.FormatInt(time.Now().UnixNano(), 36)}
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"ptr1"+f.suffix).Scan(&f.uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(sys, q, f.uid)
		}
	})
	f.asOwner = authz.WithPrincipal(ctx, authz.Principal{UserID: f.uid, Role: authz.RoleMember, Via: authz.ViaSession})
	f.srv = New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-task-r1-operator-pw"}, log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Agents: agents.NewPG(pool), Users: users.NewPG(pool), Plugins: plugins.NewPG(pool), Notes: f.notes})
	f.srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get("X-Plugin-Test-User"), 10, 64)
		return v
	}
	f.h = f.srv.Handler()
	rec := f.do("POST", "/plugins", url.Values{"name": {"example-inbox"}, "label": {"Example inbox"},
		"data_scope": {"example-inbox"}}.Encode(), f.session())
	m := mintedPluginRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("mint: %d", rec.Code)
	}
	f.tok = m[1]
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	return f
}

func (f *ptFixture) do(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	if strings.HasPrefix(body, "{") {
		r.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	set(r)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func (f *ptFixture) session() func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(f.srv))
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set("X-Plugin-Test-User", strconv.FormatInt(f.uid, 10))
	}
}

func (f *ptFixture) post(body map[string]any) (int, pluginTaskAnswer, string) {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	rec := f.do("POST", "/plugin/v1/tasks", string(raw), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tok) })
	var out pluginTaskAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func (f *ptFixture) comments(id int64) []notes.Comment {
	f.t.Helper()
	cs, err := f.notes.ListComments(f.asOwner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return cs
}

func (f *ptFixture) live(key string) []notes.Note {
	f.t.Helper()
	ns, err := f.notes.ListByTags(f.asOwner, []string{key})
	if err != nil {
		f.t.Fatal(err)
	}
	return ns
}

// M1: the SAME {key, create, comment} posted twice — a plugin resending after
// a lost answer — is ONE task with ONE comment. The create path records the
// comment in the create's own transaction, so the resend finds it and dedupes.
func TestPluginTaskCreateWithCommentIsIdempotent(t *testing.T) {
	f := newPTFixture(t)
	key := "tracker:m1" + f.suffix
	req := map[string]any{"key": key, "create": map[string]any{"title": "t", "body": "the body"}, "comment": "assigned (upstream message 7)"}
	code, first, raw := f.post(req)
	if code != http.StatusCreated || first.ID == nil || !first.Created || !first.Commented {
		t.Fatalf("first: %d %s; want 201 created:true commented:true", code, raw)
	}
	if cs := f.comments(*first.ID); len(cs) != 1 || cs[0].Body != "assigned (upstream message 7)" || cs[0].Author != "plugin:example-inbox" {
		t.Fatalf("after the first post the thread is %+v; want the one comment", cs)
	}
	code, again, raw := f.post(req)
	if code != http.StatusOK || again.ID == nil || *again.ID != *first.ID || again.Created || again.Commented {
		t.Fatalf("resend: %d %s; want 200 same id, created:false commented:false", code, raw)
	}
	if n := len(f.live(key)); n != 1 {
		t.Fatalf("%d tasks carry the key; want 1", n)
	}
	if cs := f.comments(*first.ID); len(cs) != 1 {
		t.Fatalf("after the resend the thread has %d comments; want 1", len(cs))
	}
}

// M2: a task that is complete, or the LOSER of a merge (complete and
// superseded-by:), is not the open task for its key. After a real merge the
// next notice goes to the winner (it carries the key: the merge unions tags);
// once the winner no longer carries it, a create-worthy notice files a fresh
// task; a comment-only notice with no open task writes nothing.
func TestPluginTaskOnlyAnOpenTaskIsTheTaskForAKey(t *testing.T) {
	f := newPTFixture(t)
	key := "tracker:m2" + f.suffix
	_, a, _ := f.post(map[string]any{"key": key, "create": map[string]any{"body": "plugin task"}})
	winner, err := f.notes.Create(f.asOwner, notes.Note{Body: "a newer task the owner keeps"})
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do("POST", "/tasks/merge", url.Values{"winner": {strconv.FormatInt(winner.ID, 10)},
		"loser": {strconv.FormatInt(*a.ID, 10)}}.Encode(), f.session())
	if rec.Code >= 400 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	if loser, _ := f.notes.Get(f.asOwner, *a.ID); loser.Status != "complete" {
		t.Fatalf("CONTROL: the merge left the loser %q; want complete", loser.Status)
	}
	code, out, raw := f.post(map[string]any{"key": key, "comment": "after the merge (upstream message 2)"})
	if code != http.StatusOK || out.ID == nil || *out.ID != winner.ID || !out.Commented {
		t.Fatalf("a notice after the merge: %d %s; want it on the winner %d", code, raw, winner.ID)
	}
	if _, err := f.notes.RemoveTags(f.asOwner, winner.ID, []string{key}); err != nil {
		t.Fatal(err)
	}
	if code, out, raw := f.post(map[string]any{"key": key, "comment": "nobody open (upstream message 3)"}); code != http.StatusOK || out.ID != nil {
		t.Fatalf("comment-only with no open task: %d %s; want id null", code, raw)
	}
	code, fresh, raw := f.post(map[string]any{"key": key, "create": map[string]any{"body": "again"}})
	if code != http.StatusCreated || fresh.ID == nil || *fresh.ID == *a.ID || *fresh.ID == winner.ID {
		t.Fatalf("create after the winner dropped the key: %d %s; want a fresh task", code, raw)
	}
	// A COMPLETED task (no merge): the next create files a new one, comment-only writes nothing.
	if _, err := f.notes.SetStatus(f.asOwner, *fresh.ID, "complete"); err != nil {
		t.Fatal(err)
	}
	if code, out, raw := f.post(map[string]any{"key": key, "comment": "late (upstream message 4)"}); code != http.StatusOK || out.ID != nil {
		t.Fatalf("comment-only after completion: %d %s; want id null", code, raw)
	}
	if cs := f.comments(*fresh.ID); len(cs) != 0 {
		t.Fatalf("the completed task gained %d comment(s)", len(cs))
	}
	code, next, raw := f.post(map[string]any{"key": key, "create": map[string]any{"body": "reopened upstream"}})
	if code != http.StatusCreated || next.ID == nil || *next.ID == *fresh.ID {
		t.Fatalf("create after completion: %d %s; want a new task", code, raw)
	}
}

// M3: a comment is trimmed, and whitespace-only is no comment.
func TestPluginTaskTrimsAndTruncates(t *testing.T) {
	f := newPTFixture(t)
	key := "tracker:m3" + f.suffix
	code, out, raw := f.post(map[string]any{"key": key, "create": map[string]any{"body": "b"}, "comment": " \n\t "})
	if code != http.StatusCreated || out.Commented {
		t.Fatalf("create + whitespace comment: %d %s; want 201 commented:false", code, raw)
	}
	if cs := f.comments(*out.ID); len(cs) != 0 {
		t.Fatalf("a whitespace-only comment was stored: %+v", cs)
	}
	if code, _, raw := f.post(map[string]any{"key": key, "comment": "   "}); code != http.StatusBadRequest {
		t.Fatalf("whitespace-only comment and no create: %d %s; want 400 nothing to do", code, raw)
	}
	if code, o, raw := f.post(map[string]any{"key": key, "comment": "  padded  "}); code != http.StatusOK || !o.Commented {
		t.Fatalf("padded comment: %d %s", code, raw)
	}
	if code, o, raw := f.post(map[string]any{"key": key, "comment": "padded"}); code != http.StatusOK || o.Commented {
		t.Fatalf("the same comment unpadded: %d %s; want a duplicate (trimmed before the check)", code, raw)
	}
	if cs := f.comments(*out.ID); len(cs) != 1 || cs[0].Body != "padded" {
		t.Fatalf("thread %+v; want one trimmed comment", cs)
	}
}

// M4: an oversize body, title or comment is TRUNCATED, never refused.
func TestPluginTaskTruncatesOversize(t *testing.T) {
	f := newPTFixture(t)
	big := strings.Repeat("é", plugins.MaxBodyBytes) // 2 bytes each: twice the cap
	key2 := "tracker:m4" + f.suffix
	code, out, raw := f.post(map[string]any{"key": key2, "create": map[string]any{"title": strings.Repeat("t", 2000), "body": big}})
	if code != http.StatusCreated {
		t.Fatalf("oversize body and title: %d %.200s; want 201 (truncated)", code, raw)
	}
	n, _ := f.notes.Get(f.asOwner, *out.ID)
	if n.Body != strings.Repeat("é", plugins.MaxBodyBytes/2) || n.Title != strings.Repeat("t", maxTaskFieldLen) {
		t.Fatalf("stored body %d bytes, title %d runes; want %d bytes on a rune boundary and %d runes",
			len(n.Body), len([]rune(n.Title)), plugins.MaxBodyBytes, maxTaskFieldLen)
	}
	if code, o, raw := f.post(map[string]any{"key": key2, "comment": big}); code != http.StatusOK || !o.Commented {
		t.Fatalf("oversize comment: %d %.200s; want 200 commented (truncated)", code, raw)
	}
	if cs := f.comments(*out.ID); len(cs) != 1 || cs[0].Body != strings.Repeat("é", plugins.MaxBodyBytes/2) {
		t.Fatalf("stored comment %d bytes; want %d", len(cs[0].Body), plugins.MaxBodyBytes)
	}
}
