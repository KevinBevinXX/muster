package api

import (
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sse"
)

// THE ASKS PAGE (asks_drafts.go), on real Postgres: /attention is renamed Asks
// and split into an "Ask" tab (the queue without its reply drafts) and a
// "Reply drafts" tab with two views, Awaiting decision and Archived. A draft
// leaves Awaiting the moment it is decided and is drawn in Archived with its
// live status. Every expectation is a literal: the URLs, ids and texts a
// browser, a phone notification and the e2e spec depend on.

// htmlRegion is the HTML from the element whose start tag contains open up to the
// first close after it ("" when there is none).
func htmlRegion(page, open, close string) string {
	i := strings.Index(page, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], close)
	if j < 0 {
		return ""
	}
	return page[i : i+j]
}

func askList(page string) string     { return htmlRegion(page, `<ul id="attention-items"`, "</ul>") }
func pendingList(page string) string { return htmlRegion(page, `<ul id="asks-drafts-pending"`, "</ul>") }
func archivedList(page string) string {
	return htmlRegion(page, `<ul id="asks-drafts-archived"`, "</ul>")
}

// selectedTab is the aria-selected value of the tab element id on page.
func selectedTab(t *testing.T, page, id string) string {
	t.Helper()
	m := regexp.MustCompile(`<button[^>]* id="` + id + `"[^>]*aria-selected="(true|false)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no tab %q on the page", id)
	}
	return m[1]
}

// panelHidden is whether the tab panel id is drawn hidden.
func panelHidden(t *testing.T, page, id string) bool {
	t.Helper()
	m := regexp.MustCompile(`<div id="` + id + `" role="tabpanel"[^>]*>`).FindString(page)
	if m == "" {
		t.Fatalf("no panel %q on the page", id)
	}
	return strings.Contains(m, " hidden")
}

func draftAnchor(id int64) string { return `id="draft-` + strconv.FormatInt(id, 10) + `"` }

// TestTheAsksPageSplitsAsksFromReplyDrafts: one ask, one waiting session and
// one pending reply draft. The page is "Asks"; the Ask tab lists the ask and
// the session but NOT the draft (it points at the Reply drafts tab instead);
// the draft is under Reply drafts → Awaiting decision; the badge still counts
// all three; the draft's queue link, the tab query and an old /attention#ask-N
// link all land; /plugins no longer draws drafts.
func TestTheAsksPageSplitsAsksFromReplyDrafts(t *testing.T) {
	f := newAttnFixture(t)
	askID := f.ask(t, f.opTok, "Ship it?")
	_, d := f.pendingDraft(t, "Sounds good.")
	ask, draft := `id="ask-`+strconv.FormatInt(askID, 10)+`"`, draftAnchor(d.ID)

	page := f.page(t, "/attention")
	for _, want := range []string{"<title>Asks · muster</title>", `>Asks</h1>`,
		`id="asks-tabs" role="tablist" aria-label="Asks"`, `id="asks-drafts-tabs" role="tablist" aria-label="Reply drafts"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, ">Attention</h1>") {
		t.Error("the page is still headed Attention")
	}
	if got := selectedTab(t, page, "asks-tabs-tab-ask"); got != "true" {
		t.Errorf("with no ?tab the Ask tab is aria-selected=%s; want true", got)
	}
	if !strings.Contains(page, `id="asks-tabs-tab-drafts"`) || !strings.Contains(page, ">Reply drafts (1)</button>") {
		t.Errorf("the Reply drafts tab does not count its one pending draft")
	}
	al := askList(page)
	if !strings.Contains(al, ask) || !strings.Contains(al, `data-kind="session"`) {
		t.Fatalf("the Ask tab lacks the ask or the waiting session:\n%s", al)
	}
	if strings.Contains(al, draft) || strings.Contains(al, `data-kind="plugin"`) {
		t.Fatalf("the Ask tab still lists the reply draft:\n%s", al)
	}
	if strings.Contains(al, "attention-drafts-pointer") || strings.Contains(al, "reply draft") {
		t.Fatalf("the Ask tab carries a drafts line (the tab's label is the count):\n%s", al)
	}
	if pl := pendingList(page); !strings.Contains(pl, draft) || !strings.Contains(pl, "data-draft-approve") {
		t.Fatalf("the pending draft is not under Awaiting decision with its Approve:\n%s", pl)
	}
	if strings.Contains(archivedList(page), draft) {
		t.Fatal("a pending draft is drawn in Archived")
	}
	if n := strings.Count(page, draft); n != 1 {
		t.Fatalf("the page carries %s %d times; want once (a fragment must name one element)", draft, n)
	}

	// The live list's fragment is the Ask tab's list too.
	items := f.page(t, "/attention/items")
	if strings.Contains(items, draft) || !strings.Contains(items, ask) || strings.Contains(items, "attention-drafts-pointer") {
		t.Fatalf("/attention/items: want the ask, not the draft or a drafts line:\n%s", items)
	}

	// The badge still counts the draft: ask + session + draft.
	if b := f.page(t, "/attention/badge"); !strings.Contains(b, `data-count="3"`) {
		t.Fatalf("the badge: %s; want 3 (the pending draft still counts)", b)
	}

	// The draft's queue item (agent cards' badges, attention_list) links to
	// its card on the Asks page.
	its, err := f.srv.attentionItems(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	var link string
	for _, it := range its {
		if it.Key == "draft-"+strconv.FormatInt(d.ID, 10) {
			link = it.Link
		}
	}
	if want := "/attention?tab=drafts#draft-" + strconv.FormatInt(d.ID, 10); link != want {
		t.Fatalf("the draft's queue link is %q; want %q", link, want)
	}

	// ?tab= opens the named tab with no script.
	dp := f.page(t, "/attention?tab=drafts")
	if selectedTab(t, dp, "asks-tabs-tab-drafts") != "true" || selectedTab(t, dp, "asks-drafts-tabs-tab-drafts") != "true" ||
		!panelHidden(t, dp, "asks-tabs-panel-ask") || panelHidden(t, dp, "asks-tabs-panel-drafts") || panelHidden(t, dp, "asks-drafts-tabs-panel-drafts") {
		t.Fatal("?tab=drafts does not open Reply drafts → Awaiting decision")
	}
	ap := f.page(t, "/attention?tab=archived")
	if selectedTab(t, ap, "asks-tabs-tab-drafts") != "true" || selectedTab(t, ap, "asks-drafts-tabs-tab-archived") != "true" ||
		!panelHidden(t, ap, "asks-drafts-tabs-panel-drafts") || panelHidden(t, ap, "asks-drafts-tabs-panel-archived") {
		t.Fatal("?tab=archived does not open Reply drafts → Archived")
	}
	// An old /attention#ask-N link (a phone's notification, an agent card's
	// badge): the fragment never reaches the server, so the plain page must
	// draw that ask on its VISIBLE default tab.
	if panelHidden(t, page, "asks-tabs-panel-ask") || !strings.Contains(askList(page), ask) {
		t.Fatal("/attention#ask-N: the ask is not on the visible default tab")
	}

	// /plugins keeps its plugins and loses the drafts; an old /plugins#draft-N
	// link is sent on to the Asks page.
	pp := f.page(t, "/plugins")
	if strings.Contains(pp, `id="draft-`) || strings.Contains(pp, "data-draft-approve") || strings.Contains(pp, `id="term-confirm"`) {
		t.Fatal("/plugins still draws reply drafts (or their confirm dialog)")
	}
	if !strings.Contains(pp, `data-testid="plugins-drafts-moved"`) || !strings.Contains(pp, `window.location.replace('/attention?tab=drafts' + window.location.hash)`) {
		t.Fatal("/plugins does not send a draft link on to the Asks page")
	}
	if !strings.Contains(pp, `data-testid="plugins"`) {
		t.Fatal("/plugins lost its plugin list")
	}
}

// TestAsksWithPluginsOffHasNoDraftsTab: no plugins served, no Reply drafts
// tab (and no confirm dialog): the page is the Ask list alone, still "Asks".
func TestAsksWithPluginsOffHasNoDraftsTab(t *testing.T) {
	f := newAttnFixture(t)
	askID := f.ask(t, f.opTok, "Ship it?")
	f.srv.ext.Plugins = nil
	page := f.page(t, "/attention?tab=drafts")
	if !strings.Contains(page, `>Asks</h1>`) || !strings.Contains(askList(page), `id="ask-`+strconv.FormatInt(askID, 10)+`"`) {
		t.Fatalf("the plugins-off page lacks its heading or the ask:\n%s", page)
	}
	if strings.Contains(page, `role="tablist"`) || strings.Contains(page, `id="term-confirm"`) || strings.Contains(page, "Reply drafts") {
		t.Fatal("with plugins off the page offers a Reply drafts tab")
	}
}

// TestADecidedDraftMovesToArchivedWithItsLiveStatus walks every way a draft
// leaves Awaiting decision, through the real routes: approve (fresh) → lands on
// its card in Archived, approved-awaiting-claim, Withdraw offered → the plugin
// claims it → "collected", no Withdraw, and a withdraw is refused on the
// Archived view → sent. A reject. A withdraw of an approved, unclaimed one. A
// failed send. A draft superseded by a newer one for its message. One that
// expired. Each is in Archived, not Awaiting, with its status in words.
func TestADecidedDraftMovesToArchivedWithItsLiveStatus(t *testing.T) {
	d := newDraftSeam(t)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "askchief")
	d.rt.expect(chief)
	file := func(ext, body string) (int64, int64) {
		t.Helper()
		m := d.post(tok, ext+"-"+d.suffix, "")
		id, out := d.replyDraft(chief, m, body)
		if id == 0 {
			t.Fatalf("draft %s: %s", ext, out)
		}
		return m, id
	}
	path := func(id int64, how string) string { return "/plugins/drafts/" + strconv.FormatInt(id, 10) + "/" + how }
	page := func() string { return d.do("GET", "/attention?tab=archived", "", d.session(d.a, false, true)).Body.String() }
	archivedCard := func(id int64) string {
		t.Helper()
		p := page()
		if strings.Contains(pendingList(p), draftAnchor(id)) {
			t.Fatalf("draft %d is still under Awaiting decision", id)
		}
		a := archivedList(p)
		if !strings.Contains(a, draftAnchor(id)) {
			t.Fatalf("draft %d is not in Archived:\n%s", id, a)
		}
		return cardOf(t, a, id)
	}
	decide := func(id int64, how string) {
		t.Helper()
		rec := d.do("POST", path(id, how), "", d.session(d.a, false, true))
		if want := "/attention?tab=archived&draft=" + strconv.FormatInt(id, 10) + "#draft-" + strconv.FormatInt(id, 10); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Fatalf("%s draft %d: %d → %q; want 303 → %q", how, id, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	audited := func(id int64, action string) {
		t.Helper()
		var n int
		if err := d.pool.(*pgxpool.Pool).QueryRow(authz.System(d.ctx), `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2`,
			action, plugins.DraftObject(id)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("audit %s for draft %d: %d rows (%v); want 1", action, id, n, err)
		}
	}

	// Approve: Awaiting → Archived, waiting for the plugin, Withdraw offered.
	_, d1 := file("a1", "First.")
	if p := page(); !strings.Contains(pendingList(p), draftAnchor(d1)) {
		t.Fatalf("CONTROL: the new draft is not under Awaiting decision")
	}
	decide(d1, "approve")
	audited(d1, "plugin.draft.approved")
	c := archivedCard(d1)
	if !strings.Contains(c, `data-draft-status="approved"`) || !strings.Contains(c, "waiting for the plugin to collect it") ||
		!strings.Contains(c, "data-draft-withdraw") || strings.Contains(c, "data-draft-approve") {
		t.Fatalf("the approved, unclaimed card:\n%s", c)
	}
	// Claimed: collected, being sent, NO Withdraw — and a withdraw is refused,
	// drawn on the Archived view.
	if rec := d.claim(tok, 0); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}
	c = archivedCard(d1)
	if !strings.Contains(c, "the plugin collected it") || strings.Contains(c, "data-draft-withdraw") {
		t.Fatalf("the claimed card:\n%s", c)
	}
	rec := d.do("POST", path(d1, "reject"), "", d.session(d.a, false, true))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "can no longer be withdrawn") ||
		selectedTab(t, rec.Body.String(), "asks-drafts-tabs-tab-archived") != "true" {
		t.Fatalf("withdrawing a claimed draft: %d; want 409 drawn on Archived with the reason", rec.Code)
	}
	// Sent.
	if rec := d.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(d1, 10)+"/result", `{"ok":true,"external_ref":"ref-1"}`, bearerTok(tok)); rec.Code != http.StatusOK {
		t.Fatalf("result: %d", rec.Code)
	}
	if c = archivedCard(d1); !strings.Contains(c, `data-draft-status="sent"`) || !strings.Contains(c, ">Sent ") || !strings.Contains(c, "ref-1") {
		t.Fatalf("the sent card:\n%s", c)
	}

	// Reject: straight to Archived as rejected, nothing to act on.
	_, d2 := file("a2", "Second.")
	decide(d2, "reject")
	audited(d2, "plugin.draft.rejected")
	if c = archivedCard(d2); !strings.Contains(c, `data-draft-status="rejected"`) || !strings.Contains(c, ">Rejected ") ||
		strings.Contains(c, "<form") {
		t.Fatalf("the rejected card:\n%s", c)
	}

	// Withdraw from Archived: approve, then reject the approved, unclaimed one.
	_, d3 := file("a3", "Third.")
	decide(d3, "approve")
	if c = archivedCard(d3); !strings.Contains(c, `action="/plugins/drafts/`+strconv.FormatInt(d3, 10)+`/reject"`) {
		t.Fatalf("the approved card's Withdraw does not post the reject:\n%s", c)
	}
	decide(d3, "reject")
	audited(d3, "plugin.draft.withdrawn")
	if got := d.draft(d3); got.Status != plugins.DraftRejected || got.ApprovedAt != nil {
		t.Fatalf("the withdrawn draft: %s approved_at=%v; want rejected, approval cleared", got.Status, got.ApprovedAt)
	}
	if c = archivedCard(d3); !strings.Contains(c, ">Rejected ") || strings.Contains(c, "data-draft-withdraw") {
		t.Fatalf("the withdrawn card:\n%s", c)
	}
	if rec := d.claim(tok, 0); rec.Code != http.StatusNoContent {
		t.Fatalf("a claim after the withdraw: %d; want 204", rec.Code)
	}

	// Failed.
	_, d4 := file("a4", "Fourth.")
	decide(d4, "approve")
	if rec := d.claim(tok, 0); rec.Code != http.StatusOK {
		t.Fatalf("claim d4: %d", rec.Code)
	}
	if rec := d.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(d4, 10)+"/result", `{"ok":false,"detail":"mailbox full"}`, bearerTok(tok)); rec.Code != http.StatusOK {
		t.Fatalf("result d4: %d", rec.Code)
	}
	if c = archivedCard(d4); !strings.Contains(c, `data-draft-status="failed"`) || !strings.Contains(c, "The plugin could not send it") ||
		!strings.Contains(c, "mailbox full") {
		t.Fatalf("the failed card:\n%s", c)
	}

	// Superseded: a newer draft for the same message replaces a pending one.
	m5, d5 := file("a5", "Fifth.")
	d6, out := d.replyDraft(chief, m5, "Fifth, revised.")
	if d6 == 0 {
		t.Fatalf("the newer draft: %s", out)
	}
	if c = archivedCard(d5); !strings.Contains(c, `data-draft-status="superseded"`) || !strings.Contains(c, "Replaced by a newer draft") {
		t.Fatalf("the superseded card:\n%s", c)
	}
	if !strings.Contains(pendingList(page()), draftAnchor(d6)) {
		t.Fatal("the newer draft is not under Awaiting decision")
	}

	// Expired: a pending draft past its 7 days.
	_, d7 := file("a7", "Seventh.")
	// The body-immutability trigger guards expires_at, so this one statement
	// skips triggers (plugins' ageDraft does the same).
	dbtest.OwnerExec(t, d.dsn, `SET session_replication_role = replica;
		UPDATE plugin_reply_drafts SET created_at = created_at - interval '8 days', expires_at = expires_at - interval '8 days'
		WHERE id = `+strconv.FormatInt(d7, 10))
	if c = archivedCard(d7); !strings.Contains(c, `data-draft-status="expired"`) || !strings.Contains(c, "Expired unsent") || strings.Contains(c, "<form") {
		t.Fatalf("the expired card:\n%s", c)
	}

	// Another member sees none of it.
	other := d.do("GET", "/attention?tab=archived", "", d.session(d.b, false, true)).Body.String()
	if strings.Contains(other, `id="draft-`) {
		t.Fatal("another member's Asks page draws this member's drafts")
	}
}

