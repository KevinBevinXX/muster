package flows

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// WHAT AN AGENT WRITES AND WHAT ITS OWNER APPROVES MUST BE THE SAME THING.
//
// 🔴 THE CONTROL IS THE OWNER'S APPROVAL OF THE EXACT VISIBLE TEXT. Everything in
// this file serves that: (1) an agent-composed value may not carry a character
// the owner cannot SEE — measured before this existed: "hello" followed by 29
// tag-block characters was approved from a card reading `text: hello`, and the
// job carried all 34 runes; (2) the approval card renders every value through
// VisibleEscape, where anything outside printable ASCII is spelled out, with
// the length in runes beside it. The deny patterns in requests.go are a
// HEURISTIC on top (a prompt-injected agent can paraphrase past any pattern) and
// must never be described as the gate.

// NormalizeAgentText is the form an agent-composed value is screened, stored,
// shown and sent in: NFKC, so compatibility look-alikes (full-width letters,
// ligatures, the many Unicode spaces) are folded to the plain characters the
// ASCII deny patterns and the owner's eyes both read.
func NormalizeAgentText(v string) string { return norm.NFKC.String(v) }

// hiddenRune reports whether r is a character an approval card cannot show: a
// control, format (zero-width, bidi override, tag block), private-use,
// surrogate or unassigned code point, a default-ignorable one or a variation
// selector, a line/paragraph separator, or a space other than U+0020. Newline
// and tab are the two controls free text may carry (the card spells them out).
func hiddenRune(r rune) bool {
	if r == '\n' || r == '\t' || r == ' ' {
		return false
	}
	if !unicode.IsGraphic(r) { // Cc, Cf, Co, Cs, Cn, Zl, Zp
		return true
	}
	return unicode.Is(unicode.Zs, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

// HiddenCharRefusal reports why v is refused for carrying a character its
// approver could not see, or "". v is expected to be NFKC already.
func HiddenCharRefusal(v string) string {
	for i, r := range v {
		if hiddenRune(r) {
			return fmt.Sprintf("it carries an invisible or non-printing character (U+%04X at byte %d); "+
				"an agent's request may contain only characters its owner can see", r, i)
		}
	}
	return ""
}

// VisibleEscape renders v so that NOTHING in it is left to the eye's guess:
// printable ASCII is itself (a backslash is doubled), newline and tab are the
// two-character escapes \n and \t, and every other code point — every
// non-ASCII letter included, since a look-alike letter reads as the ASCII one
// — is \u{HEX}. The mapping is one-to-one, so the card can be decoded back to
// the exact bytes (UnescapeVisible); the approval test does exactly that.
func VisibleEscape(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\u{%x}`, r)
		}
	}
	return b.String()
}

// UnescapeVisible inverts VisibleEscape. ok is false on anything VisibleEscape
// cannot have produced.
func UnescapeVisible(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			if c < 0x20 || c > 0x7e {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", false
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'u':
			end := strings.IndexByte(s[i:], '}')
			if end < 3 || s[i+1] != '{' {
				return "", false
			}
			var r rune
			if _, err := fmt.Sscanf(s[i+2:i+end], "%x", &r); err != nil {
				return "", false
			}
			b.WriteRune(r)
			i += end
		default:
			return "", false
		}
	}
	return b.String(), true
}
