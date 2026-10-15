package plugins

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/flows"
)

// REPLY DRAFTS (plugin_reply_drafts, migrations 0015 + 0016): the chief drafts a
// reply to a plugin message (the reply_draft tool), its OWNER approves or rejects
// the exact text on /plugins, and the plugin then CLAIMS an approved draft, sends
// it upstream, and reports the result. muster never sends anything itself.
//
// The life of a draft:
//
//	pending ──approve──▶ approved ──claim──▶ (approved, claimed, leased)
//	   │  ╲                  │                    │            ╲
//	reject  superseded    7 days               result         lease lapses
//	   ▼      (newer          ▼                    ▼               ▼
//	rejected   draft)      expired          sent / failed     unconfirmed
//	                                                              │
//	                     owner re-approves ──┘ (a new decision: a NEW draft,
//	                                            approved, with the same text)
//
// 🔴 AT MOST ONCE, AND THAT IS WHY A LAPSED LEASE IS NOT RE-OFFERED. A plugin that
// claimed a draft and then went quiet may have sent it upstream before it died.
// Offering the draft again would risk the sender receiving it twice; so a claim
// whose lease lapses without a result becomes `unconfirmed` and is never offered
// again unless its owner, who can see what happened upstream, approves it AGAIN.
// The claim query's `claimed_at IS NULL` is the rule; the lapse is bookkeeping.

// DraftStatus is a draft's state. The set is closed (the 0015 CHECK).
type DraftStatus string

const (
	DraftPending     DraftStatus = "pending"
	DraftApproved    DraftStatus = "approved"
	DraftRejected    DraftStatus = "rejected"
	DraftSuperseded  DraftStatus = "superseded"
	DraftSent        DraftStatus = "sent"
	DraftFailed      DraftStatus = "failed"
	DraftUnconfirmed DraftStatus = "unconfirmed"
	DraftExpired     DraftStatus = "expired"
)

const (
	// MaxDraftBytes caps a draft's body, the same as an inbound message's.
	MaxDraftBytes = 16 << 10
	// MaxPendingDraftsPerAgent: drafts one agent may have awaiting its owner.
	MaxPendingDraftsPerAgent = 20
	// DraftTTL: a draft not sent within this of being written expires, whether
	// it was still pending or approved and never claimed.
	DraftTTL = 7 * 24 * time.Hour
	// LeaseDuration: how long a claiming plugin has to report a result.
	LeaseDuration = 2 * time.Minute
	// MaxClaimWait bounds one long-poll of the claim route.
	MaxClaimWait = 25 * time.Second
	// MaxExternalRefRunes and MaxDetailBytes cap what a result may carry.
	MaxExternalRefRunes = 200
	MaxDetailBytes      = 4096
)

// Draft is one row of plugin_reply_drafts.
type Draft struct {
	ID          int64       `json:"id"`
	OwnerID     int64       `json:"-"`
	MessageID   int64       `json:"messageId"`
	AgentID     *int64      `json:"-"`
	AgentName   string      `json:"agentName"`
	Body        string      `json:"body"`
	Status      DraftStatus `json:"status"`
	CreatedAt   time.Time   `json:"createdAt"`
	ExpiresAt   time.Time   `json:"expiresAt"`
	DecidedAt   *time.Time  `json:"decidedAt,omitempty"`
	ApprovedAt  *time.Time  `json:"approvedAt,omitempty"`
	ClaimedAt   *time.Time  `json:"claimedAt,omitempty"`
	LeaseUntil  *time.Time  `json:"leaseUntil,omitempty"`
	FinishedAt  *time.Time  `json:"finishedAt,omitempty"`
	ExternalRef string      `json:"externalRef,omitempty"`
	Detail      string      `json:"detail,omitempty"`
	// ReapprovedAs is the NEW draft an unconfirmed one was approved again as
	// (ReapproveDraft); nil until then.
	ReapprovedAs *int64 `json:"reapprovedAs,omitempty"`
}

// Claim is what the claim route hands a plugin: an approved draft to send.
type Claim struct {
	DraftID           int64     `json:"draft_id"`
	MessageExternalID string    `json:"message_external_id"`
	Body              string    `json:"body"`
	ApprovedAt        time.Time `json:"approved_at"`
}

// DraftResult is what a plugin reports after trying to send a claimed draft.
type DraftResult struct {
	OK          bool   `json:"ok"`
	ExternalRef string `json:"external_ref"`
	Detail      string `json:"detail"`
}

