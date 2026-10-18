package mirrorhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/termsize"
)

// EVERY WINDOW IS MIRRORED. An operator runs one coding agent per tmux WINDOW,
// so a host that registered one entry per session showed only each session's
// active window and left the rest unreachable. A WindowLister host registers
// one entry per window (mirror.Msg.Windows), addressed by the window's tmux id.
//
// 🔴 VIEWING OR TYPING INTO A WINDOW MUST NOT MOVE THE OPERATOR'S SCREEN. A
// tmux SESSION has one current window, shared by every client attached to it:
// `tmux attach -t session:window` and `select-window` change it for all of
// them, the operator's own terminal included. So a window is never shown
// through the operator's session. Each viewer gets a session of its OWN
// (Tmux.AttachWindow) holding a LINK to the one window it shows — the same
// window, in a session whose current window is nobody else's — and gone when
// the viewer leaves.

// WindowInfo is one tmux window as a backend reads it.
type WindowInfo struct {
	// Session and ID address the window: the tmux session name and window id
	// ("@12"). Index is its index in the session now (display only).
	Session string
	ID      string
	Index   int
	// Active: the session's current window.
	Active bool
	// Pane is the window's active pane id ("%7"): the only input target.
	Pane string
	// PanePID is that pane's root process.
	PanePID int
	// Activity is the Unix time the window was last active: its own
	// #{window_activity} and, for the session's ACTIVE window, the session's
	// #{session_activity} when that is newer (a client's key that draws
	// nothing — switching to the window — is activity too; see Tmux.Activity).
	Activity int64
	// Command is what runs in the active pane (#{pane_current_command}).
	Command string
	// Claude is the Claude Code session in the active pane; nil for none.
	Claude *ClaudePane
	// Others are the Claude Code sessions in the window's OTHER panes (a
	// split), by pane: what lets a session in a non-active pane be resolved
	// from its pane (MCPProxy) and recognised by muster (mirror.Window's
	// OtherClaude). Nothing renders them.
	Others []ClaudePane
	// panes is every non-active pane of the window, as the listing read them:
	// what Windows locates Others from.
	panes []pane
}

// WindowLister is a Backend that can list every window of every session, from
// ONE read. Tmux is one; a backend that is not is registered per session, as
// before.
type WindowLister interface {
	// Windows is every window of every session, in (session, index) order.
	// Viewer sessions (mirror.IsViewSession) are not in it. A host with no tmux
	// server has none — that is not an error.
	Windows(ctx context.Context) ([]WindowInfo, error)
}

// WindowAttacher is a Backend that can attach a client showing ONE window of a
// session without changing that session's current window for anyone else.
type WindowAttacher interface {
	AttachWindow(ctx context.Context, session, window string, readOnly bool, cols, rows uint16) (Terminal, error)
}

// StatusReporter is a WindowLister that fills each Claude window's
// ClaudePane.Status from Claude Code's own sessions file — what a hello's
// Msg.StatusReport advertises, and what muster's send queue waits on. Tmux is
// one. A backend that is not is believed about no status (muster sends nothing
// queued to it).
type StatusReporter interface {
	ReportsStatus() bool
}

// ReportsStatus implements StatusReporter: ClaudeLocator reads the status from
// the sessions file the CLI writes for its peers.
func (Tmux) ReportsStatus() bool { return true }

// reportsStatus is whether b is a per-window backend that reports status.
func reportsStatus(b Backend) bool {
	if _, ok := WindowListerOf(b); !ok {
		return false
	}
	sr, ok := capabilityOf[StatusReporter](b)
	return ok && sr.ReportsStatus()
}

// WindowListerOf is b's WindowLister, looking through wrappers (see ClaudeOf).
func WindowListerOf(b Backend) (WindowLister, bool) { return capabilityOf[WindowLister](b) }

// WindowAttacherOf is b's WindowAttacher. The OUTERMOST one: a wrapper that
// sizes a client (NoShrink) implements it over what it wraps, and capabilityOf
// asks the wrapper before it unwraps.
func WindowAttacherOf(b Backend) (WindowAttacher, bool) { return capabilityOf[WindowAttacher](b) }

// windowsFormat is the ONE listing a poll reads: a line per pane of every
// window of every session. The session name is the LAST field, so it is taken
// whole whatever it holds.
const windowsFormat = "#{pane_active}\t#{window_id}\t#{window_index}\t#{window_active}\t#{pane_id}\t#{pane_pid}\t#{window_activity}\t#{session_activity}\t#{pane_current_command}\t#{session_name}"

