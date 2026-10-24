package provision

import (
	"errors"
	"strings"
	"testing"
)

// fingerprintFixture declares no Security and no Network, like every
// gateway-kind spec.
func fingerprintFixture() Spec {
	return Spec{
		Ref:       Ref{Name: "pinned-wren", ID: 31},
		Runtime:   Runtime{Image: "ghcr.io/example-org/runtime:9.1", WorkingDir: "/srv/w"},
		Env:       []EnvVar{{Name: "A", Value: "1"}},
		Secrets:   []EnvVar{{Name: "S", Value: "fixture-secret"}},
		Ports:     []Port{{Name: DefaultPortName, Port: 18789}},
		Health:    Health{HTTPGetPath: "/", PortName: DefaultPortName},
		Workspace: Workspace{Path: "/srv/w", Size: "4Gi", Persist: true},
	}
}

// pinnedFingerprint is Fingerprint(fingerprintFixture()) MEASURED AT 34118de,
// before provision.Security existed — and before provision.Network did. Adding
// either type must not move it: a moved fingerprint is a roll of every live
// instance on its next Update for a pod template that did not change.
const pinnedFingerprint = "674044b3f0e73f1d5485be9ff15f29e0b16648a0be495411de49dea6d435d8d4"

func TestASpecDeclaringNoSecurityKeepsItsPreSecurityFingerprint(t *testing.T) {
	if got := Fingerprint(fingerprintFixture()); got != pinnedFingerprint {
		t.Fatalf("Fingerprint = %s, want the pre-Security value %s", got, pinnedFingerprint)
	}
}

func TestDeclaringSecurityMovesTheFingerprint(t *testing.T) {
	base := fingerprintFixture()
	for name, sec := range map[string]Security{
		"user":       {RunAsUser: 1000},
		"group":      {RunAsGroup: 1000},
		"fsgroup":    {FSGroup: 1000},
		"nonroot":    {RunAsNonRoot: true},
		"restricted": {Restricted: true},
		"no sa":      {NoServiceAccountToken: true},
	} {
		s := fingerprintFixture()
		s.Security = sec
		if Fingerprint(s) == Fingerprint(base) {
			t.Errorf("%s: declaring %+v did not move the fingerprint; the pod template changed and nothing would roll", name, sec)
		}
	}
}

// TestDeclaringNetworkIsolationMovesTheFingerprint: a spec that GAINS a network
// declaration must stop comparing equal to the one without it — that inequality
// is the only thing that makes an instance created before its kind was confined
// reconcile on its next Update. Each field moves it on its own; the order of the
// ports does not.
func TestDeclaringNetworkIsolationMovesTheFingerprint(t *testing.T) {
	with := func(n Network) string {
		s := fingerprintFixture()
		s.Network = n
		return Fingerprint(s)
	}
	base := Fingerprint(fingerprintFixture())
	isolate := with(Network{Isolate: true})
	one := with(Network{Isolate: true, PublicEgressTCPPorts: []int{443}})
	other := with(Network{Isolate: true, PublicEgressTCPPorts: []int{8443}})
	two := with(Network{Isolate: true, PublicEgressTCPPorts: []int{443, 8443}})
	seen := map[string]string{}
	for name, fp := range map[string]string{"none": base, "isolate": isolate, "443": one, "8443": other, "443+8443": two} {
		if prev, dup := seen[fp]; dup {
			t.Errorf("%q and %q have the same fingerprint; two different confinements would compare equal", name, prev)
		}
		seen[fp] = name
	}
	if two != with(Network{Isolate: true, PublicEgressTCPPorts: []int{8443, 443}}) {
		t.Error("reordering the egress ports moved the fingerprint; the order changes nothing about what is allowed")
	}
}

// pinnedIsolatedFingerprint is Fingerprint(fingerprintFixture() + the claude-code
// kind's network declaration), MEASURED AT THE COMMIT THAT ADDED provision.Network.
//
// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE: it is green at the commit that
// introduced it by construction. What it is for is the NEXT change to how the
// declaration is encoded — which would move every isolated instance's fingerprint
// and roll every such agent's pod on its next Update, and which the "moves the
// fingerprint" test above cannot see, because it only compares fingerprints to
// each other.
const pinnedIsolatedFingerprint = "cb2482f7b48eee424a02e0defb5095686d712be2b5ba4283cd423d132f27ef44"

func TestAnIsolatedSpecsFingerprintIsPinned(t *testing.T) {
	s := fingerprintFixture()
	s.Network = Network{Isolate: true, PublicEgressTCPPorts: []int{443}}
	if got := Fingerprint(s); got != pinnedIsolatedFingerprint {
		t.Fatalf("Fingerprint = %s, want %s: the encoding of a network declaration changed, which rolls "+
			"every isolated instance on its next Update", got, pinnedIsolatedFingerprint)
	}
}

// TestValidateRefusesANetworkDeclarationThatConfinesNothing: an egress port list
// without Isolate reads as confinement and restricts nothing; a port out of range
// or listed twice is a typo. Each is refused by its own words.
func TestValidateRefusesANetworkDeclarationThatConfinesNothing(t *testing.T) {
	ok := fingerprintFixture()
	ok.Network = Network{Isolate: true, PublicEgressTCPPorts: []int{443, 8443}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("control: a well-formed isolated spec is refused: %v", err)
	}
	for name, c := range map[string]struct {
		net  Network
		want string
	}{
		"ports without isolate": {Network{PublicEgressTCPPorts: []int{443}}, "without Isolate"},
		"port zero":             {Network{Isolate: true, PublicEgressTCPPorts: []int{0}}, "port 0 out of range"},
		"port too large":        {Network{Isolate: true, PublicEgressTCPPorts: []int{65536}}, "port 65536 out of range"},
		"port twice":            {Network{Isolate: true, PublicEgressTCPPorts: []int{443, 8443, 443}}, "port 443 appears twice"},
	} {
		s := fingerprintFixture()
		s.Network = c.net
		err := s.Validate()
		if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec containing %q", name, err, c.want)
		}
	}
}
