package devices

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// rig is a migrated database with two fresh member users, a and b.
type rig struct {
	s     *PGStore
	pool  *pgxpool.Pool
	users *users.PGStore
	a, b  int64
}

func newRig(t *testing.T) rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := rig{s: NewPG(pool), pool: pool, users: users.NewPG(pool)}
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	for _, id := range []*int64{&r.a, &r.b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
			"dev"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		ids := []int64{r.a, r.b}
		for _, q := range []string{`DELETE FROM push_subscriptions WHERE owner_id = ANY($1)`,
			`DELETE FROM device_pairing_codes WHERE owner_id = ANY($1)`, `DELETE FROM devices WHERE owner_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(bg, q, ids)
		}
	})
	return r
}

func as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// pair runs the whole pairing as uid, with the pairing session signed in at
// signin, and returns the credential and device.
func (r rig) pair(t *testing.T, uid int64, signin time.Time, in PairIn) (string, Device) {
	t.Helper()
	code, _, err := r.s.CreatePairingCode(as(uid), signin)
	if err != nil {
		t.Fatalf("pairing code: %v", err)
	}
	tok, d, err := r.s.Redeem(context.Background(), code, in)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return tok, d
}

func p256SPKI(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// TestPairingIssuesOneCredentialPerCodeAndStoresOnlyHashes: a code redeems
// once; the credential authenticates as its owner; neither the code nor the
// credential is stored, only their sha256; the step-up key is stored as DER.
func TestPairingIssuesOneCredentialPerCodeAndStoresOnlyHashes(t *testing.T) {
	r := newRig(t)
	key := p256SPKI(t)
	code, expires, err := r.s.CreatePairingCode(as(r.a), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d <= 0 || d > PairingCodeTTL+5*time.Second {
		t.Fatalf("the code expires in %s; want about %s", d, PairingCodeTTL)
	}
	tok, d, err := r.s.Redeem(context.Background(), code, PairIn{Name: "Pixel", AppVersion: "1.2.3", StepUpKey: key,
		KeyType: KeyBiometric, KeyLevel: LevelTrustedEnvironment})
	if err != nil {
		t.Fatal(err)
	}
	if !ShapeOK(tok) || d.OwnerID != r.a || !d.HasStepUpKey || d.Name != "Pixel" || d.KeyType != KeyBiometric {
		t.Fatalf("token %q device %+v", tok, d)
	}
	if _, _, err := r.s.Redeem(context.Background(), code, PairIn{Name: "again"}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a spent code redeemed again: %v; want ErrBadCode", err)
	}
	got, err := r.s.Authenticate(context.Background(), tok)
	if err != nil || got.ID != d.ID || got.OwnerID != r.a {
		t.Fatalf("authenticate = %+v, %v", got, err)
	}
	sys := authz.System(context.Background())
	var stored, storedKey []byte
	if err := r.pool.QueryRow(sys, `SELECT token_sha256, stepup_pubkey FROM devices WHERE id = $1`, d.ID).Scan(&stored, &storedKey); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, hash(tok)) || bytes.Contains(stored, []byte(tok)) {
		t.Fatal("the stored credential is not its sha256")
	}
	der, _ := base64.StdEncoding.DecodeString(key)
	if !bytes.Equal(storedKey, der) {
		t.Fatal("the step-up key was not stored as its DER")
	}
	var codeRows int
	if err := r.pool.QueryRow(sys, `SELECT count(*) FROM device_pairing_codes WHERE code_sha256 = $1 AND used_at IS NOT NULL`,
		hash(code)).Scan(&codeRows); err != nil || codeRows != 1 {
		t.Fatalf("the spent code's row: %d, %v", codeRows, err)
	}
}

// TestAnExpiredOrForeignShapedCodeIsRefused: a code past its minute, and
// values of the wrong shape (a device credential among them), redeem nothing.
func TestAnExpiredOrForeignShapedCodeIsRefused(t *testing.T) {
	r := newRig(t)
	code, _, err := r.s.CreatePairingCode(as(r.a), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(authz.System(context.Background()),
		`UPDATE device_pairing_codes SET expires_at = now() - interval '1 second' WHERE code_sha256 = $1`, hash(code)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.s.Redeem(context.Background(), code, PairIn{}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an expired code: %v; want ErrBadCode", err)
	}
	tok, _ := r.pair(t, r.a, time.Now(), PairIn{})
	for _, v := range []string{"", "mdp_short", tok, "mst_" + tok[4:]} {
		if _, _, err := r.s.Redeem(context.Background(), v, PairIn{}); !errors.Is(err, ErrBadCode) {
			t.Errorf("Redeem(%q) = %v; want ErrBadCode", v, err)
		}
	}
}

// TestEveryRevocationDeletesThePhone: each way a person ends a phone — Remove,
// a disable, a password reset (the session cutoff) and Sign out everywhere —
// goes through its REAL writer and deletes the phone's row, its push
// subscription and its unspent pairing codes, so the credential is refused
// (ErrBadToken), the phone gets no push, it no longer counts toward the cap
// and Account → Devices no longer lists it. Another user's phone is untouched
// (control), and a phone paired by a sign-in AFTER the event works.
func TestEveryRevocationDeletesThePhone(t *testing.T) {
	earlier := time.Now().Add(-10 * time.Second)
	for _, c := range []struct {
		name   string
		revoke func(r rig, id int64) (time.Time, error)
	}{
		{"removed on Account → Devices", func(r rig, id int64) (time.Time, error) {
			found, err := r.s.Remove(as(r.a), id)
			if err == nil && !found {
				err = errors.New("not found")
			}
			return time.Now(), err
		}},
		{"account disabled", func(r rig, _ int64) (time.Time, error) {
			u, err := r.users.SetDisabled(context.Background(), r.a, true)
			if err != nil {
				return time.Time{}, err
			}
			_, err = r.users.SetDisabled(context.Background(), r.a, false)
			return *u.SessionsNotBefore, err
		}},
		{"password reset (session cutoff)", func(r rig, _ int64) (time.Time, error) {
			return r.users.ResetCredentials(context.Background(), r.a)
		}},
		{"sign out everywhere", func(r rig, _ int64) (time.Time, error) {
			return r.users.RecordSignOutEverywhere(context.Background(), r.a)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			tok, d := r.pair(t, r.a, earlier, PairIn{})
			other, _ := r.pair(t, r.b, earlier, PairIn{})
			sys := authz.System(context.Background())
			if _, err := r.pool.Exec(sys, `INSERT INTO push_subscriptions (owner_id, endpoint, p256dh, auth, kind, device_id)
				VALUES ($1, $2, 'k', 'a', 'android', $3)`, r.a, "https://ntfy.example.org/up"+strconv.FormatInt(d.ID, 10), d.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.s.CreatePairingCode(as(r.a), earlier); err != nil {
				t.Fatal(err)
			}
			at, err := c.revoke(r, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrBadToken) {
				t.Fatalf("the phone's credential: %v; want ErrBadToken (its row deleted)", err)
			}
			for _, q := range []string{`SELECT count(*) FROM devices WHERE owner_id = $1`,
				`SELECT count(*) FROM push_subscriptions WHERE owner_id = $1`} {
				var n int
				if err := r.pool.QueryRow(sys, q, r.a).Scan(&n); err != nil || n != 0 {
					t.Fatalf("%s → %d (%v); want 0", q, n, err)
				}
			}
			if list, err := r.s.List(as(r.a)); err != nil || len(list) != 0 {
				t.Fatalf("Account → Devices still lists %v (%v)", list, err)
			}
			if _, err := r.s.Authenticate(context.Background(), other); err != nil {
				t.Fatalf("CONTROL: another user's phone: %v", err)
			}
			after, _ := r.pair(t, r.a, at.Add(2*time.Second), PairIn{})
			if _, err := r.s.Authenticate(context.Background(), after); err != nil {
				t.Fatalf("CONTROL: a phone paired by a sign-in after the event: %v", err)
			}
		})
	}
}

// TestTheRuleRefusesAPhoneWhoseRowOutlivedItsRevocation is the SECOND wall:
// with the account columns moved directly (no writer, so no delete), the
// credential is still refused, each with its own error. The control is a
// phone paired by a sign-in after the cut, which works; and the cut's own
// second is refused (whole seconds, the session tier's rule).
func TestTheRuleRefusesAPhoneWhoseRowOutlivedItsRevocation(t *testing.T) {
	earlier := time.Now().Add(-10 * time.Second)
	sys := authz.System(context.Background())
	for _, c := range []struct {
		name, set string
		want      error
	}{
		{"disabled", `disabled_at = now()`, ErrUserDisabled},
		{"session cutoff", `sessions_not_before = now()`, ErrSessionEnded},
		{"sign out everywhere", `signed_out_at = now()`, ErrSignedOut},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			tok, _ := r.pair(t, r.a, earlier, PairIn{})
			if _, err := r.pool.Exec(sys, `UPDATE users SET `+c.set+` WHERE id = $1`, r.a); err != nil {
				t.Fatal(err)
			}
			if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, c.want) {
				t.Fatalf("a surviving row: %v; want %v", err, c.want)
			}
			if c.want == ErrUserDisabled {
				return
			}
			var cut time.Time
			if err := r.pool.QueryRow(sys, `SELECT greatest(coalesce(sessions_not_before, 'epoch'), coalesce(signed_out_at, 'epoch'))
				FROM users WHERE id = $1`, r.a).Scan(&cut); err != nil {
				t.Fatal(err)
			}
			same, _ := r.pairRaw(t, r.a, cut.Truncate(time.Second))
			if _, err := r.s.Authenticate(context.Background(), same); !errors.Is(err, c.want) {
				t.Fatalf("a sign-in in the cut's own second: %v; want %v", err, c.want)
			}
			after, _ := r.pairRaw(t, r.a, cut.Add(2*time.Second))
			if _, err := r.s.Authenticate(context.Background(), after); err != nil {
				t.Fatalf("CONTROL: a sign-in after the cut: %v", err)
			}
		})
	}
}

// pairRaw inserts a device directly, paired by a sign-in at signin: the
// fixture for a row Redeem would itself refuse.
func (r rig) pairRaw(t *testing.T, uid int64, signin time.Time) (string, int64) {
	t.Helper()
	tok, err := newSecret(TokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := r.pool.QueryRow(authz.System(context.Background()), `INSERT INTO devices (owner_id, token_sha256, paired_signin_at)
		VALUES ($1, $2, $3) RETURNING id`, uid, hash(tok), signin).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return tok, id
}

// TestTheSQLRuleIsTheGoRule pins LiveSQL (the push fan-out's filter) to
// ownerRefusal (the credential's check) over every boundary: the same cases,
// one evaluated in Go and one by Postgres, must agree.
func TestTheSQLRuleIsTheGoRule(t *testing.T) {
	r := newRig(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 500_000_000, time.UTC)
	ptr := func(d time.Duration) *time.Time { v := base.Add(d); return &v }
	cases := []struct {
		name              string
		disabled          bool
		cutoff, signedOut *time.Time
	}{
		{"nothing", false, nil, nil},
		{"disabled", true, nil, nil},
		{"cutoff a second before", false, ptr(-time.Second), nil},
		{"cutoff the same second, later", false, ptr(300 * time.Millisecond), nil},
		{"cutoff the same second, earlier", false, ptr(-400 * time.Millisecond), nil},
		{"cutoff a second after", false, ptr(time.Second), nil},
		{"sign-out a second before", false, nil, ptr(-time.Second)},
		{"sign-out the same second", false, nil, ptr(200 * time.Millisecond)},
		{"sign-out a second after", false, nil, ptr(time.Second)},
	}
	pos, neg := 0, 0
	for _, c := range cases {
		goLive := ownerRefusal(c.disabled, base, c.cutoff, c.signedOut) == nil
		var dis *time.Time
		if c.disabled {
			dis = &base
		}
		var sqlLive bool
		if err := r.pool.QueryRow(context.Background(), `SELECT `+LiveSQL("d", "u")+`
			FROM (SELECT $1::timestamptz AS paired_signin_at) d,
			     (SELECT $2::timestamptz AS disabled_at, $3::timestamptz AS sessions_not_before, $4::timestamptz AS signed_out_at) u`,
			base, dis, c.cutoff, c.signedOut).Scan(&sqlLive); err != nil {
			t.Fatal(err)
		}
		if goLive != sqlLive {
			t.Errorf("%s: Go says live=%v, SQL says live=%v", c.name, goLive, sqlLive)
		}
		if goLive {
			pos++
		} else {
			neg++
		}
	}
	if pos < 3 || neg < 5 {
		t.Fatalf("CONTROL: the cases are not both ways (%d live, %d refused)", pos, neg)
	}
}

// TestACodeFromAnEndedSessionPairsNothing: a pairing code minted before a
// password reset or a Sign out everywhere is refused at the redeem, with the
// reason, and pairs nothing.
func TestACodeFromAnEndedSessionPairsNothing(t *testing.T) {
	for _, c := range []struct {
		end  func(r rig) error
		want error
	}{
		{func(r rig) error { _, err := r.users.ResetCredentials(context.Background(), r.a); return err }, ErrSessionEnded},
		{func(r rig) error { _, err := r.users.RecordSignOutEverywhere(context.Background(), r.a); return err }, ErrSignedOut},
	} {
		r := newRig(t)
		code, _, err := r.s.CreatePairingCode(as(r.a), time.Now().Add(-10*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.end(r); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.s.Redeem(context.Background(), code, PairIn{}); !errors.Is(err, c.want) {
			t.Fatalf("a code from an ended session: %v; want %v", err, c.want)
		}
		if list, err := r.s.List(as(r.a)); err != nil || len(list) != 0 {
			t.Fatalf("devices after the refused redeem: %v, %v", list, err)
		}
	}
}

// TestTheStepUpKeyPolicy: a key needs its type and a hardware level; a
// software key is refused; a biometric-or-PIN key only with the policy on;
// no key is read-only, with no type or level.
func TestTheStepUpKeyPolicy(t *testing.T) {
	r := newRig(t)
	key := p256SPKI(t)
	redeem := func(in PairIn) (Device, error) {
		code, _, err := r.s.CreatePairingCode(as(r.a), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		_, d, err := r.s.Redeem(context.Background(), code, in)
		return d, err
	}
	for name, c := range map[string]struct {
		in   PairIn
		want error
	}{
		"software level":      {PairIn{StepUpKey: key, KeyType: KeyBiometric, KeyLevel: LevelSoftware}, ErrKeyNotAllowed},
		"PIN key, policy off": {PairIn{StepUpKey: key, KeyType: KeyBiometricOrCredential, KeyLevel: LevelStrongBox}, ErrKeyNotAllowed},
		"a key with no type":  {PairIn{StepUpKey: key, KeyLevel: LevelStrongBox}, ErrInvalid},
		"an unknown level":    {PairIn{StepUpKey: key, KeyType: KeyBiometric, KeyLevel: "tee"}, ErrInvalid},
		"a type with no key":  {PairIn{KeyType: KeyBiometric, KeyLevel: LevelStrongBox}, ErrInvalid},
		"a level with no key": {PairIn{KeyLevel: LevelStrongBox}, ErrInvalid},
	} {
		if _, err := redeem(c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v; want %v", name, err, c.want)
		}
	}
	for _, c := range []PairIn{
		{StepUpKey: key, KeyType: KeyBiometric, KeyLevel: LevelTrustedEnvironment},
		{StepUpKey: key, KeyType: KeyBiometricOrCredential, KeyLevel: LevelStrongBox, AllowPINKeys: true},
		{},
	} {
		d, err := redeem(c)
		if err != nil || d.KeyType != c.KeyType || d.KeyLevel != c.KeyLevel || d.HasStepUpKey != (c.StepUpKey != "") {
			t.Errorf("CONTROL %+v: %+v, %v", c, d, err)
		}
	}
}

// TestDevicesAreTheirOwnersOnly: row-level security keeps each user to their
// own devices and codes; another user's Remove finds nothing.
func TestDevicesAreTheirOwnersOnly(t *testing.T) {
	r := newRig(t)
	tok, d := r.pair(t, r.a, time.Now(), PairIn{Name: "a's phone"})
	if list, err := r.s.List(as(r.b)); err != nil || len(list) != 0 {
		t.Fatalf("B lists %v, %v", list, err)
	}
	if found, err := r.s.Remove(as(r.b), d.ID); err != nil || found {
		t.Fatalf("B removed A's device: %v %v", found, err)
	}
	if _, err := r.s.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("A's device after B's remove: %v", err)
	}
	var n int
	if err := r.pool.QueryRow(as(r.b), `SELECT count(*) FROM devices`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw SQL as B sees %d devices (%v)", n, err)
	}
	if err := r.pool.QueryRow(as(r.b), `SELECT count(*) FROM device_pairing_codes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw SQL as B sees %d pairing codes (%v)", n, err)
	}
	if list, err := r.s.List(as(r.a)); err != nil || len(list) != 1 || list[0].Name != "a's phone" {
		t.Fatalf("CONTROL: A lists %v, %v", list, err)
	}
	if _, _, err := r.s.CreatePairingCode(authz.System(context.Background()), time.Now()); !errors.Is(err, ErrNoUser) {
		t.Fatalf("a code minted as System: %v; want ErrNoUser", err)
	}
	if _, _, err := r.s.CreatePairingCode(as(r.a), time.Time{}); !errors.Is(err, ErrNoSignIn) {
		t.Fatalf("a code with no sign-in time: %v; want ErrNoSignIn", err)
	}
}

