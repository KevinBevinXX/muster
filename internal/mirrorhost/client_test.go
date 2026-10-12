package mirrorhost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestConnectURLMapsSchemesAndRefusesCleartextUnlessAllowed: https→wss under any
// path prefix; plain http only with AllowHTTP (the token rides every connect).
func TestConnectURLMapsSchemesAndRefusesCleartextUnlessAllowed(t *testing.T) {
	for _, c := range []struct {
		in        string
		allowHTTP bool
		want      string
		errHas    string
	}{
		{in: "https://muster.example.com", want: "wss://muster.example.com/mirror/connect"},
		{in: "https://muster.example.com/", want: "wss://muster.example.com/mirror/connect"},
		{in: "https://example.com/muster/?x=1#f", want: "wss://example.com/muster/mirror/connect"},
		{in: "http://192.0.2.10:30306", errHas: "cleartext"},
		{in: "http://192.0.2.10:30306", allowHTTP: true, want: "ws://192.0.2.10:30306/mirror/connect"},
		{in: "ftp://x", errHas: "must be https"},
		{in: "https://", errHas: "no host"},
	} {
		got, err := ConnectURL(c.in, c.allowHTTP)
		if c.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("ConnectURL(%q, %v) = %q, %v; want an error containing %q", c.in, c.allowHTTP, got, err, c.errHas)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ConnectURL(%q, %v) = %q, %v; want %q", c.in, c.allowHTTP, got, err, c.want)
		}
	}
}

// TestNewRefusesACredentialThatIsNotAMirrorToken: the hook token or an agent's
// token pasted into the token file is refused before anything dials.
func TestNewRefusesACredentialThatIsNotAMirrorToken(t *testing.T) {
	base := Config{URL: "https://muster.example.com", Host: "laptop", Backend: Tmux{}}
	for _, tok := range []string{"", "hook-token-sentinel", strings.Repeat("7", 64)} {
		c := base
		c.Token = tok
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), "not a mirror token") {
			t.Errorf("New(token %q) = %v, want the not-a-mirror-token refusal", tok, err)
		}
	}
	c := base
	c.Token = mirror.TokenPrefix + "x"
	if _, err := New(c); err != nil {
		t.Fatalf("control: a mirror-shaped token was refused: %v", err)
	}
	c.Host = "Upper_Case"
	if _, err := New(c); err == nil {
		t.Fatal("a host name that is not a lowercase DNS label was accepted")
	}
}

// TestAttachArgsAreExactMatchAndReadOnlyWhenAsked pins the argv: -r only for
// read-only, `=`-exact target, the session as one element, the socket first.
func TestAttachArgsAreExactMatchAndReadOnlyWhenAsked(t *testing.T) {
	ro := strings.Join(Tmux{Socket: "s"}.AttachArgs("my sess", true), "|")
	if ro != "-L|s|-u|attach-session|-r|-t|=my sess" {
		t.Fatalf("read-only argv = %q", ro)
	}
	rw := strings.Join(Tmux{SocketPath: "/p"}.AttachArgs("x", false), "|")
	if rw != "-S|/p|-u|attach-session|-t|=x" {
		t.Fatalf("interactive argv = %q", rw)
	}
}

// TestTmuxEnvDropsTMUX: the attach must not inherit $TMUX (a client inside a
// pane would refuse to nest, or talk to whatever server $TMUX names).
func TestTmuxEnvDropsTMUX(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("TMUX_TMPDIR", "/run/user/1000")
	env := strings.Join(tmuxEnv(), "\n")
	if strings.Contains(env, "TMUX=/tmp") || strings.Contains(env, "TMUX_PANE=") {
		t.Fatalf("tmuxEnv kept $TMUX/$TMUX_PANE:\n%s", env)
	}
	if !strings.Contains(env, "TMUX_TMPDIR=/run/user/1000") {
		t.Fatal("tmuxEnv dropped $TMUX_TMPDIR, which decides where the default socket is")
	}
	if !strings.Contains(env, "TERM=xterm-256color") {
		t.Fatal("tmuxEnv sets no TERM")
	}
}
