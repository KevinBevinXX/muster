package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestADraftCardShowsExactlyWhatIsApproved: the reply is drawn through
// flows.VisibleEscape (a non-ASCII letter and an invisible-looking character are
// spelled out, a newline is \n) with its length in characters; the outside
// sender's text is HTML-escaped in its own labelled box; only a pending draft
// has Approve/Reject and only an unconfirmed one has "Approve again".
func TestADraftCardShowsExactlyWhatIsApproved(t *testing.T) {
	at := time.Now().UTC()
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 9, AgentName: "chief", Body: "café\nsee you", Status: plugins.DraftPending,
			CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 4, Sender: `Eve <eve@example.invalid>`, Subject: `<b>hi</b>`, Body: `</pre><script>x()</script>`},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	for _, want := range []string{
		`id="draft-9"`, `data-draft-status="pending"`,
		`caf\u{e9}\nsee you`, `12 characters`, `data-runes="12"`,
		`&lt;/pre&gt;&lt;script&gt;x()&lt;/script&gt;`, `&lt;b&gt;hi&lt;/b&gt;`, `Eve &lt;eve@example.invalid&gt;`,
		"from an outside sender, untrusted",
		`action="/plugins/drafts/9/approve" data-step-up-form`, `action="/plugins/drafts/9/reject"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"<script>", "café", "data-draft-reapprove"} {
		if strings.Contains(got, bad) {
			t.Errorf("the pending card carries %q", bad)
		}
	}
	// An unconfirmed draft already approved again, or past its expiry, offers no
	// second re-approval.
	c.Draft.Status = plugins.DraftUnconfirmed
	again := int64(10)
	c.Draft.ReapprovedAs = &again
	if got := draftCardHTML(c); strings.Contains(got, "data-draft-reapprove") || !strings.Contains(got, "as draft 10") {
		t.Errorf("a re-approved unconfirmed card:\n%s", got)
	}
	c.Draft.ReapprovedAs, c.Draft.ExpiresAt = nil, at.Add(-time.Minute)
	if got := draftCardHTML(c); strings.Contains(got, "data-draft-reapprove") {
		t.Errorf("an expired unconfirmed card offers re-approval:\n%s", got)
	}
	c.Draft.ExpiresAt = at.Add(time.Hour)
	for st, want := range map[plugins.DraftStatus][2]bool{ // {approve, reapprove}
		plugins.DraftApproved: {false, false}, plugins.DraftSent: {false, false}, plugins.DraftRejected: {false, false},
		plugins.DraftUnconfirmed: {false, true}, plugins.DraftExpired: {false, false}, plugins.DraftSuperseded: {false, false},
	} {
		c.Draft.Status = st
		got := draftCardHTML(c)
		if strings.Contains(got, "data-draft-approve") != want[0] || strings.Contains(got, "data-draft-reapprove") != want[1] {
			t.Errorf("%s card: approve=%v reapprove=%v; want %v", st, strings.Contains(got, "data-draft-approve"),
				strings.Contains(got, "data-draft-reapprove"), want)
		}
	}
}
