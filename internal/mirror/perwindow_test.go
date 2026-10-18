package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// EVERY WINDOW IS MIRRORED — the server half. A per-window host (Msg.Windows)
// has one entry per tmux window, addressed by (session, window id); a
// per-session host (an older muster-mirror) is listed and addressed exactly as
// it always was.

// winBackend is a per-window host's tmux, played: a settable window list and
// attaches recorded by what was asked for.
type winBackend struct {
	mu       sync.Mutex
	windows  []mirrorhost.WindowInfo
	attached []string // "session" (the session attach) or "session @id" (the window attach)
	pasted   []string // "pane:text"
}

func (b *winBackend) set(w ...mirrorhost.WindowInfo) {
	b.mu.Lock()
	b.windows = w
	b.mu.Unlock()
}

func (b *winBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.windows), nil
}

func (b *winBackend) List(context.Context) ([]string, error) { return nil, nil }

func (b *winBackend) term(what string) mirrorhost.Terminal {
	b.mu.Lock()
	b.attached = append(b.attached, what)
	b.mu.Unlock()
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("SCREEN " + what + "\r\n")) }()
	return &fakeTerm{session: what, r: pr, w: pw}
}

func (b *winBackend) Attach(_ context.Context, session string, _ bool, _, _ uint16) (mirrorhost.Terminal, error) {
	return b.term(session), nil
}

func (b *winBackend) AttachWindow(_ context.Context, session, window string, _ bool, _, _ uint16) (mirrorhost.Terminal, error) {
	return b.term(session + " " + window), nil
}

func (b *winBackend) ClaudePanes(context.Context, []string) map[string]mirrorhost.ClaudePane {
	return nil
}

func (b *winBackend) Paste(ctx context.Context, p mirrorhost.ClaudePane, text string) error {
	return b.PastePane(ctx, p.Pane, text)
}

func (b *winBackend) ActivePane(context.Context, string) (string, bool) { return "", false }

func (b *winBackend) PastePane(_ context.Context, pane, text string) error {
	b.mu.Lock()
	b.pasted = append(b.pasted, pane+":"+text)
	b.mu.Unlock()
	return nil
}

func (b *winBackend) attaches() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.attached)
}

func (b *winBackend) pastes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.pasted)
}

// Gold is scratch2's codename. Its ACTIVE window is the one at index 8, id @7 —
// neither the first listed nor the lowest id, so "the active window" cannot be
// confused with either.
var goldWindows = []mirrorhost.WindowInfo{
	{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1, Command: "zsh"},
	{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2, Command: "claude", Claude: &mirrorhost.ClaudePane{Pane: "%21", Gen: "g7"}},
	{Session: "scratch2", ID: "@9", Index: 11, Pane: "%33", PanePID: 3, Command: "opencode"},
	{Session: "solo", ID: "@2", Index: 0, Active: true, Pane: "%5", PanePID: 4, Command: "vim"},
}

func titles(es []mirror.SessionView) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Title() + "=" + e.Window
	}
	return out
}

