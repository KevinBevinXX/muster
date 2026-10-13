package memorelay

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// Key names one memo. An id alone would do against one listener; the scope is carried so two
// scopes can never suppress each other's memos whatever a listener numbers them.
type Key struct {
	Scope string
	ID    int64
}

// Outcome values a [Ledger] row records. Both mean "this agent is not sent these memos again".
const (
	// OutcomeDelivered: the turn carrying the memos completed.
	OutcomeDelivered = "delivered"
	// OutcomeUnconfirmed: the turn was handed to the agent's runtime and then failed, timed out
	// or was cut off — the text may have been pasted, so it is never sent a second time.
	OutcomeUnconfirmed = "unconfirmed"
)

// Ledger is the durable record of which memos each agent has been sent. It is what makes
// "once" survive a restart of this process.
type Ledger interface {
	// Delivered returns every memo agentID has a record for.
	Delivered(ctx context.Context, agentID int64) (map[Key]bool, error)
	// Record appends one row: these memos of scope were sent to a, with outcome.
	Record(ctx context.Context, a agents.Agent, scope string, ids []int64, outcome string) error
}

// AuditAction is the audit_log action a delivery is recorded under, and auditObject the object:
// the agent ROW's id, which is an identity column and so is never reused — a re-dispatched task
// gets a new agent, a new id and therefore every live memo once.
const AuditAction = "memo.delivered"

func auditObject(agentID int64) string { return "agent:" + strconv.FormatInt(agentID, 10) }

// PGLedger keeps the record in audit_log: one row per delivered turn, owned by the operator
// (whose agents these are — see the package doc), action [AuditAction], object the agent, detail
// `{"scope":…,"memos":[…],"outcome":…}`.
//
// 🔴 IT IS AN APPEND-ONLY LOG ON PURPOSE, AND THAT FIXES THE ORDER OF A DELIVERY. A row is written
// AFTER the send, when it is true, and never deleted; there is no claim to take first and undo
// later. So a process KILLED between a send and its row sends those memos once more after it
// restarts. A graceful shutdown is not that case: a cut-off turn is recorded on a detached budget
// before the pool closes ([Relay.Done]).
//
// ⚠ TWO THINGS THIS SHAPE COSTS, STATED: the rows appear in the admin page's recent activity
// (one per delivered turn — an untrusted block injected into an agent is an event an admin may
// want to see), and pruning audit_log rows younger than the longest memo lifetime would deliver
// their memos again. Nothing prunes audit_log today.
type PGLedger struct {
	pool *pgxpool.Pool
}

// NewPGLedger builds the audit_log-backed ledger.
func NewPGLedger(pool *pgxpool.Pool) *PGLedger { return &PGLedger{pool: pool} }

type auditDetail struct {
	Scope   string  `json:"scope"`
	Memos   []int64 `json:"memos"`
	Outcome string  `json:"outcome"`
	Agent   string  `json:"agent"`
}

// Delivered reads the agent's rows. A row whose detail does not parse is skipped, never an
// error: only this package writes the action, and one unreadable row must not stop every
// delivery to the agent.
func (l *PGLedger) Delivered(ctx context.Context, agentID int64) (map[Key]bool, error) {
	rows, err := l.pool.Query(ctx, `SELECT detail FROM audit_log WHERE action = $1 AND object = $2`,
		AuditAction, auditObject(agentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d auditDetail
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		for _, id := range d.Memos {
			out[Key{Scope: d.Scope, ID: id}] = true
		}
	}
	return out, rows.Err()
}

// Record appends the row as the acting principal's own entry (audit_log's self_append policy):
// the relay runs as the operator, and a.OwnerID is the operator's by [Relay]'s own gate.
func (l *PGLedger) Record(ctx context.Context, a agents.Agent, scope string, ids []int64, outcome string) error {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return fmt.Errorf("memorelay: no acting user on the context; the delivery to agent %d is not recorded", a.ID)
	}
	detail, err := json.Marshal(auditDetail{Scope: scope, Memos: ids, Outcome: outcome, Agent: a.Name})
	if err != nil {
		return err
	}
	_, err = l.pool.Exec(ctx, `INSERT INTO audit_log (user_id, via, action, object, detail)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, p.UserID, string(p.Via), AuditAction, auditObject(a.ID), string(detail))
	return err
}
