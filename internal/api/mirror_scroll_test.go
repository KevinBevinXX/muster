package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// DRAG-TO-SCROLL's route and its page wiring, through the real mux and real
// session cookies. The literals (the route, the attributes) are spelled here
// rather than read from the package, so a renamed route fails a test.

const termScrollPath = "/mirror/terminal/scroll"

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

// TestAScrollNeedsNoStepUpAndIsClamped: a same-origin scroll from a signed-in
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
// refusing before the plugin is asked. The last row is the control.
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
	}
	can := func() *fakeMirror {
		return &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}}
	}
	for _, c := range []tc{
		{"no session", can(), false, "", "ws", "chief", "3", http.StatusUnauthorized},
		{"another origin", can(), true, "https://evil.example", "ws", "chief", "3", http.StatusForbidden},
		{"zero lines", can(), true, "", "ws", "chief", "0", http.StatusBadRequest},
		{"not a number", can(), true, "", "ws", "chief", "3.5", http.StatusBadRequest},
		{"no lines", can(), true, "", "ws", "chief", "", http.StatusBadRequest},
		{"another host", can(), true, "", "nope", "chief", "3", http.StatusNotFound},
		{"an unlisted session", can(), true, "", "ws", "gone", "3", http.StatusNotFound},
		{"an older muster-mirror", &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, true, "", "ws", "chief", "3", http.StatusConflict},
		{"a read-only host", &fakeMirror{online: true, interactive: false, scroll: true, v2: &fakeMirrorV2{}}, true, "", "ws", "chief", "3", http.StatusConflict},
		{"control", can(), true, "", "ws", "chief", "3", http.StatusNoContent},
	} {
		s, ts := viewServer(t, c.fm)
		cookie := ""
		if c.cookie {
			cookie = sessionCookieFor(s)
		}
		code, body, _ := postSend(t, ts.URL, termScrollPath, cookie, c.origin,
			url.Values{"host": {c.host}, "session": {c.session}, "lines": {c.lines}})
		asked := len(c.fm.scrolled())
		if code != c.status || (asked == 1) != (c.status == http.StatusNoContent) {
			t.Errorf("%s: %d %q, plugin asked %d time(s); want %d", c.name, code, body, asked, c.status)
		}
	}
}

// TestTheHostsAnswerIsMappedToAStatus: what the plugin answers decides the
// status the page reads (409 stops its gesture, anything else is one dropped
// scroll).
func TestTheHostsAnswerIsMappedToAStatus(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("%w: pane %%3 is in tree-mode", mirror.ErrScrollRefused), http.StatusConflict},
		{mirror.ErrScrollUnsupported, http.StatusConflict},
		{mirror.ErrScrollTimeout, http.StatusGatewayTimeout},
		{mirror.ErrSessionGone, http.StatusNotFound},
		{mirror.ErrHostDisconnected, http.StatusNotFound},
		{errors.New("mirror: send the scroll to the host: broken pipe"), http.StatusBadGateway},
	} {
		fm := &fakeMirror{online: true, interactive: true, scroll: true, v2: &fakeMirrorV2{}, scrollErr: c.err}
		s, ts := viewServer(t, fm)
		if code, body, _ := postSend(t, ts.URL, termScrollPath, sessionCookieFor(s), "", scrollForm("chief", "2")); code != c.status {
			t.Errorf("%v: %d %q, want %d", c.err, code, body, c.status)
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