// TestAPerWindowHostListsEveryWindowAndASessionAddressReachesItsActiveOne is the
// feature end to end over a real client: every window is an entry, titled
// "<codename>:<index>" (or "<session>:<index>" with no codename) and carrying
// what runs in it; an address naming a window reaches THAT window through the
// host's window attach; and an address naming only the session — every URL from
// before windows were addressable — reaches the session's ACTIVE window, as an
// explicit window id resolved by muster (never the host's session attach, which
// would leave "which window" to whoever looks last).
func TestAPerWindowHostListsEveryWindowAndASessionAddressReachesItsActiveOne(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	b := &winBackend{windows: goldWindows}
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "ws", Backend: mirrorhost.NoShrink(b),
		Codenames: map[string]string{"scratch2": "Gold"}, Interactive: true, AllowHTTP: true,
		Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = c.RunOnce(ctx) }()
	r.online(t, 1, "ws", "scratch2")

	h := r.svc.Hosts(asUser(1))[0]
	if got, want := titles(h.Entries()), []string{"Gold:1=@4", "Gold:8=@7", "Gold:11=@9", "solo:0=@2"}; !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	kinds := []string{}
	for _, e := range h.Entries() {
		kinds = append(kinds, e.Kind())
	}
	if want := []string{mirror.KindShell, mirror.KindClaudeCode, mirror.KindOpenCode, "vim"}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %q, want %q", kinds, want)
	}
	// The per-session view muster derives: each session by its ACTIVE window.
	if !slices.Equal(h.Sessions, []string{"scratch2", "solo"}) || !slices.Equal(h.Claude, []string{"scratch2"}) || h.Labels["scratch2"] != "Gold:8" {
		t.Fatalf("per-session view = %v / claude %v / labels %v", h.Sessions, h.Claude, h.Labels)
	}
	if e, ok := h.Find("scratch2", ""); !ok || e.Window != "@7" || !e.Active || !e.Claude {
		t.Fatalf("Find(scratch2, \"\") = %+v, %v; want the active window @7", e, ok)
	}
	if e, ok := h.Find("scratch2", "@9"); !ok || e.Index != 11 || e.Active || e.Claude || e.Title() != "Gold:11" {
		t.Fatalf("Find(scratch2, @9) = %+v, %v", e, ok)
	}
	if _, ok := h.Find("scratch2", "@2"); ok {
		t.Fatal("a window was found under a session it is not in")
	}

	// A window address: that window, through the window attach.
	_, screen, _, cancel, _ := attachWindow(t, asUser(1), r.svc, "ws", "scratch2", "@9", true)
	eventually(t, "the screen of window @9", func() bool { return strings.Contains(screen.String(), "SCREEN scratch2 @9") })
	cancel()
	// A session address: its ACTIVE window, as an explicit id.
	_, screen, _, cancel, _ = attach(t, asUser(1), r.svc, "ws", "scratch2", true)
	eventually(t, "the screen of the active window", func() bool { return strings.Contains(screen.String(), "SCREEN scratch2 @7") })
	cancel()
	if got, want := b.attaches(), []string{"scratch2 @9", "scratch2 @7"}; !slices.Equal(got, want) {
		t.Fatalf("the host attached %q, want %q — never the bare session", got, want)
	}

	// What is not listed is refused before the host is asked.
	_, _, _, _, res := attachWindow(t, asUser(1), r.svc, "ws", "scratch2", "@99", true)
	if err := within(t, res, "an unlisted window"); !errors.Is(err, mirror.ErrWindowGone) {
		t.Fatalf("attach to an unlisted window = %v, want ErrWindowGone", err)
	}
	_, _, _, _, res = attachWindow(t, asUser(1), r.svc, "ws", "solo", "@7", true)
	if err := within(t, res, "a window of another session"); !errors.Is(err, mirror.ErrWindowGone) {
		t.Fatalf("attach to another session's window = %v, want ErrWindowGone", err)
	}
	_, _, _, _, res = attachWindow(t, asUser(1), r.svc, "ws", "nope", "@7", true)
	if err := within(t, res, "an unlisted session"); !errors.Is(err, mirror.ErrSessionGone) {
		t.Fatalf("attach to an unlisted session = %v, want ErrSessionGone", err)
	}
	if got := b.attaches(); len(got) != 2 {
		t.Fatalf("a refused attach reached the host: %q", got)
	}

	// A message to a window nobody is looking at: its own pane, by pane id.
	if err := r.svc.PasteWindow(asUser(1), "ws", "scratch2", "@9", "to opencode", mirror.PasteActivePane); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.PasteWindow(asUser(1), "ws", "scratch2", "", "to claude", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	if got, want := b.pastes(), []string{"%33:to opencode", "%21:to claude"}; !slices.Equal(got, want) {
		t.Fatalf("pasted %q, want %q", got, want)
	}
	// A prompt for Claude is never typed into a window that is not Claude Code.
	if err := r.svc.PasteWindow(asUser(1), "ws", "scratch2", "@4", "for claude", mirror.PasteClaude); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("a Claude paste into a shell window = %v, want ErrNotClaude", err)
	}

	// The window closes: it is gone from the list, and no longer addressable.
	b.set(goldWindows[0], goldWindows[1], goldWindows[3])
	eventually(t, "the closed window to leave the list", func() bool {
		_, ok := r.svc.Hosts(asUser(1))[0].Find("scratch2", "@9")
		return !ok
	})
	if err := r.svc.PasteWindow(asUser(1), "ws", "scratch2", "@9", "too late", mirror.PasteActivePane); !errors.Is(err, mirror.ErrWindowGone) {
		t.Fatalf("a paste into a closed window = %v, want ErrWindowGone", err)
	}
}

