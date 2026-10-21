package hermeshost

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// shortHome is a Hermes home whose gateway.sock fits sun_path (the gateway
// then listens in the home itself); 0700, removed at the end.
func shortHome(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "hh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// longHome is a Hermes home whose gateway.sock does NOT fit (over 100 bytes):
// the gateway listens elsewhere and writes gateway.sock.path.
func longHome(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), strings.Repeat("h", 60), strings.Repeat("o", 40))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func gctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestIdentifyFindsTheGatewayInTheHomeAndThroughItsPointer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mk      func(*testing.T) string
		pointer bool
	}{{"direct", shortHome, false}, {"pointer", longHome, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.mk(t)
			g := hermestest.NewGateway(t, dir)
			if got := strings.HasSuffix(g.Socket, "/control.sock"); got != tc.pointer {
				t.Fatalf("the fake listens at %s (pointer=%v)", g.Socket, got)
			}
			gw, err := NewHome(dir).Identify(gctx(t))
			if err != nil {
				t.Fatal(err)
			}
			if gw.CodeSHA != Pin || gw.InstanceID != "fakeinstance0001" || gw.ProfileID == "" || !strings.HasPrefix(gw.APIOrigin, "http://127.0.0.1:") {
				t.Fatalf("identify = %+v", gw)
			}
		})
	}
}

func TestIdentifyWithNoGatewayRunning(t *testing.T) {
	dir := shortHome(t)
	if _, err := NewHome(dir).Identify(gctx(t)); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("no socket, no pointer: err = %v, want ErrNoGateway", err)
	}
	// A socket left behind by a gateway that exited: nothing answers.
	sock := filepath.Join(dir, "gateway.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	chmod(t, sock, 0o600)
	if _, err := NewHome(dir).Identify(gctx(t)); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("a dead socket: err = %v, want ErrNoGateway", err)
	}
}

// Every identify answer muster cannot attach to is refused with its kind.
func TestIdentifyRefusesWhatItCannotAttachTo(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value any
		want  error
		quote string
	}{
		{"another commit", "code_sha", "d923a5eb487a17c0a68801bba9ef0f56c81040a8", ErrGatewayNotPin, "d923a5eb487a"},
		{"an unreadable commit", "code_sha", "<b>not a sha</b>", ErrGatewayNotPin, "an unreadable commit"},
		{"no commit", "code_sha", nil, ErrGatewayNotPin, ""},
		{"not a gateway", "kind", "hermes-dashboard", ErrGatewayProtocol, ""},
		{"starting", "state", "starting", ErrGatewayNotReady, "starting"},
		{"draining", "state", "draining", ErrGatewayNotReady, ""},
		{"no session authority", "capabilities", []any{"event-replay-v1"}, ErrGatewayNotReady, ""},
		{"runtime protocol 2", "runtime_protocol", 2, ErrGatewayProtocol, ""},
		{"runtime protocol true", "runtime_protocol", true, ErrGatewayProtocol, ""},
		{"no epoch", "authority_epoch", 0, ErrGatewayProtocol, ""},
		{"no instance", "instance_id", "", ErrGatewayProtocol, ""},
		{"another home", "served_profiles", []any{map[string]any{"profile_id": "/x", "home": "/"}}, ErrGatewayNotReady, ""},
		{"this home twice", "served_profiles", nil, ErrGatewayNotReady, ""},
		{"a remote API", "api_origin", "http://192.0.2.10:8080", ErrGatewayProtocol, ""},
		{"an API by name", "api_origin", "http://localhost:8080", ErrGatewayProtocol, ""},
		{"an API with a path", "api_origin", "http://127.0.0.1:8080/x", ErrGatewayProtocol, ""},
		{"an API with a user", "api_origin", "http://u:p@127.0.0.1:8080", ErrGatewayProtocol, ""},
		{"an API with no port", "api_origin", "http://127.0.0.1", ErrGatewayProtocol, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortHome(t)
			g := hermestest.NewGateway(t, dir)
			v := tc.value
			if tc.name == "this home twice" {
				v = []any{map[string]any{"profile_id": "a", "home": dir}, map[string]any{"profile_id": "b", "home": dir}}
			}
			g.Set(tc.key, v)
			_, err := NewHome(dir).Identify(gctx(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.quote != "" && !strings.Contains(err.Error(), tc.quote) {
				t.Fatalf("err = %v, want it to say %q", err, tc.quote)
			}
			if strings.Contains(err.Error(), "<b>") {
				t.Fatalf("the error quotes what the gateway sent: %v", err)
			}
		})
	}
}

