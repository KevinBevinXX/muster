package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// draftWorld is user 2's plugin and chief with one message delivered to it.
type draftWorld struct {
	s     *PGStore
	dsn   string
	p     Plugin
	chief agents.Agent
	m     Message
}

func newDraftWorld(t *testing.T) draftWorld {
	t.Helper()
	s, _, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	w := draftWorld{s: s, dsn: dsn, p: p, chief: mustAgent(t, dsn, 2, "plg-chief")}
	w.m = w.delivered(t, "m-1", false)
	return w
}

// delivered stores a message for the world's plugin and settles it as delivered
// to the chief.
func (w draftWorld) delivered(t *testing.T, ext string, noReply bool) Message {
	t.Helper()
	in := msg(ext)
	in.NoReply = noReply
	m, _, err := w.s.Submit(plugin(2), w.p.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := w.s.RecordDelivery(plugin(2), m.ID, w.chief.ID, StatusDelivered); err != nil || !ok {
		t.Fatalf("deliver %s: %v %v", ext, ok, err)
	}
	m, _ = w.s.Message(plugin(2), m.ID)
	return m
}

func (w draftWorld) file(t *testing.T, m Message, body string) Draft {
	t.Helper()
	d, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, m.ID, body)
	if err != nil {
		t.Fatalf("file a draft for %d: %v", m.ID, err)
	}
	return d
}

func (w draftWorld) approve(t *testing.T, d Draft) Draft {
	t.Helper()
	got, err := w.s.DecideDraft(session(2), d.ID, true)
	if err != nil || got.Status != DraftApproved || got.ApprovedAt == nil {
		t.Fatalf("approve %d: %+v %v", d.ID, got, err)
	}
	return got
}

// TestADraftIsOnlyForAMessageDeliveredToThatAgent: the chief's draft for its own
// delivered message lands pending; a worker of the same owner, another owner's
// agent, a queued message and a missing one are each refused ErrNotFound and
// nothing is stored.
func TestADraftIsOnlyForAMessageDeliveredToThatAgent(t *testing.T) {
	w := newDraftWorld(t)
	d := w.file(t, w.m, "Thanks, noon works.")
	if d.Status != DraftPending || d.MessageID != w.m.ID || d.AgentID == nil || *d.AgentID != w.chief.ID ||
		d.Body != "Thanks, noon works." || d.ExpiresAt.Sub(d.CreatedAt) != DraftTTL {
		t.Fatalf("filed draft = %+v", d)
	}
	worker := mustAgent(t, w.dsn, 2, "plg-worker")
	other := mustAgent(t, w.dsn, 3, "plg-other")
	queued, _, _ := w.s.Submit(plugin(2), w.p.ID, msg("m-queued"))
	for name, try := range map[string]func() error{
		"a worker of the same owner": func() error {
			_, _, err := w.s.FileDraft(session(2), worker.ID, 2, worker.Name, w.m.ID, "x")
			return err
		},
		"another owner's agent, as that owner": func() error {
			_, _, err := w.s.FileDraft(session(3), other.ID, 3, other.Name, w.m.ID, "x")
			return err
		},
		"another owner's agent claiming this owner": func() error {
			_, _, err := w.s.FileDraft(session(2), other.ID, 2, other.Name, w.m.ID, "x")
			return err
		},
		"a queued message": func() error {
			_, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, queued.ID, "x")
			return err
		},
		"no such message": func() error {
			_, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, 9_000_000, "x")
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v; want ErrNotFound", name, err)
		}
	}
	cards, err := w.s.DraftCards(session(2), 50)
	if err != nil || len(cards) != 1 || cards[0].Draft.ID != d.ID {
		t.Fatalf("drafts after the refusals = %d (%v); want only the chief's one", len(cards), err)
	}
}

