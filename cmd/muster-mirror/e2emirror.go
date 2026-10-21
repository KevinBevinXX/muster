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
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
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
//
// EVERY WINDOW (per-window registration): with MUSTER_MIRROR_E2E_WINDOWS_FILE
// the fake is a PER-WINDOW host (windowFake) whose windows are that file's
// lines, re-read on every listing so a spec can close, renumber or re-order
// them:
//
//	<session>|<window id>|<index>|<active 0/1>|<command>|<activity unix secs>|<claude transcript path, or empty>[|<split pane's transcript>[|<Claude's cwd>[|<identity colour>]]]
//
// A window's active "pane" is `%e2e-<session>-<window id>`. Its terminal prints
// `FAKE-MIRROR session=<s> window=<id> mode=ro|rw`, so a spec sees WHICH window
// it was shown. A window with a transcript path runs Claude Code writing it,
// exactly as MUSTER_MIRROR_E2E_CLAUDE's session does; a paste into such a
// window is recorded in <transcript>.pasted, and one into any other window in
// MUSTER_MIRROR_E2E_PASTE_FILE — each with the pane it reached.
//
// AN OLDER HOST: with MUSTER_MIRROR_E2E_NO_PROMPT_INDEX=1 the host sends no
// prompt index (mirrorhost.Config.NoPromptIndex), as one from before it did.
//
// COPY-MODE SCROLL (the Terminal view's drag-to-scroll): with
// MUSTER_MIRROR_E2E_SCROLL_FILE the host can scroll a pane's history
// (scrollFake), and each scroll is appended to that file as `<pane> <lines>
// pos=<lines back from the bottom>` — and each cancel as `<pane> cancel`. Unset,
// the host is an OLDER muster-mirror that advertises no scrolling, which is how
// a spec sees the page fall back. While a file `<that file>.mode` exists, every
// pane is "in another mode" (a tmux menu over it): a scroll is refused with
// mirrorhost.ErrPaneInAnotherMode, as real tmux's would be.
//
// THE CLAUDE STATUS (the send queue): a per-window host's Claude window reports
// Claude Code's session status and its time from <transcript>.status, a JSON
// {"status", "statusUpdatedAt"} the spec writes the way the CLI writes its
// sessions file; without that file, busy while <transcript>.busy exists,
// waiting while <transcript>.waiting exists, idle otherwise, and NO time (which
// muster's queue must treat as unknown). The host advertises that it reports
// status (Msg.StatusReport). With MUSTER_MIRROR_E2E_NO_STATUS=1 it advertises
// nothing, which is what an older muster-mirror's hello says: muster must then
// send nothing queued to it.
//
// THE COMPOSER MENU'S CATALOG: the host's catalog is read by the REAL scanner
// (mirrorhost.CatalogScanner) from the real places — CLAUDE_CONFIG_DIR for the
// user's and the plugins' commands and skills, MUSTER_MIRROR_SNIPPETS_FILE for
// the snippets — so a spec points those at fixture directories. A per-window
// fake's every window runs in MUSTER_MIRROR_E2E_CWD (its PROJECT's .claude is
// found from there; unset, no window has a working directory). With
// MUSTER_MIRROR_E2E_NO_CATALOG=1 the host advertises no catalog and answers no
// request, which is what an older muster-mirror does: muster must then offer
// the built-in commands alone.
//
// THE REFRESH: MUSTER_MIRROR_E2E_REFRESH (a Go duration) is how often the host
// re-reads its windows, instead of mirrorhost's 5 s default, so the send-queue
// spec waits for a status flip to reach muster rather than for a fixed 5 s
// tick. A value that does not parse to a positive duration refuses to start.
func init() {
	e2eRefresh = func() time.Duration {
		v := os.Getenv("MUSTER_MIRROR_E2E_REFRESH")
		if v == "" {
			return 0
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			panic(fmt.Sprintf("muster-mirror (e2emirror): MUSTER_MIRROR_E2E_REFRESH=%q is not a positive duration", v))
		}
		return d
	}
	e2eNoPromptIndex = func() bool { return os.Getenv("MUSTER_MIRROR_E2E_NO_PROMPT_INDEX") == "1" }
	e2eNoCatalog = func() bool { return os.Getenv("MUSTER_MIRROR_E2E_NO_CATALOG") == "1" }
	e2eNoSubagents = func() bool { return os.Getenv("MUSTER_MIRROR_E2E_NO_SUBAGENTS") == "1" }
	e2eNoWork = func() bool { return os.Getenv("MUSTER_MIRROR_E2E_NO_WORK") == "1" }
	e2eWrap = func(b mirrorhost.Backend) mirrorhost.Backend {
		if f := os.Getenv("MUSTER_MIRROR_E2E_SCROLL_FILE"); f != "" {
			return &scrollFake{Backend: b, file: f, pos: map[string]int{}}
		}
		return b
	}
	e2eBackend = func() mirrorhost.Backend {
		if f := os.Getenv("MUSTER_MIRROR_E2E_WINDOWS_FILE"); f != "" {
			return windowFake{file: f}
		}
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

// windowFake is a per-window host: fakeBackend's terminal and paste recorder,
// with the windows of a file.
type windowFake struct {
	fakeBackend
	file string
}

func windowFakePane(session, window string) string { return "%e2e-" + session + "-" + window }

// Windows implements mirrorhost.WindowLister.
func (w windowFake) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	b, err := os.ReadFile(w.file)
	if err != nil {
		return nil, err
	}
	var out []mirrorhost.WindowInfo
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 7 || len(f) > 10 {
			continue
		}
		index, _ := strconv.Atoi(f[2])
		activity, _ := strconv.ParseInt(f[5], 10, 64)
		info := mirrorhost.WindowInfo{Session: f[0], ID: f[1], Index: index, Active: f[3] == "1", Command: f[4],
			Activity: activity, Pane: windowFakePane(f[0], f[1]), PanePID: 1}
		if f[6] != "" {
			st := windowFakeStatus(f[6])
			status := mirror.NormalizeStatus(st.Status)
			if st.Status != "" && status == "" {
				status = st.Status
			}
			info.Claude = &mirrorhost.ClaudePane{Pane: info.Pane, PID: 1, Transcript: f[6],
				Gen: strings.TrimSuffix(filepath.Base(f[6]), ".jsonl"), Busy: status == "busy", Status: status, StatusSince: st.Since,
				RawStatus: st.Status, StartedAt: st.StartedAt, Shells: cctasks.Shells{N: st.Shells, Monitor: st.Monitor}}
		}
		// A 10th field: the window's identity colour, as tmux would print the
		// option (WindowInfo.Color; the host's wire normalisation drops
		// anything that is not a lowercase #rrggbb).
		if len(f) == 10 {
			info.Color = f[9]
		}
		// An 8th field: the transcript of a Claude Code session in the window's
		// SECOND (non-active) pane, "<window pane>-split".
		if len(f) >= 8 && f[7] != "" {
			info.Others = []mirrorhost.ClaudePane{{Pane: info.Pane + "-split", PID: 1, Transcript: f[7],
				Gen: strings.TrimSuffix(filepath.Base(f[7]), ".jsonl")}}
		}
		// A 9th field: the working directory of the window's active pane
		// (WindowInfo.Cwd), what its repo chip is named from.
		if len(f) >= 9 {
			info.Cwd = f[8]
		}
		out = append(out, info)
	}
	// Hermes windows (e2ehermes.go), after the file's.
	return append(out, hermesWindowsE2E(context.Background())...), nil
}

