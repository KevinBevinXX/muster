package hermeshost

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

const tty = "/dev/pts/7"

// locFixture is a home with one CLI session on tty, a locator over it and the
// CLI's start time.
type locFixture struct {
	h       *hermestest.Home
	loc     *Locator
	dir     StatusDir
	root    string
	started time.Time
}

func newLoc(t *testing.T) *locFixture {
	t.Helper()
	h := hermestest.New(t)
	f := &locFixture{h: h, dir: StatusDir(t.TempDir()), root: hermestest.LocalID(), started: time.Now().Add(-time.Minute)}
	h.Session(f.root, hermestest.SessionOpts{})
	h.Breadcrumb(tty, f.root, hermestest.Now())
	st := NewHome(h.Dir).OpenState()
	t.Cleanup(func() { _ = st.Close() })
	f.loc = &Locator{State: st, Status: f.dir, Alive: func(int, string) bool { return true }}
	return f
}

func (f *locFixture) status(t *testing.T, id, status string, since time.Time) {
	t.Helper()
	sf := StatusFile{SessionID: id, Status: status, Since: since.UnixMilli(), WriterPID: 1, WriterStart: "1"}
	if status == mirror.StatusWaiting {
		sf.Kind = "approval"
	}
	if err := f.dir.Write(sf); err != nil {
		t.Fatal(err)
	}
}

func TestLocateResolvesTheBreadcrumbsSession(t *testing.T) {
	f := newLoc(t)
	f.status(t, f.root, mirror.StatusIdle, time.Now())
	ps := f.loc.Locate(ctx(t), tty, f.started)
	if ps.Session != f.root || ps.NoView != "" {
		t.Fatalf("Locate = %+v, want session %s", ps, f.root)
	}
	if ps.Status.Status != mirror.StatusIdle || ps.Status.Since == 0 {
		t.Fatalf("status = %+v, want a believed idle", ps.Status)
	}
}

// 🔴 THE END-REASON STALENESS RULE: a breadcrumb whose session ended other
// than by compression is stale — no rendered view AND an unknown status, even
// though the abandoned session's own status file reads idle.
func TestASwitchedSessionHasNoViewAndUnknownStatus(t *testing.T) {
	for _, reason := range []string{"resumed_other", "branched", "new_session", "user_exit"} {
		f := newLoc(t)
		f.status(t, f.root, mirror.StatusIdle, time.Now())
		f.h.End(f.root, reason)
		ps := f.loc.Locate(ctx(t), tty, f.started)
		if ps.Session != "" || ps.NoView != NoViewSwitched {
			t.Errorf("%s: Locate = %+v, want no view (%q)", reason, ps, NoViewSwitched)
		}
		if ps.Status.Status != "" || ps.Status.Since != 0 {
			t.Errorf("%s: status = %+v, want UNKNOWN — the abandoned tip's idle is not this pane's", reason, ps.Status)
		}
	}
}

// …while one that ended BY COMPRESSION continues in its child: the view
// follows it, and the status is the child's file's (the physical id).
func TestARotatedSessionIsFollowedToItsTip(t *testing.T) {
	f := newLoc(t)
	child := hermestest.ChildID()
	f.h.End(f.root, "compression")
	f.h.Session(child, hermestest.SessionOpts{Parent: f.root})
	f.status(t, f.root, mirror.StatusBusy, time.Now().Add(-time.Minute))
	f.status(t, child, mirror.StatusIdle, time.Now())
	ps := f.loc.Locate(ctx(t), tty, f.started)
	if ps.Session != f.root {
		t.Fatalf("Session = %q, want the lineage root (the view's Gen survives a rotation)", ps.Session)
	}
	if len(ps.Lineage) != 2 || ps.Lineage.Tip().ID != child {
		t.Fatalf("lineage = %v", ps.Lineage.IDs())
	}
	if ps.Status.Status != mirror.StatusIdle {
		t.Fatalf("status = %+v, want the child's newer idle", ps.Status)
	}
}

// A tip that ended BY COMPRESSION with no continuation row yet is a rotation
// in flight, not a switch: it is shown.
func TestATipEndedByCompressionWithNoChildYetIsShown(t *testing.T) {
	f := newLoc(t)
	f.h.End(f.root, "compression")
	ps := f.loc.Locate(ctx(t), tty, f.started)
	if ps.Session != f.root {
		t.Fatalf("Locate = %+v", ps)
	}
}

func TestABreadcrumbOlderThanTheCLIIsNotBelieved(t *testing.T) {
	f := newLoc(t)
	f.status(t, f.root, mirror.StatusIdle, time.Now())
	ps := f.loc.Locate(ctx(t), tty, time.Now().Add(time.Minute))
	if ps.Session != "" || ps.NoView != NoViewOldBreadcrumb || ps.Status.Status != "" {
		t.Fatalf("Locate = %+v, want no view and unknown", ps)
	}
	// An unknown start time is no proof either.
	if ps := f.loc.Locate(ctx(t), tty, time.Time{}); ps.Session != "" {
		t.Fatalf("zero start: %+v", ps)
	}
}

func TestNoBreadcrumbNoSessionArchived(t *testing.T) {
	f := newLoc(t)
	if ps := f.loc.Locate(ctx(t), "/dev/pts/99", f.started); ps.Session != "" || ps.NoView != NoViewNoBreadcrumb {
		t.Errorf("no breadcrumb: %+v", ps)
	}
	f.h.Breadcrumb(tty, hermestest.LocalID(), hermestest.Now())
	if ps := f.loc.Locate(ctx(t), tty, f.started); ps.Session != "" || ps.NoView != NoViewNoSession {
		t.Errorf("unknown session: %+v", ps)
	}
	arch := hermestest.LocalID()
	f.h.Session(arch, hermestest.SessionOpts{Archived: true})
	f.h.Breadcrumb(tty, arch, hermestest.Now())
	if ps := f.loc.Locate(ctx(t), tty, f.started); ps.Session != "" || ps.NoView != NoViewArchived {
		t.Errorf("archived: %+v", ps)
	}
}

func TestABreadcrumbNamingAnInvalidIDIsRefused(t *testing.T) {
	f := newLoc(t)
	f.h.Breadcrumb(tty, "../../auth", hermestest.Now())
	ps := f.loc.Locate(ctx(t), tty, f.started)
	if ps.Session != "" || !strings.Contains(ps.NoView, "could not be read") {
		t.Fatalf("Locate = %+v", ps)
	}
}