// Windows implements WindowLister with ONE `tmux list-panes -a`, however many
// windows there are: the listing, each session's current window, each window's
// active pane (its input target and the root of the Claude Code lookup), its
// activity and its command all come from that read.
func (t Tmux) Windows(ctx context.Context) ([]WindowInfo, error) {
	cmd := exec.CommandContext(ctx, t.bin(), t.query("list-panes", "-a", "-F", windowsFormat)...)
	cmd.Env = tmuxEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := errb.String()
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return []WindowInfo{}, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %v: %s", err, strings.TrimSpace(msg))
	}
	windows := parseWindows(out.String())
	panes := make([]pane, 0, len(windows))
	for _, w := range windows {
		panes = append(panes, pane{Session: w.Session, ID: w.Pane, PID: w.PanePID})
		panes = append(panes, w.panes...)
	}
	claude := t.Claude.LocatePanes(panes)
	for i := range windows {
		if cp, ok := claude[windows[i].Pane]; ok {
			windows[i].Claude = &cp
		}
		for _, p := range windows[i].panes {
			if cp, ok := claude[p.ID]; ok {
				windows[i].Others = append(windows[i].Others, cp)
			}
		}
	}
	return windows, nil
}

// parseWindows reads Windows' list-panes output: one WindowInfo per (session,
// window) — its ACTIVE pane's line, with the window's other panes in panes —
// sorted by session, then index, then id. A line that does not parse, a window
// with no usable id, and every window of a viewer session are skipped.
func parseWindows(out string) []WindowInfo {
	var list []WindowInfo
	type wkey struct{ session, id string }
	others := map[wkey][]pane{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 10)
		if len(f) == 10 && f[0] != "1" && strings.HasPrefix(f[4], "%") {
			if pid, err := strconv.Atoi(f[5]); err == nil && pid > 0 {
				others[wkey{f[9], f[1]}] = append(others[wkey{f[9], f[1]}], pane{Session: f[9], ID: f[4], PID: pid})
			}
			continue
		}
		if len(f) != 10 || f[0] != "1" {
			continue
		}
		session := f[9]
		if mirror.ValidSession(session) != nil || mirror.IsViewSession(session) ||
			mirror.ValidWindowID(f[1]) != nil || !strings.HasPrefix(f[4], "%") {
			continue
		}
		index, err := strconv.Atoi(f[2])
		if err != nil || index < 0 {
			continue
		}
		pid, err := strconv.Atoi(f[5])
		if err != nil || pid <= 0 {
			continue
		}
		w := WindowInfo{Session: session, ID: f[1], Index: index, Active: f[3] == "1", Pane: f[4], PanePID: pid, Command: f[8]}
		w.Activity, _ = strconv.ParseInt(f[6], 10, 64)
		if w.Active {
			if sa, err := strconv.ParseInt(f[7], 10, 64); err == nil && sa > w.Activity {
				w.Activity = sa
			}
		}
		list = append(list, w)
	}
	for i := range list {
		list[i].panes = others[wkey{list[i].Session, list[i].ID}]
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.ID < b.ID
	})
	return list
}

// FindWindow is the entry of windows for (session, window id).
func FindWindow(windows []WindowInfo, session, window string) (WindowInfo, bool) {
	for _, w := range windows {
		if w.Session == session && w.ID == window {
			return w, true
		}
	}
	return WindowInfo{}, false
}

// WireWindows is windows as a host SENDS them: labelled from codenames
// ("<codename>:<index>" for a session that has one), with Claude — and the
// Claude session's Status — kept only when claude is set (a tunnel that carries
// no rendered view carries no claim of one), through mirror.NormalizeWindows.
func WireWindows(windows []WindowInfo, codenames map[string]string, claude bool, now time.Time) []mirror.Window {
	out := make([]mirror.Window, 0, len(windows))
	for _, w := range windows {
		m := mirror.Window{Session: w.Session, ID: w.ID, Index: w.Index, Active: w.Active,
			Activity: w.Activity, Command: w.Command, Claude: claude && w.Claude != nil}
		if m.Claude {
			m.Status, m.StatusSince, m.ClaudeSession = w.Claude.Status, w.Claude.StatusSince, w.Claude.Gen
		}
		if claude {
			for _, o := range w.Others {
				m.OtherClaude = append(m.OtherClaude, o.Gen)
			}
		}
		if name, ok := codenames[w.Session]; ok {
			m.Label = name + ":" + strconv.Itoa(w.Index)
		}
		out = append(out, m)
	}
	return mirror.NormalizeWindows(out, now.Unix())
}

