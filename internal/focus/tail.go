package focus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// The BOUNDED TRANSCRIPT TAIL a refresh summarizes: the newest entries of the
// session's conversation, each capped, the whole capped. It is what is sent to
// the summarizer and what the fingerprint hashes: prompts and replies as they
// are (operator decision), a tool call as its name and file path only
// (ToolLine) — never its command, content or other arguments.
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

// toolPathKeys are the only tool arguments a tail entry may name: the FILE a
// tool acts on. Never a command (Bash's is a shell line that can carry a
// credential), never content (Write's body), never a URL, pattern or query.
var toolPathKeys = []string{"file_path", "notebook_path", "path"}

// MaxToolPath bounds the path a tool entry names.
const MaxToolPath = 200

// ToolLine is one tool call as the summarizer sees it, for EITHER kind of
// session: the tool's name, and — when its input names a file — that path.
// input is the call's argument JSON (an agent's stored args, a transcript
// event's capped Input); anything else in it never leaves.
func ToolLine(tool, input string) string {
	tool = oneLine(tool)
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(input)), &m) != nil {
		return tool
	}
	for _, k := range toolPathKeys {
		var v string
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &v) == nil {
			if v = oneLine(v); v != "" {
				if len(v) > MaxToolPath {
					v = strings.ToValidUTF8(v[:MaxToolPath], "") + "…"
				}
				return strings.TrimSpace(tool + " " + v)
			}
		}
	}
	return tool
}
