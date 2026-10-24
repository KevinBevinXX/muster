//go:build tmuxit

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

	"github.com/ZacxDev/muster/internal/agents"
)

// 🔴 THE ATTACK THE HOOK CHANNEL'S AUTHENTICATION AND THE DIALOG CHECK CLOSE,
// through the whole binary. The session is at a permission prompt (the fake
// CLI draws the real dialog's shape and lets input answer it). Something in the
// pod forges a Stop, which — accepted — makes ccd read the session idle, and
// the next turn's paste-then-Enter would answer the prompt "1. Yes".
//
//  1. a forger WITHOUT the credential (another container's position, given a
//     path to the socket it does not in fact have) is refused 401, and the
//     session stays busy: the next turn is refused and nothing is typed;
//  2. a forger WITH the credential — any process of ccd's own user, which is
//     the residual the credential cannot close (it reads it here exactly as a
//     tool call could, from the CLI's tmux environment) — is accepted, and the
//     turn is STILL not typed: the pane shows a dialog, so ccd answers 409 busy
//     with code awaiting_approval. The dialog is unanswered throughout.
func TestTmuxAForgedHookCannotGetATurnTypedIntoAPermissionDialog(t *testing.T) {
	r := startServe(t, "CCD_TURN_TIMEOUT=2s")
	r.waitHealth(t, "primary start", primaryReady)
	chat := func(p string) (string, error) {
		return mustersGatewayNoT(r.gwAddr).Chat(context.Background(), contractAgent, "s", p, nil)
	}
	if _, err := chat("ASK-PERMISSION run it"); err == nil {
		t.Fatal("a turn stopped at a permission dialog returned a reply")
	} else {
		t.Logf("the turn stopped at the dialog: %v", err)
	}
	answered := filepath.Join(r.cfg, "fake-dialog.log")
	var screen string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if screen = r.tmux(t, "capture-pane", "-p", "-J", "-t", "cc"); strings.Contains(screen, "Do you want to proceed?") {
			break
		}
	}
	hooklog, _ := os.ReadFile(filepath.Join(r.cfg, "fake-hooks.log"))
	if !strings.Contains(screen, "Do you want to proceed?") {
		t.Logf("fake hooks log:\n%s", hooklog)
		t.Fatalf("instrument check: the dialog is not on screen:\n%s\nccd log:\n%s", screen, r.logs.String())
	}

	// What a process of ccd's user can read: the CLI's environment.
	env := func(name string) string {
		v := strings.TrimPrefix(r.tmux(t, "show-environment", "-t", "cc", name), name+"=")
		if v == "" || strings.HasPrefix(v, "-") {
			t.Fatalf("instrument check: %s is not in the CLI's environment", name)
		}
		return v
	}
	sock, secret := env(envHookSocket), env(envHookSecret)
	forge := func(cred string) int {
		req, _ := http.NewRequest(http.MethodPost, "http://ccd/hook/Stop", strings.NewReader(`{}`))
		if cred != "" {
			req.Header.Set("Authorization", "Bearer "+cred)
		}
		resp, err := unixClient(sock, 5*time.Second).Do(req)
		if err != nil {
			t.Fatalf("forging a Stop: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// 1. No credential, and a wrong one: refused, state unchanged.
	for _, cred := range []string{"", "not-the-credential"} {
		if code := forge(cred); code != http.StatusUnauthorized {
			t.Fatalf("a Stop with credential %q: %d, want 401", cred, code)
		}
	}
	_, err := chat("typed into the dialog?")
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) || rt.Status != http.StatusConflict || rt.Type != failBusy {
		t.Fatalf("a turn after a refused forged Stop: %v; want 409 busy (the session is still mid-turn)", err)
	}
	if b, err := os.ReadFile(answered); err == nil {
		t.Fatalf("the dialog was answered after a refused forged Stop: %s", b)
	}

	// 2. With the credential (the residual): accepted — and still nothing typed.
	if code := forge(secret); code != http.StatusNoContent {
		t.Fatalf("a Stop with the credential: %d, want 204 (it is the residual this test pins)", code)
	}
	_, err = chat("typed into the dialog?")
	if !errors.As(err, &rt) || rt.Status != http.StatusConflict || rt.Type != failBusy || rt.Code != codeAwaitingApproval {
		t.Fatalf("a turn while the pane shows a dialog: %v; want 409 busy code %s", err, codeAwaitingApproval)
	}
	if b, err := os.ReadFile(answered); err == nil {
		t.Fatalf("the dialog was answered: %s", b)
	}
	if screen := r.tmux(t, "capture-pane", "-p", "-J", "-t", "cc"); !strings.Contains(screen, "Do you want to proceed?") {
		t.Fatalf("the dialog left the screen:\n%s", screen)
	}

	// Positive control: the fake's dialog IS answerable, by the Enter the
	// refused turn would have pressed.
	r.tmux(t, "send-keys", "-t", "cc", "Enter")
	if !waitFile(answered, 5*time.Second) {
		t.Fatal("control: an Enter did not answer the fake's dialog")
	}
}
