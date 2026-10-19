package api

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheRetryCopyDoesNotPromiseEveryReplyWaits (#138 follow-up 1): the
// /plugins retry section used to say "anything it drafts still waits for your
// approval", which is false for an answer routed through a session to a
// verified sender the owner allowlisted (MUSTER_ROUTE_RELAY_ALLOW; relayRoute
// approves it automatically). The WHOLE explanation is pinned, normalised —
// a guard on a word would pass a reworded false claim.
func TestTheRetryCopyDoesNotPromiseEveryReplyWaits(t *testing.T) {
	page := retryableSectionHTML([]plugins.Message{{ID: 7, Sender: "a@example.test", Subject: "s", CreatedAt: time.Unix(0, 0)}}, false)
	p := regexp.MustCompile(`(?s)</h2>\s*<p[^>]*>(.*?)</p>`).FindStringSubmatch(page)
	if p == nil {
		t.Fatalf("no explanation paragraph after the heading:\n%s", page)
	}
	got := strings.Join(strings.Fields(html.UnescapeString(p[1])), " ")
	const want = "Your chief's turn on each of these was cut off, so it may or may not have acted on it. " +
		"Retry hands it to your chief again, which handles it like any message: a reply it drafts waits for your " +
		"approval, except an answer it routes through one of your sessions to a verified sender you allowlisted " +
		"for this plugin, which is sent without it."
	if got != want {
		t.Fatalf("the retry explanation is\n  %q\nwant\n  %q", got, want)
	}
}