// TestANoReplyMessageCannotBeAnswered: a message its plugin marked no_reply is
// refused by the store (ErrNoReply), and — the store-level half — by the
// database itself when a draft row is written directly, by the table owner.
// The mark cannot be lifted afterwards either. CONTROL: the same insert for a
// replyable message lands.
func TestANoReplyMessageCannotBeAnswered(t *testing.T) {
	w := newDraftWorld(t)
	nr := w.delivered(t, "m-noreply", true)
	if !nr.NoReply {
		t.Fatalf("the stored message lost its no_reply mark: %+v", nr)
	}
	if _, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, nr.ID, "hello"); !errors.Is(err, ErrNoReply) {
		t.Fatalf("a draft for a no_reply message: %v; want ErrNoReply", err)
	}
	insert := `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (2, $1, $2, 'x', 'direct', now() + interval '1 day')`
	err := ownerExecErr(t, w.dsn, insert, nr.ID, w.chief.ID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || !strings.Contains(pg.Message, "no_reply") {
		t.Fatalf("a direct insert for a no_reply message: %v; want the 0016 trigger's check_violation", err)
	}
	if err := ownerExecErr(t, w.dsn, insert, w.m.ID, w.chief.ID); err != nil {
		t.Fatalf("CONTROL: the same direct insert for a replyable message: %v", err)
	}
	err = ownerExecErr(t, w.dsn, `UPDATE plugin_messages SET no_reply = false WHERE id = $1`, nr.ID)
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("lifting the no_reply mark: %v; want refused", err)
	}
}

// ownerExecErr is dbtest.OwnerExec returning the error instead of failing.
func ownerExecErr(t *testing.T, dsn, sql string, args ...any) error {
	t.Helper()
	pool, err := pgxConnect(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close(context.Background())
	_, err = pool.Exec(context.Background(), sql, args...)
	return err
}

// TestANewDraftSupersedesThePendingOne and the per-agent cap: a second draft for
// a message replaces the first (superseded = 1); twenty pending is the most one
// agent may have, a replacement of one of them is still allowed, and the 21st
// distinct message is refused.
func TestANewDraftSupersedesThePendingOneAndTheCapIsTwenty(t *testing.T) {
	w := newDraftWorld(t)
	first := w.file(t, w.m, "first")
	second, sup, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, "second")
	if err != nil || sup != 1 {
		t.Fatalf("the second draft: superseded=%d %v; want 1", sup, err)
	}
	if got, _ := w.s.Draft(session(2), first.ID); got.Status != DraftSuperseded || got.DecidedAt == nil {
		t.Fatalf("the first draft is %s; want superseded", got.Status)
	}
	if second.Status != DraftPending {
		t.Fatalf("the second draft is %s", second.Status)
	}
	for i := 2; i <= MaxPendingDraftsPerAgent; i++ {
		w.file(t, w.delivered(t, "cap-"+strconv.Itoa(i), false), "reply "+strconv.Itoa(i))
	}
	// 20 pending now. Replacing one is not a new pending draft.
	if _, sup, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, "third"); err != nil || sup != 1 {
		t.Fatalf("replacing a draft at the cap: superseded=%d %v; want allowed", sup, err)
	}
	extra := w.delivered(t, "cap-extra", false)
	if _, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, extra.ID, "21st"); !errors.Is(err, ErrTooManyPendingDrafts) {
		t.Fatalf("the 21st pending draft: %v; want ErrTooManyPendingDrafts", err)
	}
}

// TestConcurrentDraftsForOneMessageLeaveOnePending: ten racing drafts for one
// message end with exactly one pending, nine superseded, and no error (the
// message-row lock; without it the one-pending index refuses with a 23505).
func TestConcurrentDraftsForOneMessageLeaveOnePending(t *testing.T) {
	w := newDraftWorld(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, "race "+strconv.Itoa(i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a racing draft: %v", err)
		}
	}
	counts := map[DraftStatus]int{}
	cards, _ := w.s.DraftCards(session(2), 50)
	for _, c := range cards {
		counts[c.Draft.Status]++
	}
	if counts[DraftPending] != 1 || counts[DraftSuperseded] != 9 {
		t.Fatalf("after 10 racing drafts: %v; want 1 pending, 9 superseded", counts)
	}
}

