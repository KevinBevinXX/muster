package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// RETRYING A PLUGIN MESSAGE THE CHIEF NEVER CONFIRMED (plugin_retry.go,
// internal/plugins/retry.go), on a real Postgres.

// asSession is uid as a signed-in browser — the principal the /plugins forms run
// as (the plane's own owner context is the plugin door's).
func asSession(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// unconfirmed stores a message for the plane's plugin and settles it the way a
// cut-off delivery turn does: delivering to the chief, then unconfirmed.
func (pp *pluginPlane) unconfirmed(t *testing.T, ext string) plugins.Message {
	t.Helper()
	m, _, err := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: ext, Sender: "someone@example.test", Subject: "hello", Body: "Are you free at noon?"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pp.ps.BeginDelivery(pp.owner, m.ID, pp.chief.ID); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	if ok, err := pp.ps.RecordDelivery(pp.owner, m.ID, pp.chief.ID, plugins.StatusUnconfirmed); err != nil || !ok {
		t.Fatalf("settle unconfirmed: %v %v", ok, err)
	}
	m, _ = pp.ps.Message(pp.owner, m.ID)
	return m
}

// TestRetryDeliversAnUnconfirmedMessageAndTheChiefDrafts: an unconfirmed message,
// retried, goes back through the real deliverer to the CURRENT chief — the
// runtime is called, the chief's reply_draft lands as a pending draft for that
// message, the message settles delivered to the chief, and the attempt is
// counted. Its card leaves the page's retry list.
func TestRetryDeliversAnUnconfirmedMessageAndTheChiefDrafts(t *testing.T) {
	rt := &scriptedRuntime{name: replyDraftToolName}
	pp := newPluginPlane(t, rt)
	m := pp.unconfirmed(t, "retry-1")
	if list, _ := pp.ps.Retryable(pp.owner, 10); len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("control: the unconfirmed message is not listed retryable: %+v", list)
	}
	args, _ := json.Marshal(map[string]any{"message_id": m.ID, "body": "Yes, noon works."})
	rt.mu.Lock()
	rt.args = string(args)
	rt.mu.Unlock()

	code, got, problem := pp.srv.retryPluginMessage(asSession(pp.ownerID), m.ID)
	if code != http.StatusAccepted || problem != "" || got.Status != plugins.StatusQueued {
		t.Fatalf("retry: %d %q %+v", code, problem, got)
	}
	pp.srv.WaitForPluginDeliveries()
	if n := len(rt.seen()); n != 2 {
		t.Fatalf("the runtime saw %d request(s); want 2 (the delivery turn and its tool output)", n)
	}
	after, _ := pp.ps.Message(pp.owner, m.ID)
	if after.Status != plugins.StatusDelivered || after.AgentID == nil || *after.AgentID != pp.chief.ID || after.Attempts != m.Attempts+1 {
		t.Fatalf("after the retry: %+v (attempts before %d)", after, m.Attempts)
	}
	cards, _ := pp.ps.DraftCards(pp.owner, 10)
	if len(cards) != 1 || cards[0].Draft.MessageID != m.ID || cards[0].Draft.Status != plugins.DraftPending {
		t.Fatalf("drafts after the retry: %+v", cards)
	}
	if list, _ := pp.ps.Retryable(pp.owner, 10); len(list) != 0 {
		t.Fatalf("the delivered message is still listed retryable: %+v", list)
	}
}

