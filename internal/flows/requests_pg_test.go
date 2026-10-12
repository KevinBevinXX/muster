package flows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// seedAgent inserts an agent owned by uid and returns its id.
func seedAgent(t *testing.T, s *PGStore, uid int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(session(uid), `INSERT INTO agents (name, namespace, hooks_token) VALUES ($1, $2, $3) RETURNING id`,
		name, "ns-"+name, "tok-"+name).Scan(&id); err != nil {
		t.Fatalf("seed agent %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, id)
	})
	return id
}

func agentCtx(uid int64) context.Context { return as(uid, authz.ViaConnect) }

func TestAnAgentRequestIsOwnedByTheAgentsOwnerAndDecidedOnlyByThem(t *testing.T) {
	s, pool := pgStore(t)
	agent := seedAgent(t, s, 2, "req-agent-two")
	q, err := s.FileRequest(agentCtx(2), agent, 2, "req-agent-two", pollReq("laptop", "personal"), "because")
	if err != nil || q.OwnerID != 2 || q.Status != RequestPending || q.JobID != nil {
		t.Fatalf("filed %+v %v", q, err)
	}
	// An agent reads only its OWN requests, even another agent of the same owner.
	sibling := seedAgent(t, s, 2, "req-agent-sibling")
	if _, err := s.RequestForAgent(agentCtx(2), sibling, q.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a sibling agent read the request: %v; want ErrNotFound", err)
	}
	if got, err := s.RequestForAgent(agentCtx(2), agent, q.ID); err != nil || got.ID != q.ID {
		t.Fatalf("control: the asking agent reads its request: %+v %v", got, err)
	}
	// Another owner — a member or the admin — cannot see or decide it.
	for _, uid := range []int64{3, authz.OperatorUserID} {
		if _, _, err := s.DecideRequest(session(uid), q.ID, true, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user %d approving user 2's agent's request = %v; want ErrNotFound", uid, err)
		}
		if l, err := s.ListRequests(session(uid), 50); err != nil || len(l) != 0 {
			t.Fatalf("user %d lists %d requests (%v); want none", uid, len(l), err)
		}
	}
	var jobs int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM flow_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("%d jobs exist before any approval (%v)", jobs, err)
	}
	// The owner approves: the job is created in the OWNER's queue, via "agent".
	dq, job, err := s.DecideRequest(session(2), q.ID, true, "owner")
	if err != nil || job == nil || dq.Status != RequestApproved || dq.JobID == nil || *dq.JobID != job.ID {
		t.Fatalf("approve = %+v %+v %v", dq, job, err)
	}
	if job.OwnerID != 2 || job.RequestedVia != "agent" || !strings.Contains(job.RequestedBy, "req-agent-two") ||
		job.Status != StatusQueued || job.Params["thread"] != "isolation canary" {
		t.Fatalf("the approved job = %+v", job)
	}
	// Another owner's runner on the same host and instance claims nothing; the
	// owner's runner claims it.
	if _, ok, err := s.Claim(runner(3), "laptop", []string{"personal"}, 0); err != nil || ok {
		t.Fatalf("user 3's runner claimed user 2's agent's job (ok=%v %v)", ok, err)
	}
	if c, ok, err := s.Claim(runner(2), "laptop", []string{"personal"}, 0); err != nil || !ok || c.ID != job.ID {
		t.Fatalf("control: the owner's runner = %+v ok=%v %v", c, ok, err)
	}
	if _, _, err := s.DecideRequest(session(2), q.ID, false, "owner"); !errors.Is(err, ErrState) {
		t.Fatalf("deciding an approved request again = %v; want ErrState", err)
	}
}