// windowFakeStatus is a Claude window's status and when it was written. From
// <transcript>.status when it exists — {"status", "statusUpdatedAt"}, the two
// fields of Claude Code's own sessions file, written by the spec as the CLI
// writes them — else from the marker files (busy / waiting / idle) with NO
// time, which is what a host that reports a status without its time sends.
//
// The status file may also carry what the work counts read (mirror/work.go):
// "startedAt" (the CLI's start, Unix ms), and the CLI's process-tree shells,
// "shells" (a count) and "monitor" — what the real host reads from /proc. Its
// "status" is the RAW one ("shell" stays "shell" for the work rule), and the
// window's Status is that normalised, as the real host's is.
func windowFakeStatus(transcript string) fakeStatus {
	if b, err := os.ReadFile(transcript + ".status"); err == nil {
		var sf fakeStatus
		if json.Unmarshal(b, &sf) == nil {
			return sf
		}
		return fakeStatus{}
	}
	if _, err := os.Stat(transcript + ".busy"); err == nil {
		return fakeStatus{Status: "busy"}
	}
	if _, err := os.Stat(transcript + ".waiting"); err == nil {
		return fakeStatus{Status: "waiting"}
	}
	return fakeStatus{Status: "idle"}
}

// fakeStatus is a fake window's <transcript>.status.
type fakeStatus struct {
	Status    string `json:"status"`
	Since     int64  `json:"statusUpdatedAt"`
	StartedAt int64  `json:"startedAt"`
	Shells    int    `json:"shells"`
	Monitor   bool   `json:"monitor"`
}

