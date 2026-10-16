package cctranscript

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// THE PROMPT INDEX: every prompt in a WHOLE transcript, by its position and
// its first line — what the message index lists, including the prompts a
// viewer's backfill (the file's last window) never loaded.

// PromptRef is one prompt in a transcript.
type PromptRef struct {
	// Line is the prompt's Event.Line: the byte offset just past its record.
	Line int64 `json:"line"`
	// Text is the prompt's first non-blank line, capped at MaxPromptRefText.
	Text string `json:"text"`
}

// MaxPromptRefText bounds a PromptRef's Text, in bytes.
const MaxPromptRefText = 240

// maxPromptLine is the longest line Prompts decodes; a longer one is scanned
// past unread, so holding a line costs at most this much memory.
//
// ⚠ A PROMPT LONGER THAN THIS IS NOT INDEXED. Four times the backfill window,
// so it is far past anything the view itself could draw (ReadLines skips a line
// longer than its read): a prompt this size is pasted images, not text.
const maxPromptLine = 16 << 20

// Prompts is every prompt in r's first size bytes, in order — exactly the
// records Events draws as a prompt (a slash command included, as "/name args"),
// each named by the Line ReadLines gives its event.
//
// 🔴 IT READS THE WHOLE FILE AND DECODES ALMOST NONE OF IT. Only a line that
// holds the JSON string "user" and not "tool_result" can be a prompt (a user
// record's type; inside a JSON string a quote is escaped, so text can spell
// neither token, and JSON's own spacing cannot split one); the rest is scanned
// past. Like every reader here, a trailing line with no newline
// is not consumed.
func Prompts(r io.ReaderAt, size int64) ([]PromptRef, error) {
	br := bufio.NewReaderSize(io.NewSectionReader(r, 0, size), 64<<10)
	var (
		out  []PromptRef
		pos  int64
		line []byte
		long bool
	)
	for {
		chunk, err := br.ReadSlice('\n')
		pos += int64(len(chunk))
		if !long {
			if len(line)+len(chunk) > maxPromptLine {
				long, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil // a partial last line: not consumed
			}
			return nil, err
		}
		if !long && bytes.Contains(line, []byte(`"user"`)) && !bytes.Contains(line, []byte(`"tool_result"`)) {
			if rec, ok := DecodeLine(line[:len(line)-1]); ok {
				for _, ev := range Events(rec) {
					if ev.Kind == KindPrompt {
						out = append(out, PromptRef{Line: pos, Text: Cap(firstLine(ev.Text), MaxPromptRefText)})
					}
				}
			}
		}
		line, long = line[:0], false
	}
}
