package mirrorhost

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// hermesAnswerBackend lists one Hermes window, work:@1, waiting on kind, and
// attaches a's viewers.
type hermesAnswerBackend struct {
	*windowsBackend
	src *HermesSource
	a   *fakeAttacher
}

func (b hermesAnswerBackend) HermesSource() *HermesSource { return b.src }
func (b hermesAnswerBackend) AttachHermesViewer(ctx context.Context, s string) (HermesViewer, []hermeshost.ApprovalPrompt, error) {
	return b.a.AttachHermesViewer(ctx, s)
}

func runHermesHost(t *testing.T, kind string, answer bool) (*drivenMuster, *fakeAttacher) {
	t.Helper()
	d := newDrivenMuster(t)
	a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}}
	ws := hermesWindow(mirror.StatusWaiting, kind)
	ws[0].Index, ws[0].Active = 0, true
	b := hermesAnswerBackend{windowsBackend: &windowsBackend{windows: ws}, src: NewHermesSource(t.TempDir(), t.TempDir()), a: a}
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Interactive: true, HermesAnswer: answer,
		AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	if got := d.of(mirror.TypeHello)[0].HermesAnswer; got != answer {
		t.Fatalf("hello.HermesAnswer = %v, want %v", got, answer)
	}
	return d, a
}

func answerMsg(id uint32, prompt, choice string) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "work", Window: "@1", Runtime: mirror.RuntimeHermes,
		HermesApproval: &mirror.HermesApprovalAnswer{PromptID: prompt, Choice: choice}}
}

func answeredOf(t *testing.T, d *drivenMuster, id uint32) mirror.Msg {
	t.Helper()
	var got mirror.Msg
	waitUntil(t, "the answered", func() bool {
		for _, m := range d.of(mirror.TypeAnswered) {
			if m.ID == id {
				got = m
				return true
			}
		}
		return false
	})
	return got
}

// 🔴 The host's own half, whatever muster sends: without -hermes-answer,
// nothing; with it, only the approval its viewer holds for the window's pane
// NOW waiting on an approval, with once or deny.
func TestTheHostAnswersOnlyAnApprovalItHolds(t *testing.T) {
	t.Run("without -hermes-answer", func(t *testing.T) {
		d, a := runHermesHost(t, "approval", false)
		d.send(t, answerMsg(1, "p1", "deny"))
		if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalUnsupported || m.Error == "" {
			t.Fatalf("%+v", m)
		}
		if a.attaches() != 0 {
			t.Fatal("a viewer was attached on a host that does not answer")
		}
	})
	t.Run("a window waiting on sudo", func(t *testing.T) {
		d, a := runHermesHost(t, "sudo", true)
		d.send(t, answerMsg(1, "p1", "deny"))
		if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalHermesGone {
			t.Fatalf("%+v", m)
		}
		if a.attaches() != 0 {
			t.Fatal("a viewer was attached for a sudo prompt")
		}
	})
	t.Run("an approval", func(t *testing.T) {
		d, a := runHermesHost(t, "approval", true)
		waitUntil(t, "the viewer", func() bool { return a.last() != nil })
		waitUntil(t, "the listed approval", func() bool {
			ms := d.of(mirror.TypeSessions)
			for i := len(ms) - 1; i >= 0; i-- {
				for _, w := range ms[i].Windows {
					if w.Pending != nil && w.Pending.PromptID == "p1" {
						return true
					}
				}
			}
			return false
		})
		for i, c := range []struct{ prompt, choice string }{{"p2", "deny"}, {"p1", "always"}, {"p1", "session"}} {
			d.send(t, answerMsg(uint32(10+i), c.prompt, c.choice))
			if m := answeredOf(t, d, uint32(10+i)); m.Refusal != mirror.AskRefusalHermesGone {
				t.Fatalf("%s %s: %+v", c.prompt, c.choice, m)
			}
		}
		// Not a Hermes answer (no Runtime): Claude Code's picker path, which
		// has nothing to type — never the viewer.
		m := answerMsg(20, "p1", "deny")
		m.Runtime = ""
		d.send(t, m)
		if r := answeredOf(t, d, 20); r.Error == "" {
			t.Fatalf("a runtime-less answer went through: %+v", r)
		}
		if got := a.last().sent(); len(got) != 0 {
			t.Fatalf("the viewer was answered %v by a refused request", got)
		}
		d.send(t, answerMsg(30, "p1", "deny"))
		if r := answeredOf(t, d, 30); r.Error != "" {
			t.Fatalf("%+v", r)
		}
		if got := a.last().sent(); len(got) != 1 || got[0] != "p1 deny" {
			t.Fatalf("the viewer got %v", got)
		}
	})
}