// The control paths are checked as Hermes's own client checks them — and
// group write on the home is refused outright.
func TestTheGatewayDiscoveryRefusesAnUnsafeControlPath(t *testing.T) {
	type setup func(t *testing.T, dir string, g *hermestest.Gateway)
	cases := []struct {
		name    string
		long    bool
		mutate  setup
		want    error
		nothing bool // ErrNoGateway rather than unsafe
	}{
		{"a socket other users can reach", false, func(t *testing.T, dir string, g *hermestest.Gateway) {
			chmod(t, g.Socket, 0o660) // group only: an "other"-only rule would admit it
		}, ErrGatewayUnsafe, false},
		{"a group-writable home", false, func(t *testing.T, dir string, g *hermestest.Gateway) {
			chmod(t, dir, 0o770)
		}, ErrGatewayUnsafe, false},
		{"a world-writable home", false, func(t *testing.T, dir string, g *hermestest.Gateway) {
			chmod(t, dir, 0o707)
		}, ErrGatewayUnsafe, false},
		{"gateway.sock is a regular file", false, func(t *testing.T, dir string, g *hermestest.Gateway) {
			_ = os.Remove(g.Socket)
			if err := os.WriteFile(g.Socket, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrGatewayUnsafe, false},
		{"a pointer other users can read", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			chmod(t, filepath.Join(dir, "gateway.sock.path"), 0o640)
		}, ErrGatewayUnsafe, false},
		{"a pointer that is a symlink", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			p := filepath.Join(dir, "gateway.sock.path")
			real := filepath.Join(dir, "elsewhere")
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}, ErrGatewayUnsafe, false},
		{"a pointer that is a FIFO", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			p := filepath.Join(dir, "gateway.sock.path")
			_ = os.Remove(p)
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrGatewayUnsafe, false},
		{"a pointer over 4096 bytes", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			write(t, filepath.Join(dir, "gateway.sock.path"), g.Socket+strings.Repeat(" ", 4097))
		}, ErrGatewayUnsafe, false},
		{"a pointer to a relative path", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			write(t, filepath.Join(dir, "gateway.sock.path"), "hermes-gw-0/control.sock")
		}, ErrGatewayUnsafe, false},
		{"a pointer to another home's directory", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			// A real, private socket — only the hash refuses it.
			other := filepath.Join(filepath.Dir(filepath.Dir(g.Socket)), "hermes-gw-0123456789abcdef")
			if err := os.Mkdir(other, 0o700); err != nil {
				t.Fatal(err)
			}
			listen(t, filepath.Join(other, "control.sock"))
			write(t, filepath.Join(dir, "gateway.sock.path"), filepath.Join(other, "control.sock"))
		}, ErrGatewayUnsafe, false},
		{"a pointer to a socket of another name", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			alt := filepath.Join(filepath.Dir(g.Socket), "other.sock")
			listen(t, alt)
			write(t, filepath.Join(dir, "gateway.sock.path"), alt)
		}, ErrGatewayUnsafe, false},
		{"a pointer to a socket that is a symlink", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			real := g.Socket + ".real"
			if err := os.Rename(g.Socket, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, g.Socket); err != nil {
				t.Fatal(err)
			}
		}, ErrGatewayUnsafe, false},
		{"a pointer's directory its group can enter", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			chmod(t, filepath.Dir(g.Socket), 0o750)
		}, ErrGatewayUnsafe, false},
		{"a pointer to a socket that is gone", true, func(t *testing.T, dir string, g *hermestest.Gateway) {
			g.Close()
			_ = os.Remove(g.Socket)
		}, ErrNoGateway, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortHome(t)
			if tc.long {
				dir = longHome(t)
			}
			g := hermestest.NewGateway(t, dir)
			// The positive control: before the mutation, it attaches.
			if _, err := NewHome(dir).Identify(gctx(t)); err != nil {
				t.Fatalf("before the change: %v", err)
			}
			tc.mutate(t, dir, g)
			done := make(chan error, 1)
			go func() { _, err := NewHome(dir).Identify(gctx(t)); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("discovery hung")
			}
		})
	}
}

// The process on the socket must run as this uid (SO_PEERCRED), whatever the
// paths say: a socket swapped in after the checks is still refused.
func TestTheGatewaysProcessMustRunAsThisUser(t *testing.T) {
	dir := shortHome(t)
	hermestest.NewGateway(t, dir)
	if _, err := NewHome(dir).Identify(gctx(t)); err != nil {
		t.Fatalf("the positive control: %v", err)
	}
	old := gatewayPeerUID
	gatewayPeerUID = func() int { return os.Getuid() + 4242 }
	defer func() { gatewayPeerUID = old }()
	_, err := NewHome(dir).Identify(gctx(t))
	if !errors.Is(err, ErrGatewayUnsafe) || !strings.Contains(err.Error(), "the process on its socket runs as uid") {
		t.Fatalf("err = %v, want the peer refusal", err)
	}
}

func chmod(t *testing.T, p string, m os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, m); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listen makes a private socket at p that accepts and answers nothing.
func listen(t *testing.T, p string) {
	t.Helper()
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	chmod(t, p, 0o600)
}
