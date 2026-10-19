package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// AN ANSWER IS DELIVERED TO THE AGENT THAT ASKED (attention_delivery.go,
// migration 0031), end to end on a real Postgres: the operator answers on
// /attention, and the asking agent's DEFAULT session gets one turn carrying the
// answer — or, when it is blocked in attention_wait, the wait returns it and no
// turn is pushed.

// deliveryFixture is the attention fixture with a turn-recording gateway, no
// delivery grace, and a chief of the operator's whose default session is NOT
// its latest one.
type deliveryFixture struct {
	*attnFixture
	gw          *turnGateway
	chief       agents.Agent
	chiefTok    string
	def, newer  agents.ChatSession
	agentsStore *agents.PGStore
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	gw := &turnGateway{}
	f := &deliveryFixture{gw: gw}
	f.attnFixture = newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	noGrace := time.Duration(0)
	f.srv.attention.graceOverride = &noGrace
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.chiefTok = "attn-opchief-token-" + suffix
	f.chief = f.agentFor(t, 1, "attnopchief-"+suffix, "chief", f.chiefTok)
	f.agentsStore = agents.NewPG(f.pool)
	var err error
	if f.def, err = f.agentsStore.CreateSession(asUser(1), f.chief.ID, f.chief.Name); err != nil {
		t.Fatal(err)
	}
	if err := f.agentsStore.SetDefaultSession(asUser(1), f.chief.ID, f.def.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if f.newer, err = f.agentsStore.CreateSession(asUser(1), f.chief.ID, f.chief.Name); err != nil {
		t.Fatal(err)
	}
	if latest, err := f.agentsStore.LatestOrCreateSession(asUser(1), f.chief.ID, f.chief.Name); err != nil || latest.ID != f.newer.ID {
		t.Fatalf("control: the newer session is not the latest (%d, %v): the default-vs-latest case would not be tested", latest.ID, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE agent_id = $1`, f.chief.ID)
	})
	return f
}

// answerForm answers ask id on /attention as the operator (an option or text).
func (f *deliveryFixture) answerForm(t *testing.T, id int64, answer string) {
	t.Helper()
	rd, hdr := form(url.Values{"answer": {answer}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer %d: %d %s", id, w.Code, w.Body)
	}
}

// turnsInto is the turns the gateway saw for the chief, in session sess.
func (f *deliveryFixture) turnsInto(sess agents.ChatSession) []string {
	var out []string
	for _, tn := range f.gw.seen() {
		if strings.HasPrefix(tn, f.chief.Name+" "+sess.SessionKey+" ") {
			out = append(out, tn)
		}
	}
	return out
}

func (f *deliveryFixture) delivery(t *testing.T, id int64) attention.Ask {
	t.Helper()
	f.srv.WaitForAttentionDeliveries()
	return f.askStatus(t, id)
}

// answeredItem is the "Answered" entry of ask id on html, "" when absent.
func answeredItem(html string, id int64) string {
	start := strings.Index(html, `id="answered-`+strconv.FormatInt(id, 10)+`"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(html[start:], "</li>")
	if end < 0 {
		return html[start:]
	}
	return html[start : start+end]
}

// TestAnAnswerToTheChiefIsPushedOnceIntoItsDefaultSession is the bug: the
// chief asks and does NOT wait; the operator answers on /attention; the chief's
// DEFAULT session (not its latest) gets exactly one turn carrying the question
// and the answer, fenced, stored in that session; the item says where it went;
// later sweeps and a direct second delivery push nothing more.
func TestAnAnswerToTheChiefIsPushedOnceIntoItsDefaultSession(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Who should take task 42?", "forward it", "dispatch a new agent")
	f.answerForm(t, id, "dispatch a new agent")
	a := f.delivery(t, id)
	if a.Delivery != attention.DeliveryDelivered || a.DeliveredSession != f.def.ID {
		t.Fatalf("delivery %q into session %d (detail %q); want delivered into the default session %d", a.Delivery, a.DeliveredSession, a.DeliveryDetail, f.def.ID)
	}
	turns := f.turnsInto(f.def)
	if len(turns) != 1 {
		t.Fatalf("turns into the default session: %q (all: %q)", turns, f.gw.seen())
	}
	if other := f.turnsInto(f.newer); len(other) != 0 {
		t.Fatalf("a turn went to the latest, non-default session: %q", other)
	}
	for _, want := range []string{"your owner answered attention #" + strconv.FormatInt(id, 10), "<<<attention-answer untrusted nonce=",
		"| answer:", "|   dispatch a new agent", "|   Who should take task 42?"} {
		if !strings.Contains(turns[0], want) {
			t.Errorf("the turn lacks %q:\n%s", want, turns[0])
		}
	}
	msgs, err := f.agentsStore.ListChatMessages(asUser(1), f.def.ID)
	if err != nil {
		t.Fatal(err)
	}
	var user, reply int
	for _, m := range msgs {
		switch {
		case m.Role == "user" && strings.Contains(m.Content, "your owner answered attention #"+strconv.FormatInt(id, 10)):
			user++
		case m.Role == "assistant" && strings.Contains(m.Content, "answer to: "):
			reply++
		}
	}
	if user != 1 || reply == 0 {
		t.Fatalf("the default session holds %d delivered answer(s) and %d reply part(s); want 1 and some: %+v", user, reply, msgs)
	}
	item := answeredItem(f.page(t, "/attention"), id)
	if !strings.Contains(item, `data-delivery="delivered"`) || !strings.Contains(item, "Delivered to chief") {
		t.Fatalf("the Answered item does not say where the answer went:\n%s", item)
	}
	// Exactly once: two more sweeps, from this process and a "restarted" one,
	// and a direct delivery, push nothing.
	f.srv.deliverDueAnswers(context.Background())
	f.restarted().deliverDueAnswers(context.Background())
	if got := f.srv.deliverAnswer(context.Background(), 1, id); got != answerNotTaken {
		t.Errorf("a second delivery of a delivered answer: %s", got)
	}
	f.srv.WaitForAttentionDeliveries()
	if n := len(f.gw.seen()); n != 1 {
		t.Fatalf("%d turns after the extra sweeps; want 1", n)
	}
}

// restarted is a second server over the same database and extensions: a
// restart (or a second replica) of this one.
func (f *attnFixture) restarted() *Server {
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(f.srv.ext)
	s.attention.graceOverride = f.srv.attention.graceOverride
	return s
}

// TestAWaitingAgentGetsTheAnswerInItsWaitAndNoTurn: blocked in attention_wait,
// the chief gets the answer there; no turn is pushed, even by later sweeps,
// and the item says it was read.
func TestAWaitingAgentGetsTheAnswerInItsWaitAndNoTurn(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Ship it tonight?")
	got := f.waitAs(attention.Asker{AgentID: f.chief.ID}, id)
	eventuallyAPI(t, "the wait to be in flight", func() bool { return f.srv.waitingOn(id) })
	f.answerForm(t, id, "yes, ship it")
	if res := recv(t, got); !strings.Contains(res, `"status":"answered"`) || !strings.Contains(res, "yes, ship it") {
		t.Fatalf("the wait returned %s", res)
	}
	f.srv.deliverDueAnswers(context.Background())
	f.restarted().deliverDueAnswers(context.Background())
	a := f.delivery(t, id)
	if a.Delivery != attention.DeliveryWaited {
		t.Fatalf("delivery %q; want waited", a.Delivery)
	}
	if turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("a waiting agent was also pushed a turn: %q", turns)
	}
	if item := answeredItem(f.page(t, "/attention"), id); !strings.Contains(item, "read it while waiting") {
		t.Fatalf("the Answered item: %s", item)
	}
}

// TestAWaitStillInFlightIsNotPushedOver: an answer that reached the database
// without waking the waiter (another process wrote it; the waiter only polls)
// is not pushed while the wait is in flight here — the wait reads it on its
// next poll, and no turn follows.
func TestAWaitStillInFlightIsNotPushedOver(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Restart the node?")
	got := f.waitAs(attention.Asker{AgentID: f.chief.ID}, id)
	eventuallyAPI(t, "the wait to be in flight", func() bool { return f.srv.waitingOn(id) })
	if _, err := f.attn.Answer(asUser(1), id, "not today"); err != nil { // no askSettled: nothing wakes the waiter
		t.Fatal(err)
	}
	if res := f.srv.deliverAnswer(context.Background(), 1, id); res != answerNotTaken {
		t.Fatalf("a delivery over a wait in flight: %s", res)
	}
	if res := recv(t, got); !strings.Contains(res, "not today") {
		t.Fatalf("the wait returned %s", res)
	}
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryWaited {
		t.Fatalf("delivery %q; want waited", a.Delivery)
	}
	if n := len(f.gw.seen()); n != 0 {
		t.Fatalf("%d turns pushed over a wait in flight", n)
	}
}

// TestARestartMidDeliveryDeliversOnce: (1) an answer whose delivering process
// died mid-turn is settled unconfirmed by the next process and NOT resent; the
// operator's Retry delivers it once. (2) An answer owed when its process died
// before delivering is delivered exactly once by two processes sweeping at
// once.
func TestARestartMidDeliveryDeliversOnce(t *testing.T) {
	f := newDeliveryFixture(t)
	// (1)
	id := f.ask(t, f.chiefTok, "Which region?")
	if _, err := f.attn.Answer(asUser(1), id, "eu-west"); err != nil { // answered by a process that then died
		t.Fatal(err)
	}
	if _, ok, err := f.attn.ClaimDelivery(asUser(1), id, "a-dead-process", time.Millisecond, 0); err != nil || !ok {
		t.Fatalf("claim as the dying process: %v %v", ok, err)
	}
	time.Sleep(20 * time.Millisecond)
	next := f.restarted()
	next.deliverDueAnswers(context.Background())
	next.WaitForAttentionDeliveries()
	if a := f.askStatus(t, id); a.Delivery != attention.DeliveryUnconfirmed || a.DeliveryDetail != attention.DeliveryLapsedDetail {
		t.Fatalf("after the restart: %q %q; want unconfirmed", a.Delivery, a.DeliveryDetail)
	}
	if n := len(f.gw.seen()); n != 0 {
		t.Fatalf("a delivery that may have reached the agent was resent (%d turns)", n)
	}
	item := answeredItem(f.page(t, "/attention"), id)
	if !strings.Contains(item, "Delivery failed: ") || !strings.Contains(item, `action="/attention/asks/`+strconv.FormatInt(id, 10)+`/redeliver"`) {
		t.Fatalf("the failed item offers no Retry:\n%s", item)
	}
	rd, hdr := form(url.Values{})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/redeliver", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryDelivered {
		t.Fatalf("after Retry: %q %q", a.Delivery, a.DeliveryDetail)
	}
	if n := len(f.turnsInto(f.def)); n != 1 {
		t.Fatalf("Retry pushed %d turns; want 1", n)
	}

	// (2)
	id2 := f.ask(t, f.chiefTok, "Which bucket?")
	if _, err := f.attn.Answer(asUser(1), id2, "the archive one"); err != nil {
		t.Fatal(err)
	}
	a, b := f.restarted(), f.restarted()
	var wg sync.WaitGroup
	for _, s := range []*Server{a, b, a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); s.deliverDueAnswers(context.Background()) }()
	}
	wg.Wait()
	a.WaitForAttentionDeliveries()
	b.WaitForAttentionDeliveries()
	if got := f.askStatus(t, id2); got.Delivery != attention.DeliveryDelivered {
		t.Fatalf("the owed answer: %q %q", got.Delivery, got.DeliveryDetail)
	}
	n := 0
	for _, tn := range f.gw.seen() {
		if strings.Contains(tn, "attention #"+strconv.FormatInt(id2, 10)) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("four concurrent sweeps over two processes pushed the owed answer %d times; want 1", n)
	}
}

