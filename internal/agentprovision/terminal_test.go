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
}

func (d *termDriver) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

func (d *termDriver) ExecTTY(ctx context.Context, _ provision.Ref, cmd []string, _ io.Reader, _ io.Writer, _ <-chan provision.TermSize) error {
	d.mu.Lock()
	d.ttyCmd = cmd
	d.mu.Unlock()
	return d.attach(ctx)
}

func (d *termDriver) Exec(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout, _ io.Writer) error {
	in, _ := io.ReadAll(stdin)
	d.mu.Lock()
	defer d.mu.Unlock()
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