// attachWindow is attach (fakes_test.go) on one window.
func attachWindow(t *testing.T, ctx context.Context, svc *mirror.Service, host, session, window string, readOnly bool) (stdin io.WriteCloser, screen *syncBuf, resize chan struct{}, cancel func(), result <-chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	screen = &syncBuf{}
	ctx, c := context.WithCancel(ctx)
	ch := make(chan error, 1)
	go func() { ch <- svc.AttachWindow(ctx, host, session, window, readOnly, pr, screen, nil) }()
	t.Cleanup(func() { c(); _ = pw.Close() })
	return pw, screen, nil, c, ch
}

func (h *rawHost) of(typ string) []mirror.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []mirror.Msg
	for _, m := range h.text {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func (h *rawHost) write(t *testing.T, m mirror.Msg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := h.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// TestAPerSessionHostIsListedAndAddressedExactlyAsBefore is the back-compat
// half, on the wire: a host that sends only Sessions (every muster-mirror
// before per-window registration) is one entry per SESSION, its requests carry
// NO window, and it cannot be asked for one. An INVARIANT GUARD of the old
// behaviour, plus the one new refusal.
func TestAPerSessionHostIsListedAndAddressedExactlyAsBefore(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "old")
	m := hello("old", true, "main", "scratch")
	m.Claude, m.Labels, m.PanePaste = []string{"main"}, map[string]string{"main": "mango:3"}, true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "old", "main")
	h := r.svc.Hosts(asUser(1))[0]
	if h.Windows != nil {
		t.Fatalf("a host that sent no windows has %v", h.Windows)
	}
	if got, want := titles(h.Entries()), []string{"mango:3=", "scratch="}; !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v (one per session, no window)", got, want)
	}
	if e, ok := h.Find("main", ""); !ok || !e.Claude || !e.Active || e.Window != "" {
		t.Fatalf("Find(main) = %+v, %v", e, ok)
	}

	_, _, _, cancel, _ := attach(t, asUser(1), r.svc, "old", "main", true)
	eventually(t, "the open", func() bool { _, ok := raw.opened(); return ok })
	if open, _ := raw.opened(); open.Window != "" || open.Session != "main" {
		t.Fatalf("the open sent a per-session host = %+v; it must name the session and NO window", open)
	}
	cancel()
	go func() { _ = r.svc.Paste(asUser(1), "old", "scratch", "hi", mirror.PasteActivePane) }()
	eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
	if p := raw.of(mirror.TypePaste)[0]; p.Window != "" || p.Session != "scratch" || !p.AnyPane {
		t.Fatalf("the paste sent a per-session host = %+v", p)
	}

	// It has no windows to name.
	_, _, _, _, res := attachWindow(t, asUser(1), r.svc, "old", "main", "@1", true)
	if err := within(t, res, "a window of a per-session host"); !errors.Is(err, mirror.ErrWindowGone) {
		t.Fatalf("attach to a window of a per-session host = %v, want ErrWindowGone", err)
	}
	if err := r.svc.PasteWindow(asUser(1), "old", "main", "@1", "x", mirror.PasteClaude); !errors.Is(err, mirror.ErrWindowGone) {
		t.Fatalf("paste to a window of a per-session host = %v, want ErrWindowGone", err)
	}
	if n := len(raw.of(mirror.TypeOpen)) + len(raw.of(mirror.TypePaste)); n != 2 {
		t.Fatalf("a refused window request reached the host (%d requests)", n)
	}
}

func winHello(host string, interactive bool, windows ...mirror.Window) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeHello, Version: mirror.ProtocolVersion, Host: host, Interactive: interactive, Windows: windows}
}

