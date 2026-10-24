package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/recents"
)

// latestSessionPage is where a bare GET /agents/{name} would 303 for a: its
// page on its most recently updated session, read straight from the table and
// spelled literally (not through ui.AgentSessionHref, the code under test). A
// link or a tap must land THERE directly — the bare URL costs one more round
// trip.
func latestSessionPage(t *testing.T, pool *pgxpool.Pool, a agents.Agent) string {
	t.Helper()
	var id int64
	if err := pool.QueryRow(authz.System(context.Background()),
		`SELECT id FROM chat_sessions WHERE agent_id = $1 ORDER BY updated_at DESC, id DESC LIMIT 1`, a.ID).Scan(&id); err != nil {
		t.Fatalf("agent %s's latest session: %v", a.Name, err)
	}
	return "/agents/" + a.Name + "?session=" + strconv.FormatInt(id, 10)
}

// sessionsFor gives a two sessions and makes the OLDER (lower id) one the most
// recently updated, so "latest" (updated_at) and "newest row" (max id) name
// DIFFERENT sessions: a link built from the wrong order cannot pass. It returns
// the latest one's id.
func sessionsFor(t *testing.T, pool *pgxpool.Pool, a agents.Agent) int64 {
	t.Helper()
	st := agents.NewPG(pool)
	first, err := st.CreateSession(asUser(a.OwnerID), a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(asUser(a.OwnerID), a.ID, a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(authz.System(context.Background()),
		`UPDATE chat_sessions SET updated_at = now() + interval '1 hour' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	return first.ID
}

// anchorHrefs is the href of every <a …> in body whose opening tag carries
// marker, in order.
func anchorHrefs(body, marker string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<a [^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(m, marker) {
			continue
		}
		if h := regexp.MustCompile(`href="([^"]*)"`).FindStringSubmatch(m); h != nil {
			out = append(out, strings.ReplaceAll(h[1], "&amp;", "&"))
		}
	}
	return out
}

// TestEveryListLinkToAnAgentNamesItsLatestSession: on a real Postgres, every
// link the Agents list and the Tasks list draw to an agent — the card (its nav
// anchor and its long-title link), a pinned chip, a Recent entry, a task
// card's "Open chat" — is the canonical /agents/{name}?session=<latest>, the
// SAME URL the bare /agents/{name} 303s to. Red before ui.AgentSessionHref:
// every one of them was the bare URL, and each open paid the 303's round trip.
func TestEveryListLinkToAnAgentNamesItsLatestSession(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.Pins, ext.Recents = pins.NewPG(pool), recents.NewPG(pool)
	})
	sessionsFor(t, f.pool, f.op)
	sessionsFor(t, f.pool, f.op2)
	wantOp, wantOp2 := latestSessionPage(t, f.pool, f.op), latestSessionPage(t, f.pool, f.op2)
	if wantOp == wantOp2 {
		t.Fatal("instrument check FAILED: the two agents' pages are the same URL")
	}
	// The contract: the bare URL 303s to exactly the link.
	for _, c := range []struct {
		a    agents.Agent
		want string
	}{{f.op, wantOp}, {f.op2, wantOp2}} {
		w := f.call(t, http.MethodGet, "/agents/"+c.a.Name, nil, f.operator())
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.want {
			t.Fatalf("bare GET /agents/%s = %d → %q, want 303 → %q", c.a.Name, w.Code, w.Header().Get("Location"), c.want)
		}
	}
	// A visit puts op in the Recent strip; op2 is pinned (a chip, and so NOT a
	// Recent entry).
	if w := f.call(t, http.MethodGet, wantOp, nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", wantOp, w.Code)
	}
	if err := pins.NewPG(f.pool).PinAgent(asUser(1), f.op2.ID); err != nil {
		t.Fatal(err)
	}
	// The visit bumped nothing that moves "latest" (the open marks it read).
	if got := latestSessionPage(t, f.pool, f.op); got != wantOp {
		t.Fatalf("instrument check FAILED: opening the page moved op's latest session %q → %q", wantOp, got)
	}
	list := f.page(t, "/ui/agents")
	// A LIST link is the 303 target plus the from=list marker (ui.AgentListHref).
	listOp, listOp2 := wantOp+"&from=list", wantOp2+"&from=list"
	opCard := `href="` + strings.ReplaceAll(listOp, "&", "&amp;") + `"`
	if !strings.Contains(list, opCard) {
		t.Errorf("op's card does not link %s; its links: %v", listOp, anchorHrefs(list, "/agents/"+f.op.Name))
	}
	if got := anchorHrefs(list, "data-pinned-chip-link"); len(got) != 1 || got[0] != listOp2 {
		t.Errorf("pinned chip links %v, want [%s]", got, listOp2)
	}
	if got := anchorHrefs(list, `data-recent-entry="agent"`); len(got) != 1 || got[0] != listOp {
		t.Errorf("Recent entries link %v, want [%s]", got, listOp)
	}
	// No anchor anywhere on the list links either agent's BARE page.
	for _, a := range []agents.Agent{f.op, f.op2} {
		if strings.Contains(list, `href="/agents/`+a.Name+`"`) {
			t.Errorf("the list still links the bare /agents/%s", a.Name)
		}
	}

	// The Tasks list: a task whose dispatched agent is op opens op's session.
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "href task", Body: "href task", Status: notes.StatusInProgress})
	if err != nil {
		t.Fatal(err)
	}
	sys := authz.System(context.Background())
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `UPDATE agents SET note_id = NULL WHERE id = $1`, f.op.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	if _, err := f.pool.Exec(sys, `UPDATE agents SET note_id = $1 WHERE id = $2`, n.ID, f.op.ID); err != nil {
		t.Fatal(err)
	}
	tasks := f.page(t, "/ui/tasks")
	if got := anchorHrefs(tasks, `aria-label="Open agent chat"`); len(got) == 0 {
		t.Errorf("instrument check FAILED: no Open chat link on the Tasks list")
	} else {
		for _, h := range got {
			if h != listOp {
				t.Errorf("a task card's Open chat links %q, want %q", h, listOp)
			}
		}
	}
}