// TestABusyOrStoppedAgentKeepsTheAnswerQueuedSayingWhy: an agent mid-turn, or
// not running, is not pushed to; the item says why; once it can take it, the
// sweep delivers it.
func TestABusyOrStoppedAgentKeepsTheAnswerQueuedSayingWhy(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Rotate the key now?")
	if !f.srv.turns.enter(f.chief.ID) {
		t.Fatal("control: the gate is closed")
	}
	f.answerForm(t, id, "yes")
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryQueued || a.DeliveryDetail != "the agent is in the middle of a turn" {
		t.Fatalf("while the chief is mid-turn: %q %q", a.Delivery, a.DeliveryDetail)
	}
	if item := answeredItem(f.page(t, "/attention"), id); !strings.Contains(item, "waiting: the agent is in the middle of a turn") {
		t.Fatalf("the queued item does not say why: %s", item)
	}
	f.srv.turns.leave(f.chief.ID)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'stopped' WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryQueued || a.DeliveryDetail != "the agent is not running (stopped)" {
		t.Fatalf("while the chief is stopped: %q %q", a.Delivery, a.DeliveryDetail)
	}
	if n := len(f.gw.seen()); n != 0 {
		t.Fatalf("%d turns to a busy or stopped agent", n)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'running' WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryDelivered || a.DeliveryAttempts != 3 {
		t.Fatalf("once it can take it: %q after %d attempts", a.Delivery, a.DeliveryAttempts)
	}
	if n := len(f.turnsInto(f.def)); n != 1 {
		t.Fatalf("%d turns; want 1", n)
	}
}

// TestARuntimeThatTookNothingKeepsItQueuedAndAFailedTurnIsUnconfirmed: a typed
// "busy" refusal (nothing reached the runtime) leaves the answer queued and
// stores nothing in the session; a turn that failed after it may have reached
// the agent is unconfirmed, never resent on its own.
func TestARuntimeThatTookNothingKeepsItQueuedAndAFailedTurnIsUnconfirmed(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Merge the PR?")
	f.gw.mu.Lock()
	f.gw.err = &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "a turn is running"}
	f.gw.mu.Unlock()
	f.answerForm(t, id, "merge it")
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryQueued || !strings.Contains(a.DeliveryDetail, "took nothing") {
		t.Fatalf("after an unsent turn: %q %q", a.Delivery, a.DeliveryDetail)
	}
	if msgs, _ := f.agentsStore.ListChatMessages(asUser(1), f.def.ID); len(msgs) != 0 {
		t.Fatalf("an unsent delivery was stored in the session: %+v", msgs)
	}
	f.gw.mu.Lock()
	f.gw.err = errors.New("the stream broke mid-reply")
	f.gw.mu.Unlock()
	f.srv.deliverDueAnswers(context.Background())
	a := f.delivery(t, id)
	if a.Delivery != attention.DeliveryUnconfirmed || !strings.Contains(a.DeliveryDetail, "the stream broke mid-reply") {
		t.Fatalf("after a failed turn: %q %q", a.Delivery, a.DeliveryDetail)
	}
	f.gw.mu.Lock()
	f.gw.err = nil
	f.gw.mu.Unlock()
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryUnconfirmed {
		t.Fatalf("an unconfirmed delivery was resent by the sweep: %q", a.Delivery)
	}
	if n := len(f.gw.seen()); n != 2 {
		t.Fatalf("%d turns attempted; want 2 (the unsent one and the failed one)", n)
	}
}