// DraftCard is a draft with the message it answers and its plugin, for /plugins.
type DraftCard struct {
	Draft       Draft
	Message     Message
	PluginName  string
	PluginLabel string
	// EarlierUnconfirmed: another draft for the same message is unconfirmed —
	// it may already have reached the sender. The card says so.
	EarlierUnconfirmed bool
	// SameTextSent: an unconfirmed draft whose exact text an earlier draft for
	// the same message was reported sent with — the card offers no re-approval
	// (the store refuses one when that earlier draft is in its chain).
	SameTextSent bool
}

var (
	// ErrNoReply: the message was marked no_reply by its plugin.
	ErrNoReply = errors.New("plugins: this message may not be replied to (no_reply)")
	// ErrTooManyPendingDrafts: the agent has MaxPendingDraftsPerAgent awaiting its owner.
	ErrTooManyPendingDrafts = errors.New("plugins: too many reply drafts awaiting the owner")
	// ErrDraftState: the draft is not in a state that allows this (409).
	ErrDraftState = errors.New("plugins: the draft is not in a state that allows this")
	// ErrReplyInFlight: another reply to the same message is approved and not yet
	// settled; a second one would risk two replies reaching the sender.
	ErrReplyInFlight = errors.New("plugins: another reply to this message is already approved and on its way")
	// ErrAlreadySent: the draft repeats one that was reported sent, so it may not
	// be approved again.
	ErrAlreadySent = errors.New("plugins: this text was already reported sent")
	// ErrResultConflict: the draft already settled with the OTHER result (409).
	ErrResultConflict = errors.New("plugins: the draft already settled with a different result")
)

// DraftObject is the audit_log `object` naming a reply draft.
func DraftObject(id int64) string { return "plugin_draft:" + strconv.FormatInt(id, 10) }

// ScreenDraft is the screen an agent's reply text passes before it is stored:
// the form it is stored, shown and sent in (NFKC — flows.NormalizeAgentText), or
// an ErrInvalid naming why it is refused.
//
// 🔴 THE CONTROL IS THE OWNER'S APPROVAL OF THE EXACT VISIBLE TEXT, as for a
// browser-flow request (internal/flows/screen.go). So: no character the approval
// card cannot show (flows.HiddenCharRefusal), and the deny patterns
// (flows.AgentTextRefusal) — a heuristic against an agent talked into sending an
// address, a credential or the deployment's own names to an outside party. It is
// never the gate; the approval is.
func ScreenDraft(body string, denyWords []string) (string, error) {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", invalid("body must be UTF-8 text without NUL")
	}
	body = flows.NormalizeAgentText(body)
	switch {
	case strings.TrimSpace(body) == "":
		return "", invalid("body is required")
	case len(body) > MaxDraftBytes:
		return "", invalid("body is over %d bytes", MaxDraftBytes)
	}
	if why := flows.HiddenCharRefusal(body); why != "" {
		return "", invalid("body: %s", why)
	}
	if why := flows.AgentTextRefusal(body, denyWords); why != "" {
		return "", invalid("body: %s", why)
	}
	return body, nil
}

// Validate checks a result's free-text fields.
func (r DraftResult) Validate() error {
	switch {
	case !utf8.ValidString(r.ExternalRef) || strings.ContainsRune(r.ExternalRef, 0) ||
		utf8.RuneCountInString(r.ExternalRef) > MaxExternalRefRunes:
		return invalid("external_ref is at most %d characters of UTF-8 text", MaxExternalRefRunes)
	case !utf8.ValidString(r.Detail) || strings.ContainsRune(r.Detail, 0) || len(r.Detail) > MaxDetailBytes:
		return invalid("detail is at most %d bytes of UTF-8 text", MaxDetailBytes)
	}
	return nil
}

// WithAgentDenyWords sets the deployment's own infrastructure names a reply
// draft may not carry (MUSTER_FLOW_AGENT_DENY_WORDS, the browser-flow screen's
// list): configuration, because this repository is public.
func (s *PGStore) WithAgentDenyWords(words []string) *PGStore {
	s.denyWords = append([]string(nil), words...)
	return s
}

const draftCols = `id, owner_id, message_id, agent_id, agent_name, body, status, created_at, expires_at, decided_at,
	approved_at, claimed_at, lease_until, finished_at, external_ref, detail, reapproved_as`

