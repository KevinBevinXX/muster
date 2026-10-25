package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestSendNowFailsClosedForAGatedRuntimeWithNoWords: a window of a runtime
// whose send is gated (only idle is sent to) is refused by Send now in EVERY
// state when this server has no words for that runtime — idle included,
// which an empty entry would otherwise send into.
func TestSendNowFailsClosedForAGatedRuntimeWithNoWords(t *testing.T) {
	for _, st := range []mirror.AgentState{
		{Runtime: mirror.RuntimeHermes, Status: mirror.StatusIdle, Since: 7},
		{Runtime: mirror.RuntimeHermes, Status: mirror.StatusBusy, Since: 7},
		{Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting, Since: 7},
		{Runtime: mirror.RuntimeHermes, Unknown: mirror.ErrNoHermesStatus},
	} {
		msg, notice := sendNowRefusalIn(st, map[string]sendNowWords{})
		if msg != SendQueueGatedUnspokenNow || notice != noticeGatedUnspokenNow {
			t.Errorf("%+v with no words: %q %q; want the fail-closed refusal", st, msg, notice)
		}
	}
	if text, ok := composerNotices[noticeGatedUnspokenNow]; !ok || text.text != SendQueueGatedUnspokenNow || text.ok {
		t.Errorf("the fail-closed notice reads %+v (%v)", text, ok)
	}
	// Control: with its words, an idle window is sent to.
	if msg, _ := sendNowRefusalIn(mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusIdle, Since: 7}, gatedSendNowWords); msg != "" {
		t.Fatalf("control: an idle Hermes window with its words was refused: %q", msg)
	}
}

// TestEveryGatedRuntimeHasSendNowWords: every runtime the table gates
// (mirror.RuntimeSpec.GatedSend) has its Send now words, each one a notice
// the composer can show — so the fail-closed refusal above is a backstop,
// not what a real runtime gets.
func TestEveryGatedRuntimeHasSendNowWords(t *testing.T) {
	gated := 0
	for _, r := range mirror.Runtimes() {
		if !r.GatedSend {
			continue
		}
		gated++
		w, ok := gatedSendNowWords[r.Name]
		if !ok {
			t.Errorf("%s is gated and has no Send now words", r.Name)
			continue
		}
		for _, n := range []string{w.unknownNotice, w.waitingNotice, w.busyNotice} {
			if _, ok := composerNotices[n]; !ok {
				t.Errorf("%s: the notice %q has no text", r.Name, n)
			}
		}
	}
	if gated == 0 {
		t.Fatal("instrument check: the table gates no runtime")
	}
}
