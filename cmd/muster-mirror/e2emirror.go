//go:build e2emirror

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2emirror`
// (e2e/tests/mirrored.spec.ts builds it). It replaces tmux with a fake whose
// sessions are MUSTER_MIRROR_E2E_SESSIONS (comma-separated) and whose terminal
// prints, on screen, exactly what reached it: `FAKE-MIRROR session=<s>
// mode=ro|rw`, `size=CxR` per resize and `in=<HEX>` per input chunk. The dial,
// the token, the registration, the tunnel and everything in muster are real.
func init() {
	e2eBackend = func() mirrorhost.Backend { return fakeBackend{} }
}

type fakeBackend struct{}

func (fakeBackend) List(context.Context) ([]string, error) {
	var out []string
	for _, s := range strings.Split(os.Getenv("MUSTER_MIRROR_E2E_SESSIONS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (fakeBackend) Attach(_ context.Context, session string, readOnly bool, cols, rows uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	t := &fakeTerm{r: pr, w: pw}
	go t.say(fmt.Sprintf("FAKE-MIRROR session=%s mode=%s size=%dx%d\r\n", session, mode, cols, rows))
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
