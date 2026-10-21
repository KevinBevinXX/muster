package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/termsize"
)

// ---------------------------------------------------------------------------
// AttachTerminal's release: every attach that may have started a process in
// the pod is followed by a second exec that ends it — whatever ended the
// attach. Real tmux and a modelled container runtime are cmd/ccd's tmuxit
// suite (TestTmuxTerminalClosedStreamLeavesNoClient); this is each exit path.
// ---------------------------------------------------------------------------

// termDriver records the terminal exec and the release exec. ExecTTY behaves
// as attach says; the stream "starts" once it is called.
type termDriver struct {
	*provision.Noop
	attach func(ctx context.Context) error

	mu           sync.Mutex
	ttyCmd       []string
	releases     int
	releaseCmd   []string
	releaseIn    string
	releaseErr   error // checked when the release ran: its context's state
	releaseLeft  time.Duration
	releaseFails error // returned from the release

	// The window-size query (agents.TerminalSizeCommand) is answered with
	// window — or refused when it is "" — and is NOT a release.
	window      string
	sizeQueries int
	// tty, when set, is the attach's body in place of attach: it sees the
	// writer and the size channel the adapter hands the exec.
	tty func(ctx context.Context, stdout io.Writer, resize <-chan provision.TermSize) error
	// bare counts execs that arrived WITHOUT agentExecCommand's credential scrub.
	bare int
}

func (d *termDriver) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

// unscrub returns cmd without agentExecCommand's prefix, and false when the
// prefix is missing (an exec that did not strip the turn credentials).
func unscrub(cmd []string) ([]string, bool) {
	prefix := agentExecCommand(nil)
	if len(cmd) < len(prefix) || strings.Join(cmd[:len(prefix)], "\x00") != strings.Join(prefix, "\x00") {
		return cmd, false
	}
	return cmd[len(prefix):], true
}

func (d *termDriver) ExecTTY(ctx context.Context, _ provision.Ref, cmd []string, _ io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	cmd, ok := unscrub(cmd)
	d.mu.Lock()
	if !ok {
		d.bare++
	}
	d.ttyCmd = cmd
	tty := d.tty
	d.mu.Unlock()
	if tty != nil {
		return tty(ctx, stdout, resize)
	}
	return d.attach(ctx)
}

func (d *termDriver) Exec(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout, _ io.Writer) error {
	in, _ := io.ReadAll(stdin)
	cmd, ok := unscrub(cmd)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !ok {
		d.bare++
	}
	if strings.Join(cmd, "\x00") == strings.Join(agents.TerminalSizeCommand(termsize.TmuxWindowFormat), "\x00") {
		d.sizeQueries++
		if d.window == "" {
			return errors.New("can't find session: cc")
		}
		_, _ = io.WriteString(stdout, d.window)
		return nil
	}
	d.releases++
	d.releaseCmd = cmd
	d.releaseIn = string(in)
	d.releaseErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		d.releaseLeft = time.Until(dl)
	}
	if d.releaseFails != nil {
		return d.releaseFails
	}
	_, _ = io.WriteString(stdout, "released 1\n")
	return nil
}