// TestAStaleApprovalOnTheAsksPageIsRefusedWithTheConfirm: the step-up gate is
// unchanged by the move — a stale approval posted by the page's form is 403,
// drawn on Reply drafts → Awaiting decision with the reason and the confirm
// dialog; by a script, 403 with X-Muster-Step-Up — and the draft stays pending.
func TestAStaleApprovalOnTheAsksPageIsRefusedWithTheConfirm(t *testing.T) {
	d := newDraftSeam(t)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "stalechief")
	d.rt.expect(chief)
	m := d.post(tok, "stale-"+d.suffix, "")
	id, out := d.replyDraft(chief, m, "Yes.")
	if id == 0 {
		t.Fatalf("draft: %s", out)
	}
	approve := "/plugins/drafts/" + strconv.FormatInt(id, 10) + "/approve"
	rec := d.do("POST", approve, "", d.session(d.a, true, true))
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.Contains(body, html.EscapeString(DraftStepUpRequired)) {
		t.Fatalf("a stale form approval: %d; want 403 and the reason", rec.Code)
	}
	if !strings.Contains(body, ">Asks</h1>") || selectedTab(t, body, "asks-tabs-tab-drafts") != "true" ||
		selectedTab(t, body, "asks-drafts-tabs-tab-drafts") != "true" || !strings.Contains(body, `id="term-confirm"`) ||
		!strings.Contains(pendingList(body), draftAnchor(id)) {
		t.Fatal("the refusal is not drawn on Reply drafts → Awaiting decision with the confirm dialog and the draft")
	}
	if rec := d.do("POST", approve, "", d.session(d.a, true, false)); rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale script approval: %d %v", rec.Code, rec.Header())
	}
	if got := d.draft(id); got.Status != plugins.DraftPending {
		t.Fatalf("after the refusals the draft is %s; want pending", got.Status)
	}
	// The page's Approve is a step-up form, and the page's script asks by
	// delegation (cards re-drawn live still ask).
	page := d.do("GET", "/attention?tab=drafts", "", d.session(d.a, false, true)).Body.String()
	if !strings.Contains(cardOf(t, page, id), `action="/plugins/drafts/`+strconv.FormatInt(id, 10)+`/approve" data-step-up-form`) ||
		!strings.Contains(page, "document.addEventListener('submit'") {
		t.Fatal("the Approve is not a step-up form asked for by delegation")
	}
}

