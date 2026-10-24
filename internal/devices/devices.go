// Package devices is muster's paired-phone store: the native Android app's
// DEVICE CREDENTIAL (`mdv_…`) and the one-time PAIRING CODE that mints it
// (migration 0039; docs/design/android.md, "The device credential").
//
// The flow, in one line each:
//
//   - A signed-in web session asks for a pairing code (CreatePairingCode, the
//     caller's own principal). It needs a sign-in from the last five minutes —
//     internal/api checks that, as for adding a passkey — because the device
//     credential it leads to outlives the session.
//   - The app redeems the code (Redeem) for the device credential, sending the
//     device's name, app version and its step-up public key. The code is single
//     use and lives PairingCodeTTL.
//   - Every request the app makes presents the credential (Authenticate).
//
// 🔴 A DEVICE CREDENTIAL CANNOT MINT ANOTHER. A code is created only under a
// browser session's principal (internal/api's session tier), and the device
// door refuses an `mdv_` credential everywhere outside /api/device/*. So "a
// token cannot mint a token" holds for this credential as it does for `mst_`.
//
// 🔴 EVERY REVOCATION DELETES THE PHONE: Remove and unpair here, and a disable,
// a password reset and a Sign out everywhere in internal/users
// (deleteDevicesTx), each in the transaction that records it. Its push
// subscription goes with it. THE RULE BELOW IS THE SECOND WALL, READ ON EVERY
// REQUEST WITH NOTHING CACHED, for a row that outlived its revocation:
// Authenticate joins the device to its owner's users row and refuses the
// credential when:
//
//   - the device row is gone (the owner removed it on Account → Devices, or the
//     app unpaired itself);
//   - the owner is disabled (ErrUserDisabled);
//   - the pairing session's SIGN-IN is at or before the owner's session cutoff,
//     users.sessions_not_before, which every disable and password reset moves
//     (ErrSessionEnded);
//   - the pairing session's sign-in is at or before the owner's latest "Sign out
//     everywhere", users.signed_out_at (ErrSignedOut). Only gotrue mode stamps
//     that column, so this path exists in gotrue mode only. It is the design's
//     RECOMMENDATION, not an operator requirement: it is what "everywhere" is
//     expected to mean for a phone, and it is stricter than `mst_` tokens,
//     which Sign out everywhere does not revoke (docs/decisions.md).
//
// "At or before" is in whole seconds, the rule the session tier and the send
// queue already use (a sign-in in the cutoff's own second is refused).
//
// 🔴 TWO KINDS OF METHOD, AS IN internal/push. CreatePairingCode, List and
// Remove run under the CALLER's principal, and devices and
// device_pairing_codes are under FORCE row-level security, so they see only
// that user's rows. Redeem and Authenticate cannot: each is how a request's
// owner is LEARNED (a code or a credential, by its hash). They run under
// authz.System through this package's one helper, sys (ledgered in
// internal/authz/ledger_test.go), and hand on only the row's owner.
package devices

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

const (
	// TokenPrefix starts every device credential. The doors tell it from every
	// other credential by this prefix before any database read.
	TokenPrefix = "mdv_"
	// CodePrefix starts every pairing code.
	CodePrefix = "mdp_"
	// encodedLen is the base64url (no padding) length of 32 bytes.
	encodedLen = 43

	// PairingCodeTTL is how long a pairing code can be redeemed.
	PairingCodeTTL = 60 * time.Second
	// MaxPerOwner bounds one user's paired devices, the same bound push
	// subscriptions have (push.MaxDevicesPerOwner).
	MaxPerOwner = 20
	// MaxNameLen and MaxAppVersionLen bound the labels the app sends.
	MaxNameLen       = 100
	MaxAppVersionLen = 64
	// LastSeenGranularity is how stale last_seen_at may get before an
	// authentication rewrites it.
	LastSeenGranularity = time.Minute
)

