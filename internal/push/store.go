package push

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Device is one stored subscription.
type Device struct {
	ID        int64
	OwnerID   int64
	Sub       Subscription
	UserAgent string
	CreatedAt time.Time
	LastOKAt  *time.Time
	FailCount int
}

// MaxDevicesPerOwner bounds one user's subscriptions. Every send fans out to
// all of them, so the cap is also a bound on what one notification costs.
const MaxDevicesPerOwner = 20

// ErrTooManyDevices refuses a claim past MaxDevicesPerOwner.
var ErrTooManyDevices = fmt.Errorf("push: a user may have at most %d devices; remove one on the account page", MaxDevicesPerOwner)

// ErrPushServiceNotAllowed refuses an endpoint on a host that is not a known
// browser push service or a push host the operator named
// (MUSTER_PUSH_EXTRA_HOSTS).
var ErrPushServiceNotAllowed = errors.New("push: the endpoint is not on a known push service")

// pushServiceHosts is every host a browser's push service lives on: exact
// names, and suffixes (a leading dot).
//
// 🔴 IT IS AN ALLOWLIST BECAUSE THE ENDPOINT IS A URL A MEMBER CHOOSES AND
// muster THEN POSTS TO. Without it a member could subscribe
// `https://<anything the muster pod can reach>/…` and have the server send
// requests there on every notification — with fail_count and last_ok_at on
// /account as a reachability oracle. The browsers' push services are a short,
// stable list: Chrome/Edge-on-Android/Brave/Opera/Samsung use FCM, Firefox
// Mozilla's autopush, Safari Apple's, Edge on Windows WNS.
var pushServiceHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	"web.push.apple.com",
	".push.apple.com",
	".notify.windows.com",
}

// AllowedPushService reports whether endpoint's host is a browser push
// service, on the default port. It is the empty Allowlist: no operator hosts.
func AllowedPushService(endpoint string) bool { return Allowlist{}.Allows(endpoint) }

// Allowlist is the set of hosts muster will POST a push to: the browser push
// services (pushServiceHosts) and the operator's own push hosts
// (MUSTER_PUSH_EXTRA_HOSTS), such as a self-hosted ntfy that serves
// UnifiedPush to the Android app (docs/design/android.md).
//
// 🔴 AN OPERATOR HOST MATCHES BY EXACT NAME ONLY. No suffix, no wildcard, no
// port: a member chooses only the PATH on a host the operator named. The
// https-only, default-port rule applies to it as to the browser hosts, and
// with redirects refused (pushHTTPClient) a send cannot be bounced from a
// listed host to an unlisted one. There is deliberately no check of the
// address the name resolves to: an in-cluster ntfy whose name resolves to a
// private address is a reasonable deployment (design doc, server change 21).
type Allowlist struct{ extra []string }

// NewAllowlist is the browser push services plus extra, which must already be
// clean (ParseExtraHosts).
func NewAllowlist(extra []string) Allowlist {
	return Allowlist{extra: slices.Clone(extra)}
}

// ExtraHosts is the operator's hosts, in the order configured.
func (a Allowlist) ExtraHosts() []string { return slices.Clone(a.extra) }

// Allows reports whether endpoint is an https URL on the default port whose
// host is a browser push service or, exactly, one of the operator's hosts.
func (a Allowlist) Allows(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, x := range a.extra {
		if h == x {
			return true
		}
	}
	for _, s := range pushServiceHosts {
		if h == s || (strings.HasPrefix(s, ".") && strings.HasSuffix(h, s) && len(h) > len(s)) {
			return true
		}
	}
	return false
}

// ErrBadExtraHost refuses a MUSTER_PUSH_EXTRA_HOSTS entry that is not a bare
// host name.
var ErrBadExtraHost = errors.New("push: an extra push host must be a bare DNS host name")

// ParseExtraHosts reads MUSTER_PUSH_EXTRA_HOSTS: host names separated by
// commas or white space. Each must be a bare DNS name — no scheme, port,
// path, wildcard, leading dot or IP literal — and is lower-cased. Empty is no
// hosts. A deployment that typed the variable wrong must fail to start rather
// than run with an allowlist it did not choose.
func ParseExtraHosts(v string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		h := strings.ToLower(f)
		if !validHostName(h) {
			return nil, fmt.Errorf("%w: %q", ErrBadExtraHost, f)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out, nil
}

// validHostName reports whether h is a lower-case DNS name of at least two
// labels: letters, digits and inner hyphens, each label 1–63 characters. A
// name made only of digits and dots (an IPv4 literal) is refused.
func validHostName(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	allDigits := true
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			switch {
			case c >= 'a' && c <= 'z', c == '-':
				allDigits = false
			case c >= '0' && c <= '9':
			default:
				return false
			}
		}
	}
	return !allDigits
}

