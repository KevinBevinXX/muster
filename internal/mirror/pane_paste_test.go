package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/stepup"
)

// PASTING INTO ANY PANE (the Terminal view's composer): a real mirror.Service
// and a real muster-mirror client; only tmux is played.
//
// paneBackend is claudeBackend plus mirrorhost.PaneBackend — a current
// muster-mirror's tmux, which can type into a session's active pane whatever
// runs there. Every session's active pane is "%a-<session>", except a Claude
// session's, which is its Claude pane (as on a real host, where ClaudePanes
// reads the same active pane). claudeBackend ALONE — no PaneBackend — is an
// OLDER muster-mirror: it advertises nothing and pastes only into Claude.

type paneBackend struct {
	*claudeBackend
}

func (b paneBackend) ActivePane(ctx context.Context, session string) (string, bool) {
	if p, ok := b.ClaudePanes(ctx, []string{session})[session]; ok {
		return p.Pane, true
	}
	list, _ := b.List(ctx)
	if !slices.Contains(list, session) {
		return "", false
	}
	return "%a-" + session, true
}

func (b paneBackend) PastePane(_ context.Context, pane, text string) error {
	b.claudeBackend.mu.Lock()
	b.pastes = append(b.pastes, "pane "+pane+":"+text)
	b.claudeBackend.mu.Unlock()
	return nil
}

// noSignInTime is user id's principal with NO sign-in time on it: never fresh.
func noSignInTime(id int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: id, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// setWindow configures the step-up window (a Go duration) for one test and
// restores the default. No test in this package runs in parallel.
func setWindow(t *testing.T, v string) {
	t.Helper()
	d, err := stepup.ParseWindow(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := stepup.SetWindow(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stepup.SetWindow(stepup.DefaultWindow) })
}

// hostOf is owner 1's view of host.
func hostOf(t *testing.T, r *rig, host string) mirror.HostView {
	t.Helper()
	for _, h := range r.svc.Hosts(asUser(1)) {
		if h.Host == host {
			return h
		}
	}
	t.Fatalf("host %q is not in user 1's view", host)
	return mirror.HostView{}
}

// claudeHost connects a host with sessions main (Claude Code, pane %7) and
// shell (not), and waits for the Claude pane to be reported. current selects a
// muster-mirror that can paste into any pane; false is an older one.
func claudeHost(t *testing.T, r *rig, host string, interactive, current bool) *claudeBackend {
	t.Helper()
	tok, _ := r.store.mint(t, 1, host)
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
	var backend mirrorhost.Backend = b
	if current {
		backend = paneBackend{b}
	}
	r.connect(t, host, tok, backend, interactive)
	eventually(t, host+" lists its Claude pane", func() bool {
		for _, h := range r.svc.Hosts(asUser(1)) {
			if h.Host == host {
				return h.Online && len(h.Claude) == 1
			}
		}
		return false
	})
	return b
}

// TestTheActivePaneOfANonClaudeSessionCanBePastedInto is the feature: on a
// current, -interactive host a PasteActivePane lands in the session's active
// pane whether or not Claude Code runs there — and 🔴 a PasteClaude into the
// same non-Claude session is STILL refused and pastes nothing, so the rendered
// view's send can never be turned into "type this into the shell".
func TestTheActivePaneOfANonClaudeSessionCanBePastedInto(t *testing.T) {
	r := newRig(t)
	b := claudeHost(t, r, "rw", true, true)
	if h := hostOf(t, r, "rw"); !h.PanePaste || !h.CanPaste("shell") || !h.CanPaste("main") {
		t.Fatalf("a current interactive host: PanePaste=%v CanPaste(shell)=%v CanPaste(main)=%v, want all true", h.PanePaste, h.CanPaste("shell"), h.CanPaste("main"))
	}

	if err := r.svc.Paste(asUser(1), "rw", "shell", "ls -la", mirror.PasteActivePane); err != nil {
		t.Fatalf("PasteActivePane into a shell session: %v", err)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"pane %a-shell:ls -la"}) {
		t.Fatalf("pasted %q, want the shell session's active pane", got)
	}

	// 🔴 The Claude-only target has not widened.
	if err := r.svc.Paste(asUser(1), "rw", "shell", "a prompt for claude", mirror.PasteClaude); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("PasteClaude into a non-Claude session = %v, want ErrNotClaude", err)
	}
	if got := b.pasted(); len(got) != 1 {
		t.Fatalf("a refused PasteClaude reached the host: %q", got)
	}

	// The active pane of a Claude session IS its Claude pane; multi-line text
	// arrives as one paste with CRLF folded.
	if err := r.svc.Paste(asUser(1), "rw", "main", "one\r\ntwo", mirror.PasteActivePane); err != nil {
		t.Fatalf("PasteActivePane into the Claude session: %v", err)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"pane %a-shell:ls -la", "pane %7:one\ntwo"}) {
		t.Fatalf("pasted %q", got)
	}
}

