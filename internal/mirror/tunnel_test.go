package mirror_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAHostsSessionsAreVisibleToItsTokenOwnerOnly: user 1's host is in user 1's
// view and NOT in user 2's; user 2 cannot attach it (it answers as a host that
// does not exist); a same-named host of user 2's is a different host, showing
// only user 2's sessions; no principal, and System, see nothing.
func TestAHostsSessionsAreVisibleToItsTokenOwnerOnly(t *testing.T) {
	r := newRig(t)
	tok1, _ := r.store.mint(t, 1, "ws")
	b1 := &fakeBackend{}
	b1.set("main", "scratch")
	r.connect(t, "ws", tok1, b1, true)
	r.online(t, 1, "ws", "main")

	one, two := asUser(1), asUser(2)
	h1 := r.svc.Hosts(one)
	if len(h1) != 1 || h1[0].Host != "ws" || !slices.Equal(h1[0].Sessions, []string{"main", "scratch"}) || !h1[0].Online {
		t.Fatalf("user 1 sees %+v, want ws [main scratch] online", h1)
	}
	if h2 := r.svc.Hosts(two); len(h2) != 0 {
		t.Fatalf("user 2 sees user 1's hosts: %+v", h2)
	}
	_, _, _, _, res := attach(t, two, r.svc, "ws", "main", true)
	if err := within(t, res, "user 2's attach"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("user 2 attaching user 1's host = %v, want ErrHostOffline", err)
	}
	if b1.attached() != 0 {
		t.Fatal("user 2's attach reached user 1's host")
	}

	// User 2's own host of the same name is a separate host.
	tok2, _ := r.store.mint(t, 2, "ws")
	b2 := &fakeBackend{}
	b2.set("other")
	r.connect(t, "ws", tok2, b2, false)
	r.online(t, 2, "ws", "other")
	if h2 := r.svc.Hosts(two); len(h2) != 1 || !slices.Equal(h2[0].Sessions, []string{"other"}) {
		t.Fatalf("user 2 sees %+v, want only its own ws [other]", h2)
	}
	if h1 := r.svc.Hosts(one); len(h1) != 1 || !slices.Equal(h1[0].Sessions, []string{"main", "scratch"}) {
		t.Fatalf("user 2's same-named host changed user 1's view: %+v", h1)
	}

	for name, ctx := range map[string]context.Context{"no principal": context.Background(), "System": authz.System(context.Background())} {
		if h := r.svc.Hosts(ctx); len(h) != 0 {
			t.Errorf("%s sees hosts %+v", name, h)
		}
		_, _, _, _, res := attach(t, ctx, r.svc, "ws", "main", true)
		if err := within(t, res, name+" attach"); !errors.Is(err, mirror.ErrNoPrincipal) {
			t.Errorf("%s attach = %v, want ErrNoPrincipal", name, err)
		}
	}
	if len(r.store.nonSystem) != 0 {
		t.Fatalf("token lookups ran under a non-System principal: %v", r.store.nonSystem)
	}
}

// TestSessionsTrackTheHost: a session the host stops listing leaves the view
// and refuses an attach with ErrSessionGone.
func TestSessionsTrackTheHost(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	b := &fakeBackend{}
	b.set("main", "scratch")
	r.connect(t, "ws", tok, b, false)
	r.online(t, 1, "ws", "scratch")
	b.set("main")
	eventually(t, "scratch leaves the view", func() bool { return !r.svc.Hub().Online(1, "ws", "scratch") })
	_, _, _, _, res := attach(t, asUser(1), r.svc, "ws", "scratch", true)
	if err := within(t, res, "attach to an ended session"); !errors.Is(err, mirror.ErrSessionGone) {
		t.Fatalf("attach to an ended session = %v, want ErrSessionGone", err)
	}
	// A session that appears later is listed on both sides and attachable (the
	// host's own allowlist follows its list).
	b.set("main", "late")
	r.online(t, 1, "ws", "late")
	_, screen, _, _, _ := attach(t, asUser(1), r.svc, "ws", "late", true)
	eventually(t, "the late session draws", func() bool { return strings.Contains(screen.String(), "SCREEN late") })
}

