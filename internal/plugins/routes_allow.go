package plugins

import (
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

// RelayAllowlist is, per OWNER, the senders — each bound to ONE plugin — whose
// routed answers are relayed WITHOUT the owner's approval
// (MUSTER_ROUTE_RELAY_ALLOW). Anything else falls back to the ordinary pending
// draft the owner approves.
//
// 🔴 AN ENTRY NAMES THE PLUGIN AS WELL AS THE ADDRESS, AND THE PLUGIN MUST HAVE
// VERIFIED THE SENDER (Message.SenderVerified). A From line is whatever the
// sender typed; only the plugin that received the mail can know whether it was
// authenticated (DKIM/DMARC alignment, a verified signature). So an address on
// the list matches only mail THAT plugin delivered AND marked verified: the
// same address arriving through another plugin, or unverified, is never
// auto-approved (Allows).
//
// 🔴 CONFIGURATION, NOT A TABLE: who may receive a reply nobody approved is a
// widening of what muster sends on its own, made where a deploy reviews it, not
// by a click a stale or stolen browser session could make. It FAILS CLOSED:
// unset, or a sender that does not parse, relays nothing automatically.
type RelayAllowlist map[int64][]relayEntry

type relayEntry struct {
	plugin string // the plugin's name (a slug)
	// address is a lower-case ASCII mail address or "*@<domain>" (both always
	// contain "@"), or an agent-mail address "<owner>/<handle>" or "<owner>/*"
	// (never containing "@"). The two kinds are disjoint by construction, so
	// a mail entry never matches an agent-mail sender, nor the reverse.
	// A verified agent-mail sender proves only the OWNER (the signature is
	// checked against any of the owner's published keys); the handle is the
	// owner's gateway's assertion, so "<owner>/<handle>" trusts no less than
	// "<owner>/*" against a compromised key of that owner.
	address string
}

// Agent-mail addresses ("<owner>/<handle>", e.g. "hq/tasks") are the agent-mail
// protocol's own grammar, exactly as its plugin validates the signed "from":
// lower-case ASCII letters, digits and hyphens, the owner starting with a
// letter or digit, the handle with a letter. Nothing is case-folded: an address
// outside this shape (an upper-case letter, a look-alike, a space) is not an
// agent-mail address at all, so it never matches.
var (
	agentMailOwner  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)
	agentMailHandle = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
)

// isAgentMailAddress reports whether s is exactly "<owner>/<handle>".
func isAgentMailAddress(s string) bool {
	owner, handle, ok := strings.Cut(s, "/")
	return ok && agentMailOwner.MatchString(owner) && agentMailHandle.MatchString(handle)
}

// asciiAddress is the shape of an address the allowlist compares: printable
// ASCII only, and no angle bracket (isASCIIAddress) — "x<a@corp" is a From
// line that did not parse, not an address at corp. Case folding is ASCII's alone (strings.ToLower over ASCII), so no
// Unicode fold — the Kelvin sign K (U+212A) folding to "k" — can make two
// different addresses equal; a non-ASCII address never matches.
var asciiAddress = regexp.MustCompile(`^[\x21-\x7e]+@[\x21-\x7e]+$`)

func isASCIIAddress(s string) bool {
	return asciiAddress.MatchString(s) && strings.Count(s, "@") == 1 && !strings.ContainsAny(s, "<>")
}

// asciiLower is s lower-cased in ASCII only.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// ParseRelayAllowlist reads "<user id>:<plugin>:<sender>,…", where a sender is
// an ASCII address, "*@<domain>" (every address at exactly that domain), an
// agent-mail address "<owner>/<handle>", or "<owner>/*" (every handle of
// exactly that owner). Whitespace around entries is ignored; "" is an empty
// list.
func ParseRelayAllowlist(raw string) (RelayAllowlist, error) {
	out := RelayAllowlist{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("%q: want <user id>:<plugin>:<sender>", entry)
		}
		owner, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		plugin := strings.TrimSpace(parts[1])
		pat := strings.TrimSpace(parts[2])
		if err != nil || owner <= 0 {
			return nil, fmt.Errorf("%q: the user id is a positive integer", entry)
		}
		if !nameRE.MatchString(plugin) {
			return nil, fmt.Errorf("%q: %q is not a plugin name", entry, plugin)
		}
		if !strings.Contains(pat, "@") {
			// An agent-mail entry: never lower-cased, so "Ada/x" is refused
			// rather than silently read as "ada/x".
			mailOwner, handle, ok := strings.Cut(pat, "/")
			if !ok || !agentMailOwner.MatchString(mailOwner) || (handle != "*" && !agentMailHandle.MatchString(handle)) {
				return nil, fmt.Errorf("%q: the sender is an ASCII address, *@<domain>, <owner>/<handle> or <owner>/* (agent mail: lower-case letters, digits and hyphens)", entry)
			}
			out[owner] = append(out[owner], relayEntry{plugin: plugin, address: pat})
			continue
		}
		if !isASCIIAddress(pat) {
			return nil, fmt.Errorf("%q: the sender is an ASCII address or *@<domain>", entry)
		}
		pat = asciiLower(pat)
		local, domain, _ := strings.Cut(pat, "@")
		if domain == "" || strings.Contains(domain, "*") || (strings.Contains(local, "*") && local != "*") {
			return nil, fmt.Errorf("%q: the sender is an address or *@<domain>", entry)
		}
		out[owner] = append(out[owner], relayEntry{plugin: plugin, address: pat})
	}
	return out, nil
}

// senderAddress is the ASCII address in a plugin message's sender ("Ada
// <ada@x>", "ada@x"), ASCII-lower-cased, or the sender itself when it is
// exactly an agent-mail address ("owner/handle", never folded); "" when there
// is none or it is not ASCII.
func senderAddress(sender string) string {
	addr := strings.TrimSpace(sender)
	if isAgentMailAddress(addr) {
		return addr
	}
	if a, err := mail.ParseAddress(sender); err == nil {
		addr = a.Address
	}
	if !isASCIIAddress(addr) {
		return ""
	}
	return asciiLower(addr)
}

// Allows reports whether owner's routed answer to a message from sender,
// delivered by plugin pluginName with the sender verified, is relayed without
// approval. An unverified sender is never allowed, whoever it claims to be.
func (l RelayAllowlist) Allows(owner int64, pluginName, sender string, verified bool) bool {
	if !verified {
		return false
	}
	addr := senderAddress(sender)
	if addr == "" {
		return false
	}
	// The wildcard this sender would match: "*@<domain>" for mail,
	// "<owner>/*" for agent mail. An agent-mail address has no "@", and a mail
	// address is never cut at "/", so neither kind's wildcard is the other's.
	var wildcard string
	if isAgentMailAddress(addr) {
		mailOwner, _, _ := strings.Cut(addr, "/")
		wildcard = mailOwner + "/*"
	} else {
		_, domain, _ := strings.Cut(addr, "@")
		wildcard = "*@" + domain
	}
	for _, e := range l[owner] {
		if e.plugin == pluginName && (e.address == addr || e.address == wildcard) {
			return true
		}
	}
	return false
}

// Owners is how many owners have an allowlist (the boot banner says so).
func (l RelayAllowlist) Owners() int { return len(l) }
