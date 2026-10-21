package mirrorhost

import (
	"context"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// hermesSendBackend is a windows backend that also makes the gated send — a
// fake that records each request and answers what the test says.
type hermesSendBackend struct {
	*windowsBackend
	mu   sync.Mutex
	reqs []HermesSendRequest
	ans  error
}

func (b *hermesSendBackend) SendHermes(ctx context.Context, req HermesSendRequest) error {
	ap, err := req.Status(ctx)
	if err != nil {
		return err
	}
	if ap.Pane != req.Pane {
		return notSubmitted("pane moved")
	}
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	return b.ans
}

func (b *hermesSendBackend) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, r := range b.reqs {
		out = append(out, r.Pane+" "+r.Text)
	}
	return out
}

// runHost is drivenMuster.run with the -hermes-send switch.
func (d *drivenMuster) runHost(t *testing.T, b Backend, interactive, hermesSend bool) {
	t.Helper()
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b,
		Interactive: interactive, HermesSend: hermesSend, AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
}

var hermesWindows = []WindowInfo{
	{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1, Command: "zsh"},
	{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3, Command: "python3.14",
		Agent: &AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%30", Gen: "local-root", Status: mirror.StatusIdle, StatusSince: fakeSince}},
}

// pasteReply sends m and waits for the host's pasted answer.
func pasteReply(t *testing.T, d *drivenMuster, m mirror.Msg) mirror.Msg {
	t.Helper()
	d.send(t, m)
	var out mirror.Msg
	waitUntil(t, "the host's answer", func() bool {
		for _, a := range d.of(mirror.TypePasted) {
			if a.ID == m.ID {
				out = a
				return true
			}
		}
		return false
	})
	return out
}

// TestTheHostSwitchGatesTheHermesSend is the hermesSend switch's HOST half:
// without -hermes-send (or without -interactive) the host does not advertise
// it, and refuses a Hermes paste whatever muster sent, before the backend's
// gated send is ever called; with both, it advertises it and the paste goes
// to the gated send with the server's since and the text.
func TestTheHostSwitchGatesTheHermesSend(t *testing.T) {
	for _, c := range []struct {
		name                    string
		interactive, hermesSend bool
		advertise               bool
		why                     string
	}{
		{"off", true, false, false, "-hermes-send"},
		{"read-only", false, true, false, "-interactive"},
		{"on", true, true, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &hermesSendBackend{windowsBackend: &windowsBackend{windows: hermesWindows}}
			d := newDrivenMuster(t)
			d.runHost(t, wrapped{b}, c.interactive, c.hermesSend)
			if got := d.of(mirror.TypeHello)[0].HermesSend; got != c.advertise {
				t.Fatalf("hello.HermesSend = %t, want %t", got, c.advertise)
			}
			a := pasteReply(t, d, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "solo", Window: "@9", Text: "hello hermes", Runtime: mirror.RuntimeHermes, Since: fakeSince})
			if c.why != "" {
				if a.Refusal != mirror.PasteRefusalHermesNotSubmitted || !strings.Contains(a.Error, c.why) {
					t.Fatalf("refusal %q %q", a.Refusal, a.Error)
				}
				if got := b.sent(); len(got) != 0 {
					t.Fatalf("🔴 the gated send ran with the switch off: %q", got)
				}
				return
			}
			if a.Error != "" || a.Refusal != "" {
				t.Fatalf("refused: %q %q", a.Refusal, a.Error)
			}
			if got := b.sent(); !slices.Equal(got, []string{"%30 hello hermes"}) {
				t.Fatalf("sent %q", got)
			}
			if b.reqs[0].Since != fakeSince {
				t.Fatalf("since %d", b.reqs[0].Since)
			}
		})
	}
}

// TestAHermesPasteGoesOnlyToAHermesWindow: Runtime "hermes" aimed at a window
// that runs no Hermes now is refused (superseded), and the gated send never
// runs; a host whose backend cannot make the gated send does not advertise it.
func TestAHermesPasteGoesOnlyToAHermesWindow(t *testing.T) {
	b := &hermesSendBackend{windowsBackend: &windowsBackend{windows: hermesWindows}}
	d := newDrivenMuster(t)
	d.runHost(t, b, true, true)
	a := pasteReply(t, d, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "scratch2", Window: "@4", Text: "ls", Runtime: mirror.RuntimeHermes, Since: fakeSince})
	if a.Refusal != mirror.PasteRefusalHermesSuperseded || len(b.sent()) != 0 {
		t.Fatalf("into a shell: %q %q, sent %q", a.Refusal, a.Error, b.sent())
	}
	// The refusal kinds the gated send names reach muster as they are.
	b.ans = &HermesSendError{Kind: HermesUnconfirmed, Why: "no row"}
	a = pasteReply(t, d, mirror.Msg{Type: mirror.TypePaste, ID: 2, Session: "solo", Window: "@9", Text: "hi", Runtime: mirror.RuntimeHermes, Since: fakeSince})
	if a.Refusal != mirror.PasteRefusalHermesUnconfirmed || !strings.Contains(a.Error, "May have been submitted") {
		t.Fatalf("unconfirmed: %q %q", a.Refusal, a.Error)
	}

	plain := newDrivenMuster(t)
	plain.runHost(t, &windowsBackend{windows: hermesWindows}, true, true)
	if plain.of(mirror.TypeHello)[0].HermesSend {
		t.Fatal("a backend with no gated send advertised hermesSend")
	}
}
