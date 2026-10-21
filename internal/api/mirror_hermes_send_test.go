package api

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// HERMES PHASE 3: a Hermes window's rendered view SENDS — composer enabled,
// queue offered — exactly when its host advertised hermesSend; and how a
// Hermes send ended reads the same with and without script.

// TestAHermesWindowSendsOnlyWhenItsHostDoes: the same Hermes window, on a
// host without hermesSend (disabled, no queue — Phase 2's page) and with it
// (enabled, Hermes's words, a queue); a read-only host's stays disabled.
func TestAHermesWindowSendsOnlyWhenItsHostDoes(t *testing.T) {
	view := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}
	input := func(page string) string {
		i := strings.Index(page, `id="mv-input"`)
		tag := page[strings.LastIndex(page[:i], "<"):]
		return tag[:strings.Index(tag, ">")]
	}
	for _, c := range []struct {
		name                    string
		interactive, hermesSend bool
		sends                   bool
	}{
		{"without hermesSend", true, false, false},
		{"with hermesSend", true, true, true},
		{"read-only, hermesSend claimed", false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fm := &fakeMirror{online: true, interactive: c.interactive, hermesSend: c.hermesSend, v2: &fakeMirrorV2{}, windows: hermesWindows(), statusReport: true}
			s, ts := viewServer(t, fm)
			_, page := mirrorGet(t, s, ts, view.ViewPath())
			disabled := disabledAttr.MatchString(input(page))
			if disabled == c.sends {
				t.Fatalf("composer disabled = %t; want %t", disabled, !c.sends)
			}
			if got := strings.Contains(page, html.EscapeString(ui.MirrorHermesNoSendNote)); got == c.sends && c.interactive {
				t.Errorf("the no-send note shown = %t", got)
			}
			if !strings.Contains(page, "Message Hermes") || strings.Contains(page, "Claude is working") {
				t.Error("the Hermes composer calls the agent Claude")
			}
		})
	}
}

// TestAHermesSendsOutcomeReadsTheSameWithAndWithoutScript: the hub's idle
// gate and each way the host's gated send ends — the person reads its words
// (with script: exactly them; without: a notice whose text says the same
// thing). "May have been submitted" is never a "Not sent".
func TestAHermesSendsOutcomeReadsTheSameWithAndWithoutScript(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, hermesSend: true, v2: &fakeMirrorV2{}, windows: hermesWindows(), statusReport: true}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"hello"}}
	nav := func() string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorSendPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("Sec-Fetch-Dest", "document")
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	for _, c := range []struct {
		name   string
		err    error
		body   string
		notice string
		says   string
	}{
		{"busy", &mirror.HermesNotIdleError{Status: mirror.StatusBusy}, mirror.HermesBusyText, "hermes-busy", mirror.HermesBusyText},
		{"waiting", &mirror.HermesNotIdleError{Status: mirror.StatusWaiting}, mirror.HermesWaitingText, "hermes-waiting", mirror.HermesWaitingText},
		{"unknown", &mirror.HermesNotIdleError{}, mirror.HermesUnknownText, "hermes-unknown", mirror.HermesUnknownText},
		{"not submitted", &mirror.PasteRefusedError{Why: "Not sent: the screen. Nothing was pasted.", Refusal: mirror.PasteRefusalHermesNotSubmitted},
			"Not sent: the screen. Nothing was pasted.", "hermes-not-sent", "Nothing was pasted"},
		{"pasted", &mirror.PasteRefusedError{Why: "Pasted, not submitted: x.", Refusal: mirror.PasteRefusalHermesPasted},
			"Pasted, not submitted: x.", "hermes-pasted", "Pasted, not submitted"},
		{"unconfirmed", &mirror.PasteRefusedError{Why: "May have been submitted: y.", Refusal: mirror.PasteRefusalHermesUnconfirmed},
			"May have been submitted: y.", "hermes-unconfirmed", "May have been submitted"},
	} {
		fm.v2.pasteErr = c.err
		code, body, _ := postSend(t, ts.URL, ui.MirrorSendPath, cookie, "", form)
		if code != http.StatusConflict || body != c.body {
			t.Errorf("%s, with script: %d %q; want 409 %q", c.name, code, body, c.body)
		}
		loc := nav()
		if !strings.HasSuffix(loc, "notice="+c.notice) {
			t.Errorf("%s, without script: %q, want notice=%s", c.name, loc, c.notice)
			continue
		}
		if _, page := mirrorGet(t, s, ts, loc); !strings.Contains(page, html.EscapeString(c.says)) {
			t.Errorf("%s, without script: the page does not say %q", c.name, c.says)
		}
	}
	if strings.Contains(composerNotices[noticeHermesUnconfirmed].text, "Not sent") {
		t.Error("may-have-been-submitted reads as Not sent")
	}
}

// TestAHermesWindowIsQueuedForOnlyOnASendingHost: the queue gate.
func TestAHermesWindowIsQueuedForOnlyOnASendingHost(t *testing.T) {
	ws := mirror.NormalizeWindows(hermesWindows(), 0)
	h := mirror.HostView{Host: "ws", Online: true, StatusReport: true, Rendered: true, Windows: ws}
	var e mirror.SessionView
	for _, x := range h.Entries() {
		if x.Window == "@1" {
			e = x
		}
	}
	if !e.HermesView() {
		t.Fatalf("instrument check: @1 is not a Hermes view: %+v", e)
	}
	if off := mirrorQueueOff(h, e); off == "" {
		t.Fatal("a Hermes window of a host without hermesSend can be queued for")
	}
	h.HermesSend = true
	if off := mirrorQueueOff(h, e); off != "" {
		t.Fatalf("a Hermes window of a host with hermesSend: %q", off)
	}
	shell := mirror.SessionView{Host: "ws", Session: "main", Window: "@9"}
	if off := mirrorQueueOff(h, shell); off != SendQueueNotClaude {
		t.Fatalf("a plain window: %q", off)
	}
}

// TestAHermesWindowIsQueuedForOnlyOnASendingHost_Route: through the real mux,
// a queue post for a Hermes window is stored only when its host advertised
// hermesSend (the rendered view then also draws the queue); without it the
// post is refused saying why and nothing is stored.
func TestAHermesWindowIsQueuedForOnlyOnASendingHost_Route(t *testing.T) {
	for _, on := range []bool{false, true} {
		fm := &fakeMirror{online: true, interactive: true, statusReport: true, hermesSend: on, v2: &fakeMirrorV2{}, windows: hermesWindows()}
		q := &memQueue{}
		s, ts := queueServer(t, fm, q, &turnGateway{})
		cookie := sessionCookieFor(s)
		resp, body := postQueue(t, ts, cookie, mirrorQueueForm("main", "@1", "for Hermes"))
		_, page := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath())
		queued := len(q.all()) == 1
		switch {
		case on && (resp.StatusCode != http.StatusNoContent || !queued || !strings.Contains(page, ui.SendQueuePath)):
			t.Fatalf("with hermesSend: %d %q, queued %t, queue drawn %t", resp.StatusCode, body, queued, strings.Contains(page, ui.SendQueuePath))
		case !on && (resp.StatusCode != http.StatusConflict || queued || strings.Contains(page, ui.SendQueuePath)):
			t.Fatalf("without hermesSend: %d %q, queued %t", resp.StatusCode, body, queued)
		}
	}
}