var (
	// ErrBadToken: not a live device credential — malformed, unknown, or the
	// device was removed. One error for all three.
	ErrBadToken = errors.New("devices: invalid or removed device credential")
	// ErrBadCode: not a redeemable pairing code — malformed, unknown, used or
	// expired. One error for all four.
	ErrBadCode = errors.New("devices: invalid, used or expired pairing code")
	// ErrUserDisabled: the device's owner is disabled.
	ErrUserDisabled = errors.New("devices: the device's user is disabled")
	// ErrSessionEnded: the pairing session's sign-in is at or before the
	// owner's session cutoff (a disable or a password reset moved it).
	ErrSessionEnded = errors.New("devices: the session that paired this device has been ended")
	// ErrSignedOut: the pairing session's sign-in is at or before the owner's
	// latest Sign out everywhere.
	ErrSignedOut = errors.New("devices: the owner signed out everywhere after this device was paired")
	// ErrTooMany: the owner already has MaxPerOwner devices.
	ErrTooMany = fmt.Errorf("devices: a user may pair at most %d devices; remove one on the account page", MaxPerOwner)
	// ErrInvalid: a label or the step-up key is not acceptable.
	ErrInvalid = errors.New("devices: invalid device details")
	// ErrNoUser: a session-tier call with no user principal.
	ErrNoUser = errors.New("devices: no user principal on this context")
	// ErrNoSignIn: a pairing code was asked for with no sign-in time.
	ErrNoSignIn = errors.New("devices: a pairing code needs the session's sign-in time")
	// ErrKeyNotAllowed: the step-up key's kind or security level is one this
	// server refuses (a software key always; a biometric-or-PIN key while the
	// PIN-key policy is off).
	ErrKeyNotAllowed = errors.New("devices: this step-up key is not accepted")
)

// The step-up key's type (docs/design/android.md, "The step-up key"):
// biometric-only by default; biometric-or-device-PIN only when the operator
// allows PIN keys (MUSTER_ANDROID_ALLOW_PIN_KEYS).
const (
	KeyBiometric             = "biometric"
	KeyBiometricOrCredential = "biometric_or_credential"
)

// The Keystore security levels the app reports (KeyInfo.getSecurityLevel). A
// software key is refused: until key attestation exists the server trusts
// the app's report, and it refuses the one level that says "not hardware".
const (
	LevelSoftware           = "software"
	LevelTrustedEnvironment = "trusted_environment"
	LevelStrongBox          = "strongbox"
	LevelUnknownSecure      = "unknown_secure"
)

// checkKey validates the key's type and level against the policy. No key is
// "read-only": then type and level must be absent too.
func checkKey(key []byte, in PairIn) error {
	if key == nil {
		if in.KeyType != "" || in.KeyLevel != "" {
			return fmt.Errorf("%w: a key type or level without a step-up key", ErrInvalid)
		}
		return nil
	}
	switch in.KeyLevel {
	case LevelTrustedEnvironment, LevelStrongBox, LevelUnknownSecure:
	case LevelSoftware:
		return fmt.Errorf("%w: a software-only key cannot gate actions", ErrKeyNotAllowed)
	default:
		return fmt.Errorf("%w: the key's security level %q is not one the app reports", ErrInvalid, in.KeyLevel)
	}
	switch in.KeyType {
	case KeyBiometric:
	case KeyBiometricOrCredential:
		if !in.AllowPINKeys {
			return fmt.Errorf("%w: this server does not accept biometric-or-PIN keys", ErrKeyNotAllowed)
		}
	default:
		return fmt.Errorf("%w: the key type %q is not %s or %s", ErrInvalid, in.KeyType, KeyBiometric, KeyBiometricOrCredential)
	}
	return nil
}

// Device is one paired device's record. It never carries the credential or
// its hash.
type Device struct {
	ID         int64
	OwnerID    int64
	Name       string
	AppVersion string
	// HasStepUpKey: the device registered a step-up public key at pairing. A
	// phone without an acceptable key pairs without one (read-only).
	HasStepUpKey bool
	// KeyType and KeyLevel are the step-up key's type and reported Keystore
	// security level; empty with no key.
	KeyType  string
	KeyLevel string
	// PairedSigninAt is the pairing session's sign-in time, which every
	// revocation check compares against.
	PairedSigninAt time.Time
	CreatedAt      time.Time
	LastSeenAt     *time.Time
	// PushRegistered: the device holds a push subscription (List only).
	PushRegistered bool
}

// PairIn is what the app sends with a pairing code.
type PairIn struct {
	Name       string
	AppVersion string
	// StepUpKey is the step-up public key as SubjectPublicKeyInfo DER, base64
	// (standard or URL alphabet, padded or not). Empty: no key (read-only).
	StepUpKey string
	// KeyType (KeyBiometric | KeyBiometricOrCredential) and KeyLevel (the
	// Keystore security level) describe StepUpKey; both empty with no key.
	KeyType  string
	KeyLevel string
	// AllowPINKeys is the SERVER's policy (MUSTER_ANDROID_ALLOW_PIN_KEYS),
	// set by the handler, never read from the request.
	AllowPINKeys bool
}

