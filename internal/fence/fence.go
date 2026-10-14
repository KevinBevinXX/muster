// Package fence renders UNTRUSTED TEXT as a fenced block an agent is handed as a
// turn: a preface line written by muster, then an opening marker at column 0
// carrying a fresh nonce, the content lines, and a closing marker — with EVERY
// line between the markers behind [ContentPrefix].
//
// 🔴 THE PREFIX IS WHAT MAKES THE FENCE STRUCTURAL. No content text can reach
// column 0, where the markers live, so no text can close the block early or open
// a forged one; the marker word and the block's own nonce are additionally
// replaced inside the fence ([MarkerPlaceholder], [NoncePlaceholder]), so a
// line cannot even QUOTE the real closing marker.
//
// 🔴 [Sanitize] IS THE CALLER'S JOB, PER FIELD, BEFORE A LINE IS BUILT. A field
// holding a line break would start a line with no content prefix; [Sanitize]
// folds every line break into " ⏎ " and replaces every control, format,
// separator and unassigned code point, so no output of it can. The block itself
// does not sanitise lines a second time: a caller-composed line (a standing
// sentence, a stanza header) is muster's own text.
//
// This package was extracted from internal/memorelay, whose delivered turn must
// stay byte-identical (internal/memorelay's TestTheRenderedTurnsAreByteIdenticalToTheGolden).
// internal/plugins renders inbound plugin messages through it as well.
package fence

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ContentPrefix begins every line between the markers.
	ContentPrefix = "| "
	// MarkerPlaceholder replaces the block's marker word wherever a content line
	// carries it (any case).
	MarkerPlaceholder = "[fence-marker]"
	// NoncePlaceholder replaces the block's nonce wherever a content line carries
	// it (any case).
	NoncePlaceholder = "[nonce]"
)

// Block is one fenced turn. Marker is the word the opening and closing markers
// carry (e.g. "cairn-memo"); it should be a plain lower-case word, since it is
// matched case-insensitively inside the content.
type Block struct {
	// Preface is the one line OUTSIDE the fence, muster's own words: what the
	// turn is and that it asks for nothing. It is written verbatim.
	Preface string
	Marker  string
	// Count is the number of items the block carries, stated in the opening
	// marker.
	Count int
	// Lines are the content lines, each written behind ContentPrefix. An empty
	// string is a blank content line (the prefix alone).
	Lines []string
}

// NewNonce is a fresh 16-hex-character nonce, drawn per render, so text that
// guesses an earlier block's nonce still sits behind the content prefix of this
// one.
func NewNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+).
	return hex.EncodeToString(b[:])
}

// Render is the whole turn: the preface, a blank line, then the fenced block.
func (b Block) Render(nonce string) string {
	markerRE := regexp.MustCompile("(?i)" + regexp.QuoteMeta(b.Marker))
	nonceRE := regexp.MustCompile("(?i)" + regexp.QuoteMeta(nonce))
	var s strings.Builder
	s.WriteString(b.Preface + "\n\n")
	s.WriteString("<<<" + b.Marker + " untrusted nonce=" + nonce + " count=" + strconv.Itoa(b.Count) + ">>>\n")
	for _, line := range b.Lines {
		line = markerRE.ReplaceAllLiteralString(line, MarkerPlaceholder)
		line = nonceRE.ReplaceAllLiteralString(line, NoncePlaceholder)
		s.WriteString(ContentPrefix + line + "\n")
	}
	s.WriteString("<<<end " + b.Marker + " nonce=" + nonce + ">>>\n")
	return s.String()
}

// Cut is the first n RUNES of s, with "…" when anything was dropped.
func Cut(s string, n int) string {
	seen := 0
	for i := range s {
		if seen == n {
			return s[:i] + "…"
		}
		seen++
	}
	return s
}

// Unsafe reports whether [Sanitize] must replace r: every code point in General
// Category C* (Cc, Cf, Co, Cs, Cn), plus Zl, Zp, the variation selectors and the
// other default-ignorable code points (invisible letters and marks). "In none of
// L, M, N, P, S, Z" is exactly C*, unassigned code points included.
func Unsafe(r rune) bool {
	switch {
	case !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z):
		return true
	case unicode.In(r, unicode.Zl, unicode.Zp):
		return true
	case unicode.Is(unicode.Variation_Selector, r):
		return true
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
		return true
	}
	return false
}

// Sanitize is the replacement every untrusted field passes through before it is
// written into a turn: each [Unsafe] code point becomes U+FFFD, except a line
// break (" ⏎ ", CRLF folded first) and a tab (one space). Invalid UTF-8 decodes
// to U+FFFD per invalid byte. Sanitising already-sanitised text is a no-op.
//
// 🔴 IT IS ALSO WHAT KEEPS A FIELD ON ONE LINE AND OUT OF A RUNTIME'S REFUSAL.
// A line break inside a field would start a line with no content prefix, and the
// claude-code runtime refuses a prompt holding any control character other than
// newline and tab (a 400, nothing pasted). No output of this function holds either.
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(" ⏎ ")
		case r == '\t':
			b.WriteByte(' ')
		case Unsafe(r):
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
