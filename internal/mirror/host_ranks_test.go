package mirror_test

import (
	"context"
	"io"
	"log"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// tok is a token record for the rank tests: an id and a host (and, revoked,
// a revocation time). Every host name is invented.
func tok(id int64, host string, revoked bool) mirror.Token {
	t := mirror.Token{ID: id, OwnerID: 1, Host: host}
	if revoked {
		at := time.Unix(946684800, 0) // 2000-01-01
		t.RevokedAt = &at
	}
	return t
}

// TestRanksByRegistrationOrdersHostsByTheirFirstToken: a host's rank is its
// place by its LOWEST token id, 0 first — whatever order the tokens are read
// in. Three hosts, ids chosen so that the order by any token other than the
// first (and the order by name) differs from the right answer.
func TestRanksByRegistrationOrdersHostsByTheirFirstToken(t *testing.T) {
	toks := []mirror.Token{
		tok(3, "zeta", false), tok(5, "alpha", false), tok(9, "mid", false),
		tok(11, "zeta", false), // zeta's rotation: a later id, still first registered
		tok(2, "zeta", true),   // …and its revoked first token: the LOWEST id of all
	}
	want := map[string]int{"zeta": 0, "alpha": 1, "mid": 2}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		rnd.Shuffle(len(toks), func(a, b int) { toks[a], toks[b] = toks[b], toks[a] })
		if got := mirror.RanksByRegistration(toks); !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d: ranks %v, want %v", i, got, want)
		}
	}
}

// TestHostRanksAreDistinctAndStableAsHostsAreAdded: N hosts get the N ranks
// 0..N-1, one each; and minting each further host — a new host, a rotation of
// an existing one, a revocation — never moves an existing host's rank.
func TestHostRanksAreDistinctAndStableAsHostsAreAdded(t *testing.T) {
	var toks []mirror.Token
	var prev map[string]int
	id := int64(100)
	for n := 1; n <= 20; n++ {
		id += 7
		host := "h" + string(rune('a'+n-1))
		toks = append(toks, tok(id, host, false))
		// Noise that must not move anyone: a rotation and a revocation of an
		// EARLIER host, minted after the new one.
		if n > 1 {
			id++
			toks = append(toks, tok(id, "ha", n%2 == 0))
		}
		got := mirror.RanksByRegistration(toks)
		if len(got) != n {
			t.Fatalf("%d hosts, %d ranked: %v", n, len(got), got)
		}
		seen := map[int]string{}
		for h, r := range got {
			if r < 0 || r >= n {
				t.Fatalf("host %s rank %d out of 0..%d", h, r, n-1)
			}
			if o, dup := seen[r]; dup {
				t.Fatalf("hosts %s and %s share rank %d", h, o, r)
			}
			seen[r] = h
		}
		for h, r := range prev {
			if got[h] != r {
				t.Fatalf("adding host %s moved %s from rank %d to %d", host, h, r, got[h])
			}
		}
		if got[host] != n-1 {
			t.Fatalf("the newest host %s ranked %d, want %d (last)", host, got[host], n-1)
		}
		prev = got
	}
}

// TestServiceHostRanksAreTheViewers: Service.HostRanks ranks the VIEWER's
// hosts only — another owner's token minted between theirs does not take a
// rank from them — a host minted since is ranked on the next call, and no
// principal ranks nothing.
func TestServiceHostRanksAreTheViewers(t *testing.T) {
	st := newMemStore()
	svc := mirror.NewService(st, log.New(io.Discard, "", 0))
	st.mint(t, 1, "first")
	st.mint(t, 2, "theirs")
	st.mint(t, 1, "second")

	if got, want := svc.HostRanks(asUser(1)), map[string]int{"first": 0, "second": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("user 1's ranks %v, want %v", got, want)
	}
	if got := svc.HostRanks(asUser(2)); !reflect.DeepEqual(got, map[string]int{"theirs": 0}) {
		t.Fatalf("user 2's ranks %v, want only its own host at 0", got)
	}
	st.mint(t, 1, "third")
	if got, want := svc.HostRanks(asUser(1)), map[string]int{"first": 0, "second": 1, "third": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after a third host: %v, want %v", got, want)
	}
	if got := svc.HostRanks(context.Background()); got != nil {
		t.Fatalf("no principal ranked %v; a host's ranks are its owner's", got)
	}
}