// TestAStaleListLinkLandsOnTheLatestSession: a list link names the session that
// was latest when the list was DRAWN. The bare URL it replaced resolved the
// latest at TAP time, so a list left open while another session became the
// latest must still land there: a list link (from=list) to a session that is no
// longer the latest 303s to the latest; a fresh one renders at once (one hop);
// a link WITHOUT the marker — the session drawer's — means its session and
// renders it. Red before the marker: the stale list link rendered the old
// session.
func TestAStaleListLinkLandsOnTheLatestSession(t *testing.T) {
	f := newAttnFixture(t)
	first := sessionsFor(t, f.pool, f.op)
	list := f.page(t, "/ui/agents")
	links := anchorHrefs(list, `data-card-slot="body"`)
	var link string
	for _, h := range links {
		if strings.HasPrefix(h, "/agents/"+f.op.Name+"?") {
			link = h
		}
	}
	want := "/agents/" + f.op.Name + "?session=" + strconv.FormatInt(first, 10)
	if !strings.HasPrefix(link, want) {
		t.Fatalf("instrument check FAILED: op's card links %q (all: %v), want the session %d", link, links, first)
	}
	// Fresh: one hop, the page itself.
	if w := f.call(t, http.MethodGet, link, nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("a fresh list link = %d (→ %q), want 200", w.Code, w.Header().Get("Location"))
	}
	// Another session becomes the latest while the list is still on screen.
	var other int64
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT id FROM chat_sessions WHERE agent_id = $1 AND id <> $2 ORDER BY id DESC LIMIT 1`, f.op.ID, first).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE chat_sessions SET updated_at = now() + interval '2 hours' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	latest := latestSessionPage(t, f.pool, f.op)
	if latest != "/agents/"+f.op.Name+"?session="+strconv.FormatInt(other, 10) {
		t.Fatalf("instrument check FAILED: the latest is %q, want session %d", latest, other)
	}
	w := f.call(t, http.MethodGet, link, nil, f.operator())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != latest {
		t.Fatalf("a STALE list link = %d → %q, want 303 → %q (where the bare URL lands now)", w.Code, w.Header().Get("Location"), latest)
	}
	// The drawer's link to the older session (no marker) still opens it.
	if w := f.call(t, http.MethodGet, want, nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("a session link without the list marker = %d (→ %q), want 200: it means that session", w.Code, w.Header().Get("Location"))
	}
}

// chatLogVersionOf is the data-chat-version the agent page's #chat-log carries.
func chatLogVersionOf(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`<div id="chat-log"[^>]*\sdata-chat-version="([0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page's #chat-log carries no data-chat-version")
	}
	return m[1]
}