func TestADeniedOrExpiredRequestNeverYieldsAJob(t *testing.T) {
	s, pool := pgStore(t)
	agent := seedAgent(t, s, 2, "req-agent-deny")
	q, err := s.FileRequest(agentCtx(2), agent, 2, "req-agent-deny", pollReq("laptop", "personal"), "")
	if err != nil {
		t.Fatal(err)
	}
	dq, job, err := s.DecideRequest(session(2), q.ID, false, "owner")
	if err != nil || job != nil || dq.Status != RequestDenied || dq.JobID != nil {
		t.Fatalf("deny = %+v %+v %v", dq, job, err)
	}
	if _, _, err := s.DecideRequest(session(2), q.ID, true, "owner"); !errors.Is(err, ErrState) {
		t.Fatalf("approving a DENIED request = %v; want ErrState", err)
	}
	e, err := s.FileRequest(agentCtx(2), agent, 2, "req-agent-deny", pollReq("laptop", "personal"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE flow_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRequest(session(2), e.ID, true, "owner"); !errors.Is(err, ErrState) {
		t.Fatalf("approving an EXPIRED request = %v; want ErrState", err)
	}
	l, err := s.ListRequests(session(2), 10)
	if err != nil || len(l) != 2 || l[0].Status != RequestExpired {
		t.Fatalf("list after expiry = %+v %v; want the newest expired", l, err)
	}
	var jobs int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM flow_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("%d jobs exist after a deny and an expiry (%v); want 0", jobs, err)
	}
}

func TestThePendingRequestCapIsPerAgent(t *testing.T) {
	s, _ := pgStore(t)
	a1 := seedAgent(t, s, 2, "req-agent-cap1")
	a2 := seedAgent(t, s, 2, "req-agent-cap2")
	for i := 0; i < MaxPendingRequestsPerAgent; i++ {
		if _, err := s.FileRequest(agentCtx(2), a1, 2, "a1", pollReq("laptop", "personal"), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FileRequest(agentCtx(2), a1, 2, "a1", pollReq("laptop", "personal"), ""); !errors.Is(err, ErrTooManyPendingRequests) {
		t.Fatalf("a request past the cap = %v; want ErrTooManyPendingRequests", err)
	}
	if _, err := s.FileRequest(agentCtx(2), a2, 2, "a2", pollReq("laptop", "personal"), ""); err != nil {
		t.Fatalf("another agent is not capped by the first: %v", err)
	}
}

func TestARequestCannotNameAnOwnerOtherThanTheAgents(t *testing.T) {
	s, _ := pgStore(t)
	agent := seedAgent(t, s, 2, "req-agent-owner")
	// Under System (the kickoff loop's context) RLS would allow any owner; the
	// 0014 owner trigger (the agent's owner, read from agents) refuses it.
	if _, err := s.FileRequest(authz.System(context.Background()), agent, 3, "x", pollReq("laptop", "personal"), ""); err == nil {
		t.Fatal("a request for user 2's agent was filed as owned by user 3")
	}
	if _, err := s.FileRequest(authz.System(context.Background()), agent, 2, "x", pollReq("laptop", "personal"), ""); err != nil {
		t.Fatalf("control: the agent's real owner: %v", err)
	}
}

func TestAgentComposedTextIsScreened(t *testing.T) {
	s, _ := pgStore(t)
	s.WithAgentDenyWords([]string{"bigiron"}) // the deployment's own names come from configuration
	agent := seedAgent(t, s, 2, "req-agent-text")
	for _, bad := range []string{
		"ping 192.0.2.10 please", "see api.example.com", "token mst_AAAAAAAAAAAA",
		"key ghp_abcdefghij", strings.Repeat("Ab3", 12), "the dev kubeconfig is broken",
		"what runs on bigiron", "is Bigiron up?",
	} {
		r := sendReq("laptop", "personal")
		r.Params["text"] = bad
		if _, err := s.FileRequest(agentCtx(2), agent, 2, "t", r, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("agent text %q = %v; want ErrInvalid", bad, err)
		}
	}
	ok := sendReq("laptop", "personal")
	ok.Params["text"] = "What did you think of the plan we discussed yesterday?"
	if _, err := s.FileRequest(agentCtx(2), agent, 2, "t", ok, ""); err != nil {
		t.Fatalf("control: plain text refused: %v", err)
	}
	// The OWNER's own enqueue is not screened: the screen is for agents.
	own := sendReq("laptop", "personal")
	own.Params["text"] = "ping 192.0.2.10 please"
	if _, err := s.Enqueue(session(2), own, "session", ""); err != nil {
		t.Fatalf("the owner's own text was screened: %v", err)
	}
}