// ReportsStatus implements mirrorhost.StatusReporter: yes, unless the spec
// plays an older muster-mirror (MUSTER_MIRROR_E2E_NO_STATUS=1).
// WindowCwd plays every window's working directory: MUSTER_MIRROR_E2E_CWD.
func (windowFake) WindowCwd(context.Context, string, string) (string, error) {
	if d := os.Getenv("MUSTER_MIRROR_E2E_CWD"); d != "" {
		return d, nil
	}
	return "", fmt.Errorf("e2e fake: no MUSTER_MIRROR_E2E_CWD")
}

func (windowFake) ReportsStatus() bool { return os.Getenv("MUSTER_MIRROR_E2E_NO_STATUS") == "" }

// List is the windows' sessions: what a per-session reader of this host sees.
func (w windowFake) List(ctx context.Context) ([]string, error) {
	ws, err := w.Windows(ctx)
	var out []string
	for _, win := range ws {
		if len(out) == 0 || out[len(out)-1] != win.Session {
			out = append(out, win.Session)
		}
	}
	return out, err
}

// AttachWindow implements mirrorhost.WindowAttacher: a terminal that says which
// window it is.
func (w windowFake) AttachWindow(_ context.Context, session, window string, readOnly bool, cols, rows uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	t := &fakeTerm{r: pr, w: pw}
	go t.say(fmt.Sprintf("FAKE-MIRROR session=%s window=%s mode=%s size=%dx%d\r\n", session, window, mode, cols, rows) + edgeLine(cols))
	return t, nil
}

// WindowSizeOf plays every window at fakeBackend.WindowSize's size.
func (w windowFake) WindowSizeOf(ctx context.Context, session, _ string) (termsize.Size, error) {
	return w.fakeBackend.WindowSize(ctx, session)
}

// ClaudePanes is the per-session lookup; a per-window host is never asked it.
func (w windowFake) ClaudePanes(context.Context, []string) map[string]mirrorhost.ClaudePane {
	return nil
}

// Paste implements mirrorhost.ClaudeBackend: what claudeFake.Paste records,
// for whichever Claude window p is.
func (w windowFake) Paste(_ context.Context, p mirrorhost.ClaudePane, text string) error {
	return claudeFake{path: p.Transcript}.Paste(context.Background(), p, text)
}

// ActivePane is the per-session lookup; a per-window host is never asked it.
func (w windowFake) ActivePane(context.Context, string) (string, bool) { return "", false }

// PastePane: into a Claude window's pane, as a Claude paste; into any other,
// recorded in MUSTER_MIRROR_E2E_PASTE_FILE.
func (w windowFake) PastePane(ctx context.Context, pane, text string) error {
	ws, _ := w.Windows(ctx)
	for _, win := range ws {
		if win.Pane == pane && win.Claude != nil {
			return w.Paste(ctx, *win.Claude, text)
		}
	}
	return w.fakeBackend.PastePane(ctx, pane, text)
}

// scrollFake plays tmux copy-mode for mirrorhost.ScrollBackend: per pane, how
// many lines back from the bottom it is (absent = not in copy-mode). Like
// `copy-mode -e`, scrolling back to the bottom leaves the mode.
type scrollFake struct {
	mirrorhost.Backend
	file string
	mu   sync.Mutex
	pos  map[string]int
}

// Unwrap lets every other capability be found on the backend this wraps.
func (s *scrollFake) Unwrap() mirrorhost.Backend { return s.Backend }