// TestTheScreenRefusesWhatTheOwnerCouldNotSeeOrShouldNotSend: a hidden
// character, a host name, a credential-shaped string, the deployment's own deny
// word, an empty body and one over the cap are refused ErrInvalid; plain text
// with accents is accepted (NFKC-folded).
func TestTheScreenRefusesWhatTheOwnerCouldNotSeeOrShouldNotSend(t *testing.T) {
	w := newDraftWorld(t)
	w.s.WithAgentDenyWords([]string{"bigiron"})
	for name, body := range map[string]string{
		"a zero-width space":     "hello​there",
		"a host name":            "see files.example.com for it",
		"a credential":           "use ghp_abcdefghijklmnop to push",
		"the deployment's words": "the bigiron box is down",
		"empty":                  "  \n ",
		"over the cap":           strings.Repeat("a", MaxDraftBytes+1),
		"NUL":                    "a\x00b",
	} {
		if _, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, body); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
	d := w.file(t, w.m, "Café at noon? ﬁne")
	if d.Body != "Café at noon? fine" {
		t.Fatalf("stored body %q; want the NFKC form", d.Body)
	}
}

// TestOnlyAPendingDraftIsDecided: approve and reject act on a pending draft
// once; deciding again, or another owner deciding, is refused.
func TestOnlyAPendingDraftIsDecided(t *testing.T) {
	w := newDraftWorld(t)
	d := w.file(t, w.m, "yes")
	if _, err := w.s.DecideDraft(session(3), d.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner approving: %v; want ErrNotFound", err)
	}
	w.approve(t, d)
	if _, err := w.s.DecideDraft(session(2), d.ID, true); !errors.Is(err, ErrDraftState) {
		t.Fatalf("approving an approved draft: %v; want ErrDraftState", err)
	}
	if _, err := w.s.DecideDraft(session(3), d.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner withdrawing: %v; want ErrNotFound", err)
	}
	r := w.file(t, w.delivered(t, "m-2", false), "no")
	got, err := w.s.DecideDraft(session(2), r.ID, false)
	if err != nil || got.Status != DraftRejected || got.ApprovedAt != nil {
		t.Fatalf("reject: %+v %v", got, err)
	}
	if _, err := w.s.DecideDraft(session(2), r.ID, true); !errors.Is(err, ErrDraftState) {
		t.Fatalf("approving a rejected draft: %v; want ErrDraftState", err)
	}
}

// TestAClaimLeasesOneApprovedDraftOnce is the at-most-once core: an approved
// draft is handed out ONCE — a second claim (and a racing one) gets nothing —
// to its own plugin only, and a pending draft is never handed out.
func TestAClaimLeasesOneApprovedDraftOnce(t *testing.T) {
	w := newDraftWorld(t)
	pending := w.file(t, w.delivered(t, "m-pending", false), "not yet")
	if _, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID); err != nil || ok {
		t.Fatalf("a claim with only a pending draft: ok=%v %v; want nothing", ok, err)
	}
	d := w.approve(t, w.file(t, w.m, "the reply"))
	_, other := mustCreate(t, w.s, 2, "second-inbox")
	if _, ok, _ := w.s.ClaimDraft(plugin(2), other.ID); ok {
		t.Fatal("another plugin of the same owner was handed this plugin's draft")
	}
	c, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID)
	if err != nil || !ok || c.DraftID != d.ID || c.Body != "the reply" || c.MessageExternalID != "m-1" || c.ApprovedAt.IsZero() {
		t.Fatalf("the claim = %+v ok=%v %v", c, ok, err)
	}
	got, _ := w.s.Draft(session(2), d.ID)
	if got.ClaimedAt == nil || got.LeaseUntil == nil || got.LeaseUntil.Sub(*got.ClaimedAt) != LeaseDuration {
		t.Fatalf("the claimed draft = %+v", got)
	}
	if again, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID); err != nil || ok {
		t.Fatalf("a second claim: %+v ok=%v %v; want nothing (a draft is handed out once)", again, ok, err)
	}
	_ = pending
	// Racing claims over five approved drafts: each handed out exactly once.
	want := map[int64]bool{}
	for i := 0; i < 5; i++ {
		want[w.approve(t, w.file(t, w.delivered(t, "race-"+strconv.Itoa(i), false), "r")).ID] = true
	}
	var mu sync.Mutex
	got5 := map[int64]int{}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID)
			if err != nil {
				t.Errorf("racing claim: %v", err)
			}
			if ok {
				mu.Lock()
				got5[c.DraftID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got5) != 5 {
		t.Fatalf("racing claims handed out %v; want each of the 5 once", got5)
	}
	for id, n := range got5 {
		if !want[id] || n != 1 {
			t.Errorf("draft %d handed out %d times", id, n)
		}
	}
}

