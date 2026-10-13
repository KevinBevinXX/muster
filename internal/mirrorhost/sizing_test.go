package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/termsize"
)

// sizedFake is a Backend that reports a window size (or fails to) and records
// what its terminals were asked.
type sizedFake struct {
	mu       sync.Mutex
	win      termsize.Size
	winErr   error
	attaches []string // "ro|rw CxR"
	term     *recTerm
}

func (b *sizedFake) List(context.Context) ([]string, error) { return []string{"main"}, nil }

func (b *sizedFake) WindowSize(context.Context, string) (termsize.Size, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.win, b.winErr
}

func (b *sizedFake) Attach(_ context.Context, _ string, readOnly bool, cols, rows uint16) (Terminal, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	b.attaches = append(b.attaches, fmt.Sprintf("%s %dx%d", mode, cols, rows))
	pr, pw := io.Pipe()
	b.term = &recTerm{r: pr, w: pw}
	return b.term, nil
}

// recTerm is a terminal whose output a test writes, and whose resizes it reads.
type recTerm struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	mu      sync.Mutex
	resizes []string
}

func (t *recTerm) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *recTerm) Write(p []byte) (int, error) { return len(p), nil }
func (t *recTerm) Close() error                { return t.w.Close() }
func (t *recTerm) Resize(c, r uint16) error {
	t.mu.Lock()
	t.resizes = append(t.resizes, fmt.Sprintf("%dx%d", c, r))
	t.mu.Unlock()
	return nil
}
func (t *recTerm) got() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.resizes...)
}

// emit makes the terminal produce s, and returns what the next Read of term
// yields.
func emitAndRead(t *testing.T, term Terminal, src *recTerm, s string) string {
	t.Helper()
	go func() { _, _ = src.w.Write([]byte(s)) }()
	buf := make([]byte, 4096)
	n, err := term.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func read(t *testing.T, term Terminal) string {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := term.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// TestNoShrinkAttachesAPhoneAtTheWindowsSizeAndSaysSo: a phone-sized request on
// a 201x53 window attaches at 201x53 — read-only AND interactive — and the
// first read is the size report, alone, ahead of the session's first output.
func TestNoShrinkAttachesAPhoneAtTheWindowsSizeAndSaysSo(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		b := &sizedFake{win: termsize.Size{Cols: 201, Rows: 53}}
		term, err := NoShrink(b).Attach(context.Background(), "main", readOnly, 47, 29)
		if err != nil {
			t.Fatal(err)
		}
		mode := map[bool]string{true: "ro", false: "rw"}[readOnly]
		if len(b.attaches) != 1 || b.attaches[0] != mode+" 201x53" {
			t.Fatalf("%s: attached %v, want [%s 201x53] — a 47x29 client would shrink the window", mode, b.attaches, mode)
		}
		if got := emitAndRead(t, term, b.term, "SCREEN"); got != string(termsize.Report(termsize.Size{Cols: 201, Rows: 53})) {
			t.Fatalf("%s: first read = %q, want the report alone", mode, got)
		}
		if got := read(t, term); got != "SCREEN" {
			t.Fatalf("%s: the output after the report = %q", mode, got)
		}
	}
}

// TestNoShrinkLetsADesktopGrowTheWindow: a request that covers the window is
// granted as asked (the desktop behaviour), and reported as such.
func TestNoShrinkLetsADesktopGrowTheWindow(t *testing.T) {
	b := &sizedFake{win: termsize.Size{Cols: 201, Rows: 53}}
	term, err := NoShrink(b).Attach(context.Background(), "main", true, 239, 61)
	if err != nil {
		t.Fatal(err)
	}
	if b.attaches[0] != "ro 239x61" {
		t.Fatalf("attached %v, want 239x61", b.attaches)
	}
	if got := emitAndRead(t, term, b.term, "X"); got != string(termsize.Report(termsize.Size{Cols: 239, Rows: 61})) {
		t.Fatalf("first read = %q", got)
	}
}

// TestNoShrinkRefusesAShrinkingResizeAndGrantsAGrowingOne: after attach, a
// resize smaller than the window does not reach the client (it stays at the
// window's size) and is answered with a report ahead of the next output; a
// covering resize reaches it and needs no report.
func TestNoShrinkRefusesAShrinkingResizeAndGrantsAGrowingOne(t *testing.T) {
	b := &sizedFake{win: termsize.Size{Cols: 201, Rows: 53}}
	term, _ := NoShrink(b).Attach(context.Background(), "main", false, 239, 61)
	_ = emitAndRead(t, term, b.term, "A") // the attach report
	_ = read(t, term)                     // "A"
	b.mu.Lock()
	b.win = termsize.Size{Cols: 239, Rows: 61} // the window now follows this client
	b.mu.Unlock()

	if err := term.Resize(103, 31); err != nil {
		t.Fatal(err)
	}
	if got := b.term.got(); len(got) != 0 {
		t.Fatalf("a shrinking resize reached the client: %v", got)
	}
	if got := emitAndRead(t, term, b.term, "B"); got != string(termsize.Report(termsize.Size{Cols: 239, Rows: 61})) {
		t.Fatalf("after a refused shrink the next read = %q, want the report", got)
	}
	if got := read(t, term); got != "B" {
		t.Fatalf("output after the report = %q", got)
	}

	if err := term.Resize(251, 67); err != nil {
		t.Fatal(err)
	}
	if got := b.term.got(); len(got) != 1 || got[0] != "251x67" {
		t.Fatalf("a growing resize = %v, want [251x67]", got)
	}
	if got := emitAndRead(t, term, b.term, "C"); got != "C" {
		t.Fatalf("a granted resize was followed by %q, want plain output", got)
	}
}

// TestNoShrinkWithAnUnknownWindowIsThePreviousBehaviour: no window size, no
// report, every size passed through — the page then fits itself as before.
// And a backend that cannot report sizes is not wrapped at all.
func TestNoShrinkWithAnUnknownWindowIsThePreviousBehaviour(t *testing.T) {
	b := &sizedFake{winErr: errors.New("no server")}
	term, _ := NoShrink(b).Attach(context.Background(), "main", true, 47, 29)
	if b.attaches[0] != "ro 47x29" {
		t.Fatalf("attached %v, want as asked", b.attaches)
	}
	if got := emitAndRead(t, term, b.term, "PLAIN"); got != "PLAIN" {
		t.Fatalf("first read = %q, want no report", got)
	}
	_ = term.Resize(41, 23)
	if got := b.term.got(); len(got) != 1 || got[0] != "41x23" {
		t.Fatalf("resizes = %v, want passed through", got)
	}
	var plain Backend = Tmux{}
	if _, ok := NoShrink(onlyBackend{plain}).(noShrink); ok {
		t.Fatal("a backend without WindowSize was wrapped")
	}
}

type onlyBackend struct{ Backend }
