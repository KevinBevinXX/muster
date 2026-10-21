//go:build tmuxit

package mirrorhost

// EVERY WINDOW, against REAL tmux on a private socket (newTmuxRig): the
// listing, the per-window Claude Code lookup, and the property the whole design
// exists for — that viewing, typing into or pasting into a window NEVER changes
// what the operator's own client is looking at, nor its size.
//
// The "operator" in these tests is a real, interactive tmux client attached to
// the session by name (Tmux.Attach), exactly as a terminal emulator on the host
// is. Like the rest of the suite it never skips.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// window starts a new window in session whose pane prints marker and echoes
// what is typed, WITHOUT making it the session's current window, and returns
// its tmux id.
func (r *tmuxRig) window(session, marker string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+session+":",
		"sh", "-c", "printf '%s\\n' "+marker+"; exec cat"))
}

// current is the session's current window as "<index> <id>": what every client
// attached to that session is looking at.
func (r *tmuxRig) current(session string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run("display-message", "-p", "-t", "="+session+":", "#{window_index} #{window_id}"))
}

// clientWindow is what the client attached to session — the operator — shows:
// "<session> <window id>". It is read from the CLIENT, not the session, so a
// client that was moved to another session would show.
func (r *tmuxRig) clientWindow(session string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run("list-clients", "-t", "="+session, "-F", "#{client_session} #{window_id}"))
}

func (r *tmuxRig) windowPane(id string) string {
	r.t.Helper()
	return r.run("capture-pane", "-p", "-t", id)
}

func (r *tmuxRig) windowSize(id string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run("display-message", "-p", "-t", id, "#{window_width}x#{window_height}"))
}

// viewSessions is every session on the server whose name is a viewer session's.
func (r *tmuxRig) viewSessions() []string {
	r.t.Helper()
	var out []string
	for _, s := range strings.Fields(r.run("list-sessions", "-F", "#{session_name}")) {
		if mirror.IsViewSession(s) {
			out = append(out, s)
		}
	}
	return out
}

func ids(ws []WindowInfo) string {
	var b []string
	for _, w := range ws {
		a := ""
		if w.Active {
			a = "*"
		}
		b = append(b, fmt.Sprintf("%s:%d%s=%s", w.Session, w.Index, a, w.ID))
	}
	return strings.Join(b, " ")
}

// TestTmuxWindowsListsEveryWindowByIDThroughRenumberingAndAClose: two sessions,
// five windows, each listed once with its own id and pane; each session's
// CURRENT window marked (a middle one, and the last); and then the two things
// an index cannot survive — a window closing below the others with
// renumber-windows on (every index shifts, every id stays), and a move-window —
// leave each window's ID on the same pane.
func TestTmuxWindowsListsEveryWindowByIDThroughRenumberingAndAClose(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	if got, err := b.Windows(ctx); err != nil || len(got) != 0 {
		t.Fatalf("Windows with no server = %v, %v; want empty, nil", got, err)
	}
	r.session("alpha", "A0")
	a0 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=alpha:", "#{window_id}"))
	a1 := r.window("alpha", "A1")
	a2 := r.window("alpha", "A2")
	r.session("beta two", "B0")
	b0 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=beta two:", "#{window_id}"))
	b1 := r.window("beta two", "B1")
	r.run("select-window", "-t", a1)
	r.run("select-window", "-t", b1)

	got, err := b.Windows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("alpha:0=%s alpha:1*=%s alpha:2=%s beta two:0=%s beta two:1*=%s", a0, a1, a2, b0, b1); ids(got) != want {
		t.Fatalf("Windows = %s\n     want %s", ids(got), want)
	}
	panes := map[string]string{}
	for _, w := range got {
		if !strings.HasPrefix(w.Pane, "%") || w.PanePID <= 0 || w.Activity <= 0 {
			t.Fatalf("window %s has no usable pane / pid / activity: %+v", w.ID, w)
		}
		panes[w.ID] = w.Pane
	}
	if len(panes) != 5 {
		t.Fatalf("five windows share ids: %v", panes)
	}

	// The window at index 0 closes, with renumber-windows on: a1 and a2 move down.
	r.run("set-option", "-g", "renumber-windows", "on")
	r.run("kill-window", "-t", a0)
	got, _ = b.Windows(ctx)
	if want := fmt.Sprintf("alpha:0*=%s alpha:1=%s beta two:0=%s beta two:1*=%s", a1, a2, b0, b1); ids(got) != want {
		t.Fatalf("after closing the first window, Windows = %s\n     want %s (same ids, new indexes)", ids(got), want)
	}
	for _, w := range got {
		if panes[w.ID] != w.Pane {
			t.Fatalf("window %s is now pane %s, was %s: an id no longer names the same window", w.ID, w.Pane, panes[w.ID])
		}
	}
	// A window is moved to another index: its id, and its pane, are unchanged.
	r.run("set-option", "-g", "renumber-windows", "off")
	r.run("move-window", "-d", "-s", a2, "-t", "=alpha:7")
	got, _ = b.Windows(ctx)
	if want := fmt.Sprintf("alpha:0*=%s alpha:7=%s beta two:0=%s beta two:1*=%s", a1, a2, b0, b1); ids(got) != want {
		t.Fatalf("after move-window, Windows = %s\n     want %s", ids(got), want)
	}
	if w, ok := FindWindow(got, "alpha", a2); !ok || w.Pane != panes[a2] || w.Index != 7 {
		t.Fatalf("FindWindow(alpha, %s) = %+v, %v", a2, w, ok)
	}
	if _, ok := FindWindow(got, "beta two", a2); ok {
		t.Fatal("a window was found under a session it is not in")
	}
}

