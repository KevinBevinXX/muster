package mirror_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE ASK HALF OF THE PLUGIN (service.go AuthenticateHost, askanswer.go): the
// hook's token door, and an answer carried to the host to type into the
// picker, with the host's outcome — each kind of refusal its own error.

var askQs = []askq.Question{{Question: "Which color?", Header: "Color", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}}}}

// TestAnAnswerCarriesWhatWasLeftAndTheKeysComeBack: what an earlier answer
// left in the picker goes to the host with the answer (AskTyping.Prior), and
// how many keys the picker received comes back with a refusal (AskKeys) — 0
// from a host that reports none (an older one).
func TestAnAnswerCarriesWhatWasLeftAndTheKeysComeBack(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "hd")
	hi := winHello("hd", true, mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-a"})
	hi.AskAnswer = true
	raw, err := r.rawConnect(t, tok, hi)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "hd", "work")
	choices := []askq.Choice{{Picks: []int{1}}}
	prior := &askq.Progress{Choices: []askq.Choice{{Picks: []int{1}}}, Keys: 3}
	for _, tc := range []struct {
		prior *askq.Progress
		keys  int
	}{{prior, 5}, {nil, 0}} {
		seen := len(raw.of(mirror.TypeAnswer))
		done := make(chan error, 1)
		go func() { done <- r.svc.AnswerWindow(asUser(1), "hd", "work", "@1", askQs, choices, tc.prior) }()
		var asked mirror.Msg
		eventually(t, "the host is asked to answer", func() bool {
			if got := raw.of(mirror.TypeAnswer); len(got) > seen {
				asked = got[seen]
				return true
			}
			return false
		})
		if !reflect.DeepEqual(asked.Ask.Prior, tc.prior) {
			t.Fatalf("the host was sent prior %+v, want %+v", asked.Ask.Prior, tc.prior)
		}
		raw.write(t, mirror.Msg{Type: mirror.TypeAnswered, ID: asked.ID, Error: "moved", Refusal: mirror.AskRefusalLost, Keys: tc.keys})
		got := <-done
		if !errors.Is(got, mirror.ErrAskLost) || mirror.AskKeys(got) != tc.keys {
			t.Fatalf("lost after %d keys: %v, AskKeys %d", tc.keys, got, mirror.AskKeys(got))
		}
	}
}

// TestAnAnswerReachesTheHostAndItsRefusalComesBack: a host that advertised
// AskAnswer is sent the questions and choices for the named window; each kind
// of refusal it names is the error muster acts on, and an empty one is
// success. The controls: a host that did not advertise it — or is read-only —
// is sent nothing.
func TestAnAnswerReachesTheHostAndItsRefusalComesBack(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "hd")
	hi := winHello("hd", true, mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-a"})
	hi.AskAnswer = true
	raw, err := r.rawConnect(t, tok, hi)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "hd", "work")
	if hs := r.svc.Hosts(asUser(1)); len(hs) != 1 || !hs[0].AskAnswer {
		t.Fatalf("the host's AskAnswer is not in its view: %+v", hs)
	}
	choices := []askq.Choice{{Picks: []int{1}}}
	all := []error{mirror.ErrAskNoPicker, mirror.ErrAskTouched, mirror.ErrAskLost, mirror.ErrAskUnsupported, mirror.ErrAskUnrecognised, mirror.ErrAskBusy, mirror.ErrAskUnreadable, mirror.ErrAskOwnDifferent, mirror.ErrAskRefused}
	for _, tc := range []struct {
		why, refusal string
		want         error
		// also is a second error the outcome must be (and the only other).
		also error
	}{
		{"", "", nil, nil},
		{"gone", mirror.AskRefusalNoPicker, mirror.ErrAskNoPicker, nil},
		{"someone typed", mirror.AskRefusalTouched, mirror.ErrAskTouched, nil},
		{"moved", mirror.AskRefusalLost, mirror.ErrAskLost, nil},
		{"cannot", mirror.AskRefusalUnsupported, mirror.ErrAskUnsupported, nil},
		{"cannot read it", mirror.AskRefusalUnrecognised, mirror.ErrAskUnrecognised, nil},
		{"another drive", mirror.AskRefusalBusy, mirror.ErrAskBusy, nil},
		{"other words", mirror.AskRefusalUnreadable, mirror.ErrAskUnreadable, nil},
		{"off the top", mirror.AskRefusalUnreadableTyped, mirror.ErrAskUnreadable, mirror.ErrAskLost},
		{"muster's earlier answer", mirror.AskRefusalOwnDifferent, mirror.ErrAskOwnDifferent, nil},
		{"what", "a-kind-this-muster-does-not-know", mirror.ErrAskRefused, nil},
	} {
		seen := len(raw.of(mirror.TypeAnswer))
		done := make(chan error, 1)
		go func() { done <- r.svc.AnswerWindow(asUser(1), "hd", "work", "@1", askQs, choices, nil) }()
		var asked mirror.Msg
		eventually(t, "the host is asked to answer", func() bool {
			if got := raw.of(mirror.TypeAnswer); len(got) > seen {
				asked = got[seen]
				return true
			}
			return false
		})
		if asked.Session != "work" || asked.Window != "@1" || asked.Ask == nil || !reflect.DeepEqual(asked.Ask.Questions, askQs) || !reflect.DeepEqual(asked.Ask.Choices, choices) {
			t.Fatalf("the host was sent %+v", asked)
		}
		raw.write(t, mirror.Msg{Type: mirror.TypeAnswered, ID: asked.ID, Error: tc.why, Refusal: tc.refusal})
		var got error
		select {
		case got = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("refusal %q: the answer never returned", tc.refusal)
		}
		// Every outcome the host reported is a known one: it is not typing.
		if !mirror.AskOutcomeKnown(got) {
			t.Errorf("refusal %q: %v is not a known outcome", tc.refusal, got)
		}
		if tc.want == nil {
			if got != nil {
				t.Errorf("success: %v", got)
			}
			continue
		}
		for _, e := range all {
			if errors.Is(got, e) != (e == tc.want || e == tc.also) {
				t.Errorf("refusal %q: muster heard %v; want %v and none of the others", tc.refusal, got, tc.want)
				break
			}
		}
	}
	// A stale sign-in: refused here too, before the host hears anything.
	seen := len(raw.of(mirror.TypeAnswer))
	err = r.svc.AnswerWindow(asStaleUser(1), "hd", "work", "@1", askQs, choices, nil)
	if !errors.Is(err, mirror.ErrStepUpRequired) || !errors.Is(err, mirror.ErrAskNotSent) || !mirror.AskOutcomeKnown(err) {
		t.Fatalf("a stale sign-in: %v (want a step-up refusal, not sent)", err)
	}
	if len(raw.of(mirror.TypeAnswer)) != seen {
		t.Fatal("a stale answer reached the host")
	}
}