// TestAMirroredOrSystemAsksAnswerIsNotPushed: a mirrored session's answer
// waits for its attention_wait (and is then marked read); a system asker's has
// nothing to deliver. Neither starts a turn.
func TestAMirroredOrSystemAsksAnswerIsNotPushed(t *testing.T) {
	f := newDeliveryFixture(t)
	m := f.mirrorAsk(t, "Run the migration?")
	f.answerForm(t, m.ID, "run it")
	if a := f.delivery(t, m.ID); a.Delivery != attention.DeliveryPull {
		t.Fatalf("a mirrored answer: %q", a.Delivery)
	}
	if item := answeredItem(f.page(t, "/attention"), m.ID); !strings.Contains(item, "to read it: a mirrored session reads its answer itself") {
		t.Fatalf("the mirrored item: %s", item)
	}
	if res := recv(t, f.waitAs(m.Asker, m.ID)); !strings.Contains(res, "run it") {
		t.Fatalf("the session's wait: %s", res)
	}
	if a := f.askStatus(t, m.ID); a.Delivery != attention.DeliveryWaited {
		t.Fatalf("after the session read it: %q", a.Delivery)
	}
	sy := f.askAs(t, attention.Asker{System: true, Label: attention.SystemAskerLabel}, "Task #9 needs you")
	f.answerForm(t, sy.ID, "noted")
	if item := answeredItem(f.page(t, "/attention"), sy.ID); !strings.Contains(item, "nobody to deliver it to") {
		t.Fatalf("the system item: %s", item)
	}
	f.srv.deliverDueAnswers(context.Background())
	f.srv.WaitForAttentionDeliveries()
	if n := len(f.gw.seen()); n != 0 {
		t.Fatalf("%d turns for a mirrored or system ask", n)
	}
}

