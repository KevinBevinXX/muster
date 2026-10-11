package provision_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// allCaps is every capability on. Used as the base so that a case which turns
// exactly ONE off isolates that capability's branch — a fixture with several
// off would let an earlier refusal win and the branch under test would never
// execute, which is a mutant dying for the wrong reason.
var allCaps = provision.Capabilities{
	Isolation:      provision.IsolationNamespace,
	Secrets:        true,
	Files:          true,
	Persistence:    true,
	ResourceLimits: true,
	Scale:          true,
	Exec:           true,

	NetworkIsolation: true,
}

// TestCheckSpecRefusesExactlyTheCapabilityItLacks is the isolation-sensitive
// form of the capability suite: one capability off at a time, against a spec
// that asks for that one thing and nothing else gated.
//
// MUTATION MATRIX (each mutation applied alone to CheckSpec):
//
//	delete the !caps.Files branch          -> "Files" subtest RED
//	delete the !caps.Secrets branch        -> "Secrets" and "SecretFile" RED
//	delete the !caps.Persistence branch    -> "Persistence" RED
//	delete the !caps.ResourceLimits branch -> "ResourceLimits" RED
//	delete the !caps.Scale branch          -> "Scale" RED
//	delete the !caps.NetworkIsolation branch -> "NetworkIsolation" RED
//
// Each subtest also asserts the message NAMES the capability, so a mutation
// that returns the WRONG branch's error is caught rather than counted as a
// kill.
func TestCheckSpecRefusesExactlyTheCapabilityItLacks(t *testing.T) {
	cases := []struct {
		name    string
		off     func(*provision.Capabilities)
		ask     func(*provision.Spec)
		mustSay string
	}{
		{
			name: "Files",
			off:  func(c *provision.Capabilities) { c.Files = false },
			ask: func(s *provision.Spec) {
				s.Files = []provision.File{{Path: "/etc/muster/one.json", Content: []byte("{}")}}
			},
			mustSay: "cannot place files",
		},
		{
			name: "Secrets",
			off:  func(c *provision.Capabilities) { c.Secrets = false },
			ask: func(s *provision.Spec) {
				s.Secrets = []provision.EnvVar{{Name: "MUSTER_PROBE_TOKEN", Value: "probe-value"}}
			},
			mustSay: "no confidential store",
		},
		{
			name: "SecretFile",
			off:  func(c *provision.Capabilities) { c.Secrets = false },
			ask: func(s *provision.Spec) {
				s.Files = []provision.File{{Path: "/etc/muster/creds", Content: []byte("x"), Secret: true}}
			},
			mustSay: "marked Secret",
		},
		{
			name: "Persistence",
			off:  func(c *provision.Capabilities) { c.Persistence = false },
			ask: func(s *provision.Spec) {
				s.Workspace = provision.Workspace{Path: "/data/workspace", Size: "7Gi", Persist: true}
			},
			mustSay: "persistent workspace",
		},
		{
			name: "ResourceLimits",
			off:  func(c *provision.Capabilities) { c.ResourceLimits = false },
			ask: func(s *provision.Spec) {
				s.Resources = provision.Resources{MemoryLimit: "1536Mi"}
			},
			mustSay: "resource limits",
		},
		{
			name: "Scale",
			off:  func(c *provision.Capabilities) { c.Scale = false },
			// 🔴 THREE, NOT TWO. Two is a power-of-two multiple of the step and
			// sits on the boundary of several plausible off-by-one mutants; a
			// fixture that lands exactly on a guard's boundary lets the guard
			// pass without executing.
			ask:     func(s *provision.Spec) { s.Replicas = 3 },
			mustSay: "exactly one instance",
		},
		{
			name: "NetworkIsolation",
			off:  func(c *provision.Capabilities) { c.NetworkIsolation = false },
			ask: func(s *provision.Spec) {
				s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}}
			},
			mustSay: "cannot isolate an instance's network",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := allCaps
			c.off(&caps)
			spec := provisiontest.MinimalSpec("probe")
			c.ask(&spec)

			// Positive control FIRST: with every capability on, the very same
			// spec must be accepted. Without this, a subtest could pass because
			// the spec is malformed rather than because the branch fired.
			if err := provision.CheckSpec(allCaps, spec); err != nil {
				t.Fatalf("positive control: the spec must be ACCEPTED when every capability is on, got %v", err)
			}

			err := provision.CheckSpec(caps, spec)
			if !errors.Is(err, provision.ErrUnsupported) {
				t.Fatalf("want ErrUnsupported with %s off, got %v", c.name, err)
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Fatalf("the refusal must name what it refused: want a message containing %q, got %q", c.mustSay, err)
			}
		})
	}
}

