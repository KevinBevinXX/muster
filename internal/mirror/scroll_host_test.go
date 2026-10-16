package mirror_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// driveScroll runs a REAL muster-mirror (interactive or not) over b against a
// HAND-DRIVEN muster that sends one scroll — so the host's own gates are tested
// with muster misbehaving — and returns the host's scrolled (Error "" = done).
func driveScroll(t *testing.T, interactive bool, b mirrorhost.Backend, scroll mirror.Msg) mirror.Msg {
	t.Helper()
	got := make(chan mirror.Msg, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil { // hello
			return
		}
		write := func(m mirror.Msg) {
			buf, _ := json.Marshal(m)
			_ = ws.Write(ctx, websocket.MessageText, buf)
		}
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: mirror.ProtocolVersion, Host: "laptop"})
		scroll.Type, scroll.ID = mirror.TypeScroll, 41
		write(scroll)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				got <- mirror.Msg{Error: "(no answer)"}
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == mirror.TypeScrolled && m.ID == 41 {
				got <- m
				return
			}
		}
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
	case e := <-got:
		return e
	case <-time.After(6 * time.Second):
		t.Fatal("the hand-driven muster never heard back")
		return mirror.Msg{}
	}
}

// TestTheHostRefusesAScrollItDidNotOptInto: whatever muster sends, the host
// scrolls only as an -interactive host, only a session it listed, and only a
// non-zero count — each refusal before tmux is touched. The control scrolls.
func TestTheHostRefusesAScrollItDidNotOptInto(t *testing.T) {
	for name, tc := range map[string]struct {
		interactive bool
		session     string
		lines       int
		want        string
		refusal     string
	}{
		// Only "this host cannot scroll" is named unsupported: it is the one a
		// page stops asking after.
		"a read-only host":    {false, "shell", 3, "read-only", "unsupported"},
		"an unlisted session": {true, "secret", 3, "not in this host's latest list", ""},
		"zero lines":          {true, "shell", 0, "at least one line", ""},
		"control":             {true, "shell", 3, "", ""},
	} {
		b := newScrollingBackend("main", "shell")
		ans := driveScroll(t, tc.interactive, b, mirror.Msg{Session: tc.session, Lines: tc.lines})
		got := ans.Error
		if ans.Refusal != tc.refusal {
			t.Errorf("%s: the refusal is named %q, want %q", name, ans.Refusal, tc.refusal)
		}
		calls, _, _ := b.snapshot()
		if tc.want == "" {
			if got != "" || !slices.Equal(calls, []string{"%a-shell 3"}) {
				t.Errorf("%s: answered %q, scrolled %q; want done and one scroll of %%a-shell", name, got, calls)
			}
			continue
		}
		if !strings.Contains(got, tc.want) || len(calls) != 0 {
			t.Errorf("%s: answered %q, scrolled %q; want a refusal saying %q and no scroll", name, got, calls, tc.want)
		}
	}
	// And a count beyond the bound is clamped by the host itself.
	b := newScrollingBackend("shell")
	if got := driveScroll(t, true, b, mirror.Msg{Session: "shell", Lines: -100000}).Error; got != "" {
		t.Fatalf("a huge scroll: %q", got)
	}
	if calls, _, _ := b.snapshot(); !slices.Equal(calls, []string{fmt.Sprintf("%%a-shell %d", -mirror.MaxScrollLines)}) {
		t.Fatalf("the host scrolled %q, want one clamped to -%d", calls, mirror.MaxScrollLines)
	}
}

// TestTheHostNamesAPaneInAnotherMode: a pane whose top mode is not copy-mode
// (mirrorhost.ErrPaneInAnotherMode) is refused as mirror.ScrollRefusalMode on
// the wire — the code muster acts on — and any other backend failure is named
// nothing, so muster never reads it as "this host cannot scroll".
func TestTheHostNamesAPaneInAnotherMode(t *testing.T) {
	for name, tc := range map[string]struct {
		refuse  error
		refusal string
	}{
		"another mode on top": {fmt.Errorf("%w: pane %%a-shell is in tree-mode", mirrorhost.ErrPaneInAnotherMode), "mode"},
		"a tmux failure":      {fmt.Errorf("tmux send-keys: exit status 1"), ""},
	} {
		b := newScrollingBackend("shell")
		b.refuse = tc.refuse
		ans := driveScroll(t, true, b, mirror.Msg{Session: "shell", Lines: 3})
		if ans.Error == "" || ans.Refusal != tc.refusal {
			t.Errorf("%s: answered %q named %q, want a refusal named %q", name, ans.Error, ans.Refusal, tc.refusal)
		}
	}
}