func newSecret(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("devices: mint a secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func shapeOK(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != encodedLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// LooksLike reports whether s carries the device-credential prefix. It decides
// which DOOR a credential is for, not whether it is valid.
func LooksLike(s string) bool { return strings.HasPrefix(s, TokenPrefix) }

// ShapeOK reports whether s is shaped like a minted device credential.
func ShapeOK(s string) bool { return shapeOK(TokenPrefix, s) }

// CodeShapeOK reports whether s is shaped like a minted pairing code.
func CodeShapeOK(s string) bool { return shapeOK(CodePrefix, s) }

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ParseStepUpKey decodes a step-up public key: base64 SubjectPublicKeyInfo DER
// of an EC P-256 key (the Android Keystore's KeyProperties.KEY_ALGORITHM_EC
// with secp256r1). It returns the DER to store. Empty is no key.
func ParseStepUpKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) > 512 {
		return nil, fmt.Errorf("%w: the step-up key is longer than any P-256 key", ErrInvalid)
	}
	raw := strings.TrimRight(s, "=")
	der, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		der, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: the step-up key is not base64", ErrInvalid)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: the step-up key is not a SubjectPublicKeyInfo", ErrInvalid)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the step-up key must be an EC P-256 key", ErrInvalid)
	}
	return der, nil
}

func cleanLabel(s string, max int, what string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max || strings.ContainsAny(s, "\x00\r\n") {
		return "", fmt.Errorf("%w: the %s is longer than %d characters or has a control character", ErrInvalid, what, max)
	}
	return s, nil
}

// atOrBefore is whether at is at or before cut, in whole seconds; false for no
// cut. The session tier's and the send queue's rule.
func atOrBefore(at time.Time, cut *time.Time) bool {
	return cut != nil && at.Unix() <= cut.Unix()
}

// ownerRefusal is why owner u can no longer hold a device paired by a sign-in
// at signin, or nil. The one revocation rule, used by Redeem and Authenticate.
func ownerRefusal(disabled bool, signin time.Time, cutoff, signedOut *time.Time) error {
	switch {
	case disabled:
		return ErrUserDisabled
	case atOrBefore(signin, cutoff):
		return ErrSessionEnded
	case atOrBefore(signin, signedOut):
		return ErrSignedOut
	}
	return nil
}

// deviceCols is a device row as Device holds it, in deviceDest's order; cols
// is the same list qualified by a table alias.
var deviceCols = cols("")

func cols(alias string) string {
	q := func(c string) string {
		if alias == "" {
			return c
		}
		return alias + "." + c
	}
	return strings.Join([]string{q("id"), q("owner_id"), q("name"), q("app_version"), q("stepup_pubkey") + " IS NOT NULL",
		"coalesce(" + q("stepup_key_type") + ", '')", "coalesce(" + q("stepup_key_level") + ", '')",
		q("paired_signin_at"), q("created_at"), q("last_seen_at")}, ", ")
}

func deviceDest(d *Device) []any {
	return []any{&d.ID, &d.OwnerID, &d.Name, &d.AppVersion, &d.HasStepUpKey, &d.KeyType, &d.KeyLevel,
		&d.PairedSigninAt, &d.CreatedAt, &d.LastSeenAt}
}

// LiveSQL is ownerRefusal as a SQL predicate over a devices alias and its
// owner's users alias: true while the device's owner may still hold it. It is
// the SAME rule, for the readers that select rather than authenticate (the
// push fan-out's android rows, internal/push), and
// TestTheSQLRuleIsTheGoRule pins the two to each other.
func LiveSQL(dev, usr string) string {
	sec := func(col string) string { return "floor(extract(epoch FROM " + col + "))" }
	return "(" + usr + ".disabled_at IS NULL" +
		" AND (" + usr + ".sessions_not_before IS NULL OR " + sec(dev+".paired_signin_at") + " > " + sec(usr+".sessions_not_before") + ")" +
		" AND (" + usr + ".signed_out_at IS NULL OR " + sec(dev+".paired_signin_at") + " > " + sec(usr+".signed_out_at") + "))"
}

// sys is this package's ONE authz.System call (ledgered). See the package doc.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

