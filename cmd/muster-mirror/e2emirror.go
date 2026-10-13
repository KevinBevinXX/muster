//go:build e2emirror

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/termsize"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2emirror`
// (e2e/tests/mirrored.spec.ts builds it). It replaces tmux with a fake whose
// sessions are MUSTER_MIRROR_E2E_SESSIONS (comma-separated) and whose terminal
// prints, on screen, exactly what reached it: `FAKE-MIRROR session=<s>
// mode=ro|rw`, `size=CxR` per resize and `in=<HEX>` per input chunk. The dial,
// the token, the registration, the tunnel and everything in muster are real.
//
// THE RENDERED VIEW (tunnel v2): with MUSTER_MIRROR_E2E_CLAUDE=<session>=<path>
// that session's "pane" runs Claude Code writing the transcript at <path> — a
// real JSONL file the spec appends records to while a turn "runs", read by the
// real tailer. Its session id is the file's basename; it is busy while
// <path>.busy exists. A paste is appended to the transcript as a prompt record
// (what the CLI would write on receiving it) and to <path>.pasted, so the spec
// sees both. Unset, no session is a Claude pane — the v1-era behaviour the
// terminal tests above rely on.
//
// A PASTE INTO ANY PANE (the Terminal view's composer): every session's active
// "pane" is `%e2e-<session>`, and a paste into one is appended to
// MUSTER_MIRROR_E2E_PASTE_FILE as `<pane> <text>` (refused when that is unset,
// so a spec that did not ask for it cannot pass by accident). The Claude
// session's active pane is its Claude pane (%9), as on a real host, and a paste
// into it goes where a Claude paste goes.
func init() {
	e2eBackend = func() mirrorhost.Backend {
		if c := os.Getenv("MUSTER_MIRROR_E2E_CLAUDE"); c != "" {
			if s, p, ok := strings.Cut(c, "="); ok {
				return claudeFake{fakeBackend: fakeBackend{}, session: s, path: p}
			}
		}
		return fakeBackend{}
	}
}

// claudeFake is fakeBackend plus one Claude Code pane.
type claudeFake struct {
	fakeBackend
	session, path string
}

func (c claudeFake) ClaudePanes(_ context.Context, sessions []string) map[string]mirrorhost.ClaudePane {
	out := map[string]mirrorhost.ClaudePane{}
	for _, s := range sessions {
		if s == c.session {
			_, err := os.Stat(c.path + ".busy")
			out[s] = mirrorhost.ClaudePane{Pane: claudeFakePane, PID: 1, Transcript: c.path,
				Gen: strings.TrimSuffix(filepath.Base(c.path), ".jsonl"), Busy: err == nil}
		}
	}
	return out
}

