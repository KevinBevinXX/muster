package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// FOCUS FOR A HERMES WINDOW (docs/design/hermes.md, "Listing" §Focus; built
// in the Phase 2 fix pass): keyed by the runtime-qualified session id,
// mirror:hermes:<id>, refreshed from the window's rendered-view backfill like
// a Claude Code window's, and shown on its card. A Hermes window with no
// session (a stale breadcrumb) has none.

func hermesFocusWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: hermesSID, Activity: 1_700_000_000},
		{Session: "main", ID: "@2", Index: 2, Runtime: mirror.RuntimeHermes, Activity: 1_700_000_000},
	}
}

func TestAHermesWindowsFocusIsRefreshedAndShown(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	summ := &recordingSummarizer{}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Gen: hermesSID, Reset: true, Events: []cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Text: "port the importer"},
			{Kind: cctranscript.KindText, Text: "Dates first."},
		}},
		{Type: mirror.TypeEvents, Gen: hermesSID, PromptIndex: true},
	}}, windows: hermesFocusWindows()}
	s, ts := focusMirrorServer(t, fm, store, summ)
	sub := focus.HermesSubject(hermesSID)

	// The pass sees it as a candidate, keyed runtime-qualified.
	cands, err := focusSessions{s}.Candidates(operatorCtx())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range cands {
		if c.Subject == sub {
			found++
		}
		if c.Subject.Kind == focus.KindMirror && !strings.HasPrefix(c.Subject.Key, mirror.HermesKeyPrefix) {
			t.Fatalf("a Hermes window keyed unqualified: %+v", c.Subject)
		}
	}
	if found != 1 {
		t.Fatalf("candidates = %+v, want %s once", cands, sub)
	}

	o, err := s.refresher().Refresh(operatorCtx(), 1, sub)
	if err != nil || o != focus.OutcomeUpdated {
		t.Fatalf("refresh: %v %v", o, err)
	}
	if len(summ.tails) != 1 || summ.tails[0] != "user: port the importer\nassistant: Dates first." {
		t.Fatalf("tail = %q", summ.tails)
	}
	if fm.asked[len(fm.asked)-1] != "@1" {
		t.Fatalf("watched window %q, want @1", fm.asked)
	}
	_, body := mirrorGet(t, s, ts, "/ui/agents")
	if !strings.Contains(body, "Fixing the mirror reconnect") || strings.Count(body, "data-card-focus") != 1 {
		t.Fatal("the Hermes window's focus is not on (exactly) its card")
	}
}

// A stream that turns out to be ANOTHER conversation (a /new between the
// listing and the read) stores nothing.
func TestAHermesFocusReadOfAnotherConversationStoresNothing(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Gen: "local-another", Reset: true, Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "x"}}},
	}}, windows: hermesFocusWindows()}
	s, _ := focusMirrorServer(t, fm, store, &recordingSummarizer{})
	ctx, cancel := context.WithTimeout(operatorCtx(), 10*time.Second)
	defer cancel()
	if o, _ := s.refresher().Refresh(ctx, 1, focus.HermesSubject(hermesSID)); o == focus.OutcomeUpdated {
		t.Fatal("stored a summary of another conversation")
	}
	if _, ok := store.rows[focus.HermesSubject(hermesSID)]; ok && store.rows[focus.HermesSubject(hermesSID)].Title != "" {
		t.Fatal("a focus row was written")
	}
}
