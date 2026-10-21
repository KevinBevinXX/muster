package sendqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/mirror"
)

// EDITING A QUEUED MESSAGE (migration 0040): the ⋯ menu's Edit opens a queued
// message's text in its row, Save replaces it, Cancel leaves it as it was.
//
// 🔴 HELD WHILE IT IS EDITED, AND THE HOLD COMES FIRST. Hold marks the row held
// (held_until, hold_token) BEFORE its text is shown for editing, and the
// dispatcher's Claim skips a held row in the same conditional UPDATE that claims
// it — so whichever of the two the row lock lets in first wins, and the other
// finds the row no longer eligible. A held row is never sent, and neither is any
// message queued after it for the same window: it keeps its place, and the
// queue waits behind it (Decide: HeldWhy).
//
// 🔴 A HALF-EDITED TEXT IS NEVER PASTED. The text being edited lives in the
// browser until Save. SaveEdit replaces body in one UPDATE, only under its own
// LIVE hold (hold_token matches, held_until not passed), and clears the hold in
// the same statement: a claim sees the old text or the whole new one.
//
// 🔴 A HOLD RUNS OUT (EditHold, on Postgres's clock), so a tab closed mid-edit
// cannot freeze a window's queue. After that the row is queued again with its
// ORIGINAL text — nothing was written — and a Save under the lapsed hold is
// refused (ErrHoldLapsed, or ErrEditTooLate once the row has moved on). The
// dispatcher's next pass takes the lapsed hold off the row (ClearLapsedHolds),
// recording that it ran out (hold_lapsed_at, Item.HoldLapsed) so the list can
// say so: a row carries a hold only while it is live.
//
// ROUTED messages are not editable (ErrRoutedNotEditable): their text is the
// chief's, and their delivery is gated on the route, not on a person's sign-in.

// EditHold is how long an Edit holds its row: long enough to rewrite a message
// on a phone, short enough that an abandoned edit does not stall the queue for
// long.
const EditHold = 10 * time.Minute

var (
	// ErrNotEditable: the message is no longer queued (sent, failed or
	// cancelled), so there is nothing to edit.
	ErrNotEditable = errors.New("sendqueue: only a queued message can be edited")
	// ErrEditSending: the message is being sent right now; it is past editing.
	// An ErrNotEditable (errors.Is).
	ErrEditSending = fmt.Errorf("%w: this one is being sent right now", ErrNotEditable)
	// ErrRoutedNotEditable: the chief routed it; its text is the chief's. An
	// ErrNotEditable (errors.Is).
	ErrRoutedNotEditable = fmt.Errorf("%w: a message your chief routed is not edited here", ErrNotEditable)
	// ErrHoldLapsed: Save (or Cancel) presented a hold that is no longer the
	// row's live one — it ran out, or a later Edit took the row over. The row
	// is still queued, with the text it had.
	ErrHoldLapsed = errors.New("sendqueue: the edit's hold ran out (or the message was opened for editing again), so the edit was not saved")
	// ErrEditTooLate: Save found the row no longer queued — its hold ran out
	// and it was sent (or is being sent) with its original text, or it was
	// cancelled. Item.State says which.
	ErrEditTooLate = errors.New("sendqueue: the message is no longer queued, so the edit was not saved")
)

// newHoldToken is a fresh hold's name: 128 random bits, hex.
func newHoldToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sendqueue: hold token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// editHold is s's hold (a test's short one, else EditHold).
func (s *PGStore) editHold() time.Duration {
	if s.hold > 0 {
		return s.hold
	}
	return EditHold
}

// Hold holds the caller's QUEUED message id for an edit, for EditHold on
// Postgres's clock, and returns it with its new HoldToken. A row that is
// already held is taken over (a new token: the earlier editor's Save is then
// refused with ErrHoldLapsed). Refused: one being sent (ErrEditSending), one
// that has finished (ErrNotEditable), a routed one (ErrRoutedNotEditable);
// another user's is not found.
func (s *PGStore) Hold(ctx context.Context, id int64) (Item, error) {
	tok, err := newHoldToken()
	if err != nil {
		return Item{}, err
	}
	it, err := scanItem(s.pool.QueryRow(ctx, `UPDATE send_queue SET held_until = now() + $2 * interval '1 microsecond', hold_token = $3,
		hold_lapsed_at = NULL WHERE id = $1 AND state = 'queued' AND route_id IS NULL RETURNING `+cols, id, leaseMicros(s.editHold()), tok))
	if err == nil {
		return it, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Item{}, fmt.Errorf("sendqueue: hold: %w", err)
	}
	cur, gerr := s.Get(ctx, id)
	switch {
	case gerr != nil:
		return Item{}, gerr
	case cur.RouteID != "":
		return cur, ErrRoutedNotEditable
	case cur.State == Sending:
		return cur, ErrEditSending
	}
	return cur, ErrNotEditable
}