// viewSessionPattern is a name viewSessionName makes — and the ONLY shape of
// name this process will ever kill.
var viewSessionPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(mirror.ViewSessionPrefix) + `[0-9a-f]{12}$`)

func viewSessionName() string {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	return mirror.ViewSessionPrefix + hex.EncodeToString(rnd[:])
}

// run runs one tmux command against this server and returns its combined
// output.
func (t Tmux) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), args...)...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// killViewSession ends one viewer session.
//
// 🔴 IT REFUSES ANY NAME THAT IS NOT A VIEWER SESSION'S. This is the only place
// muster-mirror ends a tmux session, and the operator's sessions live on the
// same server: the name must be exactly one viewSessionName made, and the
// target is an exact match (`=`), never a prefix.
func (t Tmux) killViewSession(ctx context.Context, name string) error {
	if !viewSessionPattern.MatchString(name) {
		return fmt.Errorf("muster-mirror: refusing to end tmux session %q: it is not one of muster-mirror's own viewer sessions", name)
	}
	_, err := t.run(ctx, "kill-session", "-t", "="+name)
	return err
}

// ViewClientFlags is the tmux client flags (-f) a window viewer attaches with.
//
//   - ignore-size, ALWAYS: the viewer's client never takes part in sizing a
//     window. 🔴 MEASURED on tmux 3.7c (the tmuxit suite's
//     TestTmuxAWindowViewerNeverResizesWhatTheOperatorIsLookingAt): the
//     operator's client is 150x40 on a window, a 60x20 client of another
//     session that links the SAME window shows it, and without this flag the
//     operator's window becomes 60x19 — under `window-size latest` (the
//     default) and under `smallest`. A viewer of the window the operator has
//     current is the ordinary case (it is the window at the top of the list).
//     With the flag the window keeps the operator's size whichever window the
//     viewer shows.
//   - read-only for a read-only attach: tmux ignores the client's keys.
func ViewClientFlags(readOnly bool) string {
	if readOnly {
		return "read-only,ignore-size"
	}
	return "ignore-size"
}

// AttachWindow implements WindowAttacher: a client showing window (a tmux
// window id) of session, in a viewer session of its own.
//
// 🔴 THE OPERATOR'S SESSION IS NEVER THE TARGET OF A select-window OR AN
// attach. The steps, each checked:
//
//  1. `list-windows -t =<session>`: the window must be in that session NOW. A
//     window id is server-wide, so without this a request could be shown a
//     window of another session — or of a session that has ended.
//  2. `new-session -d -s muster-view-<rand> ”` and, in the SAME invocation,
//     `link-window -k -s <window id> -t =muster-view-<rand>:{start}`: a new
//     session whose one placeholder window is replaced by a LINK to the target
//     window. The viewer session then holds exactly that window, as its
//     current window; the operator's session is not named by either command.
//     The empty command makes the placeholder a pane with NO process in it:
//     nothing is forked on the host — not the operator's shell, not a sleep.
//     (tmux removes such a pane by itself a moment later, measured on 3.7c;
//     the two commands are one invocation so the link lands first.)
//     ⚠ What the tmuxit suite can SEE is only that the server's default
//     command never RUNS; it cannot tell the empty command from a default
//     command forked and removed in the same instant (a marker-writing default
//     command wrote nothing either way). The empty command is the stated
//     intent, not a tested guard.
//     A window that has closed since step 1 fails the link ("can't find
//     window"); the viewer session is then removed and the attach refused.
//  3. `attach-session -f <ViewClientFlags> -t =muster-view-<rand>`, then
//     `set-option destroy-unattached on` for it: tmux itself removes the
//     viewer session the moment its client is gone — also when muster-mirror
//     is killed outright — and Close removes it explicitly as well.
//
// 🔴 WHY A LINK AND NOT A SESSION GROUP (`new-session -t <session>`), which also
// gives a viewer its own current window. Measured on tmux 3.7c, a session of 5
// windows: joining the group fires the server's `window-linked` hook 6 times
// and needs a select-window (the `after-select-window` hook) — and an operator
// whose tmux.conf binds those hooks to per-window bookkeeping has it run, in
// the VIEWER session's context, on every open. The group also stays on the
// operator's session (#{session_group}) after the viewer is gone. A link fires
// `window-linked` twice whatever the session's size, selects nothing, and
// leaves the operator's session exactly as it was.
//
// ⚠ What still fires, as for any client: `session-created`, `client-attached`
// and `client-session-changed`, with the viewer session's name
// (mirror.ViewSessionPrefix) as the hook's session.
func (t Tmux) AttachWindow(ctx context.Context, session, window string, readOnly bool, cols, rows uint16) (Terminal, error) {
	if err := mirror.ValidSession(session); err != nil {
		return nil, err
	}
	if err := mirror.ValidWindowID(window); err != nil {
		return nil, err
	}
	out, err := t.run(ctx, "list-windows", "-t", "="+session, "-F", "#{window_id}")
	if err != nil {
		return nil, fmt.Errorf("muster-mirror: tmux session %q is gone", session)
	}
	if !slices.Contains(strings.Fields(out), window) {
		return nil, fmt.Errorf("muster-mirror: window %s is not in tmux session %q", window, session)
	}
	view := viewSessionName()
	drop := func() {
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = t.killViewSession(kctx, view)
	}
	if _, err := t.run(ctx, "new-session", "-d", "-s", view, "", ";",
		"link-window", "-k", "-s", window, "-t", "="+view+":{start}"); err != nil {
		// Whatever step failed, a viewer session that exists is not left behind.
		drop()
		return nil, fmt.Errorf("muster-mirror: window %s of tmux session %q cannot be shown: %w", window, session, err)
	}
	// ⚠ THE OPTION'S TARGET ENDS IN A COLON. set-option's -t is a PANE target;
	// a bare `=<name>` is not found by it ("no such session", measured on 3.7c —
	// the option was then never set, and a killed client's viewer session
	// stayed). `=<name>:` names the session, exactly.
	args := append(t.server(), "-u", "attach-session", "-f", ViewClientFlags(readOnly), "-t", "="+view, ";",
		"set-option", "-t", "="+view+":", "destroy-unattached", "on")
	term, err := t.client(args, cols, rows, func() {
		kctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Usually already gone (destroy-unattached); an error is that.
		_ = t.killViewSession(kctx, view)
	})
	if err != nil {
		drop()
		return nil, err
	}
	return term, nil
}

