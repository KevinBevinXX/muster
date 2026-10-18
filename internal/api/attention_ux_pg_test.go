package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE QUEUE'S SUBSTANCE, ORDER AND IN-SESSION ANSWERS (PR attention-ux), on a
// real Postgres: what a waiting session waits on and what a draft says, newest
// first across every source, and an ask answered by the operator writing to
// its asker directly.

// mirrorAsk files an ask from the fake host's window @1 session (focusSID).
func (f *attnFixture) mirrorAsk(t *testing.T, question string) attention.Ask {
	t.Helper()
	return f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main:1 @ ws"}, question)
}

func (f *attnFixture) askAs(t *testing.T, asker attention.Asker, question string) attention.Ask {
	t.Helper()
	a, err := f.attn.Create(asUser(1), attention.New{Asker: asker, Question: question})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, a.ID)
	})
	return a
}

func (f *attnFixture) askStatus(t *testing.T, id int64) attention.Ask {
	t.Helper()
	a, err := f.attn.Get(asUser(1), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// sinceOf moves an ask's or a draft's creation time (the queue's order key).
func (f *attnFixture) setCreated(t *testing.T, table string, id int64, at time.Time) {
	t.Helper()
	q := `UPDATE ` + table + ` SET created_at = $2 WHERE id = $1`
	if table == "attention_asks" {
		q = `UPDATE attention_asks SET created_at = $2, expires_at = $2::timestamptz + interval '7 days' WHERE id = $1`
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), q, id, at); err != nil {
		t.Fatal(err)
	}
}

// waitAs runs attention_wait for asker on ask id in the background.
func (f *attnFixture) waitAs(asker attention.Asker, id int64) <-chan string {
	out := make(chan string, 1)
	go func() {
		out <- f.srv.toolAttentionWait(asUser(1), toolCaller{asker: asker}, `{"id":`+strconv.FormatInt(id, 10)+`,"wait_seconds":20}`)
	}()
	return out
}

func recv(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case s := <-c:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("attention_wait did not return")
	}
	return ""
}

// TestTheQueueIsNewestFirstAcrossSources: an ask, a waiting session, a reply
// draft and a second ask, each with its own time, come back newest first —
// whichever source they were read from — on the read model, the page and
// attention_list alike.
func TestTheQueueIsNewestFirstAcrossSources(t *testing.T) {
	f := newAttnFixture(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond) // the waiting window went waiting here
	f.mirror.mu.Lock()
	f.mirror.windows[0].StatusSince = base.UnixMilli()
	f.mirror.mu.Unlock()
	old := f.ask(t, f.opTok, "The oldest question?")
	f.setCreated(t, "attention_asks", old, base.Add(-time.Hour))
	_, d := f.pendingDraft(t, "a reply") // created now: after the window went waiting
	newest := f.ask(t, f.opTok, "The newest question?")
	f.setCreated(t, "attention_asks", newest, d.CreatedAt.Add(time.Hour))

	want := []string{attentionAskKey(newest), draftKey(d.ID), sessionKey("ws", "main", "@1"), attentionAskKey(old)}
	items, err := f.srv.attentionItems(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Key)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("the queue's order: %v, want newest first %v", got, want)
	}
	page := f.page(t, "/attention/items")
	at := -1
	for _, k := range want {
		i := strings.Index(page, `id="`+k+`"`)
		if i < at {
			t.Fatalf("the page lists %s before an older item: %s", k, page)
		}
		at = i
	}
}

