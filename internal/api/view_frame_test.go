package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestOnlyTheSwitchsPagesMayBeFramedAndOnlyByThisOrigin pins the framing
// headers: a Rendered | Terminal switch opens its other view in a same-origin
// frame (ui/view_swap.go), so the views' pages allow frame-ancestors 'self'
// (X-Frame-Options SAMEORIGIN) — and every other page keeps 'none' (DENY).
// (A claude-code agent's two pages are the other pair; their frame is
// exercised by e2e/tests/view-swap.spec.ts, which cannot load without it.)
func TestOnlyTheSwitchsPagesMayBeFramedAndOnlyByThisOrigin(t *testing.T) {
	s, ts := viewServer(t, goldHost())
	check := func(path, ancestors, xfo string) {
		t.Helper()
		resp, _ := mirrorGet(t, s, ts, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (fixture)", path, resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors "+ancestors+";") {
			t.Errorf("GET %s: CSP %q, want frame-ancestors %s", path, csp, ancestors)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != xfo {
			t.Errorf("GET %s: X-Frame-Options %q, want %q", path, got, xfo)
		}
	}
	check(ref("scratch2", "@7").ViewPath(), "'self'", "SAMEORIGIN")
	check(ref("scratch2", "@7").TerminalPath(), "'self'", "SAMEORIGIN")
	// A page that is not a view of a switch.
	check("/agents", "'none'", "DENY")
}
