package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// ---------------------------------------------------------------------------
// PRETTY NAMES: a host may label its sessions ("scratch13" → "mango:1"). The
// label is display-only, optional on the wire in both directions, and bounded
// like a session name.
// ---------------------------------------------------------------------------

func TestValidLabel(t *testing.T) {
	for _, ok := range []string{"mango:1", "Gold:8", "a", "Ünïcødé:12", strings.Repeat("x", mirror.MaxLabel)} {
		if err := mirror.ValidLabel(ok); err != nil {
			t.Errorf("ValidLabel(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", mirror.MaxLabel+1), "a\x1b[31m", "a\tb", "a\x7f", "a\u0085b", string([]byte{0xff, 'a'})} {
		if mirror.ValidLabel(bad) == nil {
			t.Errorf("ValidLabel(%q) = nil, want a refusal", bad)
		}
	}
}

func TestNormalizeLabelsKeepsOnlyListedSessionsWithValidLabels(t *testing.T) {
	got := mirror.NormalizeLabels(map[string]string{
		"scratch13": "mango:1", // kept
		"scratch2":  "Gold:8",  // kept
		"ghost":     "Ghost:1", // not listed: dropped
		"main":      "bad\x1b", // control character: dropped
		"other":     "",        // blank: dropped
	}, []string{"main", "other", "scratch13", "scratch2", "plain"})
	want := map[string]string{"scratch13": "mango:1", "scratch2": "Gold:8"}
	if !maps.Equal(got, want) {
		t.Fatalf("NormalizeLabels = %v, want %v", got, want)
	}
	if got := mirror.NormalizeLabels(map[string]string{"ghost": "x"}, []string{"main"}); got != nil {
		t.Fatalf("nothing valid left must be nil (omitted on the wire), got %v", got)
	}
	if got := mirror.NormalizeLabels(nil, []string{"main"}); got != nil {
		t.Fatalf("no labels must be nil, got %v", got)
	}
}

// TestLabelsAreOmittedFromTheWireWhenAbsent: a host with no labels sends exactly
// what an older host sends, so an older muster sees no difference.
func TestLabelsAreOmittedFromTheWireWhenAbsent(t *testing.T) {
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: []string{"main"}})
	if strings.Contains(string(b), "labels") {
		t.Fatalf("an unlabelled message carries a labels key: %s", b)
	}
	b, _ = json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: []string{"main"}, Labels: map[string]string{"main": "m:0"}})
	if !strings.Contains(string(b), `"labels":{"main":"m:0"}`) {
		t.Fatalf("a labelled message does not carry them: %s", b)
	}
}

func hostView(t *testing.T, r *rig, owner int64, host string) mirror.HostView {
	t.Helper()
	for _, h := range r.svc.Hub().Hosts(owner) {
		if h.Host == host {
			return h
		}
	}
	t.Fatalf("host %s is not in owner %d's view", host, owner)
	return mirror.HostView{}
}

// TestTheHubKeepsValidLabelsAndDropsTheRestWithoutRefusingTheHello: a hello
// carrying a mix of good and bad labels registers, with only the good ones.
func TestTheHubKeepsValidLabelsAndDropsTheRestWithoutRefusingTheHello(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	h := hello("laptop", false, "scratch13", "plain")
	h.Labels = map[string]string{"scratch13": "mango:1", "plain": strings.Repeat("x", mirror.MaxLabel+1), "unlisted": "u:1"}
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatalf("a hello with an invalid label was refused: %v", err)
	}
	r.online(t, 1, "laptop", "plain")
	if got := hostView(t, r, 1, "laptop").Labels; !maps.Equal(got, map[string]string{"scratch13": "mango:1"}) {
		t.Fatalf("labels = %v, want only scratch13's", got)
	}

	// A sessions message REPLACES the labels: a moved window updates one, and a
	// message with none clears them (the host stopped labelling).
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSessions, Sessions: []string{"scratch13", "plain"}, Labels: map[string]string{"scratch13": "mango:3"}})
	if err := raw.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the label to follow the sessions message", func() bool {
		return hostView(t, r, 1, "laptop").Labels["scratch13"] == "mango:3"
	})
	raw.sendSessions(t, "scratch13", "plain")
	eventually(t, "a sessions message without labels to clear them", func() bool {
		return hostView(t, r, 1, "laptop").Labels == nil
	})
}

// TestAnOfflineHostKeepsItsLastLabels: a disconnected host is shown with its
// last list, and that list keeps its labels.
func TestAnOfflineHostKeepsItsLastLabels(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	h := hello("laptop", false, "scratch2")
	h.Labels = map[string]string{"scratch2": "Gold:8"}
	raw, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "laptop", "scratch2")
	raw.ws.CloseNow()
	eventually(t, "the host to go offline", func() bool {
		hv := r.svc.Hub().Hosts(1)
		return len(hv) == 1 && !hv[0].Online
	})
	if got := hostView(t, r, 1, "laptop").Labels; !maps.Equal(got, map[string]string{"scratch2": "Gold:8"}) {
		t.Fatalf("offline labels = %v", got)
	}
}

// windowBackend is fakeBackend plus active windows: a host whose sessions keep
// their names while the operator moves between windows.
type windowBackend struct {
	fakeBackend
	wmu     sync.Mutex
	windows map[string]string
}

func (b *windowBackend) setWindow(session, idx string) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if b.windows == nil {
		b.windows = map[string]string{}
	}
	b.windows[session] = idx
}

func (b *windowBackend) ActiveWindows(context.Context) (map[string]string, error) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return maps.Clone(b.windows), nil
}

// TestAMovedActiveWindowReachesMusterWithTheSameSessionSet is the regression
// for the refresh diff: the session SET never changes, only scratch13's active
// window — and muster must still end up showing the new label. A refresh that
// compares only the session list (and the Claude set) never re-sends, and the
// card stays "mango:1" forever.
//
// It runs the REAL muster-mirror client against the REAL hub, through
// NoShrink as the shipped binary wraps its backend, so the window capability
// is found through the wrapper exactly as in production.
func TestAMovedActiveWindowReachesMusterWithTheSameSessionSet(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "laptop")
	b := &windowBackend{}
	b.set("plain", "scratch13")
	b.setWindow("scratch13", "1")
	b.setWindow("plain", "4")

	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "laptop", Backend: mirrorhost.NoShrink(b),
		Codenames: map[string]string{"scratch13": "mango"}, AllowHTTP: true,
		Refresh: 30 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()

	r.online(t, 1, "laptop", "scratch13")
	eventually(t, "the hello's label mango:1", func() bool {
		return maps.Equal(hostView(t, r, 1, "laptop").Labels, map[string]string{"scratch13": "mango:1"})
	})

	b.setWindow("scratch13", "3")
	eventually(t, "the moved window's label mango:3 (same session set)", func() bool {
		return maps.Equal(hostView(t, r, 1, "laptop").Labels, map[string]string{"scratch13": "mango:3"})
	})
	// "plain" has no codename: it is never labelled, whatever its window.
	if _, ok := hostView(t, r, 1, "laptop").Labels["plain"]; ok {
		t.Fatal("a session without a codename was labelled")
	}
}
