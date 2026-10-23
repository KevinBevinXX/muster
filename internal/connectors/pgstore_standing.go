package connectors

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// --- standing grants (migration 0043) ---------------------------------------------

const standingCols = `id, connector_name, source, repo, coalesce(origin_grant_id, 0), created_at, revoked_at`

func scanStanding(row pgx.Row) (StandingGrant, error) {
	var sg StandingGrant
	err := row.Scan(&sg.ID, &sg.Connector, &sg.Source, &sg.Repo, &sg.OriginGrantID, &sg.CreatedAt, &sg.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StandingGrant{}, ErrNotFound
	}
	return sg, err
}

// CreateStandingGrant records the standing grant for (connector, source, repo),
// owned by the owner of grant origin (the approval it comes from), or returns
// the live one that already holds the key.
func (s *PGStore) CreateStandingGrant(ctx context.Context, connector, source, repo string, origin int64) (StandingGrant, bool, error) {
	if err := ValidateName(connector); err != nil {
		return StandingGrant{}, false, err
	}
	repo, ok := StandingRepo(source, repo)
	if !ok {
		return StandingGrant{}, false, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StandingGrant{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	// 🔴 ONE SOURCE AT A TIME: an approval under this credential source retires
	// the connector's live standing grants under the OTHER source. Otherwise an
	// approval of the stored `github` token, superseded when the App was
	// configured and an agent re-approved under it, would come back into force
	// — for every repository — the moment the App is removed.
	if _, err := tx.Exec(ctx, `UPDATE connector_standing_grants SET revoked_at = now()
		WHERE owner_id = (SELECT owner_id FROM connector_grants WHERE id = $3)
		  AND connector_name = $1 AND source <> $2 AND revoked_at IS NULL`, connector, source, origin); err != nil {
		return StandingGrant{}, false, err
	}
	created := true
	sg, err := scanStanding(tx.QueryRow(ctx, `
		INSERT INTO connector_standing_grants (owner_id, connector_name, source, repo, origin_grant_id)
		VALUES ((SELECT owner_id FROM connector_grants WHERE id = $4), $1, $2, $3, $4)
		ON CONFLICT (owner_id, connector_name, source, repo) WHERE revoked_at IS NULL DO NOTHING
		RETURNING `+standingCols, connector, source, repo, origin))
	if errors.Is(err, ErrNotFound) {
		created = false
		sg, err = scanStanding(tx.QueryRow(ctx, `SELECT `+standingCols+` FROM connector_standing_grants
			WHERE owner_id = (SELECT owner_id FROM connector_grants WHERE id = $4)
			  AND connector_name=$1 AND source=$2 AND repo=$3 AND revoked_at IS NULL`, connector, source, repo, origin))
	}
	if err != nil {
		return StandingGrant{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StandingGrant{}, false, err
	}
	return sg, created, nil
}

// MatchStandingGrant is the live standing grant for the key that may approve
// agentID's request.
//
// 🔴 OWNER: THE AGENT'S, STATED IN THE QUERY. Row-level security already hides
// another owner's standing grants from a user principal; the explicit
// `owner_id = <the agent's owner>` is what keeps a scope-all (System) caller
// from approving one owner's agent with another owner's approval
// (TestAStandingGrantNeverCrossesOwners).
//
// 🔴 A PER-AGENT "NO" OUTLIVES THE SHARED "YES": a grant on this connector that
// the owner revoked or denied for THIS agent after the standing grant was made
// keeps the standing grant from approving the agent again — revoking one agent
// on the Connectors tab is not undone by the agent simply asking again.
func (s *PGStore) MatchStandingGrant(ctx context.Context, agentID int64, connector, source, repo string) (StandingGrant, error) {
	repo, ok := StandingRepo(source, repo)
	if !ok {
		return StandingGrant{}, ErrNotFound
	}
	return scanStanding(s.pool.QueryRow(ctx, `SELECT `+standingCols+` FROM connector_standing_grants sg
		WHERE sg.owner_id = (SELECT owner_id FROM agents WHERE id = $1)
		  AND sg.connector_name = $2 AND sg.source = $3 AND sg.repo = $4 AND sg.revoked_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM connector_grants g
		      WHERE g.agent_id = $1 AND g.connector_name = $2 AND g.status IN ('revoked', 'denied')
		        AND g.decided_at >= sg.created_at)`, agentID, connector, source, repo))
}

// ApproveFromStanding approves pending grant id from live standing grant sg.
//
// 🔴 THE STANDING GRANT'S ROW IS LOCKED (FOR SHARE) FOR THE APPROVAL, so it
// serialises with RevokeStandingGrant (which updates that row first): either
// the revoke commits first and this finds it revoked (ErrNotPending), or this
// commits first and the revoke's cascade — a later statement — sees the grant
// and revokes it. An approval can never land after its standing grant's revoke
// has swept.
func (s *PGStore) ApproveFromStanding(ctx context.Context, id, sg int64) (Grant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	var connector, source string
	err = tx.QueryRow(ctx, `SELECT connector_name, source FROM connector_standing_grants
		WHERE id = $1 AND revoked_at IS NULL FOR SHARE`, sg).Scan(&connector, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotPending
	}
	if err != nil {
		return Grant{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE connector_grants SET status='approved', decided_by=$3, decided_at=now(), source=$4,
		  decided_via=$5, decided_via_host=NULL, decided_via_session=NULL, standing_grant_id=$2
		WHERE id=$1 AND status='pending' AND connector_name=$6`,
		id, sg, DecidedByStanding, source, DecidedViaStanding, connector)
	if err != nil {
		return Grant{}, err
	}
	if tag.RowsAffected() == 0 {
		return Grant{}, ErrNotPending
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, err
	}
	return s.GetGrant(ctx, id)
}

// RevokeStandingGrant revokes standing grant id and, in the same transaction,
// every APPROVED grant it gave: those it approved automatically, and the
// approval it was created from. A revoke is an off-switch for the shared
// approval, not only for the ones still to come.
func (s *PGStore) RevokeStandingGrant(ctx context.Context, id int64, by string) (StandingGrant, []Grant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StandingGrant{}, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	sg, err := scanStanding(tx.QueryRow(ctx, `UPDATE connector_standing_grants SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL RETURNING `+standingCols, id))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := scanStanding(tx.QueryRow(ctx, `SELECT `+standingCols+` FROM connector_standing_grants WHERE id=$1`, id)); gerr != nil {
			return StandingGrant{}, nil, gerr
		}
		return StandingGrant{}, nil, ErrNotPending
	}
	if err != nil {
		return StandingGrant{}, nil, err
	}
	rows, err := tx.Query(ctx, `
		UPDATE connector_grants g SET status='revoked', decided_by=$2, decided_at=now(),
		  decided_via=NULL, decided_via_host=NULL, decided_via_session=NULL, standing_grant_id=NULL
		FROM connector_standing_grants sg
		WHERE sg.id = $1 AND g.owner_id = sg.owner_id AND g.connector_name = sg.connector_name
		  AND g.status = 'approved' AND (g.standing_grant_id = sg.id OR g.id = sg.origin_grant_id)
		RETURNING g.id`, id, by)
	if err != nil {
		return StandingGrant{}, nil, err
	}
	var ids []int64
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			rows.Close()
			return StandingGrant{}, nil, err
		}
		ids = append(ids, gid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StandingGrant{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StandingGrant{}, nil, err
	}
	out := make([]Grant, 0, len(ids))
	for _, gid := range ids {
		g, err := s.GetGrant(ctx, gid)
		if err != nil {
			return sg, out, err
		}
		out = append(out, g)
	}
	return sg, out, nil
}

// ListStandingGrants returns the live standing grants, by connector.
func (s *PGStore) ListStandingGrants(ctx context.Context) ([]StandingGrant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+standingCols+` FROM connector_standing_grants
		WHERE revoked_at IS NULL ORDER BY connector_name, repo, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StandingGrant
	for rows.Next() {
		sg, err := scanStanding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}