// TestTheDraftsFragmentAndThePluginsMovesAreLive: GET /attention/drafts is
// both views and the pending count (what the page re-draws), the owner's
// only; and a plugin's claim and result each tell the owner's open pages —
// the queue's event, naming the draft — and nobody else's.
func TestTheDraftsFragmentAndThePluginsMovesAreLive(t *testing.T) {
	d := newDraftSeam(t)
	bus := sse.New(64)
	d.srv.bus = bus
	_, aCh := bus.Subscribe(d.a)
	_, bCh := bus.Subscribe(d.b)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "livechief")
	d.rt.expect(chief)
	m1 := d.post(tok, "live1-"+d.suffix, "")
	m2 := d.post(tok, "live2-"+d.suffix, "")
	pend, _ := d.replyDraft(chief, m1, "Pending one.")
	done, _ := d.replyDraft(chief, m2, "Approved one.")
	if pend == 0 || done == 0 {
		t.Fatal("drafts were not filed")
	}
	if rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(done, 10)+"/approve", "", d.session(d.a, false, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d", rec.Code)
	}

	rec := d.do("GET", "/attention/drafts", "", d.session(d.a, false, false))
	var j struct {
		Pending, Archived, Label string
		PendingIDs               string `json:"pending_ids"`
		Count                    *int
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
		t.Fatalf("GET /attention/drafts: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(j.Pending, draftAnchor(pend)) || strings.Contains(j.Pending, draftAnchor(done)) ||
		!strings.Contains(j.Archived, draftAnchor(done)) || strings.Contains(j.Archived, draftAnchor(pend)) ||
		j.PendingIDs != strconv.FormatInt(pend, 10) || j.Count != nil || j.Label != "Reply drafts (1)" {
		t.Fatalf("the fragment: %+v", j)
	}
	rec = d.do("GET", "/attention/drafts", "", d.session(d.b, false, false))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "draft-") || !strings.Contains(rec.Body.String(), `"pending_ids":""`) {
		t.Fatalf("another member's fragment: %d %s", rec.Code, rec.Body.String())
	}

	drain := func(ch <-chan sse.Event) {
		for {
			select {
			case <-ch:
			case <-time.After(100 * time.Millisecond):
				return
			}
		}
	}
	drain(aCh)
	drain(bCh)
	want := `{"id":"draft-` + strconv.FormatInt(done, 10) + `"}`
	expect := func(what string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev := <-aCh:
				if ev.Name == EventAttentionChanged && ev.Data == want {
					return
				}
			case <-deadline:
				t.Fatalf("%s: the owner's pages were not told (%s %s)", what, EventAttentionChanged, want)
			}
		}
	}
	if rec := d.claim(tok, 0); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}
	expect("the claim")
	if rec := d.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(done, 10)+"/result", `{"ok":true}`, bearerTok(tok)); rec.Code != http.StatusOK {
		t.Fatalf("result: %d", rec.Code)
	}
	expect("the result")
	select {
	case ev := <-bCh:
		t.Fatalf("another member was told: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTheAskTabWithOnlyDraftsWaitingSaysNothingIsWaitingThere: with nothing
// but a pending reply draft, the Ask tab's list is its empty line ALONE — no
// "1 reply draft awaits" line contradicting it (the count is the Reply
// drafts tab's label) — and the draft is under Awaiting decision.
func TestTheAskTabWithOnlyDraftsWaitingSaysNothingIsWaitingThere(t *testing.T) {
	d := newDraftSeam(t)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "onlychief")
	d.rt.expect(chief)
	m := d.post(tok, "only-"+d.suffix, "")
	id, out := d.replyDraft(chief, m, "Only this.")
	if id == 0 {
		t.Fatalf("draft: %s", out)
	}
	page := d.do("GET", "/attention", "", d.session(d.a, false, true)).Body.String()
	want := `<ul id="attention-items" class="flex flex-col gap-2" data-testid="attention-items" aria-live="polite">` +
		`<li class="text-sm text-fg2" data-testid="attention-empty">Nothing is waiting on you.</li>`
	if got := askList(page); got != want {
		t.Fatalf("the Ask tab's list:\n%s\nwant exactly:\n%s", got, want)
	}
	if !strings.Contains(page, ">Reply drafts (1)</button>") || !strings.Contains(pendingList(page), draftAnchor(id)) {
		t.Fatal("the draft is not counted on its tab or not under Awaiting decision")
	}
}

// TestADecisionLandsOnItsCardBeyondTheArchivedCut: a draft decided while 55
// newer decided drafts fill Archived's 50 is still drawn where the decision
// lands (?draft=N names it), first in Archived — and only there, once.
// CONTROL: the same page without ?draft= does not draw it (the cut is real).
func TestADecisionLandsOnItsCardBeyondTheArchivedCut(t *testing.T) {
	d := newDraftSeam(t)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "cutchief")
	d.rt.expect(chief)
	m := d.post(tok, "cut-"+d.suffix, "")
	old, out := d.replyDraft(chief, m, "The old one.")
	if old == 0 {
		t.Fatalf("draft: %s", out)
	}
	m2 := d.post(tok, "cut2-"+d.suffix, "")
	dbtest.OwnerExec(t, d.dsn, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, status, decided_at)
		SELECT $1, $2, $3, 'x', 'filler ' || g, now() + interval '1 day', 'rejected', now() FROM generate_series(1, 55) g`, d.a, m2, chief.ID)
	rec := d.do("POST", "/plugins/drafts/"+strconv.FormatInt(old, 10)+"/reject", "", d.session(d.a, false, true))
	loc := rec.Header().Get("Location")
	if want := "/attention?tab=archived&draft=" + strconv.FormatInt(old, 10) + "#draft-" + strconv.FormatInt(old, 10); rec.Code != http.StatusSeeOther || loc != want {
		t.Fatalf("reject: %d → %q; want 303 → %q", rec.Code, loc, want)
	}
	plain := d.do("GET", "/attention?tab=archived", "", d.session(d.a, false, true)).Body.String()
	if strings.Contains(plain, draftAnchor(old)) || strings.Count(archivedList(plain), `<li id="draft-`) != 50 {
		t.Fatal("CONTROL: without ?draft= the old draft is drawn, or Archived is not cut at 50")
	}
	landed := d.do("GET", strings.SplitN(loc, "#", 2)[0], "", d.session(d.a, false, true)).Body.String()
	a := archivedList(landed)
	if first := strings.Index(a, `<li id="draft-`); first < 0 || !strings.HasPrefix(a[first:], `<li id="draft-`+strconv.FormatInt(old, 10)+`"`) ||
		strings.Count(landed, draftAnchor(old)) != 1 {
		t.Fatalf("the decided draft is not drawn first in Archived, once:\n%.600s", a)
	}
	// The live fragment keeps it too, and another member's ?draft= draws nothing.
	if f := d.do("GET", "/attention/drafts?draft="+strconv.FormatInt(old, 10), "", d.session(d.a, false, false)).Body.String(); !strings.Contains(f, `draft-`+strconv.FormatInt(old, 10)) {
		t.Fatal("the live fragment drops the decided draft")
	}
	if o := d.do("GET", "/attention?tab=archived&draft="+strconv.FormatInt(old, 10), "", d.session(d.b, false, true)).Body.String(); strings.Contains(o, draftAnchor(old)) {
		t.Fatal("another member's ?draft= draws this member's draft")
	}
}

