package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SYNCHRONOUS REFUSAL, AND THE TWO OPERATIONS IT MUST NOT TOUCH.
//
// 🔴 WHY A PRE-FLIGHT EXISTS AT ALL. Provisioner.Dispatch refuses a kickoff it
// cannot deliver, but createAndDispatchAgent calls it inside safeGo — after the row
// has been created and after this handler has answered — so the refusal reached this
// pod's log and a status colour, and the POST answered 200 over it. The pre-flight
// asks the same question BEFORE the row exists, so the person who clicked is told.
//
// 🔴 AND THE REASON IT NEEDS A TEST OF ITS OWN IS THE SHAPE OF ITS FAILURE. It is
// one `if` in a handler: wrong, it refuses operations that work, and the loudest of
// those is the very escape route the refusal's own text recommends. "Save for later"
// and Start are the two, and a guard that only asserted the refusal would be
// satisfied by a handler that refused all three.
// ---------------------------------------------------------------------------

// preflightProvisioner is a Provisioner that reports a fixed deliverability answer
// and records which lifecycle methods were reached.
//
// ⚠ IT EMBEDS stubProvisioner so the methods this test does not drive answer
// harmlessly, and OVERRIDES the three it does. The embedded
// KickoffUndeliverableReason returns "" (deliverable); this type's own is what makes
// the refusal case expressible.
type preflightProvisioner struct {
	stubProvisioner

	reason string

	mu        sync.Mutex
	dispatch  []bool // one entry per Dispatch, carrying its kickoff argument
	starts    []int64
	dispatchC chan struct{}
	startC    chan struct{}
}

func (p *preflightProvisioner) KickoffUndeliverableReason() string { return p.reason }

func (p *preflightProvisioner) Dispatch(_ context.Context, _ int64, kickoff bool) error {
	p.mu.Lock()
	p.dispatch = append(p.dispatch, kickoff)
	p.mu.Unlock()
	select {
	case p.dispatchC <- struct{}{}:
	default:
	}
	return nil
}

func (p *preflightProvisioner) Start(_ context.Context, id int64) error {
	p.mu.Lock()
	p.starts = append(p.starts, id)
	p.mu.Unlock()
	select {
	case p.startC <- struct{}{}:
	default:
	}
	return nil
}

func (p *preflightProvisioner) dispatches() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.dispatch...)
}

func (p *preflightProvisioner) started() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.starts...)
}

func newPreflightProvisioner(reason string) *preflightProvisioner {
	return &preflightProvisioner{
		reason:    reason,
		dispatchC: make(chan struct{}, 4),
		startC:    make(chan struct{}, 4),
	}
}

// preflightStore is an agents.Store that answers the reads the create path makes
// and records every row it was asked to create.
//
// ⚠ IT EMBEDS THE INTERFACE, so anything the create path starts calling that is not
// below is a nil-interface panic rather than a zero value — the same choice
// stubAgentsStore makes, for the same reason: a handler that grew a new store read
// must fail loudly instead of passing over a stub.
type preflightStore struct {
	agents.Store

	mu      sync.Mutex
	created []agents.Agent
}

func (s *preflightStore) Create(_ context.Context, a agents.Agent) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = int64(len(s.created) + 9001)
	s.created = append(s.created, a)
	return a, nil
}

func (s *preflightStore) NameExists(context.Context, string) (bool, error) { return false, nil }

func (s *preflightStore) List(context.Context) ([]agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.Agent(nil), s.created...), nil
}

func (s *preflightStore) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return nil, nil
}

func (s *preflightStore) rows() []agents.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.Agent(nil), s.created...)
}

// preflightServer wires the create/start routes over the two fixtures above.
func preflightServer(t *testing.T, prov Provisioner, store agents.Store) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{
		UIPassword: testUIPassword,
		HookToken:  testHookToken,
	}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Agents:          store,
		SessionLiveness: stubLiveness{},
		Provisioner:     prov,
	})
	return s, s.Handler()
}