// TestMusterDerivesAPerWindowHostsSessionsItself: a host that sends windows is
// described by them alone. Whatever it ALSO claims in the per-session fields —
// a session with no window, Claude Code in a session whose active window is a
// shell, a label — is not kept: those fields are for an older muster, and a
// newer one that trusted them could be told a session exists that no window
// backs.
func TestMusterDerivesAPerWindowHostsSessionsItself(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true,
		mirror.Window{Session: "real", ID: "@1", Index: 0, Active: true, Command: "zsh"},
		mirror.Window{Session: "real", ID: "@2", Index: 1, Claude: true, Label: "Gold:1"})
	m.Sessions, m.Claude, m.Labels = []string{"ghost", "real"}, []string{"ghost", "real"}, map[string]string{"real": "lie:9", "ghost": "g:1"}
	m.Activity = map[string]int64{"ghost": time.Now().Unix()}
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "real")
	h := r.svc.Hosts(asUser(1))[0]
	if !slices.Equal(h.Sessions, []string{"real"}) || len(h.Claude) != 0 || len(h.Labels) != 0 || len(h.Activity) != 0 {
		t.Fatalf("per-session view = %v / claude %v / labels %v / activity %v; want only [real], derived from its windows",
			h.Sessions, h.Claude, h.Labels, h.Activity)
	}
	if r.svc.Hub().Online(1, "h", "ghost") {
		t.Fatal("a session no window backs is listed")
	}
	_, _, _, _, res := attach(t, asUser(1), r.svc, "h", "ghost", true)
	if err := within(t, res, "the ghost session"); !errors.Is(err, mirror.ErrSessionGone) {
		t.Fatalf("attach to a session no window backs = %v, want ErrSessionGone", err)
	}
	// The session's active window is a shell: no rendered view through the
	// session address, whatever the host's Claude list said …
	_, _, done := watchAs(t, asUser(1), r.svc, "h", "real", "", 0)
	if err := within(t, done, "the session's rendered view"); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("watch of a session whose active window is a shell = %v, want ErrNotClaude", err)
	}
	// … and the window that IS Claude Code has one, addressed by its id.
	go func() {
		_ = r.svc.WatchWindow(asUser(1), "h", "real", "@2", "", 0, func(mirror.Msg) error { return nil })
	}()
	eventually(t, "the watch for the Claude window", func() bool { return len(raw.of(mirror.TypeWatch)) == 1 })
	if w := raw.of(mirror.TypeWatch)[0]; w.Window != "@2" || w.Session != "real" {
		t.Fatalf("the watch sent the host = %+v, want window @2", w)
	}
}

// TestNormalizeWindowsKeepsWhatTheTunnelCarries is the window list's one
// normalizer, case by case: what is dropped, what is blanked, the order, the
// single Active per session — and the bound, at 74 windows (one real host,
// which the old per-session bound of 64 could not hold), at the bound and past
// it.
func TestNormalizeWindowsKeepsWhatTheTunnelCarries(t *testing.T) {
	now := int64(1791904000)
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "b", ID: "@3", Index: 2, Active: true, Label: "ok:2", Command: "zsh", Activity: 1791903899},
		{Session: "b", ID: "@1", Index: 0, Active: true}, // a second Active in one session
		{Session: "a", ID: "@9", Index: 5, Label: strings.Repeat("x", mirror.MaxLabel+1), Command: strings.Repeat("c", mirror.MaxCommand+1)},
		{Session: "a", ID: "@9", Index: 6},                          // a duplicate (session, id): the first wins
		{Session: "a", ID: "@8", Index: 5, Activity: now + 86400},   // a clock ahead: capped at now
		{Session: "a", ID: "9", Index: 0},                           // not a window id
		{Session: "a", ID: "@", Index: 0},                           //
		{Session: "a", ID: "@1x", Index: 0},                         //
		{Session: "a", ID: "@7", Index: -1},                         // a negative index
		{Session: "", ID: "@7"},                                     // no session
		{Session: "bad\x00name", ID: "@7"},                          //
		{Session: "muster-view-0123456789ab", ID: "@7", Index: 0},   // muster-mirror's own viewer session
		{Session: "a", ID: "@5", Index: 1, Command: "bad\x1bcmd"},   // a control character
		{Session: "a", ID: "@6", Index: 3, Activity: -7, Label: ""}, //
	}, now)
	want := []mirror.Window{
		{Session: "a", ID: "@5", Index: 1},
		{Session: "a", ID: "@6", Index: 3},
		{Session: "a", ID: "@8", Index: 5, Activity: now - now%mirror.ActivityBucket},
		{Session: "a", ID: "@9", Index: 5},
		{Session: "b", ID: "@1", Index: 0, Active: true},
		{Session: "b", ID: "@3", Index: 2, Label: "ok:2", Command: "zsh", Activity: 1791903899 - 1791903899%mirror.ActivityBucket},
	}
	if !slices.EqualFunc(got, want, mirror.Window.Equal) {
		t.Fatalf("NormalizeWindows =\n  %+v\nwant\n  %+v", got, want)
	}
	if mirror.NormalizeWindows(nil, now) != nil {
		t.Fatal("no window list normalized to a list: a per-session host would be taken for a per-window one")
	}
	if got := mirror.NormalizeWindows([]mirror.Window{{Session: "", ID: "@1"}}, now); got == nil || len(got) != 0 {
		t.Fatalf("an all-invalid list = %v; want empty and NOT nil (it is still a per-window host)", got)
	}

	many := func(n int) []mirror.Window {
		out := make([]mirror.Window, 0, n)
		for i := n - 1; i >= 0; i-- { // unsorted input
			out = append(out, mirror.Window{Session: fmt.Sprintf("s%03d", i/11), ID: fmt.Sprintf("@%d", i), Index: i % 11})
		}
		return out
	}
	for _, n := range []int{74, mirror.MaxWindows, mirror.MaxWindows + 40} {
		got := mirror.NormalizeWindows(many(n), now)
		if want := min(n, mirror.MaxWindows); len(got) != want {
			t.Fatalf("%d windows normalized to %d, want %d", n, len(got), want)
		}
		if !slices.IsSortedFunc(got, func(a, b mirror.Window) int {
			if c := strings.Compare(a.Session, b.Session); c != 0 {
				return c
			}
			return a.Index - b.Index
		}) {
			t.Fatalf("%d windows: the result is not in (session, index) order", n)
		}
	}
	if mirror.MaxWindows <= mirror.MaxSessions {
		t.Fatalf("MaxWindows (%d) does not exceed the per-session bound (%d)", mirror.MaxWindows, mirror.MaxSessions)
	}
}