// TestTheDeviceCapIsPerOwner: the 21st device is refused.
func TestTheDeviceCapIsPerOwner(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxPerOwner; i++ {
		r.pair(t, r.a, time.Now(), PairIn{})
	}
	code, _, err := r.s.CreatePairingCode(as(r.a), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.s.Redeem(context.Background(), code, PairIn{}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("device %d: %v; want ErrTooMany", MaxPerOwner+1, err)
	}
	r.pair(t, r.b, time.Now(), PairIn{}) // another owner is unaffected
}

// TestTheStepUpKeyMustBeP256 (no database): only an EC P-256
// SubjectPublicKeyInfo is accepted; empty is "no key".
func TestTheStepUpKeyMustBeP256(t *testing.T) {
	if der, err := ParseStepUpKey(p256SPKI(t)); err != nil || len(der) == 0 {
		t.Fatalf("P-256: %v", err)
	}
	raw := p256SPKI(t)
	der, _ := base64.StdEncoding.DecodeString(raw)
	if _, err := ParseStepUpKey(base64.RawURLEncoding.EncodeToString(der)); err != nil {
		t.Fatalf("P-256, URL alphabet: %v", err)
	}
	if der, err := ParseStepUpKey(""); err != nil || der != nil {
		t.Fatalf("empty: %v %v", der, err)
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	d384, _ := x509.MarshalPKIXPublicKey(&k384.PublicKey)
	krsa, _ := rsa.GenerateKey(rand.Reader, 2048)
	drsa, _ := x509.MarshalPKIXPublicKey(&krsa.PublicKey)
	for name, v := range map[string]string{
		"P-384": base64.StdEncoding.EncodeToString(d384), "RSA": base64.StdEncoding.EncodeToString(drsa),
		"junk": "not base64 !!", "not SPKI": base64.StdEncoding.EncodeToString([]byte("hello")),
	} {
		if _, err := ParseStepUpKey(v); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
}
