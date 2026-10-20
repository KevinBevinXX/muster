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
	tok, d, err := r.s.Redeem(context.Background(), code, PairIn{Name: "Pixel", AppVersion: "1.2.3", StepUpKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if !ShapeOK(tok) || d.OwnerID != r.a || !d.HasStepUpKey || d.Name != "Pixel" {
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

// TestEveryRevocationPathRefusesTheCredential: each of the four ways a device
// is cut off refuses its credential on the next Authenticate, with its own
// error — and the CONTROL for the two cutoffs is a device paired by a sign-in
// AFTER the cutoff, which still works: the rule compares the pairing sign-in,
// it does not revoke every device forever.
func TestEveryRevocationPathRefusesTheCredential(t *testing.T) {
	earlier := time.Now().Add(-10 * time.Second)

	t.Run("removed on Account → Devices", func(t *testing.T) {
		r := newRig(t)
		tok, d := r.pair(t, r.a, earlier, PairIn{})
		if found, err := r.s.Remove(as(r.a), d.ID); err != nil || !found {
			t.Fatalf("remove: %v %v", found, err)
		}
		if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrBadToken) {
			t.Fatalf("a removed device: %v; want ErrBadToken", err)
		}
	})
	t.Run("account disabled", func(t *testing.T) {
		r := newRig(t)
		tok, _ := r.pair(t, r.a, earlier, PairIn{})
		if _, err := r.users.SetDisabled(context.Background(), r.a, true); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrUserDisabled) {
			t.Fatalf("a disabled owner: %v; want ErrUserDisabled", err)
		}
		// Re-enabling does not bring it back: the disable moved the cutoff past
		// the pairing sign-in.
		if _, err := r.users.SetDisabled(context.Background(), r.a, false); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrSessionEnded) {
			t.Fatalf("after a re-enable: %v; want ErrSessionEnded", err)
		}
	})
	t.Run("session cutoff (password reset)", func(t *testing.T) {
		r := newRig(t)
		tok, _ := r.pair(t, r.a, earlier, PairIn{})
		cut, err := r.users.ResetCredentials(context.Background(), r.a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrSessionEnded) {
			t.Fatalf("a device paired before the cutoff: %v; want ErrSessionEnded", err)
		}
		after, _ := r.pair(t, r.a, cut.Add(2*time.Second), PairIn{})
		if _, err := r.s.Authenticate(context.Background(), after); err != nil {
			t.Fatalf("CONTROL: a device paired by a sign-in after the cutoff: %v", err)
		}
	})
	t.Run("sign out everywhere", func(t *testing.T) {
		r := newRig(t)
		tok, _ := r.pair(t, r.a, earlier, PairIn{})
		other, _ := r.pair(t, r.b, earlier, PairIn{})
		out, err := r.users.RecordSignOutEverywhere(context.Background(), r.a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Authenticate(context.Background(), tok); !errors.Is(err, ErrSignedOut) {
			t.Fatalf("a device paired before sign out everywhere: %v; want ErrSignedOut", err)
		}
		if _, err := r.s.Authenticate(context.Background(), other); err != nil {
			t.Fatalf("CONTROL: another user's device: %v", err)
		}
		after, _ := r.pair(t, r.a, out.Add(2*time.Second), PairIn{})
		if _, err := r.s.Authenticate(context.Background(), after); err != nil {
			t.Fatalf("CONTROL: a device paired by a sign-in after the sign-out: %v", err)
		}
	})
	t.Run("the cutoff's own second is refused", func(t *testing.T) {
		r := newRig(t)
		out, err := r.users.RecordSignOutEverywhere(context.Background(), r.a)
		if err != nil {
			t.Fatal(err)
		}
		// Paired AFTER the sign-out was recorded, by a sign-in stamped in the
		// sign-out's own second: refused at the redeem already.
		code, _, err := r.s.CreatePairingCode(as(r.a), out.Truncate(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.s.Redeem(context.Background(), code, PairIn{}); !errors.Is(err, ErrSignedOut) {
			t.Fatalf("a code from a sign-in in the sign-out's second: %v; want ErrSignedOut", err)
		}
	})
}

// TestACodeFromAnEndedSessionPairsNothing: a pairing code minted before a
// disable or a reset is refused at the redeem, with the reason, and spent
// nothing.
func TestACodeFromAnEndedSessionPairsNothing(t *testing.T) {
	r := newRig(t)
	code, _, err := r.s.CreatePairingCode(as(r.a), time.Now().Add(-10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.users.ResetCredentials(context.Background(), r.a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.s.Redeem(context.Background(), code, PairIn{}); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a code from a reset session: %v; want ErrSessionEnded", err)
	}
	if list, err := r.s.List(as(r.a)); err != nil || len(list) != 0 {
		t.Fatalf("devices after the refused redeem: %v, %v", list, err)
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