// TestAHostWithMoreWindowsThanTheOldSessionBoundListsThemAll is the regression
// the operator hit, at the hub: a host with 74 windows (in 20 sessions) lists
// all 74 — where one entry per session, bounded at 64, showed 20 and could
// never have shown more than 64.
func TestAHostWithMoreWindowsThanTheOldSessionBoundListsThemAll(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "big")
	var ws []mirror.Window
	for i := 0; i < 74; i++ {
		ws = append(ws, mirror.Window{Session: fmt.Sprintf("s%02d", i%20), ID: fmt.Sprintf("@%d", i), Index: i / 20, Active: i < 20})
	}
	if _, err := r.rawConnect(t, tok, winHello("big", false, ws...)); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "big", "s00")
	h := r.svc.Hosts(asUser(1))[0]
	if len(h.Windows) != 74 || len(h.Entries()) != 74 || len(mirror.Ordered([]mirror.HostView{h})) != 74 {
		t.Fatalf("a 74-window host lists %d windows / %d entries; want 74", len(h.Windows), len(h.Entries()))
	}
	if len(h.Sessions) != 20 {
		t.Fatalf("its sessions = %d, want 20", len(h.Sessions))
	}
	if mirror.MaxSessions >= 74 {
		t.Fatalf("instrument check: the per-session bound (%d) would have held 74 — this is not past it", mirror.MaxSessions)
	}
}

func orderOfWindows(hosts []mirror.HostView) []string {
	var out []string
	for _, e := range mirror.Ordered(hosts) {
		out = append(out, e.Host+"/"+e.Title())
	}
	return out
}

