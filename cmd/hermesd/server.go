package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// sessionKeyHeader is the header muster's client names a conversation with on
// every turn (internal/agents.sessionKeyHeader: the gateway runtime's spelling,
// a third party's wire, which muster sends to every kind).
const sessionKeyHeader = "X-Openclaw-Session-Key"

// runtimeHeader names another runtime in the pod (internal/agents.HeaderRuntime);
// a hermes pod runs none.
const runtimeHeader = "X-Muster-Runtime"

type server struct {
	cfg    config
	bearer string
	api    *hermesAPI
	store  *store
	relay  *relay
	keyFP  string // the fingerprint of the API key on the volume now
	logf   func(string, ...any)
	now    func() time.Time

	// sup is the gateway's supervisor; nil in a test that drives turns against
	// a fake API server with no process (then the gateway counts as ready).
	sup *supervisor
	// checkout is the agent's repository; nil when it was given none.
	checkout *podagent.Checkout

	// watchPoll is how often a run's status is read when its stream cannot be.
	watchPoll time.Duration
	// turnTimeout is one turn's budget (turnTimeout; shorter in tests).
	turnTimeout time.Duration

	mu   sync.Mutex
	live map[string]bool // muster session keys with a turn being served now
	// refusedSince is when each session's recovery was first DEFINITELY
	// refused in this gateway life (sessionFailed); reset at every start.
	refusedSince map[string]time.Time
	// retireAfter is retireAfter (shorter in tests).
	retireAfter time.Duration
	// tombstones are idempotency keys Hermes refused whose close could not be
	// written yet (closeRefused): never re-posted. Value: the close's notice.
	tombstones map[string]string
	// closing is set when hermesd is shutting down: turns are refused.
	closing bool
}

func newServer(cfg config, bearer, apiKey string, st *store) *server {
	return &server{
		cfg: cfg, bearer: bearer, store: st, keyFP: keyFingerprint(apiKey),
		api:   newHermesAPI(cfg.apiBase(), apiKey),
		relay: newRelay(cfg), logf: log.Printf, now: time.Now, watchPoll: 2 * time.Second, turnTimeout: turnTimeout,
		live: map[string]bool{}, refusedSince: map[string]time.Time{}, retireAfter: retireAfter, tombstones: map[string]string{},
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	// `/` is the same check: muster's default agent health path is "/".
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /{$}", s.handleHealthz)
	return mux
}

// gatewayState is the supervisor's state, or ready when there is none.
func (s *server) gatewayState() supervisorState {
	if s.sup == nil {
		return supervisorState{State: stReady}
	}
	return s.sup.state()
}

// readiness says why a turn cannot be served now, or "" when it can.
func (s *server) readiness() string {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return "hermesd is shutting down"
	}
	if s.checkout != nil && !s.checkout.Ready() {
		return s.checkout.NotReady()
	}
	switch st := s.gatewayState(); st.State {
	case stReady:
		return ""
	case stRecovering, stRecoveryStuck:
		return "the Hermes gateway is recovering: runs a previous gateway left unsettled are being resolved " +
			"before any turn is accepted"
	default:
		why := "the Hermes gateway is " + strings.ReplaceAll(st.State, "_", " ")
		if st.Detail != "" {
			why += ": " + st.Detail
		}
		return why
	}
}

// health is /healthz's body.
type health struct {
	OK      bool                    `json:"ok"`
	State   string                  `json:"state"`
	Detail  string                  `json:"detail,omitempty"`
	Gateway supervisorState         `json:"gateway"`
	Repo    *podagent.CheckoutState `json:"repo,omitempty"`
}

// handleHealthz answers 200 unless the gateway latched (a crash loop, or a .env
// it refuses to start with): a pod waiting for its checkout, starting or
// recovering is healthy and SAYS so, so the liveness probe never restarts it for
// waiting. `state` is the gate turns use: anything but `ready` is not_ready.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	g := s.gatewayState()
	h := health{OK: !g.failed(), State: g.State, Detail: g.Detail, Gateway: g}
	if s.checkout != nil {
		st := s.checkout.State()
		h.Repo = &st
		if st.State != podagent.CheckoutReady {
			h.State, h.Detail = stWaitingCheckout, s.checkout.NotReady()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !h.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(h)
}

// instructions is muster's instructions for every run: the file agentspec
// places outside every volume. An absent file is no instructions.
func (s *server) instructions() (string, error) {
	b, err := os.ReadFile(s.cfg.Instructions)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// responsesRequest is the subset of muster's /v1/responses body hermesd reads.
// Its `instructions` is NOT used: muster's instructions for a hermes agent are
// the file (instructions()), as a claude-code agent's are its managed CLAUDE.md.
type responsesRequest struct {
	Input  json.RawMessage `json:"input"`
	Stream *bool           `json:"stream"`
}

// claim marks key's session as served by this process; false when it already
// is (a second muster request on the same session).
func (s *server) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[key] {
		return false
	}
	s.live[key] = true
	return true
}

func (s *server) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, key)
}

func (s *server) isLive(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[key]
}

// resetRecovery starts a gateway life's recovery afresh: every session's
// retireAfter floor starts again.
func (s *server) resetRecovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusedSince = map[string]time.Time{}
}

func (s *server) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// sleepCtx waits d or until ctx ends; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
