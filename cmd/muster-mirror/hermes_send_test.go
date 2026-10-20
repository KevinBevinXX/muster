package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// TestHermesSendIsOffUnlessTheFlagOrItsVariableSaysSo: -hermes-send /
// MUSTER_MIRROR_HERMES_SEND reaches the tmux backend's Hermes runtime (which
// the hello advertises from), OFF by default; the flag wins over the variable.
// Observed on the backend run actually builds, after its wrappers.
func TestHermesSendIsOffUnlessTheFlagOrItsVariableSaysSo(t *testing.T) {
	prevB, prevW := newTmuxBackend, e2eWrap
	t.Cleanup(func() { newTmuxBackend, e2eWrap = prevB, prevW })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend {
		// A tmux that cannot run: -list then fails, after the backend is built.
		return mirrorhost.Tmux{Bin: filepath.Join(t.TempDir(), "no-tmux")}
	}
	var got []bool
	e2eWrap = func(b mirrorhost.Backend) mirrorhost.Backend {
		u, ok := b.(interface{ Unwrap() mirrorhost.Backend })
		if !ok {
			t.Fatalf("instrument check: the backend %T wraps nothing", b)
		}
		hs, ok := u.Unwrap().(mirrorhost.HermesSendBackend)
		got = append(got, ok && hs.HermesSendEnabled())
		return b
	}
	for _, c := range []struct {
		env  map[string]string
		args []string
	}{
		{nil, []string{"-list"}},
		{map[string]string{envHermesSend: "1"}, []string{"-list"}},
		{nil, []string{"-list", "-hermes-send"}},
		{map[string]string{envHermesSend: "1"}, []string{"-list", "-hermes-send=false"}},
	} {
		runWith(t, c.env, c.args...)
	}
	if want := []bool{false, true, true, false}; !slices.Equal(got, want) {
		t.Fatalf("the backend's hermesSend across default, variable, flag, flag-over-variable = %v, want %v", got, want)
	}
}
