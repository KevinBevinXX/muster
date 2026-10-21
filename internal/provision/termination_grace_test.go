package provision

import (
	"errors"
	"testing"
)

// A spec declaring no termination grace keeps its pre-field fingerprint (the
// pinned value), and one declaring a grace moves it: the pod template changed.
func TestTerminationGraceCountsOnlyWhenDeclared(t *testing.T) {
	if got := Fingerprint(fingerprintFixture()); got != pinnedFingerprint {
		t.Fatalf("a spec with no grace moved its fingerprint: %s", got)
	}
	s := fingerprintFixture()
	s.TerminationGraceSeconds = 60
	t60 := Fingerprint(s)
	s.TerminationGraceSeconds = 45
	if t60 == pinnedFingerprint || Fingerprint(s) == t60 {
		t.Fatal("declaring (or changing) a termination grace did not move the fingerprint; nothing would roll")
	}
}

func TestTerminationGraceIsBounded(t *testing.T) {
	for _, g := range []int{-1, 3601} {
		s := fingerprintFixture()
		s.TerminationGraceSeconds = g
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("grace %d: %v", g, err)
		}
	}
	s := fingerprintFixture()
	s.TerminationGraceSeconds = 60
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
