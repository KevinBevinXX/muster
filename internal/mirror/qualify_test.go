package mirror_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// SAME NAME, TWO HOSTS. A host labels its windows "<codename>:<index>", so two
// hosts each with a `mango` session both list "mango:1" — and the Agents list
// showed two cards titled exactly that, the host only in a chip. Ordered now
// gives every entry whose Title another listed entry shares a Qualifier, and
// DisplayTitle is what every surface calls it.
// ---------------------------------------------------------------------------

// win is a per-window host's window: session, id, index, label.
func win(session, id string, index int, label string) mirror.Window {
	return mirror.Window{Session: session, ID: id, Index: index, Label: label}
}

func perWindow(host string, ws ...mirror.Window) mirror.HostView {
	return mirror.HostView{Host: host, Online: true, Windows: ws}
}

// displayTitles is every listed entry's DisplayTitle keyed by host/session/window.
func displayTitles(hosts []mirror.HostView) map[string]string {
	out := map[string]string{}
	for _, e := range mirror.Ordered(hosts) {
		out[e.Host+"/"+e.Session+"/"+e.Window] = e.DisplayTitle()
	}
	return out
}

func assertTitles(t *testing.T, hosts []mirror.HostView, want map[string]string) {
	t.Helper()
	got := displayTitles(hosts)
	if !maps.Equal(got, want) {
		t.Fatalf("display titles:\n got %v\nwant %v", got, want)
	}
	// Whatever the fixture: no two listed entries are called the same.
	seen := map[string]string{}
	for k, v := range got {
		if other, dup := seen[v]; dup {
			t.Fatalf("%s and %s are both titled %q", k, other, v)
		}
		seen[v] = k
	}
}

func TestAUniqueTitleIsUnchanged(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("scratch2", "@4", 1, "")),
		perWindow("tower", win("kiwi", "@3", 1, "kiwi:1")),
	}
	for _, e := range mirror.Ordered(hosts) {
		if e.Qualifier != "" || e.DisplayTitle() != e.Title() {
			t.Fatalf("%s/%s: unique title %q got qualifier %q (display %q)", e.Host, e.Session, e.Title(), e.Qualifier, e.DisplayTitle())
		}
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/mango/@1":    "mango:1",
		"laptop/scratch2/@4": "scratch2:1",
		"tower/kiwi/@3":      "kiwi:1",
	})
}

// THE MEASURED BUG: "mango:1" on two hosts. Each gets its host; an entry whose
// title is unique beside them is left alone.
func TestATitleSharedAcrossTwoHostsGetsItsHostOnBoth(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("mango", "@2", 2, "mango:2")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/mango/@1": "mango:1 · laptop",
		"laptop/mango/@2": "mango:2",
		"tower/mango/@7":  "mango:1 · tower",
	})
	for _, e := range mirror.Ordered(hosts) {
		if e.Title() == "mango:1" && e.Qualifier != e.Host {
			t.Fatalf("%s: qualifier %q, want the host", e.Host, e.Qualifier)
		}
	}
}

func TestAThreeWayClashQualifiesAllThree(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("desk", win("mango", "@5", 1, "mango:1")),
		perWindow("laptop", win("mango", "@1", 1, "mango:1")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"desk/mango/@5":   "mango:1 · desk",
		"laptop/mango/@1": "mango:1 · laptop",
		"tower/mango/@7":  "mango:1 · tower",
	})
}

// TWO ENTRIES OF ONE HOST WITH ONE TITLE. tmux keeps window indexes unique per
// session and session names unique per host, so an UNLABELLED title
// ("<session>:<index>") cannot repeat on one host — but a LABEL can: two
// sessions given the same codename both list "Gold:1". The host does not tell
// them apart, so the tmux ADDRESS does ("<session>:<index>", unique per host).
// When the title also clashes across hosts, the host comes first.
func TestTwoEntriesOfOneHostWithOneTitleAreToldApartByTheirTmuxAddress(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("scratch3", "@2", 1, "Gold:1"), win("scratch4", "@6", 1, "Gold:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/scratch3/@2": "Gold:1 · scratch3:1",
		"laptop/scratch4/@6": "Gold:1 · scratch4:1",
	})

	hosts = append(hosts, perWindow("tower", win("gold", "@9", 1, "Gold:1")))
	assertTitles(t, hosts, map[string]string{
		"laptop/scratch3/@2": "Gold:1 · laptop · scratch3:1",
		"laptop/scratch4/@6": "Gold:1 · laptop · scratch4:1",
		"tower/gold/@9":      "Gold:1 · tower",
	})

	// A per-session host (an older muster-mirror) labels SESSIONS; its address
	// is the session name.
	old := []mirror.HostView{{Host: "attic", Online: true, Sessions: []string{"a", "b"}, Labels: map[string]string{"a": "mango", "b": "mango"}}}
	assertTitles(t, old, map[string]string{
		"attic/a/": "mango · a",
		"attic/b/": "mango · b",
	})
}

// LIVE: the qualifier is a function of the list as it is NOW. A host joining
// with a clashing label qualifies the entry that was already there; the clash
// going away (the host dropped from the list) takes it off again.
func TestTheQualifierFollowsTheListAsHostsComeAndGo(t *testing.T) {
	laptop := perWindow("laptop", win("mango", "@1", 1, "mango:1"))
	tower := perWindow("tower", win("mango", "@7", 1, "mango:1"))
	if got := displayTitles([]mirror.HostView{laptop}); got["laptop/mango/@1"] != "mango:1" {
		t.Fatalf("alone: %v", got)
	}
	if got := displayTitles([]mirror.HostView{laptop, tower}); got["laptop/mango/@1"] != "mango:1 · laptop" {
		t.Fatalf("with tower: %v", got)
	}
	if got := displayTitles([]mirror.HostView{tower}); got["tower/mango/@7"] != "mango:1" {
		t.Fatalf("laptop gone: %v", got)
	}
}

// Qualified is what a PAGE of one entry is called: the same qualifier the list
// gives it, so the page and the card that opened it agree. An entry the list
// does not have gets none.
func TestQualifiedGivesAPageTheListsQualifier(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	tower := hosts[1]
	e, ok := tower.Find("mango", "@7")
	if !ok || e.Qualifier != "" {
		t.Fatalf("Find alone: %+v %v (Find knows one host; it must not invent a qualifier)", e, ok)
	}
	if q := mirror.Qualified(hosts, e); q.Qualifier != "tower" || q.DisplayTitle() != "mango:1 · tower" {
		t.Fatalf("Qualified = %+v", q)
	}
	// Every listed entry: Qualified agrees with Ordered.
	for _, want := range mirror.Ordered(hosts) {
		bare := want
		bare.Qualifier = ""
		if got := mirror.Qualified(hosts, bare); got.Qualifier != want.Qualifier {
			t.Fatalf("%s: Qualified %q, Ordered %q", want.Host, got.Qualifier, want.Qualifier)
		}
	}
	ghost := mirror.SessionView{Host: "tower", Session: "mango", Window: "@99", Qualifier: "stale"}
	if q := mirror.Qualified(hosts, ghost); q.Qualifier != "" {
		t.Fatalf("an unlisted entry kept a qualifier: %+v", q)
	}
}

// INVARIANT GUARD (not regression coverage — the order was never broken): the
// qualifier moves nothing; the order is what it was.
func TestQualifyingDoesNotReorder(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("z", "@2", 2, "")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	var got []string
	for _, e := range mirror.Ordered(hosts) {
		got = append(got, e.Host+"/"+e.Window)
	}
	if want := []string{"laptop/@1", "laptop/@2", "tower/@7"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}