// TestAResultSettlesOnlyItsOwnPluginsClaimedDraft: sent once; the same result
// again is answered unchanged; the other result conflicts; another plugin of
// the same owner, and another owner's plugin, cannot settle it; an unclaimed
// approved draft cannot be settled.
func TestAResultSettlesOnlyItsOwnPluginsClaimedDraft(t *testing.T) {
	w := newDraftWorld(t)
	unclaimed := w.approve(t, w.file(t, w.delivered(t, "m-unclaimed", false), "u"))
	d := w.approve(t, w.file(t, w.m, "r"))
	if c, _, _ := w.s.ClaimDraft(plugin(2), w.p.ID); c.DraftID != unclaimed.ID {
		t.Fatalf("the oldest approval first: claimed %d, want %d", c.DraftID, unclaimed.ID)
	}
	c, _, _ := w.s.ClaimDraft(plugin(2), w.p.ID)
	if c.DraftID != d.ID {
		t.Fatalf("second claim got %d, want %d", c.DraftID, d.ID)
	}
	_, other := mustCreate(t, w.s, 2, "second-inbox")
	if _, err := w.s.FinishDraft(plugin(2), other.ID, d.ID, DraftResult{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another plugin of the same owner settling it: %v; want ErrNotFound", err)
	}
	_, theirs := mustCreate(t, w.s, 3, "example-inbox")
	if _, err := w.s.FinishDraft(plugin(3), theirs.ID, d.ID, DraftResult{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's plugin settling it: %v; want ErrNotFound", err)
	}
	got, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: true, ExternalRef: "up-123", Detail: "ok"})
	if err != nil || got.Status != DraftSent || got.FinishedAt == nil || got.ExternalRef != "up-123" {
		t.Fatalf("the result: %+v %v", got, err)
	}
	again, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: true, ExternalRef: "different"})
	if err != nil || again.Status != DraftSent || again.ExternalRef != "up-123" {
		t.Fatalf("the same result again: %+v %v; want the stored draft unchanged", again, err)
	}
	if _, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: false}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("the other result: %v; want ErrResultConflict", err)
	}
	fresh := w.approve(t, w.file(t, w.delivered(t, "m-fresh", false), "f"))
	if _, err := w.s.FinishDraft(plugin(2), w.p.ID, fresh.ID, DraftResult{OK: true}); !errors.Is(err, ErrDraftState) {
		t.Fatalf("a result for an unclaimed draft: %v; want ErrDraftState", err)
	}
	var n int
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log
		WHERE action = 'plugin.draft.sent' AND object = $1 AND user_id = 2`, DraftObject(d.ID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("plugin.draft.sent audit rows = %d (%v); want 1", n, err)
	}
}

// ageDraft moves a draft's clock back by secs: its created/expires and any
// claim and lease. The body-immutability trigger guards expires_at, so the
// owner connection skips triggers for this one statement.
func ageDraft(t *testing.T, dsn string, id int64, secs int) {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `SET session_replication_role = replica;
		UPDATE plugin_reply_drafts SET
			created_at = created_at - make_interval(secs => `+strconv.Itoa(secs)+`),
			expires_at = expires_at - make_interval(secs => `+strconv.Itoa(secs)+`),
			claimed_at = claimed_at - make_interval(secs => `+strconv.Itoa(secs)+`),
			lease_until = lease_until - make_interval(secs => `+strconv.Itoa(secs)+`)
		WHERE id = `+strconv.FormatInt(id, 10))
}

// TestALapsedLeaseIsUnconfirmedAndNeverReoffered: a claim whose lease lapses
// without a result becomes `unconfirmed`, audited, and the next claim does NOT
// hand it out again. The owner's explicit re-approval is a new decision (a new
// approved_at, audited) and only then is it offered again. A late result on an
// unconfirmed draft is still recorded.
func TestALapsedLeaseIsUnconfirmedAndNeverReoffered(t *testing.T) {
	w := newDraftWorld(t)
	d := w.approve(t, w.file(t, w.m, "r"))
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("the first claim got nothing")
	}
	// CONTROL: inside the lease, it is still approved and not re-offered.
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatal("a draft was re-offered inside its lease")
	}
	ageDraft(t, w.dsn, d.ID, int(LeaseDuration.Seconds())+5)
	if c, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID); err != nil || ok {
		t.Fatalf("a claim after the lease lapsed: %+v ok=%v %v; want nothing — never re-offered", c, ok, err)
	}
	got, _ := w.s.Draft(session(2), d.ID)
	if got.Status != DraftUnconfirmed {
		t.Fatalf("after the lapse the draft is %s; want unconfirmed", got.Status)
	}
	var n int
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log
		WHERE action = 'plugin.draft.unconfirmed' AND object = $1`, DraftObject(d.ID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("plugin.draft.unconfirmed audit rows = %d (%v); want 1", n, err)
	}
	// Approve (the pending decision) does not apply to it; re-approve does.
	if _, err := w.s.DecideDraft(session(2), d.ID, true); !errors.Is(err, ErrDraftState) {
		t.Fatalf("approving an unconfirmed draft through DecideDraft: %v; want ErrDraftState", err)
	}
	if _, err := w.s.ReapproveDraft(session(3), d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner re-approving: %v; want ErrNotFound", err)
	}
	re, err := w.s.ReapproveDraft(session(2), d.ID)
	if err != nil || re.ID == d.ID || re.Status != DraftApproved || re.ClaimedAt != nil || re.Body != d.Body ||
		re.MessageID != d.MessageID || !re.ExpiresAt.Equal(got.ExpiresAt) || re.ApprovedAt == nil || !re.ApprovedAt.After(*got.ApprovedAt) {
		t.Fatalf("re-approve: %+v %v; want a NEW approved draft with the same text, message and expiry", re, err)
	}
	if old, _ := w.s.Draft(session(2), d.ID); old.Status != DraftUnconfirmed || old.ClaimedAt == nil ||
		old.ReapprovedAs == nil || *old.ReapprovedAs != re.ID {
		t.Fatalf("the re-approved row: %+v; want still unconfirmed, still claimed, naming %d", old, re.ID)
	}
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log
		WHERE action = 'plugin.draft.reapproved' AND object = $1 AND user_id = 2`, DraftObject(d.ID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("plugin.draft.reapproved audit rows = %d (%v); want 1", n, err)
	}
	if _, err := w.s.ReapproveDraft(session(2), d.ID); !errors.Is(err, ErrDraftState) {
		t.Fatalf("re-approving the same unconfirmed draft twice: %v; want ErrDraftState", err)
	}
	c2, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID)
	if !ok || c2.DraftID != re.ID || c2.Body != d.Body {
		t.Fatalf("after re-approval the NEW draft is offered: ok=%v %+v; want draft %d", ok, c2, re.ID)
	}
	// 🔴 A LATE result from the FIRST claim lands on the first row, never on the
	// second claim's: the second claim's own result is then still accepted.
	if late, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: false, Detail: "timed out"}); err != nil || late.Status != DraftFailed {
		t.Fatalf("the first claim's late result: %+v %v; want the first row failed", late, err)
	}
	if cur, _ := w.s.Draft(session(2), re.ID); cur.Status != DraftApproved || cur.ClaimedAt == nil {
		t.Fatalf("the first claim's late result touched the re-approval: %+v", cur)
	}
	if got, err := w.s.FinishDraft(plugin(2), w.p.ID, re.ID, DraftResult{OK: true}); err != nil || got.Status != DraftSent {
		t.Fatalf("the second claim's result: %+v %v; want sent", got, err)
	}
	if _, err := w.s.ReapproveDraft(session(2), re.ID); !errors.Is(err, ErrDraftState) {
		t.Fatalf("re-approving a sent draft: %v; want ErrDraftState", err)
	}
}

// TestALateSentCancelsTheUnclaimedReapproval: the original's late "sent" means
// the text reached the sender, so its re-approved copy — not yet claimed — is
// expired, audited, and never handed out. CONTROL: a late "failed" leaves the
// copy approved (covered in TestALapsedLeaseIsUnconfirmedAndNeverReoffered).
func TestALateSentCancelsTheUnclaimedReapproval(t *testing.T) {
	w := newDraftWorld(t)
	d := w.approve(t, w.file(t, w.m, "r"))
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("claim")
	}
	ageDraft(t, w.dsn, d.ID, int(LeaseDuration.Seconds())+5)
	copyOf, err := w.s.ReapproveDraft(session(2), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: true}); err != nil || got.Status != DraftSent {
		t.Fatalf("the late sent: %+v %v", got, err)
	}
	if got, _ := w.s.Draft(session(2), copyOf.ID); got.Status != DraftExpired {
		t.Fatalf("the re-approved copy is %s after the original was reported sent; want expired", got.Status)
	}
	if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatalf("the cancelled copy was handed out: %+v", c)
	}
}

// TestALateSentCancelsEveryHopOfARechain: draft 1 lapses and is approved again
// as 2; 2 lapses and is approved again as 3. Draft 1's late "sent" cancels 3,
// which no claim has taken (2 is unconfirmed and stays so).
func TestALateSentCancelsEveryHopOfARechain(t *testing.T) {
	w := newDraftWorld(t)
	lapse := func(d Draft) {
		t.Helper()
		if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok || c.DraftID != d.ID {
			t.Fatalf("claim %d: %+v %v", d.ID, c, ok)
		}
		ageDraft(t, w.dsn, d.ID, int(LeaseDuration.Seconds())+5)
	}
	d1 := w.approve(t, w.file(t, w.m, "r"))
	lapse(d1)
	d2, err := w.s.ReapproveDraft(session(2), d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	lapse(d2)
	d3, err := w.s.ReapproveDraft(session(2), d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.FinishDraft(plugin(2), w.p.ID, d1.ID, DraftResult{OK: true}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]DraftStatus{d1.ID: DraftSent, d2.ID: DraftUnconfirmed, d3.ID: DraftExpired} {
		if got, _ := w.s.Draft(session(2), id); got.Status != want {
			t.Errorf("draft %d is %s; want %s", id, got.Status, want)
		}
	}
	if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatalf("a two-hop re-approval was handed out after the original was sent: %+v", c)
	}
}

// TestAnApprovalCanBeWithdrawnUntilItIsClaimed: an approved draft no plugin has
// taken can be withdrawn (rejected, approval time cleared, audited), which also
// unblocks the message; a claimed one cannot.
func TestAnApprovalCanBeWithdrawnUntilItIsClaimed(t *testing.T) {
	w := newDraftWorld(t)
	d := w.approve(t, w.file(t, w.m, "oops"))
	got, err := w.s.DecideDraft(session(2), d.ID, false)
	if err != nil || got.Status != DraftRejected || got.ApprovedAt != nil {
		t.Fatalf("withdraw: %+v %v", got, err)
	}
	var n int
	if err := w.s.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log
		WHERE action = 'plugin.draft.withdrawn' AND object = $1`, DraftObject(d.ID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("withdrawn audit rows = %d (%v)", n, err)
	}
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatal("a withdrawn draft was handed out")
	}
	d2 := w.approve(t, w.file(t, w.m, "corrected"))
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("claim the corrected draft")
	}
	if _, err := w.s.DecideDraft(session(2), d2.ID, false); !errors.Is(err, ErrDraftState) {
		t.Fatalf("withdrawing a claimed draft: %v; want ErrDraftState", err)
	}
	// The card flag: a pending draft whose message has an unconfirmed sibling.
	ageDraft(t, w.dsn, d2.ID, int(LeaseDuration.Seconds())+5)
	d3 := w.file(t, w.m, "third")
	cards, err := w.s.DraftCards(session(2), 50)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[int64]bool{}
	for _, c := range cards {
		flagged[c.Draft.ID] = c.EarlierUnconfirmed
	}
	if !flagged[d3.ID] || flagged[d2.ID] {
		t.Fatalf("EarlierUnconfirmed = %v; want true for %d only", flagged, d3.ID)
	}
}

