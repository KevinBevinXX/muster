package ui

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestHostIconSetIsDistinct: every icon of the set is its own — key AND drawing
// — and the cluster's is none of them, so a dispatched agent can never look like
// a mirrored host's window.
func TestHostIconSetIsDistinct(t *testing.T) {
	if len(hostIconSet) < 8 {
		t.Fatalf("%d host icons; the set is meant to tell at least 8 hosts apart", len(hostIconSet))
	}
	keys, bodies := map[string]bool{}, map[string]bool{}
	for _, d := range append(append([]hostIconDef{}, hostIconSet...), clusterIcon) {
		if d.Key == "" || d.Body == "" {
			t.Fatalf("an icon with no key or no drawing: %+v", d)
		}
		if keys[d.Key] || bodies[d.Body] {
			t.Errorf("icon %q repeats a key or a drawing of another", d.Key)
		}
		keys[d.Key], bodies[d.Body] = true, true
	}
}

// ranksOf is n hosts ranked 0..n-1 in registration order (what
// mirror.RanksByRegistration answers for hosts minted one after another).
func ranksOf(n int) (map[string]int, []string) {
	r := map[string]int{}
	var hosts []string
	for i := 0; i < n; i++ {
		h := "host-" + strconv.Itoa(i)
		r[h] = i
		hosts = append(hosts, h)
	}
	return r, hosts
}

// TestTheFirstHostsDrawDistinctIcons: as many hosts as there are icons draw
// that many DIFFERENT icons — the property a hash cannot give — none of them
// the cluster's; one more wraps around to the first host's icon.
func TestTheFirstHostsDrawDistinctIcons(t *testing.T) {
	n := len(hostIconSet)
	ranks, hosts := ranksOf(n + 1)
	seen := map[string]string{}
	for _, h := range hosts[:n] {
		k := HostIconFor(ranks, h)
		if k == clusterIcon.Key {
			t.Fatalf("host %s draws the cluster's icon", h)
		}
		if o, dup := seen[k]; dup {
			t.Fatalf("hosts %s and %s both draw %q", o, h, k)
		}
		seen[k] = h
	}
	if got, want := HostIconFor(ranks, hosts[n]), HostIconFor(ranks, hosts[0]); got != want {
		t.Errorf("host %d draws %q; past the set's size it wraps to rank 0's %q", n, got, want)
	}
}

// TestAHostsIconIsStableAsHostsAreAdded: a host's icon depends on its OWN rank
// only — adding hosts (ranks after it) changes no existing host's icon.
func TestAHostsIconIsStableAsHostsAreAdded(t *testing.T) {
	before, hosts := ranksOf(3)
	after, _ := ranksOf(30)
	for _, h := range hosts {
		if a, b := HostIconFor(before, h), HostIconFor(after, h); a != b {
			t.Errorf("host %s drew %q with 3 hosts and %q with 30", h, a, b)
		}
	}
}

// TestAnUnrankedHostStillDrawsAnIconOfTheSet: a host with no rank (no token
// row the viewer can read) draws the hash fallback — the same one each time,
// one of the set, never the cluster's, never nothing.
func TestAnUnrankedHostStillDrawsAnIconOfTheSet(t *testing.T) {
	inSet := map[string]bool{}
	for _, d := range hostIconSet {
		inSet[d.Key] = true
	}
	for _, h := range []string{"unknown-a", "unknown-b", "x"} {
		k := HostIconFor(nil, h)
		if !inSet[k] {
			t.Errorf("unranked host %s draws %q, not an icon of the set", h, k)
		}
		if k != HostIconFor(map[string]int{"other": 0}, h) {
			t.Errorf("unranked host %s's fallback is not deterministic", h)
		}
	}
}

var hostMarkRe = regexp.MustCompile(`<span data-host-icon="([a-z]+)" role="img" aria-label="Host: ([^"]+)" title="Host: ([^"]+)" class="inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"><svg class="h-4 w-4 shrink-0"`)

// headerOf is a card's header slot as rendered.
func headerOf(t *testing.T, card string) string {
	t.Helper()
	i := strings.Index(card, `data-card-slot="header"`)
	if i < 0 {
		t.Fatalf("no header slot:\n%s", card)
	}
	return card[i:]
}

// TestEveryListedEntryDrawsItsHostsIconBesideItsName: an agent card draws the
// cluster's, a mirrored card its host's (the key it was given), each in its
// header — beside the name, inside the 16px box, the host in its tooltip and
// accessible name — and a "Recently viewed" entry draws the very same mark as
// its card.
func TestEveryListedEntryDrawsItsHostsIconBesideItsName(t *testing.T) {
	agent := AgentCardView{ID: 4, Name: "ag-icons", Status: "running"}
	tower := MirrorCardView{Host: "tower", Session: "main", Online: true, HostIcon: HostIconFor(map[string]int{"tower": 1}, "tower")}
	for _, c := range []struct {
		name, html, key, host string
	}{
		{"agent", renderString(t, agentCard(agent)), "cluster", "cluster"},
		{"mirror", renderString(t, mirrorCard(tower)), hostIconSet[1].Key, "tower"},
	} {
		m := hostMarkRe.FindStringSubmatch(headerOf(t, c.html))
		if m == nil {
			t.Errorf("%s card: no host mark in its header:\n%s", c.name, c.html)
			continue
		}
		if m[1] != c.key || m[2] != c.host || m[3] != c.host {
			t.Errorf("%s card: mark %q for host %q/%q, want %q for %q", c.name, m[1], m[2], m[3], c.key, c.host)
		}
	}

	var b bytes.Buffer
	if err := RenderAgentsList(&b, []AgentCardView{agent}, []MirrorCardView{tower},
		[]RecentRef{{AgentID: 4}, {Mirror: MirrorRef{Host: "tower", Session: "main"}}}, 0); err != nil {
		t.Fatal(err)
	}
	strip := b.String()[strings.Index(b.String(), `id="agents-recent"`):]
	marks := hostMarkRe.FindAllStringSubmatch(strip, -1)
	if len(marks) != 2 || marks[0][1] != "cluster" || marks[1][1] != hostIconSet[1].Key || marks[1][2] != "tower" {
		t.Errorf("strip entries' marks %v; want the agent's cluster mark, then tower's %q", marks, hostIconSet[1].Key)
	}
}

// TestAMirroredPageBadgeLeadsWithItsHostsIcon: the "Mirrored · <host>" badge on
// a window's own pages draws the icon its cards draw.
func TestAMirroredPageBadgeLeadsWithItsHostsIcon(t *testing.T) {
	key := hostIconSet[2].Key
	got := renderString(t, mirrorBadge("Mirrored · tower", "tower", key))
	if !strings.Contains(got, `data-host-icon="`+key+`"`) || !strings.Contains(got, hostIconSet[2].Body) {
		t.Errorf("the badge does not draw tower's icon %q:\n%s", key, got)
	}
}
