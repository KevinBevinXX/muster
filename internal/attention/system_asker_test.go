package attention

import "testing"

// TestAnAskHasExactlyOneAsker (task routing, audit round 2): muster itself is
// a third kind of asker, never combined with an agent or a session.
func TestAnAskHasExactlyOneAsker(t *testing.T) {
	for _, c := range []struct {
		a  Asker
		ok bool
	}{
		{Asker{System: true}, true},
		{Asker{AgentID: 7}, true},
		{Asker{System: true, AgentID: 7}, false},
		{Asker{System: true, MirrorHost: "host-1", MirrorSession: "abc"}, false},
		{Asker{}, false},
	} {
		if err := c.a.Valid(); (err == nil) != c.ok {
			t.Errorf("%+v: Valid() = %v; want ok=%v", c.a, err, c.ok)
		}
	}
}