// TestAnotherOwnerCannotAnswerSeeOrRetryAnAnswer: a member cannot answer the
// operator's agent's ask, see its answer, or retry its delivery; the member's
// own chief's answer is delivered into the member's chief's session, as the
// member.
func TestAnotherOwnerCannotAnswerSeeOrRetryAnAnswer(t *testing.T) {
	f := newDeliveryFixture(t)
	const userHeader = "X-Test-User"
	f.srv.sessionUserOverride = func(r *http.Request) int64 {
		if v, err := strconv.ParseInt(r.Header.Get(userHeader), 10, 64); err == nil && v > 0 {
			return v
		}
		return authz.OperatorUserID
	}
	member := merge(f.operator(), map[string]string{userHeader: strconv.FormatInt(f.member, 10)})
	id := f.ask(t, f.chiefTok, "Who takes the pager this week?")
	rd, hdr := form(url.Values{"answer": {"the member does"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(member, hdr)); w.Code != http.StatusNotFound {
		t.Fatalf("another owner answering: %d", w.Code)
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("another owner's answer landed: %s", a.Status)
	}
	f.answerForm(t, id, "the operator does")
	f.srv.WaitForAttentionDeliveries()
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET delivery = 'unconfirmed', delivery_detail = 'x' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w := f.call(t, http.MethodGet, "/attention", nil, member)
	if w.Code != http.StatusOK || answeredItem(w.Body.String(), id) != "" || strings.Contains(w.Body.String(), "the operator does") {
		t.Fatalf("another owner sees the answer: %d", w.Code)
	}
	if w := f.call(t, http.MethodGet, "/attention/items", nil, member); strings.Contains(w.Body.String(), "the operator does") {
		t.Fatal("another owner's item list carries the answer")
	}
	rd, hdr = form(url.Values{})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/redeliver", rd, merge(member, hdr)); w.Code != http.StatusNotFound {
		t.Fatalf("another owner retrying: %d", w.Code)
	}
	if a := f.askStatus(t, id); a.Delivery != attention.DeliveryUnconfirmed {
		t.Fatalf("another owner's retry moved it: %q", a.Delivery)
	}
	if !strings.Contains(answeredItem(f.page(t, "/attention"), id), "the operator does") {
		t.Fatal("control: the owner does not see their own answer")
	}

	// The member's own chief, answered by the member: delivered as the member.
	mid := f.ask(t, f.attnFixture.chiefTok, "Which repo?")
	rd, hdr = form(url.Values{"answer": {"the docs one"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(mid, 10)+"/answer", rd, merge(member, hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("the member answering their own: %d %s", w.Code, w.Body)
	}
	f.srv.WaitForAttentionDeliveries()
	var got attention.Ask
	got, err := f.attn.Get(asUser(f.member), mid)
	if err != nil || got.Delivery != attention.DeliveryDelivered {
		t.Fatalf("the member's chief's answer: %q %q %v", got.Delivery, got.DeliveryDetail, err)
	}
	sess, err := f.agentsStore.GetSession(asUser(f.member), got.DeliveredSession)
	if err != nil || sess.AgentID != f.attnFixture.chief.ID {
		t.Fatalf("delivered into session %d of agent %d (%v); want the member's chief %d", got.DeliveredSession, sess.AgentID, err, f.attnFixture.chief.ID)
	}
}

// TestAnAnswerAStoppedAgentNeverTakesEndsUnconfirmed: an answer still queued a
// day after it was given (its agent stopped and never came back) stops being
// retried: it settles unconfirmed, saying why, and its item — though older than
// the Answered list's day — still shows it with Retry.
func TestAnAnswerAStoppedAgentNeverTakesEndsUnconfirmed(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Rotate the backups?")
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'stopped' WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	f.answerForm(t, id, "yes")
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryQueued {
		t.Fatalf("a fresh answer to a stopped agent: %q", a.Delivery)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE attention_asks SET resolved_at = now() - interval '25 hours', delivery_queued_at = now() - interval '25 hours', created_at = now() - interval '26 hours', expires_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	f.srv.deliverDueAnswers(context.Background())
	a := f.delivery(t, id)
	if a.Delivery != attention.DeliveryUnconfirmed || !strings.Contains(a.DeliveryDetail, "the agent is not running (stopped)") {
		t.Fatalf("a day on: %q %q; want unconfirmed, saying why", a.Delivery, a.DeliveryDetail)
	}
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryUnconfirmed || a.DeliveryAttempts != 2 {
		t.Fatalf("a given-up answer was retried by the sweep: %q after %d attempts", a.Delivery, a.DeliveryAttempts)
	}
	item := answeredItem(f.page(t, "/attention"), id)
	if !strings.Contains(item, "Delivery failed: ") || !strings.Contains(item, "data-attention-redeliver") {
		t.Fatalf("the given-up answer is not listed with Retry:\n%s", item)
	}
	// Retry starts a new day: with the agent still stopped the answer waits
	// queued again rather than being given up at once; once it runs, it is
	// delivered.
	rd, hdr := form(url.Values{})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/redeliver", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("retry: %d", w.Code)
	}
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryQueued {
		t.Fatalf("a retried answer to a still-stopped agent: %q %q; want queued for a new day", a.Delivery, a.DeliveryDetail)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'running' WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	f.srv.deliverDueAnswers(context.Background())
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryDelivered {
		t.Fatalf("once the agent runs: %q %q", a.Delivery, a.DeliveryDetail)
	}
}

// TestShutdownEndsADeliveryWaitingOutItsGrace: EndStreams ends a delivery
// still in its grace at once (WaitForAttentionDeliveries returns), pushing
// nothing and leaving the answer queued for the next process's sweep.
func TestShutdownEndsADeliveryWaitingOutItsGrace(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.ask(t, f.chiefTok, "Upgrade the cluster?")
	if _, err := f.attn.Answer(asUser(1), id, "after the weekend"); err != nil {
		t.Fatal(err)
	}
	f.srv.startAnswerDelivery(1, id, time.Hour)
	done := make(chan struct{})
	go func() { f.srv.WaitForAttentionDeliveries(); close(done) }()
	f.srv.EndStreams()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a delivery waiting out its grace held the shutdown")
	}
	if a := f.askStatus(t, id); a.Delivery != attention.DeliveryQueued {
		t.Fatalf("after the shutdown: %q", a.Delivery)
	}
	if n := len(f.gw.seen()); n != 0 {
		t.Fatalf("%d turns", n)
	}
}
