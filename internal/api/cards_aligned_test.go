package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAMirroredCardPrintsItsWindowsActivity: the host-reported activity
// (mirror.Window.Activity, Unix seconds floored to a minute) reaches the mirrored card's footer as
// the same "active … ago" <time> an agent card prints — per WINDOW, not per
// host. The two windows' times are pairwise distinct (pinWindows), so a mutant
// that stamps one time on every card, or drops the field, is red. Red before:
// mirrorCards dropped SessionView.Activity and the card had no footer.
func TestAMirroredCardPrintsItsWindowsActivity(t *testing.T) {
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, nil)
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	for _, w := range pinWindows() {
		card := cardWith(t, body, "session="+w.Session+"&amp;window=%40"+w.ID[1:])
		active := regexp.MustCompile(`data-card-active=""[^>]*>.*?<time data-ts="(\d+)"`).FindStringSubmatch(card)
		if active == nil {
			t.Errorf("%s%s: no \"active … ago\" in the card:\n%s", w.Session, w.ID, card)
			continue
		}
		// The hub floors a window's activity to the bucket (mirror.Normalize…).
		if want := (w.Activity - w.Activity%mirror.ActivityBucket) * 1000; active[1] != strconv.FormatInt(want, 10) {
			t.Errorf("%s%s: active data-ts = %s, want %d (the window's own activity)", w.Session, w.ID, active[1], want)
		}
	}
}

// TestAMirroredCardWithNoActivityPrintsNone: an older host that reports no
// activity (0) gets NO "active" item, rather than "active 56y ago" from the
// Unix epoch.
func TestAMirroredCardWithNoActivityPrintsNone(t *testing.T) {
	ws := pinWindows()
	for i := range ws {
		ws[i].Activity = 0
	}
	s, h := pinServer(t, &fakeMirror{online: true, windows: ws}, nil)
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	card := cardWith(t, body, "session=main&amp;window=%401")
	if strings.Contains(card, "data-card-active") {
		t.Errorf("a window with no reported activity printed one:\n%s", card)
	}
	if !strings.Contains(card, `data-mirror-host=""`) {
		t.Errorf("instrument check FAILED: the card has no footer at all:\n%s", card)
	}
}

// TestTheCardPreviewRouteIsGone: the cards' lazy chat preview was its only
// consumer, and it went with it (operator decision: no previews on cards).
func TestTheCardPreviewRouteIsGone(t *testing.T) {
	s, h := pinServer(t, nil, nil)
	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := get("/ui/agents/5/recent"); got != http.StatusNotFound {
		t.Errorf("GET /ui/agents/5/recent = %d, want 404", got)
	}
	// Control: a registered sibling route on the same id is NOT a 404 (this
	// fixture's store answers it with a 500, which is beside the point), so the
	// 404 above is the route's absence and not a refused session.
	if got := get("/ui/agents/5/status"); got == http.StatusNotFound {
		t.Errorf("control: GET /ui/agents/5/status = 404 too — the instrument cannot see a registered route")
	}
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	if strings.Contains(body, "/recent") || strings.Contains(body, "agent-recent-") {
		t.Errorf("the list still wires a card preview")
	}
}

// cardWith is the <article> in the list markup that contains marker.
func cardWith(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no card with %q in:\n%s", marker, body)
	}
	start := strings.LastIndex(body[:i], "<article")
	rest := body[start:]
	if j := strings.Index(rest, "</article>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