// TestAReadOnlyAttachShowsTheScreenAndTypesNothing (an interactive host, so only
// the read-only request decides).
func TestAReadOnlyAttachShowsTheScreenAndTypesNothing(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	b := &fakeBackend{}
	b.set("main")
	r.connect(t, "ws", tok, b, true)
	r.online(t, 1, "ws", "main")

	stdin, screen, _, cancel, res := attach(t, asUser(1), r.svc, "ws", "main", true)
	eventually(t, "the screen", func() bool { return strings.Contains(screen.String(), "SCREEN main") })
	ft := b.term(t, 0)
	if !ft.readOnly {
		t.Fatal("a read-only attach asked the host for an INTERACTIVE client")
	}
	_, _ = stdin.Write([]byte("rm -rf ~\r"))
	time.Sleep(150 * time.Millisecond)
	if typed, _, _ := ft.snapshot(); typed != "" {
		t.Fatalf("a READ-ONLY terminal received input %q", typed)
	}
	cancel()
	if err := within(t, res, "the attach to end"); err != nil {
		t.Fatalf("a browser leaving ended the attach with %v, want nil", err)
	}
	eventually(t, "the host's client is closed", func() bool { _, _, c := ft.snapshot(); return c })
}

// TestInteractiveIsRefusedWhenTheHostDidNotAdvertiseIt: a host started without
// -interactive gets no rw attach from muster — refused before anything is sent
// to it. The control: an -interactive host types.
func TestInteractiveIsRefusedWhenTheHostDidNotAdvertiseIt(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ro-host")
	b := &fakeBackend{}
	b.set("main")
	r.connect(t, "ro-host", tok, b, false)
	r.online(t, 1, "ro-host", "main")
	if h := r.svc.Hosts(asUser(1)); len(h) != 1 || h[0].Interactive {
		t.Fatalf("a host without -interactive is reported %+v", h)
	}
	_, _, _, _, res := attach(t, asUser(1), r.svc, "ro-host", "main", false)
	if err := within(t, res, "the refusal"); !errors.Is(err, mirror.ErrInteractiveRefused) {
		t.Fatalf("rw attach to a read-only host = %v, want ErrInteractiveRefused", err)
	}
	time.Sleep(100 * time.Millisecond)
	if b.attached() != 0 {
		t.Fatal("the refused rw attach still opened a client on the host")
	}

	tok2, _ := r.store.mint(t, 1, "rw-host")
	b2 := &fakeBackend{}
	b2.set("main")
	r.connect(t, "rw-host", tok2, b2, true)
	r.online(t, 1, "rw-host", "main")
	stdin, screen, resize, _, _ := attach(t, asUser(1), r.svc, "rw-host", "main", false)
	eventually(t, "screen", func() bool { return strings.Contains(screen.String(), "SCREEN main") })
	_, _ = stdin.Write([]byte("echo hi\r"))
	resize <- provision.TermSize{Cols: 63, Rows: 19}
	ft := b2.term(t, 0)
	eventually(t, "input and resize reach the host", func() bool {
		typed, sizes, _ := ft.snapshot()
		return typed == "echo hi\r" && slices.Contains(sizes, "63x19")
	})
	if _, sizes, _ := ft.snapshot(); sizes[0] != "91x37" {
		t.Fatalf("the initial size was not carried on the open: %v", sizes)
	}
}

