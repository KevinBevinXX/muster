package mirror

import "sort"

// SessionView is one mirrored session as a LIST shows it: its host's facts
// beside its own. Ordered is the only producer.
type SessionView struct {
	Host    string
	Session string
	// Label is the host's display name for the session, "" for none.
	Label string
	// Activity is when the session was last active on its host (Unix seconds,
	// floored to ActivityBucket); 0 = the host did not say.
	Activity int64
	// Online, Interactive: the host's (HostView). Claude: the host reports
	// Claude Code in this session's pane.
	Online      bool
	Interactive bool
	Claude      bool
}

// Ordered is every session of hosts, as ONE list across hosts, MOST RECENTLY
// ACTIVE FIRST.
//
// The sort is STABLE over the order mirrored sessions always had — hosts as
// given (Hub.Hosts sorts them by name), then each host's sessions by name —
// so sessions active in the same ActivityBucket keep that order, and every
// session whose host reported no activity (an older muster-mirror) comes
// after all that did, in that same order. With no activity anywhere the
// result is exactly the old list.
//
// 🔴 ONE RULE, ONE PLACE. The Agents view renders this list and the hub
// decides from this same list whether a host's update changed anything worth
// re-rendering (orderKeys); a second copy of the comparison would let the two
// disagree about what "the order changed" means.
func Ordered(hosts []HostView) []SessionView {
	var out []SessionView
	for _, h := range hosts {
		claude := make(map[string]bool, len(h.Claude))
		for _, c := range h.Claude {
			claude[c] = true
		}
		for _, s := range h.Sessions {
			out = append(out, SessionView{Host: h.Host, Session: s, Label: h.Labels[s], Activity: h.Activity[s],
				Online: h.Online, Interactive: h.Interactive, Claude: claude[s]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Activity > out[j].Activity })
	return out
}

// orderKeys is Ordered's sequence as comparable keys: which session is where.
func orderKeys(hosts []HostView) []string {
	list := Ordered(hosts)
	keys := make([]string, len(list))
	for i, s := range list {
		keys[i] = s.Host + "\x00" + s.Session
	}
	return keys
}
