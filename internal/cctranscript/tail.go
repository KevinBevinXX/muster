package cctranscript

import (
	"bytes"
	"errors"
	"io"
)

// Reading a LIVE transcript: the CLI is appending to it while it is read.
//
// 🔴 AN OFFSET IS ALWAYS A LINE BOUNDARY, AND A TRAILING LINE WITH NO NEWLINE
// IS NEVER CONSUMED. It is a record the CLI is still writing: parsing half of
// it would fail or, worse, succeed on a prefix. It is left in the file and read
// whole on the next poll — which is also what makes an offset a valid RESUME
// point: every byte before it was a complete line, decoded exactly once.

// Line is the events of one complete transcript line, and the offset just past
// its newline: a resume point that comes AFTER every one of its events.
type Line struct {
	Events []Event
	End    int64
}

// ReadLines decodes the complete lines in r between off and size, reading at
// most maxBytes, and returns those that produced events (in order) and the
// offset after the last line it consumed. A partial last line is left for the
// next call (the offset stops in front of it).
//
// A single line longer than maxBytes is SKIPPED — consumed with no events — once
// its newline exists: no window could hold it, and stalling on it would stop the
// view for good. (Claude Code writes multi-MiB tool results on one line.)
func ReadLines(r io.ReaderAt, off, size int64, maxBytes int) ([]Line, int64, error) {
	if off >= size {
		return nil, off, nil
	}
	n := size - off
	if n > int64(maxBytes) {
		n = int64(maxBytes)
	}
	buf := make([]byte, n)
	got, err := r.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, off, err
	}
	buf = buf[:got]
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		if int64(len(buf)) < int64(maxBytes) {
			return nil, off, nil // a partial line still being written
		}
		// One line longer than the whole window: skip to its end, if it has one.
		end, err := nextNewline(r, off+int64(len(buf)), size)
		if err != nil || end < 0 {
			return nil, off, err
		}
		return nil, end + 1, nil
	}
	var out []Line
	pos := off
	for _, line := range bytes.Split(buf[:last], []byte{'\n'}) {
		pos += int64(len(line)) + 1
		if rec, ok := DecodeLine(line); ok {
			if evs := Events(rec); len(evs) > 0 {
				out = append(out, Line{Events: evs, End: pos})
			}
		}
	}
	return out, off + int64(last) + 1, nil
}

// ReadEvents is ReadLines' events, flattened.
func ReadEvents(r io.ReaderAt, off, size int64, maxBytes int) ([]Event, int64, error) {
	lines, next, err := ReadLines(r, off, size, maxBytes)
	return Flatten(lines), next, err
}

// Flatten is lines' events in order.
func Flatten(lines []Line) []Event {
	var out []Event
	for _, l := range lines {
		out = append(out, l.Events...)
	}
	return out
}

// nextNewline is the offset of the first '\n' at or after from (before size),
// or -1.
func nextNewline(r io.ReaderAt, from, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for from < size {
		n := int64(len(buf))
		if size-from < n {
			n = size - from
		}
		got, err := r.ReadAt(buf[:n], from)
		if i := bytes.IndexByte(buf[:got], '\n'); i >= 0 {
			return from + int64(i), nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
		if got == 0 {
			return -1, nil
		}
		from += int64(got)
	}
	return -1, nil
}

// Backfill is the tail of a transcript a fresh viewer starts with: the lines
// (with their events) in the LAST window bytes of the file, holding at most
// maxEvents events (the newest), and the offset to continue from.
//
// 🔴 IT NEVER SENDS THE FILE WHOLESALE. A long session's transcript runs to
// tens of MiB; what a viewer opening it needs is the recent conversation. The
// window starts at the first line boundary inside it, so the first record read
// is a whole one. A tool result whose call fell before the window arrives
// unpaired, and the view draws it as its own row.
func Backfill(r io.ReaderAt, size int64, window, maxEvents int) ([]Line, int64, error) {
	start := size - int64(window)
	if start < 0 {
		start = 0
	}
	if start > 0 {
		// Unless the byte before start ends a line, start is mid-line: skip it.
		prev := make([]byte, 1)
		if _, err := r.ReadAt(prev, start-1); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		if prev[0] != '\n' {
			nl, err := nextNewline(r, start, size)
			if err != nil {
				return nil, 0, err
			}
			if nl < 0 {
				return nil, start, nil
			}
			start = nl + 1
		}
	}
	var out []Line
	off := start
	for off < size {
		lines, next, err := ReadLines(r, off, size, window)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, lines...)
		if next == off {
			break
		}
		off = next
	}
	// Keep the newest maxEvents events: whole lines from the end, then the tail
	// of the line that straddles the cap.
	n := 0
	for i := len(out) - 1; i >= 0; i-- {
		if n+len(out[i].Events) > maxEvents {
			keep := maxEvents - n
			if keep > 0 {
				l := out[i]
				l.Events = l.Events[len(l.Events)-keep:]
				return append([]Line{l}, out[i+1:]...), off, nil
			}
			return out[i+1:], off, nil
		}
		n += len(out[i].Events)
	}
	return out, off, nil
}
