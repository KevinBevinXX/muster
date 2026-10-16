package focus

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The BOUNDED TRANSCRIPT TAIL a refresh summarizes: the newest entries of the
// session's conversation, each capped, the whole capped. It is what is sent to
// the summarizer (operator decision: as-is), and what the fingerprint hashes.
const (
	// MaxTailEntries bounds how many entries (prompts, replies, tool calls) the
	// tail holds: the newest are kept.
	MaxTailEntries = 40
	// MaxEntryBytes bounds one entry's text.
	MaxEntryBytes = 1 << 10
	// MaxTailBytes bounds the rendered tail. It is the summarizer's whole input.
	MaxTailBytes = 16 << 10
)

// Entry is one line of a session's conversation: who (Role: "user",
// "assistant", "tool") and what.
type Entry struct {
	Role string
	Text string
}

// RenderTail is the tail of entries as the summarizer reads it — the newest
// that fit, oldest first, one "role: text" per line — and its fingerprint (hex
// sha256 of exactly that text). Empty entries are skipped; no entries is "".
func RenderTail(entries []Entry) (string, string) {
	var lines []string
	size := 0
	for i := len(entries) - 1; i >= 0 && len(lines) < MaxTailEntries; i-- {
		text := oneLine(entries[i].Text)
		if text == "" {
			continue
		}
		if len(text) > MaxEntryBytes {
			text = strings.ToValidUTF8(text[:MaxEntryBytes], "") + "…"
		}
		line := oneLine(entries[i].Role) + ": " + text
		if size+len(line)+1 > MaxTailBytes {
			break
		}
		size += len(line) + 1
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", ""
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	tail := strings.Join(lines, "\n")
	return tail, Fingerprint(tail)
}

// Fingerprint is the hex sha256 of a rendered tail.
func Fingerprint(tail string) string {
	sum := sha256.Sum256([]byte(tail))
	return hex.EncodeToString(sum[:])
}
