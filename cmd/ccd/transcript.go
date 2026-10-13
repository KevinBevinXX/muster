package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// The session transcript is the ONE place ccd reads a reply from.
//
// 🔴 NOT THE SCREEN. The TUI hard-wraps long lines at the pane width, draws box
// glyphs, collapses blank lines in its DISPLAY (the transcript keeps them) and has
// no turn boundary a scraper could find. The transcript JSONL is the structured
// record Claude Code itself writes, one JSON object per line.
//
// The record shapes this parser relies on, all observed in real transcripts (the
// fixtures under testdata/ are those records with their content replaced):
//
//   - the user's prompt: {"type":"user","promptId":P,"message":{"content":"<text>"}}
//     — promptId equals the `prompt_id` the UserPromptSubmit/Stop/StopFailure hooks
//     carry, which is how a turn is located without matching on text.
//   - tool results inside that same turn: {"type":"user","promptId":P,
//     "message":{"content":[{"type":"tool_result",…}]}} — SAME promptId.
//   - assistant output: {"type":"assistant","message":{"content":[<ONE block>]}},
//     one record per content block (thinking | text | tool_use), no promptId.
//   - an API failure: an assistant record with "isApiErrorMessage":true, an
//     "error" code (authentication_failed, rate_limit, server_error, …) and usually
//     "apiErrorStatus" (401, 429, …). Its text is the CLI's human message, e.g.
//     "You've hit your session limit · resets 9:45pm (UTC)".
//   - the end of a turn: {"type":"system","subtype":"turn_duration"}, written AFTER
//     the Stop hooks ran (a "stop_hook_summary" system record precedes it), which is
//     why the caller polls for it briefly after the Stop hook arrives.

// apiError is a failed turn's error, as the transcript (or a StopFailure hook)
// states it.
type apiError struct {
	Code    string // the CLI's error code: authentication_failed, rate_limit, …
	Status  int    // the upstream HTTP status, 0 when the record carries none
	Message string // the CLI's human-readable text
}

// turnResult is everything one turn produced that ccd cares about.
type turnResult struct {
	// Found reports whether the prompt's own record was located at all.
	Found bool
	// Segments are the assistant TEXT blocks, in order. Thinking is dropped and
	// tool_use is skipped: neither is the answer, and forwarding reasoning would
	// render it to the user as if it were.
	Segments []string
	// ToolUses counts tool_use blocks (diagnostics).
	ToolUses int
	// Items is the turn in transcript order: each text segment and each tool
	// call the CLI ran, with its result once the result record exists. It is
	// what the success wire streams so the agent's chat can show the calls as
	// collapsed rows; Segments stays the REPLY (text only), unchanged.
	Items []turnItem
	// Err is set when the turn carried an API-error record.
	Err *apiError
	// Complete reports that the turn's turn_duration record was seen.
	Complete bool
}

// turnItem is one Items entry: a text segment (Tool nil) or a tool call.
type turnItem struct {
	Text string
	Tool *cctranscript.Event // the KindToolCall event
	// Result is the call's KindToolResult event, nil while the tool runs (or
	// when the turn ended before it returned).
	Result *cctranscript.Event
}

// addResult pairs a tool result with its call, by id. A result whose call is
// not in this turn is dropped: it belongs to no row this turn draws.
func (r *turnResult) addResult(ev cctranscript.Event) {
	for i := range r.Items {
		if it := &r.Items[i]; it.Tool != nil && it.Tool.ToolID == ev.ToolID && it.Result == nil {
			ev := ev
			it.Result = &ev
			return
		}
	}
}

// Text joins the segments the way muster's own client joins output items: a blank
// line between two non-empty segments, so distinct paragraphs never fuse.
func (r turnResult) Text() string {
	var b strings.Builder
	for _, s := range r.Segments {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// contentBlock is one part of a REQUEST's user message (promptFrom). The
// transcript's own records are decoded by internal/cctranscript, the one
// decoder of that format in muster.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// maxTranscriptLine bounds one JSONL record. A tool result can be large; a record
// past this is skipped rather than failing the whole read.
const maxTranscriptLine = 32 << 20

// extractTurn reads a transcript and returns the turn whose prompt record carries
// promptID.
//
// ⚠ A TRAILING LINE WITH NO NEWLINE IS IGNORED: it is a record the CLI is still
// writing, and parsing half of it would either fail or, worse, succeed on a prefix.
func extractTurn(r io.Reader, promptID string) (turnResult, error) {
	var res turnResult
	if promptID == "" {
		return res, errors.New("extractTurn: empty prompt id")
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		if len(line) == 0 {
			continue
		}
		rec, ok := cctranscript.DecodeLine(line)
		if !ok {
			continue
		}
		if rec.IsSidechain {
			continue // a subagent's records, not the main conversation's reply
		}
		if !res.Found {
			if rec.Type == "user" && rec.PromptID == promptID {
				res.Found = true
			}
			continue
		}
		switch rec.Type {
		case "user":
			if rec.PromptID != "" && rec.PromptID != promptID {
				// The NEXT prompt began; this turn is over even if its
				// turn_duration was never written (an interrupted turn).
				return res, nil
			}
			for _, ev := range cctranscript.Events(rec) {
				if ev.Kind == cctranscript.KindToolResult {
					res.addResult(ev)
				}
			}
		case "assistant":
			for _, b := range rec.Blocks() {
				switch b.Type {
				case "text":
					if rec.IsAPIError {
						continue // the error's text is reported as the error, never as a reply
					}
					res.Segments = append(res.Segments, b.Text)
					res.Items = append(res.Items, turnItem{Text: b.Text})
				case "tool_use":
					res.ToolUses++
				}
			}
			if !rec.IsAPIError {
				for _, ev := range cctranscript.Events(rec) {
					if ev.Kind == cctranscript.KindToolCall {
						ev := ev
						res.Items = append(res.Items, turnItem{Tool: &ev})
					}
				}
			}
			if rec.IsAPIError {
				msg := ""
				for _, b := range rec.Blocks() {
					if b.Type == "text" {
						msg += b.Text
					}
				}
				res.Err = &apiError{Code: rec.Error, Status: rec.APIErrorStatus, Message: msg}
			}
		case "system":
			if rec.Subtype == "turn_duration" {
				res.Complete = true
				return res, nil
			}
		}
	}
}

// readLine returns one complete newline-terminated line without the newline, or
// io.EOF when only an unterminated tail (or nothing) is left. Lines longer than
// maxTranscriptLine are skipped.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxTranscriptLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if tooLong {
				return []byte{}, nil
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF:
			return nil, io.EOF
		default:
			return nil, err
		}
	}
}
