package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// The message box's microphone (dictate.go) on every kind of message box.

// dictateSurfaces renders each message box with the microphone path dict:
// a mirrored session's Rendered view, its Terminal view, an agent's Terminal
// view, an agent's chat page, and the chief panel's chat.
func dictateSurfaces(t *testing.T, dict string) map[string]string {
	t.Helper()
	out := map[string]string{}
	out["mirror rendered"] = renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true,
		StepUp: stepUpFixture(), Nonce: "0123456789abcdef", Dictate: dict}))
	out["mirror terminal"] = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Dictate: dict}}))
	out["agent terminal"] = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren",
		Composer: &ComposerView{SendPath: AgentSendPath("keen-wren"), Dictate: dict}}))
	v := sampleAgentDetailView()
	v.Dictate = dict
	var b bytes.Buffer
	if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	out["agent chat"] = b.String()
	b.Reset()
	cv := sampleAgentDetailView()
	cv.Dictate, cv.Surface = dict, ChatSurfaceChiefPanel
	if err := RenderChiefPanelBody(&b, ChiefPanelView{Configured: true, Chat: cv}); err != nil {
		t.Fatal(err)
	}
	out["chief panel"] = b.String()
	return out
}

var dictateButtonTag = regexp.MustCompile(`<button id="(mv|chat)-dictate" type="button"[^>]*>`)

// TestEveryMessageBoxHasTheMicrophone: the SAME control, addressing its own
// box, on every surface — and the script that drives it beside it.
func TestEveryMessageBoxHasTheMicrophone(t *testing.T) {
	for name, page := range dictateSurfaces(t, TranscribePath) {
		tags := dictateButtonTag.FindAllString(page, -1)
		if len(tags) != 1 {
			t.Errorf("%s: %d microphone buttons; want exactly 1", name, len(tags))
			continue
		}
		tag := tags[0]
		input := "mv-input"
		if strings.Contains(tag, `id="chat-dictate"`) {
			input = "chat-input"
		}
		for _, want := range []string{`data-dictate="/transcribe"`, `data-dictate-input="` + input + `"`, `data-dictate-max="120"`} {
			if !strings.Contains(tag, want) {
				t.Errorf("%s: the mic lacks %s: %s", name, want, tag)
			}
		}
		// The box it addresses and the note it reports into are on the page.
		note := regexp.MustCompile(`data-dictate-note="([^"]+)"`).FindStringSubmatch(tag)
		if note == nil || !strings.Contains(page, `id="`+note[1]+`"`) {
			t.Errorf("%s: the mic's note element is not on the page: %s", name, tag)
		}
		if !strings.Contains(page, `<textarea id="`+input+`"`) {
			t.Errorf("%s: the mic addresses #%s, which is not on the page", name, input)
		}
		// 🔴 HIDDEN UNTIL THE SCRIPT RUNS: with no script there is no mic (the
		// `hidden` CLASS — an attribute would lose to the display utility).
		if !regexp.MustCompile(`class="group hidden `).MatchString(tag) || strings.Contains(tag, "inline-flex") {
			t.Errorf("%s: the mic is visible before any script runs: %s", name, tag)
		}
		// A 44px target.
		if !strings.Contains(tag, "min-h-[44px]") || !strings.Contains(tag, "min-w-[44px]") {
			t.Errorf("%s: the mic is not a 44px target: %s", name, tag)
		}
		if !strings.Contains(page, "window.__musterDictate") {
			t.Errorf("%s: the microphone's script is not on the page", name)
		}
	}
}

// TestNoServiceNoMicrophone: with no speech-to-text service (an empty path)
// no surface draws the button, its note or its script.
func TestNoServiceNoMicrophone(t *testing.T) {
	for name, page := range dictateSurfaces(t, "") {
		for _, absent := range []string{"data-dictate", "dictate-note", "__musterDictate", TranscribePath} {
			if strings.Contains(page, absent) {
				t.Errorf("%s: %q is drawn with dictation off", name, absent)
			}
		}
	}
}

// TestADisabledComposerHasNoMicrophone: a read-only host's box cannot send,
// so nothing dictated into it could go anywhere.
func TestADisabledComposerHasNoMicrophone(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: false,
		StepUp: stepUpFixture(), Dictate: TranscribePath}))
	if strings.Contains(page, "data-dictate") || strings.Contains(page, "__musterDictate") {
		t.Error("a read-only host's composer draws a microphone")
	}
	page = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "x",
		Composer: &ComposerView{SendPath: AgentSendPath("x"), Disabled: "not running", Dictate: TranscribePath}}))
	if strings.Contains(page, "data-dictate") {
		t.Error("a disabled agent composer draws a microphone")
	}
}

// TestTheMicrophoneSitsBesideSend: in the input row, between the box and Send,
// so the row's layout does not change when it appears (it only grows a 44px
// square the script reveals).
func TestTheMicrophoneSitsBesideSend(t *testing.T) {
	for name, page := range dictateSurfaces(t, TranscribePath) {
		input, mic, send := "mv-input", "mv-dictate", "mv-send"
		if strings.Contains(page, `id="chat-dictate"`) {
			input, mic, send = "chat-input", "chat-dictate", "chat-send"
		}
		i, m, s := strings.Index(page, `id="`+input+`"`), strings.Index(page, `id="`+mic+`"`), strings.Index(page, `id="`+send+`"`)
		if !(i >= 0 && i < m && m < s) {
			t.Errorf("%s: order textarea(%d) < mic(%d) < send(%d) does not hold", name, i, m, s)
		}
	}
}
