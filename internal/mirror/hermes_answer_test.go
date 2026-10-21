package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// ANSWERING A HERMES APPROVAL (Hermes Phase 4): the hub sends it only to a
// host that advertised hermesAnswer, -interactive, for the approval its
// window lists with a choice it offers, behind a fresh sign-in.

const ansSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// approvalHello is a hello listing work:@1, a Hermes window waiting on the
// approval p1 (offering choices), and work:@2, a Claude Code window.
func approvalHello(extra, choices string) string {
	w := []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: ansSID,
			Status: mirror.StatusWaiting, StatusSince: 1792000001111,
			Pending: &mirror.Pending{Kind: mirror.PendingApproval, Summary: "terminal: rm -r ./build", Tool: "terminal", PromptID: "p1", Choices: choices}},
		{Session: "work", ID: "@2", Index: 1, Claude: true, Status: mirror.StatusIdle, StatusSince: 1},
	}
	b, _ := json.Marshal(w)
	return `{"type":"hello","version":2,"host":"on","interactive":true,"panePaste":true,"statusReport":true` + extra + `,"windows":` + string(b) + `}`
}

// answerHost is a JSON host whose reply to each answer is reply's.
type answerHost struct {
	mu      sync.Mutex
	answers []mirror.Msg
}

func (h *answerHost) got() []mirror.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mirror.Msg(nil), h.answers...)
}

func connectAnswerHost(t *testing.T, r *rig, hello string, reply func(mirror.Msg) mirror.Msg) *answerHost {
	t.Helper()
	tok, _ := r.store.mint(t, 1, "on")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+tok)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	h := &answerHost{}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypeAnswer {
				continue
			}
			h.mu.Lock()
			h.answers = append(h.answers, m)
			h.mu.Unlock()
			a := reply(m)
			a.Type, a.ID = mirror.TypeAnswered, m.ID
			b, _ := json.Marshal(a)
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	r.online(t, 1, "on", "work")
	return h
}

func TestAHermesApprovalAnswerReachesItsHost(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	h := connectAnswerHost(t, r, approvalHello(`,"hermesAnswer":true`, "once deny"), func(mirror.Msg) mirror.Msg { return mirror.Msg{} })
	if hv := r.svc.Hosts(ctx); len(hv) != 1 || !hv[0].HermesAnswer {
		t.Fatalf("the host's hermesAnswer was not believed: %+v", hv)
	}
	if err := r.svc.AnswerHermesApproval(ctx, "on", "work", "@1", "p1", "deny"); err != nil {
		t.Fatal(err)
	}
	got := h.got()
	if len(got) != 1 {
		t.Fatalf("the host received %d answers", len(got))
	}
	m := got[0]
	if m.Runtime != mirror.RuntimeHermes || m.Window != "@1" || m.Ask != nil || m.HermesApproval == nil ||
		*m.HermesApproval != (mirror.HermesApprovalAnswer{PromptID: "p1", Choice: "deny"}) {
		t.Fatalf("the answer: %+v (%+v)", m, m.HermesApproval)
	}
}