// Store is the device store.
type Store interface {
	// CreatePairingCode mints a pairing code for ctx's user, recording the
	// session's sign-in time. Session tier only.
	CreatePairingCode(ctx context.Context, signinAt time.Time) (code string, expiresAt time.Time, err error)
	// Redeem spends a pairing code and pairs a device, returning the
	// credential ONCE. Public: the code is the authority.
	Redeem(ctx context.Context, code string, in PairIn) (token string, d Device, err error)
	// Authenticate resolves a credential to its live device, or refuses it
	// (ErrBadToken, ErrUserDisabled, ErrSessionEnded, ErrSignedOut).
	Authenticate(ctx context.Context, token string) (Device, error)
	// List is ctx's user's devices, newest first.
	List(ctx context.Context) ([]Device, error)
	// PairingPending reports whether ctx's user has a pairing code that is
	// neither used nor expired: an app may be about to redeem it.
	PairingPending(ctx context.Context) (bool, error)
	// Remove deletes ctx's user's device id (its push subscription goes with
	// it). found is false when the user has no such device.
	Remove(ctx context.Context, id int64) (found bool, err error)
}

// PGStore is Store over Postgres (migration 0039).
type PGStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns the store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool, now: time.Now} }

var _ Store = (*PGStore)(nil)

func user(ctx context.Context) (authz.Principal, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return authz.Principal{}, ErrNoUser
	}
	return p, nil
}

// Object is the audit_log `object` a device's rows carry.
func Object(id int64) string { return "device:" + strconv.FormatInt(id, 10) }

func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreatePairingCode implements Store. signinAt is the pairing session's
// sign-in time (stepup.AuthTime); a zero one is refused, never stored as "no
// sign-in".
func (s *PGStore) CreatePairingCode(ctx context.Context, signinAt time.Time) (string, time.Time, error) {
	p, err := user(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if signinAt.IsZero() {
		return "", time.Time{}, ErrNoSignIn
	}
	code, err := newSecret(CodePrefix)
	if err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	// Expired codes of this user go with each new one: a code is worthless a
	// minute after it is minted, so nothing else needs to sweep them.
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM device_pairing_codes WHERE owner_id = $1 AND expires_at < now()`, p.UserID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO device_pairing_codes (owner_id, code_sha256, signin_at, expires_at)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4)) RETURNING expires_at`,
			p.UserID, hash(code), signinAt, PairingCodeTTL.Seconds()).Scan(&expires)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// Redeem implements Store.
//
// 🔴 IT TAKES THE OWNER'S CREDENTIALS LOCK (users.LockCredentialsTx), so the
// device cap cannot be raced past, and it re-checks the owner (disabled, the
// cutoff, the latest sign-out everywhere) against the code's sign-in time with
// the same rule Authenticate uses: a code minted by a session that has since
// been ended pairs nothing. The code is spent only by a pairing that commits.
func (s *PGStore) Redeem(ctx context.Context, code string, in PairIn) (string, Device, error) {
	if !CodeShapeOK(code) {
		return "", Device{}, ErrBadCode
	}
	name, err := cleanLabel(in.Name, MaxNameLen, "device name")
	if err != nil {
		return "", Device{}, err
	}
	version, err := cleanLabel(in.AppVersion, MaxAppVersionLen, "app version")
	if err != nil {
		return "", Device{}, err
	}
	key, err := ParseStepUpKey(in.StepUpKey)
	if err != nil {
		return "", Device{}, err
	}
	if err := checkKey(key, in); err != nil {
		return "", Device{}, err
	}
	var keyType, keyLevel *string
	if key != nil {
		keyType, keyLevel = &in.KeyType, &in.KeyLevel
	}
	tok, err := newSecret(TokenPrefix)
	if err != nil {
		return "", Device{}, err
	}
	ctx = sys(ctx)
	var d Device
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			codeID, owner int64
			signin        time.Time
		)
		// 🔴 THE CREDENTIALS LOCK COMES BEFORE ANY ROW LOCK (users.LockCredentialsTx's
		// order rule). The code row names the owner whose lock to take, so it is
		// read FIRST WITHOUT a lock, then the owner's credentials lock is taken,
		// then the row is locked and re-checked: a code spent, or one that names
		// another owner, between the two reads is refused like any spent code.
		// ⚠ Measured, not assumed: today no writer that holds the credentials
		// lock touches a code row (the revocations delete devices, not codes),
		// so the reverse order would not deadlock either — a mutation that locks
		// the code row first survives TestCredentialWritersNeverDeadlock. The
		// order is kept so the rule has no exception for the next writer.
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM device_pairing_codes WHERE code_sha256 = $1`, hash(code)).
			Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
			return ErrBadCode
		} else if err != nil {
			return err
		}
		if err := users.LockCredentialsTx(ctx, tx, owner); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id, signin_at FROM device_pairing_codes
			WHERE code_sha256 = $1 AND owner_id = $2 AND used_at IS NULL AND expires_at > now() FOR UPDATE`, hash(code), owner).
			Scan(&codeID, &signin)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadCode
		}
		if err != nil {
			return err
		}
		var (
			disabled          bool
			cutoff, signedOut *time.Time
		)
		if err := tx.QueryRow(ctx, `SELECT disabled_at IS NOT NULL, sessions_not_before, signed_out_at FROM users WHERE id = $1`,
			owner).Scan(&disabled, &cutoff, &signedOut); err != nil {
			return err
		}
		if err := ownerRefusal(disabled, signin, cutoff, signedOut); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM devices WHERE owner_id = $1`, owner).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPerOwner {
			return ErrTooMany
		}
		if _, err := tx.Exec(ctx, `UPDATE device_pairing_codes SET used_at = now() WHERE id = $1`, codeID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO devices (owner_id, name, app_version, token_sha256, stepup_pubkey,
				stepup_key_type, stepup_key_level, paired_signin_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+deviceCols,
			owner, name, version, hash(tok), key, keyType, keyLevel, signin).
			Scan(deviceDest(&d)...)
		if err != nil {
			return err
		}
		return users.AuditTx(ctx, tx, users.Actor{UserID: owner, Via: authz.ViaDevice}, "device.pair", Object(d.ID),
			map[string]any{"name": d.Name, "app_version": d.AppVersion, "stepup_key": d.HasStepUpKey,
				"stepup_key_type": d.KeyType, "stepup_key_level": d.KeyLevel})
	})
	if err != nil {
		return "", Device{}, err
	}
	return tok, d, nil
}

// Authenticate implements Store. See the package doc for the refusals.
//
// 🔴 THE SHAPE CHECK COMES FIRST AND COSTS NO I/O. last_seen_at is best
// effort: a failure to write it never refuses a live credential.
func (s *PGStore) Authenticate(ctx context.Context, token string) (Device, error) {
	if !ShapeOK(token) {
		return Device{}, ErrBadToken
	}
	ctx = sys(ctx)
	var (
		d                 Device
		disabled          bool
		cutoff, signedOut *time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT `+cols("d")+`,
			u.disabled_at IS NOT NULL, u.sessions_not_before, u.signed_out_at
		FROM devices d JOIN users u ON u.id = d.owner_id
		WHERE d.token_sha256 = $1`, hash(token)).
		Scan(append(deviceDest(&d), &disabled, &cutoff, &signedOut)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrBadToken
	}
	if err != nil {
		return Device{}, err
	}
	if err := ownerRefusal(disabled, d.PairedSigninAt, cutoff, signedOut); err != nil {
		return Device{}, err
	}
	if now := s.now(); d.LastSeenAt == nil || now.Sub(*d.LastSeenAt) >= LastSeenGranularity {
		var seen time.Time
		if err := s.pool.QueryRow(ctx, `UPDATE devices SET last_seen_at = now() WHERE id = $1 RETURNING last_seen_at`,
			d.ID).Scan(&seen); err == nil {
			d.LastSeenAt = &seen
		}
	}
	return d, nil
}

