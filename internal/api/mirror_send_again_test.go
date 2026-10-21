package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// postSendNonce posts the rendered view's send as the composer's script does,
// with nonce; its status, body and whether it offered "Send again".
func postSendNonce(t *testing.T, ts, cookie string, form url.Values) (int, string, bool) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts+ui.MirrorSendPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b)), resp.Header.Get(SendAgainHeader) != ""
}

// TestAnUnconfirmedSendIsNeverRepeatedByItsNonce: 🔴 a send whose answer was
// "may have been submitted" KEEPS its nonce: the same message posted again
// with it (a re-tap, the script's retry) is refused and nothing is pasted a
// second time — it may already be in the conversation. A retry is explicit:
// the answer offers "Send again" (SendAgainHeader), which mints a NEW nonce,
// and that one is delivered. For a Claude Code window and a Hermes one.
func TestAnUnconfirmedSendIsNeverRepeatedByItsNonce(t *testing.T) {
	for _, c := range []struct {
		name    string
		windows []mirror.Window
		hermes  bool
		err     error
	}{
		{"Claude Code", []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "c1"}}, false,
			fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)},
		{"Hermes", hermesWindows(), true,
			fmt.Errorf("%w", joinUnconfirmed(&mirror.PasteRefusedError{Why: "May have been submitted: check the conversation before sending again. Hermes recorded no new message within 8s of the Enter.", Refusal: mirror.PasteRefusalHermesUnconfirmed}))},
	} {
		t.Run(c.name, func(t *testing.T) {
			fm := &fakeMirror{online: true, interactive: true, hermesSend: c.hermes, v2: &fakeMirrorV2{}, windows: c.windows, statusReport: true}
			s, ts := viewServer(t, fm)
			cookie := sessionCookieFor(s)
			form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"do the thing"}, "nonce": {"nonce-0123456789abcdef"}}
			fm.v2.pasteErr = c.err
			code, body, again := postSendNonce(t, ts.URL, cookie, form)
			if code < 400 || !again || strings.HasPrefix(body, "Not sent") {
				t.Fatalf("the unconfirmed send: %d %q, send-again offered %t; want a refusal that offers Send again and never says Not sent", code, body, again)
			}
			fm.v2.pasteErr = nil
			code, body, again = postSendNonce(t, ts.URL, cookie, form)
			if code != http.StatusConflict || body != MirrorSendUnconfirmedRepeat || !again {
				t.Fatalf("the same nonce again: %d %q (send-again %t); want 409 %q", code, body, again, MirrorSendUnconfirmedRepeat)
			}
			if _, ps := fm.v2.snapshot(); len(ps) != 1 {
				t.Fatalf("🔴 pasted %d times; the repeat must not paste", len(ps))
			}
			form.Set("nonce", "nonce-fedcba9876543210") // what "Send again" mints
			if code, body, _ = postSendNonce(t, ts.URL, cookie, form); code != http.StatusNoContent {
				t.Fatalf("Send again with a new nonce: %d %q", code, body)
			}
			if _, ps := fm.v2.snapshot(); len(ps) != 2 {
				t.Fatalf("pasted %d times; want 2 (the explicit Send again)", len(ps))
			}
		})
	}
}

// joinUnconfirmed is err marked ErrPasteUnconfirmed, as the hub returns a
// host's hermes-unconfirmed answer.
func joinUnconfirmed(err error) error { return unconfirmedErr{err} }

type unconfirmedErr struct{ err error }

func (u unconfirmedErr) Error() string   { return u.err.Error() }
func (u unconfirmedErr) Unwrap() []error { return []error{u.err, mirror.ErrPasteUnconfirmed} }

// TestAnIdleWithoutATimeRefusalHasItsNoticeWithoutScript: the hub's
// not-idle refusal for status "idle" with no since reads the unknown words on
// the no-script path too (a notice code, never an empty notice).
func TestAnIdleWithoutATimeRefusalHasItsNoticeWithoutScript(t *testing.T) {
	if hermesNotIdleNotices[mirror.StatusIdle] != noticeHermesUnknown {
		t.Fatalf("hermesNotIdleNotices[idle] = %q, want %q", hermesNotIdleNotices[mirror.StatusIdle], noticeHermesUnknown)
	}
	if (&mirror.HermesNotIdleError{Status: mirror.StatusIdle}).Error() != composerNotices[noticeHermesUnknown].text {
		t.Fatal("the idle-without-a-time refusal and its notice say different things")
	}
}