func draftDest(d *Draft, st *string) []any {
	return []any{&d.ID, &d.OwnerID, &d.MessageID, &d.AgentID, &d.AgentName, &d.Body, st, &d.CreatedAt, &d.ExpiresAt,
		&d.DecidedAt, &d.ApprovedAt, &d.ClaimedAt, &d.LeaseUntil, &d.FinishedAt, &d.ExternalRef, &d.Detail, &d.ReapprovedAs}
}

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	var st string
	err := row.Scan(draftDest(&d, &st)...)
	d.Status = DraftStatus(st)
	return d, err
}

// ancestorSentSQL answers whether any draft that draft $1 is a re-approval of —
// at any depth — was reported sent.
const ancestorSentSQL = `
	WITH RECURSIVE up(id) AS (
		SELECT id FROM plugin_reply_drafts WHERE reapproved_as = $1
		UNION
		SELECT d.id FROM plugin_reply_drafts d JOIN up u ON d.reapproved_as = u.id)
	SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts WHERE id IN (SELECT id FROM up) AND status = 'sent')`

// sweep runs sweepDrafts in a transaction of its own.
//
// 🔴 IT IS NEVER RUN INSIDE A TRANSACTION THAT HOLDS A MESSAGE OR DRAFT LOCK.
// It updates rows across every message the principal can see, so inside such a
// transaction it could wait on a row another writer holds while that writer
// waits on this one's (a deadlock, as a 500). Run first and on its own, it
// holds nothing it waits for; and its rows are taken in id order
// (sweepDrafts), so two sweeps cannot deadlock each other either.
func (s *PGStore) sweep(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return sweepDrafts(ctx, tx) })
}

// sweepDrafts settles what time has decided, for the principal's rows: a draft
// pending or approved-but-unclaimed past its expiry is `expired`, and a claim
// whose lease lapsed without a result is `unconfirmed` — the at-most-once rule
// acting. Each is audited, one row per draft, as whichever principal's request
// ran the sweep.
func sweepDrafts(ctx context.Context, tx pgx.Tx) error {
	for _, step := range []struct{ q, action, why string }{
		{`UPDATE plugin_reply_drafts SET status = 'expired', decided_at = coalesce(decided_at, now())
			WHERE id IN (SELECT id FROM plugin_reply_drafts
			              WHERE status IN ('pending', 'approved') AND claimed_at IS NULL AND expires_at <= now()
			              ORDER BY id FOR UPDATE)
			RETURNING id`,
			"plugin.draft.expired", "not sent within 7 days of being written"},
		{`UPDATE plugin_reply_drafts SET status = 'unconfirmed'
			WHERE id IN (SELECT id FROM plugin_reply_drafts
			              WHERE status = 'approved' AND claimed_at IS NOT NULL AND lease_until <= now()
			              ORDER BY id FOR UPDATE)
			RETURNING id`,
			"plugin.draft.unconfirmed", "lease lapsed without a result"},
	} {
		rows, err := tx.Query(ctx, step.q)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := audit(ctx, tx, step.action, DraftObject(id), map[string]any{"why": step.why}); err != nil {
				return err
			}
		}
	}
	return nil
}