// List implements Store.
func (s *PGStore) List(ctx context.Context) ([]Device, error) {
	p, err := user(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols("d")+`,
			EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.device_id = d.id)
		FROM devices d WHERE d.owner_id = $1 ORDER BY d.created_at DESC, d.id DESC`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(append(deviceDest(&d), &d.PushRegistered)...); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PairingPending implements Store, under the caller's principal (row-level
// security) like List.
func (s *PGStore) PairingPending(ctx context.Context) (bool, error) {
	p, err := user(ctx)
	if err != nil {
		return false, err
	}
	var pending bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM device_pairing_codes
		WHERE owner_id = $1 AND used_at IS NULL AND expires_at > now())`, p.UserID).Scan(&pending)
	return pending, err
}

// Remove implements Store. The row is DELETED, so the credential stops working
// on its next request and the device's push subscription goes with it (ON
// DELETE CASCADE). The device's own unpair (DELETE /api/device) is this call
// under the device's owner.
func (s *PGStore) Remove(ctx context.Context, id int64) (bool, error) {
	p, err := user(ctx)
	if err != nil {
		return false, err
	}
	found := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := users.LockCredentialsTx(ctx, tx, p.UserID); err != nil {
			return err
		}
		var name string
		err := tx.QueryRow(ctx, `DELETE FROM devices WHERE id = $1 AND owner_id = $2 RETURNING name`, id, p.UserID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return users.AuditTx(ctx, tx, users.Actor{UserID: p.UserID, Via: p.Via}, "device.remove", Object(id),
			map[string]any{"name": name})
	})
	return found, err
}