// TestTmuxWindowActivityIsEachWindowsOwn: a BACKGROUND window that prints is
// newer than its session's current window, which did nothing — the order a
// per-window list needs, and one a per-session time cannot give.
func TestTmuxWindowActivityIsEachWindowsOwn(t *testing.T) {
	r := newTmuxRig(t)
	r.session("work", "QUIET")
	// 🔴 THE QUIET WINDOW MUST HAVE FINISHED PRINTING BEFORE ITS TIME IS READ.
	// new-session returns before the pane's shell has run, and the marker it
	// prints is activity: read `before` first and the marker can land after it,
	// in the next second under load — a "moved" time that is the fixture's own
	// output, not the clock or the neighbour (CI job 118961513333; reproduced
	// every time by delaying the marker 1.1 s). Waiting here, before the noisy
	// window exists, also keeps its output two seconds after the quiet one's.
	waitFor(t, "the quiet window to print its marker", func() bool { return strings.Contains(r.pane("work"), "QUIET") })
	noisy := strings.TrimSpace(r.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=work:",
		"sh", "-c", "sleep 2; printf 'late output\\n'; exec cat"))
	b := Tmux{Socket: r.sock}
	before, err := b.Windows(context.Background())
	if err != nil || len(before) != 2 {
		t.Fatalf("fixture: %v, %v", before, err)
	}
	waitFor(t, "the background window to print", func() bool { return strings.Contains(r.windowPane(noisy), "late output") })
	after, _ := b.Windows(context.Background())
	quiet, _ := FindWindow(after, "work", before[0].ID)
	loud, _ := FindWindow(after, "work", noisy)
	if quiet.ID == noisy || !quiet.Active || loud.Active {
		t.Fatalf("fixture: the quiet window must be the current one: %+v / %+v", quiet, loud)
	}
	if loud.Activity <= quiet.Activity {
		t.Fatalf("the window that printed (%d) is not newer than the one that did nothing (%d)", loud.Activity, quiet.Activity)
	}
	was, _ := FindWindow(before, "work", before[0].ID)
	if quiet.Activity != was.Activity {
		t.Fatalf("the quiet window's time moved (%d -> %d) with nothing happening in it: it is following the clock or its neighbour", was.Activity, quiet.Activity)
	}
}