// TestMusterSendsNoInputFrameForAReadOnlyStream isolates muster's OWN read-only
// layer with a hand-driven (interactive) host that records every frame.
func TestMusterSendsNoInputFrameForAReadOnlyStream(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		r := newRig(t)
		tok, _ := r.store.mint(t, 1, "ws")
		h, err := r.rawConnect(t, tok, hello("ws", true, "main"))
		if err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, "ws", "main")
		stdin, _, _, _, _ := attach(t, asUser(1), r.svc, "ws", "main", readOnly)
		eventually(t, "open reaches the host", func() bool { _, ok := h.opened(); return ok })
		if m, _ := h.opened(); m.ReadOnly != readOnly {
			t.Fatalf("open.readOnly = %v, want %v", m.ReadOnly, readOnly)
		}
		_, _ = stdin.Write([]byte("x"))
		if readOnly {
			time.Sleep(150 * time.Millisecond)
			if f := h.frames(); len(f) != 0 {
				t.Fatalf("muster sent input frames for a READ-ONLY stream: %v", f)
			}
		} else {
			eventually(t, "control: an interactive stream's input frame", func() bool { return len(h.frames()) == 1 })
		}
	}
}

// TestATokenForOneHostCannotRegisterAnother: the token decides the host.
func TestATokenForOneHostCannotRegisterAnother(t *testing.T) {
	r := newRig(t)
	tokA, _ := r.store.mint(t, 1, "alpha")
	b := &fakeBackend{}
	b.set("main")
	_, done := r.connect(t, "bravo", tokA, b, false)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `minted for host "alpha" and cannot register host "bravo"`) {
			t.Fatalf("connect = %v, want the host-mismatch refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a mismatched host was not refused")
	}
	if h := r.svc.Hosts(asUser(1)); len(h) != 0 {
		t.Fatalf("a refused registration is in the view: %+v", h)
	}
	r.connect(t, "alpha", tokA, b, false)
	r.online(t, 1, "alpha", "main")
}

