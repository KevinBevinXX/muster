package flows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// AGENT REQUESTS (migration 0014). A cluster agent asks for a catalogue flow
// through the MCP tool browser_flow_request; muster files a flow_requests row
// (`pending`) owned by the AGENT'S OWNER. Nothing runs until that owner, in a
// browser session on /flows, approves it — which creates the flow job, in the
// owner's own queue, in the same transaction. A denial or an expiry never
// creates a job, and there is no agent-side decide path at all.
//
// 🔴 A MEMBER'S AGENT CAN ONLY EVER FEED ITS OWNER'S QUEUE. The row's owner_id
// is the agent's owner (pinned by a trigger, 0014), the
// approval runs as the approving session's principal and row-level security
// shows a request to its owner only, and the job it creates lands owned by
// that same principal — so only that owner's runner tokens can claim it.

// RequestStatus is a request's state. The set is closed (the 0014 CHECK).
type RequestStatus string

const (
	RequestPending  RequestStatus = "pending"
	RequestApproved RequestStatus = "approved"
	RequestDenied   RequestStatus = "denied"
	RequestExpired  RequestStatus = "expired"
)

// FlowRequest is one row of flow_requests.
type FlowRequest struct {
	ID      int64 `json:"id"`
	OwnerID int64 `json:"ownerId"`
	// AgentID is nil once the agent has been deleted: the request stays, as
	// the record of what was asked and decided (0014: ON DELETE SET NULL).
	AgentID   *int64            `json:"agentId,omitempty"`
	AgentName string            `json:"agentName"`
	Flow      string            `json:"flow"`
	Host      string            `json:"host"`
	Instance  string            `json:"instance"`
	Params    map[string]string `json:"params"`
	Reason    string            `json:"reason"`
	Status    RequestStatus     `json:"status"`
	JobID     *int64            `json:"jobId,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	ExpiresAt time.Time         `json:"expiresAt"`
	DecidedAt *time.Time        `json:"decidedAt,omitempty"`
}

// MaxPendingRequestsPerAgent bounds an agent's undecided requests — the
// connector store's per-agent shape (connectors.MaxPendingGrantsPerAgent).
const MaxPendingRequestsPerAgent = 3

// MaxReasonRunes caps an agent's stated reason.
const MaxReasonRunes = 300

// RequestTTL is how long a request waits for its owner's decision before it
// expires (and can no longer be approved).
const RequestTTL = time.Hour

// ErrTooManyPendingRequests: the agent already has MaxPendingRequestsPerAgent
// undecided requests.
var ErrTooManyPendingRequests = errors.New("flows: too many pending flow requests from this agent")

// agentTextDeny is the defence-in-depth screen on text an AGENT composes for a
// third party (muse.send goes to Meta): an IP address, a host name with a
// dot, a token-shaped string, or one of this deployment's infrastructure
// words. 🔴 IT IS A HEURISTIC, NOT A PROOF AND NOT THE GATE — a prompt-injected
// agent can paraphrase anything past a regex, and a look-alike letter from
// another script walks past an ASCII pattern. THE CONTROL IS THE OWNER'S
// APPROVAL OF THE EXACT VISIBLE TEXT (screen.go): values are NFKC-normalised,
// may carry no character the owner cannot see, and the card spells out every
// non-ASCII code point. This list only refuses the obvious shapes before
// they reach that screen.
var agentTextDeny = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), "an IP address"},
	{regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:lan|local|internal|svc|cluster|dev|com|net|org|io|ai|app)\b`), "a host name"},
	{regexp.MustCompile(`(?i)\b(?:mst|mfr|ghp|gho|ghs|sk|xox[abprs])[-_][A-Za-z0-9_-]{8,}`), "a credential-shaped string"},
	{regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`), "a long token-shaped string"},
	{regexp.MustCompile(`(?i)\b(?:kubeconfig|kubectl|namespace|password|secret)\b`), "an infrastructure word"},
}

// AgentTextRefusal reports why agent-composed free text is refused, or "".
// words are the deployment's OWN infrastructure names (cluster, host and
// project names — MUSTER_FLOW_AGENT_DENY_WORDS): they are configuration, not
// source, because this repository is public and may not name them.
func AgentTextRefusal(text string, words []string) string {
	for _, d := range agentTextDeny {
		if d.re.MatchString(text) {
			return "the text carries " + d.what + "; an agent may not send that to a third party"
		}
	}
	low := strings.ToLower(text)
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && containsWord(low, w) {
			return "the text carries an infrastructure name; an agent may not send that to a third party"
		}
	}
	return ""
}

// containsWord reports whether w occurs in s on word boundaries.
func containsWord(s, w string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(w) + `\b`).MatchString(s)
}

const requestCols = `id, owner_id, agent_id, agent_name, flow, host, instance, params, reason, status, job_id,
	created_at, expires_at, decided_at`

func scanRequest(row pgx.Row) (FlowRequest, error) {
	var q FlowRequest
	var params []byte
	err := row.Scan(&q.ID, &q.OwnerID, &q.AgentID, &q.AgentName, &q.Flow, &q.Host, &q.Instance, &params,
		&q.Reason, &q.Status, &q.JobID, &q.CreatedAt, &q.ExpiresAt, &q.DecidedAt)
	if err != nil {
		return FlowRequest{}, err
	}
	q.Params = map[string]string{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &q.Params); err != nil {
			return FlowRequest{}, fmt.Errorf("flows: request %d params: %w", q.ID, err)
		}
	}
	return q, nil
}

// sweepRequests expires the caller's undecided requests past their TTL.
func (s *PGStore) sweepRequests(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE flow_requests SET status = 'expired', decided_at = now()
		WHERE status = 'pending' AND expires_at <= now()`)
	return err
}