// TestWindowsAreOrderedMostRecentlyActiveFirstAndOnlyAVisibleChangeNudges: the
// list is ONE order across hosts and windows — most recently active first, two
// windows of one session far apart when their activity is. And each visible
// change nudges once: a time moving without the order changing (the card's
// "active … ago" — throttled per owner, see
// TestABusyWindowThatStaysFirstRefreshesItsCardWithinTheThrottle), a changed
// order, a renumbered window with its new label.
//
// 🔴 Each silent step is proven processed before its count is read, and the
// counter is proven able to move (the connect, and the last steps).
func TestWindowsAreOrderedMostRecentlyActiveFirstAndOnlyAVisibleChangeNudges(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	var nudges atomic.Int64
	r.svc.SetNotify(func(owner int64) {
		if owner == 1 {
			nudges.Add(1)
		}
	})
	now := bucket(time.Now().Unix()) - 20*mirror.ActivityBucket
	at := func(n int64) int64 { return now + n*mirror.ActivityBucket }
	tokA, _ := r.store.mint(t, 1, "laptop")
	tokB, _ := r.store.mint(t, 1, "tower")
	gold := func(a4, a7, a9 int64, idx9 int, label9 string) []mirror.Window {
		return []mirror.Window{
			{Session: "scratch2", ID: "@4", Index: 1, Label: "Gold:1", Activity: a4},
			{Session: "scratch2", ID: "@7", Index: 8, Active: true, Label: "Gold:8", Activity: a7},
			{Session: "scratch2", ID: "@9", Index: idx9, Label: label9, Activity: a9},
		}
	}
	rawA, err := r.rawConnect(t, tokA, winHello("laptop", false, gold(at(9), at(1), at(5), 11, "Gold:11")...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.rawConnect(t, tokB, winHello("tower", false,
		mirror.Window{Session: "main", ID: "@1", Index: 0, Active: true, Activity: at(7)},
		mirror.Window{Session: "main", ID: "@2", Index: 1, Activity: at(3)})); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "scratch2")
	r.online(t, 1, "tower", "main")
	order := func() []string { return orderOfWindows(r.svc.Hub().Hosts(1)) }
	if got, want := order(), []string{"laptop/Gold:1", "tower/main:0", "laptop/Gold:11", "tower/main:1", "laptop/Gold:8"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v\n  want %v", got, want)
	}
	settle := func() int64 {
		time.Sleep(120 * time.Millisecond)
		return nudges.Load()
	}
	base := settle()
	if base == 0 {
		t.Fatal("instrument check FAILED: connecting two hosts nudged nobody")
	}
	send := func(ws []mirror.Window) {
		rawA.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: ws})
	}
	activityOf := func(id string) int64 {
		e, _ := hostView(t, r, 1, "laptop").Find("scratch2", id)
		return e.Activity
	}

	// 1. The top window stays busy: its time moves, the order does not. Its
	// card's "active … ago" moved, so the views are told.
	send(gold(at(10), at(1), at(5), 11, "Gold:11"))
	eventually(t, "the hub to take @4's newer time", func() bool { return activityOf("@4") == at(10) })
	if got := settle(); got != base+1 {
		t.Fatalf("a window's shown time moving without the order changing nudged %d time(s), want 1", got-base)
	}
	// 2. The LAST window becomes the most recent: the order changes — at once,
	// although step 1 just spent this host's activity throttle.
	send(gold(at(10), at(12), at(5), 11, "Gold:11"))
	eventually(t, "the active window to move to the top", func() bool { return order()[0] == "laptop/Gold:8" })
	if got := settle(); got != base+2 {
		t.Fatalf("a changed order nudged %d time(s), want 1", got-base-1)
	}
	// 3. A window is renumbered (its label follows): same order, visible change.
	send(gold(at(10), at(12), at(5), 2, "Gold:2"))
	eventually(t, "the renumbered window", func() bool {
		e, _ := hostView(t, r, 1, "laptop").Find("scratch2", "@9")
		return e.Index == 2 && e.Label == "Gold:2"
	})
	if got := settle(); got != base+3 {
		t.Fatalf("a renumbered window nudged %d time(s) in total, want 3", got-base)
	}
	if got, want := order(), []string{"laptop/Gold:8", "laptop/Gold:1", "tower/main:0", "laptop/Gold:2", "tower/main:1"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v\n  want %v", got, want)
	}
}

