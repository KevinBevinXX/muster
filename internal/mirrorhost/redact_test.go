package mirrorhost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// TestThePendingSummaryAndLastWordsAreRedacted: the redaction is on the path
// that leaves the host — PendingOf's summary and last text — not only in the
// helper.
func TestThePendingSummaryAndLastWordsAreRedacted(t *testing.T) {
	p := PendingOf([]cctranscript.Event{
		{Kind: cctranscript.KindText, Text: "Using GITHUB_TOKEN=ghp_0123456789abcdef to push."},
		call("t1", "Bash", map[string]string{"command": `curl -H "Authorization: Bearer abc123def" https://example.test`}),
	})
	if p == nil || p.Summary != `Bash: curl -H "Authorization: «redacted»" https://example.test` || p.Tool != "Bash" ||
		p.Last != "Using GITHUB_TOKEN=«redacted» to push." {
		t.Fatalf("pending %+v", p)
	}
	for _, f := range []string{p.Summary, p.Last} {
		if strings.Contains(f, "abc123def") || strings.Contains(f, "ghp_") {
			t.Fatalf("a secret left the host: %q", f)
		}
	}
}
