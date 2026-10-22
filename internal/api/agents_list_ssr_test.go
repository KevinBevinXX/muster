package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/recents"
)

// agentsListOpen is #agents-list's opening tag in a document.
var agentsListOpen = regexp.MustCompile(`<div id="agents-list"[^>]*>`)

// listServer is the pins fixture (three agents, two mirrored windows, agent 6
// pinned, a recent entry) with a provisioner that counts its reads.
func listServer(t *testing.T) (*Server, http.Handler, *countingInstances) {
	t.Helper()
	mem := &memRecents{}
	mem.put(recents.Entry{AgentID: 5})
	mp := &memPins{}
	s, _ := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	mp.pinFor(1).Agents[6] = true
	prov := &countingInstances{Provisioner: newFakeTerminal()}
	s.ext.Recents = mem
	s.ext.Provisioner = prov
	return s, s.Handler(), prov
}

// 🔴 ONE RULE, ONE RENDERER: the /agents document's #agents-list holds EXACTLY
// the /ui/agents partial's cards — byte for byte — for one state. The partial
// is that same body followed by the out-of-band "Recently viewed" strip, so the
// document's list is a prefix of it that ends where the strip begins.
//
// The fixture's times are fixed and months old, so the relative "active … ago"
// both requests print cannot tick between them.
func TestTheAgentsDocumentDrawsTheListPartial(t *testing.T) {
	s, h, _ := listServer(t)
	doc := getOwed(t, s, h, "/agents").Body.String()
	part := getOwed(t, s, h, "/ui/agents").Body.String()

	strip := strings.Index(part, `<nav id="agents-recent"`)
	if strip < 0 {
		t.Fatalf("control: the partial carries no out-of-band strip:\n%s", part)
	}
	cards := part[:strip]
	if n := strings.Count(cards, "<article "); n != 5 {
		t.Fatalf("control: the partial drew %d cards, want the fixture's 5:\n%s", n, cards)
	}
	open := agentsListOpen.FindStringIndex(doc)
	if open == nil {
		t.Fatalf("no #agents-list in the /agents document")
	}
	got := doc[open[1]:]
	if !strings.HasPrefix(got, cards+"</div>") {
		t.Errorf("the /agents document's #agents-list is not the /ui/agents partial's cards.\n--- document list (first 600 bytes):\n%.600s\n--- partial cards (first 600 bytes):\n%.600s", got, cards)
	}
}

// TestTheAgentsFirstPaintBuildsTheListOnce: the Agents tab's first paint is
// ONE list build. The document draws the cards (one provisioner read, for their
// live status) and its list issues no `load` request of its own — every live
// trigger is kept.
//
// On the base tree the document read no instances and drew no cards; every
// paint of the tab was empty until /ui/agents answered.
func TestTheAgentsFirstPaintBuildsTheListOnce(t *testing.T) {
	s, h, prov := listServer(t)
	doc := getOwed(t, s, h, "/agents").Body.String()
	if prov.n != 1 {
		t.Errorf("GET /agents read the provisioner's instances %d time(s), want exactly 1 (the list's one build)", prov.n)
	}
	open := agentsListOpen.FindString(doc)
	if open == "" {
		t.Fatal("no #agents-list in the /agents document")
	}
	if n := strings.Count(doc[strings.Index(doc, open):], "<article "); n != 5 {
		t.Errorf("the /agents document drew %d cards in its list, want the fixture's 5", n)
	}
	trig := regexp.MustCompile(`hx-trigger="([^"]*)"`).FindStringSubmatch(open)
	if trig == nil {
		t.Fatalf("the list has no hx-trigger: %s", open)
	}
	if strings.HasPrefix(trig[1], "load") || strings.Contains(trig[1], " load,") {
		t.Errorf("the server-rendered list still loads itself: hx-trigger=%q", trig[1])
	}
	for _, live := range []string{"sse:agent.changed from:body", "sse:chat.reply from:body", "muster:resync from:body", "agents:changed from:body"} {
		if !strings.Contains(trig[1], live) {
			t.Errorf("the list lost its live trigger %q: %q", live, trig[1])
		}
	}
}

