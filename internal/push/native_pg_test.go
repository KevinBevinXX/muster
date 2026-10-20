package push

import (
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// device inserts a paired device for owner directly (the devices package owns
// pairing; here only the row matters) and returns its id.
func device(t *testing.T, s *PGStore, owner int64, tag string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(sys(t.Context()), `INSERT INTO devices (owner_id, token_sha256, paired_signin_at)
		VALUES ($1, sha256(convert_to($2, 'UTF8')), $3) RETURNING id`, owner, tag+time.Now().String(), time.Now()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(authz.System(t.Context()), `DELETE FROM devices WHERE id = $1`, id) })
	return id
}

// TestAPhonesSubscriptionCannotBeTurnedIntoABrowsers: a paired phone holds
// its endpoint as a native (android) row. A browser claim of that endpoint —
// by its own owner or anyone else — is refused (ErrNativeEndpoint) and the
// row stays the phone's: a session cannot take a phone's subscription out of
// the device's control. ClaimNative admits the configured operator host,
// which a browser claim never does.
func TestAPhonesSubscriptionCannotBeTurnedIntoABrowsers(t *testing.T) {
	s, _, _ := pgStore(t)
	s.WithAllowlist(NewAllowlist([]string{"ntfy.example.org"}))
	id := device(t, s, 2, "phone")
	var ntfy Subscription
	ntfy.Endpoint = "https://ntfy.example.org/upPhone"
	ntfy.Keys.P256dh, ntfy.Keys.Auth = "p256-phone", "auth-phone"
	if err := s.ClaimNative(t.Context(), 2, id, ntfy, "muster app"); err != nil {
		t.Fatalf("ClaimNative on the operator host: %v", err)
	}
	fcm := sub("phone-on-fcm")
	id2 := device(t, s, 2, "phone2")
	if err := s.ClaimNative(t.Context(), 2, id2, fcm, "muster app"); err != nil {
		t.Fatalf("ClaimNative on a browser host: %v", err)
	}
	for _, who := range []int64{2, 3} {
		if _, err := s.Claim(as(who), who, fcm, "a browser"); !errors.Is(err, ErrNativeEndpoint) {
			t.Fatalf("user %d's browser claim of a phone's endpoint: %v; want ErrNativeEndpoint", who, err)
		}
	}
	devs, err := s.ForOwner(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, d := range devs {
		kinds[d.Sub.Endpoint] = d.Kind
	}
	if kinds[fcm.Endpoint] != KindAndroid || kinds[ntfy.Endpoint] != KindAndroid || len(devs) != 2 {
		t.Fatalf("after the refused claims: %v; want both rows still the phones'", kinds)
	}
	// Control: a browser claim of a browser's own endpoint still works.
	if _, err := s.Claim(as(3), 3, sub("a-real-browser"), "a browser"); err != nil {
		t.Fatalf("CONTROL: %v", err)
	}
}