func TestAHostThatCannotAnswerIsSentNothing(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		host        string
		interactive bool
		advertise   bool
	}{{"older", true, false}, {"readonly", false, true}} {
		tok, _ := r.store.mint(t, 1, c.host)
		hi := winHello(c.host, c.interactive, mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-a"})
		hi.AskAnswer = c.advertise
		raw, err := r.rawConnect(t, tok, hi)
		if err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, c.host, "work")
		ctx, cancel := context.WithTimeout(asUser(1), time.Second)
		err = r.svc.AnswerWindow(ctx, c.host, "work", "@1", askQs, []askq.Choice{{Picks: []int{0}}}, nil)
		cancel()
		if !errors.Is(err, mirror.ErrAskUnsupported) || len(raw.of(mirror.TypeAnswer)) != 0 {
			t.Errorf("%s: %v, %d answer frame(s); want ErrAskUnsupported and none", c.host, err, len(raw.of(mirror.TypeAnswer)))
		}
		if hs := r.svc.Hosts(asUser(1)); func() bool {
			for _, h := range hs {
				if h.Host == c.host && h.AskAnswer {
					return true
				}
			}
			return false
		}() {
			t.Errorf("%s: its view says it can answer", c.host)
		}
	}
	// Another owner: not their host.
	if err := r.svc.AnswerWindow(asUser(2), "older", "work", "@1", askQs, []askq.Choice{{Picks: []int{0}}}, nil); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("another owner: %v", err)
	}
}

// TestAuthenticateHostIsTheConnectDoor: a minted token yields its owner and
// host; a wrong one is refused with 401 and nothing yielded.
func TestAuthenticateHostIsTheConnectDoor(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 7, "lap")
	for _, c := range []struct {
		token string
		ok    bool
	}{{tok, true}, {"mmt_" + "x", false}, {"", false}, {"not-a-token", false}} {
		req := httptest.NewRequest(http.MethodPost, "/mirror/ask", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		owner, host, ok := r.svc.AuthenticateHost(rec, req)
		if ok != c.ok {
			t.Fatalf("token %q: ok %v", c.token, ok)
		}
		if ok && (owner != 7 || host != "lap") {
			t.Fatalf("yielded %d/%s, want 7/lap", owner, host)
		}
		if !ok && rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q refused with %d", c.token, rec.Code)
		}
	}
}

// TestAnOutcomeNeverReportedIsUnknown: an answer sent whose outcome the host
// never reported — the hub's timeout, the host dropping after it was sent, a
// cancelled wait — may still be being typed. Refusals before sending may not.
func TestAnOutcomeNeverReportedIsUnknown(t *testing.T) {
	for _, err := range []error{mirror.ErrAskTimeout, mirror.ErrHostDisconnected, context.Canceled, errors.New("mirror: send the answer to the host: broken pipe")} {
		if mirror.AskOutcomeKnown(err) {
			t.Errorf("%v: known, want unknown", err)
		}
	}
	r := newRig(t)
	err := r.svc.AnswerWindow(asUser(1), "nohost", "work", "@1", askQs, []askq.Choice{{Picks: []int{0}}}, nil)
	if !errors.Is(err, mirror.ErrHostOffline) || !errors.Is(err, mirror.ErrAskNotSent) || !mirror.AskOutcomeKnown(err) {
		t.Fatalf("an offline host: %v (want offline, not sent)", err)
	}
}