func newTermAdapter(t *testing.T, attach func(ctx context.Context) error) (*Adapter, *termDriver, *bytes.Buffer) {
	t.Helper()
	d := &termDriver{Noop: provision.MustNewNoop(), attach: attach}
	var logs bytes.Buffer
	a, err := New(Config{Driver: d, Store: &recordingStore{agent: fixtureAgent()}, Spec: fixtureSpecConfig(),
		Logger: log.New(&lockedBuf{b: &logs}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return a, d, &logs
}

type lockedBuf struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func claudeCodeAgent() agents.Agent {
	return agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode}
}

// checkReleased asserts exactly one release ran, for the tag the attach was
// started with, on a live context with the release's own budget.
func checkReleased(t *testing.T, d *termDriver) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.releases != 1 {
		t.Fatalf("want exactly 1 release, got %d", d.releases)
	}
	if d.bare != 0 {
		t.Fatalf("%d exec(s) reached the pod without the turn-credential scrub", d.bare)
	}
	if strings.Join(d.releaseCmd, "\x00") != strings.Join(agents.TerminalReleaseCommand(), "\x00") {
		t.Fatalf("the release ran %q, not the release command", d.releaseCmd)
	}
	if len(d.ttyCmd) < 5 {
		t.Fatalf("the attach command %q carries no tag", d.ttyCmd)
	}
	if want := agents.TerminalReleaseInput(agents.TerminalTag(d.ttyCmd[4])); d.releaseIn != want {
		t.Fatalf("the release was told %q; the attach was tagged %q", d.releaseIn, d.ttyCmd[4])
	}
	if d.releaseErr != nil {
		t.Fatalf("the release ran on a context that was already done: %v", d.releaseErr)
	}
	if d.releaseLeft <= TerminalReleaseTimeout/2 || d.releaseLeft > TerminalReleaseTimeout {
		t.Fatalf("the release's budget is %s, want its own %s", d.releaseLeft, TerminalReleaseTimeout)
	}
}

// TestAttachTerminalReleasesOnEveryExitThatMayHaveStartedAProcess: the exits a
// browser session can end by — its context cancelled (a closed tab, a dropped
// connection the keepalive noticed, a mode switch's close, muster's shutdown:
// all of them cancel the attach's context), the stream failing, and the client
// exiting on its own — each run exactly one release, for the attach's own
// tag, and the attach's own result is what the caller gets.
func TestAttachTerminalReleasesOnEveryExitThatMayHaveStartedAProcess(t *testing.T) {
	streamErr := errors.New("exec into keen-wren: stream reset")
	for _, c := range []struct {
		name    string
		cancel  bool
		returns error
		want    error
	}{
		{name: "context cancelled (tab closed, connection dropped, mode switch, shutdown)", cancel: true, want: context.Canceled},
		{name: "stream error", returns: streamErr, want: streamErr},
		{name: "client exited", returns: nil, want: nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, d, _ := newTermAdapter(t, func(ctx context.Context) error {
				if c.cancel {
					<-ctx.Done()
					return ctx.Err()
				}
				return c.returns
			})
			ctx, cancel := context.WithCancel(context.Background())
			if c.cancel {
				cancel()
			} else {
				defer cancel()
			}
			err := a.AttachTerminal(ctx, claudeCodeAgent(), true, strings.NewReader(""), io.Discard, nil)
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
				t.Fatalf("AttachTerminal returned %v, want %v", err, c.want)
			}
			checkReleased(t, d)
		})
	}
}

// TestAttachTerminalSkipsTheReleaseOnlyForARefusalBeforeAnyStream: a driver
// that cannot exec, an exec the cluster refused (RBAC is ErrUnsupported), no
// pod, an invalid command — none can have started a process, so no second
// exec is sent into a pod that refused or lacks the first.
func TestAttachTerminalSkipsTheReleaseOnlyForARefusalBeforeAnyStream(t *testing.T) {
	for _, refusal := range []error{
		fmt.Errorf("%w: the cluster refused to exec", provision.ErrUnsupported),
		fmt.Errorf("%w: no running pod", provision.ErrNotFound),
		fmt.Errorf("%w: bad command", provision.ErrInvalidSpec),
	} {
		a, d, _ := newTermAdapter(t, func(context.Context) error { return refusal })
		err := a.AttachTerminal(context.Background(), claudeCodeAgent(), false, strings.NewReader(""), io.Discard, nil)
		if !errors.Is(err, refusal) {
			t.Fatalf("AttachTerminal returned %v, want %v", err, refusal)
		}
		if d.releases != 0 {
			t.Errorf("%v: a release ran after a refusal before any stream", refusal)
		}
	}
}

// TestAttachTerminalRefusesAKindWithoutATerminalBeforeAnyExec: a gateway agent
// gets ErrNoTerminal, and neither exec is sent.
func TestAttachTerminalRefusesAKindWithoutATerminalBeforeAnyExec(t *testing.T) {
	a, d, _ := newTermAdapter(t, func(context.Context) error {
		t.Fatal("the terminal exec was sent for a gateway agent")
		return nil
	})
	err := a.AttachTerminal(context.Background(), agents.Agent{ID: 8, Name: "plain-otter", Kind: agents.KindGateway},
		true, strings.NewReader(""), io.Discard, nil)
	if !errors.Is(err, agents.ErrNoTerminal) {
		t.Fatalf("want ErrNoTerminal, got %v", err)
	}
	if d.releases != 0 {
		t.Fatal("a release ran for a gateway agent")
	}
}

