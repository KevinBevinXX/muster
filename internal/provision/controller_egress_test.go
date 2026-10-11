package provision

import (
	"errors"
	"strings"
	"testing"
)

// TestControllerEgressMovesTheFingerprintOnlyWhenDeclared: an isolated spec
// without controller egress keeps its pinned fingerprint (see
// TestAnIsolatedSpecsFingerprintIsPinned); declaring a port moves it, and
// different ports differ, so an agent that gains the connector allowance
// reconciles on its next Update.
func TestControllerEgressMovesTheFingerprintOnlyWhenDeclared(t *testing.T) {
	with := func(n Network) string {
		s := fingerprintFixture()
		s.Network = n
		return Fingerprint(s)
	}
	base := with(Network{Isolate: true, PublicEgressTCPPorts: []int{443}})
	if base != pinnedIsolatedFingerprint {
		t.Fatalf("an isolated spec with no controller egress changed fingerprint: %s", base)
	}
	a := with(Network{Isolate: true, PublicEgressTCPPorts: []int{443}, ControllerEgressTCPPorts: []int{18191}})
	b := with(Network{Isolate: true, PublicEgressTCPPorts: []int{443}, ControllerEgressTCPPorts: []int{18192}})
	if a == base || b == base || a == b {
		t.Fatalf("controller egress does not move the fingerprint distinctly: base=%s a=%s b=%s", base, a, b)
	}
	// It is not interchangeable with a public port of the same number.
	if with(Network{Isolate: true, PublicEgressTCPPorts: []int{443, 18191}}) == a {
		t.Fatal("a controller port and a public port of the same number fingerprint the same")
	}
}

func TestValidateRefusesAControllerEgressThatConfinesNothing(t *testing.T) {
	ok := fingerprintFixture()
	ok.Network = Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, c := range map[string]struct {
		net  Network
		want string
	}{
		"without isolate": {Network{ControllerEgressTCPPorts: []int{18191}}, "controller egress ports [18191] without Isolate"},
		"port zero":       {Network{Isolate: true, ControllerEgressTCPPorts: []int{0}}, "controller egress port 0 out of range"},
		"port twice":      {Network{Isolate: true, ControllerEgressTCPPorts: []int{18191, 18191}}, "controller egress port 18191 appears twice"},
	} {
		s := fingerprintFixture()
		s.Network = c.net
		err := s.Validate()
		if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec containing %q", name, err, c.want)
		}
	}
}
