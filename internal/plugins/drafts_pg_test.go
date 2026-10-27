package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// draftWorld is user 2's plugin and target with one message delivered to it.
type draftWorld struct {
	s      *PGStore
	dsn    string
	p      Plugin
	target agents.Agent
	m      Message
}

func newDraftWorld(t *testing.T) draftWorld {
	t.Helper()
	s, _, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	w := draftWorld{s: s, dsn: dsn, p: p, target: mustAgent(t, dsn, 2, "plg-target")}
	w.m = w.delivered(t, "m-1", false)
	return w
}

// delivered stores a message for the world's plugin and settles it as delivered
// to the target.
func (w draftWorld) delivered(t *testing.T, ext string, noReply bool) Message {
	t.Helper()
	in := msg(ext)
	in.NoReply = noReply
	m, _, err := w.s.Submit(plugin(2), w.p.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := w.s.BeginDelivery(plugin(2), m.ID, w.target.ID); err != nil || !ok {
		t.Fatalf("begin delivery %s: %v %v", ext, ok, err)
	}
	if ok, err := w.s.RecordDelivery(plugin(2), m.ID, w.target.ID, StatusDelivered); err != nil || !ok {
		t.Fatalf("deliver %s: %v %v", ext, ok, err)
	}
	m, _ = w.s.Message(plugin(2), m.ID)
	return m
}

func (w draftWorld) file(t *testing.T, m Message, body string) Draft {
	t.Helper()
	d, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, m.ID, body)
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

// TestADraftIsOnlyForAMessageDeliveredToThatAgent: the target's draft for its own
// delivered message lands pending; a worker of the same owner, another owner's
// agent, a queued message and a missing one are each refused ErrNotFound and
// nothing is stored.
func TestADraftIsOnlyForAMessageDeliveredToThatAgent(t *testing.T) {
	w := newDraftWorld(t)
	d := w.file(t, w.m, "Thanks, noon works.")
	if d.Status != DraftPending || d.MessageID != w.m.ID || d.AgentID == nil || *d.AgentID != w.target.ID ||
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
			_, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, queued.ID, "x")
			return err
		},
		"no such message": func() error {
			_, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, 9_000_000, "x")
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v; want ErrNotFound", name, err)
		}
	}
	cards, err := w.s.DraftCards(session(2), 50)
	if err != nil || len(cards) != 1 || cards[0].Draft.ID != d.ID {
		t.Fatalf("drafts after the refusals = %d (%v); want only the target's one", len(cards), err)
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
	if _, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, nr.ID, "hello"); !errors.Is(err, ErrNoReply) {
		t.Fatalf("a draft for a no_reply message: %v; want ErrNoReply", err)
	}
	insert := `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (2, $1, $2, 'x', 'direct', now() + interval '1 day')`
	err := ownerExecErr(t, w.dsn, insert, nr.ID, w.target.ID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || !strings.Contains(pg.Message, "no_reply") {
		t.Fatalf("a direct insert for a no_reply message: %v; want the 0016 trigger's check_violation", err)
	}
	if err := ownerExecErr(t, w.dsn, insert, w.m.ID, w.target.ID); err != nil {
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
	second, sup, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, w.m.ID, "second")
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
	if _, sup, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, w.m.ID, "third"); err != nil || sup != 1 {
		t.Fatalf("replacing a draft at the cap: superseded=%d %v; want allowed", sup, err)
	}
	extra := w.delivered(t, "cap-extra", false)
	if _, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, extra.ID, "21st"); !errors.Is(err, ErrTooManyPendingDrafts) {
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
			_, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, w.m.ID, "race "+strconv.Itoa(i))
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
		if _, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, w.m.ID, body); !errors.Is(err, ErrInvalid) {
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
// hand it out again — not even after an approve attempt. A late result on an
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
	// Approve (the pending decision) does not apply to it: an unconfirmed draft
	// is never offered again, whatever its owner does.
	if _, err := w.s.DecideDraft(session(2), d.ID, true); !errors.Is(err, ErrDraftState) {
		t.Fatalf("approving an unconfirmed draft: %v; want ErrDraftState", err)
	}
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); ok {
		t.Fatal("an unconfirmed draft was handed out after an approve attempt")
	}
	// A LATE result is still recorded on it.
	if late, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: false, Detail: "timed out"}); err != nil || late.Status != DraftFailed {
		t.Fatalf("the late result: %+v %v; want failed", late, err)
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

// TestOneReplyPerMessageIsInFlight: while a reply to a message is approved and
// unsettled, no other draft for that message may be filed or approved; once it
// has settled (sent), a new one may. Rejecting is never blocked.
func TestOneReplyPerMessageIsInFlight(t *testing.T) {
	w := newDraftWorld(t)
	d1 := w.approve(t, w.file(t, w.m, "first"))
	if _, _, err := w.s.FileDraft(session(2), w.target.ID, 2, w.target.Name, w.m.ID, "second"); !errors.Is(err, ErrReplyInFlight) {
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
		race.ID, w.target.ID)
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

// TestAResultTakesTheMessageLockFirst: FinishDraft waits for a transaction
// holding the message row (the lock every draft write takes first), so a late
// result and a filing, approval or withdrawal on the same message are serialised in
// one order instead of deadlocking. CONTROL: with the lock released it lands.
func TestAResultTakesTheMessageLockFirst(t *testing.T) {
	w := newDraftWorld(t)
	d := w.approve(t, w.file(t, w.m, "r"))
	if _, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok {
		t.Fatal("claim")
	}
	conn, err := pgxConnect(w.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM plugin_messages WHERE id = $1 FOR UPDATE`, w.m.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: true})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("FinishDraft finished (%v) while the message row was locked; want it to wait for the message lock", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FinishDraft after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FinishDraft did not finish after the message lock was released")
	}
}

// TestALateSentCancelsEveryOtherReplyToTheMessage is the hole round 0 found: draft
// 1 is claimed and its lease lapses (unconfirmed); the target files draft 2 for the
// same message and the owner approves it; THEN the plugin reports draft 1 sent.
// The sender has the reply, so draft 2 — approved, unclaimed — must not go too:
// it is superseded in the same transaction, audited, and never handed out. A
// PENDING draft of a message whose earlier draft is reported sent late is
// superseded likewise.
//
// CONTROLS: a late "failed" cancels nothing (its message's approved draft stays
// approved); a sibling a plugin has already CLAIMED is left to its own result;
// an approved draft for ANOTHER message is untouched; and the final claim is
// live (it hands out a draft — just never draft 2).
func TestALateSentCancelsEveryOtherReplyToTheMessage(t *testing.T) {
	w := newDraftWorld(t)
	sys := authz.System(context.Background())
	m2, m3, m4 := w.delivered(t, "m-2", false), w.delivered(t, "m-3", false), w.delivered(t, "m-4", false)

	// One claimed draft per message, each claim then lapsing.
	var claimed []Draft
	for _, m := range []Message{w.m, m2, m3, m4} {
		d := w.approve(t, w.file(t, m, "first for "+m.ExternalID))
		if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok || c.DraftID != d.ID {
			t.Fatalf("claim the draft for %s: %+v ok=%v", m.ExternalID, c, ok)
		}
		ageDraft(t, w.dsn, d.ID, int(LeaseDuration.Seconds())+5)
		claimed = append(claimed, d)
	}
	d1, d3, d5, d7 := claimed[0], claimed[1], claimed[2], claimed[3]
	// m4: a second draft approved AND claimed (inside its lease) before draft 7's
	// late sent arrives.
	d8 := w.approve(t, w.file(t, m4, "eighth, claimed"))
	if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok || c.DraftID != d8.ID {
		t.Fatalf("claim draft 8: %+v ok=%v", c, ok)
	}
	// After the lapses: m gets an APPROVED second draft, m2 a PENDING one, m3 an
	// approved one (for the "failed" control); and another message an approved one.
	d2 := w.approve(t, w.file(t, w.m, "second"))
	d4 := w.file(t, m2, "fourth, pending")
	d6 := w.approve(t, w.file(t, m3, "sixth"))
	other := w.approve(t, w.file(t, w.delivered(t, "m-other", false), "other"))
	if got, _ := w.s.Draft(session(2), d1.ID); got.Status != DraftUnconfirmed {
		t.Fatalf("draft 1 after its lease lapsed: %s; want unconfirmed", got.Status)
	}

	for _, d := range []Draft{d1, d3, d7} {
		if got, err := w.s.FinishDraft(plugin(2), w.p.ID, d.ID, DraftResult{OK: true, ExternalRef: "up"}); err != nil || got.Status != DraftSent {
			t.Fatalf("draft %d's late sent: %+v %v; want sent", d.ID, got, err)
		}
	}
	if got, err := w.s.FinishDraft(plugin(2), w.p.ID, d5.ID, DraftResult{OK: false, Detail: "upstream said no"}); err != nil || got.Status != DraftFailed {
		t.Fatalf("draft 5's late failed: %+v %v; want failed", got, err)
	}
	for _, c := range []struct {
		d    Draft
		want DraftStatus
		why  string
	}{
		{d2, DraftSuperseded, "an approved draft of a message whose earlier draft was reported sent"},
		{d4, DraftSuperseded, "a pending draft of a message whose earlier draft was reported sent"},
		{d6, DraftApproved, "the control: a late FAILED cancels nothing"},
		{other, DraftApproved, "the control: another message's draft"},
		{d8, DraftApproved, "the control: a sibling a plugin already claimed"},
	} {
		if got, _ := w.s.Draft(session(2), c.d.ID); got.Status != c.want || (c.d.ID == d8.ID && got.ClaimedAt == nil) {
			t.Fatalf("%s is %s; want %s", c.why, got.Status, c.want)
		}
	}
	for sent, cancelled := range map[int64]int64{d1.ID: d2.ID, d3.ID: d4.ID} {
		var n int
		if err := w.s.pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'plugin.draft.superseded'
			AND object = $1 AND detail->>'sent_draft_id' = $2`, DraftObject(cancelled), strconv.FormatInt(sent, 10)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("plugin.draft.superseded audit rows for draft %d: %d (%v); want 1 naming draft %d", cancelled, n, err, sent)
		}
	}
	// Draft 2 was approved FIRST of the live ones, so a claim query that still
	// saw it would hand it out now.
	if c, ok, _ := w.s.ClaimDraft(plugin(2), w.p.ID); !ok || c.DraftID == d2.ID {
		t.Fatalf("the next claim: %+v ok=%v; want a live claim that is NOT draft 2 (%d)", c, ok, d2.ID)
	}
}

// TestTheDraftTriggersHoldUnderTheScopeAllPrincipal (moved here with the table,
// from 0015 to 0016): even a principal that sees every row cannot rewrite a
// draft's body, name another owner's agent, file a draft with no agent, or
// approve one without an approval time; the agent's deletion keeps the draft
// (agent_id NULL); and the table is under FORCED row-level security, so a bare
// connection sees none of it.
func TestTheDraftTriggersHoldUnderTheScopeAllPrincipal(t *testing.T) {
	w := newDraftWorld(t)
	sys := authz.System(context.Background())
	theirs := mustAgent(t, w.dsn, 3, "plg-theirs")
	check := func(name, sql string, args ...any) {
		t.Helper()
		_, err := w.s.pool.Exec(sys, sql, args...)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != "23514" && pg.Code != "23503") {
			t.Errorf("%s: err = %v; want a check or foreign-key violation", name, err)
		}
	}
	var draft int64
	if err := w.s.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (2, $1, $2, 'target', 'reply', now() + interval '7 days') RETURNING id`, w.m.ID, w.target.ID).Scan(&draft); err != nil {
		t.Fatalf("a draft by the owner's own agent: %v", err)
	}
	check("draft body", `UPDATE plugin_reply_drafts SET body = 'swapped' WHERE id = $1`, draft)
	check("draft by another owner's agent", `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (2, $1, $2, 'x', 'r', now() + interval '1 day')`, w.m.ID, theirs.ID)
	check("draft with no agent", `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (2, $1, NULL, 'x', 'r', now() + interval '1 day')`, w.m.ID)
	check("draft approved without a time", `UPDATE plugin_reply_drafts SET status = 'approved' WHERE id = $1`, draft)
	check("draft pending WITH an approval time", `UPDATE plugin_reply_drafts SET approved_at = now() WHERE id = $1`, draft)
	// What MAY change does: a status move; superseded may keep an approval time.
	if _, err := w.s.pool.Exec(sys, `UPDATE plugin_reply_drafts SET status = 'approved', approved_at = now(), decided_at = now() WHERE id = $1`, draft); err != nil {
		t.Fatalf("approving a draft: %v", err)
	}
	if _, err := w.s.pool.Exec(sys, `UPDATE plugin_reply_drafts SET status = 'superseded' WHERE id = $1`, draft); err != nil {
		t.Fatalf("superseding an approved draft, keeping its approval time: %v", err)
	}
	if _, err := w.s.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, w.target.ID); err != nil {
		t.Fatalf("deleting the agent a draft names: %v", err)
	}
	var stillThere int
	if err := w.s.pool.QueryRow(sys, `SELECT count(*) FROM plugin_reply_drafts WHERE id = $1 AND agent_id IS NULL`, draft).Scan(&stillThere); err != nil || stillThere != 1 {
		t.Fatalf("the draft after its agent's deletion: %d (%v); want kept with agent_id NULL", stillThere, err)
	}
	var n int
	if err := w.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM plugin_reply_drafts`).Scan(&n); err != nil || n != 0 {
		t.Errorf("plugin_reply_drafts with no principal: %d rows (%v); want 0", n, err)
	}
	var forced bool
	if err := w.s.pool.QueryRow(sys, `SELECT relrowsecurity AND relforcerowsecurity FROM pg_class
		WHERE relname = 'plugin_reply_drafts'`).Scan(&forced); err != nil || !forced {
		t.Fatalf("plugin_reply_drafts FORCED row-level security: %v (%v); want true", forced, err)
	}
}
