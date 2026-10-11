package notes

import (
	"testing"
	"time"
)

// TestListPageActivityIsNotTheCardRevision: ListPage hands the board the time it
// ORDERS by (Note.ActivityAt, so a card can print it) without that value ever
// reaching UpdatedAt, which is the card revision (data-card-rev).
//
// The fixture makes all three times pairwise distinct — the note's own write, a
// comment, and a linked agent — and the agent the newest, so ActivityAt must
// equal the AGENT time while UpdatedAt stays the NOTE time. A projection that
// selected the activity expression into UpdatedAt, or that left ActivityAt at the
// note time, fails one of the two.
func TestListPageActivityIsNotTheCardRevision(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("revfix"))
	base := time.Date(2025, 1, 12, 9, 30, 0, 0, time.UTC)
	noteAt := base.Add(1 * time.Hour)
	commentAt := base.Add(3 * time.Hour)
	agentAt := base.Add(7 * time.Hour)

	id := f.task("worked by an agent", StatusOpen, noteAt)
	f.comment(id, "progress note", commentAt)
	f.agent(id, uniqueMarker("revagent"), agentAt)

	p, err := f.store.ListPage(f.ctx, ListFilter{Tags: []string{f.tag}})
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(p.Notes) != 1 {
		t.Fatalf("ListPage returned %d notes, want 1", len(p.Notes))
	}
	n := p.Notes[0]
	if !n.UpdatedAt.Equal(noteAt) {
		t.Errorf("UpdatedAt = %v, want the note's own write %v: the card revision must not move "+
			"with activity the note itself never had", n.UpdatedAt, noteAt)
	}
	if !n.ActivityAt.Equal(agentAt) {
		t.Errorf("ActivityAt = %v, want the newest activity (the agent) %v", n.ActivityAt, agentAt)
	}
	if !n.LastActivity().Equal(agentAt) {
		t.Errorf("LastActivity() = %v, want %v", n.LastActivity(), agentAt)
	}

	// Get computes no activity: LastActivity falls back to UpdatedAt, never zero.
	g, err := f.store.Get(f.ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !g.ActivityAt.IsZero() || !g.LastActivity().Equal(noteAt) {
		t.Errorf("Get: ActivityAt=%v LastActivity=%v, want zero and the note time %v",
			g.ActivityAt, g.LastActivity(), noteAt)
	}
}