// Store is the subscription persistence.
//
// 🔴 TWO KINDS OF METHOD, AND WHICH IS WHICH IS THE WHOLE SECURITY STORY.
//
//   - The REQUEST methods (Devices, Remove, Unsubscribe) run under the
//     caller's principal: push_subscriptions is under FORCE row-level security
//     (0013), so they see and change only that user's rows.
//   - The SYSTEM methods (Claim, ForOwner, MarkDelivered, MarkFailed, Prune,
//     ForgetDevice) run under authz.System through this package's one helper,
//     sys, which is ledgered in internal/authz/ledger_test.go. Each exists
//     because no principal can do its job: a send runs from loops and hooks
//     with no request; a claim must see another user's row to move it; a
//     logout has already ended its session. EACH ONE FILTERS ON AN EXPLICIT
//     OWNER (or a row id it was handed with its owner) — under System that
//     WHERE clause is the only isolation there is.
type Store interface {
	Claim(ctx context.Context, owner int64, sub Subscription, userAgent string) (previousOwner int64, err error)
	Devices(ctx context.Context) ([]Device, error)
	Remove(ctx context.Context, id int64) (bool, error)
	Unsubscribe(ctx context.Context, endpoint string) (bool, error)

	ForOwner(ctx context.Context, owner int64) ([]Device, error)
	MarkDelivered(ctx context.Context, d Device) error
	MarkFailed(ctx context.Context, d Device) error
	Prune(ctx context.Context, d Device) (bool, error)
	ForgetDevice(ctx context.Context, owner int64, endpointHash string) (bool, error)

	// Muted is owner's muted events, for a send (System, filtered by owner).
	Muted(ctx context.Context, owner int64) ([]string, error)
	// MyMuted / SetMuted are the CALLER's preferences (row-level security).
	MyMuted(ctx context.Context) ([]string, error)
	SetMuted(ctx context.Context, muted []string) error
}

// sys is this package's ONE authz.System call (ledgered). See Store.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// PGStore is Store over Postgres (migration 0013).
type PGStore struct {
	pool  *pgxpool.Pool
	allow Allowlist
}

// NewPG builds the Postgres store. It admits the browser push services only;
// WithAllowlist adds the operator's hosts.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// WithAllowlist sets the hosts a claimed endpoint may be on.
func (s *PGStore) WithAllowlist(a Allowlist) *PGStore {
	s.allow = a
	return s
}

const deviceCols = `id, owner_id, endpoint, p256dh, auth, user_agent, created_at, last_ok_at, fail_count`

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.OwnerID, &d.Sub.Endpoint, &d.Sub.Keys.P256dh, &d.Sub.Keys.Auth,
		&d.UserAgent, &d.CreatedAt, &d.LastOKAt, &d.FailCount)
	return d, err
}