// TestAPendingListCarriesItsIdentity: the Awaiting decision list carries the
// ids it was drawn with (data-draft-ids), the live fragment the ids it would
// draw (pending_ids), and the page's script re-draws the list in place only
// when the two agree — a superseding draft changes the ids, so the cards on
// screen stay and the "changed" line is offered (the e2e drives it).
func TestAPendingListCarriesItsIdentity(t *testing.T) {
	d := newDraftSeam(t)
	tok := d.mint(d.a)
	chief := d.chief(d.a, "idchief")
	d.rt.expect(chief)
	m := d.post(tok, "ids-"+d.suffix, "")
	a, _ := d.replyDraft(chief, m, "A.")
	page := d.do("GET", "/attention?tab=drafts", "", d.session(d.a, false, true)).Body.String()
	if !strings.Contains(page, `data-testid="drafts-pending" data-draft-ids="`+strconv.FormatInt(a, 10)+`"`) ||
		!strings.Contains(page, `id="asks-drafts-changed"`) {
		t.Fatal("the pending list does not carry its ids, or the page has no changed line")
	}
	b, _ := d.replyDraft(chief, m, "B, revised.")
	f := d.do("GET", "/attention/drafts", "", d.session(d.a, false, false)).Body.String()
	if !strings.Contains(f, `"pending_ids":"`+strconv.FormatInt(b, 10)+`"`) {
		t.Fatalf("the fragment's pending_ids after the supersede: %s", f)
	}
	for _, want := range []string{"j.pending_ids === (pendingList.getAttribute('data-draft-ids') || '')", "changed.hidden = false"} {
		if !strings.Contains(asksPageScript, want) {
			t.Errorf("the page script lacks %q", want)
		}
	}
	if strings.Contains(asksPageScript, "setInterval") {
		t.Error("the page script runs its own timer (attentionPageScript's backstop already fires asks:changed)")
	}
}

// TestARefusalPageLeavesNoPostOnlyAddress: a refusal drawn in answer to a
// decision's POST is the Asks page, and its script moves the address to
// /attention (on load and on every tab switch) — never a POST-only
// /plugins/drafts/N/... for a reload to 405 on. The e2e drives it.
func TestARefusalPageLeavesNoPostOnlyAddress(t *testing.T) {
	for _, want := range []string{"u.pathname = '/attention';", "if (window.location.pathname !== '/attention') syncURL();"} {
		if !strings.Contains(asksPageScript, want) {
			t.Errorf("the page script lacks %q", want)
		}
	}
}
