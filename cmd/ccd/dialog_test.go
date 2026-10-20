package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// askdriveFixture is a capture of the real CLI's pane from internal/askdrive's
// testdata (Claude Code 2.1.289).
func askdriveFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "askdrive", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What dialogOnScreen reads, against captures of the real CLI.
func TestDialogOnScreenReadsTheRealCLIsScreens(t *testing.T) {
	for _, name := range []string{
		"not-picker-permission.txt", // the permission prompt
		"single-q.txt", "multi-q.txt", "three-q1.txt", "three-q1-other.txt", "three-q2-next.txt",
		"three-review.txt", "multi-review.txt", "long-q2-clipped-80x20.txt", // the AskUserQuestion picker
	} {
		if what, shown := dialogOnScreen(askdriveFixture(t, name)); !shown {
			t.Errorf("%s: no dialog read; want one", name)
		} else {
			t.Logf("%s: %s", name, what)
		}
	}
	for _, name := range []string{
		"multi-done.txt", "single-done.txt", "three-done.txt", // the CLI at its prompt after a turn
		"not-picker-prose.txt", // prose quoting the picker's and prompt's words
	} {
		if what, shown := dialogOnScreen(askdriveFixture(t, name)); shown {
			t.Errorf("%s: read as %s; want no dialog", name, what)
		}
	}
	// The PINNED CLI (2.1.296), captured in the image: at its prompt on a fresh
	// start, and at a Bash permission prompt (a turn against a stub model API).
	if what, shown := dialogOnScreen(string(fixture(t, "pane_idle_2.1.296.txt"))); shown {
		t.Errorf("pane_idle_2.1.296.txt: read as %s; want no dialog", what)
	}
	if _, shown := dialogOnScreen(string(fixture(t, "pane_permission_2.1.296.txt"))); !shown {
		t.Error("pane_permission_2.1.296.txt: no dialog read; want the permission prompt")
	}
}

// Each of the permission prompt's two marks is enough alone, and neither counts
// above the screen's last rule — where the input box is, holding whatever an
// operator (or ccd's own paste) put there.
func TestDialogOnScreenEachMarkAloneAndOnlyBelowTheLastRule(t *testing.T) {
	rule := strings.Repeat("─", 80)
	perm := askdriveFixture(t, "not-picker-permission.txt")
	noFooter := strings.Replace(perm, "Esc to cancel", "Something else", 1)
	if _, shown := dialogOnScreen(noFooter); !shown {
		t.Error("the select cursor alone (no footer) was not read as a dialog")
	}
	noCursor := strings.Replace(perm, "❯ 1. Yes", "  1. Yes", 1)
	if _, shown := dialogOnScreen(noCursor); !shown {
		t.Error("the footer alone (no cursor) was not read as a dialog")
	}
	if _, shown := dialogOnScreen(strings.Replace(noCursor, "Esc to cancel", "Something else", 1)); shown {
		t.Error("control: with neither mark the permission prompt still read as a dialog")
	}
	// The cursor with a no-break space after it.
	if _, shown := dialogOnScreen(strings.Replace(noFooter, "❯ 1.", "❯ 1.", 1)); !shown {
		t.Error("the select cursor followed by a no-break space was not read")
	}
	// The same lines inside the input box (above the last rule) are not a dialog.
	box := "earlier output\n" + rule + "\n❯ 1. Yes\n  Esc to cancel · Tab to amend\n" + rule + "\n  ? for shortcuts\n"
	if what, shown := dialogOnScreen(box); shown {
		t.Errorf("text in the input box read as %s", what)
	}
	// Control: the same lines with no rule below them are.
	if _, shown := dialogOnScreen("earlier output\n" + rule + "\n❯ 1. Yes\n  Esc to cancel · Tab to amend\n"); !shown {
		t.Error("control: the lines below the last rule were not read as a dialog")
	}
}

// fakeTmuxScreen is fakeTmux (afterpaste_test.go) whose pane is live and whose
// capture-pane prints the file `screen`.
const fakeTmuxScreen = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
case "$*" in
  *display-message*) echo "4242 1791600000 %1 0  " ;;
  *capture-pane*) cat "$dir/screen" ;;
  *load-buffer*) cat >/dev/null ;;
