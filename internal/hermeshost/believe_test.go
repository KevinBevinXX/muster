package hermeshost

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

func TestBelieveMatrix(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	nowSec := float64(now.Unix())
	since := now.Add(-10 * time.Second).UnixMilli()
	live := Snapshot{LeaseExpires: nowSec + 200, NewestMessage: nowSec - 20}
	none := Snapshot{NewestMessage: nowSec - 20}
	expired := Snapshot{LeaseExpires: nowSec - 1, NewestMessage: nowSec - 20}
	newer := Snapshot{NewestMessage: nowSec - 5} // a row written after the idle
	alive := func(int, string) bool { return true }
	dead := func(int, string) bool { return false }
	f := func(status string) StatusFile { return StatusFile{Status: status, Since: since} }
	for _, c := range []struct {
		name   string
		file   StatusFile
		found  bool
		snap   Snapshot
		alive  func(int, string) bool
		status string
		reason string
	}{
		{"no file", StatusFile{}, false, none, alive, "", ReasonNoStatusFile},
		{"busy, lease live", f(mirror.StatusBusy), true, live, alive, mirror.StatusBusy, ""},
		{"busy, no lease", f(mirror.StatusBusy), true, none, alive, "", ReasonNoLease},
		{"busy, lease expired", f(mirror.StatusBusy), true, expired, alive, "", ReasonNoLease},
		{"busy, writer gone", f(mirror.StatusBusy), true, live, dead, "", ReasonWriterGone},
		{"waiting, lease live", f(mirror.StatusWaiting), true, live, alive, mirror.StatusWaiting, ""},
		{"waiting, no lease", f(mirror.StatusWaiting), true, none, alive, "", ReasonNoLease},
		{"waiting, writer gone", f(mirror.StatusWaiting), true, live, dead, "", ReasonWriterGone},
		{"idle, quiet", f(mirror.StatusIdle), true, none, alive, mirror.StatusIdle, ""},
		{"idle, writer gone is still idle", f(mirror.StatusIdle), true, none, dead, mirror.StatusIdle, ""},
		{"idle, lease live", f(mirror.StatusIdle), true, live, alive, "", ReasonLeaseLive},
		{"idle, newer row", f(mirror.StatusIdle), true, newer, alive, "", ReasonNewerMessage},
		// Slack 0: a row at exactly the idle's millisecond is not newer.
		{"idle, row at the same ms", f(mirror.StatusIdle), true, Snapshot{NewestMessage: float64(since) / 1000}, alive, mirror.StatusIdle, ""},
		{"idle, row one ms later", f(mirror.StatusIdle), true, Snapshot{NewestMessage: float64(since+1) / 1000}, alive, "", ReasonNewerMessage},
	} {
		got := Believe(c.file, c.found, c.snap, now, c.alive)
		if got.Status != c.status || got.Reason != c.reason {
			t.Errorf("%s: Believe = %+v, want status %q reason %q", c.name, got, c.status, c.reason)
		}
		if c.status == "" && got.Since != 0 {
			t.Errorf("%s: an unknown status carries a time: %+v", c.name, got)
		}
		if c.status != "" && got.Since != since {
			t.Errorf("%s: Since = %d, want the file's %d", c.name, got.Since, since)
		}
	}
}

// A believed waiting status says what it waits on (Phase 4: which ask muster
// shows); no other status carries a kind, and an unbelieved one carries none.
func TestBelieveCarriesTheWaitingKind(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	since := now.Add(-time.Second).UnixMilli()
	live := Snapshot{LeaseExpires: float64(now.Unix()) + 200}
	alive := func(int, string) bool { return true }
	for _, kind := range []string{"approval", "clarify", "sudo"} {
		got := Believe(StatusFile{Status: mirror.StatusWaiting, Since: since, Kind: kind}, true, live, now, alive)
		if got.Status != mirror.StatusWaiting || got.Kind != kind {
			t.Errorf("waiting on %s: %+v", kind, got)
		}
		if got := Believe(StatusFile{Status: mirror.StatusWaiting, Since: since, Kind: kind}, true, Snapshot{}, now, alive); got.Kind != "" {
			t.Errorf("an unbelieved waiting status carries kind %q", got.Kind)
		}
	}
	if got := Believe(StatusFile{Status: mirror.StatusBusy, Since: since, Kind: "approval"}, true, live, now, alive); got.Kind != "" {
		t.Errorf("a busy status carries kind %q", got.Kind)
	}
}