// TestOtherTabsStillLoadTheListThemselves: a tab that is not Agents draws the
// panel hidden and EMPTY, and fills it with the list's own load — it does not
// pay for the list build on every page of the app.
func TestOtherTabsStillLoadTheListThemselves(t *testing.T) {
	s, h, prov := listServer(t)
	doc := getOwed(t, s, h, "/tasks").Body.String()
	if prov.n != 0 {
		t.Errorf("GET /tasks read the provisioner's instances %d time(s), want none", prov.n)
	}
	open := agentsListOpen.FindString(doc)
	if !strings.Contains(open, `hx-trigger="load, `) {
		t.Errorf("the /tasks document's hidden list does not load itself: %s", open)
	}
	rest := doc[strings.Index(doc, open)+len(open):]
	if strings.HasPrefix(rest, "<article") {
		t.Error("the /tasks document drew the list's cards")
	}
}

// blockingInstances is a Provisioner whose Instances read never answers (a
// stalled apiserver) — not even to its context — until release is closed.
type blockingInstances struct {
	Provisioner
	release chan struct{}
}

func (b *blockingInstances) Instances(context.Context) ([]provision.Instance, error) {
	<-b.release
	return nil, nil
}

// pendingInstances is a Provisioner that answers at once: chatting-oldest's
// instance is still starting.
type pendingInstances struct{ Provisioner }

func (pendingInstances) Instances(context.Context) ([]provision.Instance, error) {
	return []provision.Instance{{Ref: provision.Ref{Name: "chatting-oldest"}, Phase: provision.PhasePending}}, nil
}

// statusOf is the status agent card <name> was drawn with.
func statusOf(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<a href="/agents/` + name + `" hx-boost="false" data-card-slot="body".*?aria-label="Status: ([a-z]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no card for %s with a status", name)
	}
	return m[1]
}

// 🔴 A STALLED APISERVER MUST NOT TAKE THE AGENTS TAB WITH IT: GET /agents
// answers within shellInstancesWithin (plus a margin) behind an instance read
// that never returns, and still draws the CARDS — with their stored status
// ("running" for chatting-oldest, which is stored running). A provisioner that
// answers is waited for: the same card is drawn with the LIVE status (its
// instance still starting: "provisioning"). On c055625f the document waited for the read forever.
func TestTheAgentsDocumentDoesNotWaitOnAStalledInstanceRead(t *testing.T) {
	s, h, _ := listServer(t)
	block := &blockingInstances{Provisioner: newFakeTerminal(), release: make(chan struct{})}
	t.Cleanup(func() { close(block.release) })
	s.ext.Provisioner = block

	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/agents", nil)
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(shellInstancesWithin + 3*time.Second):
		t.Fatalf("GET /agents did not answer within %s behind a stalled instance read", shellInstancesWithin+3*time.Second)
	}
	if took := time.Since(start); took > shellInstancesWithin+time.Second {
		t.Errorf("GET /agents took %s behind a stalled instance read, want under %s", took, shellInstancesWithin+time.Second)
	}
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Count(body[strings.Index(body, `id="agents-list"`):], "<article ") != 5 {
		t.Fatalf("GET /agents = %d without the fixture's 5 cards", rec.Code)
	}
	if got := statusOf(t, body, "chatting-oldest"); got != "running" {
		t.Errorf("past the bound the card is drawn %q, want its stored status running", got)
	}

	// Control: a provisioner that answers is waited for, and its answer is drawn.
	s.ext.Provisioner = pendingInstances{Provisioner: newFakeTerminal()}
	if got := statusOf(t, getOwed(t, s, h, "/agents").Body.String(), "chatting-oldest"); got != "provisioning" {
		t.Errorf("with the live read answered, the card is drawn %q, want the live status provisioning", got)
	}
}
