package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// tap is the operator tapping key's notification: GET /attention/open/<key>,
// and where it redirects.
func (f *answerFixture) tap(t *testing.T, key string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, attentionOpenURL(key), nil, f.operator())
	if w.Code != http.StatusSeeOther {
		t.Fatalf("tap %s: %d %s", key, w.Code, w.Body)
	}
	return w.Header().Get("Location")
}

// TestAnAskOverEachDoorPushesItsSession: on a real Postgres, through the real
// MCP doors (row-level security and all), each push carries its tap route, and
// a tap opens the asker's session — a mirrored Claude session's window
// (secondSID runs in ws's @3, session "side"), a dispatched agent's page (the
// chief's), a waiting window.
func TestAnAskOverEachDoorPushesItsSession(t *testing.T) {
	f := newAnswerFixture(t)
	urls := func() map[string]string {
		f.srv.WaitForPushes()
		out := map[string]string{}
		for _, p := range f.pusher.to(ep("attn-operator")) {
			if p.Type == attentionNotificationType {
				out[p.Tag] = p.Data["url"]
			}
		}
		return out
	}
	mirrored := attentionAskKey(f.mirrorAsk(t, secondSID, "Keep the old schema?"))
	chiefAsk := attentionAskKey(f.ask(t, f.chiefTok, "Route task 5 to whom?"))
	got := urls()
	for _, k := range []string{mirrored, chiefAsk} {
		if got[attentionTag(k)] != "/attention/open/"+k {
			t.Fatalf("%s's push carries %q (pushes %v)", k, got[attentionTag(k)], got)
		}
	}
	if loc, want := f.tap(t, mirrored), "/mirror/view?host=ws&session=side&window=%403"; loc != want {
		t.Errorf("a tap on the mirrored ask opens %q, want %q", loc, want)
	}
	if loc, want := f.tap(t, chiefAsk), "/agents/"+f.chief.Name; loc != want {
		t.Errorf("a tap on the chief's ask opens %q, want %q", loc, want)
	}
	// ws's @1 starts waiting at a prompt after the host's first observation
	// (which seeds silently).
	f.setWindows(mirror.StatusBusy)
	f.srv.BroadcastAgentsChangedFor(1)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	waiting := sessionKey("ws", "main", "@1")
	if got := urls()[attentionTag(waiting)]; got != "/attention/open/"+waiting {
		t.Fatalf("the waiting window's push carries %q", got)
	}
	if loc, want := f.tap(t, waiting), "/mirror/view?host=ws&session=main&window=%401"; loc != want {
		t.Errorf("a tap on the waiting window opens %q, want %q", loc, want)
	}
}

// TestATapResolvesWhenTappedNotWhenSent: what changed between the push and
// the tap decides where the tap lands — the asking session's window closed,
// the waiting window stopped waiting, the asking agent was destroyed, the ask
// was answered: each opens its item on the queue, never a dead page. Another
// owner's ask reads exactly like a missing one (the queue, naming nothing of
// theirs), and a key that is not one is a 404.
func TestATapResolvesWhenTappedNotWhenSent(t *testing.T) {
	f := newAnswerFixture(t)
	mirrored := attentionAskKey(f.mirrorAsk(t, secondSID, "Keep the old schema?"))
	chiefAsk := attentionAskKey(f.ask(t, f.chiefTok, "Route task 5 to whom?"))
	answered := f.ask(t, f.opTok, "Answered elsewhere?")
	// While everything still stands, each opens its session (the control).
	if loc := f.tap(t, mirrored); !strings.HasPrefix(loc, "/mirror/view?") {
		t.Fatalf("control: %s opens %q", mirrored, loc)
	}
	if loc := f.tap(t, chiefAsk); loc != "/agents/"+f.chief.Name {
		t.Fatalf("control: %s opens %q", chiefAsk, loc)
	}
	// ws's @1 starts waiting; the window list it is read from no longer has
	// @3, so the window running secondSID has CLOSED since its ask was sent.
	f.setWindows(mirror.StatusBusy)
	f.srv.BroadcastAgentsChangedFor(1)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	waiting := sessionKey("ws", "main", "@1")
	f.srv.WaitForPushes()
	if loc := f.tap(t, waiting); !strings.HasPrefix(loc, "/mirror/view?") {
		t.Fatalf("control: %s opens %q", waiting, loc)
	}

	// @1 stops waiting, the chief is destroyed, and the third ask is answered
	// on another device.
	f.setWindows(mirror.StatusBusy)
	if _, err := f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.answerAsk(asUser(1), answered, "done"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{mirrored, waiting, chiefAsk, attentionAskKey(answered)} {
		if loc := f.tap(t, k); loc != "/attention#"+k {
			t.Errorf("a tap on %s after it went stale opens %q, want its queue item", k, loc)
		}
	}

	// Another owner's ask: the queue, exactly as for a missing id.
	theirs := attentionAskKey(f.ask(t, f.workerTok, "A member's question?"))
	missing := "ask-987654321"
	if loc := f.tap(t, theirs); loc != "/attention#"+theirs || strings.Contains(loc, f.worker.Name) {
		t.Errorf("another owner's ask opens %q", loc)
	}
	if loc := f.tap(t, missing); loc != "/attention#"+missing {
		t.Errorf("a missing ask opens %q", loc)
	}
	for _, bad := range []string{"draft-1", "ask-0", "ask-1x", "session-zz", "ask-1%0d%0aSet-Cookie:x=1"} {
		if w := f.call(t, http.MethodGet, attentionOpenPath+bad, nil, f.operator()); w.Code != http.StatusNotFound {
			t.Errorf("GET %s%s: %d, want 404", attentionOpenPath, bad, w.Code)
		}
	}
}
