package mirror_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

func pendingWindow() mirror.Window {
	return mirror.Window{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: "sess-p",
		Status: mirror.StatusWaiting, StatusSince: 42, Pending: &mirror.Pending{Summary: "Bash: make deploy", Last: "Ready."}}
}

func pendingHostView(r *rig, host string) (mirror.HostView, bool) {
	for _, h := range r.svc.Hub().Hosts(1) {
		if h.Host == host {
			return h, true
		}
	}
	return mirror.HostView{}, false
}

// TestTheHubKeepsWhatAWindowWaitsOnAndWhatADroppedHostReported: a v2 host's
// waiting window keeps its Pending; a v1 host's does not (it has no rendered
// view, so no Claude claim). When a host that reports status drops, its last
// list keeps the status and pending and the view says since when it is
// offline; a host that does not report status keeps neither.
func TestTheHubKeepsWhatAWindowWaitsOnAndWhatADroppedHostReported(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		host         string
		version      int
		statusReport bool
	}{{"lap", mirror.ProtocolVersion, true}, {"old", 1, true}, {"quiet", mirror.ProtocolVersion, false}} {
		tok, _ := r.store.mint(t, 1, c.host)
		m := winHello(c.host, true, pendingWindow())
		m.Version, m.StatusReport = c.version, c.statusReport
		raw, err := r.rawConnect(t, tok, m)
		if err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, c.host, "work")
		h, _ := pendingHostView(r, c.host)
		got := h.Windows[0].Pending
		if c.version >= 2 && (got == nil || got.Summary != "Bash: make deploy" || got.Last != "Ready.") {
			t.Fatalf("%s: the window's pending: %+v", c.host, got)
		}
		if c.version < 2 && got != nil {
			t.Fatalf("%s: a v1 host's window kept a pending: %+v", c.host, got)
		}
		raw.ws.CloseNow()
		eventually(t, c.host+" offline", func() bool { h, ok := pendingHostView(r, c.host); return ok && !h.Online })
		h, _ = pendingHostView(r, c.host)
		if h.OfflineSince.IsZero() {
			t.Fatalf("%s: the offline view has no OfflineSince", c.host)
		}
		w := h.Windows[0]
		switch {
		case c.host == "lap" && (w.Status != mirror.StatusWaiting || w.Pending == nil):
			t.Fatalf("lap: a reporting host's dropped window lost its status: %+v", w)
		case c.host == "quiet" && (w.Status != "" || w.Pending != nil):
			t.Fatalf("quiet: a non-reporting host's dropped window kept a status: %+v", w)
		}
	}
}
