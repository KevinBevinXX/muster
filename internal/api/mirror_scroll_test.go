package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// DRAG-TO-SCROLL's route and its page wiring, through the real mux and real
// session cookies. The literals (the route, the attributes) are spelled here
// rather than read from the package, so a renamed route fails a test.

const termScrollPath = "/mirror/terminal/scroll"

// scrollRefusalHeader is the header a refusal names its kind in; the page
// stops its gesture only on "unsupported".
const scrollRefusalHeader = "X-Muster-Scroll-Refusal"

func scrollForm(session string, lines string) url.Values {
	return url.Values{"host": {"ws"}, "session": {session}, "lines": {lines}}
}

func (f *fakeMirror) scrolled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scrolls)
}

// TestTheTerminalPageWiresDragScrollOnlyWhereTheHostCanScroll: #term carries
// the scroll route and its address only for a host that advertised scrolling
// AND is -interactive; an older muster-mirror's or a read-only host's page has
// neither — and with them, the page's script wires no gesture at all.
func TestTheTerminalPageWiresDragScrollOnlyWhereTheHostCanScroll(t *testing.T) {
	for name, tc := range map[string]struct {
		fm   *fakeMirror
		want bool
	}{
		"an interactive host that can scroll": {&fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}, true},
		"an older muster-mirror":              {&fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, false},
		"a read-only host":                    {&fakeMirror{online: true, interactive: false, scroll: true, v2: &fakeMirrorV2{}}, false},
		"an offline host":                     {&fakeMirror{online: false, interactive: true, scroll: true, v2: &fakeMirrorV2{}}, false},
	} {
		s, ts := viewServer(t, tc.fm)
		_, page := mirrorGet(t, s, ts, chiefTermPage)
		term := tagOf(page, "term")
		if term == "" {
			t.Fatalf("%s: fixture: no #term", name)
		}
		got := strings.Contains(term, `data-scroll="/mirror/terminal/scroll"`)
		if got != tc.want {
			t.Errorf("%s: #term wires drag-to-scroll = %v, want %v: %s", name, got, tc.want, term)
		}
		if tc.want && !strings.Contains(term, `data-scroll-form="host=ws&amp;session=chief"`) {
			t.Errorf("%s: the scroll's address: %s", name, term)
		}
		if !tc.want && strings.Contains(term, "data-scroll-form") {
			t.Errorf("%s: an address with no route: %s", name, term)
		}
	}
}

// TestAnAgentPodsTerminalPageWiresNoDragScroll: RECORDED EXCLUSION (the
// operator's decision) — drag-to-scroll is for mirrored windows only, so an
// agent pod's Terminal page, served by its real route, carries no scroll route,
// no address and no scroll note, and with them the page's script wires no
// gesture (it wires one only on data-scroll). The control is a mirrored
// session's page from the same kind of server, which has all three.
func TestAnAgentPodsTerminalPageWiresNoDragScroll(t *testing.T) {
	s, ts := sendServer(t, newSendAgents(), &turnGateway{})
	resp, page := mirrorGet(t, s, ts, "/agents/keen-wren/terminal")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the agent's terminal page = %d", resp.StatusCode)
	}
	term := tagOf(page, "term")
	if term == "" {
		t.Fatal("fixture: no #term on the agent's terminal page")
	}
	for _, attr := range []string{"data-scroll", "data-scroll-form"} {
		if strings.Contains(term, attr+"=") {
			t.Errorf("an agent pod's #term carries %s: %s", attr, term)
		}
	}
	if strings.Contains(page, `id="term-scroll-note"`) {
		t.Error("an agent pod's terminal page has the scroll note")
	}

	fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	ms, mts := viewServer(t, fm)
	_, control := mirrorGet(t, ms, mts, chiefTermPage)
	if c := tagOf(control, "term"); !strings.Contains(c, `data-scroll="/mirror/terminal/scroll"`) || !strings.Contains(c, "data-scroll-form=") ||
		!strings.Contains(control, `id="term-scroll-note"`) {
		t.Fatalf("control: a mirrored session's page lacks the scroll wiring this test looks for: %s", c)
	}
}

