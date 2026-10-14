package provisiontest

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// TestTheContractSuiteMatchesWhatTheDocsClaimAboutIt pins the two numbers the
// package docs and docs/provisioner.md state ABOUT this suite.
//
// 🔴 A COUNT IN PROSE IS UNFALSIFIABLE UNLESS SOMETHING READS IT. The provision
// package doc and docs/provisioner.md both say the four contract rules are pinned here
// "with one named exception" — rule 2's ErrNotManaged carve-out — and both cite
// "20 cases, none of which mentions ErrNotManaged". Those sentences were written
// after counting by hand, and a hand count is a claim about the moment it was
// made: adding a case, or pinning the carve-out here after all, silently turns
// both documents into confident false statements about their own test suite.
//
// This is what makes the numbers a measurement. If it fails, do not adjust the
// number here alone — the sentences in internal/provision/doc.go and docs/provisioner.md
// are part of the same claim and have to move with it.
func TestTheContractSuiteMatchesWhatTheDocsClaimAboutIt(t *testing.T) {
	// The case list lives inside RunContract, so count it from the source
	// rather than from a second copy of the list that could agree with a wrong
	// docstring.
	src, err := os.ReadFile("contract.go")
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}
	text := string(src)

	// 🔴 POSITIVE CONTROL. A counter that matches nothing reports zero, which
	// is indistinguishable from a suite with no cases — and every assertion
	// below would then be about the parser rather than the suite.
	const marker = "\", test"
	n := strings.Count(text, marker)
	t.Logf("contract.go declares %d case(s) (counted on %q)", n, marker)
	if n == 0 {
		t.Fatalf("the case counter matched NOTHING in contract.go, so its number says nothing about "+
			"the suite: the %q shape must have changed", marker)
	}

	const wantCases = 20
	if n != wantCases {
		t.Errorf("RunContract declares %d cases; internal/provision/doc.go and docs/provisioner.md both say %d. "+
			"Update the code and BOTH documents together, or the docs become a false statement about "+
			"their own suite.", n, wantCases)
	}

	// The carve-out claim: this suite deliberately does not pin ErrNotManaged,
	// because a foreign co-named object needs a SHARED backend and Noop's
	// backend is its own map. If that changes, the "one named exception"
	// sentences are wrong in the other direction — they would understate the
	// coverage, which is the safer error but still an error.
	if strings.Contains(text, "ErrNotManaged") {
		t.Errorf("contract.go now mentions ErrNotManaged. That may be an improvement, but " +
			"internal/provision/doc.go, provision.go's ErrNotManaged doc and docs/provisioner.md all state that " +
			"this suite does NOT pin it and that it is pinned in the kubernetes driver instead. Fix " +
			"the three documents in the same change.")
	}
}

// execOnly declares Exec and implements only the non-terminal entry point.
type execOnly struct{ *provision.Noop }

func (execOnly) Capabilities() provision.Capabilities { return provision.Capabilities{Exec: true} }
func (execOnly) Exec(context.Context, provision.Ref, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

// execAndTTY implements both.
type execAndTTY struct{ execOnly }

func (execAndTTY) ExecTTY(context.Context, provision.Ref, []string, io.Reader, io.Writer, <-chan provision.TermSize) error {
	return nil
}

// TestTheExecDeclarationCheckFiresOnADriverWithoutATerminal: the contract's
// "declares Exec ⇒ implements both" case, shown red on a driver that breaks it
// and green on one that does not. No real driver breaks it, which is exactly
// why it needs a fixture that does.
func TestTheExecDeclarationCheckFiresOnADriverWithoutATerminal(t *testing.T) {
	if got := execDeclarationProblem(execOnly{provision.MustNewNoop()}); !strings.Contains(got, "TTYExecer") {
		t.Fatalf("a driver declaring Exec without ExecTTY must be named; got %q", got)
	}
	if got := execDeclarationProblem(execAndTTY{execOnly{provision.MustNewNoop()}}); got != "" {
		t.Fatalf("control: a driver implementing both must pass; got %q", got)
	}
	if got := execDeclarationProblem(provision.MustNewNoop()); !strings.Contains(got, "provision.Execer") {
		t.Fatalf("a driver implementing neither must be named for Execer first; got %q", got)
	}
}
