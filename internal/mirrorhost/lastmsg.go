package mirrorhost

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// WHEN A CLAUDE WINDOW LAST HAD A MESSAGE (mirror.Window.LastMessage): the
// Agents list's "active … ago", its order and its Idle section for a Claude
// window, in place of tmux's #{window_activity}, which a tmux restart resets
// for every window (see the wire field).
//
// THE LAST MESSAGE is the newest `timestamp` of a transcript row whose type is
// "user" or "assistant", that is not isMeta, and that is not an assistant row
// Claude Code wrote ITSELF (message.model "<synthetic>"). Everything else is
// left out — and it has to be, because RESUMING a session writes to its
// transcript without anyone saying anything. Measured read-only on real
// transcripts on one host: a resume appends `attachment` rows and
// `system`/`informational` rows carrying the RESUME's time, plus untimestamped
// bookkeeping rows (cost-state, bridge-session, last-prompt,
// history-suppression, atis-latch, mode, permission-mode, ai-title); and a
// resume that is continued writes an isMeta user row ("Continue from where you
// left off") followed by a SYNTHETIC assistant row ("No response requested.",
// model "<synthetic>") — 14 such rows, 11 right after a meta user row. (The
// first sample of 45 resumed transcripts had none, and this comment said a
// resume appends no user or assistant row; review found one that read 18 days
// late.) The other synthetic rows (a usage limit hit, "Not logged in", an API
// error: 229 of 243) answer a human prompt, which already counts. The user rows
// that do follow a reply are things a person did (a prompt, an interrupt,
// /compact's summary, a background task's notification arriving), and count.
//
// 🔴 CHEAP: the transcript is read BACKWARDS from its end in lastMsgChunk
// pieces, stopping at the first qualifying row, at most lastMsgMaxScan bytes;
// and a transcript is re-read only when its size or modification time moved
// (lastMessageReader's cache) — a listing every few seconds over sixty-odd
// idle windows reads nothing.

const (
	lastMsgChunk   = 64 << 10
	lastMsgMaxScan = 8 << 20
)

// lastMsgRow is the fields of a transcript row the rule reads.
type lastMsgRow struct {
	Type      string `json:"type"`
	IsMeta    bool   `json:"isMeta"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
	} `json:"message"`
}

// syntheticModel is the model Claude Code names on an assistant row it wrote
// itself rather than received from a model.
const syntheticModel = "<synthetic>"

// lastMessageOf is the time of the last message in the transcript r of the
// given size (see the file header), and false when none is found in the last
// lastMsgMaxScan bytes or r cannot be read.
func lastMessageOf(r io.ReaderAt, size int64) (time.Time, bool) {
	var carry []byte // the start of a line cut by the previous (later) chunk
	end := size
	for end > 0 && size-end < lastMsgMaxScan {
		start := end - lastMsgChunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := r.ReadAt(buf, start); err != nil && err != io.EOF {
			return time.Time{}, false
		}
		buf = append(buf, carry...)
		// Lines whole in buf, newest first. The first piece is a partial line
		// unless the chunk began the file: it is carried into the next read.
		lines := bytes.Split(buf, []byte{'\n'})
		first := 0
		if start > 0 {
			carry = append([]byte(nil), lines[0]...)
			first = 1
		} else {
			carry = nil
		}
		for i := len(lines) - 1; i >= first; i-- {
			if t, ok := lastMsgLine(lines[i]); ok {
				return t, true
			}
		}
		end = start
	}
	return time.Time{}, false
}

// lastMsgLine is the time of one transcript line when it is a message.
func lastMsgLine(line []byte) (time.Time, bool) {
	line = bytes.TrimSpace(line)
	// A cheap filter before the decode: only these rows can count.
	if len(line) == 0 || (!bytes.Contains(line, []byte(`"assistant"`)) && !bytes.Contains(line, []byte(`"user"`))) {
		return time.Time{}, false
	}
	var row lastMsgRow
	if json.Unmarshal(line, &row) != nil || (row.Type != "user" && row.Type != "assistant") || row.IsMeta || row.Timestamp == "" ||
		row.Message.Model == syntheticModel {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// lastMessageReader is lastMessageOf for transcripts, re-reading one only when
// its size or modification time moved. Safe for concurrent use; the zero value
// is ready.
type lastMessageReader struct {
	mu    sync.Mutex
	cache map[string]lastMsgEntry
	// reads counts the transcripts actually read (tests: the cache).
	reads int
}

type lastMsgEntry struct {
	size int64
	mod  time.Time
	at   int64
}

// of is the last message's Unix time for the transcript at path; 0 when there
// is none or it cannot be read.
func (r *lastMessageReader) of(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	r.mu.Lock()
	if e, ok := r.cache[path]; ok && e.size == st.Size() && e.mod.Equal(st.ModTime()) {
		r.mu.Unlock()
		return e.at
	}
	r.reads++
	r.mu.Unlock()
	var at int64
	if t, ok := lastMessageOf(f, st.Size()); ok && t.Unix() > 0 {
		at = t.Unix()
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]lastMsgEntry{}
	}
	r.cache[path] = lastMsgEntry{size: st.Size(), mod: st.ModTime(), at: at}
	r.mu.Unlock()
	return at
}

// attach sets each Claude window's LastMessage from its transcript (a copy of
// its ClaudePane: the backend's value is not changed), and drops the cache of
// every transcript no window names any more. A pane that already carries one
// keeps it.
func (r *lastMessageReader) attach(windows []WindowInfo) {
	seen := map[string]bool{}
	for i := range windows {
		c := windows[i].Claude
		if c == nil || c.Transcript == "" || c.LastMessage != 0 {
			continue
		}
		seen[c.Transcript] = true
		cp := *c
		cp.LastMessage = r.of(c.Transcript)
		windows[i].Claude = &cp
	}
	r.mu.Lock()
	for p := range r.cache {
		if !seen[p] {
			delete(r.cache, p)
		}
	}
	r.mu.Unlock()
}