// TestTheChatLogRefetchIsAnsweredOnlyWhenItChanged: the agent page's #chat-log
// re-fetches its partial when its event stream opens (and on every chat.reply),
// and it used to download the whole transcript again each time. With the
// version handshake (ui.ChatLogBody): the version the page was rendered with
// gets a 204 and no body while nothing changed, and a full 200 with a NEW
// version once a message was stored — so the page still converges. Red before
// the handshake: every refetch was a 200 with the full body.
func TestTheChatLogRefetchIsAnsweredOnlyWhenItChanged(t *testing.T) {
	f := newAttnFixture(t)
	sid := sessionsFor(t, f.pool, f.op)
	st := agents.NewPG(f.pool)
	if _, err := st.AddChatMessage(asUser(1), agents.ChatMessage{AgentID: f.op.ID, SessionID: sid, Role: "user", Content: "first synthetic message"}); err != nil {
		t.Fatal(err)
	}
	page := "/agents/" + f.op.Name + "?session=" + strconv.FormatInt(sid, 10)
	partial := "/ui/agents/" + f.op.Name + "/chat-log?session=" + strconv.FormatInt(sid, 10)
	ver := chatLogVersionOf(t, f.page(t, page))
	refetch := func(v string) (int, string, string) {
		hdr := f.operator()
		if v != "" {
			hdr = merge(hdr, map[string]string{"Muster-Chat-Log-Version": v})
		}
		w := f.call(t, http.MethodGet, partial, nil, hdr)
		return w.Code, w.Body.String(), w.Header().Get("Muster-Chat-Log-Version")
	}
	// Control: no version sent → the full transcript, carrying the SAME version
	// the page was rendered with (the page and the partial agree on "unchanged").
	code, body, got := refetch("")
	if code != http.StatusOK || !strings.Contains(body, "first synthetic message") {
		t.Fatalf("control: GET %s = %d without the message: %.200s", partial, code, body)
	}
	if got != ver {
		t.Fatalf("the partial's version %q is not the page's %q for the same transcript", got, ver)
	}
	// Unchanged → 204, no body.
	if code, body, _ := refetch(ver); code != http.StatusNoContent || body != "" {
		t.Fatalf("an unchanged refetch = %d with %d bytes, want 204 and none", code, len(body))
	}
	// A stale or unknown version → the full body.
	if code, _, _ := refetch("0123456789abcdef01234567"); code != http.StatusOK {
		t.Fatalf("a refetch with a version that is not the current one = %d, want 200", code)
	}
	// A message arrives while the page is open → 200 with it, and a new version.
	time.Sleep(5 * time.Millisecond)
	if _, err := st.AddChatMessage(asUser(1), agents.ChatMessage{AgentID: f.op.ID, SessionID: sid, Role: "assistant", Content: "second synthetic reply"}); err != nil {
		t.Fatal(err)
	}
	code, body, next := refetch(ver)
	if code != http.StatusOK || !strings.Contains(body, "second synthetic reply") {
		t.Fatalf("a refetch after a new message = %d without it: %.200s", code, body)
	}
	if next == "" || next == ver {
		t.Fatalf("the changed transcript's version %q, want a new one (was %q)", next, ver)
	}
	// …and that new version is unchanged from now on.
	if code, _, _ := refetch(next); code != http.StatusNoContent {
		t.Fatalf("a refetch with the new version = %d, want 204", code)
	}
	// A status move changes what the partial draws (the provisioning row), so it
	// changes the version too, with no message at all.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'provisioning' WHERE id = $1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := refetch(next); code != http.StatusOK {
		t.Fatalf("a refetch after a status move = %d, want 200 (the provisioning row is new)", code)
	}
}
