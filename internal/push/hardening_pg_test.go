package push

import (
	"errors"
	"testing"
)

// TestClaimAdmitsAnOperatorHostOnlyWhenConfigured: the store's Claim is where
// the allowlist is enforced, so an ntfy endpoint is refused by the store with
// no operator hosts and stored by one configured with that exact host.
func TestClaimAdmitsAnOperatorHostOnlyWhenConfigured(t *testing.T) {
	s, _, _ := pgStore(t)
	var ntfy Subscription
	ntfy.Endpoint = "https://ntfy.example.org/upAbC123?up=1"
	ntfy.Keys.P256dh, ntfy.Keys.Auth = "p256-ntfy", "auth-ntfy"
	if _, err := s.Claim(as(2), 2, ntfy, "phone"); !errors.Is(err, ErrPushServiceNotAllowed) {
		t.Fatalf("no operator hosts: Claim = %v; want ErrPushServiceNotAllowed", err)
	}
	s.WithAllowlist(NewAllowlist([]string{"other.example.org"}))
	if _, err := s.Claim(as(2), 2, ntfy, "phone"); !errors.Is(err, ErrPushServiceNotAllowed) {
		t.Fatalf("another operator host: Claim = %v; want ErrPushServiceNotAllowed", err)
	}
	s.WithAllowlist(NewAllowlist([]string{"ntfy.example.org"}))
	if _, err := s.Claim(as(2), 2, ntfy, "phone"); err != nil {
		t.Fatalf("the configured host: Claim = %v", err)
	}
	devs, err := s.Devices(as(2))
	if err != nil || len(devs) != 1 || devs[0].Sub.Endpoint != ntfy.Endpoint {
		t.Fatalf("devices = %v, %v", devs, err)
	}
}