// TestAWaitingSessionShowsWhatItWaitsOn: a window whose host reports what it
// waits on (mirror.Pending) is listed with that action, the last thing Claude
// said, its focus title and its window — and pushed with the action; a host
// that says nothing (an older muster-mirror) gets muster's generic line.
func TestAWaitingSessionShowsWhatItWaitsOn(t *testing.T) {
	f := newAttnFixture(t)
	fs := focus.NewPG(f.pool)
	if _, err := fs.Upsert(asUser(1), focus.MirrorSubject(focusSID), focus.Write{Title: "Publish the CLI release", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	key := sessionKey("ws", "main", "@1")
	// The old host first: nothing said, the generic line.
	items, _ := f.srv.attentionItems(asUser(1))
	if len(items) != 1 || items[0].Key != key || items[0].Text != sessionWaitingText || items[0].Detail != "" {
		t.Fatalf("an older host's waiting window: %+v", items)
	}
	f.srv.BroadcastAgentsChangedFor(1) // seeds the host
	f.setWindows(mirror.StatusIdle)
	f.srv.BroadcastAgentsChangedFor(1)
	f.srv.now = func() time.Time { return time.Now().Add(time.Hour) } // past the re-push window

	f.mirror.mu.Lock()
	f.mirror.windows = attnWindows(mirror.StatusWaiting)
	f.mirror.windows[0].Pending = &mirror.Pending{Summary: "Bash: npm publish --tag next", Tool: "Bash", Last: "Tests pass. Publishing \x1b[31mnow\x1b[0m."}
	f.mirror.mu.Unlock()
	f.srv.BroadcastAgentsChangedFor(1)

	items, _ = f.srv.attentionItems(asUser(1))
	if len(items) != 1 || items[0].Text != "Bash: npm publish --tag next" || items[0].Detail != "Tests pass. Publishing [31mnow [0m." {
		t.Fatalf("the waiting window's substance: %+v", items)
	}
	page := f.page(t, "/attention/items")
	for _, want := range []string{"Bash: npm publish --tag next", `data-attention-detail>Tests pass. Publishing [31mnow [0m.</p>`,
		`data-attention-focus>Publish the CLI release</span>`, `data-attention-source>main:1 @ ws</span>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q: %s", want, page)
		}
	}
	var body string
	for _, p := range f.pusher.to(ep("attn-operator")) {
		if p.Tag == "attention-"+key && p.Type == attentionNotificationType {
			body = p.Body
		}
	}
	// The lock screen says who and where, never the command (the page has it).
	if body != "Waiting: Bash · main:1 @ ws" {
		t.Fatalf("the push body: %q", body)
	}
}

// TestADraftShowsTheMessageAndTheReply: a pending reply draft is listed with
// who wrote and what, and the draft's start — not the push's fixed title.
func TestADraftShowsTheMessageAndTheReply(t *testing.T) {
	f := newAttnFixture(t)
	_, d := f.pendingDraft(t, "Sure — "+strings.Repeat("long ", 60)+"\nsecond line")
	items, _ := f.srv.attentionItems(asUser(1))
	var it attentionItem
	for _, x := range items {
		if x.Key == draftKey(d.ID) {
			it = x
		}
	}
	if it.Text != "s@example.test: hello" {
		t.Fatalf("the draft's text: %q", it.Text)
	}
	if !strings.HasPrefix(it.Detail, "Draft reply: Sure — long long") || !strings.HasSuffix(it.Detail, "…") ||
		strings.Contains(it.Detail, "\n") || len([]rune(it.Detail)) != len("Draft reply: ")+draftReplyMax {
		t.Fatalf("the draft's reply excerpt: %q (%d runes)", it.Detail, len([]rune(it.Detail)))
	}
}

// TestAMessageToTheSessionAnswersItsAsks: the operator SENDS into the asking
// session's window. Every ask that session raised before the message is
// answered in session — its waiter returns that status and the message, its
// notification is cleared, it leaves the queue — while another session's ask,
// an agent's ask and an ask raised after the message stay open.
func TestAMessageToTheSessionAnswersItsAsks(t *testing.T) {
	f := newAttnFixture(t)
	me := attention.Asker{MirrorHost: "ws", MirrorSession: focusSID}
	a := f.mirrorAsk(t, "Publish to npm now?")
	other := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: "3e5f6071-8293-4a41-9dce-3f4a5b6c7d8e"}, "Another session's question?")
	agentAsk := f.ask(t, f.opTok, "An agent's question?")
	waited := f.waitAs(me, a.ID)
	time.Sleep(200 * time.Millisecond)

	rd, hdr := form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"  yes, publish it  "}})
	if w := f.call(t, http.MethodPost, "/mirror/view/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	// The whole result pinned: the answer, and the note that it is only the
	// owner's next message — not necessarily an answer, and not consent.
	note, _ := json.Marshal(answeredInSessionNote)
	if got := recv(t, waited); got != `{"answer":"yes, publish it","id":`+strconv.FormatInt(a.ID, 10)+`,"note":`+string(note)+`,"question":"Publish to npm now?","status":"answered_in_session"}` {
		t.Fatalf("attention_wait returned %s", got)
	}
	if answeredInSessionNote != "your owner wrote to you directly — in your session or your chat — instead of answering in the Attention view, "+
		"and answer is that message (it also reaches you as an ordinary message). It is your owner's NEXT MESSAGE, not necessarily an answer to "+
		"your question and NOT consent: if it does not clearly address the question, ask again with attention_ask" {
		t.Fatalf("the note's wording moved: %q", answeredInSessionNote)
	}
	if desc := attentionToolDefs()[1].Description; !strings.Contains(desc, `(status "answered_in_session": `+answeredInSessionNote+`)`) {
		t.Fatalf("attention_wait's description does not carry the note: %s", desc)
	}
	if st := f.askStatus(t, a.ID); st.Status != attention.AnsweredInSession || st.AnsweredBy != attention.ByOperator {
		t.Fatalf("the ask: %+v", st)
	}
	if st := f.askStatus(t, other.ID); st.Status != attention.Open {
		t.Fatalf("another session's ask: %s", st.Status)
	}
	if st := f.askStatus(t, agentAsk); st.Status != attention.Open {
		t.Fatalf("an agent's ask: %s", st.Status)
	}
	if !containsStr(f.pushes(), "resolved attention-"+attentionAskKey(a.ID)) {
		t.Fatalf("its notification was not cleared: %v", f.pushes())
	}
	if strings.Contains(f.page(t, "/attention/items"), `id="`+attentionAskKey(a.ID)+`"`) {
		t.Fatal("the answered ask is still listed")
	}
	// An ask the session raises AFTER the message is not answered by it.
	later := f.mirrorAsk(t, "And the changelog?")
	if st := f.askStatus(t, later.ID); st.Status != attention.Open {
		t.Fatalf("an ask raised after the message: %s", st.Status)
	}
	// A blank-after-trim message answers nothing (the send itself is refused).
	rd, hdr = form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"   "}})
	f.call(t, http.MethodPost, "/mirror/view/send", rd, merge(f.operator(), hdr))
	if st := f.askStatus(t, later.ID); st.Status != attention.Open {
		t.Fatalf("an empty send answered an ask: %s", st.Status)
	}
}

// TestAQueuedMessageToTheSessionAnswersItsAsks: QUEUEING a message for the
// asking session answers its asks at once — a session blocked in
// attention_wait is busy, so a queued message would not be pasted while the
// ask held it.
func TestAQueuedMessageToTheSessionAnswersItsAsks(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) { ext.SendQueue = sendqueue.NewPG(pool) })
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = 1 AND mirror_host = 'ws'`)
	})
	a := f.mirrorAsk(t, "Rotate the key?")
	waited := f.waitAs(attention.Asker{MirrorHost: "ws", MirrorSession: focusSID}, a.ID)
	time.Sleep(200 * time.Millisecond)
	rd, hdr := form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"rotate it after the deploy"}})
	if w := f.call(t, http.MethodPost, "/send-queue", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	if got := recv(t, waited); !strings.Contains(got, `"status":"answered_in_session"`) || !strings.Contains(got, `"answer":"rotate it after the deploy"`) {
		t.Fatalf("attention_wait returned %s", got)
	}
	// The queue says the message already answered the question…
	list := f.page(t, "/send-queue?host=ws&session=main&window=%401")
	if strings.Count(list, "data-queue-answered") != 1 || !strings.Contains(list, ">Already answered main:1 @ ws&#39;s question.</p>") {
		t.Fatalf("the queue does not say the message answered a question: %s", list)
	}
	var queued int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT answer_queue_id FROM attention_asks WHERE id = $1`, a.ID).Scan(&queued); err != nil || queued <= 0 {
		t.Fatalf("the ask does not name the queued message: %d %v", queued, err)
	}
	// …and its Cancel warns that the answer cannot be un-sent (script and form).
	w := f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(queued, 10)+"/cancel", nil, merge(f.operator(), map[string]string{"X-Requested-With": "XMLHttpRequest"}))
	if w.Code != http.StatusOK || w.Body.String() != "Cancelled — but this message had already answered main:1 @ ws's question, and that answer was delivered: it can't be un-sent." {
		t.Fatalf("cancel: %d %q", w.Code, w.Body)
	}
	// Control: a queued message that answered nothing cancels quietly; as a
	// form, the one that did is redirected with the warning notice.
	rd, hdr = form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"and one more thing"}})
	if w := f.call(t, http.MethodPost, "/send-queue", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("queue 2: %d %s", w.Code, w.Body)
	}
	var second int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT max(id) FROM send_queue WHERE owner_id = 1 AND mirror_host = 'ws'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if w := f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(second, 10)+"/cancel", nil, merge(f.operator(), map[string]string{"X-Requested-With": "XMLHttpRequest"})); w.Code != http.StatusNoContent {
		t.Fatalf("a plain cancel: %d %q", w.Code, w.Body)
	}
	if composerNotices[noticeCancelledAnswered].text != "Cancelled — but this message had already answered a question in that session, and that answer was delivered: it can't be un-sent." {
		t.Fatalf("the form's notice: %q", composerNotices[noticeCancelledAnswered].text)
	}
}

// TestATerminalSendDoesNotAnswerAnAsk: only a message to the asker's CLAUDE
// PANE answers its ask. A Terminal send goes to the window's active pane —
// possibly a shell beside Claude Code — and answers nothing; the control is the
// rendered view's Send to the same window, which does.
func TestATerminalSendDoesNotAnswerAnAsk(t *testing.T) {
	f := newAttnFixture(t)
	a := f.mirrorAsk(t, "Run the migration?")
	rd, hdr := form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"ls -la"}})
	if w := f.call(t, http.MethodPost, "/mirror/terminal/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("terminal send: %d %s", w.Code, w.Body)
	}
	if st := f.askStatus(t, a.ID); st.Status != attention.Open {
		t.Fatalf("a terminal send answered the ask: %s", st.Status)
	}
	rd, hdr = form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"yes"}})
	if w := f.call(t, http.MethodPost, "/mirror/view/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("view send: %d %s", w.Code, w.Body)
	}
	if st := f.askStatus(t, a.ID); st.Status != attention.AnsweredInSession {
		t.Fatalf("control: the view's Send did not answer: %s", st.Status)
	}
}

// TestAMessageToTheAgentAnswersItsAsks: the operator's message to a dispatched
// agent's chat answers that agent's asks — not another agent's. (That no
// message from an agent or from muster itself does is the call-site ledger's:
// TestOnlyTheOperatorsSendsAnswerAsksInSession.)
func TestAMessageToTheAgentAnswersItsAsks(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = &turnGateway{} })
	mine := f.ask(t, f.opTok, "Which branch?")
	theirs := f.ask(t, f.op2Tok, "Which region?")
	waited := f.waitAs(attention.Asker{AgentID: f.op.ID}, mine)
	time.Sleep(200 * time.Millisecond)
	rd, hdr := form(url.Values{"text": {"use main"}})
	if w := f.call(t, http.MethodPost, "/agents/"+f.op.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	note, _ := json.Marshal(answeredInSessionNote)
	if got := recv(t, waited); got != `{"answer":"use main","id":`+strconv.FormatInt(mine, 10)+`,"note":`+string(note)+`,"question":"Which branch?","status":"answered_in_session"}` {
		t.Fatalf("attention_wait returned %s", got)
	}
	if st := f.askStatus(t, theirs); st.Status != attention.Open {
		t.Fatalf("another agent's ask: %s", st.Status)
	}
}

// TestAHostThatDroppedIsListedAndNotifiedAlikeUntilItsGraceEnds: the view and
// the notification follow ONE rule (attentionHostBelieved). While the host is
// gone for less than attentionOfflineGrace its waiting window is still listed
// — marked "host offline" — and its notification stays; past the grace it
// leaves the page AND its notification is cleared (by the sweep's recheck,
// since a host that is gone nudges nothing).
func TestAHostThatDroppedIsListedAndNotifiedAlikeUntilItsGraceEnds(t *testing.T) {
	f := newAttnFixture(t)
	clock := time.Now()
	f.srv.now = func() time.Time { return clock }
	key := sessionKey("ws", "main", "@1")
	f.srv.BroadcastAgentsChangedFor(1) // seeded
	f.setWindows(mirror.StatusIdle)
	f.srv.BroadcastAgentsChangedFor(1)
	clock = clock.Add(attentionRepushAfter + time.Minute)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	// (Pushes are delivered concurrently, so they are counted, not ordered.)
	count := func(line string) int {
		n := 0
		for _, p := range f.pushes() {
			if p == line {
				n++
			}
		}
		return n
	}
	pushedN, clearedN := count("attention attention-"+key), count("resolved attention-"+key)
	if pushedN != 1 {
		t.Fatalf("control: the waiting window was pushed %d times: %v", pushedN, f.pushes())
	}
	since := func() []string { // what came after that push
		var out []string
		for i := clearedN; i < count("resolved attention-"+key); i++ {
			out = append(out, "resolved attention-"+key)
		}
		for i := pushedN; i < count("attention attention-"+key); i++ {
			out = append(out, "attention attention-"+key)
		}
		return out
	}
	f.mirror.mu.Lock()
	f.mirror.online, f.mirror.offlineSince = false, clock
	f.mirror.mu.Unlock()
	f.srv.BroadcastAgentsChangedFor(1)
	clock = clock.Add(attentionOfflineGrace - time.Second)
	f.srv.recheckOfflineHosts()
	page := f.page(t, "/attention/items")
	if !strings.Contains(page, `id="`+key+`"`) || !strings.Contains(page, "data-attention-offline") {
		t.Fatalf("within the grace the dropped host's window left the page: %s", page)
	}
	if got := since(); len(got) != 0 {
		t.Fatalf("within the grace its notification was cleared (or re-sent): %v", got)
	}
	clock = clock.Add(2 * time.Second)
	f.srv.recheckOfflineHosts()
	if page := f.page(t, "/attention/items"); strings.Contains(page, `id="`+key+`"`) {
		t.Fatalf("past the grace the window is still listed: %s", page)
	}
	if got := since(); len(got) != 1 || got[0] != "resolved attention-"+key {
		t.Fatalf("past the grace its notification was not cleared: %v", got)
	}
}

// TestWhatAWaitingWindowWaitsOnArrivingLaterMovesTheOpenViews: a window already
// waiting whose pending action arrives on a later report (the host reads the
// transcript after the status) is the same item — no second push — but its
// substance changed, so the owner's open views are told (the queue's SSE event
// and its wake-up, observed here). The control: a nudge with nothing changed
// tells nobody.
func TestWhatAWaitingWindowWaitsOnArrivingLaterMovesTheOpenViews(t *testing.T) {
	f := newAttnFixture(t)
	f.srv.BroadcastAgentsChangedFor(1) // seeded: waiting, nothing said yet
	woken := func(change func()) bool {
		ch := f.srv.attention.wakeFor(1).wait()
		change()
		f.srv.BroadcastAgentsChangedFor(1)
		select {
		case <-ch:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	if woken(func() {}) {
		t.Fatal("control: a nudge with nothing changed moved the views")
	}
	pushes := len(f.pushes())
	if !woken(func() {
		f.mirror.mu.Lock()
		f.mirror.windows[0].Pending = &mirror.Pending{Summary: "Edit go.mod"}
		f.mirror.mu.Unlock()
	}) {
		t.Fatal("a pending action that arrived later did not move the open views")
	}
	if got := f.pushes(); len(got) != pushes {
		t.Fatalf("the same waiting window was pushed again: %v", got)
	}
}
