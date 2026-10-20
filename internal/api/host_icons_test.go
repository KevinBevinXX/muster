package api

import (
	"regexp"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// cardHostIcon is a mirrored card's host (data-f-host) and the icon its header
// draws (data-host-icon), in the order the list draws them.
var cardHostIcon = regexp.MustCompile(`(?s)<article id="mirror-[0-9a-f]+" data-mirror-card=""[^>]*data-f-host="([^"]+)".*?data-card-slot="header".*?data-host-icon="([a-z]+)"`)

// TestEachHostDrawsTheIconOfItsRegistrationRank: through the real mux, the
// Agents list draws each host's icon by the plugin's registration ranks
// (Mirror.HostRanks → ui.HostIconFor) — two hosts, two icons — and a host's
// window's terminal page draws the same one its cards do. The ranks are given
// in the OPPOSITE order to the list's host order and to the names', so an icon
// picked by position on the list or by name would be red.
func TestEachHostDrawsTheIconOfItsRegistrationRank(t *testing.T) {
	ranks := map[string]int{"tower": 0, "ws": 1}
	fm := &fakeMirror{online: true, ranks: ranks,
		others: []mirror.HostView{{Host: "tower", Online: true, Sessions: []string{"mango"}}}}
	s, ts := viewServer(t, fm)

	_, list := mirrorGet(t, s, ts, "/ui/agents")
	got := map[string]map[string]bool{}
	for _, m := range cardHostIcon.FindAllStringSubmatch(list, -1) {
		if got[m[1]] == nil {
			got[m[1]] = map[string]bool{}
		}
		got[m[1]][m[2]] = true
	}
	for host, r := range ranks {
		want := ui.HostIconFor(map[string]int{host: r}, host)
		if len(got[host]) != 1 || !got[host][want] {
			t.Errorf("host %s's cards draw %v; want only %q (rank %d)", host, got[host], want, r)
		}
	}
	if ui.HostIconFor(ranks, "tower") == ui.HostIconFor(ranks, "ws") {
		t.Fatal("control: the two ranks name one icon")
	}

	_, term := mirrorGet(t, s, ts, ui.MirrorTerminalPath("tower", "mango"))
	badge := regexp.MustCompile(`data-term-badge=""[^>]*><span data-host-icon="([a-z]+)"`).FindStringSubmatch(term)
	if badge == nil || badge[1] != ui.HostIconFor(ranks, "tower") {
		t.Errorf("tower's terminal page badge draws %v, want %q", badge, ui.HostIconFor(ranks, "tower"))
	}
}
