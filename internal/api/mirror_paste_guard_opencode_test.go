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

// TestAHostsOpenCodeRefusalReadsTheSameWithAndWithoutScript: when the host
// refuses a send into an opencode pane, the person reads "Sending to opencode
// isn't supported yet" — the host's own sentence — on BOTH paths: the
// composer's script (409, exactly the sentence) and a form the browser posted
// itself (a redirect whose notice says the same). A queued message (Queue,
// Send now) refused the same way says it in its row.
func TestAHostsOpenCodeRefusalReadsTheSameWithAndWithoutScript(t *testing.T) {
	if !strings.HasPrefix(mirror.PasteRefusedOpenCodeText, "Sending to opencode isn't supported yet") {
		t.Fatalf("the opencode refusal no longer says what the operator asked for: %q", mirror.PasteRefusedOpenCodeText)
	}
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	form := url.Values{"host": {"ws"}, "session": {"chief"}, "text": {"y"}}
	fm.v2.pasteErr = &mirror.PasteRefusedError{Why: mirror.PasteRefusedOpenCodeText, Refusal: mirror.PasteRefusalOpenCode}

	code, body, _ := postSend(t, ts.URL, ui.MirrorTerminalSendPath, cookie, "", form)
	if code != http.StatusConflict || body != mirror.PasteRefusedOpenCodeText {
		t.Errorf("with script: %d %q; want 409 and exactly the host's words", code, body)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorTerminalSendPath, strings.NewReader(form.Encode()))
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
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "notice=opencode") {
		t.Fatalf("without script: %d to %q, want a redirect with notice=opencode", resp.StatusCode, loc)
	}
	if _, page := mirrorGet(t, s, ts, loc); !strings.Contains(page, html.EscapeString(mirror.PasteRefusedOpenCodeText)) {
		t.Errorf("without script: the page after the redirect does not say %q", mirror.PasteRefusedOpenCodeText)
	}

	out := sendQueueFailure(fm.v2.pasteErr)
	if out.Sent || out.MayHaveDelivered || out.Pasted || !strings.HasPrefix(out.Detail, "Not sent: ") ||
		!strings.Contains(out.Detail, mirror.PasteRefusedOpenCodeText) {
		t.Errorf("a queued message refused into opencode: %+v; want not sent, saying %q", out, mirror.PasteRefusedOpenCodeText)
	}
}