// fakeClaude writes a stand-in Claude Code CLI under dir and returns the argv
// that runs it in a pane: a process whose argv names the npm package's entry
// script (what the locator looks for), which records itself in
// <home>/sessions/<pid>.json as the CLI does, prints a marker and then echoes
// what is typed.
func fakeClaude(t *testing.T, dir, home, sessionID string) []string {
	t.Helper()
	script := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `mkdir -p "$1/sessions"
printf '{"sessionId":"%s","cwd":"%s","status":"idle"}' "$2" "$PWD" > "$1/sessions/$$.json"
printf 'FAKE-CLAUDE %s\n' "$2"
cat
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", script, home, sessionID}
}

// TestTmuxTwoClaudeWindowsOfOneSessionInOneCwdAreTheirOwnTranscripts is the
// defect per-window registration must not inherit: ONE session, TWO windows,
// each running Claude Code in the SAME directory. Each window resolves to its
// own pane, its own process and its own transcript — and a message pasted to
// the window that is NOT the session's current one lands in that window's pane
// and nowhere else.
func TestTmuxTwoClaudeWindowsOfOneSessionInOneCwdAreTheirOwnTranscripts(t *testing.T) {
	r := newTmuxRig(t)
	home, cwd := t.TempDir(), t.TempDir()
	project := filepath.Join(home, "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := func(id string) string {
		p := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The window listed FIRST gets the OLDER transcript: "the newest in the cwd"
	// would be the other window's.
	first, second := transcript("sess-first"), transcript("sess-second")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(first, old, old)

	one := fakeClaude(t, t.TempDir(), home, "sess-first")
	two := fakeClaude(t, t.TempDir(), home, "sess-second")
	r.run(append([]string{"new-session", "-d", "-s", "scratch", "-x", "120", "-y", "40", "-c", cwd}, one...)...)
	w1 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=scratch:", "#{window_id}"))
	w2 := strings.TrimSpace(r.run(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=scratch:", "-c", cwd}, two...)...))
	// A third window in the same cwd that is NOT Claude Code.
	w3 := strings.TrimSpace(r.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=scratch:", "-c", cwd, "sh", "-c", "printf 'SHELL\\n'; exec cat"))
	waitFor(t, "both fake CLIs to start", func() bool {
		return strings.Contains(r.windowPane(w1), "FAKE-CLAUDE sess-first") && strings.Contains(r.windowPane(w2), "FAKE-CLAUDE sess-second")
	})

	b := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home}}
	ctx := context.Background()
	var ws []WindowInfo
	waitFor(t, "both windows to resolve", func() bool {
		ws, _ = b.Windows(ctx)
		a, _ := FindWindow(ws, "scratch", w1)
		c, _ := FindWindow(ws, "scratch", w2)
		return a.Claude != nil && c.Claude != nil
	})
	a, _ := FindWindow(ws, "scratch", w1)
	c, _ := FindWindow(ws, "scratch", w2)
	sh, _ := FindWindow(ws, "scratch", w3)
	if a.Claude.Transcript != first || a.Claude.Gen != "sess-first" || a.Claude.Pane != a.Pane {
		t.Fatalf("window %s = %+v, want sess-first %s on its own pane %s", w1, a.Claude, first, a.Pane)
	}
	if c.Claude.Transcript != second || c.Claude.Gen != "sess-second" || c.Claude.Pane != c.Pane {
		t.Fatalf("window %s = %+v, want sess-second %s on its own pane %s", w2, c.Claude, second, c.Pane)
	}
	if a.Pane == c.Pane || a.Claude.PID == c.Claude.PID {
		t.Fatalf("two windows resolved to one pane / process: %+v / %+v", a.Claude, c.Claude)
	}
	if sh.Claude != nil {
		t.Fatalf("a shell in the same cwd was taken for Claude Code: %+v", sh.Claude)
	}
	// 🔴 The per-SESSION lookup sees only the session's current window.
	if legacy := b.ClaudePanes(ctx, []string{"scratch"}); legacy["scratch"].Gen != "sess-first" {
		t.Fatalf("instrument check: the per-session lookup = %+v, want the current window's (sess-first)", legacy)
	}

	// A message to the SECOND window — not the session's current one.
	if cur := r.current("scratch"); cur != "0 "+w1 {
		t.Fatalf("fixture: the session's current window is %q, want 0 %s", cur, w1)
	}
	if err := b.Paste(ctx, *c.Claude, "for-the-second-window"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the paste in the second window", func() bool { return strings.Contains(r.windowPane(w2), "for-the-second-window") })
	if strings.Contains(r.windowPane(w1), "for-the-second-window") || strings.Contains(r.windowPane(w3), "for-the-second-window") {
		t.Fatal("a message for the second window was typed into another window")
	}
	if cur := r.current("scratch"); cur != "0 "+w1 {
		t.Fatalf("pasting into a background window moved the session's current window to %q", cur)
	}
}

// TestTmuxViewingTypingAndPastingIntoAnotherWindowNeverMovesTheOperator is THE
// property. The operator's own client is attached to `main`, looking at window
// 0. A viewer then: opens window 2 read-only; opens window 1 interactively and
// types into it; a message is pasted into window 2; and both viewers leave.
// After EVERY step the operator's client is still on window 0 of `main` — read
// from the client itself — and the session's current window has not moved.
//
// The viewer really did see and reach the OTHER windows (their markers on its
// screen, its keys in their panes), so "nothing moved" is not "nothing
// happened". While it is attached its own session is on the server and is NOT
// in what this host lists; afterwards no viewer session is left.
func TestTmuxViewingTypingAndPastingIntoAnotherWindowNeverMovesTheOperator(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "WINDOW-ZERO")
	w0 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}"))
	w1 := r.window("main", "WINDOW-ONE")
	w2 := r.window("main", "WINDOW-TWO")
	b := Tmux{Socket: r.sock}
	ctx := context.Background()

	op, err := b.Attach(ctx, "main", false, 150, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	var opScreen screen
	go opScreen.read(op)
	waitFor(t, "the operator's client to draw window 0", func() bool { return strings.Contains(opScreen.String(), "WINDOW-ZERO") })
	const home = "main"
	wantClient, wantCurrent := home+" "+w0, "0 "+w0
	still := func(step string) {
		t.Helper()
		// Give a wrong implementation time to act: a select-window is applied at once.
		time.Sleep(150 * time.Millisecond)
		if got := r.clientWindow(home); got != wantClient {
			t.Fatalf("after %s the operator's client shows %q; it was on %q — its screen was moved", step, got, wantClient)
		}
		if got := r.current(home); got != wantCurrent {
			t.Fatalf("after %s the session's current window is %q; it was %q", step, got, wantCurrent)
		}
	}
	still("nothing (the baseline)")

	// 1. A read-only viewer of window 2.
	ro, err := b.AttachWindow(ctx, "main", w2, true, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var roScreen screen
	go roScreen.read(ro)
	// 🔴 THE OPERATOR IS CHECKED BEFORE THE VIEWER'S OWN SCREEN IS. Whatever
	// AttachWindow selected, it selected before it returned; an implementation
	// that moved the OPERATOR's session instead would also leave the viewer on
	// the wrong window, and waiting for the viewer first would report that — a
	// timeout — rather than the screen it took.
	still("a read-only viewer opened window 2")
	waitFor(t, "the viewer to see window 2", func() bool { return strings.Contains(roScreen.String(), "WINDOW-TWO") })
	still("the read-only viewer drew window 2")
	if strings.Contains(roScreen.String(), "WINDOW-ZERO") {
		t.Fatal("the viewer was shown the operator's window before its own (it flashed the wrong window)")
	}
	views := r.viewSessions()
	if len(views) != 1 {
		t.Fatalf("with one viewer attached there are %d viewer sessions: %v", len(views), views)
	}
	// 🔴 destroy-unattached IS ON THE VIEWER SESSION AND NOWHERE ELSE. Set on the
	// operator's session (or globally) it would END that session the moment the
	// operator detached.
	waitFor(t, "destroy-unattached on the viewer session", func() bool {
		return strings.TrimSpace(r.run("show-options", "-t", "="+views[0]+":", "destroy-unattached")) == "destroy-unattached on"
	})
	if got := strings.TrimSpace(r.run("show-options", "-t", "=main:", "destroy-unattached")); got != "" {
		t.Fatalf("the OPERATOR's session has %q set — it would be destroyed when the operator detaches", got)
	}
	if got := strings.TrimSpace(r.run("show-options", "-g", "destroy-unattached")); got != "destroy-unattached off" {
		t.Fatalf("the GLOBAL option is %q after a viewer attached", got)
	}
	// The viewer session holds the ONE window it shows, and the operator's
	// session was not made part of anything (a session group would show here,
	// and would stay after the viewer left).
	if got := strings.Fields(r.run("list-windows", "-t", "="+views[0]+":", "-F", "#{window_id}")); !slices.Equal(got, []string{w2}) {
		t.Fatalf("the viewer session holds windows %q, want only [%s]", got, w2)
	}
	if got := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "[#{session_group}]")); got != "[]" {
		t.Fatalf("the operator's session is now in session group %s", got)
	}
	// The viewer session is on the server and is NOT mirrored.
	if list, _ := b.List(ctx); !slices.Equal(list, []string{"main"}) {
		t.Fatalf("List = %q while a viewer is attached; want only [main]", list)
	}
	ws, _ := b.Windows(ctx)
	if want := fmt.Sprintf("main:0*=%s main:1=%s main:2=%s", w0, w1, w2); ids(ws) != want {
		t.Fatalf("Windows = %s while a viewer is attached\n     want %s", ids(ws), want)
	}
	// Its keys are ignored (read-only).
	_, _ = ro.Write([]byte("ro-keys\r"))

	// 2. An interactive viewer of window 1, typing.
	rw, err := b.AttachWindow(ctx, "main", w1, false, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var rwScreen screen
	go rwScreen.read(rw)
	still("an interactive viewer opened window 1")
	waitFor(t, "the interactive viewer to see window 1", func() bool { return strings.Contains(rwScreen.String(), "WINDOW-ONE") })
	still("the interactive viewer drew window 1")
	_, _ = rw.Write([]byte("typed-by-the-viewer\r"))
	waitFor(t, "the viewer's keys in window 1", func() bool { return strings.Contains(r.windowPane(w1), "typed-by-the-viewer") })
	still("the interactive viewer typed into window 1")

	// 3. A message pasted into window 2's pane.
	two, _ := FindWindow(ws, "main", w2)
	if err := b.PastePane(ctx, two.Pane, "pasted-into-window-two"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the paste in window 2", func() bool { return strings.Contains(r.windowPane(w2), "pasted-into-window-two") })
	still("a message was pasted into window 2")

	// What each window received is exactly its own.
	if p := r.windowPane(w0); strings.Contains(p, "typed-by-the-viewer") || strings.Contains(p, "pasted-into-window-two") || strings.Contains(p, "ro-keys") {
		t.Fatalf("the operator's window received input meant for another:\n%s", p)
	}
	if strings.Contains(r.windowPane(w2), "ro-keys") {
		t.Fatal("a READ-ONLY viewer typed into the window")
	}

	// 4. Both viewers leave.
	_ = ro.Close()
	_ = rw.Close()
	waitFor(t, "the viewer sessions to be gone", func() bool { return len(r.viewSessions()) == 0 })
	still("both viewers detached")
	if got := strings.Fields(r.run("list-sessions", "-F", "#{session_name}")); !slices.Equal(got, []string{"main"}) {
		t.Fatalf("sessions after the viewers left = %q, want only [main]", got)
	}
	ws, _ = b.Windows(ctx)
	if want := fmt.Sprintf("main:0*=%s main:1=%s main:2=%s", w0, w1, w2); ids(ws) != want {
		t.Fatalf("the operator's windows after the viewers left = %s\n     want %s", ids(ws), want)
	}
	if n := len(strings.Fields(r.run("list-clients", "-F", "#{client_name}"))); n != 1 {
		t.Fatalf("%d clients are attached after the viewers left; want only the operator's", n)
	}
}

// TestTmuxAWindowViewerNeverResizesWhatTheOperatorIsLookingAt: the operator's
// client is 150x40 on window 0. A 60x20 viewer — interactive, typing, then
// resizing itself — leaves window 0 at 150x39 whether it shows that SAME window
// (the ordinary case: it is the one at the top of the list) or another one,
// under tmux's default `window-size latest` and under `smallest`, through the
// bare backend and through NoShrink.
//
// 🔴 THE CONTROL IS THE HAZARD, MEASURED IN THE SAME RIG: the same kind of
// client — of a session linking the operator's window — WITHOUT ignore-size,
// typing, shrinks it to 60x19. So the size read here can see a resize, and
// ignore-size is what prevents it.
func TestTmuxAWindowViewerNeverResizesWhatTheOperatorIsLookingAt(t *testing.T) {
	for _, policy := range []string{"latest", "smallest"} {
		r := newTmuxRig(t)
		r.run("start-server", ";", "set-option", "-g", "window-size", policy)
		r.session("main", "WINDOW-ZERO")
		w0 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}"))
		r.window("main", "WINDOW-ONE")
		w2 := r.window("main", "WINDOW-TWO")
		b := Tmux{Socket: r.sock}
		ctx := context.Background()
		op, err := b.Attach(ctx, "main", false, 150, 40)
		if err != nil {
			t.Fatal(err)
		}
		go (&screen{}).read(op)
		waitFor(t, "the operator's client to size window 0", func() bool { return r.windowSize(w0) == "150x39" })

		for _, wrap := range []struct {
			name string
			wa   WindowAttacher
		}{{"the bare backend", b}, {"NoShrink", NoShrink(b).(WindowAttacher)}} {
			for _, view := range []struct{ name, id, marker string }{
				{"the operator's own current window", w0, "WINDOW-ZERO"},
				{"another window", w2, "WINDOW-TWO"},
			} {
				what := fmt.Sprintf("window-size %s, %s, a 60x20 viewer of %s", policy, wrap.name, view.name)
				v, err := wrap.wa.AttachWindow(ctx, "main", view.id, false, 60, 20)
				if err != nil {
					t.Fatal(err)
				}
				var vs screen
				go vs.read(v)
				waitFor(t, "the viewer to draw", func() bool { return strings.Contains(vs.String(), view.marker) })
				keys := "viewer-types-" + strings.ReplaceAll(wrap.name+view.name, " ", "")
				_, _ = v.Write([]byte(keys + "\r"))
				waitFor(t, "the viewer's keys", func() bool { return strings.Contains(r.windowPane(view.id), keys) })
				time.Sleep(300 * time.Millisecond)
				if got := r.windowSize(w0); got != "150x39" {
					t.Fatalf("%s: resized the operator's window 0 to %s (want 150x39)", what, got)
				}
				_ = v.Resize(40, 12)
				time.Sleep(200 * time.Millisecond)
				if got := r.windowSize(w0); got != "150x39" {
					t.Fatalf("%s: the viewer resizing itself resized the operator's window 0 to %s", what, got)
				}
				_ = v.Close()
				waitFor(t, "the viewer session to be gone", func() bool { return len(r.viewSessions()) == 0 })
				if got := r.windowSize(w0); got != "150x39" {
					t.Fatalf("%s: the viewer leaving left the operator's window 0 at %s", what, got)
				}
			}
		}

		// Control: a session linking the operator's own window, attached
		// WITHOUT ignore-size.
		r.run("new-session", "-d", "-s", "plain-link", "", ";", "link-window", "-k", "-s", w0, "-t", "=plain-link:{start}")
		plain, err := b.client(append(b.server(), "-u", "attach-session", "-t", "=plain-link"), 60, 20, nil)
		if err != nil {
			t.Fatal(err)
		}
		var ps screen
		go ps.read(plain)
		waitFor(t, "the control client to draw", func() bool { return strings.Contains(ps.String(), "WINDOW-ZERO") })
		_, _ = plain.Write([]byte("control\r"))
		waitFor(t, "CONTROL: a client without ignore-size shrinks the operator's window (window-size "+policy+")", func() bool {
			return r.windowSize(w0) == "60x19"
		})
		_ = plain.Close()
		_ = op.Close()
	}
}

// TestTmuxALoneWindowViewerDoesNotShrinkTheWindow: with NOBODY else attached
// tmux does not honour ignore-size (the control below: a bare 60x20 viewer
// leaves a 120x40 window at 60x19). Through NoShrink the viewer's client takes
// the WINDOW's size instead — read for that window, not for the session's
// current one, which here is a different size — and says so in-band.
func TestTmuxALoneWindowViewerDoesNotShrinkTheWindow(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "WINDOW-ZERO")
	w1 := r.window("main", "WINDOW-ONE")
	// The CURRENT window is made a different size from the one viewed, so a
	// lookup that read the session's current window would answer 90x25.
	r.run("resize-window", "-t", "=main:0", "-x", "90", "-y", "25")
	if got := r.windowSize(w1); got != "120x40" {
		t.Fatalf("fixture: window 1 is %s", got)
	}
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	if got, err := b.WindowSizeOf(ctx, "main", w1); err != nil || got.Cols != 120 || got.Rows != 41 {
		t.Fatalf("WindowSizeOf(window 1) = %+v, %v; want 120x41 (the window plus its status line)", got, err)
	}
	// tmux answers for the session's CURRENT window when the id is not in it;
	// that answer is refused rather than believed.
	r.session("other", "OTHER")
	if got, err := b.WindowSizeOf(ctx, "other", w1); err == nil {
		t.Fatalf("WindowSizeOf(other, a window of main) = %+v; want an error, not another window's size", got)
	}

	v, err := NoShrink(b).(WindowAttacher).AttachWindow(ctx, "main", w1, true, 60, 20)
	if err != nil {
		t.Fatal(err)
	}
	var vs screen
	go vs.read(v)
	waitFor(t, "the viewer to draw", func() bool { return strings.Contains(vs.String(), "WINDOW-ONE") })
	time.Sleep(300 * time.Millisecond)
	if got := r.windowSize(w1); got != "120x40" {
		t.Fatalf("a lone 60x20 viewer through NoShrink resized the window to %s (want 120x40)", got)
	}
	if !strings.HasPrefix(vs.String(), "\x1b]7701;120;41\x07") {
		t.Fatalf("the viewer was not told its client is 120x41 first: %q", vs.String()[:min(len(vs.String()), 40)])
	}
	_ = v.Close()
	waitFor(t, "the viewer session to be gone", func() bool { return len(r.viewSessions()) == 0 })

	// Control: the bare backend, same request.
	bare, err := b.AttachWindow(ctx, "main", w1, true, 60, 20)
	if err != nil {
		t.Fatal(err)
	}
	go (&screen{}).read(bare)
	waitFor(t, "CONTROL: a lone viewer WITHOUT NoShrink shrinks the window", func() bool { return r.windowSize(w1) == "60x19" })
	_ = bare.Close()
}

// TestTmuxAViewerSessionDiesWithItsClientAndStaleOnesAreCleaned: a viewer
// session is removed by tmux itself the moment its client is gone — here the
// client is SIGKILLed, which is what happens to it when muster-mirror is killed
// outright — and a viewer session that never got a client (a muster-mirror
// killed between creating it and attaching) is ended by CleanViewSessions,
// which ends NOTHING else: not a viewer session in use, not a session whose
// name merely starts the same way, not the operator's.
func TestTmuxAViewerSessionDiesWithItsClientAndStaleOnesAreCleaned(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "WINDOW-ZERO")
	w0 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}"))
	w1 := r.window("main", "WINDOW-ONE")
	b := Tmux{Socket: r.sock}
	ctx := context.Background()

	v, err := b.AttachWindow(ctx, "main", w1, true, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var vs screen
	go vs.read(v)
	waitFor(t, "the viewer to draw", func() bool { return strings.Contains(vs.String(), "WINDOW-ONE") })
	if got := r.viewSessions(); len(got) != 1 {
		t.Fatalf("viewer sessions = %v", got)
	}
	// The client is killed with no chance to detach, and Close is NOT called.
	if err := v.(*ptyTerm).cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tmux to remove the dead client's viewer session by itself", func() bool { return len(r.viewSessions()) == 0 })

	// Stale and not-stale, side by side.
	const stale, inUse, lookalike = "muster-view-aaaaaaaaaaaa", "muster-view-bbbbbbbbbbbb", "muster-view-keep"
	r.run("new-session", "-d", "-s", stale, "-t", "=main")
	r.run("new-session", "-d", "-s", inUse, "-t", "=main")
	r.session(lookalike, "THE-OPERATORS")
	held, err := b.client(append(b.server(), "-u", "attach-session", "-f", ViewClientFlags(true), "-t", "="+inUse), 80, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	go (&screen{}).read(held)
	waitFor(t, "the in-use viewer session to have its client", func() bool {
		return strings.TrimSpace(r.run("display-message", "-p", "-t", "="+inUse+":", "#{session_attached}")) == "1"
	})

	ended, err := b.CleanViewSessions(ctx)
	if err != nil || !slices.Equal(ended, []string{stale}) {
		t.Fatalf("CleanViewSessions ended %q (%v); want exactly [%s]", ended, err, stale)
	}
	left := strings.Split(strings.TrimSpace(r.run("list-sessions", "-F", "#{session_name}")), "\n")
	slices.Sort(left)
	if want := []string{"main", inUse, lookalike}; !slices.Equal(left, want) {
		t.Fatalf("sessions after the clean-up = %q, want %q", left, want)
	}
	// And the operator's windows are all still there.
	ws, _ := b.Windows(ctx)
	if want := fmt.Sprintf("main:0*=%s main:1=%s", w0, w1); !strings.HasPrefix(ids(ws), want) {
		t.Fatalf("the operator's windows after the clean-up = %s", ids(ws))
	}
	if again, _ := b.CleanViewSessions(ctx); len(again) != 0 {
		t.Fatalf("a second clean-up ended %q", again)
	}
}

// hookLog arms the server-wide hooks named, each appending its name to a file,
// and returns a reader of how many times each has fired.
func (r *tmuxRig) hookLog(names ...string) func() map[string]int {
	r.t.Helper()
	file := filepath.Join(r.t.TempDir(), "hooks")
	for _, h := range names {
		r.run("set-hook", "-g", h, "run-shell \"echo "+h+" >> "+file+"\"")
	}
	return func() map[string]int {
		b, _ := os.ReadFile(file)
		out := map[string]int{}
		for _, line := range strings.Fields(string(b)) {
			out[line]++
		}
		return out
	}
}

// TestTmuxAttachWindowRefusesWhatIsNotThereAndStartsNothing: a window id that
// is not in the session, a session that is gone (even when the id names a live
// window of ANOTHER session), and an id that is not an id are each refused —
// with no viewer session left, and the operator's sessions, windows and panes
// exactly as they were.
//
// 🔴 AND THE OPERATOR'S SHELL NEVER RUNS, for a refused attach or a successful
// one. A viewer session needs a first window, which tmux would fill with the
// server's default command. So the default command is a marker-writer here
// (immune to the hangup that removing its window sends), and the marker must
// never appear. The control is a plain new-window, which does write it.
//
// ⚠ WHAT THIS DOES NOT SHOW: that no process is FORKED. A placeholder running
// the default command is replaced so fast that it never gets to write (measured
// — this test passes with the empty command removed from AttachWindow), so the
// marker says "nothing of the operator's ran", which is the property that
// matters, and not "nothing was started".
func TestTmuxAttachWindowRefusesWhatIsNotThereAndStartsNothing(t *testing.T) {
	r := newTmuxRig(t)
	marker := filepath.Join(t.TempDir(), "the-default-command-ran")
	r.session("main", "WINDOW-ZERO")
	r.session("other", "OTHER")
	r.run("set-option", "-g", "default-command", "sh -c 'trap \"\" HUP TERM; echo ran >> "+marker+"; sleep 1'")
	mainWin := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}"))
	otherWin := strings.TrimSpace(r.run("display-message", "-p", "-t", "=other:", "#{window_id}"))
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	state := func() string {
		return r.run("list-panes", "-a", "-F", "#{session_name} #{window_id} #{window_index} #{pane_id} #{pane_pid} [#{session_group}] #{window_linked}")
	}
	before := state()
	for name, tc := range map[string][2]string{
		"a window of another session":                  {"main", otherWin},
		"a window that never existed":                  {"main", "@987"},
		"a session that is gone, a window that is not": {"vanished", otherWin},
		"an index, not an id":                          {"main", "0"},
		"a prefix of a session name":                   {"mai", mainWin},
	} {
		term, err := b.AttachWindow(ctx, tc[0], tc[1], true, 80, 24)
		if err == nil {
			_ = term.Close()
			t.Errorf("%s: AttachWindow(%q, %q) attached", name, tc[0], tc[1])
		}
		if got := r.viewSessions(); len(got) != 0 {
			t.Fatalf("%s: a refused attach left viewer sessions %v", name, got)
		}
	}
	// A successful attach, for the same measurement.
	term, err := b.AttachWindow(ctx, "main", mainWin, true, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var sc screen
	go sc.read(term)
	waitFor(t, "the viewer to draw", func() bool { return strings.Contains(sc.String(), "WINDOW-ZERO") })
	_ = term.Close()
	waitFor(t, "the viewer session to be gone", func() bool { return len(r.viewSessions()) == 0 })

	if after := state(); after != before {
		t.Fatalf("the attaches changed the server:\nbefore:\n%safter:\n%s", before, after)
	}
	if got := r.current("main"); !strings.HasPrefix(got, "0 ") {
		t.Fatalf("the operator's current window is %q after the attaches", got)
	}
	time.Sleep(500 * time.Millisecond) // a started process writes its marker at once
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the server's default command — the operator's shell — was run by an attach (marker: %q)", b)
	}
	// Control: the marker CAN be written — a plain new-window runs the default command.
	r.run("new-window", "-d", "-t", "=other:")
	waitFor(t, "CONTROL: a new window writes the marker", func() bool { _, err := os.ReadFile(marker); return err == nil })
}

// TestTmuxAViewerSessionSelectsNothingAndLinksTwiceWhateverTheSessionsSize is
// the measurement behind showing a window through a LINK rather than a session
// group. An operator's tmux.conf may bind the server's hooks to per-window
// bookkeeping; whatever a viewer fires, it fires in the viewer session's
// context, on every open. Counted here on a session of SIX windows:
//
//   - `after-select-window` never fires: the viewer session's current window is
//     set by the link, not by a select-window;
//   - `window-linked` fires exactly twice — the placeholder and the link —
//     whatever the session's size;
//   - `session-created` once.
//
// The controls make both zeroes and both counts mean something: an explicit
// select-window is counted, and joining the same session as a GROUP (the
// alternative) fires window-linked once per window and more.
func TestTmuxAViewerSessionSelectsNothingAndLinksTwiceWhateverTheSessionsSize(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "WINDOW-ZERO")
	var last string
	for i := 1; i <= 5; i++ {
		last = r.window("main", fmt.Sprintf("WINDOW-%d", i))
	}
	fired := r.hookLog("after-select-window", "window-linked", "session-created")
	b := Tmux{Socket: r.sock}
	term, err := b.AttachWindow(context.Background(), "main", last, false, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var sc screen
	go sc.read(term)
	waitFor(t, "the viewer to draw the sixth window", func() bool { return strings.Contains(sc.String(), "WINDOW-5") })
	waitFor(t, "the hooks to be recorded", func() bool { return fired()["session-created"] == 1 && fired()["window-linked"] >= 2 })
	time.Sleep(300 * time.Millisecond)
	if got := fired(); got["after-select-window"] != 0 || got["window-linked"] != 2 || got["session-created"] != 1 {
		t.Fatalf("a viewer of one window of a six-window session fired %v; want after-select-window 0, window-linked 2, session-created 1", got)
	}
	_ = term.Close()
	waitFor(t, "the viewer session to be gone", func() bool { return len(r.viewSessions()) == 0 })

	// Control 1: a select-window IS counted.
	r.run("select-window", "-t", "=main:1")
	waitFor(t, "CONTROL: an explicit select-window fires its hook", func() bool { return fired()["after-select-window"] == 1 })
	// Control 2: the alternative — a session group — links every window.
	base := fired()["window-linked"]
	r.run("new-session", "-d", "-s", "a-group", "-t", "=main")
	waitFor(t, "CONTROL: joining the session as a group fires window-linked once per window", func() bool {
		return fired()["window-linked"]-base >= 6
	})
}