// Every refusal the hub can make is made BEFORE anything reaches the host.
func TestAHermesApprovalAnswerIsRefusedBeforeTheHost(t *testing.T) {
	for _, c := range []struct {
		name, extra, choices, window, prompt, choice string
		ctx                                          func() context.Context
		want                                         error
	}{
		{"no hermesAnswer", ``, "once deny", "@1", "p1", "deny", nil, mirror.ErrHermesAnswerUnsupported},
		{"read-only host", `,"hermesAnswer":true,"interactive":false`, "once deny", "@1", "p1", "deny", nil, mirror.ErrHermesAnswerUnsupported},
		{"a stale sign-in", `,"hermesAnswer":true`, "once deny", "@1", "p1", "deny", func() context.Context { return asStaleUser(1) }, mirror.ErrStepUpRequired},
		{"another approval", `,"hermesAnswer":true`, "once deny", "@1", "p2", "deny", nil, mirror.ErrHermesApprovalGone},
		{"a choice not offered", `,"hermesAnswer":true`, "deny", "@1", "p1", "once", nil, mirror.ErrHermesApprovalGone},
		{"always", `,"hermesAnswer":true`, "once deny", "@1", "p1", "always", nil, mirror.ErrAskNotSent},
		{"session", `,"hermesAnswer":true`, "once deny", "@1", "p1", "session", nil, mirror.ErrAskNotSent},
		{"a Claude window", `,"hermesAnswer":true`, "once deny", "@2", "p1", "deny", nil, mirror.ErrHermesApprovalGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			base := asUser(1)
			if c.ctx != nil {
				base = c.ctx()
			}
			ctx, cancel := context.WithTimeout(base, 5*time.Second)
			defer cancel()
			hello := approvalHello(c.extra, c.choices)
			if strings.Contains(c.extra, `"interactive":false`) {
				hello = strings.Replace(hello, `"interactive":true,`, "", 1)
			}
			h := connectAnswerHost(t, r, hello, func(mirror.Msg) mirror.Msg { return mirror.Msg{} })
			if hv := r.svc.Hosts(ctx); len(hv) == 1 && hv[0].HermesAnswer && (!hv[0].Interactive || !strings.Contains(c.extra, "hermesAnswer")) {
				t.Fatalf("the host is shown as answering Hermes approvals: %+v", hv[0])
			}
			err := r.svc.AnswerHermesApproval(ctx, "on", "work", c.window, c.prompt, c.choice)
			if !errors.Is(err, c.want) || !errors.Is(err, mirror.ErrAskNotSent) || !mirror.AskOutcomeKnown(err) {
				t.Fatalf("err = %v, want %v (not sent)", err, c.want)
			}
			time.Sleep(30 * time.Millisecond)
			if got := h.got(); len(got) != 0 {
				t.Fatalf("the host received %+v", got)
			}
		})
	}
}

// The host's refusals come back as muster's own.
func TestAHermesApprovalAnswersHostRefusals(t *testing.T) {
	for refusal, want := range map[string]error{
		mirror.AskRefusalHermesGone:        mirror.ErrHermesApprovalGone,
		mirror.AskRefusalHermesUnreachable: mirror.ErrHermesUnreachable,
		mirror.AskRefusalHermesUnknown:     mirror.ErrHermesAnswerUnknown,
	} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		connectAnswerHost(t, r, approvalHello(`,"hermesAnswer":true`, "once deny"), func(mirror.Msg) mirror.Msg {
			return mirror.Msg{Error: "the host's words", Refusal: refusal}
		})
		err := r.svc.AnswerHermesApproval(ctx, "on", "work", "@1", "p1", "once")
		cancel()
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "the host's words") {
			t.Errorf("%s: %v, want %v", refusal, err, want)
		}
		if known := mirror.AskOutcomeKnown(err); known != (refusal != mirror.AskRefusalHermesUnknown) {
			t.Errorf("%s: AskOutcomeKnown = %v", refusal, known)
		}
	}
}

// The wire: a Hermes window's Pending keeps its ask; a Claude window's loses
// the Hermes fields; choices are muster's, and only with a prompt id.
func TestAHermesWindowsPendingOnTheWire(t *testing.T) {
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "approval", Summary: "terminal: ls", PromptID: "p1", Choices: "always session deny once", Detail: "why\x1b[31m"}},
		{Session: "s", ID: "@2", Claude: true, Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "approval", Summary: "Bash: ls", PromptID: "p1", Choices: "once", Detail: "x"}},
		{Session: "s", ID: "@3", Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "approval", Summary: "x", PromptID: "not a/valid id", Choices: "once deny"}},
		{Session: "s", ID: "@4", Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "telepathy", Summary: "x"}},
		{Session: "s", ID: "@5", Runtime: mirror.RuntimeHermes, Status: mirror.StatusBusy,
			Pending: &mirror.Pending{Kind: "sudo", Summary: "x"}},
	}, 0)
	if p := got[0].Pending; p == nil || *p != (mirror.Pending{Kind: "approval", Summary: "terminal: ls", PromptID: "p1", Choices: "once deny", Detail: "why [31m"}) {
		t.Errorf("Hermes: %+v", p)
	}
	if p := got[1].Pending; p == nil || *p != (mirror.Pending{Summary: "Bash: ls"}) {
		t.Errorf("Claude: %+v", p)
	}
	if p := got[2].Pending; p == nil || p.PromptID != "" || p.Choices != "" {
		t.Errorf("an invalid prompt id: %+v", p)
	}
	if got[3].Pending != nil || got[4].Pending != nil {
		t.Errorf("an unknown kind, or not waiting: %+v %+v", got[3].Pending, got[4].Pending)
	}
}

// ---------------------------------------------------------------- end to end