// postAgents drives POST /agents with one form action through the production chain.
func postAgents(t *testing.T, s *Server, h http.Handler, action string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"action": {action}}
	req := httptest.NewRequest(http.MethodPost, "/agents", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestADispatchIsRefusedWhileASaveAndAStartAreNot is the relationship guard.
//
// 🔴 THE THREE CASES ARE ONE CLAIM, NOT THREE. The property an operator depends on
// is that the refusal is NARROW: the one request that cannot be satisfied is
// refused, and the two that can — "Save for later", and Start, which is what
// actually brings a saved agent up — still go through. Measured end to end, that
// save-then-Start pair is the only way an agent has been brought up on this
// deployment, so a pre-flight that caught it would delete the working path in order
// to report a missing one. Asserting the refusal alone would be satisfied by a
// handler that refused every POST.
//
// ⚠ THE SAVE AND START HALVES ARE INVARIANT GUARDS, NOT REGRESSION COVERAGE. The
// defect being fixed is a dispatch that was NOT refused; neither of these two was
// ever refused, so they are green before the change and after it. They are here
// because they are the property the fix could plausibly break, which is a different
// job from witnessing the bug.
func TestADispatchIsRefusedWhileASaveAndAStartAreNot(t *testing.T) {
	// The exact string the adapter returns on the deployment this guards, read from
	// the implementation's own constant rather than retyped: what is under test is
	// the handler's PLUMBING of it, not its wording (the wording has a golden of its
	// own in internal/agentprovision).
	const reason = agentprovision.KickoffRefusalSummary

	t.Run("a dispatch is refused 409 and NOTHING is created", func(t *testing.T) {
		prov := newPreflightProvisioner(reason)
		store := &preflightStore{}
		s, h := preflightServer(t, prov, store)

		rec := postAgents(t, s, h, "dispatch")

		if rec.Code != http.StatusConflict {
			t.Errorf("POST /agents action=dispatch answered %d, want %d.\n"+
				"    A dispatch whose kickoff cannot be delivered has to be refused to the "+
				"caller: the provisioner's own refusal happens in a background goroutine "+
				"after this handler has answered, so without this the operator gets a 2xx, "+
				"an agents list, and a card that reads `running` over an agent nothing told "+
				"what to do.\n    body: %s", rec.Code, http.StatusConflict,
				truncate(rec.Body.String(), 400))
		}
		// 🔴 THE REFUSAL HAS TO CARRY ITS REASON. internal/ui toasts the body
		// verbatim; a bare 409 is a number.
		if !strings.Contains(rec.Body.String(), reason) {
			t.Errorf("the 409 body does not carry the provisioner's reason, so the operator "+
				"is told a number.\n  body:   %s\n  reason: %s",
				truncate(rec.Body.String(), 400), reason)
		}
		// 🔴 AND IT HAS TO HAPPEN BEFORE THE ROW. A refusal that answered 409 after
		// creating the agent would leave a row the operator never asked to keep, and
		// the next render would show a card for it.
		if rows := store.rows(); len(rows) != 0 {
			t.Errorf("the refused dispatch created %d agent row(s): %+v\n"+
				"    The pre-flight must run BEFORE Agents.Create.", len(rows), rows)
		}
		if d := prov.dispatches(); len(d) != 0 {
			t.Errorf("the refused dispatch still reached Provisioner.Dispatch (%v), so the "+
				"pre-flight is not short-circuiting the create path", d)
		}
	})

	t.Run("a save is NOT refused", func(t *testing.T) {
		prov := newPreflightProvisioner(reason)
		store := &preflightStore{}
		s, h := preflightServer(t, prov, store)

		rec := postAgents(t, s, h, "save")

		if rec.Code == http.StatusConflict {
			t.Fatalf("POST /agents action=save was REFUSED %d.\n"+
				"    A save provisions nothing and asks for no kickoff, so there is nothing "+
				"to refuse — and it is the operation the refusal's own remedy text points the "+
				"operator at, so refusing it makes that remedy a lie.\n    body: %s",
				rec.Code, truncate(rec.Body.String(), 400))
		}
		if strings.Contains(rec.Body.String(), reason) {
			t.Errorf("the save's response carries the kickoff refusal, so the pre-flight is "+
				"leaking onto a path that works.\n  body: %s", truncate(rec.Body.String(), 400))
		}
		// POSITIVE CONTROL: the save must have reached the create path, or "not
		// refused" would also be true of a handler that failed earlier for some
		// other reason.
		if rows := store.rows(); len(rows) != 1 {
			t.Fatalf("the save created %d agent row(s), want 1 — this case is not exercising "+
				"the create path it claims to.\n  body: %s", len(rows),
				truncate(rec.Body.String(), 400))
		}
		select {
		case <-prov.dispatchC:
		case <-time.After(5 * time.Second):
			t.Fatal("the save never reached Provisioner.Dispatch")
		}
		if d := prov.dispatches(); len(d) != 1 || d[0] {
			t.Errorf("the save reached Dispatch with kickoff=%v, want exactly one call with "+
				"kickoff=false", d)
		}
	})

	t.Run("a start is NOT refused", func(t *testing.T) {
		prov := newPreflightProvisioner(reason)
		s, h := preflightServer(t, prov, &preflightStore{})

		req := httptest.NewRequest(http.MethodPost, "/agents/9001/start", strings.NewReader(""))
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusConflict {
			t.Fatalf("POST /agents/{id}/start was REFUSED %d.\n"+
				"    Start promises something narrower and entirely deliverable — bring this "+
				"instance up — and it is what provisions an agent that was SAVED. Refusing "+
				"it dead-ends the save-then-Start route, which is the only path an agent has "+
				"been brought up by on this deployment.\n    body: %s",
				rec.Code, truncate(rec.Body.String(), 400))
		}
		select {
		case <-prov.startC:
		case <-time.After(5 * time.Second):
			t.Fatal("the start never reached Provisioner.Start, so this case asserts nothing")
		}
		if got := prov.started(); len(got) != 1 || got[0] != 9001 {
			t.Errorf("Provisioner.Start was called with %v, want exactly [9001]", got)
		}
	})
}

// TestTheCreateHandlersPreflightAgreesWithWhatDispatchWouldDo is the isolation-seam
// check between the pre-flight and the refusal it predicts.
//
// 🔴 TWO ANSWERS TO ONE QUESTION IS THE HAZARD, AND NEITHER PACKAGE'S OWN SUITE CAN
// SEE IT. internal/agentprovision pins that Dispatch refuses when it is told a
// kickoff is undeliverable; this package pins that the handler refuses when the
// provisioner says so. Both stay green if the two answers come from DIFFERENT
// facts — and a pre-flight that disagrees with the dispatch is worse than none: it
// either refuses requests that would have worked, or admits ones that will not and
// restores the 200-over-a-stranded-note observable with an extra guard in front of
// it looking reassuring.
//
// ⚠ IT ASSERTS THE RELATIONSHIP IN BOTH POLARITIES, over the REAL adapter rather
// than a stub, because the stub is the thing that cannot disagree with itself.
func TestTheCreateHandlersPreflightAgreesWithWhatDispatchWouldDo(t *testing.T) {
	spec := agentspec.Config{
		ImageRepo:  "registry.example.test/muster/agent-runtime",
		APIBaseURL: "http://muster.example.test:8105",
	}

	for _, tc := range []struct {
		label       string
		deliverable bool
	}{
		{label: "nothing can deliver a kickoff — the deployed configuration", deliverable: false},
		{label: "a kickoff is deliverable", deliverable: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			store := &seamStore{agent: agents.Agent{
				ID:        4291,
				Name:      "harbour-kestrel",
				Namespace: agents.NamespaceFor(agents.NamespacePrefix, "harbour-kestrel"),
			}}
			adapter, err := agentprovision.New(agentprovision.Config{
				Driver:             provision.MustNewNoop(),
				Store:              store,
				Spec:               spec,
				KickoffDeliverable: tc.deliverable,
				Logger:             log.New(os.Stderr, "", 0),
			})
			if err != nil {
				t.Fatalf("building the adapter: %v", err)
			}

			// What the handler's pre-flight would read...
			reason := adapter.KickoffUndeliverableReason()
			// ...and what the dispatch it predicts actually does.
			dispatchErr := adapter.Dispatch(context.Background(), store.agent.ID, true)
			refused := errors.Is(dispatchErr, agentprovision.ErrKickoffUndeliverable)

			if (reason != "") != refused {
				t.Fatalf("the pre-flight and the dispatch DISAGREE: "+
					"KickoffUndeliverableReason()=%q (refuse=%t) but Dispatch(kickoff=true) "+
					"refused=%t (err=%v).\n"+
					"    They must be two reads of ONE fact. A pre-flight computed from "+
					"anything else than what Dispatch branches on either refuses requests "+
					"that would have worked, or admits ones that cannot.",
					truncate(reason, 120), reason != "", refused, dispatchErr)
			}
			// ⚠ NOTHING MORE IS ASSERTED HERE, AND THE OBVIOUS EXTRA CHECK WOULD BE
			// VACUOUS. "a refusal must carry a non-empty reason" cannot fail: the
			// assertion above reads refusal OFF the reason being non-empty, so the two
			// are the same bit. What the 409's body carries is pinned where it can
			// actually be wrong — TestADispatchIsRefusedWhileASaveAndAStartAreNot, over
			// the response the handler writes.
		})
	}
}

// seamStore answers the reads the adapter makes on a dispatch and swallows the
// writes. It is deliberately NOT a recorder: what this seam asserts is the
// agreement of two answers, and a transcript would invite assertions about the
// adapter's internals that internal/agentprovision already owns.
type seamStore struct {
	agents.Store
	agent agents.Agent
}

func (s *seamStore) Get(context.Context, int64) (agents.Agent, error) { return s.agent, nil }

func (s *seamStore) UpdateStatus(context.Context, int64, string, string, string) error { return nil }

func (s *seamStore) SetHooksToken(_ context.Context, _ int64, token string) error {
	s.agent.HooksToken = token
	return nil
}

func (s *seamStore) SetKickoffError(context.Context, int64, string) error { return nil }