// lockMessage locks message id's row and answers ErrReplyInFlight when another
// draft for it (not except) is approved and unsettled: not yet claimed, or
// claimed within its lease. (A lapsed lease is unconfirmed, whether or not the
// sweep has said so yet: it no longer blocks, and its card warns instead.)
//
// 🔴 ONE REPLY IN FLIGHT PER MESSAGE, AND THE MESSAGE ROW IS THE LOCK. Filing,
// approving and re-approving all take it first, so an owner approving draft 1
// while the chief files draft 2 for the same message are serialised, and the
// second never becomes a second approved reply the sender would receive too.
// A reply that has SETTLED (sent, failed) does not block a new one: answering
// again after a send is the owner's decision to make on the new card.
func lockMessage(ctx context.Context, tx pgx.Tx, id, except int64) (Message, error) {
	m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	var inFlight bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts
		WHERE message_id = $1 AND status = 'approved' AND id <> $2
		  AND (claimed_at IS NULL OR lease_until > now()))`, id, except).Scan(&inFlight); err != nil {
		return Message{}, err
	}
	if inFlight {
		return m, ErrReplyInFlight
	}
	return m, nil
}

// FileDraft stores agent agentID's reply to message messageID: the message must
// be one delivered to that agent and its owner (DeliveredTo — checked here, so
// it holds whatever store is wired) and not marked no_reply; the body passes
// ScreenDraft. A pending draft already filed for the message is superseded
// (superseded counts them). At most MaxPendingDraftsPerAgent may be pending per
// agent, not counting the one this replaces.
func (s *PGStore) FileDraft(ctx context.Context, agentID, ownerID int64, agentName string, messageID int64, body string) (Draft, int, error) {
	if ownerID <= 0 || agentID <= 0 {
		return Draft{}, 0, invalid("the agent has no owner")
	}
	body, err := ScreenDraft(body, s.denyWords)
	if err != nil {
		return Draft{}, 0, err
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, 0, err
	}
	var d Draft
	superseded := 0
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE MESSAGE ROW IS LOCKED FIRST (lockMessage): two drafts for one
		// message are filed one at a time, so "supersede the pending one, insert
		// mine" cannot leave two pending (the one-pending index would refuse it, as
		// a 500), and none is filed while another reply to it is in flight.
		m, inFlight := lockMessage(ctx, tx, messageID, 0)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		// Who may draft is decided before whether they may draft NOW, so the
		// in-flight answer says nothing about a message that is not theirs.
		if !DeliveredTo(m, agentID, ownerID) {
			return ErrNotFound
		}
		if m.NoReply {
			return ErrNoReply
		}
		if inFlight != nil {
			return inFlight
		}
		// One agent's filings are serialised so the pending count cannot be raced.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_reply_drafts.file'), $1::int)`, int32(agentID)); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_reply_drafts
			WHERE agent_id = $1 AND status = 'pending' AND message_id <> $2`, agentID, messageID).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPendingDraftsPerAgent {
			return fmt.Errorf("%w: %d await your owner (the limit is %d); wait for a decision",
				ErrTooManyPendingDrafts, pending, MaxPendingDraftsPerAgent)
		}
		tag, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET status = 'superseded', decided_at = now()
			WHERE message_id = $1 AND status = 'pending'`, messageID)
		if err != nil {
			return err
		}
		superseded = int(tag.RowsAffected())
		d, err = scanDraft(tx.QueryRow(ctx, `
			INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING `+draftCols,
			ownerID, messageID, agentID, cutRunes(agentName, 100), body, DraftTTL.Seconds()))
		if err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.draft.filed", DraftObject(d.ID), map[string]any{
			"message_id": messageID, "agent_id": agentID, "superseded": superseded,
		})
	})
	if err != nil {
		return Draft{}, 0, err
	}
	return d, superseded, nil
}