// FileRequest records an agent's request, owned by the agent's owner. It
// validates against the catalogue AND screens agent-composed free text.
func (s *PGStore) FileRequest(ctx context.Context, agentID, ownerID int64, agentName string, r Request, reason string) (FlowRequest, error) {
	if ownerID <= 0 {
		return FlowRequest{}, invalidf("the agent has no owner")
	}
	r.TTL = 0 // an agent does not choose a job ttl; the approval does
	// Everything an agent composed is normalised FIRST, so what is validated,
	// screened, stored, shown on the card and sent is one string.
	norm := make(map[string]string, len(r.Params))
	for k, v := range r.Params {
		norm[k] = NormalizeAgentText(v)
	}
	r.Params = norm
	f, err := Validate(&r)
	if err != nil {
		return FlowRequest{}, err
	}
	// 🔴 EVERY PARAMETER IS SCREENED, DRIVEN BY THE CATALOGUE — not a named key.
	// (It screened only `text`; `thread` carried anything.) No value may hold
	// a character the approver cannot see, and every parameter that is not a
	// closed identifier (Param.ID) goes through the deny patterns too. A
	// parameter added to the catalogue later is screened by default.
	for _, p := range f.Params {
		v, ok := r.Params[p.Name]
		if !ok {
			continue
		}
		if why := HiddenCharRefusal(v); why != "" {
			return FlowRequest{}, invalidf("parameter %q: %s", p.Name, why)
		}
		if !p.ID {
			if why := AgentTextRefusal(v, s.denyWords); why != "" {
				return FlowRequest{}, invalidf("parameter %q: %s", p.Name, why)
			}
		}
	}
	// The reason is shown to the owner and never sent: one line, capped, and
	// under the same no-invisible-characters rule.
	reason = NormalizeAgentText(reason)
	if why := HiddenCharRefusal(reason); why != "" { // before the collapse, which would hide a hidden space
		return FlowRequest{}, invalidf("reason: %s", why)
	}
	reason = truncateRunes(strings.Join(strings.Fields(reason), " "), MaxReasonRunes)
	params, err := json.Marshal(r.Params)
	if err != nil {
		return FlowRequest{}, err
	}
	if err := s.sweepRequests(ctx); err != nil {
		return FlowRequest{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FlowRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('flow_requests.file'), $1::int)`, int32(agentID)); err != nil {
		return FlowRequest{}, err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM flow_requests WHERE agent_id = $1 AND status = 'pending'`,
		agentID).Scan(&pending); err != nil {
		return FlowRequest{}, err
	}
	if pending >= MaxPendingRequestsPerAgent {
		return FlowRequest{}, fmt.Errorf("%w: %d awaiting the owner (the limit is %d); wait for a decision",
			ErrTooManyPendingRequests, pending, MaxPendingRequestsPerAgent)
	}
	q, err := scanRequest(tx.QueryRow(ctx, `
		INSERT INTO flow_requests (owner_id, agent_id, agent_name, flow, host, instance, params, reason, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9))
		RETURNING `+requestCols, ownerID, agentID, truncateRunes(agentName, 100), r.Flow, r.Host, r.Instance,
		params, reason, RequestTTL.Seconds()))
	if err != nil {
		return FlowRequest{}, err
	}
	if err := auditRequest(ctx, tx, "flow.request.filed", "agent", q, nil); err != nil {
		return FlowRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FlowRequest{}, err
	}
	count(q.Flow, "requested")
	return q, nil
}