// TestCheckSpecAcceptsWhatTheDriverCanDo is the other direction. A CheckSpec
// that refused everything would pass every case above.
func TestCheckSpecAcceptsWhatTheDriverCanDo(t *testing.T) {
	spec := provisiontest.MinimalSpec("rich")
	spec.Files = []provision.File{{Path: "/etc/muster/a.json", Content: []byte("{}")}}
	spec.Secrets = []provision.EnvVar{{Name: "T", Value: "v"}}
	spec.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
	spec.Resources = provision.Resources{MemoryLimit: "2Gi"}
	spec.Replicas = 3
	spec.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}}
	if err := provision.CheckSpec(allCaps, spec); err != nil {
		t.Fatalf("a fully-capable driver refused a spec it can satisfy: %v", err)
	}
	// A driver with NO capability at all still accepts a spec that declares no
	// isolation: the refusal is for the ask, not for the driver.
	if err := provision.CheckSpec(provision.Capabilities{}, provisiontest.MinimalSpec("plain")); err != nil {
		t.Fatalf("a spec asking for nothing gated was refused: %v", err)
	}
}

// TestCheckSpecRejectsAnInvalidSpecRegardlessOfCapabilities pins the ordering:
// validity is checked before capability, so "your spec is malformed" is never
// reported as "your driver cannot do that".
func TestCheckSpecRejectsAnInvalidSpecRegardlessOfCapabilities(t *testing.T) {
	spec := provisiontest.MinimalSpec("broken")
	spec.Runtime = provision.Runtime{} // neither Image nor Command
	err := provision.CheckSpec(allCaps, spec)
	if !errors.Is(err, provision.ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
	if errors.Is(err, provision.ErrUnsupported) {
		t.Fatal("a malformed spec must not be reported as a capability problem; a caller would go and " +
			"reconfigure a driver that was never at fault")
	}
}

func TestCheckScale(t *testing.T) {
	if err := provision.CheckScale(provision.Capabilities{}, 0); err != nil {
		t.Errorf("scaling to zero is universal, got %v", err)
	}
	if err := provision.CheckScale(provision.Capabilities{}, 1); err != nil {
		t.Errorf("scaling to one is universal, got %v", err)
	}
	if err := provision.CheckScale(provision.Capabilities{}, 3); !errors.Is(err, provision.ErrUnsupported) {
		t.Errorf("want ErrUnsupported scaling a non-scaling driver to 3, got %v", err)
	}
	if err := provision.CheckScale(provision.Capabilities{Scale: true}, 3); err != nil {
		t.Errorf("a scaling driver must accept 3, got %v", err)
	}
	if err := provision.CheckScale(allCaps, -1); !errors.Is(err, provision.ErrInvalidSpec) {
		t.Errorf("want ErrInvalidSpec for a negative replica count, got %v", err)
	}
}

func TestIsolationString(t *testing.T) {
	// The strings are user-visible: an interface has to warn on IsolationNone
	// by name.
	for level, want := range map[provision.Isolation]string{
		provision.IsolationNone:      "none",
		provision.IsolationProcess:   "process",
		provision.IsolationContainer: "container",
		provision.IsolationNamespace: "namespace",
	} {
		if got := level.String(); got != want {
			t.Errorf("Isolation(%d).String() = %q, want %q", int(level), got, want)
		}
	}
	if provision.IsolationNone >= provision.IsolationContainer {
		t.Error("Isolation must be ordered so a caller can compare levels")
	}
}