// TestAnActivePanePasteKeepsEveryGateOfAClaudePaste: the new target passes
// through the same gates, in the plugin itself — a fresh sign-in (stale and
// timeless contexts refused before the host is asked), the host's
// -interactive, the owner, and clean text.
func TestAnActivePanePasteKeepsEveryGateOfAClaudePaste(t *testing.T) {
	r := newRig(t)
	b := claudeHost(t, r, "rw", true, true)
	ro := claudeHost(t, r, "ro", false, true)

	if h := hostOf(t, r, "ro"); h.PanePaste || h.CanPaste("shell") || h.CanPaste("main") {
		t.Fatalf("a read-only host advertises PanePaste=%v CanPaste=%v/%v", h.PanePaste, h.CanPaste("shell"), h.CanPaste("main"))
	}
	for name, tc := range map[string]struct {
		ctx     context.Context
		host    string
		session string
		text    string
		want    error
	}{
		"a stale sign-in":     {asStaleUser(1), "rw", "shell", "ls", mirror.ErrStepUpRequired},
		"no sign-in time":     {noSignInTime(1), "rw", "shell", "ls", mirror.ErrStepUpRequired},
		"a read-only host":    {asUser(1), "ro", "shell", "ls", mirror.ErrInteractiveRefused},
		"another owner":       {asUser(2), "rw", "shell", "ls", mirror.ErrHostOffline},
		"no principal":        {context.Background(), "rw", "shell", "ls", mirror.ErrNoPrincipal},
		"an escape sequence":  {asUser(1), "rw", "shell", "x\x1b[201~rm -rf /", mirror.ErrBadPaste},
		"an unlisted session": {asUser(1), "rw", "gone", "ls", mirror.ErrSessionGone},
	} {
		if err := r.svc.Paste(tc.ctx, tc.host, tc.session, tc.text, mirror.PasteActivePane); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if got := append(b.pasted(), ro.pasted()...); len(got) != 0 {
		t.Fatalf("a refused paste reached a host: %q", got)
	}
	// Control: the same call, fresh, on the interactive host, lands.
	if err := r.svc.Paste(asUser(1), "rw", "shell", "ls", mirror.PasteActivePane); err != nil || len(b.pasted()) != 1 {
		t.Fatalf("control: %v, pasted %q", err, b.pasted())
	}
}

// TestAnOlderMirrorIsNeverAskedForAnyPane: a muster-mirror that predates
// Msg.PanePaste advertises nothing. For its Claude session PasteActivePane
// falls back to the Claude-only paste (the same pane); for any other session it
// is refused HERE, with the reason, and the host is never asked.
func TestAnOlderMirrorIsNeverAskedForAnyPane(t *testing.T) {
	r := newRig(t)
	b := claudeHost(t, r, "old", true, false)
	h := hostOf(t, r, "old")
	if h.PanePaste || h.CanPaste("shell") || !h.CanPaste("main") {
		t.Fatalf("an older host: PanePaste=%v CanPaste(shell)=%v CanPaste(main)=%v, want false false true", h.PanePaste, h.CanPaste("shell"), h.CanPaste("main"))
	}
	err := r.svc.Paste(asUser(1), "old", "shell", "ls", mirror.PasteActivePane)
	if !errors.Is(err, mirror.ErrPanePasteUnsupported) || !mirror.IsAttachRefusal(err) || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("PasteActivePane into an older host's shell = %v, want ErrPanePasteUnsupported naming the upgrade", err)
	}
	if got := b.pasted(); len(got) != 0 {
		t.Fatalf("the older host was asked: %q", got)
	}
	if err := r.svc.Paste(asUser(1), "old", "main", "hi", mirror.PasteActivePane); err != nil {
		t.Fatalf("PasteActivePane into an older host's Claude session: %v", err)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"%7:hi"}) {
		t.Fatalf("pasted %q, want the Claude-only paste into %%7", got)
	}
}

