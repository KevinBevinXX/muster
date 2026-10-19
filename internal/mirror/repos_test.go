package mirror_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestCleanRepoNameIsANameNeverAPath: what the chip may say. A path is
// refused whole (never trimmed to its last element, which would hide that the
// host sent one); control and format characters go; a long name is cut to
// MaxRepoName BYTES on a rune boundary.
func TestCleanRepoNameIsANameNeverAPath(t *testing.T) {
	for in, want := range map[string]string{
		"fixture-repo":    "fixture-repo",
		"  spaced\tname ": "spaced name",
		"home/user/repo":  "",
		"/":               "",
		".":               "",
		"..":              "",
		"":                "",
		"bi\u202edi":      "bidi",
		"bell\x07":        "bell",
		// 40 two-byte runes: cut to 64 BYTES, 32 runes, on a rune boundary.
		strings.Repeat("é", 40): strings.Repeat("é", 32),
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		// Over 64 runes: cleanLine's "…" lands past byte 63, so it is cut away.
		strings.Repeat("a", 65): strings.Repeat("a", 63),
	} {
		got := mirror.CleanRepoName(in)
		if got != want {
			t.Errorf("CleanRepoName(%q) = %q, want %q", in, got, want)
		}
		if len(got) > mirror.MaxRepoName {
			t.Errorf("CleanRepoName(%q) is %d bytes, over %d", in, len(got), mirror.MaxRepoName)
		}
	}
}

// TestAReposMessageNamesEachWindowsEntry: a per-window v2 host's repos message
// lands on its entries — only for windows it lists, only names CleanRepoName
// keeps — nudges the owner's views when it changes (and not when it repeats),
// survives the host going offline, and a host that never sends one (an older
// muster-mirror) has entries with no repo. A v1 host's message is ignored.
func TestAReposMessageNamesEachWindowsEntry(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(time.Millisecond)
	var nudges atomic.Int32
	r.svc.SetNotify(func(int64) { nudges.Add(1) })
	tok, _ := r.store.mint(t, 7, "laptop")
	h := hello("laptop", false)
	h.Windows = []mirror.Window{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Claude: true, ClaudeSession: "sess-a"},
		{Session: "scratch2", ID: "@4", Index: 1},
	}
	host, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	repoOf := func(window string) string {
		for _, hv := range r.svc.Hub().Hosts(7) {
			for _, e := range hv.Entries() {
				if e.Window == window {
					return e.Repo
				}
			}
		}
		return "<unlisted>"
	}
	eventually(t, "the host listed", func() bool { return repoOf("@7") == "" })
	// Control: before any repos message — what an older host looks like.
	if got := repoOf("@4"); got != "" {
		t.Fatalf("a host that sent no repos message has repo %q", got)
	}
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	before := nudges.Load()
	send(mirror.Msg{Type: mirror.TypeRepos, Repos: map[string]string{"@7": "fixture-repo", "@4": "plain", "@99": "ghost", "@8": "a/b"}})
	eventually(t, "the repo names", func() bool { return repoOf("@7") == "fixture-repo" })
	if got := r.svc.Hub().Hosts(7)[0].Repos; !maps.Equal(got, map[string]string{"@7": "fixture-repo", "@4": "plain"}) {
		t.Fatalf("Repos = %v, want only the listed windows' names", got)
	}
	eventually(t, "a nudge for the changed names", func() bool { return nudges.Load() > before })
	time.Sleep(20 * time.Millisecond)
	settled := nudges.Load()
	send(mirror.Msg{Type: mirror.TypeRepos, Repos: map[string]string{"@7": "fixture-repo", "@4": "plain"}})
	time.Sleep(50 * time.Millisecond)
	if n := nudges.Load(); n != settled {
		t.Fatalf("the same names again nudged %d times, want none", n-settled)
	}
	// Offline: the last names are kept with the last list.
	host.ws.CloseNow()
	eventually(t, "the host offline", func() bool { hs := r.svc.Hub().Hosts(7); return len(hs) == 1 && !hs[0].Online })
	if got := repoOf("@7"); got != "fixture-repo" {
		t.Fatalf("an offline host's entry has repo %q, want its last name", got)
	}

	// A v1 host has no rendered view, and its repos message is dropped.
	tok1, _ := r.store.mint(t, 8, "oldbox")
	h1 := hello("oldbox", false)
	h1.Version = 1
	h1.Windows = []mirror.Window{{Session: "s", ID: "@1", Index: 0, Active: true}}
	old, err := r.rawConnect(t, tok1, h1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeRepos, Repos: map[string]string{"@1": "fixture-repo"}})
	if err := old.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, hv := range r.svc.Hub().Hosts(8) {
		if len(hv.Repos) != 0 {
			t.Fatalf("a v1 host's repos were kept: %v", hv.Repos)
		}
	}
}