// TestOnlyALiveMirrorTokenOpensTheConnectRoute: no token, a hook- or
// agent-shaped token, an unknown mirror-shaped one and a revoked one all get
// 401 before any upgrade; the live token gets past auth (control).
func TestOnlyALiveMirrorTokenOpensTheConnectRoute(t *testing.T) {
	r := newRig(t)
	live, _ := r.store.mint(t, 1, "alpha")
	revoked, rec := r.store.mint(t, 1, "alpha")
	if ok, _ := r.store.RevokeToken(context.Background(), rec.ID); !ok {
		t.Fatal("revoke")
	}
	get := func(tok string) int {
		req, _ := http.NewRequest("GET", r.srv.URL+mirror.ConnectPath, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, tok := range map[string]string{
		"none":         "",
		"hook token":   "hook-token-sentinel-0123456789",
		"agent token":  strings.Repeat("7", 64),
		"unknown mmt_": mirror.TokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"revoked":      revoked,
	} {
		if code := get(tok); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if code := get(live); code == http.StatusUnauthorized || code == http.StatusTooManyRequests {
		t.Fatalf("control: the LIVE token got %d", code)
	}
}

// TestAuthFailuresAreLimitedButAValidTokenAlwaysConnects: past the failure
// limit for an address — which behind the public path is EVERY client's address
// — bad tokens get 429, and a VALID token from that same address still connects
// and registers. A string not even shaped like a token is refused without a
// store lookup.
func TestAuthFailuresAreLimitedButAValidTokenAlwaysConnects(t *testing.T) {
	r := newRig(t)
	live, _ := r.store.mint(t, 1, "alpha")
	get := func(tok string) int {
		req, _ := http.NewRequest("GET", r.srv.URL+mirror.ConnectPath, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	malformed := mirror.TokenPrefix + "bad"
	wellShaped := mirror.TokenPrefix + strings.Repeat("A", 43)
	codes := []int{}
	for i := 0; i < 12; i++ {
		tok := malformed
		if i%2 == 1 {
			tok = wellShaped
		}
		codes = append(codes, get(tok))
	}
	if codes[0] != http.StatusUnauthorized || codes[8] != http.StatusUnauthorized || codes[10] != http.StatusTooManyRequests || codes[11] != http.StatusTooManyRequests {
		t.Fatalf("failure codes %v; want 401 for the first %d, then 429", codes, 10)
	}
	r.store.mu.Lock()
	lookups := r.store.authCalls
	r.store.mu.Unlock()
	if lookups != 6 {
		t.Fatalf("%d store lookups for 6 well-shaped and 6 malformed tokens; malformed ones must not reach the store", lookups)
	}

	// The budget is exhausted for this address. A valid token still connects.
	b := &fakeBackend{}
	b.set("main")
	r.connect(t, "alpha", live, b, false)
	r.online(t, 1, "alpha", "main")
	if code := get(wellShaped); code != http.StatusTooManyRequests {
		t.Fatalf("after the valid connect, a bad token got %d; the limit must still hold for failures", code)
	}
}

// TestAHostThatDisconnectsGoesOfflineAndEndsItsTerminals: the attached terminal
// ends with ErrHostDisconnected; the host stays in the view as OFFLINE with its
// last list; a fresh attach answers ErrHostOffline.
func TestAHostThatDisconnectsGoesOfflineAndEndsItsTerminals(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	b := &fakeBackend{}
	b.set("main")
	stop, _ := r.connect(t, "laptop", tok, b, false)
	r.online(t, 1, "laptop", "main")
	_, screen, _, _, res := attach(t, asUser(1), r.svc, "laptop", "main", true)
	eventually(t, "screen", func() bool { return strings.Contains(screen.String(), "SCREEN main") })

	stop()
	if err := within(t, res, "the attach to end"); !errors.Is(err, mirror.ErrHostDisconnected) {
		t.Fatalf("the terminal ended with %v, want ErrHostDisconnected", err)
	}
	eventually(t, "offline in the view", func() bool {
		h := r.svc.Hosts(asUser(1))
		return len(h) == 1 && !h[0].Online && slices.Equal(h[0].Sessions, []string{"main"})
	})
	_, _, _, _, res2 := attach(t, asUser(1), r.svc, "laptop", "main", true)
	if err := within(t, res2, "attach to an offline host"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("attach to an offline host = %v, want ErrHostOffline", err)
	}
}

// TestARevokedTokenClosesOpenTerminals: revocation ends the connection AND the
// terminal open on it — asserted on the terminal, not just the connection.
func TestARevokedTokenClosesOpenTerminals(t *testing.T) {
	r := newRig(t)
	r.svc.SetTokenRecheck(20 * time.Millisecond)
	tok, rec := r.store.mint(t, 1, "laptop")
	b := &fakeBackend{}
	b.set("main")
	r.connect(t, "laptop", tok, b, false)
	r.online(t, 1, "laptop", "main")
	_, screen, _, _, res := attach(t, asUser(1), r.svc, "laptop", "main", true)
	eventually(t, "screen", func() bool { return strings.Contains(screen.String(), "SCREEN main") })
	if ok, _ := r.store.RevokeToken(context.Background(), rec.ID); !ok {
		t.Fatal("revoke")
	}
	if err := within(t, res, "the open terminal to close on revocation"); !errors.Is(err, mirror.ErrHostDisconnected) {
		t.Fatalf("the open terminal ended with %v, want ErrHostDisconnected", err)
	}
	eventually(t, "the host's client closed", func() bool { _, _, c := b.term(t, 0).snapshot(); return c })
	// A REVOKED host is gone from the view at once, not kept as offline.
	eventually(t, "the revoked host leaves the view", func() bool { return len(r.svc.Hosts(asUser(1))) == 0 })
}

// TestChangeNudgesAreCoalesced: a burst of session-list changes reaches open
// views as a handful of nudges, not one per change.
func TestChangeNudgesAreCoalesced(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(200 * time.Millisecond)
	var mu sync.Mutex
	nudges := 0
	r.svc.SetNotify(func() { mu.Lock(); nudges++; mu.Unlock() })
	tok, _ := r.store.mint(t, 1, "flappy")
	h, err := r.rawConnect(t, tok, hello("flappy", false, "s0"))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "flappy", "s0")
	for i := 1; i <= 40; i++ {
		h.sendSessions(t, fmt.Sprintf("s%d", i))
	}
	r.online(t, 1, "flappy", "s40")
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if nudges == 0 || nudges > 3 {
		t.Fatalf("41 changes within ~0.2 s produced %d nudges; want 1-3 (coalesced)", nudges)
	}
}

// TestARecheckThatCannotReachTheDatabaseFailsClosed: RecheckFailClosedAfter
// consecutive failed rechecks end the connection and its terminals; a single
// failure (a blip) does not.
func TestARecheckThatCannotReachTheDatabaseFailsClosed(t *testing.T) {
	t.Run("one blip is tolerated", func(t *testing.T) {
		r := newRig(t)
		r.svc.SetTokenRecheck(20 * time.Millisecond)
		r.store.liveErr, r.store.liveErrN = errors.New("connection refused"), 1
		tok, _ := r.store.mint(t, 1, "laptop")
		b := &fakeBackend{}
		b.set("main")
		r.connect(t, "laptop", tok, b, false)
		r.online(t, 1, "laptop", "main")
		eventually(t, "several rechecks", func() bool {
			r.store.mu.Lock()
			defer r.store.mu.Unlock()
			return r.store.liveCalls >= 4
		})
		if !r.svc.Hub().Online(1, "laptop", "main") {
			t.Fatal("one failed recheck took the host offline")
		}
	})
	t.Run("a persistent failure closes", func(t *testing.T) {
		r := newRig(t)
		r.svc.SetTokenRecheck(20 * time.Millisecond)
		tok, _ := r.store.mint(t, 1, "laptop")
		b := &fakeBackend{}
		b.set("main")
		r.connect(t, "laptop", tok, b, false)
		r.online(t, 1, "laptop", "main")
		_, screen, _, _, res := attach(t, asUser(1), r.svc, "laptop", "main", true)
		eventually(t, "screen", func() bool { return strings.Contains(screen.String(), "SCREEN main") })
		r.store.mu.Lock()
		r.store.liveErr = errors.New("connection refused")
		r.store.mu.Unlock()
		if err := within(t, res, "the terminal to close on an unverifiable token"); !errors.Is(err, mirror.ErrHostDisconnected) {
			t.Fatalf("the open terminal ended with %v, want ErrHostDisconnected", err)
		}
	})
}

// TestANewConnectionFromTheSameHostReplacesTheOld.
func TestANewConnectionFromTheSameHostReplacesTheOld(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	b1, b2 := &fakeBackend{}, &fakeBackend{}
	b1.set("one")
	b2.set("two")
	_, done1 := r.connect(t, "laptop", tok, b1, false)
	r.online(t, 1, "laptop", "one")
	r.connect(t, "laptop", tok, b2, false)
	eventually(t, "second replaces first", func() bool {
		return r.svc.Hub().Online(1, "laptop", "two") && !r.svc.Hub().Online(1, "laptop", "one")
	})
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced connection did not end")
	}
	if !r.svc.Hub().Online(1, "laptop", "two") {
		t.Fatal("the replaced connection's teardown took the NEW one offline")
	}
}

// TestAHostHasAtMostMaxStreamsPerHostTerminals.
func TestAHostHasAtMostMaxStreamsPerHostTerminals(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	b := &fakeBackend{}
	b.set("main")
	r.connect(t, "laptop", tok, b, false)
	r.online(t, 1, "laptop", "main")
	for i := 0; i < mirror.MaxStreamsPerHost; i++ {
		_, screen, _, _, _ := attach(t, asUser(1), r.svc, "laptop", "main", true)
		eventually(t, "a terminal draws", func() bool { return strings.Contains(screen.String(), "SCREEN main") })
	}
	_, _, _, _, res := attach(t, asUser(1), r.svc, "laptop", "main", true)
	if err := within(t, res, "the over-cap attach"); !errors.Is(err, mirror.ErrTooManyStreams) {
		t.Fatalf("attach #%d = %v, want ErrTooManyStreams", mirror.MaxStreamsPerHost+1, err)
	}
}