// CleanViewSessions ends every viewer session with NO client attached: what a
// muster-mirror that was killed between creating one and attaching to it left
// behind (or a session-restore plugin brought back). It returns the names it
// ended. A viewer session in use — another muster-mirror's, on the same server
// — has a client and is left alone.
func (t Tmux) CleanViewSessions(ctx context.Context) ([]string, error) {
	out, err := t.run(ctx, "-u", "list-sessions", "-F", "#{session_attached}\t#{session_name}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting to") {
			return nil, nil
		}
		return nil, err
	}
	var ended []string
	var errs []error
	for _, line := range strings.Split(out, "\n") {
		attached, name, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || attached != "0" || !viewSessionPattern.MatchString(name) {
			continue
		}
		if err := t.killViewSession(ctx, name); err != nil {
			errs = append(errs, err)
			continue
		}
		ended = append(ended, name)
	}
	return ended, errors.Join(errs...)
}

// ViewCleaner is a Backend that may have left viewer sessions behind.
type ViewCleaner interface {
	CleanViewSessions(ctx context.Context) ([]string, error)
}

// ViewCleanerOf is b's ViewCleaner, looking through wrappers (see ClaudeOf).
func ViewCleanerOf(b Backend) (ViewCleaner, bool) { return capabilityOf[ViewCleaner](b) }

// windowSizeFormat is termsize.TmuxWindowFormat plus the window it answered
// for.
const windowSizeFormat = termsize.TmuxWindowFormat + " #{window_id}"

// WindowSizeOf is the CLIENT size that shows window of session exactly, like
// WindowSize.
//
// ⚠ tmux ANSWERS FOR ANOTHER WINDOW RATHER THAN FAILING when the id is not in
// the session (measured on 3.7c: `display-message -t =other:@1` printed
// other's current window), so the answer carries the window id and one that is
// not the window asked for is an error.
func (t Tmux) WindowSizeOf(ctx context.Context, session, window string) (termsize.Size, error) {
	out, err := t.run(ctx, "display-message", "-p", "-t", "="+session+":"+window, windowSizeFormat)
	if err != nil {
		return termsize.Size{}, err
	}
	out = strings.TrimSpace(out)
	i := strings.LastIndexByte(out, ' ')
	if i < 0 || out[i+1:] != window {
		return termsize.Size{}, fmt.Errorf("tmux display-message: asked for window %s of %q, answered %q", window, session, out)
	}
	return termsize.ParseTmuxWindow(out[:i])
}

// WindowOfSizer is a Backend that can say how big one window is right now.
type WindowOfSizer interface {
	WindowSizeOf(ctx context.Context, session, window string) (termsize.Size, error)
}