func collect(rows pgx.Rows, err error) ([]Device, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// maxUserAgent bounds the stored label.
const maxUserAgent = 200

// Claim stores sub as owner's, moving it from whoever held the endpoint before
// (the shared-device case: one browser, one subscription per origin, a second
// user signed in). It returns the previous owner when the endpoint moved, 0
// otherwise.
//
// 🔴 owner COMES FROM THE CALLER'S PRINCIPAL, NEVER FROM THE REQUEST BODY — the
// handler passes requestOwner(ctx). It runs under System because the row it
// may have to move is invisible to the claimant under row-level security.
func (s *PGStore) Claim(ctx context.Context, owner int64, sub Subscription, userAgent string) (int64, error) {
	if owner <= 0 {
		return 0, fmt.Errorf("push: claim for no user (%d)", owner)
	}
	if err := sub.Check(); err != nil {
		return 0, err
	}
	if !s.allow.Allows(sub.Endpoint) {
		return 0, ErrPushServiceNotAllowed
	}
	if r := []rune(userAgent); len(r) > maxUserAgent {
		userAgent = string(r[:maxUserAgent])
	}
	ctx = sys(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialise claims by OWNER so the device cap cannot be raced past.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.push_subscriptions.owner', $1::bigint))`, owner); err != nil {
		return 0, err
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE owner_id = $1 AND endpoint <> $2`,
		owner, sub.Endpoint).Scan(&others); err != nil {
		return 0, err
	}
	if others >= MaxDevicesPerOwner {
		return 0, ErrTooManyDevices
	}
	var prev *int64
	err = tx.QueryRow(ctx, `
		WITH prev AS (SELECT owner_id FROM push_subscriptions WHERE endpoint = $2)
		INSERT INTO push_subscriptions AS p (owner_id, endpoint, p256dh, auth, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET
			p256dh     = EXCLUDED.p256dh,
			auth       = EXCLUDED.auth,
			user_agent = EXCLUDED.user_agent,
			fail_count = 0,
			-- A device that changed hands starts its history over: the previous
			-- owner's last delivery is not this owner's.
			last_ok_at = CASE WHEN p.owner_id = EXCLUDED.owner_id THEN p.last_ok_at END,
			created_at = CASE WHEN p.owner_id = EXCLUDED.owner_id THEN p.created_at ELSE now() END,
			owner_id   = EXCLUDED.owner_id
		RETURNING (SELECT owner_id FROM prev)`,
		owner, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, userAgent).Scan(&prev)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if prev != nil && *prev != owner {
		return *prev, nil
	}
	return 0, nil
}

// Devices lists the CALLER's devices (row-level security).
func (s *PGStore) Devices(ctx context.Context) ([]Device, error) {
	return collect(s.pool.Query(ctx, `SELECT `+deviceCols+` FROM push_subscriptions ORDER BY id`))
}

// Remove deletes one of the CALLER's devices by id (row-level security: another
// user's id matches nothing).
func (s *PGStore) Remove(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Unsubscribe deletes the CALLER's subscription for endpoint, if it holds it.
func (s *PGStore) Unsubscribe(ctx context.Context, endpoint string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ForOwner lists owner's devices, for a send. System, filtered by owner.
func (s *PGStore) ForOwner(ctx context.Context, owner int64) ([]Device, error) {
	if owner <= 0 {
		return nil, fmt.Errorf("push: devices for no user (%d)", owner)
	}
	return collect(s.pool.Query(sys(ctx), `SELECT `+deviceCols+` FROM push_subscriptions WHERE owner_id = $1 ORDER BY id`, owner))
}

// MarkDelivered stamps a successful delivery. Keyed by (id, owner) so a row
// that changed hands mid-send is not stamped for the new owner.
func (s *PGStore) MarkDelivered(ctx context.Context, d Device) error {
	_, err := s.pool.Exec(sys(ctx), `UPDATE push_subscriptions SET last_ok_at = now(), fail_count = 0
		WHERE id = $1 AND owner_id = $2`, d.ID, d.OwnerID)
	return err
}

// MarkFailed counts a failed (non-expired) delivery.
func (s *PGStore) MarkFailed(ctx context.Context, d Device) error {
	_, err := s.pool.Exec(sys(ctx), `UPDATE push_subscriptions SET fail_count = fail_count + 1
		WHERE id = $1 AND owner_id = $2`, d.ID, d.OwnerID)
	return err
}

// Prune deletes a device the push service reported gone (404/410). Keyed by
// (id, owner, endpoint): a row re-claimed by another user, or re-subscribed
// with a fresh endpoint, between the send and the prune is left alone.
func (s *PGStore) Prune(ctx context.Context, d Device) (bool, error) {
	tag, err := s.pool.Exec(sys(ctx), `DELETE FROM push_subscriptions WHERE id = $1 AND owner_id = $2 AND endpoint = $3`,
		d.ID, d.OwnerID, d.Sub.Endpoint)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetDevice deletes owner's subscription whose endpoint hashes to
// endpointHash: the browser that is signing out (its device cookie). It runs
// under System because logout has no principal; it is filtered by owner, so a
// cookie naming another user's device deletes nothing.
func (s *PGStore) ForgetDevice(ctx context.Context, owner int64, endpointHash string) (bool, error) {
	if owner <= 0 || len(endpointHash) != 64 {
		return false, nil
	}
	devs, err := s.ForOwner(ctx, owner)
	if err != nil {
		return false, err
	}
	for _, d := range devs {
		if EndpointHash(d.Sub.Endpoint) == endpointHash {
			return s.Prune(ctx, d)
		}
	}
	return false, nil
}

// Muted reads owner's muted events. System, filtered by owner. A user with no
// preference row has nothing muted: every event is on by default.
func (s *PGStore) Muted(ctx context.Context, owner int64) ([]string, error) {
	return s.muted(sys(ctx), `SELECT muted FROM push_preferences WHERE owner_id = $1`, owner)
}

// MyMuted reads the CALLER's muted events (row-level security).
func (s *PGStore) MyMuted(ctx context.Context) ([]string, error) {
	return s.muted(ctx, `SELECT muted FROM push_preferences`)
}

func (s *PGStore) muted(ctx context.Context, q string, args ...any) ([]string, error) {
	var out []string
	err := s.pool.QueryRow(ctx, q, args...).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{}, nil
	}
	return out, err
}

// SetMuted replaces the CALLER's muted events. Names outside Events are
// dropped, so the stored set is always a subset of the vocabulary.
func (s *PGStore) SetMuted(ctx context.Context, muted []string) error {
	clean := []string{}
	for _, e := range Events {
		for _, m := range muted {
			if m == e.Name {
				clean = append(clean, e.Name)
				break
			}
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO push_preferences (muted) VALUES ($1)
		ON CONFLICT (owner_id) DO UPDATE SET muted = EXCLUDED.muted, updated_at = now()`, clean)
	return err
}