// TestABusyWindowThatStaysFirstRefreshesItsCardWithinTheThrottle: the window
// at the top of the list stays busy, so every refresh moves only its time and
// never the order. Its card still has to follow ("active … ago" would
// otherwise freeze at the time the page was opened while the window works):
// the views are nudged for it, at most once per throttle window, and the
// re-fetch that nudge triggers reads the NEWEST time — never a storm of one
// nudge per message, never silence.
func TestABusyWindowThatStaysFirstRefreshesItsCardWithinTheThrottle(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(5 * time.Millisecond)
	const throttle = 500 * time.Millisecond
	r.svc.SetActivityNudgeEvery(throttle)
	var nudges atomic.Int64
	// What an open view would re-fetch at each nudge: the busy window's time.
	var seen sync.Map // nudge number -> activity read at that nudge
	r.svc.SetNotify(func(owner int64) {
		if owner != 1 {
			return
		}
		var at int64
		for _, h := range r.svc.Hub().Hosts(1) {
			if e, ok := h.Find("main", "@1"); ok {
				at = e.Activity
			}
		}
		seen.Store(nudges.Add(1), at)
	})
	tok, _ := r.store.mint(t, 1, "laptop")
	old := bucket(time.Now().Unix()) - 600*mirror.ActivityBucket
	other := old // @2's time: far behind @1, so never its session's newest
	busy := func(a int64) []mirror.Window {
		return []mirror.Window{
			{Session: "main", ID: "@1", Index: 0, Active: true, Activity: a},
			{Session: "main", ID: "@2", Index: 1, Activity: other},
		}
	}
	start := bucket(time.Now().Unix()) - 100*mirror.ActivityBucket
	raw, err := r.rawConnect(t, tok, winHello("laptop", false, busy(start)...))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "main")
	time.Sleep(120 * time.Millisecond) // well past the debounce: the connect's nudge has fired
	base := nudges.Load()
	if base == 0 {
		t.Fatal("instrument check FAILED: the connect nudged nobody, so this counter cannot see a nudge")
	}
	order := func() []string { return orderOfWindows(r.svc.Hub().Hosts(1)) }
	want := order()
	if want[0] != "laptop/main:0" {
		t.Fatalf("the busy window is not first: %v", want)
	}

	// Eight messages over ~2 throttle windows, each moving the busy window one
	// bucket and nothing else.
	const msgs = 8
	last := start
	began := time.Now()
	for i := 1; i <= msgs; i++ {
		last = start + int64(i)*mirror.ActivityBucket
		raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: busy(last)})
		eventually(t, "the hub to take the busy window's newer time", func() bool {
			e, _ := hostView(t, r, 1, "laptop").Find("main", "@1")
			return e.Activity == last
		})
		if got := order(); !slices.Equal(got, want) {
			t.Fatalf("the order moved (%v), so this is not the activity-only case", got)
		}
		time.Sleep(throttle / 4)
	}
	// The card follows within one throttle window of the last change: some
	// nudge re-fetches the newest time.
	eventually(t, "a nudge that re-fetches the newest time", func() bool {
		v, ok := seen.Load(nudges.Load())
		return ok && v.(int64) == last
	})
	if since := time.Since(began); since > time.Duration(msgs)*throttle/4+2*throttle {
		t.Fatalf("the card caught up %v after the burst began: later than one throttle window past the last change", since)
	}
	time.Sleep(throttle + 120*time.Millisecond)
	n := nudges.Load() - base
	// Not a storm: one per throttle window over the burst, plus the trailing one.
	if maxN := int64(time.Since(began)/throttle) + 1; n < 2 || n > maxN {
		t.Fatalf("%d message(s) moving the busy window's time nudged %d time(s), want 2..%d (once per %v window)", msgs, n, maxN, throttle)
	}

	// A window that is NOT its session's newest moves (the throttle window is
	// open again): the session's own time does not change, but that window's
	// card does — so it is compared per window, not only per session.
	other = old + mirror.ActivityBucket
	raw.write(t, mirror.Msg{Type: mirror.TypeSessions, Windows: busy(last)})
	eventually(t, "the hub to take @2's newer time", func() bool {
		e, _ := hostView(t, r, 1, "laptop").Find("main", "@2")
		return e.Activity == other
	})
	time.Sleep(120 * time.Millisecond)
	if got := nudges.Load() - base - n; got != 1 {
		t.Fatalf("a non-newest window's shown time moving nudged %d time(s), want 1", got)
	}
}

// TestAV1HostThatSendsWindowsHasNoRenderedView: the rendered view is tunnel v2;
// a v1 connection's windows are listed, and none of them is Claude Code
// whatever the host claims.
func TestAV1HostThatSendsWindowsHasNoRenderedView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", false, mirror.Window{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true})
	m.Version = 1
	if _, err := r.rawConnect(t, tok, m); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "main")
	h := r.svc.Hosts(asUser(1))[0]
	if e, ok := h.Find("main", "@1"); !ok || e.Claude || e.HasRenderedView() || h.Rendered {
		t.Fatalf("a v1 host's window = %+v (rendered %v); it has no rendered view", e, h.Rendered)
	}
	_, _, done := watchAs(t, asUser(1), r.svc, "h", "main", "", 0)
	if err := within(t, done, "a v1 host's rendered view"); !errors.Is(err, mirror.ErrNoRenderedView) {
		t.Fatalf("watch on a v1 host = %v, want ErrNoRenderedView", err)
	}
}
