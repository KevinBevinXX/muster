package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// THE HELD LINE (sendQueueWaiting): a held queue's time without script, in
// UTC and saying so — with its date when it is not today's (a weekly limit's
// reset is days away) — and a line that never takes a third line at any width:
// clamped to two, its whole text its title.

func TestUTCClockCarriesTheDateWhenTheTimeIsNotToday(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC), "22:30 UTC"},
		{time.Date(2026, 10, 13, 0, 5, 0, 0, time.UTC), "Oct 13 00:05 UTC"},
		{time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC), "Oct 15 22:00 UTC"},
		// The same instant in another zone is still drawn in UTC.
		{time.Date(2026, 10, 12, 18, 30, 0, 0, time.FixedZone("x", -4*3600)), "22:30 UTC"},
	} {
		if got := utcClock(c.t, now); got != c.want {
			t.Errorf("utcClock(%s) = %q; want %q", c.t, got, c.want)
		}
	}
	if got := utcClock(time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC), time.Time{}); got != "Oct 12 22:30 UTC" {
		t.Errorf("with no now: %q; want the date", got)
	}
}

func TestTheWaitingLineIsClampedToTwoLinesWithItsWholeTextAsTitle(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	for _, v := range []SendQueueListView{
		{Waiting: "Claude Code is working."},
		{Waiting: "Held — usage limit, resumes", Held: true, ResumeAt: time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC), Now: now},
		{Waiting: strings.Repeat("a long reason ", 20), Held: true},
	} {
		var b bytes.Buffer
		if err := sendQueueWaiting(v).Render(&b); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		if !strings.Contains(html, "line-clamp-2") || !strings.Contains(html, "min-h-[2lh]") {
			t.Fatalf("not clamped to two reserved lines: %s", html)
		}
		want := v.Waiting
		if !v.Held {
			want = "Next: " + v.Waiting
		}
		if !v.ResumeAt.IsZero() {
			want += " Oct 15 22:00 UTC"
		}
		if !strings.Contains(html, `title="`+want+`"`) {
			t.Fatalf("title is not the whole line %q: %s", want, html)
		}
	}
}
