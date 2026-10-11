package provision_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// TestNoopSatisfiesTheContract runs the shared suite. It is the whole reason
// the noop driver is worth having: it proves the contract is satisfiable by
// something that is not a cluster.
func TestNoopSatisfiesTheContract(t *testing.T) {
	provisiontest.RunContract(t, provisiontest.Harness{
		Name: "noop",
		New: func(t *testing.T) provision.Provisioner {
			return provision.MustNewNoop()
		},
		Blind: func(t *testing.T) provision.Provisioner {
			return provision.MustNewNoop(provision.NoopBlind())
		},
		Restricted: func(t *testing.T) provision.Provisioner {
			// Every CheckSpec-gated capability off. The noop is the one driver
			// that can genuinely be configured this way, which makes it the
			// place the refusals are exercised most thoroughly.
			return provision.MustNewNoop(provision.NoopCapabilities(provision.Capabilities{}))
		},
	})
}

// TestNoopDestroyIsNotTheOldBareNil pins defect 3 of the driver this replaces.
//
// MUTATION: replace Noop.Destroy's body with `return nil`. This test goes red
// on the Get assertion ("Destroy returned nil but Get still finds it"), the
// contract's DestroyRemovesTheInstance goes red the same way, and NOTHING ELSE
// in the suite moves — which is the point: that mutation is invisible to a
// driver's own happy-path tests.
func TestNoopDestroyIsNotTheOldBareNil(t *testing.T) {
	n := provision.MustNewNoop()
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("gone")
	if err := n.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := n.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := n.Get(ctx, spec.Ref); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Destroy returned nil but Get still finds it: %v", err)
	}
}

// TestNoopBlindListNeverReportsEmptiness is the rule stated once more against
// the driver directly, because it is the one a caller's correctness depends on
// most and it deserves a test that names it.
func TestNoopBlindListNeverReportsEmptiness(t *testing.T) {
	blind := provision.MustNewNoop(provision.NoopBlind())
	list, err := blind.List(context.Background())
	if err == nil {
		t.Fatal("blind List returned nil error")
	}
	if !errors.Is(err, provision.ErrBlind) {
		t.Fatalf("want ErrBlind, got %v", err)
	}
	if list != nil {
		t.Fatalf("blind List returned %d instances alongside the error", len(list))
	}
}

// TestNoopRecordsRatherThanDiscards proves the "noop" is not inert: the
// lifecycle is observable through the interface afterwards.
func TestNoopRecordsRatherThanDiscards(t *testing.T) {
	fixed := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	n := provision.MustNewNoop(provision.NoopClock(func() time.Time { return fixed }))
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("recorder")
	if err := n.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := n.Scale(ctx, spec.Ref, 0); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	logs, err := n.TailLogs(ctx, spec.Ref, 0)
	if err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	// The whole normalised line, not a keyword: a guard on a WORD is walkable
	// by rewording.
	wantCreate := "2001-02-03T04:05:06Z created recorder replicas=1 image=\"ghcr.io/muster-example/agent:1\""
	wantScale := "2001-02-03T04:05:06Z scaled recorder to 0"
	for _, want := range []string{wantCreate, wantScale} {
		if !strings.Contains(logs, want) {
			t.Errorf("recorded log missing line\n  want: %s\n  got:\n%s", want, logs)
		}
	}

	var streamed []string
	if err := n.StreamLogs(ctx, spec.Ref, func(l string) { streamed = append(streamed, l) }); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if len(streamed) != 2 {
		t.Fatalf("StreamLogs emitted %d lines, want 2: %v", len(streamed), streamed)
	}
}

// TestNoopTailLogsHonoursTheLineLimit picks a limit that is neither the total
// nor a divisor of it, so an implementation that ignored it, or that returned
// the FIRST n lines instead of the last, both fail.
func TestNoopTailLogsHonoursTheLineLimit(t *testing.T) {
	n := provision.MustNewNoop()
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("tailer")
	if err := n.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := n.Scale(ctx, spec.Ref, i%2); err != nil {
			t.Fatalf("Scale: %v", err)
		}
	}
	// 5 lines recorded (1 create + 4 scales); ask for 3.
	out, err := n.TailLogs(ctx, spec.Ref, 3)
	if err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("TailLogs(3) returned %d lines: %q", len(lines), out)
	}
	if strings.Contains(out, "created") {
		t.Fatalf("TailLogs(3) returned the FIRST 3 lines, not the last 3: %q", out)
	}
}

// TestNoopCapabilitiesAreDeclaredHonestly pins the defaults, because they are
// what the contract suite's capability probes measure against and a silent
// widening would make those probes vacuous.
func TestNoopCapabilitiesAreDeclaredHonestly(t *testing.T) {
	caps := provision.MustNewNoop().Capabilities()
	if caps.Exec {
		t.Error("the noop driver must not claim Exec: there is no process to enter")
	}
	if caps.Isolation != provision.IsolationNone {
		t.Errorf("the noop driver isolates nothing; Isolation is %v, want %v", caps.Isolation, provision.IsolationNone)
	}
}