// TestAFailedReleaseIsLoggedAndDoesNotReplaceTheAttachResult: the operator's
// log says the client may still be attached; the browser still gets what
// ended its terminal.
func TestAFailedReleaseIsLoggedAndDoesNotReplaceTheAttachResult(t *testing.T) {
	a, d, logs := newTermAdapter(t, func(context.Context) error { return nil })
	d.releaseFails = errors.New("pods \"keen-wren\" not found")
	if err := a.AttachTerminal(context.Background(), claudeCodeAgent(), false, strings.NewReader(""), io.Discard, nil); err != nil {
		t.Fatalf("a failed release replaced the attach's result: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "release FAILED") || !strings.Contains(got, "not found") {
		t.Fatalf("the failed release was not logged with its cause: %q", got)
	}
}

// TestASuccessfulReleaseLogsWhatItEnded: the release's own report reaches the
// log, so "detached" in muster's log is followed by what happened in the pod.
func TestASuccessfulReleaseLogsWhatItEnded(t *testing.T) {
	a, _, logs := newTermAdapter(t, func(context.Context) error { return nil })
	if err := a.AttachTerminal(context.Background(), claudeCodeAgent(), false, strings.NewReader(""), io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, "agents: terminal keen-wren: released 1") {
		t.Fatalf("log %q does not report the release", got)
	}
}

// writes records each Write as its own element (a frame each, on the socket).
type writes struct {
	mu sync.Mutex
	w  []string
}

func (r *writes) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.w = append(r.w, string(p))
	r.mu.Unlock()
	return len(p), nil
}

func (r *writes) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.w...)
}

// TestAnAgentTerminalNeverShrinksTheSessionsWindow: the attach asks the pod how
// big the window is first; a phone's 47x29 — read-only or interactive — reaches
// the exec as the window's 201x53 (201x52 plus the status line), the browser is
// told so ahead of the TUI's first output, and a later shrinking resize does
// not reach the exec while a growing one does. With the window unknown (the
// control) the browser's size goes through as asked and nothing is reported.
func TestAnAgentTerminalNeverShrinksTheSessionsWindow(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		a, d, _ := newTermAdapter(t, nil)
		d.window = "201 52 on\n"
		sizes := make(chan provision.TermSize, 8)
		emit := make(chan string)
		d.tty = func(ctx context.Context, stdout io.Writer, resize <-chan provision.TermSize) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case sz := <-resize:
					sizes <- sz
				case s := <-emit:
					_, _ = io.WriteString(stdout, s)
				}
			}
		}
		var out writes
		resize := make(chan provision.TermSize, 8)
		resize <- provision.TermSize{Cols: 47, Rows: 29}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- a.AttachTerminal(ctx, claudeCodeAgent(), readOnly, strings.NewReader(""), &out, resize)
		}()

		next := func(what string) provision.TermSize {
			t.Helper()
			select {
			case sz := <-sizes:
				return sz
			case <-time.After(5 * time.Second):
				t.Fatalf("readOnly=%v: no size reached the exec (%s)", readOnly, what)
				return provision.TermSize{}
			}
		}
		if got := next("initial"); got != (provision.TermSize{Cols: 201, Rows: 53}) {
			t.Fatalf("readOnly=%v: the exec started at %v, want the window's 201x53", readOnly, got)
		}
		emit <- "TUI"
		want := []string{string(termsize.Report(termsize.Size{Cols: 201, Rows: 53})), "TUI"}
		for deadline := time.Now().Add(5 * time.Second); len(out.all()) < 2 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		if got := out.all(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("readOnly=%v: the browser got %q, want the report then the TUI, as separate frames", readOnly, got)
		}

		resize <- provision.TermSize{Cols: 52, Rows: 31}
		resize <- provision.TermSize{Cols: 239, Rows: 61}
		if got := next("after a shrink then a grow"); got != (provision.TermSize{Cols: 239, Rows: 61}) {
			t.Fatalf("readOnly=%v: the exec got %v; the shrinking 52x31 must not reach it", readOnly, got)
		}
		cancel()
		<-done
		d.mu.Lock()
		q := d.sizeQueries
		d.mu.Unlock()
		if q != 1 {
			t.Fatalf("readOnly=%v: %d window-size queries, want 1", readOnly, q)
		}
	}

	// Control: the window cannot be read — the old behaviour, exactly.
	a, d, _ := newTermAdapter(t, nil)
	sizes := make(chan provision.TermSize, 8)
	d.tty = func(ctx context.Context, stdout io.Writer, resize <-chan provision.TermSize) error {
		sizes <- <-resize
		_, _ = io.WriteString(stdout, "TUI")
		<-ctx.Done()
		return ctx.Err()
	}
	var out writes
	resize := make(chan provision.TermSize, 8)
	resize <- provision.TermSize{Cols: 47, Rows: 29}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.AttachTerminal(ctx, claudeCodeAgent(), true, strings.NewReader(""), &out, resize) }()
	if got := <-sizes; got != (provision.TermSize{Cols: 47, Rows: 29}) {
		t.Fatalf("control: with no window size the exec got %v, want the browser's 47x29", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := out.all(); fmt.Sprint(got) != "[TUI]" {
		t.Fatalf("control: the browser got %q, want only the TUI", got)
	}
	cancel()
	<-done
}
