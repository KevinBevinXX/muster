package memorelay

import (
	"fmt"
	"sort"
	"strings"
)

// ScopeMap is the deployment's DETERMINISTIC mapping from a repository's derived scope (what
// [ScopeForRepo] answers) to the SET of cairn scopes an agent working on that repository reads.
// A repository absent from the map reads exactly its derived scope.
//
// 🔴 IT EXISTS BECAUSE cairn NAMES A SCOPE AFTER A CLONE'S DIRECTORY, NOT ITS REPOSITORY. A
// repository cloned under another directory name has its memos filed under that name by host
// sessions, while the relay derives the repository's own name — and when BOTH scopes exist the
// mismatch is silent (the listener answers 200 for the one nobody writes to). The map names, per
// repository, every scope its memos are filed under. It is configuration and not a rule, because
// which directory a clone lives in is a fact about a deployment's hosts, not about the repository.
//
// ⚠ AN ENTRY REPLACES THE DEFAULT, IT DOES NOT ADD TO IT: a repository mapped to {b} reads b and
// NOT its own name. Name the derived scope in the set to keep it.
type ScopeMap map[string][]string

// ParseScopeMap reads `repo=scope,scope;repo=scope`. Every name must already be in the
// normalised form [ScopeForRepo] produces (so a key can only ever match by equality), every
// entry needs at least one scope, and a repository may appear once. Whitespace around names is
// ignored; an empty spec is an empty map.
func ParseScopeMap(spec string) (ScopeMap, error) {
	out := ScopeMap{}
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		repo, list, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("memorelay: scope map entry %q has no `=`; want repo=scope[,scope…]", entry)
		}
		repo = strings.TrimSpace(repo)
		if err := normalName("repository", repo); err != nil {
			return nil, err
		}
		if _, dup := out[repo]; dup {
			return nil, fmt.Errorf("memorelay: scope map names repository %q twice", repo)
		}
		seen := map[string]bool{}
		var scopes []string
		for _, s := range strings.Split(list, ",") {
			s = strings.TrimSpace(s)
			if err := normalName("scope", s); err != nil {
				return nil, fmt.Errorf("%w (in the entry for %q)", err, repo)
			}
			if !seen[s] {
				seen[s] = true
				scopes = append(scopes, s)
			}
		}
		sort.Strings(scopes)
		out[repo] = scopes
	}
	return out, nil
}

func normalName(what, name string) error {
	if name == "" || ScopeForRepo(name) != name {
		return fmt.Errorf("memorelay: scope map %s %q is not a normalised scope name (lower case, "+
			"[a-z0-9.-], no leading or trailing `-`)", what, name)
	}
	return nil
}

// scopesFor is the set of scopes an agent working on repo reads: the mapped set when the map
// names repo's derived scope, else that scope alone, else nothing.
func (m ScopeMap) scopesFor(repo string) []string {
	s := ScopeForRepo(repo)
	if s == "" {
		return nil
	}
	if mapped, ok := m[s]; ok {
		return mapped
	}
	return []string{s}
}

// String renders the map in the form ParseScopeMap reads, keys sorted — for the boot banner.
func (m ScopeMap) String() string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strings.Join(m[k], ",")
	}
	return strings.Join(parts, ";")
}
