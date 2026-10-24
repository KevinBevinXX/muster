package cctranscript

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// THE PINNED SAMPLES: the usage-limit texts the detection and the reset parser
// are built from. The first four are the shapes the current CLI writes
// (counted on real transcripts: "You've hit your session limit · resets
// <h>[:<mm>]<am|pm> (<Zone>)" and "You've hit your weekly limit · resets <Mon>
// <d>, <h><am|pm> (<Zone>)"); the times and zones here are invented. The two
// after are an earlier CLI's wording, kept so a host on an older CLI is read
// the same way.
var limitSamples = []struct {
	name, text string
	at         string // the record's timestamp
	want       string // RFC 3339 reset; "" = none can be placed
}{
	{"session limit, hour and minutes, later today", "You've hit your session limit · resets 10:30pm (America/New_York)",
		"2026-10-12T23:10:39.769Z", "2026-10-12T22:30:00-04:00"},
	{"session limit, hour only, tomorrow (already past today)", "You've hit your session limit · resets 9am (Europe/London)",
		"2026-10-12T14:28:49Z", "2026-10-13T09:00:00+01:00"},
	{"weekly limit, a date this year", "You've hit your weekly limit · resets Oct 15, 10pm (Asia/Tokyo)",
		"2026-10-12T20:54:28Z", "2026-10-15T22:00:00+09:00"},
	{"weekly limit, a date that is next year's", "You've hit your weekly limit · resets Jan 2, 7am (UTC)",
		"2026-12-30T08:00:00Z", "2027-01-02T07:00:00Z"},
	{"an earlier CLI: usage limit reached, reset at", "Claude usage limit reached. Your limit will reset at 3pm (America/Los_Angeles).",
		"2026-07-01T18:00:00Z", "2026-07-01T15:00:00-07:00"},
	{"an earlier CLI: no zone, so no reset time", "5-hour limit reached ∙ resets 3pm",
		"2026-07-01T18:00:00Z", ""},
	{"an unknown zone: no reset time", "You've hit your session limit · resets 10:30pm (Mars/Olympus_Mons)",
		"2026-10-12T23:10:39Z", ""},
	{"12am is midnight", "You've hit your session limit · resets 12am (UTC)",
		"2026-10-12T23:10:00Z", "2026-10-13T00:00:00Z"},
	{"12pm is noon", "You've hit your session limit · resets 12:15pm (UTC)",
		"2026-10-12T08:00:00Z", "2026-10-12T12:15:00Z"},
	{"no reset clause at all", "You've hit your session limit", "2026-10-12T08:00:00Z", ""},
}

func TestLimitResetReadsEveryPinnedSample(t *testing.T) {
	for _, s := range limitSamples {
		t.Run(s.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339Nano, s.at)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := LimitReset(s.text, at)
			if s.want == "" {
				if ok {
					t.Fatalf("LimitReset(%q) = %s; want none", s.text, got)
				}
				return
			}
			want, err := time.Parse(time.RFC3339, s.want)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || !got.Equal(want) {
				t.Fatalf("LimitReset(%q, %s) = %s, %t; want %s", s.text, s.at, got, ok, want)
			}
			// Its zone is the one the text names (what a person reads as "10:30
			// PM"), not the reader's.
			if _, off := got.Zone(); off != func() int { _, o := want.Zone(); return o }() {
				t.Fatalf("zone offset of %s; want %s's", got, want)
			}
		})
	}
}