// TestWithdrawingAReapprovalFreesTheOriginal: the original of a withdrawn
// re-approval no longer names it and may be approved again.
func TestWithdrawingAReapprovalFreesTheOriginal(t *testing.T) {
	w := newDraftWorld(t)
	d := w.approve(t, w.file(t, w.m, "r"))
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("claim")
	}
	ageDraft(t, w.dsn, d.ID, int(LeaseDuration.Seconds())+5)
	cp, err := w.s.ReapproveDraft(session(2), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.s.DecideDraft(session(2), cp.ID, false); err != nil || got.Status != DraftRejected {
		t.Fatalf("withdraw the copy: %+v %v", got, err)
	}
	if orig, _ := w.s.Draft(session(2), d.ID); orig.ReapprovedAs != nil {
		t.Fatalf("the original still names the withdrawn copy: %+v", orig)
	}
	if _, err := w.s.ReapproveDraft(session(2), d.ID); err != nil {
		t.Fatalf("re-approving the original after its copy was withdrawn: %v", err)
	}
}

// TestOneReplyPerMessageIsInFlight: while a reply to a message is approved and
// unsettled, no other draft for that message may be filed or approved; once it
// has settled (sent), a new one may. Rejecting is never blocked.
func TestOneReplyPerMessageIsInFlight(t *testing.T) {
	w := newDraftWorld(t)
	d1 := w.approve(t, w.file(t, w.m, "first"))
	if _, _, err := w.s.FileDraft(session(2), w.chief.ID, 2, w.chief.Name, w.m.ID, "second"); !errors.Is(err, ErrReplyInFlight) {
		t.Fatalf("filing while a reply is approved: %v; want ErrReplyInFlight", err)
	}
	// Another message's draft is unaffected.
	other := w.file(t, w.delivered(t, "m-other", false), "other")
	if _, err := w.s.DecideDraft(session(2), other.ID, true); err != nil {
		t.Fatalf("approving a draft for ANOTHER message: %v", err)
	}
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("claim d1")
	}
	if _, err := w.s.FinishDraft(plugin(2), w.p.ID, d1.ID, DraftResult{OK: true}); err != nil {
		t.Fatal(err)
	}
	d2 := w.file(t, w.m, "a follow-up after the first was sent")
	if _, err := w.s.DecideDraft(session(2), d2.ID, true); err != nil {
		t.Fatalf("approving a follow-up after the first settled: %v", err)
	}
	// Approving while another reply is in flight: d3 is pending for m-race when
	// a reply to m-race is already approved (written directly — through the
	// store it can only arise from the race lockMessage serialises).
	race := w.delivered(t, "m-race", false)
	d3 := w.file(t, race, "three")
	dbtest.OwnerExec(t, w.dsn, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at,
		status, decided_at, approved_at) VALUES (2, $1, $2, 'x', 'already approved', now() + interval '1 day', 'approved', now(), now())`,
		race.ID, w.chief.ID)
	if _, err := w.s.DecideDraft(session(2), d3.ID, true); !errors.Is(err, ErrReplyInFlight) {
		t.Fatalf("approving while another reply is in flight: %v; want ErrReplyInFlight", err)
	}
	if got, err := w.s.DecideDraft(session(2), d3.ID, false); err != nil || got.Status != DraftRejected {
		t.Fatalf("rejecting while another reply is in flight: %+v %v; want rejected", got, err)
	}
}

// TestDraftsExpireAfterSevenDays: a pending draft and an approved-but-unclaimed
// one older than DraftTTL are expired by the next sweep and never claimed; one
// a day younger is untouched (the control).
func TestDraftsExpireAfterSevenDays(t *testing.T) {
	w := newDraftWorld(t)
	oldPending := w.file(t, w.m, "old pending")
	oldApproved := w.approve(t, w.file(t, w.delivered(t, "m-oa", false), "old approved"))
	young := w.file(t, w.delivered(t, "m-young", false), "young")
	ageDraft(t, w.dsn, oldPending.ID, int(DraftTTL.Seconds())+60)
	ageDraft(t, w.dsn, oldApproved.ID, int(DraftTTL.Seconds())+60)
	ageDraft(t, w.dsn, young.ID, int(DraftTTL.Seconds())-24*3600)
	if c, ok, err := w.s.ClaimDraft(plugin(2), w.p.ID); err != nil || ok {
		t.Fatalf("an expired approved draft was claimed: %+v %v", c, err)
	}
	for id, want := range map[int64]DraftStatus{oldPending.ID: DraftExpired, oldApproved.ID: DraftExpired, young.ID: DraftPending} {
		if got, _ := w.s.Draft(session(2), id); got.Status != want {
			t.Errorf("draft %d is %s; want %s", id, got.Status, want)
		}
	}
	if got, _ := w.s.Draft(session(2), oldApproved.ID); got.ApprovedAt == nil {
		t.Error("an approved draft that expired lost its approval time")
	}
	if _, err := w.s.DecideDraft(session(2), oldPending.ID, true); !errors.Is(err, ErrDraftState) {
		t.Fatalf("approving an expired draft: %v; want ErrDraftState", err)
	}
}

// TestQueuedForOwnerIsTheOwnersLivePluginsOnly: the retry list is the owner's
// queued messages across live plugins, oldest first — not another owner's, not
// a revoked plugin's, not a delivered one.
func TestQueuedForOwnerIsTheOwnersLivePluginsOnly(t *testing.T) {
	w := newDraftWorld(t)
	q1, _, _ := w.s.Submit(plugin(2), w.p.ID, msg("q-1"))
	_, second := mustCreate(t, w.s, 2, "second-inbox")
	q2, _, _ := w.s.Submit(plugin(2), second.ID, msg("q-2"))
	_, revoked := mustCreate(t, w.s, 2, "gone-inbox")
	_, _, _ = w.s.Submit(plugin(2), revoked.ID, msg("q-gone"))
	if _, err := w.s.Revoke(session(2), revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, theirs := mustCreate(t, w.s, 3, "example-inbox")
	_, _, _ = w.s.Submit(plugin(3), theirs.ID, msg("q-theirs"))
	got, err := w.s.QueuedForOwner(authz.System(context.Background()), 2, 50)
	if err != nil || len(got) != 2 || got[0].ID != q1.ID || got[1].ID != q2.ID {
		t.Fatalf("QueuedForOwner = %v %v; want [%d %d]", ids(got), err, q1.ID, q2.ID)
	}
	counts, err := w.s.QueuedCounts(session(2))
	if err != nil || counts[w.p.ID] != 1 || counts[second.ID] != 1 || counts[theirs.ID] != 0 {
		t.Fatalf("QueuedCounts = %v %v", counts, err)
	}
}

func ids(ms []Message) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func pgxConnect(dsn string) (*pgx.Conn, error) { return pgx.Connect(context.Background(), dsn) }
