package cctranscript

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A TURN THAT ENDED IN AN ERROR THE CLI WROTE ITSELF: when Claude Code cannot
// get an answer — a usage limit, "Not logged in", an API error — it ends the
// turn with an assistant record of its OWN (message.model "<synthetic>",
// "isApiErrorMessage": true, and an "error" code), and goes back to its
// prompt. Its sessions file then reads idle, with a newer time, exactly as
// after a real answer — which is all the send queue could see, so it sent the
// next queued message into the same wall (internal/sendqueue's Decide now
// reads this too).
//
// The shapes relied on, counted read-only on real transcripts on one host (464
// synthetic rows; message content not kept): 413 carried "error":"rate_limit"
// with "apiErrorStatus":429 and a usage-limit text, 165 of them also a
// "quotaLimits" object with "resetsAt" (Unix seconds); 21 "authentication_failed"
// ("Not logged in · Please run /login"); 13 "server_error" / "unknown" /
// "model_not_found" API errors; and 14 "No response requested." rows with NO
// error flag — the CLI's reply to a resumed session's meta prompt, which is not
// an error and is not read as one.
//
// 🔴 A USAGE LIMIT IS TOLD BY THE RECORD'S STRUCTURE AND ITS TEXT TOGETHER:
// a synthetic error record (ErrorEndOf) whose text says a limit was hit
// (limitText). Neither alone: a real reply may quote the words, and an error
// code is not a limit by itself.

// SyntheticModel is the model Claude Code names on an assistant record it
// wrote itself rather than received from a model.
const SyntheticModel = "<synthetic>"

// ErrorEnd is what a synthetic error record says (ErrorEndOf).
type ErrorEnd struct {
	// Limit: the record reports a usage limit (the text says so).
	Limit bool
	// Code is the record's own error code ("rate_limit",
	// "authentication_failed", "server_error", …), "" when it names none.
	Code string
	// ResetsAt is when the limit resets: the record's quotaLimits.resetsAt
	// when it carries one, else the time its text names (LimitReset). Zero
	// when neither says, and always zero for a record that is not a limit.
	ResetsAt time.Time
}

// ErrorEndOf is what r says when it is an assistant record the CLI wrote
// itself to report a failure, and false for any other record — a real reply,
// a user record, and a synthetic record with no error flag ("No response
// requested.").
func (r Record) ErrorEndOf() (ErrorEnd, bool) {
	if r.Type != "assistant" || r.Message.Model != SyntheticModel || (!r.IsAPIError && r.Error == "") {
		return ErrorEnd{}, false
	}
	e := ErrorEnd{Code: r.Error}
	text := r.text()
	if !limitText.MatchString(text) {
		return e, true
	}
	e.Limit = true
	if q := r.QuotaLimits; q != nil && q.ResetsAt > 0 {
		e.ResetsAt = time.Unix(q.ResetsAt, 0)
	} else if at := r.Time(); at > 0 {
		if t, ok := LimitReset(text, time.UnixMilli(at)); ok {
			e.ResetsAt = t
		}
	}
	return e, true
}

// text is the record's text blocks, joined (a synthetic record carries one).
func (r Record) text() string {
	var parts []string
	for _, b := range r.Blocks() {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// limitText is a usage-limit message: "You've hit your session limit · …",
// "You've hit your weekly limit · …" (the CLI's words now), and the earlier
// CLI's "Claude usage limit reached. …" and "5-hour limit reached ∙ …".
var limitText = regexp.MustCompile(`(?i)\byou(?:'|’)?ve hit your\b[^\n·∙.]{0,40}?\blimit\b|\busage limit reached\b|\blimit reached\b`)

// resetText is the reset time a limit message names: "resets 9:20pm (Zone)",
// "resets 10pm (Zone)", "resets Oct 15, 10pm (Zone)", "will reset at 3pm
// (Zone)". The zone is an IANA name, in parentheses; without it the time
// cannot be placed, and is not read.
var resetText = regexp.MustCompile(`(?i)\breset(?:s)?(?:\s+at)?\s+` +
	`(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?` +
	`(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([A-Za-z][A-Za-z0-9_+\-]*(?:/[A-Za-z0-9_+\-]+)*)\)`)

var months = map[string]time.Month{"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August, "sep": time.September, "oct": time.October,
	"nov": time.November, "dec": time.December}

// LimitReset is the reset time a usage-limit message written at `at` names,
// and false when it names none this can place: no reset clause, no zone, a
// zone this machine does not know, or a clock or date that does not exist.
//
// The message names a wall-clock time in a zone, and sometimes a date with no
// year. It is read as the FIRST such time after `at` — the record's own
// timestamp, never the reader's clock, so a transcript read days later still
// says when that limit reset: a time of day is today's in that zone if it is
// still ahead of `at`, else tomorrow's; a date is this year's unless that is
// more than a day before `at`, then next year's.
func LimitReset(text string, at time.Time) (time.Time, bool) {
	m := resetText.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[6])
	if err != nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	if hour < 1 || hour > 12 || minute > 59 {
		return time.Time{}, false
	}
	hour %= 12
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	ref := at.In(loc)
	if m[1] == "" {
		t := time.Date(ref.Year(), ref.Month(), ref.Day(), hour, minute, 0, 0, loc)
		if !t.After(ref) {
			t = time.Date(ref.Year(), ref.Month(), ref.Day()+1, hour, minute, 0, 0, loc)
		}
		return t, true
	}
	month := months[strings.ToLower(m[1])]
	day, _ := strconv.Atoi(m[2])
	for _, year := range []int{ref.Year(), ref.Year() + 1} {
		t := time.Date(year, month, day, hour, minute, 0, 0, loc)
		if t.Month() != month || t.Day() != day {
			return time.Time{}, false // Feb 30
		}
		if !t.Before(ref.Add(-24 * time.Hour)) {
			return t, true
		}
	}
	return time.Time{}, false
}