// TestLimitResetIsReadFromTheRecordsTimeNotTheReadersClock: the same text
// read at two record times names two different resets — the first such time
// after each — so a transcript read later still says when THAT limit reset.
func TestLimitResetIsReadFromTheRecordsTimeNotTheReadersClock(t *testing.T) {
	text := "You've hit your session limit · resets 10pm (UTC)"
	before := time.Date(2026, 3, 1, 21, 0, 0, 0, time.UTC)
	after := time.Date(2026, 3, 1, 22, 0, 1, 0, time.UTC)
	a, ok1 := LimitReset(text, before)
	b, ok2 := LimitReset(text, after)
	if !ok1 || !ok2 || !a.Equal(time.Date(2026, 3, 1, 22, 0, 0, 0, time.UTC)) || !b.Equal(time.Date(2026, 3, 2, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("at %s: %s %t; at %s: %s %t; want today's 10pm, then tomorrow's", before, a, ok1, after, b, ok2)
	}
}

func TestLimitResetRefusesAClockOrDateThatDoesNotExist(t *testing.T) {
	at := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"You've hit your session limit · resets 13pm (UTC)",
		"You've hit your session limit · resets 0am (UTC)",
		"You've hit your session limit · resets 10:61pm (UTC)",
		"You've hit your weekly limit · resets Feb 30, 10pm (UTC)",
	} {
		if got, ok := LimitReset(text, at); ok {
			t.Errorf("LimitReset(%q) = %s; want none", text, got)
		}
	}
}

// synthetic is a record the CLI wrote itself, built from the shape the real
// ones have (field names counted on real transcripts; content invented).
func synthetic(text, code string, quotaResetsAt int64) string {
	q := ""
	if quotaResetsAt > 0 {
		q = `,"quotaLimits":{"status":"rejected","resetsAt":` + itoa(quotaResetsAt) + `,"rateLimitType":"five_hour"}`
	}
	errField := ""
	if code != "" {
		errField = `,"error":"` + code + `","isApiErrorMessage":true`
	}
	return `{"type":"assistant","timestamp":"2026-10-12T20:00:00.000Z"` + errField + q +
		`,"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func TestErrorEndOfTellsALimitFromOtherErrorsAndFromNoError(t *testing.T) {
	limit := "You've hit your session limit · resets 10:30pm (UTC)"
	cases := []struct {
		name      string
		line      string
		ok, isLim bool
		code      string
		resets    time.Time
	}{
		{"a limit, its reset read from the text", synthetic(limit, "rate_limit", 0), true, true, "rate_limit",
			time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC)},
		{"a limit whose record carries quotaLimits.resetsAt: that wins", synthetic(limit, "rate_limit", 1792000000), true, true, "rate_limit",
			time.Unix(1792000000, 0)},
		{"a limit with no reset time", synthetic("You've hit your session limit", "rate_limit", 0), true, true, "rate_limit", time.Time{}},
		{"not logged in: an error, not a limit", synthetic("Not logged in · Please run /login", "authentication_failed", 0), true, false,
			"authentication_failed", time.Time{}},
		{"an API error", synthetic("API Error: Request timeout", "server_error", 0), true, false, "server_error", time.Time{}},
		{"a synthetic record with no error flag (a resumed session's reply) is no error",
			synthetic("No response requested.", "", 0), false, false, "", time.Time{}},
		{"a REAL reply that quotes the words is no error",
			`{"type":"assistant","timestamp":"2026-10-12T20:00:00Z","message":{"model":"claude-x","content":[{"type":"text","text":"You've hit your session limit · resets 10pm (UTC)"}]}}`,
			false, false, "", time.Time{}},
		{"a record a model wrote (not the CLI) is no error, flags or not",
			`{"type":"assistant","timestamp":"2026-10-12T20:00:00Z","error":"rate_limit","isApiErrorMessage":true,"message":{"model":"claude-x","content":[{"type":"text","text":"You've hit your session limit · resets 10pm (UTC)"}]}}`,
			false, false, "", time.Time{}},
		{"a user record that quotes the words is no error",
			`{"type":"user","timestamp":"2026-10-12T20:00:00Z","message":{"content":"You've hit your session limit · resets 10pm (UTC)"}}`,
			false, false, "", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := DecodeLine([]byte(c.line))
			if !ok {
				t.Fatalf("decode %s", c.line)
			}
			e, got := r.ErrorEndOf()
			if got != c.ok || e.Limit != c.isLim || e.Code != c.code || !e.ResetsAt.Equal(c.resets) {
				t.Fatalf("ErrorEndOf = %+v, %t; want ok %t, limit %t, code %q, resets %s", e, got, c.ok, c.isLim, c.code, c.resets)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