// TestAScrollNeedsNoStepUpAndIsClamped:a same-origin scroll from a signed-in
// session reaches the plugin with its line count — from a STALE sign-in too
// (the operator's decision: copy-mode reaches no program) — and a count beyond
// MaxScrollLines either way is clamped, not refused.
func TestAScrollNeedsNoStepUpAndIsClamped(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	for _, c := range []struct {
		cookie, lines string
	}{{sessionCookieFor(s), "7"}, {staleCookieFor(s), "-3"}, {sessionCookieFor(s), "100000"}, {sessionCookieFor(s), "-100000"}} {
		if code, body, _ := postSend(t, ts.URL, termScrollPath, c.cookie, "", scrollForm("chief", c.lines)); code != http.StatusNoContent {
			t.Fatalf("a scroll of %s: %d %q, want 204", c.lines, code, body)
		}
	}
	want := []string{"1 ws/chief/ 7", "1 ws/chief/ -3", "1 ws/chief/ 200", "1 ws/chief/ -200"}
	if got := fm.scrolled(); !slices.Equal(got, want) {
		t.Fatalf("the plugin was asked %q, want %q", got, want)
	}
}

// TestAScrollIsRefusedBeforeThePluginIsAsked: every gate the route keeps, each
// refusing before the plugin is asked, and naming its refusal (the header the
// page branches on): only a host that CANNOT scroll is "unsupported" — one
// that is offline, or a session that is gone, is not. The last row is the
// control.
func TestAScrollIsRefusedBeforeThePluginIsAsked(t *testing.T) {
	type tc struct {
		name    string
		fm      *fakeMirror
		cookie  bool
		origin  string
		host    string
		session string
		lines   string
		status  int
		code    string
	}
	can := func() *fakeMirror {
		return &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	}
	for _, c := range []tc{
		{"no session", can(), false, "", "ws", "chief", "3", http.StatusUnauthorized, ""},
		{"another origin", can(), true, "https://evil.example", "ws", "chief", "3", http.StatusForbidden, ""},
		{"zero lines", can(), true, "", "ws", "chief", "0", http.StatusBadRequest, ""},
		{"not a number", can(), true, "", "ws", "chief", "3.5", http.StatusBadRequest, ""},
		{"no lines", can(), true, "", "ws", "chief", "", http.StatusBadRequest, ""},
		{"not a session name", can(), true, "", "ws", "a\x01b", "3", http.StatusBadRequest, ""},
		{"a host not connected now (or another owner's)", can(), true, "", "nope", "chief", "3", http.StatusServiceUnavailable, "offline"},
		{"an offline host", &fakeMirror{online: false, interactive: true, scroll: true, v2: &fakeMirrorV2{}}, true, "", "ws", "chief", "3", http.StatusServiceUnavailable, "offline"},
		{"an unlisted session", can(), true, "", "ws", "gone", "3", http.StatusNotFound, "gone"},
		{"an older muster-mirror", &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, true, "", "ws", "chief", "3", http.StatusConflict, "unsupported"},
		{"a read-only host", &fakeMirror{online: true, interactive: false, scroll: true, v2: &fakeMirrorV2{}}, true, "", "ws", "chief", "3", http.StatusConflict, "unsupported"},
		{"control", can(), true, "", "ws", "chief", "3", http.StatusNoContent, ""},
	} {
		s, ts := viewServer(t, c.fm)
		cookie := ""
		if c.cookie {
			cookie = sessionCookieFor(s)
		}
		code, body, hdr := postSend(t, ts.URL, termScrollPath, cookie, c.origin,
			url.Values{"host": {c.host}, "session": {c.session}, "lines": {c.lines}})
		asked := len(c.fm.scrolled())
		if code != c.status || (asked == 1) != (c.status == http.StatusNoContent) || hdr.Get(scrollRefusalHeader) != c.code {
			t.Errorf("%s: %d %q (refusal %q), plugin asked %d time(s); want %d (refusal %q)", c.name, code, body, hdr.Get(scrollRefusalHeader), asked, c.status, c.code)
		}
	}
}

