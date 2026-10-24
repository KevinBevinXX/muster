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

// TestTheClaudePromptGuardsRefusalReadsTheSameWithAndWithoutScript: when the
// Claude prompt guard (the hub's, or the host's at the moment of typing)
// refuses a rendered-view Send, the person reads its own words — with the
// composer's script (409, exactly the sentence) and with a form the browser
// posted (a redirect whose notice says the same). "Pasted, not submitted" is
// said as such, never as "not sent".
func TestTheClaudePromptGuardsRefusalReadsTheSameWithAndWithoutScript(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true,
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "c1"}}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"1"}}
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
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("a browser-submitted send answered %d, want a redirect back to the view", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	pasted := "Pasted, not submitted: the text is in Claude Code's input box, and muster did not press Enter, because Claude is waiting on a permission prompt — answer it first. Check the terminal."
	for _, c := range []struct{ refusal, why, notice, page string }{
		{mirror.PasteRefusalClaudeWaiting, mirror.ClaudeWaitingText, "claude-waiting", mirror.ClaudeWaitingText},
		{mirror.PasteRefusalClaudeWaiting, mirror.ClaudeDialogText, "claude-waiting", mirror.ClaudeWaitingText},
		{mirror.PasteRefusalClaudePasted, pasted, "claude-pasted", "Pasted, not submitted"},
		{mirror.PasteRefusalClaudeBox, mirror.ClaudeBoxText, "claude-box", mirror.ClaudeBoxText},
	} {
		fm.v2.pasteErr = &mirror.PasteRefusedError{Why: c.why, Refusal: c.refusal}
		code, body, _ := postSend(t, ts.URL, ui.MirrorSendPath, cookie, "", form)
		if code != http.StatusConflict || body != c.why {
			t.Errorf("%s, with script: %d %q; want 409 and exactly %q", c.refusal, code, body, c.why)
		}
		loc := nav()
		if !strings.HasSuffix(loc, "notice="+c.notice) {
			t.Errorf("%s, without script: redirected to %q, want the notice %q", c.refusal, loc, c.notice)
			continue
		}
		if _, page := mirrorGet(t, s, ts, loc); !strings.Contains(page, html.EscapeString(c.page)) {
			t.Errorf("%s, without script: the page after the redirect does not say %q", c.refusal, c.page)
		}
	}
	if !strings.Contains(mirror.ClaudeWaitingText, "Claude is waiting on a permission prompt — answer it first") {
		t.Fatalf("the refusal no longer says what the operator asked for: %q", mirror.ClaudeWaitingText)
	}
}