// TestRetryRefusesWhatMustNotBeSentAgain: each state that must not be handed to
// the chief again is refused (409) and left exactly as it was — queued (which is
// also a double click), delivering, delivered, a pending draft, a sent draft, a
// live session route — as is a message of another owner (404) and an owner with
// no chief (409).
func TestRetryRefusesWhatMustNotBeSentAgain(t *testing.T) {
	pp := newPluginPlane(t, &scriptedRuntime{name: replyDraftToolName})
	sys := authz.System(context.Background())
	pool := pp.pool

	queued, _, _ := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: "r-queued", Body: "b"})
	delivering, _, _ := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: "r-delivering", Body: "b"})
	if ok, err := pp.ps.BeginDelivery(pp.owner, delivering.ID, pp.chief.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	delivered, _, _ := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: "r-delivered", Body: "b"})
	_, _ = pp.ps.BeginDelivery(pp.owner, delivered.ID, pp.chief.ID)
	_, _ = pp.ps.RecordDelivery(pp.owner, delivered.ID, pp.chief.ID, plugins.StatusDelivered)
	// A reply drafted on the turn that was cut off.
	drafted := func(ext string, status string) plugins.Message {
		m, _, _ := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: ext, Body: "b"})
		_, _ = pp.ps.BeginDelivery(pp.owner, m.ID, pp.chief.ID)
		d, _, err := pp.ps.FileDraft(pp.owner, pp.chief.ID, pp.ownerID, pp.chief.Name, m.ID, "a reply")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = pp.ps.RecordDelivery(pp.owner, m.ID, pp.chief.ID, plugins.StatusUnconfirmed)
		if status != "pending" {
			if _, err := pool.Exec(sys, `UPDATE plugin_reply_drafts SET status=$2, decided_at=now(), approved_at=now(), claimed_at=now(), lease_until=now(), finished_at=now() WHERE id=$1`, d.ID, status); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	pending, sent := drafted("r-pending", "pending"), drafted("r-sent", "sent")
	routed := pp.unconfirmed(t, "r-routed")
	if _, err := pool.Exec(sys, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id, target_kind, target_key, timeout_at)
		VALUES ($1, $4, $2, $3, 'mirror', 'sess-1', now() + interval '1 hour')`, pp.ownerID, routed.ID, pp.chief.ID,
		fmt.Sprintf("rt_%024x", time.Now().UnixNano())); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		m    plugins.Message
		want string
	}{
		{"queued", queued, "already queued"},
		{"delivering", delivering, "being delivered"},
		{"delivered", delivered, "already received it"},
		{"pending draft", pending, "already drafted a reply"},
		{"sent draft", sent, "already drafted a reply"},
		{"live route", routed, "routed it to a session"},
	} {
		before, _ := pp.ps.Message(pp.owner, c.m.ID)
		code, _, problem := pp.srv.retryPluginMessage(asSession(pp.ownerID), c.m.ID)
		if code != http.StatusConflict || !strings.Contains(problem, c.want) {
			t.Errorf("%s: %d %q; want 409 saying %q", c.name, code, problem, c.want)
		}
		if after, _ := pp.ps.Message(pp.owner, c.m.ID); after.Status != before.Status || after.Attempts != before.Attempts {
			t.Errorf("%s: a refused retry changed the message: %+v -> %+v", c.name, before, after)
		}
	}

	// The page's list uses the same predicate: none of the refused messages is
	// offered a Retry button, and a plain unconfirmed one is (positive control).
	plain := pp.unconfirmed(t, "r-plain")
	list, err := pp.ps.Retryable(pp.owner, 50)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, m := range list {
		listed[m.ID] = true
	}
	if !listed[plain.ID] {
		t.Fatalf("control: a plain unconfirmed message is not listed retryable")
	}
	for name, m := range map[string]plugins.Message{"queued": queued, "delivering": delivering, "delivered": delivered,
		"pending draft": pending, "sent draft": sent, "live route": routed} {
		if listed[m.ID] {
			t.Errorf("%s: listed with a Retry button the retry would refuse", name)
		}
	}

	// A double click: the first retry requeues it, the second finds it queued.
	again := pp.unconfirmed(t, "r-twice")
	if code, _, p := pp.srv.retryPluginMessage(asSession(pp.ownerID), again.ID); code != http.StatusAccepted {
		t.Fatalf("first click: %d %q", code, p)
	}
	// The first click's delivery runs in the background, so the second click
	// finds the message queued OR already being delivered: both are refusals.
	if code, _, p := pp.srv.retryPluginMessage(asSession(pp.ownerID), again.ID); code != http.StatusConflict ||
		!(strings.Contains(p, "already queued") || strings.Contains(p, "being delivered") || strings.Contains(p, "already received")) {
		t.Fatalf("second click: %d %q", code, p)
	}
	pp.srv.WaitForPluginDeliveries()

	// Another owner: not found, and the message untouched.
	other := pp.unconfirmed(t, "r-other-owner")
	var stranger int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "retry-stranger-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&stranger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(sys, `DELETE FROM agents WHERE owner_id=$1`, stranger)
		_, _ = pool.Exec(sys, `DELETE FROM users WHERE id=$1`, stranger)
	})
	if _, err := agents.NewPG(pool).Create(asSession(stranger), agents.Agent{Name: "retry-stranger-chief-" + strconv.FormatInt(stranger, 10), Namespace: "ns-retry-stranger",
		DisplayName: "chief", Status: agents.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if code, _, p := pp.srv.retryPluginMessage(asSession(stranger), other.ID); code != http.StatusNotFound {
		t.Fatalf("another owner's retry: %d %q; want 404", code, p)
	}
	if after, _ := pp.ps.Message(pp.owner, other.ID); after.Status != plugins.StatusUnconfirmed {
		t.Fatalf("another owner's retry moved the message to %s", after.Status)
	}

	// No chief: refused before anything is requeued.
	if _, err := pool.Exec(sys, `UPDATE agents SET display_name='retired' WHERE id=$1`, pp.chief.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, p := pp.srv.retryPluginMessage(asSession(pp.ownerID), other.ID); code != http.StatusConflict || !strings.Contains(p, "no chief") {
		t.Fatalf("with no chief: %d %q", code, p)
	}
	if after, _ := pp.ps.Message(pp.owner, other.ID); after.Status != plugins.StatusUnconfirmed {
		t.Fatalf("a retry with no chief requeued the message (%s)", after.Status)
	}
}

// TestTheRetryRoutes: the CLI's door (hook tier, the operator) and the page's
// form (session + same origin) each requeue an unconfirmed message of the
// operator's and record the audit row; the form redirects back to /plugins and a
// second post is the page with the refusal. The page lists the message with its
// Retry button first. (No agent gateway here: the message stays queued.)
func TestTheRetryRoutes(t *testing.T) {
	var ps *plugins.PGStore
	f := newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ps = plugins.NewPG(pool)
		ext.Plugins = ps
	})
	op := authz.Operator(context.Background())
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	ca, cerr := agents.NewPG(f.pool).Create(op, agents.Agent{Name: "retry-opchief-" + suffix, Namespace: "ns-retry-opchief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning})
	if cerr != nil {
		t.Fatal(cerr)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id=$1`, ca.ID)
	})
	chief := ca.ID
	_, p, err := ps.Create(op, "retry-inbox-"+suffix[len(suffix)-6:], "Retry inbox", "retry-inbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugins WHERE id=$1`, p.ID)
	})
	mk := func(ext string) plugins.Message {
		m, _, err := ps.Submit(op, p.ID, plugins.MessageIn{ExternalID: ext, Sender: "<b>someone</b>", Subject: "hi", Body: "b"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ps.BeginDelivery(op, m.ID, chief)
		_, _ = ps.RecordDelivery(op, m.ID, chief, plugins.StatusUnconfirmed)
		return m
	}
	viaCLI, viaForm := mk("route-cli-"+suffix), mk("route-form-"+suffix)

	page := f.call(t, http.MethodGet, "/plugins", nil, f.operator()).Body.String()
	if !strings.Contains(page, `data-retry-message-id="`+strconv.FormatInt(viaForm.ID, 10)+`"`) ||
		!strings.Contains(page, `action="/plugins/messages/`+strconv.FormatInt(viaForm.ID, 10)+`/retry"`) ||
		strings.Contains(page, "<b>someone</b>") {
		t.Fatalf("the page does not list the unconfirmed message with its Retry button (or does not escape its sender)")
	}

	w := f.call(t, http.MethodPost, "/api/plugins/messages/"+strconv.FormatInt(viaCLI.ID, 10)+"/retry", nil,
		map[string]string{"Authorization": "Bearer " + connTestHookToken})
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Fatalf("the CLI door: %d %s", w.Code, w.Body)
	}
	if w := f.call(t, http.MethodPost, "/api/plugins/messages/"+strconv.FormatInt(viaCLI.ID, 10)+"/retry", nil,
		map[string]string{"Authorization": "Bearer " + connTestHookToken}); w.Code != http.StatusConflict {
		t.Fatalf("the CLI door, again: %d %s", w.Code, w.Body)
	}

	w = f.call(t, http.MethodPost, "/plugins/messages/"+strconv.FormatInt(viaForm.ID, 10)+"/retry", nil, f.operator())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/plugins#plugin-retryable" {
		t.Fatalf("the form: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := f.call(t, http.MethodPost, "/plugins/messages/"+strconv.FormatInt(viaForm.ID, 10)+"/retry", nil, f.operator()); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "cannot be retried") {
		t.Fatalf("the form, again: %d", w.Code)
	}
	// Same-origin: a cross-site post is refused and changes nothing.
	third := mk("route-xsite-" + suffix)
	hdr := f.operator()
	hdr["Origin"] = "http://evil.example"
	if w := f.call(t, http.MethodPost, "/plugins/messages/"+strconv.FormatInt(third.ID, 10)+"/retry", nil, hdr); w.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin form post: %d", w.Code)
	}
	for _, m := range []plugins.Message{viaCLI, viaForm, third} {
		got, _ := ps.Message(op, m.ID)
		want := plugins.StatusQueued
		if m.ID == third.ID {
			want = plugins.StatusUnconfirmed
		}
		if got.Status != want {
			t.Errorf("message %d is %s, want %s", m.ID, got.Status, want)
		}
	}
	var audits int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log WHERE action='plugin.retried' AND object = ANY($1)`,
		[]string{plugins.MessageObject(viaCLI.ID), plugins.MessageObject(viaForm.ID)}).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("plugin.retried audit rows = %d (%v), want 2", audits, err)
	}
}
