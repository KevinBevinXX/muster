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
// has Approve/Reject, and no card of any state offers a second approval (there
// is no re-approval: an unconfirmed draft is never offered again).
func TestADraftCardShowsExactlyWhatIsApproved(t *testing.T) {
	at := time.Now().UTC()
	agentID := int64(3)
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 9, AgentID: &agentID, AgentName: "chief", Body: "café\nsee you", Status: plugins.DraftPending,
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
	for _, bad := range []string{"<script>", "café"} {
		if strings.Contains(got, bad) {
			t.Errorf("the pending card carries %q", bad)
		}
	}
	// Approved and not yet claimed: the approval can be withdrawn; once claimed, not.
	c.Draft.Status = plugins.DraftApproved
	if got := draftCardHTML(c); !strings.Contains(got, `action="/plugins/drafts/9/reject"`) || !strings.Contains(got, "data-draft-withdraw") {
		t.Errorf("an approved, unclaimed card has no Withdraw:\n%s", got)
	}
	c.Draft.ClaimedAt = &at
	if got := draftCardHTML(c); strings.Contains(got, "data-draft-withdraw") {
		t.Errorf("a claimed card offers Withdraw:\n%s", got)
	}
	c.Draft.ClaimedAt = nil
	// A pending card whose message has an unconfirmed earlier reply says so.
	c.Draft.Status, c.EarlierUnconfirmed = plugins.DraftPending, true
	if got := draftCardHTML(c); !strings.Contains(got, "data-draft-twice") {
		t.Errorf("the send-twice warning is missing:\n%s", got)
	}
	c.EarlierUnconfirmed = false
	// The approval surface, structurally: the step-up form is the ONLY way an
	// approval is posted, so its count per state is the claim — one on a pending
	// card, none on any other (an unconfirmed card included).
	for _, st := range []plugins.DraftStatus{plugins.DraftPending, plugins.DraftApproved, plugins.DraftSent, plugins.DraftFailed,
		plugins.DraftRejected, plugins.DraftUnconfirmed, plugins.DraftExpired, plugins.DraftSuperseded} {
		c.Draft.Status = st
		got := draftCardHTML(c)
		want := 0
		if st == plugins.DraftPending {
			want = 1
		}
		if n := strings.Count(got, "data-step-up-form"); n != want || strings.Count(got, "/approve\"") != want {
			t.Errorf("%s card: %d approval form(s); want %d:\n%s", st, n, want, got)
		}
	}
}