func (s *scrollFake) record(line string) error {
	fh, err := os.OpenFile(s.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.WriteString(line + "\n")
	return err
}

func (s *scrollFake) ScrollPane(_ context.Context, pane string, lines int) (entered, inMode bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.file + ".mode"); err == nil {
		_, in := s.pos[pane]
		return false, in, fmt.Errorf("%w: pane %s is in tree-mode, not copy-mode", mirrorhost.ErrPaneInAnotherMode, pane)
	}
	p, in := s.pos[pane]
	if !in && lines < 0 {
		// Already at the bottom: nothing moves, but the request is recorded,
		// so a spec can count one line per scroll the page sent.
		return false, false, s.record(fmt.Sprintf("%s %d pos=0", pane, lines))
	}
	entered = !in
	p += lines
	if p <= 0 {
		delete(s.pos, pane)
		p = 0
	} else {
		s.pos[pane] = p
	}
	_, inMode = s.pos[pane]
	return entered, inMode, s.record(fmt.Sprintf("%s %d pos=%d", pane, lines, p))
}

func (s *scrollFake) CancelCopyMode(_ context.Context, pane string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, in := s.pos[pane]; !in {
		return nil
	}
	delete(s.pos, pane)
	return s.record(pane + " cancel")
}

// E2ETmuxSocket is the fake tmux server's socket path: what a spec sends as
// X-Muster-Tmux (its $TMUX) to the host's MCP endpoint.
const E2ETmuxSocket = "/tmp/muster-e2e-fake-tmux/default"

// ServerSocket implements mirrorhost.ServerSocketer.
func (w windowFake) ServerSocket(context.Context) (string, error) { return E2ETmuxSocket, nil }

// ANSWERING A PICKER (Msg.AskAnswer): a Claude window whose transcript is T
// shows an AskUserQuestion picker while T.picker.json exists —
// {"toolUseId", "questions"}, written by the spec as the CLI draws its picker —
// and T.picker.touched, when it also exists, says someone has started answering
// it in the terminal. AnswerAsk drives askdrive's FakePicker (the picker as
// measured on Claude Code 2.1.289) through internal/askdrive's verified
// driver, exactly as the shipped binary drives a pane; when it submits, the
// call and its result are appended to T as the CLI writes them (a tool_use,
// then a tool_result with toolUseResult {questions, answers}) and the picker
// file is removed. A spec answers "in the terminal" by appending those records
// itself and removing the picker file.
func (w windowFake) AnswerAsk(ctx context.Context, pane string, qs []askq.Question, cs []askq.Choice) error {
	ws, err := w.Windows(ctx)
	if err != nil {
		return err
	}
	transcript := ""
	for _, win := range ws {
		if win.Claude != nil && win.Claude.Pane == pane {
			transcript = win.Claude.Transcript
		}
	}
	if transcript == "" {
		return fmt.Errorf("e2e fake: %w: no Claude window for pane %s", askdrive.ErrNoPicker, pane)
	}
	b, err := os.ReadFile(transcript + ".picker.json")
	if err != nil {
		return fmt.Errorf("e2e fake: %w: no picker on screen", askdrive.ErrNoPicker)
	}
	var shown struct {
		ToolUseID string          `json:"toolUseId"`
		Questions []askq.Question `json:"questions"`
	}
	if err := json.Unmarshal(b, &shown); err != nil {
		return err
	}
	p := askdrive.NewFakePicker(shown.Questions)
	if _, err := os.Stat(transcript + ".picker.touched"); err == nil {
		p.Type("1") // someone in the terminal chose (or ticked) the first option
	}
	if err := askdrive.Drive(ctx, fakePickerPane{p}, qs, cs, askdrive.Options{}); err != nil {
		return err
	}
	answers, cancelled, done := p.Result()
	if !done || cancelled {
		return fmt.Errorf("e2e fake: the picker did not submit")
	}
	use, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": shown.ToolUseID, "name": askq.ToolName, "input": map[string]any{"questions": shown.Questions}}}}})
	res, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": shown.ToolUseID, "content": "Your questions have been answered."}}},
		"toolUseResult": map[string]any{"questions": shown.Questions, "answers": answers}})
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(append(use, '\n'), append(res, '\n')...))
	_ = f.Close()
	_ = os.Remove(transcript + ".picker.json")
	return werr
}

// fakePickerPane is a FakePicker as the driver's pane.
type fakePickerPane struct{ p *askdrive.FakePicker }

func (f fakePickerPane) Capture(context.Context) (string, error) { return f.p.Screen(), nil }
func (f fakePickerPane) Send(_ context.Context, keys string) error {
	f.p.Type(keys)
	return nil
}
