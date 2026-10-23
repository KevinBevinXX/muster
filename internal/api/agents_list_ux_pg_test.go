package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE AGENTS LIST'S UX BATCH on a real Postgres: marking a task complete from
// an agent's page without JavaScript lands on the Agents list, and each card
// says what it is waiting on (data-wait), from the attention queue's one read.

// TestMarkCompleteWithoutJavaScriptLandsOnTheAgentsList: the no-JS form path
// of the agent page's Mark-complete (ui.TaskCompletePath). Operator ask:
// "after marking a task complete, it should just redirect back out of the
// agent instead of to another page". A plain form POST completes the task and
// answers 303 → /agents. Red before POST /tasks/{id}/complete existed (the
// route answered 405: the form had no method and no action at all).
func TestMarkCompleteWithoutJavaScriptLandsOnTheAgentsList(t *testing.T) {
	f := newAttnFixture(t)
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "ux complete", Body: "ux complete", Status: notes.StatusReadyForReview})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	path := "/tasks/" + strconv.FormatInt(n.ID, 10) + "/complete"
	if path != ui.TaskCompletePath(n.ID) {
		t.Fatalf("the form's action is %q, the route is %q", ui.TaskCompletePath(n.ID), path)
	}
	// A browser without JavaScript: no HX-Request, a form body, its own origin.
	hdr := map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "http://example.com"}
	body, ct := form(url.Values{})
	w := f.call(t, http.MethodPost, path, body, merge(hdr, ct))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/agents" {
		t.Fatalf("POST %s = %d → %q, want 303 → /agents (%s)", path, w.Code, w.Header().Get("Location"), w.Body)
	}
	if got, _ := f.notes.Get(asUser(1), n.ID); got.Status != notes.StatusComplete {
		t.Fatalf("the task is %s after the POST, want complete", got.Status)
	}
	// Refused cross-origin, and the task is not touched by a refused write.
	n2, err := f.notes.Create(asUser(1), notes.Note{Title: "ux complete 2", Body: "ux complete 2", Status: notes.StatusReadyForReview})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n2.ID)
	})
	before, _ := f.notes.Get(asUser(1), n2.ID)
	body, ct = form(url.Values{})
	w = f.call(t, http.MethodPost, ui.TaskCompletePath(n2.ID), body, merge(map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "http://evil.example"}, ct))
	if w.Code == http.StatusSeeOther {
		t.Fatalf("a cross-origin POST was accepted: %d", w.Code)
	}
	if got, _ := f.notes.Get(asUser(1), n2.ID); got.Status != before.Status || got.Status == notes.StatusComplete {
		t.Fatalf("a refused cross-origin POST changed the task to %s", got.Status)
	}
}

// listCardAttrs is each card's opening <article …> tag in a rendered list, by
// its id.
func listCardAttrs(body string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`<article id="([^"]+)"[^>]*>`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[0]
	}
	return out
}

// TestTheCardsCarryWhatTheyAreWaitingOn: an agent that asked over MCP
// (attention_ask) and a mirrored Claude window stopped at a prompt (status
// waiting) both carry data-wait="you" — what the "Waiting on you" chip
// selects — and a card waiting on nothing carries no data-wait. Red before the
// chips: no card carried the attribute.
func TestTheCardsCarryWhatTheyAreWaitingOn(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship it tonight?")
	cards := listCardAttrs(f.page(t, "/ui/agents"))
	op, op2 := cards["agent-"+strconv.FormatInt(f.op.ID, 10)], cards["agent-"+strconv.FormatInt(f.op2.ID, 10)]
	if op == "" || op2 == "" {
		t.Fatalf("instrument check FAILED: the two agents' cards are not on the list: %v", cards)
	}
	if !strings.Contains(op, ` data-wait="you"`) {
		t.Errorf("the asking agent's card does not carry data-wait=\"you\": %s", op)
	}
	if strings.Contains(op2, "data-wait") {
		t.Errorf("an agent waiting on nothing carries data-wait: %s", op2)
	}
	var waiting, idle int
	for id, tag := range cards {
		if !strings.HasPrefix(id, "mirror-") {
			continue
		}
		switch {
		case strings.Contains(tag, ` data-wait="you"`):
			waiting++
		case !strings.Contains(tag, "data-wait"):
			idle++
		}
	}
	// attnWindows: @1 waiting at a prompt, @2 idle.
	if waiting != 1 || idle != 1 {
		t.Errorf("mirrored cards: %d waiting on an ask and %d on nothing, want 1 and 1: %v", waiting, idle, cards)
	}
}
