package plugins

import (
	"fmt"
	"net/mail"
	"strconv"
	"strings"
)

// RelayAllowlist is, per OWNER, the senders whose routed answers are relayed
// WITHOUT the owner's approval (MUSTER_ROUTE_RELAY_ALLOW). A sender not on its
// owner's list falls back to the ordinary pending draft the owner approves.
//
// 🔴 CONFIGURATION, NOT A TABLE, AND THAT IS A DECISION. Who may receive a reply
// nobody approved is a widening of what muster sends on its own, so it is made
// where a deploy reviews it — not by a click a stale or stolen browser session
// could make — and it needs no page, route or migration. It FAILS CLOSED: unset,
// or a sender that does not parse, relays nothing automatically.
type RelayAllowlist map[int64][]string

// ParseRelayAllowlist reads "<user id>:<sender>,…", where a sender is an address
// (matched case-insensitively) or "*@<domain>" (every address at exactly that
// domain). Whitespace around entries is ignored; "" is an empty list.
func ParseRelayAllowlist(raw string) (RelayAllowlist, error) {
	out := RelayAllowlist{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		uid, pat, ok := strings.Cut(entry, ":")
		owner, err := strconv.ParseInt(strings.TrimSpace(uid), 10, 64)
		pat = strings.ToLower(strings.TrimSpace(pat))
		if !ok || err != nil || owner <= 0 {
			return nil, fmt.Errorf("%q: want <user id>:<sender>", entry)
		}
		local, domain, at := strings.Cut(pat, "@")
		if !at || local == "" || domain == "" || strings.ContainsAny(domain, "@*") || (strings.Contains(local, "*") && local != "*") {
			return nil, fmt.Errorf("%q: the sender is an address or *@<domain>", entry)
		}
		out[owner] = append(out[owner], pat)
	}
	return out, nil
}

// senderAddress is the address in a plugin message's sender ("Ada <ada@x>",
// "ada@x"), lower-cased; "" when there is none.
func senderAddress(sender string) string {
	if a, err := mail.ParseAddress(sender); err == nil {
		return strings.ToLower(a.Address)
	}
	s := strings.ToLower(strings.TrimSpace(sender))
	if strings.Count(s, "@") == 1 && !strings.ContainsAny(s, " <>") {
		return s
	}
	return ""
}

// Allows reports whether owner's routed answers to sender are relayed without
// approval.
func (l RelayAllowlist) Allows(owner int64, sender string) bool {
	addr := senderAddress(sender)
	if addr == "" {
		return false
	}
	_, domain, _ := strings.Cut(addr, "@")
	for _, pat := range l[owner] {
		if pat == addr || pat == "*@"+domain {
			return true
		}
	}
	return false
}
