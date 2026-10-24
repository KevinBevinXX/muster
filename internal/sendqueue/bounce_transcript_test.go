package sendqueue_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE SEAM between the host's transcript reading (mirrorhost.TurnEndOf) and
// the queue's "was this message refused?" (sendqueue.Bounced), on synthetic
// transcripts in the CLI's record shapes: no fake of either side.
//
// 🔴 The round-1 audit's case: the queue pastes M on status S; Claude ANSWERS
// M; a prompt X typed in the terminal then hits the usage limit. X's only
// reply was the limit, so the host reports that turn Rejected — and before the
// fix, M was marked failed for it, and its Retry ran M twice. Red at a5d97d58.

var b0 = time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)

func ts(off time.Duration) string { return b0.Add(off).Format(time.RFC3339Nano) }

func js(s string) string { b, _ := json.Marshal(s); return string(b) }

func bPrompt(off time.Duration, text string) string {
	return `{"type":"user","timestamp":"` + ts(off) + `","message":{"role":"user","content":` + js(text) + `}}`
}

func bReply(off time.Duration, text string) string {
	return `{"type":"assistant","timestamp":"` + ts(off) + `","message":{"model":"claude-x","content":[{"type":"text","text":` + js(text) + `}]}}`
}

func bLimit(off time.Duration) string {
	return `{"type":"assistant","timestamp":"` + ts(off) + `","error":"rate_limit","isApiErrorMessage":true,` +
		`"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 10:30pm (UTC)"}]}}`
}

// bounced is Bounced for message body, pasted on status pastedOn (ms) into
// Claude session pastedIn, with the window idle now on status now (ms) in
// session now's transcript lines; the hub's Pasted is 0 (a terminal prompt
// moves nothing in muster; a restart forgets).
func bounced(body, pastedIn, session string, pastedOn, now int64, lines ...string) bool {
	tr := strings.Join(lines, "\n") + "\n"
	st := sendqueue.AgentState{Idle: true, Since: now, Session: session, TurnError: mirrorhost.TurnEndOf(strings.NewReader(tr), int64(len(tr)))}
	return sendqueue.Bounced(sendqueue.Item{State: sendqueue.Sent, Body: body, AfterSince: pastedOn}, pastedIn, st)
}

func ms(off time.Duration) int64 { return b0.Add(off).UnixMilli() }

func TestAMessageClaudeAnsweredIsNotBouncedByALaterPromptsLimit(t *testing.T) {
	// The window was idle on status 0s (the earlier turn's end); M is pasted.
	earlier := []string{bPrompt(-time.Minute, "earlier"), bReply(-time.Second, "done")}
	audit := append(append([]string{}, earlier...),
		bPrompt(time.Second, "M"), bReply(5*time.Second, "answered M"),
		bPrompt(time.Minute, "X typed in the terminal"), bLimit(time.Minute+500*time.Millisecond))
	if bounced("M", "s1", "s1", ms(0), ms(time.Minute+time.Second), audit...) {
		t.Fatal("M, which Claude answered, is Bounced by the limit a later prompt hit")
	}
	// The same text typed again in the terminal: still not M's refusal — M's
	// answer lies between its paste and that prompt.
	again := append(append([]string{}, earlier...),
		bPrompt(time.Second, "M"), bReply(5*time.Second, "answered M"),
		bPrompt(time.Minute, "M"), bLimit(time.Minute+500*time.Millisecond))
	if bounced("M", "s1", "s1", ms(0), ms(time.Minute+time.Second), again...) {
		t.Fatal("M is Bounced by a later refusal of the same text, though M was answered")
	}
	// CONTROL: M itself refused, nothing between its paste and the refusal.
	refused := append(append([]string{}, earlier...), bPrompt(time.Second, "M"), bLimit(1500*time.Millisecond))
	if !bounced("M", "s1", "s1", ms(0), ms(2*time.Second), refused...) {
		t.Fatal("CONTROL: M, refused by the limit right after its paste, is not Bounced")
	}
	// A first turn in the SAME session the message was pasted into (nothing
	// before it in the transcript) still bounces...
	first := []string{bPrompt(time.Second, "M"), bLimit(1500 * time.Millisecond)}
	if !bounced("M", "s1", "s1", ms(0), ms(2*time.Second), first...) {
		t.Fatal("CONTROL: M refused as its session's first turn is not Bounced")
	}
	// ...but the same first turn in a DIFFERENT session — a fresh claude in the
	// same pane, whose first prompt is M's text — never does (F1).
	if bounced("M", "s1", "s2", ms(0), ms(2*time.Second), first...) {
		t.Fatal("M is Bounced by a refusal in another Claude session")
	}
}
