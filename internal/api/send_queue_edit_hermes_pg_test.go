package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAHermesWindowsQueuedMessageIsEditableAndHeld: the queue's Edit (#198)
// works for a HERMES window's row as for Claude Code's — the row is held, the
// dispatcher sends nothing while it is (the Hermes window idle and its host
// sending to Hermes), and after Save the EDITED text is what is delivered.
func TestAHermesWindowsQueuedMessageIsEditableAndHeld(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.windows = hermesWindows()
	f.mirror.hermesSend = true
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusIdle,
		Since: time.Now().Add(-time.Hour).UnixMilli(), Observed: time.Now().Add(-time.Hour)}
	it := f.enqueue(t, "for Hermes, as queued", 0, 0)
	cookie := sessionCookieFor(f.srv)

	resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), cookie, true, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Edit of a Hermes row: %d %q", resp.StatusCode, body)
	}
	held := f.row(t, it.ID)
	if !held.Held {
		t.Fatal("the Hermes row is not held after Edit")
	}
	f.tick(t)
	f.tick(t)
	if p := f.pasted(); len(p) != 0 {
		t.Fatalf("🔴 pasted %q into the Hermes window while its row was held", p)
	}
	resp, body = f.retry(t, ui.SendQueueEditSavePath(it.ID), cookie, true, url.Values{"hold": {held.HoldToken}, "text": {"for Hermes, as edited"}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Save: %d %q", resp.StatusCode, body)
	}
	f.tick(t)
	if p := f.pasted(); len(p) != 1 || p[0] != "for Hermes, as edited" {
		t.Fatalf("after Save the dispatcher pasted %q; want the edited text", p)
	}
}
