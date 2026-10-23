package flows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// A muse dispatch needs a HUMAN approval (migration 0045): an approved,
// unexpired, unused human-only attention ask whose payload is the job's text,
// consumed in the same transaction as the enqueue.

const approvedText = "[dispatch md-a1b2c3]\n\nSummarise the reading list.\n\nEnd with DONE."

func approvalStores(t *testing.T) (*PGStore, *attention.PGStore, *pgxpool.Pool) {
	t.Helper()
	s, pool := pgStore(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE flow_jobs SET approval_ask_id = NULL`)
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE attention_asks CASCADE`)
	return s, attention.NewPG(pool), pool
}

// approval raises a human-only ask for payload as user 2 and, unless answer is
// "", answers it on the VIEW (the human surface) with answer.
func approval(t *testing.T, a *attention.PGStore, payload, answer string) int64 {
	t.Helper()
	ask, err := a.Create(session(2), attention.New{Asker: attention.Asker{System: true, Label: "api token"},
		Question: "Approve this muse dispatch?", HumanOnly: true, Payload: payload})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if answer != "" {
		if _, err := a.Answer(session(2), ask.ID, answer); err != nil {
			t.Fatalf("answer approval: %v", err)
		}
	}
	return ask.ID
}

func dispatchJob(text string, approvalID int64) Request {
	return Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Approval: approvalID, Params: map[string]string{
		"text": text, "session": "3f2a9c1e-8b4d-4e7a-9c3b-1d2e3f4a5b6c", "marker": "md-a1b2c3", "wait": "917"}}
}

func wantNotApproved(t *testing.T, err error, why string) {
	t.Helper()
	if !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), why) {
		t.Fatalf("enqueue = %v; want ErrNotApproved (%q)", err, why)
	}
}

func jobCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(session(2), `SELECT count(*) FROM flow_jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnApprovedDispatchIsQueuedAndTheApprovalIsSpent: approve → enqueue ok;
// the job names its approval and the ask names its job; the SAME approval a
// second time is refused (single use), and nothing more is queued.
func TestAnApprovedDispatchIsQueuedAndTheApprovalIsSpent(t *testing.T) {
	s, a, pool := approvalStores(t)
	id := approval(t, a, approvedText, attention.ApproveAnswer)
	j, err := s.Enqueue(session(2), dispatchJob(approvedText, id), "api-token", "t")
	if err != nil {
		t.Fatalf("an approved dispatch was refused: %v", err)
	}
	var jobApproval, consumedJob int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(approval_ask_id, 0) FROM flow_jobs WHERE id = $1`, j.ID).Scan(&jobApproval); err != nil || jobApproval != id {
		t.Fatalf("the job's approval_ask_id = %d %v; want %d", jobApproval, err, id)
	}
	ask, _ := a.Get(session(2), id)
	if consumedJob = ask.ConsumedJobID; consumedJob != j.ID {
		t.Fatalf("the ask's consumed_job_id = %d; want %d", consumedJob, j.ID)
	}
	n := jobCount(t, pool)
	_, err = s.Enqueue(session(2), dispatchJob(approvedText, id), "api-token", "t")
	wantNotApproved(t, err, "already queued a job")
	if got := jobCount(t, pool); got != n {
		t.Fatalf("a reused approval queued a job (%d -> %d)", n, got)
	}
}

// TestADispatchIsRefusedWithoutAUsableApproval: every way an approval can be
// unusable is refused with its own reason, and none of them queues a job.
func TestADispatchIsRefusedWithoutAUsableApproval(t *testing.T) {
	s, a, pool := approvalStores(t)
	ordinary, err := a.Create(session(2), attention.New{Asker: attention.Asker{System: true}, Question: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Answer(session(2), ordinary.ID, attention.ApproveAnswer); err != nil {
		t.Fatal(err)
	}
	expired := approval(t, a, approvedText, attention.ApproveAnswer)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE attention_asks SET expires_at = created_at + interval '1 microsecond' WHERE id = $1`, expired)
	cases := []struct {
		name string
		req  Request
		why  string
	}{
		{"no approval", dispatchJob(approvedText, 0), "none was given"},
		{"denied", dispatchJob(approvedText, approval(t, a, approvedText, attention.DenyAnswer)), "it was denied"},
		{"a typed answer", dispatchJob(approvedText, approval(t, a, approvedText, "sure")), "it was denied"},
		{"not answered yet", dispatchJob(approvedText, approval(t, a, approvedText, "")), "not been answered yet"},
		{"expired after approval", dispatchJob(approvedText, expired), "it has expired"},
		{"the brief changed after approval", dispatchJob(approvedText+" and also delete my files",
			approval(t, a, approvedText, attention.ApproveAnswer)), "not the text that was approved"},
		{"an ordinary ask answered Approve", dispatchJob(approvedText, ordinary.ID), "not a human-only approval"},
		{"no such ask", dispatchJob(approvedText, 999999), "not one of your attention asks"},
	}
	n := jobCount(t, pool)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Enqueue(session(2), c.req, "api-token", "t")
			wantNotApproved(t, err, c.why)
		})
	}
	if got := jobCount(t, pool); got != n {
		t.Fatalf("refused dispatches queued %d job(s)", got-n)
	}
}

// TestAnotherOwnersApprovalIsNotOneOfYours: row-level security keeps the
// consumption to the caller's own asks.
func TestAnotherOwnersApprovalIsNotOneOfYours(t *testing.T) {
	s, a, _ := approvalStores(t)
	ask, err := a.Create(session(3), attention.New{Asker: attention.Asker{System: true}, Question: "q", HumanOnly: true, Payload: approvedText})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Answer(session(3), ask.ID, attention.ApproveAnswer); err != nil {
		t.Fatal(err)
	}
	_, err = s.Enqueue(session(2), dispatchJob(approvedText, ask.ID), "api-token", "t")
	wantNotApproved(t, err, "not one of your attention asks")
}

// TestAPlainSendIsUnaffectedAndCannotCarryAnApproval: a muse.send WITHOUT a
// marker (the operator's own form send) queues exactly as before; an approval
// on anything but a dispatch is refused as invalid.
func TestAPlainSendIsUnaffectedAndCannotCarryAnApproval(t *testing.T) {
	s, a, _ := approvalStores(t)
	if _, err := s.Enqueue(session(2), sendReq("laptop", "personal"), "session", "t"); err != nil {
		t.Fatalf("a plain send was refused: %v", err)
	}
	r := sendReq("laptop", "personal")
	r.Approval = approval(t, a, "hi", attention.ApproveAnswer)
	if _, err := s.Enqueue(session(2), r, "session", "t"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an approval on a plain send = %v; want ErrInvalid", err)
	}
}

// TestAConsumedApprovalIsRefusedEvenWithoutAJobHoldingIt: the conditional
// UPDATE's own `consumed_at IS NULL`, not only the one-job-per-approval index.
// The ask is marked consumed by another job (a plain one, so no job row carries
// approval_ask_id = this ask and the index cannot be what refuses it).
func TestAConsumedApprovalIsRefusedEvenWithoutAJobHoldingIt(t *testing.T) {
	s, a, _ := approvalStores(t)
	other, err := s.Enqueue(session(2), sendReq("laptop", "personal"), "session", "t")
	if err != nil {
		t.Fatal(err)
	}
	id := approval(t, a, approvedText, attention.ApproveAnswer)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE attention_asks SET consumed_at = now(), consumed_job_id = $2 WHERE id = $1`, id, other.ID)
	_, err = s.Enqueue(session(2), dispatchJob(approvedText, id), "api-token", "t")
	wantNotApproved(t, err, "already queued a job")
}