// TestTheHostDecidesThePaneAndRefusesWhatItDidNotOptInto: the HOST's own rules,
// with muster misbehaving (a hand-driven muster sends the frames).
//   - AnyPane on a read-only host: refused, nothing pasted — it never said yes.
//   - no AnyPane on a non-Claude session of an interactive host: refused — the
//     pre-existing Claude-only rule is what an absent field means.
//   - AnyPane on an interactive host: the active pane.
//   - AnyPane for a session the host does not list, or with a control
//     character, or on a backend that cannot paste into a pane: refused.
func TestTheHostDecidesThePaneAndRefusesWhatItDidNotOptInto(t *testing.T) {
	newB := func() *claudeBackend {
		tr := newTranscript(t, promptLine("x"))
		b := newClaudeBackend("main", "shell")
		b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
		return b
	}
	b := newB()
	m := driveV2(t, false, paneBackend{b}, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "ls", AnyPane: true})
	if m.Type != mirror.TypePasted || !strings.Contains(m.Error, "read-only") || len(b.pasted()) != 0 {
		t.Fatalf("AnyPane on a read-only host: %+v, pasted %q", m, b.pasted())
	}
	b = newB()
	m = driveV2(t, true, paneBackend{b}, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "ls"})
	if m.Type != mirror.TypePasted || !strings.Contains(m.Error, "Claude Code is not running") || len(b.pasted()) != 0 {
		t.Fatalf("a Claude-only paste into a shell session: %+v, pasted %q", m, b.pasted())
	}
	b = newB()
	m = driveV2(t, true, paneBackend{b}, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "ls\r\nwhoami", AnyPane: true})
	if m.Type != mirror.TypePasted || m.Error != "" || !slices.Equal(b.pasted(), []string{"pane %a-shell:ls\nwhoami"}) {
		t.Fatalf("AnyPane on an interactive host: %+v, pasted %q", m, b.pasted())
	}
	b = newB()
	m = driveV2(t, true, paneBackend{b}, mirror.Msg{Type: mirror.TypePaste, Session: "elsewhere", Text: "ls", AnyPane: true})
	if m.Type != mirror.TypePasted || !strings.Contains(m.Error, "not in this host's latest list") || len(b.pasted()) != 0 {
		t.Fatalf("AnyPane for an unlisted session: %+v, pasted %q", m, b.pasted())
	}
	b = newB()
	m = driveV2(t, true, paneBackend{b}, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "x\x1b[201~y", AnyPane: true})
	if m.Type != mirror.TypePasted || m.Error == "" || len(b.pasted()) != 0 {
		t.Fatalf("AnyPane with a control character: %+v, pasted %q", m, b.pasted())
	}
	b = newB()
	m = driveV2(t, true, b, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "ls", AnyPane: true})
	if m.Type != mirror.TypePasted || m.Error == "" || len(b.pasted()) != 0 {
		t.Fatalf("AnyPane on a backend without PaneBackend: %+v, pasted %q", m, b.pasted())
	}
}

// TestTheStepUpRefusalNamesTheConfiguredWindow: mirror.ErrStepUpRequired is
// stepup.ErrStale under this package's prefix, and its text follows the
// window — it is not frozen at whatever the window was when the package loaded.
func TestTheStepUpRefusalNamesTheConfiguredWindow(t *testing.T) {
	if got := mirror.ErrStepUpRequired.Error(); !strings.HasPrefix(got, "mirror: confirm it's you") || !strings.HasSuffix(got, "the last 12 hours") {
		t.Fatalf("default refusal = %q", got)
	}
	if !errors.Is(mirror.ErrStepUpRequired, stepup.ErrStale) {
		t.Fatal("ErrStepUpRequired no longer unwraps to stepup.ErrStale")
	}
	setWindow(t, "45m")
	if got := mirror.ErrStepUpRequired.Error(); !strings.HasSuffix(got, "the last 45 minutes") {
		t.Fatalf("refusal under a 45 minute window = %q", got)
	}
}