// Draft reads one of the principal's drafts.
func (s *PGStore) Draft(ctx context.Context, id int64) (Draft, error) {
	d, err := scanDraft(s.pool.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	return d, err
}

// DraftCards is the principal's drafts for /plugins — every one awaiting a
// decision (pending, and unconfirmed: the owner may re-approve it), then the
// most recent others — at most limit, after a sweep.
func (s *PGStore) DraftCards(ctx context.Context, limit int) ([]DraftCard, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if err := s.sweep(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("d", draftCols)+`, `+prefixed("m", messageCols)+`, p.name, p.label,
		       EXISTS (SELECT 1 FROM plugin_reply_drafts o
		                WHERE o.message_id = d.message_id AND o.id <> d.id AND o.status = 'unconfirmed'),
		       d.status = 'unconfirmed' AND EXISTS (SELECT 1 FROM plugin_reply_drafts o
		                WHERE o.message_id = d.message_id AND o.status = 'sent' AND o.id < d.id
		                  AND o.body = d.body)
		  FROM plugin_reply_drafts d
		  JOIN plugin_messages m ON m.id = d.message_id
		  JOIN plugins p ON p.id = m.plugin_id
		 ORDER BY (d.status = 'pending' OR (d.status = 'unconfirmed' AND d.reapproved_as IS NULL
		           AND d.expires_at > now())) DESC, d.id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DraftCard{}
	for rows.Next() {
		var c DraftCard
		var dst, mst string
		dest := draftDest(&c.Draft, &dst)
		m := &c.Message
		dest = append(dest, &m.ID, &m.OwnerID, &m.PluginID, &m.ExternalID, &m.Sender, &m.Subject, &m.Body, &m.ReceivedAt,
			&m.CreatedAt, &mst, &m.Attempts, &m.LastAttemptAt, &m.DeliveredAt, &m.AgentID, &m.NoReply,
			&c.PluginName, &c.PluginLabel, &c.EarlierUnconfirmed, &c.SameTextSent)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		c.Draft.Status, m.Status = DraftStatus(dst), Status(mst)
		out = append(out, c)
	}
	return out, rows.Err()
}

// lockedDraft locks draft id after its message (lockMessage, the lock order
// every draft write takes), answering the message's in-flight check as well.
func lockedDraft(ctx context.Context, tx pgx.Tx, id int64) (Draft, error) {
	var messageID int64
	err := tx.QueryRow(ctx, `SELECT message_id FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	_, inFlight := lockMessage(ctx, tx, messageID, id)
	if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
		return Draft{}, inFlight
	}
	cur, err := scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	return cur, inFlight
}

// DecideDraft approves or rejects one of the principal's PENDING, unexpired
// drafts (ErrDraftState otherwise). An approved draft is what the plugin's next
// claim may hand out. Approving while another reply to the same message is in
// flight is ErrReplyInFlight; rejecting is always allowed.
//
// Rejecting an APPROVED draft that no plugin has claimed yet WITHDRAWS the
// approval (rejected, its approval time cleared — the audit row keeps it): an
// owner who spots a mistake, or whose plugin is offline, is not left with a
// reply that blocks the message for its whole 7 days. A claimed draft cannot be
// withdrawn: the plugin may already be sending it.
func (s *PGStore) DecideDraft(ctx context.Context, id int64, approve bool) (Draft, error) {
	if err := s.sweep(ctx); err != nil {
		return Draft{}, err
	}
	var d Draft
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockedDraft(ctx, tx, id)
		if errors.Is(err, ErrReplyInFlight) && (!approve || cur.Status != DraftPending) {
			err = nil
		}
		if err != nil {
			d = cur
			return err
		}
		// Past its expiry but not yet swept (the sweep ran in its own transaction a
		// moment ago): not decidable, as if the sweep had already said so.
		var live bool
		if err := tx.QueryRow(ctx, `SELECT expires_at > now() FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&live); err != nil {
			return err
		}
		if !live {
			d = cur
			return ErrDraftState
		}
		if !approve && cur.Status == DraftApproved && cur.ClaimedAt == nil {
			if d, err = scanDraft(tx.QueryRow(ctx, `UPDATE plugin_reply_drafts
				SET status = 'rejected', decided_at = now(), approved_at = NULL WHERE id = $1 RETURNING `+draftCols, id)); err != nil {
				return err
			}
			// A withdrawn re-approval frees its original to be approved again.
			if _, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET reapproved_as = NULL WHERE reapproved_as = $1`, id); err != nil {
				return err
			}
			return audit(ctx, tx, "plugin.draft.withdrawn", DraftObject(id), map[string]any{
				"message_id": d.MessageID, "approved_at": cur.ApprovedAt.UTC().Format(time.RFC3339)})
		}
		if cur.Status != DraftPending {
			d = cur
			return ErrDraftState
		}
		q, action := `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now() WHERE id = $1 RETURNING `+draftCols, "plugin.draft.rejected"
		if approve {
			q, action = `UPDATE plugin_reply_drafts SET status = 'approved', decided_at = now(), approved_at = now() WHERE id = $1 RETURNING `+draftCols, "plugin.draft.approved"
		}
		if d, err = scanDraft(tx.QueryRow(ctx, q, id)); err != nil {
			return err
		}
		return audit(ctx, tx, action, DraftObject(id), map[string]any{"message_id": d.MessageID})
	})
	return d, err
}