// RequestForAgent returns one of AGENT agentID's own requests.
func (s *PGStore) RequestForAgent(ctx context.Context, agentID, id int64) (FlowRequest, error) {
	if err := s.sweepRequests(ctx); err != nil {
		return FlowRequest{}, err
	}
	q, err := scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestCols+` FROM flow_requests WHERE id = $1 AND agent_id = $2`, id, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FlowRequest{}, ErrNotFound
	}
	return q, err
}

// ListRequests returns the caller's newest requests.
func (s *PGStore) ListRequests(ctx context.Context, limit int) ([]FlowRequest, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if err := s.sweepRequests(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+requestCols+` FROM flow_requests ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowRequest{}
	for rows.Next() {
		q, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// DecideRequest approves or denies one of the caller's pending, unexpired
// requests. Approval creates the job (queued in the caller's own queue,
// DefaultTTL from now) in the SAME transaction, so an approved request always
// has its job and a job never exists without its approval. via/by are the
// job's audit facts.
func (s *PGStore) DecideRequest(ctx context.Context, id int64, approve bool, by string) (FlowRequest, *Job, error) {
	if err := s.sweepRequests(ctx); err != nil {
		return FlowRequest{}, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FlowRequest{}, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestCols+` FROM flow_requests WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return FlowRequest{}, nil, ErrNotFound
	}
	if err != nil {
		return FlowRequest{}, nil, err
	}
	if q.Status != RequestPending || !q.ExpiresAt.After(time.Now()) {
		return q, nil, ErrState
	}
	var job *Job
	if approve {
		r := Request{Flow: q.Flow, Host: q.Host, Instance: q.Instance, Params: q.Params}
		f, err := Validate(&r) // the catalogue at DECISION time, not filing time
		if err != nil {
			return q, nil, err
		}
		params, err := json.Marshal(r.Params)
		if err != nil {
			return q, nil, err
		}
		j, err := enqueueTx(ctx, tx, f, r, params, "agent",
			fmt.Sprintf("agent %s (request %d), approved by %s", q.AgentName, q.ID, by))
		if err != nil {
			return q, nil, err
		}
		job = &j
		q, err = scanRequest(tx.QueryRow(ctx, `UPDATE flow_requests SET status = 'approved', job_id = $2, decided_at = now()
			WHERE id = $1 RETURNING `+requestCols, id, j.ID))
		if err != nil {
			return FlowRequest{}, nil, err
		}
	} else {
		q, err = scanRequest(tx.QueryRow(ctx, `UPDATE flow_requests SET status = 'denied', decided_at = now()
			WHERE id = $1 RETURNING `+requestCols, id))
		if err != nil {
			return FlowRequest{}, nil, err
		}
	}
	action := "flow.request.denied"
	if approve {
		action = "flow.request.approved"
	}
	if err := auditRequest(ctx, tx, action, "session", q, job); err != nil {
		return FlowRequest{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FlowRequest{}, nil, err
	}
	if job != nil {
		count(job.Flow, "enqueued")
	}
	count(q.Flow, string(q.Status))
	return q, job, nil
}

// auditRequest writes one audit_log row for a request's filing or decision, in
// the SAME transaction as the change it records. The row names ids, the flow,
// the agent and where it would run — and for each parameter only a sha256 and
// a length, NEVER the text: the audit log is not a second copy of a message
// bound for a third party.
func auditRequest(ctx context.Context, tx pgx.Tx, action, via string, q FlowRequest, job *Job) error {
	params := map[string]any{}
	for k, v := range q.Params {
		sum := sha256.Sum256([]byte(v))
		params[k] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "runes": utf8.RuneCountInString(v)}
	}
	detail := map[string]any{"request": q.ID, "flow": q.Flow, "agent": q.AgentName, "host": q.Host,
		"instance": q.Instance, "params": params}
	if q.AgentID != nil {
		detail["agent_id"] = *q.AgentID
	}
	if job != nil {
		detail["job"] = job.ID
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (user_id, via, action, object, detail) VALUES ($1, $2, $3, $4, $5)`,
		q.OwnerID, via, action, fmt.Sprintf("flow_request:%d", q.ID), raw)
	return err
}