// SaveEdit replaces the text of the caller's message id with body, under the
// live hold token, and releases the hold — in one UPDATE, so the row is never
// claimed with half of an edit. stepUpAt is the sign-in time of the request
// that saved it, which the caller has checked is fresh; it is stored
// (edit_stepup_at) for the delivery to check again. The row keeps its place
// (id) and its queued time (created_at). Refused: an empty or unpasteable
// body; a hold that is no longer live (ErrHoldLapsed: the row is unchanged); a
// row no longer queued (ErrEditTooLate, with the row as it is now).
func (s *PGStore) SaveEdit(ctx context.Context, id int64, token, body string, stepUpAt time.Time) (Item, error) {
	if strings.TrimSpace(body) == "" {
		return Item{}, ErrEmpty
	}
	text, err := mirror.SanitizePaste(body)
	if err != nil {
		return Item{}, err
	}
	if stepUpAt.IsZero() {
		return Item{}, errors.New("sendqueue: an edit needs the sign-in time of the session that saved it")
	}
	if token == "" {
		return Item{}, ErrHoldLapsed
	}
	it, err := scanItem(s.pool.QueryRow(ctx, `UPDATE send_queue SET body = $3, edited_at = now(), edit_stepup_at = $4, hold_lapsed_at = NULL, `+clearHold+`
		WHERE id = $1 AND state = 'queued' AND hold_token = $2 AND held_until > now() RETURNING `+cols, id, token, text, stepUpAt))
	if err == nil {
		return it, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Item{}, fmt.Errorf("sendqueue: save edit: %w", err)
	}
	cur, gerr := s.Get(ctx, id)
	switch {
	case gerr != nil:
		return Item{}, gerr
	case cur.State != Queued:
		return cur, ErrEditTooLate
	}
	return cur, ErrHoldLapsed
}

// ReleaseEdit releases the caller's hold token on message id, leaving its text
// as it was: an edit's Cancel. released is false when the token is no longer
// the row's live hold — it ran out, was taken over, or the row moved on — and
// the row is returned as it is now; there is then nothing to release.
func (s *PGStore) ReleaseEdit(ctx context.Context, id int64, token string) (it Item, released bool, err error) {
	if token != "" {
		it, err = scanItem(s.pool.QueryRow(ctx, `UPDATE send_queue SET `+clearHold+`
			WHERE id = $1 AND state = 'queued' AND hold_token = $2 AND held_until > now() RETURNING `+cols, id, token))
		if err == nil {
			return it, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Item{}, false, fmt.Errorf("sendqueue: release edit: %w", err)
		}
	}
	cur, gerr := s.Get(ctx, id)
	if gerr != nil {
		return Item{}, false, gerr
	}
	return cur, false, nil
}

// ClearLapsedHolds takes every hold that has run out off its row, across every
// owner (the system scope), recording that it ran out (hold_lapsed_at), and
// returns how many. The dispatcher runs it on every pass: a row then carries
// held_until and hold_token only while its hold is live, which bounds what an
// older image (whose statements do not clear them, and so trip 0040's
// send_queue_hold_check on a held row) can meet during a rollout or rollback.
func (s *PGStore) ClearLapsedHolds(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(sys(ctx), `UPDATE send_queue SET hold_lapsed_at = held_until, `+clearHold+`
		WHERE held_until IS NOT NULL AND held_until <= now()`)
	if err != nil {
		return 0, fmt.Errorf("sendqueue: clear lapsed holds: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// clearHold is the SET clause that ends a row's hold: every transition out of
// 'queued' says it (0040's send_queue_hold_check), as do Save and a release.
const clearHold = `held_until = NULL, hold_token = NULL`

// notHeld is the predicate "this row has no live hold": the claim's.
const notHeld = `(held_until IS NULL OR held_until <= now())`

// HeldWhy is what a held head is waiting for (Decide).
const HeldWhy = "it is being edited; nothing for this window is sent until the edit is saved or cancelled (or its hold runs out)"