// ReapproveDraft is the owner's NEW decision on an UNCONFIRMED draft — one a
// plugin claimed and never reported on, which may or may not have been sent. It
// returns a NEW draft, approved now, with the same text, message and expiry; the
// old row keeps its claim and its `unconfirmed` state and names the new one
// (reapproved_as, migration 0016).
//
// 🔴 A NEW ID, ON PURPOSE. Clearing the old row's claim and handing it out again
// under the same id would (a) let a late result from the FIRST claim settle the
// SECOND, and (b) be skipped by a plugin that, as docs/plugins.md tells it to,
// never sends one draft id twice. A new row has neither problem.
//
// Only an unconfirmed draft not yet re-approved, not past its expiry, whose
// agent still exists, may be re-approved (ErrDraftState otherwise); and not
// while another reply to the message is in flight (ErrReplyInFlight).
func (s *PGStore) ReapproveDraft(ctx context.Context, id int64) (Draft, error) {
	if err := s.sweep(ctx); err != nil {
		return Draft{}, err
	}
	var d Draft
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, inFlight := lockedDraft(ctx, tx, id)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT expires_at > now() FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&live); err != nil {
			return err
		}
		// The draft's own state answers first: "not re-approvable" is the true
		// reason whatever else is in flight.
		if cur.Status != DraftUnconfirmed || cur.ReapprovedAs != nil || cur.AgentID == nil || !live {
			d = cur
			return ErrDraftState
		}
		// 🔴 NOT IF THIS TEXT IS KNOWN TO HAVE GONE: a draft this one repeats (it
		// is a re-approval of a re-approval of …) was reported sent.
		var sentBefore bool
		if err := tx.QueryRow(ctx, ancestorSentSQL, id).Scan(&sentBefore); err != nil {
			return err
		}
		if sentBefore {
			d = cur
			return ErrAlreadySent
		}
		if inFlight != nil {
			d = cur
			return inFlight
		}
		var err error
		if d, err = scanDraft(tx.QueryRow(ctx, `
			INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at,
			                                 status, decided_at, approved_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'approved', now(), now()) RETURNING `+draftCols,
			cur.OwnerID, cur.MessageID, *cur.AgentID, cur.AgentName, cur.Body, cur.ExpiresAt)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET reapproved_as = $2 WHERE id = $1`, id, d.ID); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.draft.approved", DraftObject(d.ID), map[string]any{
			"message_id": d.MessageID, "reapproval_of": id}); err != nil {
			return err
		}
		detail := map[string]any{"message_id": d.MessageID, "new_draft_id": d.ID}
		if cur.ClaimedAt != nil {
			detail["previous_claimed_at"] = cur.ClaimedAt.UTC().Format(time.RFC3339)
		}
		return audit(ctx, tx, "plugin.draft.reapproved", DraftObject(id), detail)
	})
	return d, err
}

// ClaimDraft hands plugin pluginID ONE of its approved, unclaimed, unexpired
// drafts — the oldest approval first — and leases it for LeaseDuration. ok is
// false when there is none.
//
// 🔴 `claimed_at IS NULL` IS THE AT-MOST-ONCE RULE (see the file header): a
// draft is claimed once and its claim is never cleared; an owner's re-approval
// makes a new draft (ReapproveDraft). SKIP
// LOCKED lets two claims race without both taking one draft.
func (s *PGStore) ClaimDraft(ctx context.Context, pluginID int64) (Claim, bool, error) {
	if err := s.sweep(ctx); err != nil {
		return Claim{}, false, err
	}
	var c Claim
	found := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT d.id, m.external_id, d.body, d.approved_at
			  FROM plugin_reply_drafts d
			  JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE m.plugin_id = $1 AND d.status = 'approved' AND d.claimed_at IS NULL
			   AND d.expires_at > now() AND NOT m.no_reply
			 ORDER BY d.approved_at, d.id
			 LIMIT 1
			   FOR UPDATE OF d SKIP LOCKED`, pluginID).Scan(&c.DraftID, &c.MessageExternalID, &c.Body, &c.ApprovedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts
			SET claimed_at = now(), lease_until = now() + make_interval(secs => $2) WHERE id = $1`,
			c.DraftID, LeaseDuration.Seconds()); err != nil {
			return err
		}
		found = true
		return audit(ctx, tx, "plugin.draft.claimed", DraftObject(c.DraftID), map[string]any{"plugin_id": pluginID})
	})
	if err != nil || !found {
		return Claim{}, false, err
	}
	c.ApprovedAt = c.ApprovedAt.UTC()
	return c, true, nil
}

// FinishDraft records plugin pluginID's result for a draft IT claimed: sent when
// r.OK, failed otherwise. The same result again answers the stored draft
// unchanged; the other result is ErrResultConflict. A draft of another plugin is
// ErrNotFound; one never claimed is ErrDraftState.
//
// A result arriving after the lease lapsed (the draft is `unconfirmed`) is still
// recorded: it is exactly the fact the owner would otherwise have to find out
// upstream before deciding whether to re-approve.
func (s *PGStore) FinishDraft(ctx context.Context, pluginID, draftID int64, r DraftResult) (Draft, error) {
	if err := r.Validate(); err != nil {
		return Draft{}, err
	}
	want := DraftFailed
	if r.OK {
		want = DraftSent
	}
	var d Draft
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE MESSAGE ROW FIRST, the lock order every draft write takes
		// (lockMessage): a late result and a withdraw or re-approval of the same
		// message's drafts are serialised rather than deadlocking, and the chain
		// walk below sees every re-approval committed before it.
		var messageID int64
		err := tx.QueryRow(ctx, `SELECT d.message_id FROM plugin_reply_drafts d
			  JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE d.id = $1 AND m.plugin_id = $2`, draftID, pluginID).Scan(&messageID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := lockMessage(ctx, tx, messageID, draftID); err != nil && !errors.Is(err, ErrReplyInFlight) {
			return err
		}
		cur, err := scanDraft(tx.QueryRow(ctx, `SELECT `+prefixed("d", draftCols)+`
			  FROM plugin_reply_drafts d
			  JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE d.id = $1 AND m.plugin_id = $2
			   FOR UPDATE OF d`, draftID, pluginID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		d = cur
		switch {
		case cur.Status == DraftSent || cur.Status == DraftFailed:
			if cur.Status != want {
				return ErrResultConflict
			}
			return nil // the same result again: the stored draft is the answer
		case cur.ClaimedAt == nil:
			return ErrDraftState
		case cur.Status != DraftApproved && cur.Status != DraftUnconfirmed:
			return ErrDraftState
		}
		var late bool
		if err := tx.QueryRow(ctx, `SELECT lease_until <= now() FROM plugin_reply_drafts WHERE id = $1`, draftID).Scan(&late); err != nil {
			return err
		}
		if d, err = scanDraft(tx.QueryRow(ctx, `UPDATE plugin_reply_drafts
			SET status = $2, finished_at = now(), external_ref = $3, detail = $4
			WHERE id = $1 RETURNING `+draftCols, draftID, string(want), r.ExternalRef, r.Detail)); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.draft."+string(want), DraftObject(draftID), map[string]any{
			"plugin_id": pluginID, "external_ref": r.ExternalRef, "late": late || cur.Status == DraftUnconfirmed,
		}); err != nil {
			return err
		}
		// 🔴 A LATE "SENT" CANCELS ITS UNCLAIMED RE-APPROVAL. The owner approved
		// this text again because nobody knew whether it had gone; now the plugin
		// says it has, so the copy (if no plugin has taken it yet) must not go
		// too. Lock order: the message (above), this row, then its copies.
		// The whole chain: a copy can itself have lapsed and been approved again.
		if want == DraftSent && cur.ReapprovedAs != nil {
			rows, err := tx.Query(ctx, `
				WITH RECURSIVE chain(id) AS (
					SELECT $1::bigint
					UNION -- not ALL: a cycle (impossible through the store) ends
					SELECT d.reapproved_as FROM plugin_reply_drafts d JOIN chain c ON d.id = c.id
					 WHERE d.reapproved_as IS NOT NULL)
				UPDATE plugin_reply_drafts SET status = 'expired', decided_at = now()
				 WHERE id IN (SELECT id FROM chain) AND status = 'approved' AND claimed_at IS NULL
				RETURNING id`, *cur.ReapprovedAs)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := audit(ctx, tx, "plugin.draft.expired", DraftObject(id), map[string]any{
					"why": "the draft it approved again was reported sent", "original": draftID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return d, err
}

// QueuedForOwner is ownerID's undelivered messages across the owner's LIVE
// plugins, oldest first, at most limit — what the claim route retries.
func (s *PGStore) QueuedForOwner(ctx context.Context, ownerID int64, limit int) ([]Message, error) {
	if limit <= 0 || ownerID <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+`
		  FROM plugin_messages m JOIN plugins p ON p.id = m.plugin_id
		 WHERE m.owner_id = $1 AND m.status = 'queued' AND p.revoked_at IS NULL
		 ORDER BY m.id LIMIT $2`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueuedCounts is how many undelivered messages each of the principal's plugins
// has, by plugin id (a plugin with none is absent).
func (s *PGStore) QueuedCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT plugin_id, count(*) FROM plugin_messages WHERE status = 'queued' GROUP BY plugin_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// cutRunes keeps at most n runes of s.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
