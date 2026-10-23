package ui

import (
	"fmt"
	"regexp"
	"strings"
)

// HOST PREFERENCES: what the operator calls their own hosts, which this code
// cannot know. Two settings, both empty by default:
//
//   - Order: the hosts whose tabs come FIRST on the Agents list, after "All",
//     in this order. Every other mirrored host follows, A–Z by its label; the
//     cluster (dispatched pod agents, podHost) is always LAST.
//   - Icons: a host's icon BY ITS NAME ("laptop" draws the laptop glyph),
//     whatever its registration rank. No host picked by rank draws a glyph
//     named here, so a named glyph means that host.
//
// They are configuration (MUSTER_HOST_ORDER, MUSTER_HOST_ICONS, read in
// cmd/muster-server) rather than a list in this file because host names are
// the operator's own, and a deployment's real ones must not be written into
// this repository.
type HostPreferences struct {
	Order []string
	Icons map[string]string
}

// hostPrefs is the installed HostPreferences, normalised (lower-cased names).
// Set once at start-up (SetHostPreferences), before any page is rendered.
var hostPrefs HostPreferences

// SetHostPreferences installs p (Normalized). A refused p leaves the
// installed preferences as they were.
func SetHostPreferences(p HostPreferences) error {
	n, err := p.Normalized()
	if err != nil {
		return err
	}
	hostPrefs = n
	return nil
}

// Normalized is p checked, with its host names lower-cased and trimmed: they
// are matched case-insensitively (the list's host field is lower-cased). It
// refuses:
//   - an empty or repeated host, or the cluster's own name (it is always last,
//     and draws clusterIcon);
//   - a name that is not a host name (hostNamePattern: no space, no separator);
//   - an icon that is not one of hostIconSet's keys, or one named for two hosts
//     (it would no longer say which host it is).
func (p HostPreferences) Normalized() (HostPreferences, error) {
	norm := HostPreferences{Icons: map[string]string{}}
	seen := map[string]bool{}
	for _, h := range p.Order {
		h = strings.ToLower(strings.TrimSpace(h))
		if err := checkPrefHost(h); err != nil {
			return HostPreferences{}, fmt.Errorf("host order: %w", err)
		}
		if seen[h] {
			return HostPreferences{}, fmt.Errorf("host order: %q is listed twice", h)
		}
		seen[h] = true
		norm.Order = append(norm.Order, h)
	}
	inSet := map[string]bool{}
	for _, d := range hostIconSet {
		inSet[d.Key] = true
	}
	taken := map[string]string{}
	for h, k := range p.Icons {
		h = strings.ToLower(strings.TrimSpace(h))
		if err := checkPrefHost(h); err != nil {
			return HostPreferences{}, fmt.Errorf("host icons: %w", err)
		}
		if _, dup := norm.Icons[h]; dup {
			return HostPreferences{}, fmt.Errorf("host icons: %q is given two icons", h)
		}
		if !inSet[k] {
			return HostPreferences{}, fmt.Errorf("host icons: %q is not an icon (one of %s)", k, strings.Join(hostIconKeys(), ", "))
		}
		if o, dup := taken[k]; dup {
			return HostPreferences{}, fmt.Errorf("host icons: %q is given to both %q and %q", k, o, h)
		}
		taken[k] = h
		norm.Icons[h] = k
	}
	return norm, nil
}

// checkPrefHost is why h cannot be named in a preference, nil if it can be.
func checkPrefHost(h string) error {
	if h == podHost {
		return fmt.Errorf("%q is the dispatched agents' host: its tab is always last and its icon its own", podHost)
	}
	if !hostNamePattern.MatchString(h) {
		return fmt.Errorf("%q is not a host name (lower-case letters, digits and inner hyphens, at most 63)", h)
	}
	return nil
}

// hostNamePattern is what a configured host name must be: a DNS label. It
// refuses what a mistyped list carries — a space, a ';' or ':' separator, an
// empty entry — rather than install a name no host has.
var hostNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// hostIconKeys is hostIconSet's keys, in order.
func hostIconKeys() []string {
	out := make([]string, len(hostIconSet))
	for i, d := range hostIconSet {
		out[i] = d.Key
	}
	return out
}