// e2eAttacher is a backend's HermesViewerAttacher whose viewer records
// answers.
type e2eHermesBackend struct {
	*fakeBackend
	src     *mirrorhost.HermesSource
	mu      sync.Mutex
	answers []string
}

func (b *e2eHermesBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{{Session: "work", ID: "@1", Active: true, Pane: "%1", Command: "python3.14",
		Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%1", Gen: ansSID, Status: mirror.StatusWaiting, StatusSince: 7,
			Hermes: &hermeshost.PaneSession{Session: ansSID, Status: hermeshost.Believed{Status: mirror.StatusWaiting, Since: 7, Kind: "approval"}}}}}, nil
}
func (b *e2eHermesBackend) HermesSource() *mirrorhost.HermesSource { return b.src }
func (b *e2eHermesBackend) AttachHermesViewer(context.Context, string) (mirrorhost.HermesViewer, []hermeshost.ApprovalPrompt, error) {
	return &e2eViewer{b: b, done: make(chan struct{}), events: make(chan hermeshost.ViewerEvent)},
		[]hermeshost.ApprovalPrompt{{PromptID: "p1", Generation: 1, Command: "rm -r ./build", Description: "recursive delete", Choices: []string{"once", "deny", "session", "always"}}}, nil
}

type e2eViewer struct {
	b      *e2eHermesBackend
	done   chan struct{}
	events chan hermeshost.ViewerEvent
	once   sync.Once
}

func (v *e2eViewer) Respond(_ context.Context, p hermeshost.ApprovalPrompt, c string) error {
	v.b.mu.Lock()
	defer v.b.mu.Unlock()
	v.b.answers = append(v.b.answers, p.PromptID+" "+c)
	return nil
}
func (v *e2eViewer) Events() <-chan hermeshost.ViewerEvent { return v.events }
func (v *e2eViewer) Done() <-chan struct{}                 { return v.done }
func (v *e2eViewer) Close()                                { v.once.Do(func() { close(v.done) }) }

// The whole path with a REAL muster-mirror client: the host lists the
// approval its viewer holds, muster answers it, the viewer gets the answer —
// and a host without -hermes-answer lists the words alone and advertises
// nothing.
func TestAHermesApprovalAnsweredEndToEnd(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 10*time.Second)
		tok, _ := r.store.mint(t, 1, "on")
		// The conversation holds the pending terminal call the approval is for:
		// the host offers "once" only for a command that IS one.
		hh := hermestest.New(t)
		hh.Session(ansSID, hermestest.SessionOpts{})
		hh.Message(ansSID, hermestest.Msg{Role: "assistant", ToolCalls: `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"rm -r ./build\"}"}}]`})
		b := &e2eHermesBackend{fakeBackend: &fakeBackend{}, src: mirrorhost.NewHermesSource(hh.Dir, t.TempDir())}
		c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "on", Backend: b, Interactive: true, HermesAnswer: on,
			AllowHTTP: true, Refresh: 30 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
		if err != nil {
			t.Fatal(err)
		}
		rctx, stop := context.WithCancel(context.Background())
		go func() { _ = c.RunOnce(rctx) }()
		pending := func() *mirror.Pending {
			for _, h := range r.svc.Hosts(ctx) {
				for _, w := range h.Windows {
					if w.ID == "@1" {
						return w.Pending
					}
				}
			}
			return nil
		}
		if on {
			eventually(t, "the approval listed", func() bool { p := pending(); return p != nil && p.PromptID == "p1" })
			if p := pending(); p.Summary != "terminal: rm -r ./build" || p.Choices != "once deny" || p.Detail != "recursive delete" {
				t.Fatalf("%+v", p)
			}
			if err := r.svc.AnswerHermesApproval(ctx, "on", "work", "@1", "p1", "once"); err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			got := strings.Join(b.answers, ",")
			b.mu.Unlock()
			if got != "p1 once" {
				t.Fatalf("the viewer got %q", got)
			}
		} else {
			eventually(t, "the window listed", func() bool { return pending() != nil })
			if p := pending(); p.PromptID != "" || p.Summary != mirrorhost.HermesApprovalText {
				t.Fatalf("without -hermes-answer: %+v", p)
			}
			if err := r.svc.AnswerHermesApproval(ctx, "on", "work", "@1", "p1", "deny"); !errors.Is(err, mirror.ErrHermesAnswerUnsupported) {
				t.Fatalf("err = %v", err)
			}
		}
		stop()
		cancel()
	}
}