esac
exit 0
`

func fakeTermScreen(t *testing.T, screen string) (tmuxTerminal, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fakeTmuxScreen), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "screen"), []byte(screen), 0o644); err != nil {
		t.Fatal(err)
	}
	return tmuxTerminal{bin: filepath.Join(dir, "tmux")}, dir
}

// 🔴 NOTHING IS TYPED INTO A PANE SHOWING A DIALOG: not the paste, and not an
// Enter (runTurn's submit retry, or the paste's own). The control: the same
// terminal with the CLI at its prompt pastes and presses Enter.
func TestTheTerminalTypesNothingIntoADialog(t *testing.T) {
	ref := paneRef{Server: "4242 1791600000", Pane: "%1"}
	term, dir := fakeTermScreen(t, askdriveFixture(t, "not-picker-permission.txt"))
	err := term.Paste(context.Background(), ref, "hello")
	if !errors.Is(err, errDialogShown) || errors.Is(err, errPaneNotLive) {
		t.Fatalf("Paste into a dialog: %v; want errDialogShown", err)
	}
	if err := term.Enter(context.Background(), ref); !errors.Is(err, errDialogShown) {
		t.Fatalf("Enter into a dialog: %v; want errDialogShown", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "capture-pane") {
		t.Fatalf("instrument check: the screen was never read:\n%s", calls)
	}
	if strings.Contains(string(calls), "paste-buffer") || strings.Contains(string(calls), "send-keys") {
		t.Fatalf("keys reached a pane showing a dialog:\n%s", calls)
	}

	term, dir = fakeTermScreen(t, askdriveFixture(t, "multi-done.txt"))
	if err := term.Paste(context.Background(), ref, "hello"); err != nil {
		t.Fatalf("control: Paste at the prompt: %v", err)
	}
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "paste-buffer") || !strings.Contains(string(calls), "send-keys") {
		t.Fatalf("control: the prompt was not pasted and submitted:\n%s", calls)
	}
}

// A screen that cannot be read is refused before anything is typed, as
// not_ready (nothing was pasted, so the caller may send again).
func TestAnUnreadableScreenIsRefusedBeforeTyping(t *testing.T) {
	dir := t.TempDir()
	script := strings.Replace(fakeTmuxScreen, `*capture-pane*) cat "$dir/screen" ;;`,
		`*capture-pane*) echo "no server" >&2; exit 1 ;;`, 1)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	term := tmuxTerminal{bin: filepath.Join(dir, "tmux")}
	err := term.Paste(context.Background(), paneRef{Server: "4242 1791600000", Pane: "%1"}, "hello")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !errors.Is(err, errPaneNotLive) || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("Paste with an unreadable screen: %v; want errPaneNotLive naming the failure", err)
	}
	if strings.Contains(string(calls), "paste-buffer") {
		t.Fatalf("pasted with an unreadable screen:\n%s", calls)
	}
}

// dialogTerm is a terminal whose pane shows a dialog (tmuxTerminal's refusal)
// until clear is set.
type dialogTerm struct{ clear bool }

func (d *dialogTerm) Paste(context.Context, paneRef, string) error {
	if d.clear {
		return nil
	}
	return errors.Join(errDialogShown, errors.New("a permission prompt"))
}
func (d *dialogTerm) Enter(context.Context, paneRef) error { return errDialogShown }
func (d *dialogTerm) Alive(context.Context, paneRef) error { return nil }

// The terminal's dialog refusal is the turn's 409 busy, code awaiting_approval
// — the refusal muster reads as "nothing was pasted".
func TestADialogOnScreenIsABusyAwaitingApprovalRefusal(t *testing.T) {
	srv := newServer(serverConfig{Bearer: "b", ConfigDir: t.TempDir()}, &dialogTerm{}, newAuthTracker())
	srv.inputPane = func() (paneRef, bool) { return fakeRef, true }
	srv.onHook(hookEvent{Event: "SessionStart", SessionID: "s"})
	_, f := srv.runTurn(context.Background(), "hello")
	if f == nil || f.Status != http.StatusConflict || f.Type != failBusy || f.Code != codeAwaitingApproval {
		t.Fatalf("runTurn with a dialog on screen: %+v; want 409 busy code %s", f, codeAwaitingApproval)
	}
}

// 🔴 WORDS ARE NOT A DIALOG: text that quotes a picker's or a permission prompt's
// lines, in the transcript or in the input box, is not one. Read as one, it
// refuses every turn for as long as it is on screen — and an idle screen does not
// change by itself, so the agent is wedged. Real captures (2.1.296, the image):
// a reply quoting "Review your answers", "❯ 1. Submit answers", "❯ 1. Yes" and
// "Esc to cancel · Tab to amend" above the prompt; a multi-line paste of those
// words in the box (the CLI collapses it); a short one it draws verbatim. And
// the idle capture with the two picker lines written into the transcript and into
// the box, as the audit described.
func TestDialogWordsInTheTranscriptOrTheInputBoxAreNotADialog(t *testing.T) {
	for _, name := range []string{
		"pane_idle_quoted_transcript_2.1.296.txt",
		"pane_idle_quoted_inputbox_2.1.296.txt",
		"pane_idle_dialogwords_inputbox_2.1.296.txt",
	} {
		if what, shown := dialogOnScreen(string(fixture(t, name))); shown {
			t.Errorf("%s: read as %s; want no dialog", name, what)
		}
	}
	idle := string(fixture(t, "pane_idle_2.1.296.txt"))
	lines := strings.Split(idle, "\n")
	first, prompt := -1, -1
	for i, l := range lines {
		if isRule(l) && first < 0 {
			first = i
		}
		if strings.HasPrefix(l, "❯") {
			prompt = i
		}
	}
	if first < 0 || prompt != first+1 {
		t.Fatalf("instrument check: the idle capture's input box is not where expected (rule %d, prompt %d)", first, prompt)
	}
	words := []string{"  Review your answers", "", "  1. Submit answers", "  2. Cancel"}
	inject := func(at int) string {
		out := append(append(append([]string{}, lines[:at]...), words...), lines[at:]...)
		return strings.Join(out, "\n")
	}
	for where, screen := range map[string]string{
		"in the transcript": inject(first),
		"in the input box":  inject(prompt + 1),
	} {
		if what, shown := dialogOnScreen(screen); shown {
			t.Errorf("the picker's words %s: read as %s; want no dialog", where, what)
		}
	}
	// Control: the same words AS the bottom of the screen are a picker's review.
	if _, shown := dialogOnScreen(strings.Join(append(append([]string{}, lines[:first]...), "Review your answers", "",
		"❯ 1. Submit answers", "  2. Cancel"), "\n")); !shown {
		t.Error("control: a review screen at the bottom was not read as a dialog")
	}
}

// A turn refused for a dialog shows on the agent's awaiting-approval badge, as
// its own source, and the next turn that is pasted withdraws it — a refusal is
// never only a silent retry.
func TestADialogRefusalShowsOnTheBadgeUntilATurnIsPasted(t *testing.T) {
	term := &dialogTerm{}
	srv := newServer(serverConfig{Bearer: "b", ConfigDir: t.TempDir(), SubmitTimeout: 50 * time.Millisecond}, term, newAuthTracker())
	srv.blocked = &blockedReporter{ch: make(chan bool, 1)}
	srv.inputPane = func() (paneRef, bool) { return fakeRef, true }
	srv.onHook(hookEvent{Event: "SessionStart", SessionID: "s"})
	if _, f := srv.runTurn(context.Background(), "hello"); f == nil || f.Code != codeAwaitingApproval {
		t.Fatalf("instrument check: the turn was not refused for the dialog: %+v", f)
	}
	select {
	case v := <-srv.blocked.ch:
		if !v {
			t.Fatal("the refusal reported blocked=false")
		}
	default:
		t.Fatal("the refusal did not reach the badge")
	}
	if srv.awaitingApproval {
		t.Fatal("instrument check: no hook said a prompt was up; awaitingApproval should be false")
	}
	term.clear = true
	_, _ = srv.runTurn(context.Background(), "hello") // pasted; it then times out waiting for its submit
	select {
	case v := <-srv.blocked.ch:
		if v {
			t.Fatal("the pasted turn reported blocked=true")
		}
	default:
		t.Fatal("the pasted turn did not withdraw the badge")
	}
}

// The screen source is withdrawn by a hook that says the session moved on (as
// awaitingApproval is), and by its lane's stop: a stopped lane's screen is gone.
func TestTheScreenBadgeIsWithdrawnByAHookAndByTheLanesStop(t *testing.T) {
	rep := &blockedReporter{ch: make(chan bool, 1)}
	srv := newServer(serverConfig{}, nil, newAuthTracker())
	srv.blocked = rep
	srv.setScreenBlocked(true, "a permission prompt")
	if v := <-rep.ch; !v {
		t.Fatal("instrument check: the refusal did not set the badge")
	}
	srv.onHook(hookEvent{Event: "Stop"})
	select {
	case v := <-rep.ch:
		if v {
			t.Fatal("the Stop reported blocked=true")
		}
	default:
		t.Fatal("a Stop did not withdraw the screen badge")
	}

	ls := newServer(serverConfig{}, nil, newAuthTracker())
	ls.blocked, ls.blockedSrc = rep, "aaaaaaaaaaaa"
	ln := &lane{id: "aaaaaaaaaaaa", srv: ls}
	ls.setScreenBlocked(true, "a permission prompt")
	if v := <-rep.ch; !v {
		t.Fatal("instrument check: the lane's refusal did not set the badge")
	}
	ln.clearBlocked()
	select {
	case v := <-rep.ch:
		if v {
			t.Fatal("the lane's stop reported blocked=true")
		}
	default:
		t.Fatal("the lane's stop did not withdraw its screen badge")
	}
}

// The screen source is the session's OWN source beside the hook's: withdrawing
// it leaves a permission prompt a hook reported on the badge.
func TestWithdrawingTheScreenBadgeLeavesTheHooksPrompt(t *testing.T) {
	rep := &blockedReporter{ch: make(chan bool, 1)}
	srv := newServer(serverConfig{}, nil, newAuthTracker())
	srv.blocked = rep
	srv.onHook(hookEvent{Event: "Notification", NotificationType: "permission_prompt"})
	if v := <-rep.ch; !v {
		t.Fatal("instrument check: the prompt did not set the badge")
	}
	srv.setScreenBlocked(true, "a permission prompt")
	srv.setScreenBlocked(false, "")
	select {
	case v := <-rep.ch:
		t.Fatalf("withdrawing the screen source reported %v while the hook's prompt is still up", v)
	default:
	}
}

// A lane's id is laneID(key), the same for the next CLI started for that
// session, and the reporter's sources outlive the server that raised one. A
// raised screen source left behind by a lane's CLI that died is withdrawn by the
// NEXT lane server's first clear, although that server never raised it itself.
func TestANewLaneServerWithdrawsAScreenSourceItsPredecessorLeftRaised(t *testing.T) {
	rep := &blockedReporter{ch: make(chan bool, 1)}
	id := laneID("k")
	old := newServer(serverConfig{}, nil, newAuthTracker())
	old.blocked, old.blockedSrc = rep, id
	old.setScreenBlocked(true, "a permission prompt")
	if !rep.sources[screenSource(id)] {
		t.Fatalf("instrument check: the old lane's screen source is not raised: %v", rep.sources)
	}
	// The old CLI dies; a new server serves the same lane id.
	next := newServer(serverConfig{}, nil, newAuthTracker())
	next.blocked, next.blockedSrc = rep, id
	next.setScreenBlocked(false, "") // its first pasted turn
	if _, raised := rep.sources[screenSource(id)]; raised {
		t.Fatalf("the predecessor's screen source is still raised: %v", rep.sources)
	}
	// The same through a hook that says the session moved on.
	old.setScreenBlocked(false, "")
	old.setScreenBlocked(true, "a permission prompt")
	next2 := newServer(serverConfig{}, nil, newAuthTracker())
	next2.blocked, next2.blockedSrc = rep, id
	next2.onHook(hookEvent{Event: "SessionStart", SessionID: "s"})
	if _, raised := rep.sources[screenSource(id)]; raised {
		t.Fatalf("a SessionStart in the new lane did not withdraw the predecessor's screen source: %v", rep.sources)
	}
}
