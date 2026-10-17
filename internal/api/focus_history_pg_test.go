package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// GET /ui/focus/history against a real Postgres: the operator's (user 1's)
// browser session reads its own sessions' history a page at a time, and gets
// a 404 — never an empty 200 — for a session that is another owner's or
// nobody's.

type historyFixture struct {
	*connFixture
	store  *focus.PGStore
	member int64
}

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
	h := &historyFixture{}
	h.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		h.store = focus.NewPG(pool)
		ext.Focus = h.store
	})
	sys := authz.System(context.Background())
	if err := h.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"hist-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&h.member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM agent_focus_history WHERE owner_id = $1`, `DELETE FROM agent_focus WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = h.pool.Exec(sys, q, h.member)
		}
		_, _ = h.pool.Exec(sys, `DELETE FROM agent_focus WHERE owner_id = 1 AND subject_key LIKE 'hist%'`)
		_, _ = h.pool.Exec(sys, `DELETE FROM agent_focus_history WHERE owner_id = 1 AND subject_key LIKE 'hist%'`)
	})
	return h
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func (h *historyFixture) agent(t *testing.T, uid int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(h.pool).Create(asUser(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, Status: agents.StatusRunning,
		HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	return a
}

func (h *historyFixture) focusN(t *testing.T, uid int64, sub focus.Subject, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := h.store.Upsert(asUser(uid), sub, focus.Write{Title: fmt.Sprintf("hist focus %02d", i), Source: focus.SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *historyFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	w := h.call(t, http.MethodGet, path, nil, h.operator())
	return w.Code, w.Body.String()
}

var historyTitle = regexp.MustCompile(`data-focus-history-title=""[^>]*>([^<]*)<`)
var historyNext = regexp.MustCompile(`data-focus-history-more="" data-href="([^"]*)"`)

func TestFocusHistoryRoutePagesTheCallersOwnHistory(t *testing.T) {
	h := newHistoryFixture(t)
	mine := focus.AgentSubject(h.agent(t, 1, "hist-op-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID)
	h.focusN(t, 1, mine, focus.DefaultHistoryPage+5)

	code, body := h.get(t, ui.FocusHistoryPath(mine.String(), ""))
	if code != http.StatusOK {
		t.Fatalf("own agent: %d %s", code, body)
	}
	first := historyTitle.FindAllStringSubmatch(body, -1)
	if len(first) != focus.DefaultHistoryPage || first[0][1] != "hist focus 25" {
		t.Fatalf("first page: %d rows %v\n%s", len(first), first, body)
	}
	m := historyNext.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the first page names no next page:\n%s", body)
	}
	code, body = h.get(t, strings.ReplaceAll(m[1], "&amp;", "&"))
	rest := historyTitle.FindAllStringSubmatch(body, -1)
	if code != http.StatusOK || len(rest) != 5 || rest[0][1] != "hist focus 05" || rest[4][1] != "hist focus 01" {
		t.Fatalf("second page: %d, %v", code, rest)
	}
	if historyNext.MatchString(body) || !strings.Contains(body, "data-focus-history-end") {
		t.Fatal("the last page does not end the list")
	}

	// A mirrored session the operator holds a focus for: its history.
	sid := focus.MirrorSubject("hist-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	h.focusN(t, 1, sid, 2)
	if code, body := h.get(t, ui.FocusHistoryPath(sid.String(), "")); code != http.StatusOK || len(historyTitle.FindAllString(body, -1)) != 2 {
		t.Fatalf("own mirrored session: %d %s", code, body)
	}
	// Its focus row pruned (a week unseen): the history outlives it (0022), and
	// is still the caller's to read.
	if _, err := h.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE owner_id = 1 AND subject_key = $1`, sid.Key); err != nil {
		t.Fatal(err)
	}
	if code, body := h.get(t, ui.FocusHistoryPath(sid.String(), "")); code != http.StatusOK || len(historyTitle.FindAllString(body, -1)) != 2 {
		t.Fatalf("own mirrored session, focus row pruned: %d %s", code, body)
	}
	// An agent of the caller's with no history yet: 200, the empty state.
	empty := focus.AgentSubject(h.agent(t, 1, "hist-empty-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID)
	if code, body := h.get(t, ui.FocusHistoryPath(empty.String(), "")); code != http.StatusOK || !strings.Contains(body, "data-focus-history-empty") {
		t.Fatalf("own agent, no history: %d %s", code, body)
	}
}

// TestFocusHistoryRouteIs404ForAnotherOwnersSession: the member's agent and
// mirrored session — each WITH history, so an empty answer would be the
// row-level security alone — are a 404 to the operator, the same as a session
// that does not exist; a malformed subject or cursor is a 400.
func TestFocusHistoryRouteIs404ForAnotherOwnersSession(t *testing.T) {
	h := newHistoryFixture(t)
	theirs := focus.AgentSubject(h.agent(t, h.member, "hist-mem-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID)
	theirSID := focus.MirrorSubject("histm-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	h.focusN(t, h.member, theirs, 2)
	h.focusN(t, h.member, theirSID, 2)
	// Control: the member's history exists and is readable — as the member.
	if p, err := h.store.History(asUser(h.member), theirs, "", 10); err != nil || len(p.Entries) != 2 {
		t.Fatalf("control: the member reads %d rows (%v), want 2", len(p.Entries), err)
	}
	// The member's other mirrored session: history only, its focus row pruned.
	theirPruned := focus.MirrorSubject("histp-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	h.focusN(t, h.member, theirPruned, 2)
	if _, err := h.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, theirPruned.Key); err != nil {
		t.Fatal(err)
	}
	for name, sub := range map[string]string{
		"another owner's pruned session":   theirPruned.String(),
		"another owner's agent":            theirs.String(),
		"another owner's mirrored session": theirSID.String(),
		"no such agent":                    "agent:987654321",
		"no such mirrored session":         "mirror:hist-nobody",
	} {
		if code, body := h.get(t, ui.FocusHistoryPath(sub, "")); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404\n%s", name, code, body)
		}
	}
	for name, path := range map[string]string{
		"no subject":  ui.FocusHistoryRoute,
		"bad subject": ui.FocusHistoryRoute + "?subject=agent:x",
		"bad cursor":  ui.FocusHistoryPath(focus.AgentSubject(h.agent(t, 1, "hist-cur-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID).String(), "nope"),
	} {
		if code, _ := h.get(t, path); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
}