func (c claudeFake) Paste(_ context.Context, p mirrorhost.ClaudePane, text string) error {
	rec, _ := json.Marshal(map[string]any{"type": "user", "promptId": "e2e-pasted",
		"message": map[string]any{"role": "user", "content": text}})
	for _, f := range []struct {
		path string
		line []byte
	}{{c.path + ".pasted", []byte(p.Pane + " " + text + "\n")}, {c.path, append(rec, '\n')}} {
		fh, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = fh.Write(f.line)
		_ = fh.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ActivePane: the Claude session's active pane IS its Claude pane.
func (c claudeFake) ActivePane(ctx context.Context, session string) (string, bool) {
	if session == c.session {
		return claudeFakePane, true
	}
	return c.fakeBackend.ActivePane(ctx, session)
}

func (c claudeFake) PastePane(ctx context.Context, pane, text string) error {
	if pane == claudeFakePane {
		return c.Paste(ctx, mirrorhost.ClaudePane{Pane: pane}, text)
	}
	return c.fakeBackend.PastePane(ctx, pane, text)
}

const claudeFakePane = "%9"

type fakeBackend struct{}

func (b fakeBackend) ActivePane(ctx context.Context, session string) (string, bool) {
	list, _ := b.List(ctx)
	for _, s := range list {
		if s == session {
			return "%e2e-" + session, true
		}
	}
	return "", false
}

func (fakeBackend) PastePane(_ context.Context, pane, text string) error {
	f := os.Getenv("MUSTER_MIRROR_E2E_PASTE_FILE")
	if f == "" {
		return fmt.Errorf("e2e fake: no MUSTER_MIRROR_E2E_PASTE_FILE")
	}
	fh, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.WriteString(pane + " " + text + "\n")
	return err
}

func (fakeBackend) List(context.Context) ([]string, error) {
	var out []string
	for _, s := range strings.Split(os.Getenv("MUSTER_MIRROR_E2E_SESSIONS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// ActiveWindows plays each session's active window (pretty-name labels): the
// file MUSTER_MIRROR_E2E_ACTIVE_FILE holds `session=index` lines and is re-read
// on every call, so a spec can MOVE a session's window by rewriting it. Unset,
// no window is known (a label is then the codename alone).
func (fakeBackend) ActiveWindows(context.Context) (map[string]string, error) {
	f := os.Getenv("MUSTER_MIRROR_E2E_ACTIVE_FILE")
	if f == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if s, idx, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			out[s] = idx
		}
	}
	return out, nil
}

// Activity plays each session's last-active time (most-recently-active-first
// ordering): the file MUSTER_MIRROR_E2E_ACTIVITY_FILE holds `session=<unix
// seconds>` lines and is re-read on every call, so a spec can make a session
// "active" by rewriting it. Unset, no activity is known and muster keeps the
// name order — what every spec written before the ordering relies on.
func (fakeBackend) Activity(context.Context) (map[string]int64, error) {
	f := os.Getenv("MUSTER_MIRROR_E2E_ACTIVITY_FILE")
	if f == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, line := range strings.Split(string(b), "\n") {
		if s, at, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			if v, err := strconv.ParseInt(at, 10, 64); err == nil {
				out[s] = v
			}
		}
	}
	return out, nil
}

// WindowSize plays a session window of MUSTER_MIRROR_E2E_WINDOW ("CxR"); with
// it unset the fake is a backend that cannot report one (the pre-termsize
// behaviour: attach as asked, no report).
func (fakeBackend) WindowSize(context.Context, string) (termsize.Size, error) {
	var c, r uint16
	if _, err := fmt.Sscanf(os.Getenv("MUSTER_MIRROR_E2E_WINDOW"), "%dx%d", &c, &r); err != nil || c == 0 || r == 0 {
		return termsize.Size{}, fmt.Errorf("e2e fake: no MUSTER_MIRROR_E2E_WINDOW")
	}
	return termsize.Size{Cols: c, Rows: r}, nil
}

// edgeLine is a ruler ending exactly at the client's last column with
// RIGHT-EDGE, so a browser test can scroll to it: on a client wider than the
// page it is off-screen until scrolled.
func edgeLine(cols uint16) string {
	const mark = "RIGHT-EDGE"
	if int(cols) <= len(mark) {
		return ""
	}
	return strings.Repeat("-", int(cols)-len(mark)) + mark + "\r\n"
}

func (fakeBackend) Attach(_ context.Context, session string, readOnly bool, cols, rows uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	t := &fakeTerm{r: pr, w: pw}
	go t.say(fmt.Sprintf("FAKE-MIRROR session=%s mode=%s size=%dx%d\r\n", session, mode, cols, rows) + edgeLine(cols))
	return t, nil
}

type fakeTerm struct {
	r    *io.PipeReader
	w    *io.PipeWriter
	mu   sync.Mutex
	once sync.Once
}

func (t *fakeTerm) say(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.w.Write([]byte(s))
}

func (t *fakeTerm) Read(p []byte) (int, error) { return t.r.Read(p) }

func (t *fakeTerm) Write(p []byte) (int, error) {
	go t.say("in=" + strings.ToUpper(hex.EncodeToString(p)) + "\r\n")
	return len(p), nil
}

func (t *fakeTerm) Resize(cols, rows uint16) error {
	go t.say(fmt.Sprintf("size=%dx%d\r\n", cols, rows))
	return nil
}

func (t *fakeTerm) Close() error {
	t.once.Do(func() { _ = t.w.Close() })
	return nil
}