// TestAReadOnlyHostClaimingPanePasteIsNotBelieved: the hello is the HOST's
// word, and a host that says "paste into any pane" without -interactive (no
// muster-mirror build does; a hand-driven host here) is listed as unable to be
// pasted into, and is never asked.
func TestAReadOnlyHostClaimingPanePasteIsNotBelieved(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "liar")
	h := hello("liar", false, "shell")
	h.PanePaste = true
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "liar", "shell")
	if v := hostOf(t, r, "liar"); v.PanePaste || v.CanPaste("shell") {
		t.Fatalf("a read-only host's PanePaste claim was believed: %+v", v)
	}
	if err := r.svc.Paste(asUser(1), "liar", "shell", "ls", mirror.PasteActivePane); !errors.Is(err, mirror.ErrInteractiveRefused) {
		t.Fatalf("paste = %v, want ErrInteractiveRefused", err)
	}
	raw.mu.Lock()
	defer raw.mu.Unlock()
	for _, m := range raw.text {
		if m.Type == mirror.TypePaste {
			t.Fatalf("the read-only host was sent a paste: %+v", m)
		}
	}

	// Control: the same hello WITH -interactive is believed and is asked.
	tok2, _ := r.store.mint(t, 1, "honest")
	h2 := hello("honest", true, "shell")
	h2.PanePaste = true
	raw2, err := r.rawConnect(t, tok2, h2)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "honest", "shell")
	if v := hostOf(t, r, "honest"); !v.PanePaste || !v.CanPaste("shell") {
		t.Fatalf("control: an interactive host's PanePaste was not believed: %+v", v)
	}
	go func() { _ = r.svc.Paste(asUser(1), "honest", "shell", "ls", mirror.PasteActivePane) }()
	eventually(t, "the interactive host to be sent an AnyPane paste", func() bool {
		raw2.mu.Lock()
		defer raw2.mu.Unlock()
		for _, m := range raw2.text {
			if m.Type == mirror.TypePaste && m.AnyPane && m.Session == "shell" && m.Text == "ls" {
				return true
			}
		}
		return false
	})
}

// helloOf is the hello a REAL muster-mirror sends for this configuration, read
// by a hand-driven muster.
func helloOf(t *testing.T, interactive bool, b mirrorhost.Backend) mirror.Msg {
	t.Helper()
	got := make(chan mirror.Msg, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var m mirror.Msg
		_ = json.Unmarshal(data, &m)
		got <- m
	}))
	t.Cleanup(srv.Close)
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: b, Interactive: interactive, AllowHTTP: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case m := <-got:
		return m
	case <-time.After(6 * time.Second):
		t.Fatal("the host never sent its hello")
		return mirror.Msg{}
	}
}

// TestAHostAdvertisesPanePasteOnlyWhenItWouldAcceptOne: what muster-mirror
// itself puts in its hello. It says PanePaste only when started -interactive
// AND its backend can paste into a pane — a read-only host does not advertise
// what it would refuse, and a backend without the capability does not claim it.
func TestAHostAdvertisesPanePasteOnlyWhenItWouldAcceptOne(t *testing.T) {
	for name, tc := range map[string]struct {
		interactive bool
		current     bool
		want        bool
	}{
		"interactive, can paste into a pane": {true, true, true},
		"read-only, can paste into a pane":   {false, true, false},
		"interactive, an older backend":      {true, false, false},
	} {
		b := newClaudeBackend("main", "shell")
		var backend mirrorhost.Backend = b
		if tc.current {
			backend = paneBackend{b}
		}
		m := helloOf(t, tc.interactive, backend)
		if m.Type != mirror.TypeHello || m.Interactive != tc.interactive {
			t.Fatalf("%s: fixture: hello = %+v", name, m)
		}
		if m.PanePaste != tc.want {
			t.Errorf("%s: the hello says PanePaste=%v, want %v", name, m.PanePaste, tc.want)
		}
	}
}
