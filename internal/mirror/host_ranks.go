package mirror

import (
	"context"
	"sort"
)

// HOST RANKS: each of an owner's hosts' place in the order the host was first
// registered — its first mirror token minted — 0 for the first. The Agents
// list picks a host's icon by it (internal/ui's HostIconFor), so the first N
// hosts can never share an icon and a host's icon never changes.
//
// 🔴 WHY IT IS STABLE: a host's rank is decided by its LOWEST token id, and a
// token is never deleted — RevokeToken stamps revoked_at and keeps the row (the
// Store has no delete). So:
//   - a host registered later gets a higher first id, hence the next rank, and
//     no existing host's rank moves;
//   - minting a new token for an existing host (a rotation) adds a HIGHER id
//     for it, which is not its lowest, so its rank does not move;
//   - revoking keeps the row, so a host whose tokens are all revoked keeps its
//     rank and no later host inherits it.
//
// If tokens ever become deletable this property breaks, and that change must
// carry its own answer for it (TestHostRanksAreDistinctAndStableAsHostsAreAdded).

// RanksByRegistration ranks the hosts tokens were minted for by each host's
// lowest token id, 0 first. tokens may come in any order and may hold several
// per host (rotations) and revoked ones; all count.
func RanksByRegistration(tokens []Token) map[string]int {
	first := map[string]int64{}
	for _, t := range tokens {
		if id, ok := first[t.Host]; !ok || t.ID < id {
			first[t.Host] = t.ID
		}
	}
	hosts := make([]string, 0, len(first))
	for h := range first {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if first[hosts[i]] != first[hosts[j]] {
			return first[hosts[i]] < first[hosts[j]]
		}
		return hosts[i] < hosts[j]
	})
	ranks := make(map[string]int, len(hosts))
	for i, h := range hosts {
		ranks[h] = i
	}
	return ranks
}

// HostRanks is the viewing principal's hosts' registration ranks
// (RanksByRegistration over its own tokens — mirror_tokens is under row-level
// security, so ListTokens runs as the viewer and reads only theirs). Read on
// every call, uncached, like the rest of the Agents list's per-render reads. A
// failed read is logged and answers nil: the caller's fallback (ui.HostIconFor's
// hash) still draws an icon.
func (s *Service) HostRanks(ctx context.Context) map[string]int {
	if _, err := owner(ctx); err != nil {
		return nil
	}
	toks, err := s.store.ListTokens(ctx)
	if err != nil {
		s.logger.Printf("mirror: host ranks: %v", err)
		return nil
	}
	return RanksByRegistration(toks)
}
