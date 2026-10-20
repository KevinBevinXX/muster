package push

import (
	"errors"
	"testing"
)

// TestABrowserClaimNeverAdmitsAnOperatorHost: a BROWSER subscription
// (/api/push/subscribe → Claim) may name only a browser push service. An
// operator host is refused whether or not it is configured — it is for the
// paired phones alone — and a browser host is stored either way (the control).
func TestABrowserClaimNeverAdmitsAnOperatorHost(t *testing.T) {
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
	if _, err := s.Claim(as(2), 2, ntfy, "phone"); !errors.Is(err, ErrPushServiceNotAllowed) {
		t.Fatalf("the configured operator host through a browser claim: %v; want ErrPushServiceNotAllowed", err)
	}
	if _, err := s.Claim(as(2), 2, sub("browser-ok"), "browser"); err != nil {
		t.Fatalf("CONTROL: a browser host: %v", err)
	}
	devs, err := s.Devices(as(2))
	if err != nil || len(devs) != 1 || devs[0].Sub.Endpoint != sub("browser-ok").Endpoint {
		t.Fatalf("devices = %v, %v", devs, err)
	}
}