// TestTheHostsAnswerIsMappedToAStatus: what the plugin answers decides the
// status and the refusal code the page reads. Only "unsupported" stops the
// gesture; "offline", "mode", "gone" and "refused" show a note and keep it;
// no code (a timeout, a broken link) is put back and sent again.
func TestTheHostsAnswerIsMappedToAStatus(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: pane %%3 is in tree-mode", mirror.ErrScrollPaneInMode), http.StatusConflict, "mode"},
		{fmt.Errorf("%w: tmux send-keys: exit 1", mirror.ErrScrollRefused), http.StatusConflict, "refused"},
		{mirror.ErrBadScroll, http.StatusConflict, "refused"},
		{mirror.ErrScrollUnsupported, http.StatusConflict, "unsupported"},
		{mirror.ErrScrollTimeout, http.StatusGatewayTimeout, ""},
		{mirror.ErrSessionGone, http.StatusNotFound, "gone"},
		{mirror.ErrWindowGone, http.StatusNotFound, "gone"},
		{mirror.ErrHostDisconnected, http.StatusServiceUnavailable, "offline"},
		{mirror.ErrHostOffline, http.StatusServiceUnavailable, "offline"},
		{errors.New("mirror: send the scroll to the host: broken pipe"), http.StatusBadGateway, ""},
	} {
		fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}, scrollErr: c.err}
		s, ts := viewServer(t, fm)
		code, body, hdr := postSend(t, ts.URL, termScrollPath, sessionCookieFor(s), "", scrollForm("chief", "2"))
		if code != c.status || hdr.Get(scrollRefusalHeader) != c.code {
			t.Errorf("%v: %d %q (refusal %q), want %d (refusal %q)", c.err, code, body, hdr.Get(scrollRefusalHeader), c.status, c.code)
		}
	}
}

// TestScrollIsRateLimitedPerUser: past scrollRateBurst in one window the route
// answers 429 with Retry-After, and the plugin is not asked.
func TestScrollIsRateLimitedPerUser(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	limited, hdr := 0, http.Header{}
	for i := 0; i < scrollRateBurst+10; i++ {
		code, _, h := postSend(t, ts.URL, termScrollPath, cookie, "", scrollForm("chief", "1"))
		if code == http.StatusTooManyRequests {
			limited++
			hdr = h
		}
	}
	// A slow runner could straddle a window and earn a second burst; never
	// fewer than the burst gets through, and the overflow is refused.
	if asked := len(fm.scrolled()); asked < scrollRateBurst || asked > 2*scrollRateBurst || limited == 0 {
		t.Fatalf("%d asked, %d limited; want at least the burst (%d) asked and some limited", asked, limited, scrollRateBurst)
	}
	if hdr.Get("Retry-After") != "1" {
		t.Errorf("a 429 without Retry-After: %v", hdr)
	}
}

// TestTheScrollLimitPassesAFullSpeedPageAndStopsAFlood: the limit sits ABOVE
// the page's own fastest cadence — one request per animation frame on a 120 Hz
// screen, so 120 a second, spread evenly — for several seconds running, and it
// still refuses a script that sends 1000 in a tenth of a second. The clock is
// synthetic, so neither half depends on how fast the runner is. 120 and 1000
// are literals: they are the page's cadence and a flood, not the constant.
func TestTheScrollLimitPassesAFullSpeedPageAndStopsAFlood(t *testing.T) {
	var k keyedRate
	start := time.Unix(1_700_000_000, 0)
	const perSecond, seconds = 120, 4
	for i := 0; i < perSecond*seconds; i++ {
		now := start.Add(time.Duration(i) * time.Second / perSecond)
		if !k.allow(7, now, scrollRateBurst, scrollRateWindow) {
			t.Fatalf("a page sending %d a second was refused at request %d (%v in)", perSecond, i, now.Sub(start))
		}
	}
	flood := start.Add(time.Hour)
	allowed := 0
	for i := 0; i < 1000; i++ {
		if k.allow(8, flood.Add(time.Duration(i)*100*time.Millisecond/1000), scrollRateBurst, scrollRateWindow) {
			allowed++
		}
	}
	if allowed == 1000 || allowed < perSecond {
		t.Fatalf("a flood of 1000 in 100ms: %d allowed; want it limited, yet at least %d through", allowed, perSecond)
	}
	// Per user: user 8's flood did not spend user 7's allowance.
	if !k.allow(7, flood, scrollRateBurst, scrollRateWindow) {
		t.Error("one user's flood limited another user")
	}
}

// TestASecondOfAFullSpeedPageIsNeverRefusedThroughTheRoute: 120 scrolls — a
// 120 Hz page's second — back to back through the real mux and limiter, each
// answered 204. They arrive faster than a page sends them (a page waits for
// each answer AND for the next frame), so this is the harder case.
func TestASecondOfAFullSpeedPageIsNeverRefusedThroughTheRoute(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	for i := 0; i < 120; i++ {
		if code, body, _ := postSend(t, ts.URL, termScrollPath, cookie, "", scrollForm("chief", "1")); code != http.StatusNoContent {
			t.Fatalf("scroll %d of a full-speed page: %d %q, want 204", i+1, code, body)
		}
	}
	if got := len(fm.scrolled()); got != 120 {
		t.Fatalf("the plugin was asked %d times, want 120", got)
	}
}
